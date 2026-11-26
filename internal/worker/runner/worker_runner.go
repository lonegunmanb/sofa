package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/session"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

// WorkerRunner orchestrates single-node Work -> Freeze -> Gatekeeper -> Read-Only QC -> Publish execution.
type WorkerRunner struct {
	deps WorkerRunnerDeps
}

// NewWorkerRunner validates deps and constructs a WorkerRunner.
func NewWorkerRunner(deps WorkerRunnerDeps) (*WorkerRunner, error) {
	if deps.ControlPlane == nil {
		return nil, errors.New("worker runner: control_plane is required")
	}
	if deps.GitAdapter == nil {
		return nil, errors.New("worker runner: git_adapter is required")
	}
	if deps.BlobStore == nil {
		return nil, errors.New("worker runner: blob_store is required")
	}
	return &WorkerRunner{deps: deps}, nil
}

func (r *WorkerRunner) artifactService() *artifact.Service {
	if r.deps.ArtifactSvc != nil {
		return r.deps.ArtifactSvc
	}
	return artifact.NewService(r.deps.BlobStore, artifact.ServiceOptions{})
}

func (r *WorkerRunner) sandboxEngine() sandboxruntime.Engine {
	if r.deps.SandboxEngine != nil {
		return r.deps.SandboxEngine
	}
	return sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{
		GitAdapter: r.deps.GitAdapter,
	})
}

func (r *WorkerRunner) sessionLauncher() SessionLauncher {
	if r.deps.SessionLauncher != nil {
		return r.deps.SessionLauncher
	}
	return &ContainerSessionLauncher{
		Installer: harnessinstaller.NewInstaller(),
	}
}

func (r *WorkerRunner) toolExecForContainer(containerID string) toolruntime.ContainerExecFunc {
	if r.deps.ToolExecFactory != nil {
		return r.deps.ToolExecFactory(containerID)
	}
	if strings.TrimSpace(containerID) == "" {
		return nil
	}
	return toolruntime.NewPodmanContainerExec(r.sandboxEngine(), containerID)
}

