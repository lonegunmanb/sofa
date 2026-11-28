package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/session"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const (
	shaBase   = "0000000000000000000000000000000000000001"
	shaCkpt1  = "1111111111111111111111111111111111111111"
	shaCkpt2  = "2222222222222222222222222222222222222222"
	shaCkpt3  = "3333333333333333333333333333333333333333"
	shaRebase = "4444444444444444444444444444444444444444"
)

type fakeGitAdapter struct {
	mu             sync.Mutex
	baseSHA        string
	headSHA        string
	nextCommitSHAs []string
	statusPaths    []git.ChangedPath
	revertedPaths  [][]string
	pushedCommits  []pushedCommitCall
	branchTips     map[string]string
	branchContains map[string]map[string]bool
	rebaseOutcomes []git.RebaseOutcome
	rebaseErr      error
	pushErr        error
	onPrepare      func(destDir string)
}

type pushedCommitCall struct {
	branch   string
	sha      string
	leaseSHA string
}

func newFakeGitAdapter(baseSHA string) *fakeGitAdapter {
	return &fakeGitAdapter{
		baseSHA:        baseSHA,
		headSHA:        baseSHA,
		branchTips:     map[string]string{"main": baseSHA},
		branchContains: map[string]map[string]bool{"main": {baseSHA: true}},
	}
}

func (f *fakeGitAdapter) ResolveBranchSHA(_ context.Context, _, branch string, _ git.AuthConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tip, ok := f.branchTips[branch]; ok && tip != "" {
		return tip, nil
	}
	return "", errors.New("branch not found")
}

func (f *fakeGitAdapter) PrepareWorkspace(_ context.Context, _, _ string, pinSHA, destDir string, _ git.AuthConfig) error {
	if err := os.MkdirAll(filepath.Join(destDir, ".git"), 0o755); err != nil {
		return err
	}
	f.mu.Lock()
	f.headSHA = pinSHA
	cb := f.onPrepare
	f.mu.Unlock()
	if cb != nil {
		cb(destDir)
	}
	return nil
}

func (f *fakeGitAdapter) HeadSHA(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headSHA, nil
}

func (f *fakeGitAdapter) StatusPaths(_ context.Context, _ string) ([]git.ChangedPath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.statusPaths), nil
}

func (f *fakeGitAdapter) RevertPaths(_ context.Context, _ string, paths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revertedPaths = append(f.revertedPaths, slices.Clone(paths))
	remaining := make([]git.ChangedPath, 0, len(f.statusPaths))
	for _, cp := range f.statusPaths {
		if !slices.Contains(paths, cp.Path) && !slices.Contains(paths, cp.OldPath) {
			remaining = append(remaining, cp)
		}
	}
	f.statusPaths = remaining
	return nil
}

func (f *fakeGitAdapter) CommitAllAndPush(_ context.Context, _, _, remoteBranch, _ string, _ git.AuthConfig) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := f.headSHA
	committed := false
	if len(f.nextCommitSHAs) > 0 {
		sha = f.nextCommitSHAs[0]
		f.nextCommitSHAs = f.nextCommitSHAs[1:]
		committed = sha != f.headSHA
		f.headSHA = sha
	}
	f.branchTips[remoteBranch] = sha
	if f.branchContains[remoteBranch] == nil {
		f.branchContains[remoteBranch] = make(map[string]bool)
	}
	f.branchContains[remoteBranch][sha] = true
	return sha, committed, nil
}

func (f *fakeGitAdapter) PushCommit(_ context.Context, _, _, remoteBranch, commitSHA, expectedOldRemoteSHA string, _ git.AuthConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return f.pushErr
	}
	if expectedOldRemoteSHA != "" && f.branchTips[remoteBranch] != expectedOldRemoteSHA {
		return errors.New("stale lease on remote branch")
	}
	f.pushedCommits = append(f.pushedCommits, pushedCommitCall{
		branch:   remoteBranch,
		sha:      commitSHA,
		leaseSHA: expectedOldRemoteSHA,
	})
	f.branchTips[remoteBranch] = commitSHA
	if f.branchContains[remoteBranch] == nil {
		f.branchContains[remoteBranch] = make(map[string]bool)
	}
	f.branchContains[remoteBranch][commitSHA] = true
	return nil
}

func (f *fakeGitAdapter) BranchContainsSHA(_ context.Context, _, remoteBranch, targetSHA string, _ git.AuthConfig) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tip := f.branchTips[remoteBranch]
	if tip == "" {
		return "", false, nil
	}
	return tip, f.branchContains[remoteBranch][targetSHA], nil
}

func (f *fakeGitAdapter) DeleteRemoteBranch(_ context.Context, _, remoteBranch string, _ git.AuthConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.branchTips, remoteBranch)
	delete(f.branchContains, remoteBranch)
	return nil
}

func (f *fakeGitAdapter) RebaseCandidate(_ context.Context, _, _, _, workDir string, _ git.AuthConfig) (git.RebaseOutcome, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return git.RebaseOutcome{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rebaseErr != nil {
		return git.RebaseOutcome{}, f.rebaseErr
	}
	if len(f.rebaseOutcomes) == 0 {
		return git.RebaseOutcome{}, errors.New("no rebase outcome scripted")
	}
	out := f.rebaseOutcomes[0]
	f.rebaseOutcomes = f.rebaseOutcomes[1:]
	return out, nil
}

type mockSandboxEngine struct {
	mu       sync.Mutex
	timeline *[]string
	failures []*sandboxruntime.GatekeeperFailure
	calls    []sandboxruntime.GatekeeperRunSpec
}

func (e *mockSandboxEngine) DetectPlatform(_ context.Context) (sandboxruntime.Platform, error) {
	return harnessinstaller.Platform{OS: "linux", Arch: "amd64"}, nil
}

func (e *mockSandboxEngine) Exec(_ context.Context, _, _ string, _ []string, _ map[string]string, _ io.Reader) (sandboxruntime.ExecResult, error) {
	return sandboxruntime.ExecResult{}, nil
}

func (e *mockSandboxEngine) RunGatekeepers(_ context.Context, spec sandboxruntime.GatekeeperRunSpec, _ []workerspec.GatekeeperConfig) (*sandboxruntime.GatekeeperFailure, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, spec)
	if e.timeline != nil {
		*e.timeline = append(*e.timeline, "gatekeeper")
	}
	if len(e.failures) == 0 {
		return nil, nil
	}
	f := e.failures[0]
	e.failures = e.failures[1:]
	return f, nil
}

type scriptedSessionLauncher struct {
	mu       sync.Mutex
	timeline *[]string
	requests []runner.SessionLaunchRequest
	openers  []func(phase string, req runner.SessionLaunchRequest) harnessruntime.Opener
}

func (l *scriptedSessionLauncher) Launch(_ context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error) {
	l.mu.Lock()
	idx := len(l.requests)
	l.requests = append(l.requests, req)
	if l.timeline != nil {
		*l.timeline = append(*l.timeline, "launch:"+req.Phase)
	}
	var openerFn func(string, runner.SessionLaunchRequest) harnessruntime.Opener
	if idx < len(l.openers) {
		openerFn = l.openers[idx]
	}
	l.mu.Unlock()

	if openerFn == nil {
		return nil, errors.New("unexpected session launch")
	}
	return &scriptedSessionHandle{
		phase:    req.Phase,
		cid:      req.Phase + "-ctr",
		opener:   openerFn(req.Phase, req),
		timeline: l.timeline,
		mu:       &l.mu,
	}, nil
}

type scriptedSessionHandle struct {
	phase    string
	cid      string
	opener   harnessruntime.Opener
	timeline *[]string
	mu       *sync.Mutex
	closed   bool
}

func (h *scriptedSessionHandle) Opener() harnessruntime.Opener { return h.opener }
func (h *scriptedSessionHandle) ContainerID() string           { return h.cid }
func (h *scriptedSessionHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		if h.timeline != nil {
			*h.timeline = append(*h.timeline, "close:"+h.phase)
		}
	}
	return nil
}

type scriptedOpener struct {
	onOpen func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error)
}

func (o *scriptedOpener) Capabilities(_ context.Context) (harnessruntime.Capabilities, error) {
	return harnessruntime.Capabilities{HarnessName: "mock", SupportsSessionResume: true}, nil
}

func (o *scriptedOpener) Open(_ context.Context, cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
	return o.onOpen(cfg)
}

type scriptedSession struct {
	id     string
	cfg    harnessruntime.SessionConfig
	onTurn func(prompt string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error)
}

