//go:build integration

package run_test

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
	"github.com/dapr/go-sdk/actor/runtime"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	actorcfg "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/blob"
	appcfg "github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type itHTTPServer struct {
	port    int
	svc     common.Service
	errCh   chan error
	stopped atomic.Bool
}

func reserveITPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func startITHTTPService(t *testing.T, srv *itHTTPServer) {
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
	if s.stopped.Swap(true) {
		return
	}
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

func initBareRepoForRunIT(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bareDir := filepath.Join(root, "remote.git")
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
	baseSHA := runGitCLI(t, seedDir, "rev-parse", "HEAD")
	return bareDir, baseSHA
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
}

func (h *itSessionHandle) Opener() harnessruntime.Opener { return h.opener }
func (h *itSessionHandle) ContainerID() string           { return "it-run-ctr" }
func (h *itSessionHandle) Close() error                  { return nil }

type itSessionLauncherFunc func(ctx context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error)

func (f itSessionLauncherFunc) Launch(ctx context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error) {
	return f(ctx, req)
}

type itFakeOpener struct {
	onOpen func(cfg harnessruntime.SessionConfig) harnessruntime.Session
}

func (o *itFakeOpener) Capabilities(_ context.Context) (harnessruntime.Capabilities, error) {
	return harnessruntime.Capabilities{HarnessName: "copilot", SupportsSessionResume: true}, nil
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

func invokeITToolJSON(t *testing.T, cfg harnessruntime.SessionConfig, name string, payload any) {
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

type itNodeOutcomeReporter struct {
	client daprclient.Client
	killed *atomic.Bool
}

func (r *itNodeOutcomeReporter) ReportNodeOutcome(ctx context.Context, report daemon.NodeOutcomeReport) error {
	if r.killed != nil && r.killed.Load() {
		return context.Canceled
	}
	stub, err := node.NewClientStubForNode(report.RunID, report.NodeAddr, r.client)
	if err != nil {
		return err
	}
	_, err = stub.ReportNodeOutcome(ctx, &report)
	return err
}

type itMergeResultReporter struct {
	client daprclient.Client
}

func (r *itMergeResultReporter) ReportCandidateResult(ctx context.Context, report daemon.MergeOutcomeReport) error {
	stub := merge.NewClientStub(report.MergeActorID, r.client)
	_, err := stub.ReportCandidateResult(ctx, &report)
	return err
}

func TestOperatorAndRunActor_RealDaprRestartRecoveryAcrossEpochTakeover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rootDir := t.TempDir()
	cpStateDir := filepath.Join(rootDir, "cp-state")
	nrDBPath := filepath.Join(rootDir, "nameresolution.db")
	blobStore, err := blob.NewLocalStore(filepath.Join(rootDir, "blobs"))
	require.NoError(t, err)

	bareDir, _ := initBareRepoForRunIT(t)
	runID := "run-it-028-recovery"

	// 1. Build and upload real 2-node .sofaplan archive (worker.step1 -> worker.step2).
	configDir := filepath.Join(rootDir, "hcl-cfg")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	hclBody := fmt.Sprintf(`
source "project" {
  url    = %q
  branch = "main"
}

sandbox "local_env" {
  runtime = "podman"
  image   = "alpine:latest"
  network = "bridge"
}

harness "copilot" {}

worker "step1" {
  based_source   = source.project.branch
  model          = "fake-model"
  system_prompt  = "Step 1"
  writable_paths = ["allowed/**"]
}

worker "step2" {
  based_source   = worker.step1.work_branch
  model          = "fake-model"
  system_prompt  = "Step 2"
  writable_paths = ["allowed/**"]
}

merge "final" {
  based_source = source.project.branch
  sources      = [worker.step2.work_branch]
}

output "final_branch" {
  value = merge.final.work_branch
}
`, bareDir)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "main.sofa.hcl"), []byte(strings.TrimLeft(hclBody, "\n")), 0o600))

	loadedCtx, diags, err := appcfg.LoadDirectoryContext(ctx, configDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:    ctx,
		Directory:  configDir,
		BundlePath: "config",
	})
	require.NoError(t, cfg.Init(loadedCtx.Blocks))
	require.NoError(t, cfg.RunPlan())
	planned, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	archivePath := filepath.Join(rootDir, "plan.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, planned, plan.ArchiveSource{ConfigDir: configDir}))
	pf, err := os.Open(archivePath)
	require.NoError(t, err)
	meta, err := blobStore.Put(ctx, fmt.Sprintf("runs/%s/plan/plan.sofaplan", runID), pf, "")
	require.NoError(t, pf.Close())
	require.NoError(t, err)

	// Shared dynamic provider for Control Plane actors across Phase 1 -> Phase 2 restart.
	var cpClientRef atomic.Pointer[daprclient.Client]
	cpClientProvider := func() daprclient.Client {
		c := cpClientRef.Load()
		if c == nil {
			return nil
		}
		return *c
	}

	startControlPlane := func(registerFactories bool) (*itHTTPServer, *atomic.Value, *daprhost.Manager, daprclient.Client) {
		port := reserveITPort(t)
		var tok atomic.Value
		tok.Store("")
		router := chi.NewRouter()
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want, _ := tok.Load().(string)
				if want == "" || r.Header.Get(common.APITokenKey) != want {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		svc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", port), router)
		if registerFactories {
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return actorcfg.NewConfigActor(cpClientProvider)
			})
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return registry.NewProcessRegistryActor(registry.Options{
					ClientProvider: cpClientProvider,
					SuspectTimeout: 500 * time.Millisecond,
					ProbeInterval:  150 * time.Millisecond,
					DeadThreshold:  1,
					PingTimeout:    200 * time.Millisecond,
				})
			})
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return node.NewNodeActor(node.Options{
					ClientProvider:    cpClientProvider,
					SlotRetryInterval: 150 * time.Millisecond,
				})
			})
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return merge.NewMergeActor(merge.Options{
					ClientProvider:    cpClientProvider,
					SlotRetryInterval: 150 * time.Millisecond,
				})
			})
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return run.NewOperatorActor(run.OperatorOptions{
					ClientProvider: cpClientProvider,
				})
			})
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return run.NewRunActor(run.RunOptions{
					ClientProvider:    cpClientProvider,
					BlobStore:         blobStore,
					BundleCacheRoot:   filepath.Join(rootDir, "bundle-cache"),
					ReconcileInterval: 300 * time.Millisecond,
				})
			})
		}
		srv := &itHTTPServer{port: port, svc: svc, errCh: make(chan error, 1)}
		startITHTTPService(t, srv)

		mgr, err := daprhost.NewManager(daprhost.Options{
			StateDir:             cpStateDir,
			AppID:                "sofa-cp-run-it",
			AppPort:              port,
			NameResolutionDBPath: nrDBPath,
		})
		require.NoError(t, err)
		tok.Store(mgr.AppAPIToken())
		client, err := mgr.Start(ctx)
		require.NoError(t, err)
		cpClientRef.Store(&client)
		return srv, &tok, mgr, client
	}

	// ---- Phase 1: Start Control Plane #1 & Worker #1 ----
	cpSrv1, _, mgrCP1, cpClient1 := startControlPlane(true)
	defer func() {
		_ = mgrCP1.Stop(context.Background())
		cpSrv1.stop(t)
	}()

	w1Port := reserveITPort(t)
	var w1Token atomic.Value
	w1Token.Store("")
	w1Router := chi.NewRouter()
	w1Router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := w1Token.Load().(string)
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
	w1Svc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", w1Port), w1Router)
	w1Srv := &itHTTPServer{port: w1Port, svc: w1Svc, errCh: make(chan error, 1)}
	startITHTTPService(t, w1Srv)
	defer w1Srv.stop(t)

	mgrW1, err := daprhost.NewManager(daprhost.Options{
		StateDir:              filepath.Join(rootDir, "w1-state"),
		AppID:                 "sofa-worker-it-1",
		AppPort:               w1Port,
		ExternalSchedulerAddr: mgrCP1.SchedulerAddress(),
		NameResolutionDBPath:  nrDBPath,
	})
	require.NoError(t, err)
	w1Token.Store(mgrW1.AppAPIToken())
	w1Client, err := mgrW1.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = mgrW1.Stop(context.Background())
	}()

	step2CheckpointedCh := make(chan struct{})
	var step2CheckpointedOnce sync.Once
	step2ReleaseCh := make(chan struct{})
	var worker1Killed atomic.Bool

	wd1, err := daemon.New(daemon.Options{
		ProcessID:         "proc-run-it-1",
		AppID:             "sofa-worker-it-1",
		FixedAppID:        "sofa-worker-it-1",
		MaxConcurrency:    2,
		Arch:              "linux/amd64",
		Registry:          daemon.NewDaprRegistryClient(w1Client),
		PubSub:            daemon.NewDaprPubSubPublisher(w1Client),
		BlobStore:         blobStore,
		NodeReporter:      &itNodeOutcomeReporter{client: w1Client, killed: &worker1Killed},
		HeartbeatInterval: 150 * time.Millisecond,
		NodeRunnerDepsProvider: func(_ context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error) {
			remoteCP, cpErr := node.NewRemoteNodeControlPlaneForNode(w1Client, req.RunID, req.NodeAddr, req.Epoch)
			if cpErr != nil {
				return runner.WorkerRunnerDeps{}, cpErr
			}
			return runner.WorkerRunnerDeps{
				ControlPlane:  remoteCP,
				GitAdapter:    git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: req.LookupEnv}),
				BlobStore:     blobStore,
				SandboxEngine: &itSandboxEngine{},
				SessionLauncher: itSessionLauncherFunc(func(_ context.Context, launchReq runner.SessionLaunchRequest) (runner.SessionHandle, error) {
					wsDir := launchReq.MountLayout.HostWorkspaceDir
					opener := &itFakeOpener{
						onOpen: func(cfg harnessruntime.SessionConfig) harnessruntime.Session {
							return &itFakeSession{
								id:  fmt.Sprintf("w1-%s-e%d", req.NodeAddr, req.Epoch),
								cfg: cfg,
								onTurn: func(_ string, scfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
									if req.NodeAddr == "worker.step1" {
										require.NoError(t, os.WriteFile(filepath.Join(wsDir, "allowed", "step1.txt"), []byte("from-step1\n"), 0o644))
										invokeITToolJSON(t, scfg, session.ToolSofaCompleteWork, map[string]string{"summary": "step1 done"})
										return harnessruntime.TurnSummary{}, nil
									}
									// worker.step2 epoch 1: write checkpoint & block mid-RUNNING!
									require.NoError(t, os.WriteFile(filepath.Join(wsDir, "allowed", "step2_e1.txt"), []byte("ckpt-e1\n"), 0o644))
									invokeITToolJSON(t, scfg, session.ToolSofaCheckpoint, session.DiaryInput{
										Completed: []string{"wrote allowed/step2_e1.txt"},
										NextSteps: []string{"finish step2"},
									})
									step2CheckpointedOnce.Do(func() { close(step2CheckpointedCh) })
									<-step2ReleaseCh
									return harnessruntime.TurnSummary{}, context.Canceled
								},
							}
						},
					}
					return &itSessionHandle{opener: opener}, nil
				}),
			}, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, wd1.RegisterHandlers(w1Svc))
	require.NoError(t, wd1.Start(ctx))

	// Submit StartRun via real Dapr RunClient.
	runClient1, err := run.NewDaprRunClient(runID, cpClient1)
	require.NoError(t, err)
	startResp, err := runClient1.StartRun(ctx, &run.StartRunReq{
		RunID:        runID,
		Namespace:    "default",
		PlanBlobKey:  meta.Key,
		PlanSHA256:   meta.SHA256,
		RequiredArch: "linux/amd64",
		TempRoot:     filepath.Join(rootDir, "worker-tmp"),
	})
	require.NoError(t, err)
	assert.True(t, startResp.Accepted)

	// Wait until worker.step1 succeeded and worker.step2 persisted checkpoint e1 while still RUNNING.
	select {
	case <-step2CheckpointedCh:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for worker.step2 epoch 1 checkpoint")
	}
	if regBefore, rErr := cpClient1.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: registry.ActorType, ActorID: registry.GlobalActorID, Method: "ListProcesses", Data: []byte(`{}`),
	}); rErr == nil && regBefore != nil {
		t.Logf("registry before Phase 1 stop: %s", string(regBefore.Data))
	}

	// Simulate abrupt host shutdown mid-execution: freeze worker1 callbacks and stop Control Plane
	// first so proc-run-it-1's active lease remains persisted until Phase 2 recovery.
	worker1Killed.Store(true)
	close(step2ReleaseCh)

	step1ActorID, err := node.EncodeNodeActorID(runID, "worker.step1")
	require.NoError(t, err)
	step2ActorID, err := node.EncodeNodeActorID(runID, "worker.step2")
	require.NoError(t, err)
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, run.OperatorActorType, run.GlobalOperatorActorID)
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, run.RunActorType, runID)
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, actorcfg.ActorType, actorcfg.GlobalActorID)
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, registry.ActorType, registry.GlobalActorID)
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, node.ActorType, step1ActorID)
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, node.ActorType, step2ActorID)

	cpClientRef.Store(nil)
	require.NoError(t, mgrCP1.Stop(ctx))
	cpSrv1.stop(t)

	_ = wd1.Stop(context.Background())
	require.NoError(t, mgrW1.Stop(ctx))
	w1Srv.stop(t)

	// ---- Phase 2: Restart Control Plane #2 on SAME cpStateDir + start Worker #2 ----
	cpSrv2, _, mgrCP2, cpClient2 := startControlPlane(true)
	defer func() {
		_ = mgrCP2.Stop(context.Background())
		cpSrv2.stop(t)
	}()

	w2Port := reserveITPort(t)
	var w2Token atomic.Value
	w2Token.Store("")
	w2Router := chi.NewRouter()
	w2Router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := w2Token.Load().(string)
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
	w2Svc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", w2Port), w2Router)
	w2Srv := &itHTTPServer{port: w2Port, svc: w2Svc, errCh: make(chan error, 1)}
	startITHTTPService(t, w2Srv)
	defer w2Srv.stop(t)

	mgrW2, err := daprhost.NewManager(daprhost.Options{
		StateDir:              filepath.Join(rootDir, "w2-state"),
		AppID:                 "sofa-worker-it-2",
		AppPort:               w2Port,
		ExternalSchedulerAddr: mgrCP2.SchedulerAddress(),
		NameResolutionDBPath:  nrDBPath,
	})
	require.NoError(t, err)
	w2Token.Store(mgrW2.AppAPIToken())
	w2Client, err := mgrW2.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = mgrW2.Stop(context.Background())
	}()

	var (
		w2Mu              sync.Mutex
		step2Epoch2Seen   bool
		step2E1FileRestor bool
		step1ReranOnW2    bool
	)

	wd2, err := daemon.New(daemon.Options{
		ProcessID:         "proc-run-it-2",
		AppID:             "sofa-worker-it-2",
		FixedAppID:        "sofa-worker-it-2",
		MaxConcurrency:    2,
		Arch:              "linux/amd64",
		Registry:          daemon.NewDaprRegistryClient(w2Client),
		PubSub:            daemon.NewDaprPubSubPublisher(w2Client),
		BlobStore:         blobStore,
		SandboxEngine:     &itSandboxEngine{},
		NodeReporter:      &itNodeOutcomeReporter{client: w2Client},
		MergeReporter:     &itMergeResultReporter{client: w2Client},
		HeartbeatInterval: 150 * time.Millisecond,
		NodeRunnerDepsProvider: func(_ context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error) {
			remoteCP, cpErr := node.NewRemoteNodeControlPlaneForNode(w2Client, req.RunID, req.NodeAddr, req.Epoch)
			if cpErr != nil {
				return runner.WorkerRunnerDeps{}, cpErr
			}
			return runner.WorkerRunnerDeps{
				ControlPlane:  remoteCP,
				GitAdapter:    git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: req.LookupEnv}),
				BlobStore:     blobStore,
				SandboxEngine: &itSandboxEngine{},
				SessionLauncher: itSessionLauncherFunc(func(_ context.Context, launchReq runner.SessionLaunchRequest) (runner.SessionHandle, error) {
					wsDir := launchReq.MountLayout.HostWorkspaceDir
					opener := &itFakeOpener{
						onOpen: func(cfg harnessruntime.SessionConfig) harnessruntime.Session {
							return &itFakeSession{
								id:  fmt.Sprintf("w2-%s-e%d", req.NodeAddr, req.Epoch),
								cfg: cfg,
								onTurn: func(_ string, scfg harnessruntime.SessionConfig) (harnessruntime.TurnSummary, error) {
									t.Logf("worker2 session turn start node=%s epoch=%d", req.NodeAddr, req.Epoch)
									if req.NodeAddr == "worker.step1" {
										w2Mu.Lock()
										step1ReranOnW2 = true
										w2Mu.Unlock()
									}
									_, statE1 := os.Stat(filepath.Join(wsDir, "allowed", "step2_e1.txt"))
									w2Mu.Lock()
									if req.Epoch >= 2 {
										step2Epoch2Seen = true
									}
									step2E1FileRestor = statE1 == nil
									w2Mu.Unlock()

									require.NoError(t, os.WriteFile(filepath.Join(wsDir, "allowed", "step2_final.txt"), []byte("done-in-e2\n"), 0o644))
									invokeITToolJSON(t, scfg, session.ToolSofaCompleteWork, map[string]string{"summary": "step2 done in epoch 2"})
									return harnessruntime.TurnSummary{}, nil
								},
							}
						},
					}
					return &itSessionHandle{opener: opener}, nil
				}),
			}, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, wd2.RegisterHandlers(w2Svc))
	require.NoError(t, wd2.Start(ctx))
	defer func() {
		_ = wd2.Stop(context.Background())
	}()

	// Trigger OperatorActor recovery on restarted control plane!
	opStub2 := run.NewOperatorClientStub(cpClient2)
	recResp, err := opStub2.RecoverActiveRuns(ctx, &run.RecoverActiveRunsReq{Reason: "control-plane-restart"})
	require.NoError(t, err)
	assert.Contains(t, recResp.RecoveredRunIDs, runID)

	// Wait for ProcessRegistryActor to detect proc-run-it-1 DEAD, NodeActor(step2) to advance to epoch >= 2,
	// WorkerDaemon #2 to complete step2 from the recovered e1 checkpoint, and RunActor to reach SUCCEEDED!
	runClient2, err := run.NewDaprRunClient(runID, cpClient2)
	require.NoError(t, err)

	var finalState *run.GetRunStateResp
	ok := assert.Eventually(t, func() bool {
		st, gErr := runClient2.GetRunState(ctx, &run.GetRunStateReq{})
		if gErr != nil || st == nil {
			return false
		}
		finalState = st
		return st.Status == run.RunStatusSucceeded
	}, 20*time.Second, 200*time.Millisecond)
	if !ok {
		ns2, _ := node.NewClientStub(step2ActorID, cpClient2).GetState(ctx, &node.GetNodeStateReq{})
		regResp, _ := cpClient2.InvokeActor(ctx, &daprclient.InvokeActorRequest{
			ActorType: registry.ActorType, ActorID: registry.GlobalActorID, Method: "ListProcesses", Data: []byte(`{}`),
		})
		var regData string
		if regResp != nil {
			regData = string(regResp.Data)
		}
		t.Fatalf("expected recovered Run to reach SUCCEEDED; runState=%+v nodeStep2=%+v registry=%s", finalState, ns2, regData)
	}

	w2Mu.Lock()
	assert.False(t, step1ReranOnW2, "completed worker.step1 must not be re-dispatched after restart")
	assert.True(t, step2Epoch2Seen, "worker.step2 must execute at epoch >= 2 on recovery worker")
	assert.True(t, step2E1FileRestor, "worker.step2 epoch 2 must restore e1 checkpoint workspace file")
	w2Mu.Unlock()

	var step2Summary *run.NodeRunSummary
	var mergeSummary *run.NodeRunSummary
	for i := range finalState.Nodes {
		switch finalState.Nodes[i].CanonicalAddress {
		case "worker.step2":
			step2Summary = &finalState.Nodes[i]
		case "merge.final":
			mergeSummary = &finalState.Nodes[i]
		}
	}
	require.NotNil(t, step2Summary)
	assert.Equal(t, node.StatusSucceeded, step2Summary.Status)
	assert.GreaterOrEqual(t, step2Summary.CurrentEpoch, uint64(2))

	require.NotNil(t, mergeSummary)
	assert.Equal(t, node.StatusSucceeded, mergeSummary.Status)
	assert.Equal(t, git.WorkBranchName(runID, "merge.final"), mergeSummary.WorkBranch)

	finalOut, ok := finalState.Outputs["final_branch"]
	require.True(t, ok)
	outVal, err := finalOut.DecodeValue()
	require.NoError(t, err)
	expectedMergeBranch := git.WorkBranchName(runID, "merge.final")
	assert.Equal(t, expectedMergeBranch, outVal.AsString())

	mergedSHA := runGitCLI(t, bareDir, "rev-parse", "--verify", "refs/heads/"+expectedMergeBranch)
	assert.NotEmpty(t, mergedSHA)

	// Clean explicit stop of Worker #2 & Control Plane #2 so zero orphan daprd/scheduler remain.
	require.NoError(t, wd2.Stop(ctx))
	require.NoError(t, mgrW2.Stop(ctx))
	w2Srv.stop(t)
	require.NoError(t, mgrCP2.Stop(ctx))
	cpSrv2.stop(t)
}