// Run executes a single static or dynamically-expanded worker node end-to-end.
func (r *WorkerRunner) Run(ctx context.Context, req NodeExecutionRequest) (NodeOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return NodeOutcome{Status: StatusFailed, FailureReason: "run_id is required"}, errors.New("worker runner: run_id is required")
	}
	if strings.TrimSpace(req.NodeAddr) == "" {
		return NodeOutcome{Status: StatusFailed, FailureReason: "node_addr is required"}, errors.New("worker runner: node_addr is required")
	}
	if req.Epoch == 0 {
		return NodeOutcome{Status: StatusFailed, FailureReason: "epoch must be > 0"}, errors.New("worker runner: epoch must be > 0")
	}
	if strings.TrimSpace(req.RepoURL) == "" {
		return NodeOutcome{Status: StatusFailed, FailureReason: "repo_url is required"}, errors.New("worker runner: repo_url is required")
	}

	basedSource, err := resolveStringOrCty(req.BasedSource, req.Worker.BasedSource, "based_source")
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	systemPrompt, err := resolveStringOrCty(req.SystemPrompt, req.Worker.SystemPrompt, "system_prompt")
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	initialPrompt := resolveOptionalStringOrCty(req.Prompt, req.Worker.Prompt)
	lookupEnv := req.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	hostOS := strings.TrimSpace(req.HostOS)
	if hostOS == "" {
		hostOS = goruntime.GOOS
	}

	workSandbox, err := lookupSandbox(req.Sandboxes, req.Worker.Sandbox, "worker sandbox")
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	workHarness, err := lookupHarness(req.Harnesses, req.Worker.Harness, "worker harness")
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	workProvider, hasWorkProvider, err := lookupOptionalProvider(req.ModelProviders, req.Worker.ModelProvider, "worker model_provider")
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	workRetry, err := computeRetryPolicy(hasWorkProvider, workProvider, req.Worker.Retry, nil)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}

	platform, err := r.resolveTargetPlatform(ctx, req)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}

	// Step 1: Prepare isolated host working directory and clean up unconditionally on return.
	baseTempRoot := strings.TrimSpace(req.TempRoot)
	if baseTempRoot == "" {
		baseTempRoot = os.TempDir()
	}
	execRoot := filepath.Join(
		baseTempRoot,
		sanitizePathSegment(req.RunID, "run"),
		fmt.Sprintf("%s-e%d-%s", sanitizeNodeDirSegment(req.NodeAddr), req.Epoch, uuid.NewString()[:8]),
	)
	workspaceDir := filepath.Join(execRoot, "workspace")
	importsDir := filepath.Join(execRoot, "imports")
	artifactsDir := filepath.Join(execRoot, "artifacts")
	workStoreDir := filepath.Join(execRoot, "work-session-store")
	qcStoreDir := filepath.Join(execRoot, "qc-session-store")
	workToolsDir := filepath.Join(execRoot, "tools", "work")
	qcToolsDir := filepath.Join(execRoot, "tools", "qc")

	for _, dir := range []string{importsDir, artifactsDir, workStoreDir, qcStoreDir, workToolsDir, qcToolsDir} {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: mkErr.Error()}, fmt.Errorf("create worker temp dir %q: %w", dir, mkErr)
		}
	}
	defer func() {
		_ = os.RemoveAll(execRoot)
	}()

	artSvc := r.artifactService()
	ckptMgr, err := checkpoint.NewManager(checkpoint.ManagerConfig{
		RunID:       req.RunID,
		NodeAddr:    req.NodeAddr,
		RepoURL:     req.RepoURL,
		Auth:        req.Auth,
		StateStore:  r.deps.ControlPlane,
		GitAdapter:  r.deps.GitAdapter,
		BlobStore:   r.deps.BlobStore,
		ArtifactSvc: artSvc,
	})
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}

	var qcLedger *session.QCLedger
	var qcCriteria map[string]string
	if req.Worker.QC != nil {
		qcCriteria, err = extractQCCriteria(req.Worker.QC.Criteria)
		if err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
		qcLedger, err = session.NewQCLedger(qcCriteria)
		if err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
	}

	baseSHA := strings.TrimSpace(req.BaseSHA)
	var diaryContext string
	recRes, recErr := ckptMgr.RecoverLatestAligned(ctx, req.Epoch, workspaceDir, artifactsDir)
	switch {
	case recErr == nil:
		diaryContext = recRes.DiaryPromptContext
		if qcLedger != nil {
			snapBytes, snapErr := r.deps.ControlPlane.LoadQCLedgerSnapshot(ctx)
			if snapErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: snapErr.Error()}, snapErr
			}
			if len(snapBytes) > 0 {
				if restErr := qcLedger.Restore(snapBytes); restErr != nil {
					return NodeOutcome{Status: StatusFailed, FailureReason: restErr.Error()}, restErr
				}
			}
		}
		pendingPub, pendErr := r.deps.ControlPlane.LoadPendingPublication(ctx)
		if pendErr != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: pendErr.Error()}, pendErr
		}
		if pendingPub != nil {
			shouldShortCircuit := false
			if req.Worker.HumanApproval == "required" && pendingPub.CommitSHA == recRes.Record.FinalSHA {
				hasInterj, ierr := r.deps.ControlPlane.HasPendingInterjections(ctx)
				if ierr != nil {
					return NodeOutcome{Status: StatusFailed, FailureReason: ierr.Error()}, ierr
				}
				if !hasInterj {
					ok, verErr := verifyPendingArtifactsInBlobStore(ctx, r.deps.BlobStore, pendingPub.Artifacts)
					if verErr != nil {
						return NodeOutcome{Status: StatusFailed, FailureReason: verErr.Error()}, verErr
					}
					shouldShortCircuit = ok
				}
			}
			if shouldShortCircuit {
				effBaseSHA := baseSHA
				if effBaseSHA == "" {
					effBaseSHA = pendingPub.BaseSHA
				}
				return r.handleApprovalAndPublish(
					ctx, req, basedSource, effBaseSHA, workspaceDir,
					pendingPub.CommitSHA, pendingPub.Artifacts, pendingPub.ArtifactIDs, pendingPub.ResultJSON,
				)
			}
			if clearErr := r.deps.ControlPlane.SavePendingPublication(ctx, req.Epoch, nil); clearErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: clearErr.Error()}, clearErr
			}
		}

	case errors.Is(recErr, checkpoint.ErrNoAlignedCheckpoint):
		if baseSHA == "" {
			baseSHA, err = r.deps.GitAdapter.ResolveBranchSHA(ctx, req.RepoURL, basedSource, req.Auth)
			if err != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
			}
		}
		if err := r.deps.GitAdapter.PrepareWorkspace(ctx, req.RepoURL, basedSource, baseSHA, workspaceDir, req.Auth); err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}

	default:
		return NodeOutcome{Status: StatusFailed, FailureReason: recErr.Error()}, recErr
	}

	if err := artSvc.StageImports(ctx, importsDir, req.Imports); err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}

	artifactDecls := sortedArtifactDecls(req.Worker.Artifacts)
	workBindings, workGoTools, err := resolveToolBindings(req.Worker.ToolIDs, req.Worker.TerminateToolID, req.Worker.ToolUses, req.Tools, req.ToolEnvs)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	workMCPServers, err := materializeMCPServers(req.MCPServers, req.Worker.MCPToolIDs, req.Worker.MCPResourceIDs, lookupEnv)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	workMCPResources := filterMCPResources(req.MCPResources, req.Worker.MCPResourceIDs)

	var matWorkProvider *harnessruntime.ProviderConfig
	if hasWorkProvider {
		p, matErr := harnessruntime.MaterializeProviderConfig(workProvider, req.Worker.Model, effectiveModelID(req.Worker.Model, req.Worker.Profile), lookupEnv)
		if matErr != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: matErr.Error()}, matErr
		}
		matWorkProvider = &p
	}
	matWorkHarnessEnv, err := harnessruntime.MaterializeHarnessEnv(workHarness.EnvRefs, lookupEnv)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	workHarnessOpts, err := harnessruntime.MarshalHarnessOptions(workHarness.Options)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}

	maxGKReworks := req.MaxGatekeeperReworks
	if maxGKReworks <= 0 {
		maxGKReworks = DefaultMaxGatekeeperReworks
	}
	maxArtReworks := req.MaxArtifactReworks
	if maxArtReworks <= 0 {
		maxArtReworks = DefaultMaxArtifactReworks
	}

	var (
		lastWorkSessionID string
		lastDiary         *session.DiaryInput
		resultJSON        json.RawMessage
		reworkPrompt      string
		artifactReworks   int
		gatekeeperReworks int
		qcRound           int
	)

	// Step 2: Main Work -> Freeze -> Artifact Validate -> Gatekeeper -> Read-Only QC rework loop.
	for {
		effectiveSysPrompt := systemPrompt
		turnPrompt := initialPrompt
		resumeID := ""
		if lastWorkSessionID != "" {
			resumeID = lastWorkSessionID
			turnPrompt = reworkPrompt
		} else if diaryContext != "" {
			effectiveSysPrompt = appendPromptSection(effectiveSysPrompt, diaryContext)
		}

		workLayout := sandboxruntime.SessionMountLayout{
			Sandbox:           workSandbox,
			HostOS:            hostOS,
			ForAgentSession:   true,
			Entrypoint:        "/.sofa/harness/sofa-harness-" + workHarness.Name,
			WorkingDir:        ".",
			HostWorkspaceDir:  workspaceDir,
			ReadOnlyWorkspace: false,
			WritablePaths:     slices.Clone(req.Worker.WritablePaths),
			BundleDir:         req.BundleDir,
			ImportsDir:        importsDir,
			ArtifactsDir:      artifactsDir,
			ReadOnlyArtifacts: false,
			ToolsDir:          workToolsDir,
			SessionStoreDir:   workStoreDir,
		}

		workBaseSession := harnessruntime.SessionConfig{
			SessionID:             lastWorkSessionID,
			ResumeSessionID:       resumeID,
			Model:                 req.Worker.Model,
			Profile:               req.Worker.Profile,
			ReasoningEffort:       req.Worker.ReasoningEffort,
			SystemPrompt:          effectiveSysPrompt,
			WorkingDirectory:      sandboxruntime.DefaultContainerWorkingDir,
			SessionStoreDirectory: "/.sofa/session-store",
			PermissionMode:        effectivePermission(req.Worker.Permission),
			Provider:              matWorkProvider,
			HarnessEnv:            matWorkHarnessEnv,
			HarnessOptionsJSON:    workHarnessOpts,
			AvailableTools:        toToolFilter(req.Worker.AllowedTools),
			ExcludedTools:         toToolFilter(req.Worker.DisallowedTools),
			SkillDirectories:      slices.Clone(req.Worker.SkillDirectories),
			Skills:                slices.Clone(req.Worker.Skills),
			DisabledSkills:        slices.Clone(req.Worker.DisabledSkills),
			MCPServers:            workMCPServers,
		}

		workRes, workErr := r.runWorkAttempt(ctx, workAttemptParams{
			Req:          req,
			Sandbox:      workSandbox,
			Harness:      workHarness,
			MountLayout:  workLayout,
			GoTools:      workGoTools,
			Platform:     platform,
			BaseSession:  workBaseSession,
			Prompt:       turnPrompt,
			DiarySummary: diaryContext,
			ToolBindings: workBindings,
			MCPResources: workMCPResources,
			RetryPolicy:  workRetry,
			WorkspaceDir: workspaceDir,
			ImportsDir:   importsDir,
			ArtifactsDir: artifactsDir,
			CheckpointFn: func(cctx context.Context, d session.DiaryInput) error {
				_, saveErr := ckptMgr.SaveCheckpoint(cctx, req.Epoch, workspaceDir, artifactsDir, req.Worker.WritablePaths, d)
				return saveErr
			},
		})
		if workErr != nil {
			if rbErr := r.rollbackUnauthorizedIfPresent(ctx, workspaceDir, req.Worker.WritablePaths); rbErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: rbErr.Error()}, rbErr
			}
			if errors.Is(workErr, session.ErrProtocolAttemptsExhausted) {
				return NodeOutcome{Status: StatusFailed, FailureReason: workErr.Error()}, nil
			}
			return NodeOutcome{Status: StatusFailed, FailureReason: workErr.Error()}, workErr
		}

		if workRes.SessionID != "" {
			lastWorkSessionID = workRes.SessionID
		}
		if len(workRes.Diaries) > 0 {
			d := workRes.Diaries[len(workRes.Diaries)-1]
			lastDiary = &d
		}
		if len(workRes.Outcome.ResultJSON) > 0 {
			resultJSON = append([]byte(nil), workRes.Outcome.ResultJSON...)
		}

		switch workRes.Outcome.Status {
		case session.OutcomeWaitingHuman:
			pauseDiary := effectiveCheckpointDiary(lastDiary, "paused for human input", "resume after human decision")
			if _, ckptErr := ckptMgr.SaveCheckpoint(ctx, req.Epoch, workspaceDir, artifactsDir, req.Worker.WritablePaths, pauseDiary); ckptErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: ckptErr.Error()}, ckptErr
			}
			if err := r.persistQCLedger(ctx, req.Epoch, qcLedger); err != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
			}
			return NodeOutcome{
				Status:          StatusWaitingHuman,
				DecisionRequest: workRes.Outcome.DecisionRequest,
				PauseReason:     workRes.Outcome.PauseReason,
			}, nil

		case session.OutcomeAgentEscalated:
			escName := ""
			if workRes.Outcome.Escalation != nil {
				escName = workRes.Outcome.Escalation.FallbackName
			}
			fb, found := findFallback(req.Worker.Fallbacks, "agent_escalated", escName)
			if !found {
				return NodeOutcome{Status: StatusFailed, FailureReason: "agent escalated without matching fallback"}, nil
			}
			if rbErr := r.rollbackUnauthorizedIfPresent(ctx, workspaceDir, req.Worker.WritablePaths); rbErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: rbErr.Error()}, rbErr
			}
			escDiary := effectiveCheckpointDiary(lastDiary, "agent escalated to fallback "+fb.Name, "hand off to "+fb.Target)
			if _, ckptErr := ckptMgr.SaveCheckpoint(ctx, req.Epoch, workspaceDir, artifactsDir, req.Worker.WritablePaths, escDiary); ckptErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: ckptErr.Error()}, ckptErr
			}
			fbArts, pubErr := r.publishFallbackArtifacts(ctx, artSvc, req.RunID, artifactsDir, req.Worker.Artifacts, fb.Artifacts)
			if pubErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: pubErr.Error()}, pubErr
			}
			return NodeOutcome{
				Status:            StatusFallbackTriggered,
				FallbackName:      fb.Name,
				FallbackTarget:    fb.Target,
				FallbackReason:    "agent_escalated",
				FallbackArtifacts: fbArts,
			}, nil
		}

		// Step 2b: Host post-freeze verification (writable_paths + artifact.Validate).
		changed, err := r.deps.GitAdapter.StatusPaths(ctx, workspaceDir)
		if err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
		if unauth := git.FindUnauthorizedPaths(changed, req.Worker.WritablePaths); len(unauth) > 0 {
			if rbErr := r.deps.GitAdapter.RevertPaths(ctx, workspaceDir, unauth); rbErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: rbErr.Error()}, rbErr
			}
			return NodeOutcome{
				Status:        StatusFailed,
				FailureReason: fmt.Sprintf("unauthorized paths detected after work freeze and reverted: %s", strings.Join(unauth, ", ")),
			}, nil
		}

		if len(artifactDecls) > 0 {
			_, artIssues, artErr := artifact.Validate(artifactsDir, artifactDecls)
			if artErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: artErr.Error()}, artErr
			}
			if len(artIssues) > 0 {
				if artifactReworks < maxArtReworks {
					artifactReworks++
					reworkPrompt = formatArtifactIssuesPrompt(artIssues)
					continue
				}
				return NodeOutcome{
					Status:        StatusFailed,
					FailureReason: formatArtifactIssuesPrompt(artIssues),
				}, nil
			}
		}

		// Step 2c: Deterministic gatekeeper containers.
		if len(req.Worker.Gatekeepers) > 0 {
			gkFail, gkErr := r.sandboxEngine().RunGatekeepers(ctx, sandboxruntime.GatekeeperRunSpec{
				Sandbox:          workSandbox,
				HostWorkspaceDir: workspaceDir,
				BundleDir:        req.BundleDir,
				ImportsDir:       importsDir,
				ArtifactsDir:     artifactsDir,
				HostOS:           hostOS,
			}, req.Worker.Gatekeepers)
			if gkErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: gkErr.Error()}, gkErr
			}
			if gkFail != nil {
				if gatekeeperReworks < maxGKReworks {
					gatekeeperReworks++
					reworkPrompt = formatGatekeeperFailurePrompt(gkFail)
					continue
				}
				if fb, found := findFallback(req.Worker.Fallbacks, "gatekeeper_exhausted", ""); found {
					fbArts, pubErr := r.publishFallbackArtifacts(ctx, artSvc, req.RunID, artifactsDir, req.Worker.Artifacts, fb.Artifacts)
					if pubErr != nil {
						return NodeOutcome{Status: StatusFailed, FailureReason: pubErr.Error()}, pubErr
					}
					return NodeOutcome{
						Status:            StatusFallbackTriggered,
						FallbackName:      fb.Name,
						FallbackTarget:    fb.Target,
						FallbackReason:    "gatekeeper_exhausted",
						FallbackArtifacts: fbArts,
					}, nil
				}
				return NodeOutcome{
					Status:        StatusFailed,
					FailureReason: formatGatekeeperFailurePrompt(gkFail),
				}, nil
			}
		}

		// Step 2d: Independent read-only container QC session.
		if req.Worker.QC != nil {
			qcRound++
			qcRes, qcErr := r.runQCRound(ctx, req, qcRoundParams{
				Round:           qcRound,
				Criteria:        qcCriteria,
				Ledger:          qcLedger,
				Platform:        platform,
				HostOS:          hostOS,
				LookupEnv:       lookupEnv,
				WorkSandbox:     workSandbox,
				WorkHarness:     workHarness,
				HasWorkProvider: hasWorkProvider,
				WorkProvider:    workProvider,
				WorkRetry:       workRetry,
				WorkspaceDir:    workspaceDir,
				ImportsDir:      importsDir,
				ArtifactsDir:    artifactsDir,
				ToolsDir:        qcToolsDir,
				SessionStoreDir: qcStoreDir,
			})
			if snapErr := r.persistQCLedger(ctx, req.Epoch, qcLedger); snapErr != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: snapErr.Error()}, snapErr
			}
			if qcErr != nil {
				if errors.Is(qcErr, session.ErrProtocolAttemptsExhausted) {
					return NodeOutcome{Status: StatusFailed, FailureReason: qcErr.Error()}, nil
				}
				return NodeOutcome{Status: StatusFailed, FailureReason: qcErr.Error()}, qcErr
			}
			if qcRes.Verdict.Decision == "revise" {
				if qcRound < req.Worker.QC.MaxQCRounds {
					reworkPrompt = formatQCRevisePrompt(qcRes.Verdict, qcRes.OpenIssues)
					continue
				}
				if fb, found := findFallback(req.Worker.Fallbacks, "qc_exhausted", ""); found {
					fbArts, pubErr := r.publishFallbackArtifacts(ctx, artSvc, req.RunID, artifactsDir, req.Worker.Artifacts, fb.Artifacts)
					if pubErr != nil {
						return NodeOutcome{Status: StatusFailed, FailureReason: pubErr.Error()}, pubErr
					}
					return NodeOutcome{
						Status:            StatusFallbackTriggered,
						FallbackName:      fb.Name,
						FallbackTarget:    fb.Target,
						FallbackReason:    "qc_exhausted",
						FallbackArtifacts: fbArts,
					}, nil
				}
				return NodeOutcome{
					Status:        StatusFailed,
					FailureReason: fmt.Sprintf("qc exhausted after %d rounds: %s", qcRound, qcRes.Verdict.Summary),
				}, nil
			}
		}

		break
	}

	// Step 3: Final verified checkpoint, immutable artifact publication, human_approval gate, and work_branch push.
	finalDiary := effectiveCheckpointDiary(lastDiary, "verified worker completion", "publish work branch")
	finalRec, err := ckptMgr.SaveCheckpoint(ctx, req.Epoch, workspaceDir, artifactsDir, req.Worker.WritablePaths, finalDiary)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	finalCommitSHA := finalRec.FinalSHA

	var publishedArts []artifact.PublishedArtifact
	if len(artifactDecls) > 0 {
		validDecls, issues, valErr := artifact.Validate(artifactsDir, artifactDecls)
		if valErr != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: valErr.Error()}, valErr
		}
		if len(issues) > 0 {
			return NodeOutcome{Status: StatusFailed, FailureReason: formatArtifactIssuesPrompt(issues)}, nil
		}
		sort.Slice(validDecls, func(i, j int) bool {
			return validDecls[i].Name < validDecls[j].Name
		})
		publishedArts, err = artSvc.Publish(ctx, req.RunID, artifactsDir, validDecls)
		if err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
	}
	artifactIDs := make([]string, len(publishedArts))
	for i, art := range publishedArts {
		artifactIDs[i] = art.ID
	}

	return r.handleApprovalAndPublish(
		ctx, req, basedSource, baseSHA, workspaceDir,
		finalCommitSHA, publishedArts, artifactIDs, resultJSON,
	)
}