func (s *scriptedSession) ID() string { return s.id }
func (s *scriptedSession) RunTurn(_ context.Context, prompt string, _ func(harnessruntime.Event)) (harnessruntime.TurnSummary, error) {
	return s.onTurn(prompt, s.cfg)
}
func (s *scriptedSession) Abort(_ context.Context) error  { return nil }
func (s *scriptedSession) Resume(_ context.Context) error { return nil }
func (s *scriptedSession) ReadMCPResource(_ context.Context, _, _ string) ([]harnessruntime.MCPResourceContent, error) {
	return nil, nil
}
func (s *scriptedSession) Close(_ context.Context) error { return nil }

func invokeToolMustOK(t *testing.T, cfg harnessruntime.SessionConfig, name string, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	h := cfg.ToolHandlers[name]
	require.NotNil(t, h, "expected host tool %q", name)
	out, err := h(context.Background(), harnessruntime.ToolInvocation{
		ToolCallID:    "tc-" + name,
		ToolName:      name,
		ArgumentsJSON: raw,
	})
	require.NoError(t, err)
	require.Empty(t, out.HandlerError, "tool %s failed: %s", name, out.TextResultForLLM)
	return out.TextResultForLLM
}

func baseSandboxRef(name string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal("sandbox." + name),
		"kind":    cty.StringVal("sandbox"),
		"name":    cty.StringVal(name),
	})
}

func baseHarnessRef(name string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal("harness." + name),
		"kind":    cty.StringVal("harness"),
		"name":    cty.StringVal(name),
	})
}

func baseProviderRef(name string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal("model_provider." + name),
		"kind":    cty.StringVal("model_provider"),
		"name":    cty.StringVal(name),
	})
}

func testSandboxes() map[string]sandboxspec.SandboxConfig {
	return map[string]sandboxspec.SandboxConfig{
		"sandbox.work": {Runtime: "podman", Image: "golang:1.27", Network: "bridge"},
		"sandbox.qc":   {Runtime: "podman", Image: "alpine:latest", Network: "bridge"},
	}
}

func testHarnesses() map[string]harnessspec.HarnessConfig {
	return map[string]harnessspec.HarnessConfig{
		"harness.copilot":     {Name: "copilot"},
		"harness.antigravity": {Name: "antigravity"},
	}
}

func testProviders() map[string]provider.Config {
	key := "sk-test"
	return map[string]provider.Config{
		"model_provider.openai": {
			Type:     provider.TypeOpenAI,
			Endpoint: "https://api.example.invalid/v1",
			APIKey:   &key,
		},
	}
}

