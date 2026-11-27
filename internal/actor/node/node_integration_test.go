//go:build integration

package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

type itHTTPServer struct {
	port  int
	svc   common.Service
	errCh chan error
}

func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func startHTTPService(t *testing.T, srv *itHTTPServer) {
	t.Helper()
	go func() {
		err := srv.svc.Start()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.errCh <- err
		}
		close(srv.errCh)
	}()
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", srv.port), 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 15*time.Millisecond)
}

func (s *itHTTPServer) stop(t *testing.T) {
	t.Helper()
	if s.svc != nil {
		_ = s.svc.Stop()
	}
	select {
	case err := <-s.errCh:
		if err != nil {
			t.Logf("callback server exited with: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for callback server shutdown")
	}
}

func runGitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initBareRepoForNode(t *testing.T) (bareDir, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")

	runGitCLI(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runGitCLI(t, seedDir, "init")
	runGitCLI(t, seedDir, "config", "user.name", "seed")
	runGitCLI(t, seedDir, "config", "user.email", "seed@sofa.local")
	runGitCLI(t, seedDir, "config", "commit.gpgsign", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(seedDir, "allowed"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "allowed", "base.txt"), []byte("base\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "initial")
	runGitCLI(t, seedDir, "branch", "-M", "main")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	baseSHA = runGitCLI(t, seedDir, "rev-parse", "HEAD")
	return bareDir, baseSHA
}

type ledgerRunNotifier struct {
	mu     sync.Mutex
	ledger *git.Ledger
	events []node.NodeEventNotification
}

func (n *ledgerRunNotifier) RegisterManagedBranch(_ context.Context, req node.RegisterManagedBranchReq) error {
	return n.ledger.Register(req.RepoURL, req.Branch, req.BaseSHA, req.Kind, req.NodeAddr, req.Epoch)
}

func (n *ledgerRunNotifier) NotifyNodeEvent(_ context.Context, ev node.NodeEventNotification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	return nil
}

type itSandboxEngine struct{}

func (e *itSandboxEngine) DetectPlatform(_ context.Context) (sandboxruntime.Platform, error) {
	return harnessinstaller.Platform{OS: "linux", Arch: "amd64"}, nil
}

func (e *itSandboxEngine) Exec(_ context.Context, _, _ string, _ []string, _ map[string]string, _ io.Reader) (sandboxruntime.ExecResult, error) {
	return sandboxruntime.ExecResult{}, nil
}

func (e *itSandboxEngine) RunGatekeepers(_ context.Context, _ sandboxruntime.GatekeeperRunSpec, _ []workerspec.GatekeeperConfig) (*sandboxruntime.GatekeeperFailure, error) {
	return nil, nil
}

type itSessionHandle struct {
	opener harnessruntime.Opener
	closed *atomic.Int32
}

func (h *itSessionHandle) Opener() harnessruntime.Opener { return h.opener }
func (h *itSessionHandle) ContainerID() string           { return "it-fake-ctr" }
func (h *itSessionHandle) Close() error {
	h.closed.Add(1)
	return nil
}

type itFakeOpener struct {
	onOpen func(cfg harnessruntime.SessionConfig) harnessruntime.Session
}

func (o *itFakeOpener) Capabilities(_ context.Context) (harnessruntime.Capabilities, error) {
	return harnessruntime.Capabilities{HarnessName: "fake", SupportsSessionResume: true}, nil
}

func (o *itFakeOpener) Open(_ context.Context, cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
	return o.onOpen(cfg), nil
}

type itFakeSession struct {
	id     string
	cfg    harnessruntime.SessionConfig
	onTurn func(prompt string, cfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error)
}

func (s *itFakeSession) ID() string { return s.id }
func (s *itFakeSession) RunTurn(_ context.Context, prompt string, _ func(harnessruntime.Event)) (harnessruntime.TurnSummary, error) {
	return s.onTurn(prompt, s.cfg)
}
func (s *itFakeSession) Abort(_ context.Context) error  { return nil }
func (s *itFakeSession) Resume(_ context.Context) error { return nil }
func (s *itFakeSession) ReadMCPResource(_ context.Context, _, _ string) ([]harnessruntime.MCPResourceContent, error) {
	return nil, nil
}
func (s *itFakeSession) Close(_ context.Context) error { return nil }

func invokeToolJSON(t *testing.T, cfg harnessruntime.SessionConfig, name string, payload any) {
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
	require.Empty(t, out.HandlerError, "tool %s error: %s", name, out.TextResultForLLM)
}

type daprNodeOutcomeReporter struct {
	client daprclient.Client
}

func (r *daprNodeOutcomeReporter) ReportNodeOutcome(ctx context.Context, report daemon.NodeOutcomeReport) error {
	stub, err := node.NewClientStubForNode(report.RunID, report.NodeAddr, r.client)
	if err != nil {
		return err
	}
	_, err = stub.ReportNodeOutcome(ctx, &report)
	return err
}

func TestNodeActorAndRemoteControlPlane_RealDaprPauseAndCrossEpochResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rootDir := t.TempDir()
	nrDBPath := filepath.Join(rootDir, "nameresolution.db")
	blobStore, err := blob.NewLocalStore(filepath.Join(rootDir, "blobs"))
	require.NoError(t, err)

	bareDir, baseSHA := initBareRepoForNode(t)
	runID := "run-it-025"
	nodeAddr := `dynamic_worker.impl["feat/a"]`

	ledger := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	runNotifier := &ledgerRunNotifier{ledger: ledger}

	// 1. Control-plane Dapr sidecar hosting ConfigActor, ProcessRegistryActor, and NodeActor.
	var cpClientRef atomic.Pointer[daprclient.Client]
	cpClientProvider := func() daprclient.Client {
		c := cpClientRef.Load()
		if c == nil {
			return nil
		}
		return *c
	}

	cpPort := reservePort(t)
	var cpExpectedToken atomic.Value
	cpExpectedToken.Store("")
	cpRouter := chi.NewRouter()
	cpRouter.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := cpExpectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	cpSvc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", cpPort), cpRouter)
	cpSvc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return config.NewConfigActor(cpClientProvider)
	})
	cpSvc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return registry.NewProcessRegistryActor(registry.Options{ClientProvider: cpClientProvider})
	})
	cpSvc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return node.NewNodeActor(node.Options{
			ClientProvider:    cpClientProvider,
			RunNotifier:       runNotifier,
			SlotRetryInterval: 100 * time.Millisecond,
		})
	})
	cpSrv := &itHTTPServer{port: cpPort, svc: cpSvc, errCh: make(chan error, 1)}
	startHTTPService(t, cpSrv)
	defer cpSrv.stop(t)

	mgrCP, err := daprhost.NewManager(daprhost.Options{
		StateDir:             filepath.Join(rootDir, "cp"),
		AppID:                "sofa-control-plane",
		AppPort:              cpPort,
		NameResolutionDBPath: nrDBPath,
	})
	require.NoError(t, err)
	cpExpectedToken.Store(mgrCP.AppAPIToken())
	cpClient, err := mgrCP.Start(ctx)
	require.NoError(t, err)
	cpClientRef.Store(&cpClient)
	defer func() {
		_ = mgrCP.Stop(context.Background())
	}()

	// Seed managed credential in ConfigActor.
	cfgStub := config.NewClientStub(cpClient)
	_, err = cfgStub.UpsertEntries(ctx, &config.UpsertEntriesReq{
		Scope: config.ScopeGlobal,
		Entries: []config.EntryInput{
			{Name: "NODE_MANAGED_SECRET", Value: "managed-val-42", IsSecret: true},
		},
	})
	require.NoError(t, err)

	// 2. Worker Dapr sidecar hosting WorkerDaemon + real WorkerRunner + RemoteNodeControlPlane.
	wPort := reservePort(t)
	var wExpectedToken atomic.Value
	wExpectedToken.Store("")
	wRouter := chi.NewRouter()
	wRouter.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := wExpectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/dapr/config" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"entities":[]}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	wSvc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", wPort), wRouter)
	wSrv := &itHTTPServer{port: wPort, svc: wSvc, errCh: make(chan error, 1)}
	startHTTPService(t, wSrv)
	defer wSrv.stop(t)

	mgrW, err := daprhost.NewManager(daprhost.Options{
		StateDir:              filepath.Join(rootDir, "w1"),
		AppID:                 "sofa-worker-it-node",
		AppPort:               wPort,
		ExternalSchedulerAddr: mgrCP.SchedulerAddress(),
		NameResolutionDBPath:  nrDBPath,
	})
	require.NoError(t, err)
	wExpectedToken.Store(mgrW.AppAPIToken())
	wClient, err := mgrW.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = mgrW.Stop(context.Background())
	}()

	var (
		mu              sync.Mutex
		observedEpochs  []uint64
		observedEnvVal  string
		epoch2Prompt    string
		epoch2SysPrompt string
		step1InEpoch2   bool
		handlesClosed   atomic.Int32
	)

	workerDaemon, err := daemon.New(daemon.Options{
		ProcessID:         "proc-node-it",
		AppID:             "sofa-worker-it-node",
		FixedAppID:        "sofa-worker-it-node",
		MaxConcurrency:    2,
		Arch:              "linux/amd64",
		Registry:          daemon.NewDaprRegistryClient(wClient),
		PubSub:            daemon.NewDaprPubSubPublisher(wClient),
		BlobStore:         blobStore,
		NodeReporter:      &daprNodeOutcomeReporter{client: wClient},
		HeartbeatInterval: 250 * time.Millisecond,
		TempDir:           filepath.Join(rootDir, "worker-tmp"),
		NodeRunnerDepsProvider: func(_ context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error) {
			remoteCP, cpErr := node.NewRemoteNodeControlPlaneForNode(wClient, req.RunID, req.NodeAddr, req.Epoch)
			if cpErr != nil {
				return runner.WorkerRunnerDeps{}, cpErr
			}
			mu.Lock()
			observedEpochs = append(observedEpochs, req.Epoch)
			if v, ok := req.LookupEnv("NODE_MANAGED_SECRET"); ok {
				observedEnvVal = v
			}
			mu.Unlock()

			return runner.WorkerRunnerDeps{
				ControlPlane:  remoteCP,
				GitAdapter:    git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: req.LookupEnv}),
				BlobStore:     blobStore,
				SandboxEngine: &itSandboxEngine{},
				SessionLauncher: sessionLauncherFunc(func(_ context.Context, launchReq runner.SessionLaunchRequest) (runner.SessionHandle, error) {
					wsDir := launchReq.MountLayout.HostWorkspaceDir
					opener := &itFakeOpener{
						onOpen: func(cfg harnessruntime.SessionConfig) harnessruntime.Session {
							return &itFakeSession{
								id:  fmt.Sprintf("sess-e%d", req.Epoch),
								cfg: cfg,
								onTurn: func(prompt string, scfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
									if req.Epoch == 1 {
										require.NoError(t, os.WriteFile(filepath.Join(wsDir, "allowed", "step1.txt"), []byte("step1-ok\n"), 0o644))
										invokeToolJSON(t, scfg, session.ToolSofaCheckpoint, session.DiaryInput{
											Completed: []string{"wrote allowed/step1.txt"},
											NextSteps: []string{"ask human for output format"},
										})
										invokeToolJSON(t, scfg, session.ToolSofaRequestDecision, session.DecisionRequest{
											Question:    "Which format should we write?",
											Options:     []string{"json", "yaml"},
											Recommended: "yaml",
										})
										return harnessruntime.TurnSummary{}, nil
									}

									// Epoch 2: recovered workspace + diary context + human decision interjection!
									_, statErr := os.Stat(filepath.Join(wsDir, "allowed", "step1.txt"))
									mu.Lock()
									step1InEpoch2 = statErr == nil
									epoch2Prompt = prompt
									epoch2SysPrompt = scfg.SystemPrompt
									mu.Unlock()

									require.NoError(t, os.WriteFile(filepath.Join(wsDir, "allowed", "final.txt"), []byte("format=yaml\n"), 0o644))
									invokeToolJSON(t, scfg, session.ToolSofaCompleteWork, map[string]string{
										"summary": "completed using human decision",
									})
									return harnessruntime.TurnSummary{}, nil
								},
							}
						},
					}
					return &itSessionHandle{opener: opener, closed: &handlesClosed}, nil
				}),
			}, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, workerDaemon.RegisterHandlers(wSvc))
	require.NoError(t, workerDaemon.Start(ctx))
	defer func() {
		_ = workerDaemon.Stop(context.Background())
	}()

	// 3. Dispatch NodeActor at epoch=1 -> WorkerRunner checkpoints & pauses at WAITING_HUMAN.
	nodeStub, err := node.NewClientStubForNode(runID, nodeAddr, cpClient)
	require.NoError(t, err)

	startResp, err := nodeStub.StartOrResume(ctx, &node.StartOrResumeReq{
		RunID:           runID,
		NodeAddr:        nodeAddr,
		Namespace:       "default",
		RequiredEnvKeys: []string{"NODE_MANAGED_SECRET"},
		RepoURL:         bareDir,
		BasedSource:     "main",
		BaseSHA:         baseSHA,
		RequiredArch:    "linux/amd64",
		Sandboxes: map[string]sandboxspec.SandboxConfig{
			"sandbox.alpine": {Runtime: "podman", Image: "alpine:latest", Network: "bridge"},
		},
		Harnesses: map[string]harnessspec.HarnessConfig{
			"harness.fake": {Name: "fake"},
		},
		Worker: workerspec.Config{
			Sandbox: cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("sandbox.alpine"),
				"kind":    cty.StringVal("sandbox"),
				"name":    cty.StringVal("alpine"),
			}),
			Harness: cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("harness.fake"),
				"kind":    cty.StringVal("harness"),
				"name":    cty.StringVal("fake"),
			}),
			Model:         "fake-model",
			SystemPrompt:  cty.StringVal("sys"),
			Prompt:        cty.StringVal("do work"),
			WritablePaths: []string{"allowed/**"},
			HumanApproval: "optional",
		},
	})
	require.NoError(t, err)
	assert.True(t, startResp.Accepted)
	assert.Equal(t, uint64(1), startResp.Epoch)

	// Wait for WAITING_HUMAN + released slot + closed session handle.
	var waitingState *node.GetNodeStateResp
	require.Eventually(t, func() bool {
		st, gErr := nodeStub.GetState(ctx, &node.GetNodeStateReq{})
		if gErr != nil {
			return false
		}
		if st.Status == node.StatusWaitingHuman {
			waitingState = st
			return true
		}
		return false
	}, 15*time.Second, 50*time.Millisecond)

	require.NotNil(t, waitingState)
	require.Len(t, waitingState.DecisionCards, 1)
	card := waitingState.DecisionCards[0]
	assert.Equal(t, node.DecisionKindRequestDecision, card.Kind)
	assert.Equal(t, node.DecisionStatusPending, card.Status)
	assert.Equal(t, int32(1), handlesClosed.Load())

	regStub := registry.NewClientStub(cpClient)
	require.Eventually(t, func() bool {
		lr, lErr := regStub.ListProcesses(ctx, &registry.ListProcessesReq{})
		if lErr != nil || len(lr.Processes) == 0 {
			return false
		}
		return lr.Processes[0].ActiveSlots == 0
	}, 5*time.Second, 25*time.Millisecond)

	// 4. Submit human decision -> NodeActor auto-increments epoch=2, recovers checkpoint, and reaches SUCCEEDED!
	subResp, err := nodeStub.SubmitDecision(ctx, &node.SubmitDecisionReq{
		CardID:         card.CardID,
		SelectedOption: "yaml",
		AnswerText:     "use yaml format",
	})
	require.NoError(t, err)
	assert.True(t, subResp.Accepted)
	assert.True(t, subResp.AutoResumed)
	assert.Equal(t, uint64(2), subResp.Epoch)

	var finalState *node.GetNodeStateResp
	require.Eventually(t, func() bool {
		st, gErr := nodeStub.GetState(ctx, &node.GetNodeStateReq{})
		if gErr != nil {
			return false
		}
		if st.Status == node.StatusSucceeded {
			finalState = st
			return true
		}
		return false
	}, 15*time.Second, 50*time.Millisecond)

	require.NotNil(t, finalState)
	assert.Equal(t, uint64(2), finalState.CurrentEpoch)
	require.NotNil(t, finalState.Outcome)
	expectedWorkBranch := git.WorkBranchName(runID, nodeAddr)
	assert.Equal(t, expectedWorkBranch, finalState.Outcome.WorkBranch)
	assert.NotEmpty(t, finalState.Outcome.CommitSHA)

	mu.Lock()
	assert.Equal(t, []uint64{1, 2}, observedEpochs)
	assert.Equal(t, "managed-val-42", observedEnvVal)
	assert.True(t, step1InEpoch2, "epoch 2 must restore allowed/step1.txt from epoch 1 checkpoint")
	assert.Contains(t, epoch2SysPrompt, "wrote allowed/step1.txt")
	assert.Contains(t, epoch2Prompt, "use yaml format")
	mu.Unlock()

	// Verify real bare Git remote received work_branch with both step1.txt and final.txt,
	// and RunNotifier's git.Ledger recorded both checkpoint branches and the work_branch.
	verifyDir := filepath.Join(t.TempDir(), "verify-work")
	cliGit := git.NewCLIAdapter(git.CLIAdapterOptions{})
	require.NoError(t, cliGit.PrepareWorkspace(ctx, bareDir, expectedWorkBranch, finalState.Outcome.CommitSHA, verifyDir, git.AuthConfig{}))
	b1, err := os.ReadFile(filepath.Join(verifyDir, "allowed", "step1.txt"))
	require.NoError(t, err)
	assert.Equal(t, "step1-ok\n", strings.ReplaceAll(string(b1), "\r\n", "\n"))
	b2, err := os.ReadFile(filepath.Join(verifyDir, "allowed", "final.txt"))
	require.NoError(t, err)
	assert.Equal(t, "format=yaml\n", strings.ReplaceAll(string(b2), "\r\n", "\n"))

	require.NoError(t, ledger.EnsureRegistered(bareDir, expectedWorkBranch))
	require.NoError(t, ledger.EnsureRegistered(bareDir, git.CheckpointBranchName(runID, nodeAddr, 1)))
	require.NoError(t, ledger.EnsureRegistered(bareDir, git.CheckpointBranchName(runID, nodeAddr, 2)))
}

type sessionLauncherFunc func(ctx context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error)

func (f sessionLauncherFunc) Launch(ctx context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error) {
	return f(ctx, req)
}