func (r *WorkerRunner) handleApprovalAndPublish(
	ctx context.Context,
	req NodeExecutionRequest,
	basedSource, baseSHA, workspaceDir string,
	finalCommitSHA string,
	publishedArts []artifact.PublishedArtifact,
	artifactIDs []string,
	resultJSON json.RawMessage,
) (NodeOutcome, error) {
	if baseSHA == "" {
		var err error
		baseSHA, err = r.deps.GitAdapter.ResolveBranchSHA(ctx, req.RepoURL, basedSource, req.Auth)
		if err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
	}

	if req.Worker.HumanApproval == "required" {
		approval, err := r.deps.ControlPlane.GetApproval(ctx)
		if err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
		if approval != nil && approval.ReturnedUpstream {
			if fb, found := findFallback(req.Worker.Fallbacks, "human_returned_upstream", approval.ReturnFallback); found {
				if clearErr := r.deps.ControlPlane.SavePendingPublication(ctx, req.Epoch, nil); clearErr != nil {
					return NodeOutcome{Status: StatusFailed, FailureReason: clearErr.Error()}, clearErr
				}
				return NodeOutcome{
					Status:            StatusFallbackTriggered,
					FallbackName:      fb.Name,
					FallbackTarget:    fb.Target,
					FallbackReason:    "human_returned_upstream",
					FallbackArtifacts: slices.Clone(publishedArts),
				}, nil
			}
			return NodeOutcome{Status: StatusFailed, FailureReason: "human returned work upstream without matching fallback"}, nil
		}

		versionMatched := approval != nil &&
			approval.Approved &&
			approval.CommitSHA == finalCommitSHA &&
			slices.Equal(approval.ArtifactIDs, artifactIDs)
		if !versionMatched {
			pending := &PendingPublication{
				CommitSHA:   finalCommitSHA,
				BaseSHA:     baseSHA,
				ArtifactIDs: slices.Clone(artifactIDs),
				Artifacts:   slices.Clone(publishedArts),
				ResultJSON:  append([]byte(nil), resultJSON...),
			}
			if err := r.deps.ControlPlane.SavePendingPublication(ctx, req.Epoch, pending); err != nil {
				return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
			}
			return NodeOutcome{
				Status: StatusWaitingHuman,
				PendingApproval: &ApprovalTarget{
					CommitSHA:   finalCommitSHA,
					ArtifactIDs: slices.Clone(artifactIDs),
					Artifacts:   slices.Clone(publishedArts),
					ResultJSON:  append([]byte(nil), resultJSON...),
				},
			}, nil
		}
	}

	workBranch := git.WorkBranchName(req.RunID, req.NodeAddr)
	if err := r.deps.ControlPlane.RegisterWorkBranch(ctx, req.Epoch, req.RepoURL, workBranch, baseSHA); err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	existingTip, _, err := r.deps.GitAdapter.BranchContainsSHA(ctx, req.RepoURL, workBranch, finalCommitSHA, req.Auth)
	if err != nil {
		return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
	}
	if existingTip != finalCommitSHA {
		if err := r.deps.GitAdapter.PushCommit(ctx, workspaceDir, req.RepoURL, workBranch, finalCommitSHA, existingTip, req.Auth); err != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: err.Error()}, err
		}
	}
	if req.Worker.HumanApproval == "required" {
		if clearErr := r.deps.ControlPlane.SavePendingPublication(ctx, req.Epoch, nil); clearErr != nil {
			return NodeOutcome{Status: StatusFailed, FailureReason: clearErr.Error()}, clearErr
		}
	}
	return NodeOutcome{
		Status:     StatusSucceeded,
		WorkBranch: workBranch,
		CommitSHA:  finalCommitSHA,
		ResultJSON: append([]byte(nil), resultJSON...),
		Artifacts:  slices.Clone(publishedArts),
	}, nil
}