func TestWorkerRunnerReworkLoopFreezeAndQCOverrides(t *testing.T) {
	ctx := context.Background()
	tempRoot := filepath.Join(t.TempDir(), "runner-tmp")
	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
	repoURL := "https://github.com/acme/repo.git"
	runID := "run-rework"
	nodeAddr := "worker.impl"

	ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{
		InitialEpoch: 1,
		Ledger:       ledger,
		OwnerNode:    nodeAddr,
	})
	require.NoError(t, err)

	fakeGit := newFakeGitAdapter(shaBase)
	fakeGit.nextCommitSHAs = []string{shaCkpt1}

	var timeline []string
	gkEngine := &mockSandboxEngine{
		timeline: &timeline,
		failures: []*sandboxruntime.GatekeeperFailure{
			{
				Name:             "unit",
				Command:          []string{"go", "test", "./..."},
				ExitCode:         1,
				ExpectedExitCode: 0,
				Stderr:           "FAIL: TestFeature",
			},
			nil, // Round 2 gatekeeper passes
			nil, // Round 3 gatekeeper passes
		},
	}

	var workPrompts []string
	var workResumeIDs []string
	var workSessionProviders []*harnessruntime.ProviderConfig
	var workSessionCfgs []harnessruntime.SessionConfig
	var qcSessionProviders []*harnessruntime.ProviderConfig
	var qcSessionModels []string
	var qcSessionCfgs []harnessruntime.SessionConfig

	launcher := &scriptedSessionLauncher{
		timeline: &timeline,
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			// 1. Work iteration 0: writes feature, gatekeeper will fail.
			func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					workResumeIDs = append(workResumeIDs, cfg.ResumeSessionID)
					workSessionProviders = append(workSessionProviders, cfg.Provider)
					workSessionCfgs = append(workSessionCfgs, cfg)
					return &scriptedSession{
						id:  "work-sess-1",
						cfg: cfg,
						onTurn: func(prompt string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							workPrompts = append(workPrompts, prompt)
							require.NoError(t, os.WriteFile(filepath.Join(req.MountLayout.ArtifactsDir, "report.json"), []byte(`{"ok":false}`), 0o644))
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "initial impl"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						},
					}, nil
				}}
			},
			// 2. Work iteration 1 (after gatekeeper fail): fixes test, gatekeeper will pass -> QC Round 1.
			func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					workResumeIDs = append(workResumeIDs, cfg.ResumeSessionID)
					workSessionCfgs = append(workSessionCfgs, cfg)
					return &scriptedSession{
						id:  "work-sess-1",
						cfg: cfg,
						onTurn: func(prompt string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							workPrompts = append(workPrompts, prompt)
							require.NoError(t, os.WriteFile(filepath.Join(req.MountLayout.ArtifactsDir, "report.json"), []byte(`{"ok":true}`), 0o644))
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "fixed gatekeeper"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						},
					}, nil
				}}
			},
			// 3. QC Round 1: opens QC-001 and submits revise.
			func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					qcSessionProviders = append(qcSessionProviders, cfg.Provider)
					qcSessionModels = append(qcSessionModels, cfg.Model)
					qcSessionCfgs = append(qcSessionCfgs, cfg)
					return &scriptedSession{
						id:  "qc-sess-1",
						cfg: cfg,
						onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaQCOpenIssues, map[string]any{
								"issues": []map[string]any{{
									"criterion": "quality",
									"code":      "missing_edge_case",
									"message":   "handle empty input",
								}},
							})
							invokeToolMustOK(t, cfg, session.ToolSofaQCSubmitVerdict, map[string]any{
								"decision": "revise",
								"summary":  "needs edge case fix",
							})
							return harnessruntime.TurnSummary{FinalContent: "revise"}, nil
						},
					}, nil
				}}
			},
			// 4. Work iteration 2 (after QC-001 revise): fixes edge case.
			func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					workResumeIDs = append(workResumeIDs, cfg.ResumeSessionID)
					workSessionCfgs = append(workSessionCfgs, cfg)
					return &scriptedSession{
						id:  "work-sess-1",
						cfg: cfg,
						onTurn: func(prompt string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							workPrompts = append(workPrompts, prompt)
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "fixed QC-001"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						},
					}, nil
				}}
			},
			// 5. QC Round 2: resolves QC-001 and submits pass.
			func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					qcSessionProviders = append(qcSessionProviders, cfg.Provider)
					qcSessionModels = append(qcSessionModels, cfg.Model)
					qcSessionCfgs = append(qcSessionCfgs, cfg)
					return &scriptedSession{
						id:  "qc-sess-2",
						cfg: cfg,
						onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaQCResolveIssues, map[string]any{
								"ids":                 []string{"QC-001"},
								"resolution_evidence": "verified edge case fixed",
							})
							invokeToolMustOK(t, cfg, session.ToolSofaQCSubmitVerdict, map[string]any{
								"decision": "pass",
								"summary":  "all criteria satisfied",
							})
							return harnessruntime.TurnSummary{FinalContent: "pass"}, nil
						},
					}, nil
				}}
			},
		},
	}

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane:    cp,
		GitAdapter:      fakeGit,
		BlobStore:       blobStore,
		ArtifactSvc:     artSvc,
		SandboxEngine:   gkEngine,
		SessionLauncher: launcher,
	})
	require.NoError(t, err)

	outcome, err := r.Run(ctx, runner.NodeExecutionRequest{
		RunID:          runID,
		NodeAddr:       nodeAddr,
		Epoch:          1,
		RepoURL:        repoURL,
		BasedSource:    "main",
		Sandboxes:      testSandboxes(),
		Harnesses:      testHarnesses(),
		ModelProviders: testProviders(),
		TempRoot:       tempRoot,
		Worker: workerspec.Config{
			Sandbox:       baseSandboxRef("work"),
			Harness:       baseHarnessRef("copilot"),
			ModelProvider: baseProviderRef("openai"),
			Model:         "gpt-5",
			Profile:       "work-profile",
			SystemPrompt:  cty.StringVal("sys"),
			Prompt:        cty.StringVal("implement feature"),
			WritablePaths: []string{"allowed/**"},
			HumanApproval: "optional",
			Imports: map[string]workerspec.ImportArtifact{
				"bundle": {Name: "bundle", Path: "bundle", Desc: "Spec bundle"},
			},
			Artifacts: map[string]workerspec.Artifact{
				"report": {Name: "report", Type: "file", Path: "report.json", Description: "report", Required: true, NonEmpty: true},
			},
			QC: &workerspec.QCConfig{
				Criteria:         cty.MapVal(map[string]cty.Value{"quality": cty.StringVal("must pass")}),
				Sandbox:          baseSandboxRef("qc"),
				Harness:          baseHarnessRef("antigravity"),
				ModelProviderSet: true, // explicit model_provider = null overrides worker's openai provider
				ModelProvider:    cty.NullVal(cty.EmptyObject),
				Model:            "gemini-2.5-pro",
				MaxQCRounds:      2,
				Gatekeepers: []workerspec.GatekeeperConfig{
					{Name: "unit", Command: []string{"go", "test", "./..."}, WorkingDir: ".", Timeout: "5m", ExpectExitCode: 0, RequireCleanTree: true},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, outcome.Status)
	require.Equal(t, git.WorkBranchName(runID, nodeAddr), outcome.WorkBranch)
	require.Equal(t, shaCkpt1, outcome.CommitSHA)
	require.Len(t, outcome.Artifacts, 1)
	require.Equal(t, "report", outcome.Artifacts[0].Name)

	// Verify strict physical freeze ordering: every Work launch is closed BEFORE RunGatekeepers and BEFORE QC!
	require.Equal(t, []string{
		"launch:work", "close:work", "gatekeeper",
		"launch:work", "close:work", "gatekeeper",
		"launch:qc", "close:qc",
		"launch:work", "close:work", "gatekeeper",
		"launch:qc", "close:qc",
	}, timeline)

	// Verify ResumeSessionID passed on rework rounds and prompts include gatekeeper & QC-001 feedback.
	require.Equal(t, []string{"", "work-sess-1", "work-sess-1"}, workResumeIDs)
	require.Contains(t, workPrompts[1], "TestFeature")
	require.Contains(t, workPrompts[2], "QC-001")
	require.Contains(t, workPrompts[2], "handle empty input")

	// Verify QC mount isolation and orthogonal overrides (including explicit model_provider = null).
	for _, req := range launcher.requests {
		if req.Phase == "work" {
			assert.False(t, req.MountLayout.ReadOnlyWorkspace)
			assert.False(t, req.MountLayout.ReadOnlyArtifacts)
			assert.Equal(t, []string{"allowed/**"}, req.MountLayout.WritablePaths)
			assert.Equal(t, "golang:1.27", req.Sandbox.Image)
			assert.Equal(t, "copilot", req.Harness.Name)
		} else if req.Phase == "qc" {
			assert.True(t, req.MountLayout.ReadOnlyWorkspace)
			assert.True(t, req.MountLayout.ReadOnlyArtifacts)
			assert.Nil(t, req.MountLayout.WritablePaths)
			assert.Equal(t, "alpine:latest", req.Sandbox.Image)
			assert.Equal(t, "antigravity", req.Harness.Name)
			assert.Equal(t, "/.sofa/harness/sofa-harness-antigravity", req.MountLayout.Entrypoint)
		}
	}
	require.Len(t, workSessionProviders, 1)
	require.NotNil(t, workSessionProviders[0])
	assert.Equal(t, "gpt-5", workSessionProviders[0].WireModel)
	assert.Equal(t, "work-profile", workSessionProviders[0].ModelID)

	require.Len(t, qcSessionProviders, 2)
	assert.Nil(t, qcSessionProviders[0])
	assert.Equal(t, []string{"gemini-2.5-pro", "gemini-2.5-pro"}, qcSessionModels)

	// 041: the host auto-injects the runtime/protocol contract into Work and QC prompts.
	require.NotEmpty(t, workSessionCfgs)
	workSysPrompt := workSessionCfgs[0].SystemPrompt
	assert.True(t, strings.HasPrefix(workSysPrompt, "sys"), "user system_prompt must be preserved and lead the prompt")
	assert.Contains(t, workSysPrompt, "/workspace")
	assert.Contains(t, workSysPrompt, "git commit")
	assert.Contains(t, workSysPrompt, "git push")
	assert.Contains(t, workSysPrompt, "git checkout")
	assert.Contains(t, workSysPrompt, "allowed/**")
	assert.Contains(t, workSysPrompt, "/.sofa/artifacts/report.json")
	assert.Contains(t, workSysPrompt, "/.sofa/imports/bundle")
	assert.Contains(t, workSysPrompt, "sofa_checkpoint")
	assert.Contains(t, workSysPrompt, "sofa_complete_work")

	require.NotEmpty(t, qcSessionCfgs)
	qcSysPrompt := qcSessionCfgs[0].SystemPrompt
	assert.Contains(t, qcSysPrompt, "/workspace")
	assert.Contains(t, qcSysPrompt, "/.sofa/artifacts/report.json")
	assert.Contains(t, qcSysPrompt, "read-only")
	assert.Contains(t, qcSysPrompt, "git diff")
	assert.Contains(t, qcSysPrompt, "sofa_qc_open_issues")
	assert.Contains(t, qcSysPrompt, "sofa_qc_resolve_issues")
	assert.Contains(t, qcSysPrompt, "sofa_qc_submit_verdict")

	// Verify host temp root was completely cleaned up.
	entries, err := os.ReadDir(tempRoot)
	if err == nil {
		for _, e := range entries {
			sub, _ := os.ReadDir(filepath.Join(tempRoot, e.Name()))
			assert.Empty(t, sub, "expected tempRoot %s/%s to have zero leftover execution dirs", tempRoot, e.Name())
		}
	}
}

func TestWorkerRunnerAutoInjectsCustomTerminateToolIntoWorkPrompt(t *testing.T) {
	ctx := context.Background()
	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
	require.NoError(t, err)
	fakeGit := newFakeGitAdapter(shaBase)

	inputTypeJSON, err := ctyjson.MarshalType(cty.EmptyObject)
	require.NoError(t, err)
	outputTypeJSON, err := ctyjson.MarshalType(cty.Bool)
	require.NoError(t, err)

	var captured harnessruntime.SessionConfig
	launcher := &scriptedSessionLauncher{
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					captured = cfg
					return &scriptedSession{id: "w-custom-term", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
						require.NoError(t, os.WriteFile(filepath.Join(req.MountLayout.ArtifactsDir, "summary.md"), []byte("ok"), 0o644))
						invokeToolMustOK(t, cfg, "submit_result", map[string]any{})
						return harnessruntime.TurnSummary{FinalContent: "done"}, nil
					}}, nil
				}}
			},
		},
	}

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane:    cp,
		GitAdapter:      fakeGit,
		BlobStore:       blobStore,
		ArtifactSvc:     artSvc,
		SandboxEngine:   &mockSandboxEngine{},
		SessionLauncher: launcher,
		ToolExecFactory: func(string) toolruntime.ContainerExecFunc {
			return func(context.Context, string, []string, map[string]string, []byte) (string, string, int, error) {
				return `{"accepted":true,"output":true}`, "", 0, nil
			}
		},
	})
	require.NoError(t, err)

	outcome, err := r.Run(ctx, runner.NodeExecutionRequest{
		RunID:          "run-custom-terminate",
		NodeAddr:       "worker.impl",
		Epoch:          1,
		RepoURL:        "https://github.com/acme/repo.git",
		BasedSource:    "main",
		Sandboxes:      testSandboxes(),
		Harnesses:      testHarnesses(),
		ModelProviders: testProviders(),
		Tools: map[string]plan.ToolSpec{
			"finish_slice": {
				ID:          "finish_slice",
				Name:        "finish_slice",
				Kind:        "go",
				InputType:   inputTypeJSON,
				OutputType:  outputTypeJSON,
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			},
		},
		Worker: workerspec.Config{
			Sandbox:       baseSandboxRef("work"),
			Harness:       baseHarnessRef("copilot"),
			ModelProvider: baseProviderRef("openai"),
			Model:         "gpt-5",
			SystemPrompt:  cty.StringVal("domain guidance only"),
			Prompt:        cty.StringVal("implement the slice"),
			ToolUses: []workerspec.ToolUse{
				{ToolID: "finish_slice", Name: "submit_result", Terminate: true},
			},
			Artifacts: map[string]workerspec.Artifact{
				"summary": {Name: "summary", Type: "file", Path: "summary.md", Description: "summary", Required: true},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, outcome.Status)

	assert.Contains(t, captured.SystemPrompt, "domain guidance only")
	assert.Contains(t, captured.SystemPrompt, "submit_result")
	assert.NotContains(t, captured.SystemPrompt, session.ToolSofaCompleteWork)
	assert.Contains(t, captured.SystemPrompt, "/.sofa/artifacts/summary.md")
}

func TestWorkerRunnerUnauthorizedPathExhaustionHardRollback(t *testing.T) {
	ctx := context.Background()
	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
	require.NoError(t, err)

	fakeGit := newFakeGitAdapter(shaBase)
	fakeGit.statusPaths = []git.ChangedPath{
		{Path: "allowed/ok.go"},
		{Path: "forbidden/secret.txt", Untracked: true},
	}

	launcher := &scriptedSessionLauncher{
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					return &scriptedSession{
						id:  "work-unauth",
						cfg: cfg,
						onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							// Keep calling sofa_complete_work without cleaning unauthorized file.
							raw, _ := json.Marshal(map[string]any{"summary": "try"})
							_, _ = cfg.ToolHandlers[session.ToolSofaCompleteWork](ctx, harnessruntime.ToolInvocation{
								ToolCallID:    "tc-1",
								ToolName:      session.ToolSofaCompleteWork,
								ArgumentsJSON: raw,
							})
							return harnessruntime.TurnSummary{FinalContent: "oops"}, nil
						},
					}, nil
				}}
			},
		},
	}

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane:    cp,
		GitAdapter:      fakeGit,
		BlobStore:       blobStore,
		SandboxEngine:   &mockSandboxEngine{},
		SessionLauncher: launcher,
	})
	require.NoError(t, err)

	outcome, err := r.Run(ctx, runner.NodeExecutionRequest{
		RunID:          "run-unauth",
		NodeAddr:       "worker.impl",
		Epoch:          1,
		RepoURL:        "https://github.com/acme/repo.git",
		BasedSource:    "main",
		Sandboxes:      testSandboxes(),
		Harnesses:      testHarnesses(),
		ModelProviders: testProviders(),
		Worker: workerspec.Config{
			Sandbox:             baseSandboxRef("work"),
			Harness:             baseHarnessRef("copilot"),
			Model:               "gpt-5",
			SystemPrompt:        cty.StringVal("sys"),
			Prompt:              cty.StringVal("do work"),
			WritablePaths:       []string{"allowed/**"},
			MaxProtocolAttempts: 1,
		},
	})
	require.NoError(t, err)
	require.Equal(t, runner.StatusFailed, outcome.Status)
	require.Contains(t, outcome.FailureReason, "max protocol attempts")
	require.Equal(t, [][]string{{"forbidden/secret.txt"}}, fakeGit.revertedPaths)
	// Authorized path remains untouched in statusPaths.
	require.Equal(t, []git.ChangedPath{{Path: "allowed/ok.go"}}, fakeGit.statusPaths)
}

func TestWorkerRunnerHumanApprovalVersionBindingAndResume(t *testing.T) {
	ctx := context.Background()
	sharedRoot := t.TempDir()
	blobStore, err := blob.NewLocalStore(filepath.Join(sharedRoot, "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
	repoURL := "https://github.com/acme/repo.git"
	runID := "run-approval"
	nodeAddr := "worker.release"

	ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{
		InitialEpoch:        1,
		Ledger:              ledger,
		OwnerNode:           nodeAddr,
		CheckpointStatePath: filepath.Join(sharedRoot, "ckpt.json"),
		StatePath:           filepath.Join(sharedRoot, "cp.json"),
	})
	require.NoError(t, err)

	fakeGit := newFakeGitAdapter(shaBase)
	fakeGit.nextCommitSHAs = []string{shaCkpt1}

	launchCount := 0
	launcher := &scriptedSessionLauncher{
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
				launchCount++
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					return &scriptedSession{
						id:  "work-app-1",
						cfg: cfg,
						onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							require.NoError(t, os.WriteFile(filepath.Join(req.MountLayout.ArtifactsDir, "release.txt"), []byte("v1"), 0o644))
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "ready"})
							return harnessruntime.TurnSummary{FinalContent: "ready"}, nil
						},
					}, nil
				}}
			},
		},
	}

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane:    cp,
		GitAdapter:      fakeGit,
		BlobStore:       blobStore,
		ArtifactSvc:     artSvc,
		SandboxEngine:   &mockSandboxEngine{},
		SessionLauncher: launcher,
	})
	require.NoError(t, err)

	req := runner.NodeExecutionRequest{
		RunID:          runID,
		NodeAddr:       nodeAddr,
		Epoch:          1,
		RepoURL:        repoURL,
		BasedSource:    "main",
		Sandboxes:      testSandboxes(),
		Harnesses:      testHarnesses(),
		ModelProviders: testProviders(),
		Worker: workerspec.Config{
			Sandbox:       baseSandboxRef("work"),
			Harness:       baseHarnessRef("copilot"),
			Model:         "gpt-5",
			SystemPrompt:  cty.StringVal("sys"),
			Prompt:        cty.StringVal("build"),
			HumanApproval: "required",
			Artifacts: map[string]workerspec.Artifact{
				"rel": {Name: "rel", Type: "file", Path: "release.txt", Description: "rel", Required: true, NonEmpty: true},
			},
		},
	}

	// (a) No approval -> StatusWaitingHuman with PendingApproval.
	out1, err := r.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runner.StatusWaitingHuman, out1.Status)
	require.NotNil(t, out1.PendingApproval)
	require.Equal(t, shaCkpt1, out1.PendingApproval.CommitSHA)
	require.Len(t, out1.PendingApproval.ArtifactIDs, 1)
	require.Equal(t, 1, launchCount)
	require.Empty(t, fakeGit.pushedCommits)

	// (b) Resume at epoch 2 with STALE CommitSHA approval -> still StatusWaitingHuman, ZERO new LLM sessions!
	require.NoError(t, cp.AdvanceEpoch(2))
	require.NoError(t, cp.SetApproval(runner.ApprovalRecord{
		Approved:    true,
		CommitSHA:   shaBase, // stale!
		ArtifactIDs: out1.PendingApproval.ArtifactIDs,
	}))
	req.Epoch = 2
	out2, err := r.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runner.StatusWaitingHuman, out2.Status)
	require.Equal(t, shaCkpt1, out2.PendingApproval.CommitSHA)
	require.Equal(t, 1, launchCount)
	require.Empty(t, fakeGit.pushedCommits)

	// (c) Resume at epoch 3 with EXACT matching (CommitSHA, ArtifactIDs) -> publishes without re-running LLM and clears PendingPublication!
	require.NoError(t, cp.AdvanceEpoch(3))
	require.NoError(t, cp.SetApproval(runner.ApprovalRecord{
		Approved:    true,
		CommitSHA:   shaCkpt1,
		ArtifactIDs: out1.PendingApproval.ArtifactIDs,
	}))
	req.Epoch = 3
	out3, err := r.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, out3.Status)
	require.Equal(t, git.WorkBranchName(runID, nodeAddr), out3.WorkBranch)
	require.Equal(t, shaCkpt1, out3.CommitSHA)
	require.Equal(t, 1, launchCount)
	require.Len(t, fakeGit.pushedCommits, 1)
	require.Equal(t, shaCkpt1, fakeGit.pushedCommits[0].sha)
	clearedPending, err := cp.LoadPendingPublication(ctx)
	require.NoError(t, err)
	require.Nil(t, clearedPending)

	// (d) Queued human interjection while paused on PendingApproval -> must NOT short-circuit; clears PendingPublication and enters Work loop!
	require.NoError(t, cp.ClearApproval())
	require.NoError(t, cp.AdvanceEpoch(4))
	require.NoError(t, cp.SavePendingPublication(ctx, 4, &runner.PendingPublication{
		CommitSHA:   shaCkpt1,
		BaseSHA:     shaBase,
		ArtifactIDs: out1.PendingApproval.ArtifactIDs,
		Artifacts:   out1.PendingApproval.Artifacts,
	}))
	require.NoError(t, cp.EnqueueInterjection(session.HumanInterjection{
		ID:      "int-1",
		Message: "add changelog note before publishing",
	}))
	var seenInterjection bool
	launcher.openers = append(launcher.openers, func(_ string, reqLaunch runner.SessionLaunchRequest) harnessruntime.Opener {
		launchCount++
		return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
			return &scriptedSession{
				id:  "work-app-2",
				cfg: cfg,
				onTurn: func(prompt string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
					if assert.Contains(t, prompt, "add changelog note before publishing") {
						seenInterjection = true
					}
					require.NoError(t, os.WriteFile(filepath.Join(reqLaunch.MountLayout.ArtifactsDir, "release.txt"), []byte("v2"), 0o644))
					invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "incorporated feedback"})
					return harnessruntime.TurnSummary{FinalContent: "v2 ready"}, nil
				},
			}, nil
		}}
	})
	fakeGit.nextCommitSHAs = []string{shaCkpt2}
	req.Epoch = 4
	outInterj, err := r.Run(ctx, req)
	require.NoError(t, err)
	require.True(t, seenInterjection)
	require.Equal(t, 2, launchCount)
	require.Equal(t, runner.StatusWaitingHuman, outInterj.Status)
	require.NotNil(t, outInterj.PendingApproval)
	require.Equal(t, shaCkpt2, outInterj.PendingApproval.CommitSHA)

	// (e) Resume at epoch 5 with ReturnedUpstream -> triggers human_returned_upstream fallback without re-running LLM and clears PendingPublication!
	require.NoError(t, cp.AdvanceEpoch(5))
	require.NoError(t, cp.SetApproval(runner.ApprovalRecord{
		ReturnedUpstream: true,
		ReturnFallback:   "back_up",
	}))
	req.Epoch = 5
	req.Worker.Fallbacks = []workerspec.FallbackConfig{{
		Name:   "back_up",
		On:     []string{"human_returned_upstream"},
		Target: "worker.plan",
	}}
	out4, err := r.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runner.StatusFallbackTriggered, out4.Status)
	require.Equal(t, "human_returned_upstream", out4.FallbackReason)
	require.Equal(t, "back_up", out4.FallbackName)
	require.Equal(t, "worker.plan", out4.FallbackTarget)
	require.Equal(t, 2, launchCount)
	clearedAfterFB, err := cp.LoadPendingPublication(ctx)
	require.NoError(t, err)
	require.Nil(t, clearedAfterFB)
}