type workAttemptParams struct {
	Req          NodeExecutionRequest
	Sandbox      sandboxspec.SandboxConfig
	Harness      harnessspec.HarnessConfig
	MountLayout  sandboxruntime.SessionMountLayout
	GoTools      []plan.ToolSpec
	Platform     harnessinstaller.Platform
	BaseSession  harnessruntime.SessionConfig
	Prompt       string
	DiarySummary string
	ToolBindings []toolruntime.ToolBinding
	MCPResources []toolruntime.MCPResourceSpec
	RetryPolicy  provider.RetryPolicy
	WorkspaceDir string
	ImportsDir   string
	ArtifactsDir string
	CheckpointFn func(context.Context, session.DiaryInput) error
}

func (r *WorkerRunner) runWorkAttempt(ctx context.Context, p workAttemptParams) (session.WorkResult, error) {
	handle, err := r.sessionLauncher().Launch(ctx, SessionLaunchRequest{
		Phase:       "work",
		Sandbox:     p.Sandbox,
		Harness:     p.Harness,
		MountLayout: p.MountLayout,
		GoTools:     p.GoTools,
		Platform:    p.Platform,
		ResolveOpts: p.Req.PluginResolveOpts,
	})
	if err != nil {
		return session.WorkResult{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = handle.Close()
		}
	}()

	var toolExec *toolruntime.Executor
	if len(p.ToolBindings) > 0 {
		toolExec = toolruntime.NewExecutor(toolruntime.ExecutorOptions{
			WorkspaceDir: p.WorkspaceDir,
			ImportsDir:   p.ImportsDir,
			BundleDir:    p.Req.BundleDir,
			Exec:         r.toolExecForContainer(handle.ContainerID()),
			LookupEnv:    p.Req.LookupEnv,
		})
	}

	res, runErr := session.RunWorkPhase(ctx, session.WorkSessionConfig{
		Opener:                handle.Opener(),
		BaseSession:           p.BaseSession,
		Prompt:                p.Prompt,
		DiarySummary:          p.DiarySummary,
		ToolExecutor:          toolExec,
		ToolBindings:          p.ToolBindings,
		MCPResources:          p.MCPResources,
		WritablePaths:         p.Req.Worker.WritablePaths,
		CheckpointEveryRounds: p.Req.Worker.CheckpointRounds,
		MaxProtocolAttempts:   p.Req.Worker.MaxProtocolAttempts,
		ToolCallQuota:         p.Req.Worker.ToolCallQuota,
		Fallbacks:             p.Req.Worker.Fallbacks,
		RetryPolicy:           p.RetryPolicy,
		StallTimeout:          p.Req.StallTimeout,
		WatchdogCheckInterval: p.Req.WatchdogCheckInterval,
		OnEvent:               p.Req.OnEvent,
		Sleep:                 p.Req.Sleep,
		Jitter:                p.Req.Jitter,
		Hooks: &workerControlHooks{
			cp:            r.deps.ControlPlane,
			gitAdapter:    r.deps.GitAdapter,
			workspaceDir:  p.WorkspaceDir,
			writablePaths: p.Req.Worker.WritablePaths,
			onCheckpoint:  p.CheckpointFn,
		},
	})

	closed = true
	if closeErr := handle.Close(); closeErr != nil && runErr == nil {
		return res, closeErr
	}
	return res, runErr
}