func TestWorkerRunnerFallbacksAndDecisionPause(t *testing.T) {
	ctx := context.Background()

	t.Run("qc_exhausted triggers fallback when declared and StatusFailed when undeclared", func(t *testing.T) {
		for _, withFallback := range []bool{true, false} {
			blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
			require.NoError(t, err)
			artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
			cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
			require.NoError(t, err)
			fakeGit := newFakeGitAdapter(shaBase)
			launcher := &scriptedSessionLauncher{
				openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
					func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
						return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
							return &scriptedSession{id: "w1", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
								require.NoError(t, os.WriteFile(filepath.Join(req.MountLayout.ArtifactsDir, "diag.txt"), []byte("diag"), 0o644))
								invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
								return harnessruntime.TurnSummary{FinalContent: "done"}, nil
							}}, nil
						}}
					},
					func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
						return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
							return &scriptedSession{id: "qc1", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
								invokeToolMustOK(t, cfg, session.ToolSofaQCOpenIssues, map[string]any{
									"issues": []map[string]any{{"criterion": "c", "code": "bad", "message": "still broken"}},
								})
								invokeToolMustOK(t, cfg, session.ToolSofaQCSubmitVerdict, map[string]any{"decision": "revise", "summary": "reject"})
								return harnessruntime.TurnSummary{FinalContent: "revise"}, nil
							}}, nil
						}}
					},
				},
			}

			r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
				ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore, ArtifactSvc: artSvc,
				SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
			})
			require.NoError(t, err)

			var fallbacks []workerspec.FallbackConfig
			if withFallback {
				fallbacks = []workerspec.FallbackConfig{{
					Name: "rework_up", On: []string{"qc_exhausted"}, Target: "worker.plan", Artifacts: []string{"diag"},
				}}
			}
			out, err := r.Run(ctx, runner.NodeExecutionRequest{
				RunID: "run-qc-ex", NodeAddr: "worker.impl", Epoch: 1,
				RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
				Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
				Worker: workerspec.Config{
					Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
					Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
					Artifacts: map[string]workerspec.Artifact{
						"diag": {Name: "diag", Type: "file", Path: "diag.txt", Description: "diag"},
					},
					QC:        &workerspec.QCConfig{Criteria: cty.MapVal(map[string]cty.Value{"c": cty.StringVal("ok")}), MaxQCRounds: 1},
					Fallbacks: fallbacks,
				},
			})
			require.NoError(t, err)
			if withFallback {
				require.Equal(t, runner.StatusFallbackTriggered, out.Status)
				require.Equal(t, "rework_up", out.FallbackName)
				require.Equal(t, "worker.plan", out.FallbackTarget)
				require.Equal(t, "qc_exhausted", out.FallbackReason)
				require.Len(t, out.FallbackArtifacts, 1)
			} else {
				require.Equal(t, runner.StatusFailed, out.Status)
			}
		}
	})

	t.Run("agent_escalated triggers fallback", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)
		launcher := &scriptedSessionLauncher{
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						return &scriptedSession{id: "w1", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaEscalate, map[string]any{"fallback": "up", "summary": "need spec"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						}}, nil
					}}
				},
			},
		}
		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-fb", NodeAddr: "worker.impl", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
				Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
				Fallbacks: []workerspec.FallbackConfig{{Name: "up", On: []string{"agent_escalated"}, Target: "worker.spec"}},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusFallbackTriggered, out.Status)
		require.Equal(t, "agent_escalated", out.FallbackReason)
		require.Equal(t, "up", out.FallbackName)
		require.Equal(t, "worker.spec", out.FallbackTarget)
	})

	t.Run("sofa_request_decision saves checkpoint, closes container, and cleans tempRoot", func(t *testing.T) {
		tempRoot := filepath.Join(t.TempDir(), "tmp-root")
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.nextCommitSHAs = []string{shaCkpt1}
		var timeline []string

		launcher := &scriptedSessionLauncher{
			timeline: &timeline,
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						return &scriptedSession{id: "w-dec", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaRequestDecision, map[string]any{
								"question": "Use SQLite or Postgres?",
								"options":  []string{"sqlite", "postgres"},
							})
							return harnessruntime.TurnSummary{FinalContent: "paused"}, nil
						}}, nil
					}}
				},
			},
		}

		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-dec", NodeAddr: "worker.impl", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(), TempRoot: tempRoot,
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
				Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusWaitingHuman, out.Status)
		require.NotNil(t, out.DecisionRequest)
		require.Equal(t, "Use SQLite or Postgres?", out.DecisionRequest.Question)
		require.Equal(t, []string{"launch:work", "close:work"}, timeline)

		ckpts, err := cp.ListCheckpoints(ctx)
		require.NoError(t, err)
		require.Len(t, ckpts, 1)
		require.Equal(t, checkpoint.StatusComplete, ckpts[0].Status)
		require.Equal(t, shaCkpt1, ckpts[0].FinalSHA)
	})
}

func TestWorkerRunnerGatekeeperExhaustedFallback(t *testing.T) {
	ctx := context.Background()
	for _, withFallback := range []bool{true, false} {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)

		fail := &sandboxruntime.GatekeeperFailure{Name: "unit", Command: []string{"go", "test"}, ExitCode: 1, Stderr: "test failed"}
		gkEngine := &mockSandboxEngine{failures: []*sandboxruntime.GatekeeperFailure{fail, fail}}
		makeOpener := func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
			return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
				return &scriptedSession{id: "w-gk", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
					invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
					return harnessruntime.TurnSummary{FinalContent: "done"}, nil
				}}, nil
			}}
		}
		launcher := &scriptedSessionLauncher{
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{makeOpener, makeOpener},
		}

		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: gkEngine, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		var fallbacks []workerspec.FallbackConfig
		if withFallback {
			fallbacks = []workerspec.FallbackConfig{{Name: "up", On: []string{"gatekeeper_exhausted"}, Target: "worker.spec"}}
		}
		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-gk-ex", NodeAddr: "worker.impl", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			MaxGatekeeperReworks: 1,
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
				Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
				QC: &workerspec.QCConfig{
					Gatekeepers: []workerspec.GatekeeperConfig{{Name: "unit", Command: []string{"go", "test"}, WorkingDir: ".", Timeout: "1m"}},
				},
				Fallbacks: fallbacks,
			},
		})
		require.NoError(t, err)
		if withFallback {
			require.Equal(t, runner.StatusFallbackTriggered, out.Status)
			require.Equal(t, "gatekeeper_exhausted", out.FallbackReason)
			require.Equal(t, "up", out.FallbackName)
			require.Equal(t, "worker.spec", out.FallbackTarget)
		} else {
			require.Equal(t, runner.StatusFailed, out.Status)
			require.Contains(t, out.FailureReason, "test failed")
		}
	}
}

func TestWorkerRunnerQCProviderProfileAndCrossEpochDivergedPush(t *testing.T) {
	ctx := context.Background()
	repoURL := "https://github.com/acme/repo.git"
	runID := "run-div"
	nodeAddr := "worker.impl"

	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{
		InitialEpoch: 1,
		Ledger:       ledger,
		OwnerNode:    nodeAddr,
	})
	require.NoError(t, err)

	fakeGit := newFakeGitAdapter(shaBase)
	fakeGit.nextCommitSHAs = []string{shaCkpt1}

	var qcProviders []*harnessruntime.ProviderConfig
	makeWorkOpener := func(id string) func(string, runner.SessionLaunchRequest) harnessruntime.Opener {
		return func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
			return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
				return &scriptedSession{id: id, cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
					invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": id})
					return harnessruntime.TurnSummary{FinalContent: id}, nil
				}}, nil
			}}
		}
	}
	makeQCOpener := func(id string) func(string, runner.SessionLaunchRequest) harnessruntime.Opener {
		return func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
			return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
				qcProviders = append(qcProviders, cfg.Provider)
				return &scriptedSession{id: id, cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
					invokeToolMustOK(t, cfg, session.ToolSofaQCSubmitVerdict, map[string]any{"decision": "pass", "summary": "ok"})
					return harnessruntime.TurnSummary{FinalContent: "ok"}, nil
				}}, nil
			}}
		}
	}

	launcher := &scriptedSessionLauncher{
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			makeWorkOpener("w-e1"),
			makeQCOpener("qc-e1"),
		},
	}

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
		SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
	})
	require.NoError(t, err)

	req := runner.NodeExecutionRequest{
		RunID: runID, NodeAddr: nodeAddr, Epoch: 1,
		RepoURL: repoURL, BasedSource: "main", BaseSHA: shaBase,
		Sandboxes: testSandboxes(), Harnesses: testHarnesses(), ModelProviders: testProviders(),
		Worker: workerspec.Config{
			Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
			ModelProvider: baseProviderRef("openai"), Model: "gpt-5", Profile: "work-prof",
			SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
			QC: &workerspec.QCConfig{
				Criteria:    cty.MapVal(map[string]cty.Value{"c": cty.StringVal("ok")}),
				Model:       "gpt-5-qc",
				Profile:     "qc-prof",
				MaxQCRounds: 1,
			},
		},
	}

	// Epoch 1 creates work_branch at shaCkpt1 with empty lease.
	out1, err := r.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, out1.Status)
	require.Equal(t, shaCkpt1, out1.CommitSHA)
	require.Len(t, fakeGit.pushedCommits, 1)
	require.Empty(t, fakeGit.pushedCommits[0].leaseSHA)

	// Verify QC inherited provider and applied qc.Model + qc.Profile -> WireModel & ModelID.
	require.Len(t, qcProviders, 1)
	require.NotNil(t, qcProviders[0])
	assert.Equal(t, "gpt-5-qc", qcProviders[0].WireModel)
	assert.Equal(t, "qc-prof", qcProviders[0].ModelID)

	// Simulate merge rejection and Epoch 2 re-run from updated BaseSHA (shaCkpt2) producing diverged shaCkpt3.
	cp2, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{
		InitialEpoch: 2,
		Ledger:       ledger,
		OwnerNode:    nodeAddr,
	})
	require.NoError(t, err)
	fakeGit.nextCommitSHAs = []string{shaCkpt3}
	launcher2 := &scriptedSessionLauncher{
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			makeWorkOpener("w-e2"),
			makeQCOpener("qc-e2"),
		},
	}
	r2, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane: cp2, GitAdapter: fakeGit, BlobStore: blobStore,
		SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher2,
	})
	require.NoError(t, err)
	req.Epoch = 2
	req.BaseSHA = shaCkpt2
	out2, err := r2.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, out2.Status)
	require.Equal(t, shaCkpt3, out2.CommitSHA)
	require.Len(t, fakeGit.pushedCommits, 2)
	require.Equal(t, shaCkpt1, fakeGit.pushedCommits[1].leaseSHA)
	require.Equal(t, shaCkpt3, fakeGit.pushedCommits[1].sha)
}

func TestWorkerRunnerStaleEpochFencesWorkBranchRegistration(t *testing.T) {
	ctx := context.Background()
	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
	require.NoError(t, err)
	fakeGit := newFakeGitAdapter(shaBase)
	launcher := &scriptedSessionLauncher{
		openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
			func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					return &scriptedSession{id: "w-stale", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
						invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
						// Advance epoch after work completes to simulate superseded zombie execution before publication.
						require.NoError(t, cp.AdvanceEpoch(2))
						return harnessruntime.TurnSummary{FinalContent: "done"}, nil
					}}, nil
				}}
			},
		},
	}

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
		SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
	})
	require.NoError(t, err)

	out, err := r.Run(ctx, runner.NodeExecutionRequest{
		RunID: "run-stale", NodeAddr: "worker.impl", Epoch: 1,
		RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
		Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
		Worker: workerspec.Config{
			Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
			Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
		},
	})
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)
	require.Equal(t, runner.StatusFailed, out.Status)
	require.Empty(t, fakeGit.pushedCommits)
}

func TestExecuteMergeCandidateTier1(t *testing.T) {
	ctx := context.Background()
	repoURL := "https://github.com/acme/repo.git"

	t.Run("clean rebase + gatekeeper + lease push succeeds", func(t *testing.T) {
		ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.rebaseOutcomes = []git.RebaseOutcome{{
			Conflicted: false,
			TargetSHA:  shaBase,
			RebasedSHA: shaRebase,
		}}
		gkEngine := &mockSandboxEngine{failures: []*sandboxruntime.GatekeeperFailure{nil}}

		res, err := runner.ExecuteMergeCandidate(ctx, runner.MergeCandidateRequest{
			RunID:           "run-m1",
			MergeNodeAddr:   "merge.int",
			Epoch:           1,
			RepoURL:         repoURL,
			BasedSource:     "main",
			CandidateBranch: "sofa/work/run-m1/w1",
			Sandbox:         testSandboxes()["sandbox.qc"],
			Gatekeepers:     []workerspec.GatekeeperConfig{{Name: "reg", Command: []string{"true"}, WorkingDir: ".", Timeout: "1m"}},
			GitAdapter:      fakeGit,
			Ledger:          ledger,
			SandboxEngine:   gkEngine,
		})
		require.NoError(t, err)
		require.Equal(t, runner.MergeStatusSucceeded, res.Status)
		require.Equal(t, git.WorkBranchName("run-m1", "merge.int"), res.TargetBranch)
		require.Equal(t, shaRebase, res.MergedCommitSHA)
		require.Equal(t, shaRebase, res.LatestTargetSHA)
		require.NoError(t, ledger.EnsureRegistered(repoURL, res.TargetBranch))
		require.Equal(t, shaBase, fakeGit.pushedCommits[len(fakeGit.pushedCommits)-1].leaseSHA)
	})

	t.Run("conflict returns MergeStatusReworkSource without pushing", func(t *testing.T) {
		ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.branchTips["release"] = shaCkpt1
		fakeGit.rebaseOutcomes = []git.RebaseOutcome{{
			Conflicted:    true,
			TargetSHA:     shaCkpt1,
			ConflictFiles: []string{"shared.go"},
			Details:       "CONFLICT (content): Merge conflict in shared.go",
		}}
		target := "release"
		res, err := runner.ExecuteMergeCandidate(ctx, runner.MergeCandidateRequest{
			RunID:           "run-m2",
			MergeNodeAddr:   "merge.int",
			Epoch:           1,
			RepoURL:         repoURL,
			BasedSource:     "main",
			TargetBranch:    &target,
			CandidateBranch: "sofa/work/run-m2/w2",
			GitAdapter:      fakeGit,
			Ledger:          ledger,
			SandboxEngine:   &mockSandboxEngine{},
		})
		require.NoError(t, err)
		require.Equal(t, runner.MergeStatusReworkSource, res.Status)
		require.Equal(t, "release", res.TargetBranch)
		require.Equal(t, shaCkpt1, res.LatestTargetSHA)
		require.Equal(t, []string{"shared.go"}, res.ConflictFiles)
		require.Empty(t, fakeGit.pushedCommits)
	})
}