type qcRoundParams struct {
	Round           int
	Criteria        map[string]string
	Ledger          *session.QCLedger
	Platform        harnessinstaller.Platform
	HostOS          string
	LookupEnv       func(string) (string, bool)
	WorkSandbox     sandboxspec.SandboxConfig
	WorkHarness     harnessspec.HarnessConfig
	HasWorkProvider bool
	WorkProvider    provider.Config
	WorkRetry       provider.RetryPolicy
	WorkspaceDir    string
	ImportsDir      string
	ArtifactsDir    string
	ToolsDir        string
	SessionStoreDir string
}

func (r *WorkerRunner) runQCRound(ctx context.Context, req NodeExecutionRequest, p qcRoundParams) (session.QCResult, error) {
	qc := req.Worker.QC

	qcSandbox := p.WorkSandbox
	if extractBlockAddress(qc.Sandbox) != "" {
		var err error
		qcSandbox, err = lookupSandbox(req.Sandboxes, qc.Sandbox, "qc sandbox")
		if err != nil {
			return session.QCResult{}, err
		}
	}
	qcHarness := p.WorkHarness
	if extractBlockAddress(qc.Harness) != "" {
		var err error
		qcHarness, err = lookupHarness(req.Harnesses, qc.Harness, "qc harness")
		if err != nil {
			return session.QCResult{}, err
		}
	}

	qcProvider := p.WorkProvider
	hasQCProvider := p.HasWorkProvider
	if qc.ModelProviderSet {
		var err error
		qcProvider, hasQCProvider, err = lookupOptionalProvider(req.ModelProviders, qc.ModelProvider, "qc model_provider")
		if err != nil {
			return session.QCResult{}, err
		}
	}

	qcModel := qc.Model
	if strings.TrimSpace(qcModel) == "" {
		qcModel = req.Worker.Model
	}
	qcProfile := qc.Profile
	if strings.TrimSpace(qcProfile) == "" {
		qcProfile = req.Worker.Profile
	}
	qcEffort := qc.ReasoningEffort
	if strings.TrimSpace(qcEffort) == "" {
		qcEffort = req.Worker.ReasoningEffort
	}
	qcPermission := qc.Permission
	if strings.TrimSpace(qcPermission) == "" {
		qcPermission = effectivePermission(req.Worker.Permission)
	}

	qcRetry, err := computeRetryPolicy(hasQCProvider, qcProvider, req.Worker.Retry, &qc.Retry)
	if err != nil {
		return session.QCResult{}, err
	}

	var matQCProvider *harnessruntime.ProviderConfig
	if hasQCProvider {
		mat, err := harnessruntime.MaterializeProviderConfig(qcProvider, qcModel, effectiveModelID(qcModel, qcProfile), p.LookupEnv)
		if err != nil {
			return session.QCResult{}, err
		}
		matQCProvider = &mat
	}
	matQCHarnessEnv, err := harnessruntime.MaterializeHarnessEnv(qcHarness.EnvRefs, p.LookupEnv)
	if err != nil {
		return session.QCResult{}, err
	}
	qcHarnessOpts, err := harnessruntime.MarshalHarnessOptions(qcHarness.Options)
	if err != nil {
		return session.QCResult{}, err
	}

	qcBindings, qcGoTools, err := resolveToolBindings(qc.ToolIDs, "", nil, req.Tools, req.ToolEnvs)
	if err != nil {
		return session.QCResult{}, err
	}

	qcLayout := sandboxruntime.SessionMountLayout{
		Sandbox:           qcSandbox,
		HostOS:            p.HostOS,
		ForAgentSession:   true,
		Entrypoint:        "/.sofa/harness/sofa-harness-" + qcHarness.Name,
		WorkingDir:        ".",
		HostWorkspaceDir:  p.WorkspaceDir,
		ReadOnlyWorkspace: true,
		WritablePaths:     nil,
		BundleDir:         req.BundleDir,
		ImportsDir:        p.ImportsDir,
		ArtifactsDir:      p.ArtifactsDir,
		ReadOnlyArtifacts: true,
		ToolsDir:          p.ToolsDir,
		SessionStoreDir:   p.SessionStoreDir,
	}

	handle, err := r.sessionLauncher().Launch(ctx, SessionLaunchRequest{
		Phase:       "qc",
		Sandbox:     qcSandbox,
		Harness:     qcHarness,
		MountLayout: qcLayout,
		GoTools:     qcGoTools,
		Platform:    p.Platform,
		ResolveOpts: req.PluginResolveOpts,
	})
	if err != nil {
		return session.QCResult{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = handle.Close()
		}
	}()

	var toolExec *toolruntime.Executor
	if len(qcBindings) > 0 {
		toolExec = toolruntime.NewExecutor(toolruntime.ExecutorOptions{
			WorkspaceDir: p.WorkspaceDir,
			ImportsDir:   p.ImportsDir,
			BundleDir:    req.BundleDir,
			Exec:         r.toolExecForContainer(handle.ContainerID()),
			LookupEnv:    p.LookupEnv,
		})
	}

	qcPrompt := buildQCPrompt(req.QCPrompt, p.Round, p.Criteria, p.Ledger)
	qcBaseSession := harnessruntime.SessionConfig{
		Model:                 qcModel,
		Profile:               qcProfile,
		ReasoningEffort:       qcEffort,
		SystemPrompt:          "Perform an independent, read-only quality control audit of /workspace and /.sofa/artifacts against all declared criteria.",
		WorkingDirectory:      sandboxruntime.DefaultContainerWorkingDir,
		SessionStoreDirectory: "/.sofa/session-store",
		PermissionMode:        qcPermission,
		Provider:              matQCProvider,
		HarnessEnv:            matQCHarnessEnv,
		HarnessOptionsJSON:    qcHarnessOpts,
		AvailableTools:        toToolFilter(qc.AllowedTools),
		ExcludedTools:         toToolFilter(qc.DisallowedTools),
		SkillDirectories:      slices.Clone(qc.SkillDirectories),
		Skills:                slices.Clone(qc.Skills),
		DisabledSkills:        slices.Clone(qc.DisabledSkills),
	}

	qcRes, qcErr := session.RunQCPhase(ctx, session.QCSessionConfig{
		Opener:                handle.Opener(),
		BaseSession:           qcBaseSession,
		Prompt:                qcPrompt,
		Ledger:                p.Ledger,
		Criteria:              p.Criteria,
		ToolExecutor:          toolExec,
		ToolBindings:          qcBindings,
		MaxProtocolAttempts:   req.Worker.MaxProtocolAttempts,
		ToolCallQuota:         qc.ToolCallQuota,
		RetryPolicy:           qcRetry,
		StallTimeout:          req.StallTimeout,
		WatchdogCheckInterval: req.WatchdogCheckInterval,
		OnEvent:               req.OnEvent,
		Sleep:                 req.Sleep,
		Jitter:                req.Jitter,
	})

	closed = true
	if closeErr := handle.Close(); closeErr != nil && qcErr == nil {
		return qcRes, closeErr
	}
	return qcRes, qcErr
}