func TestWorkerRunnerQCNestedGatekeeperBehavior(t *testing.T) {
	ctx := context.Background()

	t.Run("gatekeeper runs in qc.sandbox (fallback work sandbox), skips LLM QC on failure, and reruns after QC revise", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.nextCommitSHAs = []string{shaCkpt1}

		var timeline []string
		gkEngine := &mockSandboxEngine{
			timeline: &timeline,
			failures: []*sandboxruntime.GatekeeperFailure{
				{Name: "unit", Command: []string{"go", "test"}, ExitCode: 1, Stderr: "fail-1"},
				nil, // Work iteration 1 passes gatekeeper -> QC Round 1 (revise)
				nil, // Work iteration 2 (after QC revise) reruns gatekeeper and passes -> QC Round 2 (pass)
			},
		}
		makeWork := func(id string) func(string, runner.SessionLaunchRequest) harnessruntime.Opener {
			return func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					return &scriptedSession{id: id, cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
						invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
						return harnessruntime.TurnSummary{FinalContent: "done"}, nil
					}}, nil
				}}
			}
		}
		makeQC := func(decision string) func(string, runner.SessionLaunchRequest) harnessruntime.Opener {
			return func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
				return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
					return &scriptedSession{id: "qc-" + decision, cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
						if decision == "revise" {
							invokeToolMustOK(t, cfg, session.ToolSofaQCOpenIssues, map[string]any{
								"issues": []map[string]any{{"criterion": "c", "code": "bad", "message": "fix me"}},
							})
						} else {
							invokeToolMustOK(t, cfg, session.ToolSofaQCResolveIssues, map[string]any{
								"ids": []string{"QC-001"}, "resolution_evidence": "fixed",
							})
						}
						invokeToolMustOK(t, cfg, session.ToolSofaQCSubmitVerdict, map[string]any{"decision": decision, "summary": decision})
						return harnessruntime.TurnSummary{FinalContent: decision}, nil
					}}, nil
				}}
			}
		}

		launcher := &scriptedSessionLauncher{
			timeline: &timeline,
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				makeWork("w"),
				makeWork("w"),
				makeQC("revise"),
				makeWork("w"),
				makeQC("pass"),
			},
		}
		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: gkEngine, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-qc-gk", NodeAddr: "worker.impl", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			MaxGatekeeperReworks: 1,
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
				Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
				QC: &workerspec.QCConfig{
					Sandbox:                 baseSandboxRef("qc"),
					Criteria:                cty.MapVal(map[string]cty.Value{"c": cty.StringVal("ok")}),
					MaxQCRounds:             2,
					MaxGatekeeperReworks:    2,
					MaxGatekeeperReworksSet: true,
					Gatekeepers:             []workerspec.GatekeeperConfig{{Name: "unit", Command: []string{"go", "test"}, WorkingDir: ".", Timeout: "1m"}},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusSucceeded, out.Status)
		require.Equal(t, []string{
			"launch:work", "close:work", "gatekeeper",
			"launch:work", "close:work", "gatekeeper",
			"launch:qc", "close:qc",
			"launch:work", "close:work", "gatekeeper",
			"launch:qc", "close:qc",
		}, timeline)
		require.Len(t, gkEngine.calls, 3)
		for _, call := range gkEngine.calls {
			assert.Equal(t, "alpine:latest", call.Sandbox.Image, "gatekeeper must run in qc.sandbox")
		}
	})

	t.Run("gatekeeper-only qc completes without QC LLM session or QC ledger", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.nextCommitSHAs = []string{shaCkpt1}

		var timeline []string
		gkEngine := &mockSandboxEngine{timeline: &timeline, failures: []*sandboxruntime.GatekeeperFailure{nil}}
		launcher := &scriptedSessionLauncher{
			timeline: &timeline,
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						return &scriptedSession{id: "w-gk-only", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						}}, nil
					}}
				},
			},
		}
		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: gkEngine, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-gk-only", NodeAddr: "worker.impl", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
				Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
				QC: &workerspec.QCConfig{
					Gatekeepers: []workerspec.GatekeeperConfig{{Name: "unit", Command: []string{"go", "test"}, WorkingDir: ".", Timeout: "1m"}},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusSucceeded, out.Status)
		require.Equal(t, []string{"launch:work", "close:work", "gatekeeper"}, timeline)
		require.Len(t, gkEngine.calls, 1)
		assert.Equal(t, "golang:1.27", gkEngine.calls[0].Sandbox.Image, "gatekeeper falls back to worker sandbox when qc.sandbox omitted")
		snap, err := cp.LoadQCLedgerSnapshot(ctx)
		require.NoError(t, err)
		require.Empty(t, snap, "gatekeeper-only qc must not persist any QC ledger")
	})

	t.Run("per-node max_gatekeeper_reworks = 0 overrides request MaxGatekeeperReworks and budget is independent from max_qc_rounds", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)

		fail := &sandboxruntime.GatekeeperFailure{Name: "unit", Command: []string{"go", "test"}, ExitCode: 1, Stderr: "broken"}
		gkEngine := &mockSandboxEngine{failures: []*sandboxruntime.GatekeeperFailure{fail}}
		launcher := &scriptedSessionLauncher{
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						return &scriptedSession{id: "w-zero", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						}}, nil
					}}
				},
			},
		}
		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: gkEngine, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-zero-gk", NodeAddr: "worker.impl", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			MaxGatekeeperReworks: 5,
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("work"), Harness: baseHarnessRef("copilot"),
				Model: "gpt-5", SystemPrompt: cty.StringVal("sys"), Prompt: cty.StringVal("go"),
				Fallbacks: []workerspec.FallbackConfig{{Name: "up", On: []string{"gatekeeper_exhausted"}, Target: "worker.spec"}},
				QC: &workerspec.QCConfig{
					Criteria:                cty.MapVal(map[string]cty.Value{"c": cty.StringVal("ok")}),
					MaxQCRounds:             3,
					MaxGatekeeperReworks:    0,
					MaxGatekeeperReworksSet: true,
					Gatekeepers:             []workerspec.GatekeeperConfig{{Name: "unit", Command: []string{"go", "test"}, WorkingDir: ".", Timeout: "1m"}},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusFallbackTriggered, out.Status)
		require.Equal(t, "gatekeeper_exhausted", out.FallbackReason)
		require.Len(t, gkEngine.calls, 1)
		require.Len(t, launcher.requests, 1, "only 1 work attempt, 0 reworks, 0 QC sessions")
	})
}