type workerControlHooks struct {
	cp            NodeControlPlane
	gitAdapter    git.Adapter
	workspaceDir  string
	writablePaths []string
	onCheckpoint  func(context.Context, session.DiaryInput) error
}

func (h *workerControlHooks) DrainInterjections(ctx context.Context) ([]session.HumanInterjection, error) {
	return h.cp.DrainInterjections(ctx)
}

func (h *workerControlHooks) OnCheckpointRequested(ctx context.Context, diary session.DiaryInput) error {
	if h.onCheckpoint == nil {
		return nil
	}
	return h.onCheckpoint(ctx, diary)
}

func (h *workerControlHooks) ValidateWorkspaceForHandoff(ctx context.Context) ([]string, error) {
	changed, err := h.gitAdapter.StatusPaths(ctx, h.workspaceDir)
	if err != nil {
		return nil, err
	}
	return git.FindUnauthorizedPaths(changed, h.writablePaths), nil
}

func (r *WorkerRunner) resolveTargetPlatform(ctx context.Context, req NodeExecutionRequest) (harnessinstaller.Platform, error) {
	if req.TargetPlatform != nil {
		if err := req.TargetPlatform.Validate(); err != nil {
			return harnessinstaller.Platform{}, err
		}
		return *req.TargetPlatform, nil
	}
	if r.deps.SandboxEngine != nil {
		return r.deps.SandboxEngine.DetectPlatform(ctx)
	}
	return harnessinstaller.Platform{OS: "linux", Arch: "amd64"}, nil
}

func (r *WorkerRunner) rollbackUnauthorizedIfPresent(ctx context.Context, workspaceDir string, writablePaths []string) error {
	if writablePaths == nil {
		return nil
	}
	if _, err := os.Stat(workspaceDir); err != nil {
		return nil
	}
	changed, err := r.deps.GitAdapter.StatusPaths(ctx, workspaceDir)
	if err != nil {
		return err
	}
	unauth := git.FindUnauthorizedPaths(changed, writablePaths)
	if len(unauth) == 0 {
		return nil
	}
	return r.deps.GitAdapter.RevertPaths(ctx, workspaceDir, unauth)
}

func (r *WorkerRunner) persistQCLedger(ctx context.Context, epoch uint64, ledger *session.QCLedger) error {
	if ledger == nil {
		return nil
	}
	snap, err := ledger.Snapshot()
	if err != nil {
		return err
	}
	return r.deps.ControlPlane.SaveQCLedgerSnapshot(ctx, epoch, snap)
}

func (r *WorkerRunner) publishFallbackArtifacts(
	ctx context.Context,
	artSvc *artifact.Service,
	runID, artifactsDir string,
	allArtifacts map[string]workerspec.Artifact,
	fallbackNames []string,
) ([]artifact.PublishedArtifact, error) {
	if len(fallbackNames) == 0 {
		return nil, nil
	}
	decls := make([]workerspec.Artifact, 0, len(fallbackNames))
	for _, name := range fallbackNames {
		if decl, ok := allArtifacts[name]; ok {
			decls = append(decls, decl)
		}
	}
	if len(decls) == 0 {
		return nil, nil
	}
	validDecls, _, err := artifact.Validate(artifactsDir, decls)
	if err != nil || len(validDecls) == 0 {
		return nil, err
	}
	sort.Slice(validDecls, func(i, j int) bool {
		return validDecls[i].Name < validDecls[j].Name
	})
	return artSvc.Publish(ctx, runID, artifactsDir, validDecls)
}

func verifyPendingArtifactsInBlobStore(ctx context.Context, store blob.Store, arts []artifact.PublishedArtifact) (bool, error) {
	for _, art := range arts {
		meta, err := store.Stat(ctx, art.BlobKey)
		if errors.Is(err, blob.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if meta.SHA256 != art.SHA256 {
			return false, nil
		}
	}
	return true, nil
}

func resolveStringOrCty(explicit string, val cty.Value, field string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	if val != cty.NilVal {
		unmarked, _ := val.UnmarkDeep()
		if !unmarked.IsNull() && unmarked.IsWhollyKnown() && unmarked.Type().Equals(cty.String) {
			if s := unmarked.AsString(); strings.TrimSpace(s) != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("worker runner: %s is required", field)
}

func resolveOptionalStringOrCty(explicit string, val cty.Value) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if val != cty.NilVal {
		unmarked, _ := val.UnmarkDeep()
		if !unmarked.IsNull() && unmarked.IsWhollyKnown() && unmarked.Type().Equals(cty.String) {
			return unmarked.AsString()
		}
	}
	return ""
}

func extractBlockAddress(val cty.Value) string {
	if val == cty.NilVal {
		return ""
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().IsObjectType() {
		return ""
	}
	if unmarked.Type().HasAttribute("address") {
		addr := unmarked.GetAttr("address")
		if !addr.IsNull() && addr.IsKnown() && addr.Type().Equals(cty.String) {
			return strings.TrimSpace(addr.AsString())
		}
	}
	return ""
}

func extractBlockName(val cty.Value) string {
	if val == cty.NilVal {
		return ""
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().IsObjectType() {
		return ""
	}
	if unmarked.Type().HasAttribute("name") {
		n := unmarked.GetAttr("name")
		if !n.IsNull() && n.IsKnown() && n.Type().Equals(cty.String) {
			return strings.TrimSpace(n.AsString())
		}
	}
	return ""
}

func lookupSandbox(catalog map[string]sandboxspec.SandboxConfig, ref cty.Value, label string) (sandboxspec.SandboxConfig, error) {
	addr := extractBlockAddress(ref)
	if addr == "" {
		return sandboxspec.SandboxConfig{}, fmt.Errorf("worker runner: %s reference is required", label)
	}
	if cfg, ok := catalog[addr]; ok {
		return cfg, nil
	}
	if name := extractBlockName(ref); name != "" {
		if cfg, ok := catalog[name]; ok {
			return cfg, nil
		}
	}
	return sandboxspec.SandboxConfig{}, fmt.Errorf("worker runner: %s %q not found in catalog", label, addr)
}

func lookupHarness(catalog map[string]harnessspec.HarnessConfig, ref cty.Value, label string) (harnessspec.HarnessConfig, error) {
	addr := extractBlockAddress(ref)
	if addr == "" {
		return harnessspec.HarnessConfig{}, fmt.Errorf("worker runner: %s reference is required", label)
	}
	if cfg, ok := catalog[addr]; ok {
		return cfg.Clone(), nil
	}
	if name := extractBlockName(ref); name != "" {
		if cfg, ok := catalog[name]; ok {
			return cfg.Clone(), nil
		}
	}
	return harnessspec.HarnessConfig{}, fmt.Errorf("worker runner: %s %q not found in catalog", label, addr)
}

func lookupOptionalProvider(catalog map[string]provider.Config, ref cty.Value, label string) (provider.Config, bool, error) {
	addr := extractBlockAddress(ref)
	if addr == "" {
		return provider.Config{}, false, nil
	}
	if cfg, ok := catalog[addr]; ok {
		return cfg.Clone(), true, nil
	}
	if name := extractBlockName(ref); name != "" {
		if cfg, ok := catalog[name]; ok {
			return cfg.Clone(), true, nil
		}
	}
	return provider.Config{}, false, fmt.Errorf("worker runner: %s %q not found in catalog", label, addr)
}

func computeRetryPolicy(hasProvider bool, prov provider.Config, workerOverride provider.RetryOverride, qcOverride *provider.RetryOverride) (provider.RetryPolicy, error) {
	policy := provider.DefaultRetryPolicy()
	var err error
	if hasProvider {
		policy, err = provider.MergeRetry(policy, prov.Retry)
		if err != nil {
			return provider.RetryPolicy{}, err
		}
	}
	policy, err = provider.MergeRetry(policy, workerOverride)
	if err != nil {
		return provider.RetryPolicy{}, err
	}
	if qcOverride != nil {
		policy, err = provider.MergeRetry(policy, *qcOverride)
		if err != nil {
			return provider.RetryPolicy{}, err
		}
	}
	return policy, nil
}

func extractQCCriteria(val cty.Value) (map[string]string, error) {
	if val == cty.NilVal {
		return nil, errors.New("qc criteria is required")
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || (!unmarked.Type().IsMapType() && !unmarked.Type().IsObjectType()) {
		return nil, errors.New("qc criteria must be a known map(string)")
	}
	out := make(map[string]string, unmarked.LengthInt())
	for it := unmarked.ElementIterator(); it.Next(); {
		k, v := it.Element()
		out[k.AsString()] = v.AsString()
	}
	return out, nil
}

func sortedArtifactDecls(arts map[string]workerspec.Artifact) []workerspec.Artifact {
	if len(arts) == 0 {
		return nil
	}
	out := make([]workerspec.Artifact, 0, len(arts))
	for _, a := range arts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func resolveToolBindings(
	toolIDs []string,
	terminateToolID string,
	toolUses []workerspec.ToolUse,
	catalog map[string]plan.ToolSpec,
	envs map[string]ExternalToolEnvConfig,
) ([]toolruntime.ToolBinding, []plan.ToolSpec, error) {
	var bindings []toolruntime.ToolBinding
	var goTools []plan.ToolSpec
	seenGo := make(map[string]struct{})

	appendTool := func(id string, exposed string, preset cty.Value, terminate bool) error {
		spec, ok := catalog[id]
		if !ok {
			return fmt.Errorf("worker runner: tool %q not found in catalog", id)
		}
		var envCfg ExternalToolEnvConfig
		if envs != nil {
			envCfg = envs[id]
		}
		bindings = append(bindings, toolruntime.ToolBinding{
			Spec:        spec,
			ExposedName: exposed,
			PresetInput: preset,
			Terminate:   terminate,
			Env:         envCfg.Env,
			EnvRefs:     envCfg.EnvRefs,
			Timeout:     envCfg.Timeout,
		})
		if spec.Kind == "go" {
			if _, dup := seenGo[spec.ID]; !dup {
				seenGo[spec.ID] = struct{}{}
				goTools = append(goTools, spec)
			}
		}
		return nil
	}

	if len(toolUses) > 0 {
		for _, tu := range toolUses {
			if err := appendTool(tu.ToolID, tu.Name, tu.Input, tu.Terminate); err != nil {
				return nil, nil, err
			}
		}
		return bindings, goTools, nil
	}

	seen := make(map[string]struct{}, len(toolIDs)+1)
	for _, id := range toolIDs {
		seen[id] = struct{}{}
		if err := appendTool(id, "", cty.NilVal, id == terminateToolID); err != nil {
			return nil, nil, err
		}
	}
	if terminateToolID != "" {
		if _, exists := seen[terminateToolID]; !exists {
			if err := appendTool(terminateToolID, "", cty.NilVal, true); err != nil {
				return nil, nil, err
			}
		}
	}
	return bindings, goTools, nil
}

func materializeMCPServers(servers []mcppkg.Config, toolIDs, resourceIDs []string, lookupEnv func(string) (string, bool)) ([]harnessruntime.MCPServerConfig, error) {
	if len(servers) == 0 || (len(toolIDs) == 0 && len(resourceIDs) == 0) {
		return nil, nil
	}
	out := make([]harnessruntime.MCPServerConfig, 0, len(servers))
	for _, srv := range servers {
		mat, err := harnessruntime.MaterializeMCPServerConfig(srv, lookupEnv)
		if err != nil {
			return nil, err
		}
		out = append(out, mat)
	}
	return out, nil
}

func filterMCPResources(all []toolruntime.MCPResourceSpec, resourceIDs []string) []toolruntime.MCPResourceSpec {
	if len(all) == 0 || len(resourceIDs) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(resourceIDs))
	for _, id := range resourceIDs {
		allowed[id] = struct{}{}
	}
	var out []toolruntime.MCPResourceSpec
	for _, spec := range all {
		if _, ok := allowed[spec.ID]; ok {
			out = append(out, spec)
		}
	}
	return out
}

func toToolFilter(names []string) harnessruntime.ToolFilter {
	if names == nil {
		return harnessruntime.ToolFilter{}
	}
	return harnessruntime.ToolFilter{
		Configured: true,
		Names:      slices.Clone(names),
	}
}

func effectivePermission(perm string) string {
	if strings.TrimSpace(perm) == "" {
		return workerspec.DefaultPermission
	}
	return perm
}

func effectiveModelID(model, profile string) string {
	if strings.TrimSpace(profile) != "" {
		return profile
	}
	return model
}

func effectiveCheckpointDiary(last *session.DiaryInput, completed, next string) session.DiaryInput {
	if last != nil {
		return *last
	}
	return session.DiaryInput{
		Completed: []string{completed},
		NextSteps: []string{next},
	}
}

func findFallback(fallbacks []workerspec.FallbackConfig, reason, preferredName string) (workerspec.FallbackConfig, bool) {
	for _, fb := range fallbacks {
		if !slices.Contains(fb.On, reason) {
			continue
		}
		if preferredName != "" && fb.Name != preferredName {
			continue
		}
		return fb, true
	}
	return workerspec.FallbackConfig{}, false
}

func appendPromptSection(base, extra string) string {
	if strings.TrimSpace(extra) == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + "\n\n" + extra
}

func formatArtifactIssuesPrompt(issues []artifact.Issue) string {
	var b strings.Builder
	b.WriteString("Artifact validation failed after Work completion. Fix all missing/invalid artifacts under /.sofa/artifacts and resubmit:\n")
	for _, iss := range issues {
		pathStr := ""
		if iss.Path != nil {
			pathStr = *iss.Path
		}
		hint := ""
		if iss.RepairHint != nil {
			hint = *iss.RepairHint
		}
		fmt.Fprintf(&b, "- [%s] %s (%s) %s\n", iss.Code, iss.Message, pathStr, hint)
	}
	return strings.TrimSpace(b.String())
}

func formatGatekeeperFailurePrompt(f *sandboxruntime.GatekeeperFailure) string {
	if f == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Gatekeeper %q failed:\n", f.Name)
	fmt.Fprintf(&b, "- command: %s\n", strings.Join(f.Command, " "))
	fmt.Fprintf(&b, "- exit_code: %d (expected %d, timed_out=%v)\n", f.ExitCode, f.ExpectedExitCode, f.TimedOut)
	if len(f.DirtyFiles) > 0 {
		fmt.Fprintf(&b, "- dirty_files: %s\n", strings.Join(f.DirtyFiles, ", "))
	}
	if strings.TrimSpace(f.Stdout) != "" {
		fmt.Fprintf(&b, "- stdout:\n%s\n", strings.TrimSpace(f.Stdout))
	}
	if strings.TrimSpace(f.Stderr) != "" {
		fmt.Fprintf(&b, "- stderr:\n%s\n", strings.TrimSpace(f.Stderr))
	}
	return strings.TrimSpace(b.String())
}

func formatQCRevisePrompt(verdict session.QCVerdict, openIssues []session.QCIssue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "QC returned verdict %q: %s\nOpen QC issues to resolve:\n", verdict.Decision, verdict.Summary)
	for _, iss := range openIssues {
		fmt.Fprintf(&b, "- %s [%s/%s]: %s", iss.ID, iss.Criterion, iss.Code, iss.Message)
		if iss.Path != "" {
			fmt.Fprintf(&b, " (path: %s)", iss.Path)
		}
		if iss.RepairHint != "" {
			fmt.Fprintf(&b, " — hint: %s", iss.RepairHint)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func buildQCPrompt(explicit string, round int, criteria map[string]string, ledger *session.QCLedger) string {
	var b strings.Builder
	if strings.TrimSpace(explicit) != "" {
		b.WriteString(strings.TrimSpace(explicit))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "QC Audit Round %d\nCriteria:\n", round)
	keys := make([]string, 0, len(criteria))
	for k := range criteria {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", k, criteria[k])
	}
	if ledger != nil {
		open := ledger.OpenIssuesSnapshot()
		if len(open) > 0 {
			b.WriteString("\nPreviously opened unresolved issues (verify and close via sofa_qc_resolve_issues if fixed):\n")
			for _, iss := range open {
				fmt.Fprintf(&b, "- %s [%s/%s]: %s\n", iss.ID, iss.Criterion, iss.Code, iss.Message)
			}
		}
	}
	b.WriteString("\nInspect frozen /workspace and /.sofa/artifacts, use sofa_qc_open_issues / sofa_qc_resolve_issues as needed, and finish with sofa_qc_submit_verdict.")
	return strings.TrimSpace(b.String())
}