func TestWorkerRunnerSkillRelocationFilteringAndStaging(t *testing.T) {
	ctx := context.Background()

	writeSkill := func(dir, entry, content string) {
		t.Helper()
		p := filepath.Join(dir, entry)
		require.NoError(t, os.MkdirAll(p, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(content), 0o644))
	}

	t.Run("relocates bundle and workspace dirs, stages disabled-filtered copies with exec bits, and leniently filters skills for work and qc", func(t *testing.T) {
		bundleDir := t.TempDir()
		writeSkill(filepath.Join(bundleDir, "config", "skills"), "alpha", "---\nname: \"alpha-custom\"\n---\n# Alpha\n")
		writeSkill(filepath.Join(bundleDir, "config", "skills"), "disabled_one", "---\nname: disabled_one\n---\n# Disabled\n")
		writeSkill(filepath.Join(bundleDir, "config", "skills"), "shebang_skill", "# Shebang Skill\n")
		scriptPath := filepath.Join(bundleDir, "config", "skills", "shebang_skill", "scripts", "run.sh")
		require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
		require.NoError(t, os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok\n"), 0o755))

		writeSkill(filepath.Join(bundleDir, "modules", "sub", "skills"), "mod_skill", "# Mod\n")
		writeSkill(filepath.Join(bundleDir, "external_skills", "abcd1234"), "ext_skill", "# Ext\n")
		writeSkill(filepath.Join(bundleDir, "config", "qc-skills"), "qc_keep", "---\nname: qc-audit\n---\n# QC\n")
		writeSkill(filepath.Join(bundleDir, "config", "qc-skills"), "qc_drop", "---\nname: qc-drop\n---\n# Drop\n")

		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)

		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.nextCommitSHAs = []string{shaCkpt1}
		fakeGit.onPrepare = func(destDir string) {
			writeSkill(filepath.Join(destDir, ".agents", "skills"), "ws_skill", "# Workspace skill\n")
		}

		var (
			workSkillsDir      string
			workSessionCfg     harnessruntime.SessionConfig
			qcSkillsDir        string
			qcSessionCfg       harnessruntime.SessionConfig
			stagedAlphaExists  bool
			stagedDisabledGone bool
			stagedRunMode      os.FileMode
			qcStagedKeepExists bool
			qcStagedDropGone   bool
		)

		launcher := &scriptedSessionLauncher{
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
					workSkillsDir = req.MountLayout.SkillsDir
					if workSkillsDir != "" {
						_, aErr := os.Stat(filepath.Join(workSkillsDir, "dir-00", "alpha", "SKILL.md"))
						stagedAlphaExists = aErr == nil
						_, dErr := os.Stat(filepath.Join(workSkillsDir, "dir-00", "disabled_one"))
						stagedDisabledGone = errors.Is(dErr, os.ErrNotExist)
						if info, sErr := os.Stat(filepath.Join(workSkillsDir, "dir-00", "shebang_skill", "scripts", "run.sh")); sErr == nil {
							stagedRunMode = info.Mode()
						}
					}
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						workSessionCfg = cfg
						return &scriptedSession{id: "w-skills", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						}}, nil
					}}
				},
				func(_ string, req runner.SessionLaunchRequest) harnessruntime.Opener {
					qcSkillsDir = req.MountLayout.SkillsDir
					if qcSkillsDir != "" {
						_, kErr := os.Stat(filepath.Join(qcSkillsDir, "dir-00", "qc_keep", "SKILL.md"))
						qcStagedKeepExists = kErr == nil
						_, dErr := os.Stat(filepath.Join(qcSkillsDir, "dir-00", "qc_drop"))
						qcStagedDropGone = errors.Is(dErr, os.ErrNotExist)
					}
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						qcSessionCfg = cfg
						return &scriptedSession{id: "qc-skills", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaQCSubmitVerdict, map[string]any{"decision": "pass", "summary": "ok"})
							return harnessruntime.TurnSummary{FinalContent: "ok"}, nil
						}}, nil
					}}
				},
			},
		}

		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-skills", NodeAddr: "worker.skills", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			BundleDir: bundleDir,
			Worker: workerspec.Config{
				Sandbox:          baseSandboxRef("work"),
				Harness:          baseHarnessRef("copilot"),
				Model:            "gpt-5",
				SystemPrompt:     cty.StringVal("sys"),
				Prompt:           cty.StringVal("go"),
				SkillDirectories: []string{"config/skills", "modules/sub/skills", "external_skills/abcd1234", "/workspace/.agents/skills", "/workspace/nonexistent-skills"},
				Skills:           []string{"alpha", "disabled_one", "shebang_skill", "mod_skill", "ext_skill", "ws_skill", "missing_skill"},
				DisabledSkills:   []string{"disabled_one"},
				QC: &workerspec.QCConfig{
					Criteria:         cty.MapVal(map[string]cty.Value{"c": cty.StringVal("ok")}),
					MaxQCRounds:      1,
					SkillDirectories: []string{"config/qc-skills", "/workspace/optional-missing"},
					Skills:           []string{"qc_keep", "qc-drop", "absent_qc_skill"},
					DisabledSkills:   []string{"qc-drop"},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusSucceeded, out.Status)

		assert.NotEmpty(t, workSkillsDir)
		assert.True(t, stagedAlphaExists, "enabled skill alpha must be staged in workSkillsDir")
		assert.True(t, stagedDisabledGone, "disabled skill disabled_one must be absent from workSkillsDir")
		if goruntime.GOOS != "windows" {
			assert.NotZero(t, stagedRunMode.Perm()&0o111, "staged run.sh must keep +x executable bit")
		}
		assert.Equal(t, []string{
			"/.sofa/skills/dir-00",
			"/.sofa/bundle/modules/sub/skills",
			"/.sofa/bundle/external_skills/abcd1234",
			"/workspace/.agents/skills",
			"/workspace/nonexistent-skills",
		}, workSessionCfg.SkillDirectories)
		assert.Equal(t, []string{"alpha-custom", "shebang_skill", "mod_skill", "ext_skill", "ws_skill"}, workSessionCfg.Skills)
		assert.Equal(t, []string{"disabled_one"}, workSessionCfg.DisabledSkills)

		assert.NotEmpty(t, qcSkillsDir)
		assert.NotEqual(t, workSkillsDir, qcSkillsDir)
		assert.True(t, qcStagedKeepExists)
		assert.True(t, qcStagedDropGone)
		assert.Equal(t, []string{"/.sofa/skills/dir-00", "/workspace/optional-missing"}, qcSessionCfg.SkillDirectories)
		assert.Equal(t, []string{"qc-audit"}, qcSessionCfg.Skills)
		assert.Equal(t, []string{"qc-drop"}, qcSessionCfg.DisabledSkills)
	})

	t.Run("legacy direct caller passing config dir as BundleDir relocates config/skills to /.sofa/bundle/skills", func(t *testing.T) {
		legacyConfigDir := t.TempDir()
		writeSkill(filepath.Join(legacyConfigDir, "skills"), "legacy_skill", "# Legacy\n")

		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.nextCommitSHAs = []string{shaCkpt1}

		var workCfg harnessruntime.SessionConfig
		launcher := &scriptedSessionLauncher{
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						workCfg = cfg
						return &scriptedSession{id: "w-legacy", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						}}, nil
					}}
				},
			},
		}

		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-legacy-skills", NodeAddr: "worker.legacy", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			BundleDir: legacyConfigDir,
			Worker: workerspec.Config{
				Sandbox:          baseSandboxRef("work"),
				Harness:          baseHarnessRef("copilot"),
				Model:            "gpt-5",
				SystemPrompt:     cty.StringVal("sys"),
				Prompt:           cty.StringVal("go"),
				SkillDirectories: []string{"config/skills"},
				Skills:           []string{"legacy_skill"},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusSucceeded, out.Status)
		assert.Equal(t, []string{"/.sofa/bundle/skills"}, workCfg.SkillDirectories)
		assert.Equal(t, []string{"legacy_skill"}, workCfg.Skills)
	})

	t.Run("container-image-baked unmapped skill_directories preserves non-disabled skills on host", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(shaBase)
		fakeGit.nextCommitSHAs = []string{shaCkpt1}

		var workCfg harnessruntime.SessionConfig
		launcher := &scriptedSessionLauncher{
			openers: []func(string, runner.SessionLaunchRequest) harnessruntime.Opener{
				func(_ string, _ runner.SessionLaunchRequest) harnessruntime.Opener {
					return &scriptedOpener{onOpen: func(cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
						workCfg = cfg
						return &scriptedSession{id: "w-baked", cfg: cfg, onTurn: func(_ string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
							invokeToolMustOK(t, cfg, session.ToolSofaCompleteWork, map[string]any{"summary": "done"})
							return harnessruntime.TurnSummary{FinalContent: "done"}, nil
						}}, nil
					}}
				},
			},
		}

		r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane: cp, GitAdapter: fakeGit, BlobStore: blobStore,
			SandboxEngine: &mockSandboxEngine{}, SessionLauncher: launcher,
		})
		require.NoError(t, err)

		out, err := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: "run-baked-skills", NodeAddr: "worker.baked", Epoch: 1,
			RepoURL: "https://github.com/acme/repo.git", BasedSource: "main",
			Sandboxes: testSandboxes(), Harnesses: testHarnesses(),
			Worker: workerspec.Config{
				Sandbox:          baseSandboxRef("work"),
				Harness:          baseHarnessRef("copilot"),
				Model:            "gpt-5",
				SystemPrompt:     cty.StringVal("sys"),
				Prompt:           cty.StringVal("go"),
				SkillDirectories: []string{"/opt/image-skills"},
				Skills:           []string{"baked-skill", "disabled-baked"},
				DisabledSkills:   []string{"disabled-baked"},
			},
		})
		require.NoError(t, err)
		require.Equal(t, runner.StatusSucceeded, out.Status)
		assert.Equal(t, []string{"/opt/image-skills"}, workCfg.SkillDirectories)
		assert.Equal(t, []string{"baked-skill"}, workCfg.Skills)
		assert.Equal(t, []string{"disabled-baked"}, workCfg.DisabledSkills)
	})
}
