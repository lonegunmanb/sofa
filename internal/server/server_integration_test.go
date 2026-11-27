//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	actorcfg "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/lonegunmanb/sofa/internal/server"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reserveITPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func runGitIT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initBareRepoIT(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bareDir := filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")

	runGitIT(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runGitIT(t, seedDir, "init")
	runGitIT(t, seedDir, "config", "user.name", "seed")
	runGitIT(t, seedDir, "config", "user.email", "seed@sofa.local")
	runGitIT(t, seedDir, "config", "commit.gpgsign", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(seedDir, "allowed"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "allowed", "base.txt"), []byte("base\n"), 0o644))
	runGitIT(t, seedDir, "add", "-A")
	runGitIT(t, seedDir, "commit", "-m", "initial")
	runGitIT(t, seedDir, "branch", "-M", "main")
	runGitIT(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	return bareDir
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
func (h *itSessionHandle) ContainerID() string           { return "it-server-ctr" }
func (h *itSessionHandle) Close() error                  { return nil }

type itLauncherFunc func(ctx context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error)

func (f itLauncherFunc) Launch(ctx context.Context, req runner.SessionLaunchRequest) (runner.SessionHandle, error) {
	return f(ctx, req)
}

type itOpener struct {
	onOpen func(cfg harnessruntime.SessionConfig) harnessruntime.Session
}

func (o *itOpener) Capabilities(_ context.Context) (harnessruntime.Capabilities, error) {
	return harnessruntime.Capabilities{HarnessName: "copilot", SupportsSessionResume: true}, nil
}
func (o *itOpener) Open(_ context.Context, cfg harnessruntime.SessionConfig) (harnessruntime.Session, error) {
	return o.onOpen(cfg), nil
}

type itSession struct {
	id     string
	cfg    harnessruntime.SessionConfig
	onTurn func(prompt string, cfg harnessruntime.SessionConfig, onEvent func(harnessruntime.Event)) (harnessruntime.TurnSummary, error)
}

func (s *itSession) ID() string { return s.id }
func (s *itSession) RunTurn(_ context.Context, prompt string, onEvent func(harnessruntime.Event)) (harnessruntime.TurnSummary, error) {
	return s.onTurn(prompt, s.cfg, onEvent)
}
func (s *itSession) Abort(_ context.Context) error  { return nil }
func (s *itSession) Resume(_ context.Context) error { return nil }
func (s *itSession) ReadMCPResource(_ context.Context, _, _ string) ([]harnessruntime.MCPResourceContent, error) {
	return nil, nil
}
func (s *itSession) Close(_ context.Context) error { return nil }

type dynamicCPAdapter struct {
	provider func() daprclient.Client
}

func (a *dynamicCPAdapter) RegisterProcess(ctx context.Context, req *registry.RegisterProcessReq) (*registry.RegisterProcessResp, error) {
	return registry.NewClientStub(a.provider()).RegisterProcess(ctx, req)
}
func (a *dynamicCPAdapter) Heartbeat(ctx context.Context, req *registry.HeartbeatReq) (*registry.HeartbeatResp, error) {
	return registry.NewClientStub(a.provider()).Heartbeat(ctx, req)
}
func (a *dynamicCPAdapter) ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	return registry.NewClientStub(a.provider()).ReleaseSlot(ctx, req)
}
func (a *dynamicCPAdapter) PublishEvent(ctx context.Context, pubsubName, topicName string, data any) error {
	return a.provider().PublishEvent(ctx, pubsubName, topicName, data)
}
func (a *dynamicCPAdapter) ReportNodeOutcome(ctx context.Context, report daemon.NodeOutcomeReport) error {
	stub, err := node.NewClientStubForNode(report.RunID, report.NodeAddr, a.provider())
	if err != nil {
		return err
	}
	_, err = stub.ReportNodeOutcome(ctx, &report)
	return err
}

func TestServer_RealDaprEndToEndPreflightWSRunPreviewAndDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rootDir := t.TempDir()
	nrDBPath := filepath.Join(rootDir, "nameresolution.db")
	blobStore, err := blob.NewLocalStore(filepath.Join(rootDir, "blobs"))
	require.NoError(t, err)
	bareDir := initBareRepoIT(t)

	// 1. Build a .sofaplan requiring GEMINI_API_KEY, producing an HTML artifact, and exposing a sensitive output.
	hclBody := fmt.Sprintf(`
source "project" {
  url    = %q
  branch = "main"
}

sandbox "sb" {
  runtime = "podman"
  image   = "alpine:latest"
}

model_provider "gemini" {
  type        = "openai"
  endpoint    = "https://generativelanguage.googleapis.com"
  api_key_ref = "GEMINI_API_KEY"
}

harness "copilot" {}

worker "build" {
  based_source   = source.project.branch
  model          = "gemini-2.5-pro"
  system_prompt  = "Generate HTML report"
  writable_paths = ["allowed/**"]
  artifact "report" {
    type        = "file"
    path        = "report.html"
    description = "Generated HTML report"
  }
}

output "work_branch" {
  value = worker.build.work_branch
}

output "secret_note" {
  value     = "top-secret-output-note"
  sensitive = true
}
`, bareDir)
	planBytes := buildTestSofaplanBytes(t, hclBody)

	// 2. Start Control Plane + co-located WorkerDaemon dual-port server + daprhost.Manager.
	var cpClientRef atomic.Pointer[daprclient.Client]
	cpClientProvider := func() daprclient.Client {
		c := cpClientRef.Load()
		if c == nil {
			return nil
		}
		return *c
	}

	cpAdapter := &dynamicCPAdapter{provider: cpClientProvider}
	wd, err := daemon.New(daemon.Options{
		ProcessID:         "proc-srv-it-1",
		AppID:             "sofa-cp-server-it",
		FixedAppID:        "sofa-cp-server-it",
		DevMode:           true,
		MaxConcurrency:    2,
		Arch:              "linux/amd64",
		Registry:          cpAdapter,
		PubSub:            cpAdapter,
		BlobStore:         blobStore,
		NodeReporter:      cpAdapter,
		HeartbeatInterval: 200 * time.Millisecond,
		TempDir:           filepath.Join(rootDir, "worker-tmp"),
		NodeRunnerDepsProvider: func(_ context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error) {
			c := cpClientProvider()
			remoteCP, cpErr := node.NewRemoteNodeControlPlaneForNode(c, req.RunID, req.NodeAddr, req.Epoch)
			if cpErr != nil {
				return runner.WorkerRunnerDeps{}, cpErr
			}
			return runner.WorkerRunnerDeps{
				ControlPlane:  remoteCP,
				GitAdapter:    git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: req.LookupEnv}),
				BlobStore:     blobStore,
				SandboxEngine: &itSandboxEngine{},
				SessionLauncher: itLauncherFunc(func(_ context.Context, launchReq runner.SessionLaunchRequest) (runner.SessionHandle, error) {
					wsDir := launchReq.MountLayout.HostWorkspaceDir
					artDir := launchReq.MountLayout.ArtifactsDir
					opener := &itOpener{
						onOpen: func(cfg harnessruntime.SessionConfig) harnessruntime.Session {
							return &itSession{
								id:  "srv-it-sess",
								cfg: cfg,
								onTurn: func(_ string, scfg harnessruntime.SessionConfig, onEvent func(harnessruntime.Event)) (harnessruntime.TurnSummary, error) {
									if onEvent != nil {
										onEvent(harnessruntime.Event{
											EventID: "it-ev-msg",
											Kind:    harnessruntime.EventAssistantMessageDelta,
											Text:    "building html report",
										})
									}
									require.NoError(t, os.WriteFile(filepath.Join(wsDir, "allowed", "hello.txt"), []byte("hello\n"), 0o644))
									require.NoError(t, os.MkdirAll(artDir, 0o755))
									require.NoError(t, os.WriteFile(filepath.Join(artDir, "report.html"), []byte("<html><body><h1>Artifact Report</h1></body></html>"), 0o644))
									raw, mErr := json.Marshal(map[string]string{"summary": "report ready"})
									require.NoError(t, mErr)
									h := scfg.ToolHandlers[session.ToolSofaCompleteWork]
									require.NotNil(t, h)
									out, tErr := h(context.Background(), harnessruntime.ToolInvocation{
										ToolCallID:    "tc-complete",
										ToolName:      session.ToolSofaCompleteWork,
										ArgumentsJSON: raw,
									})
									require.NoError(t, tErr)
									require.Empty(t, out.HandlerError)
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

	cpInternalPort := reserveITPort(t)
	apiToken := "it-server-bearer-token"
	srv, err := server.New(server.Config{
		PublicAddr:   "127.0.0.1:0",
		InternalAddr: fmt.Sprintf("127.0.0.1:%d", cpInternalPort),
		APIToken:     apiToken,
		Backend:      server.NewDaprBackend(cpClientProvider),
		BlobStore:    blobStore,
		RunDefaults: server.RunStartDefaults{
			RequiredArch: "linux/amd64",
		},
		RegisterInternal: func(svc common.Service) error {
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return actorcfg.NewConfigActor(cpClientProvider)
			})
			svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
				return registry.NewProcessRegistryActor(registry.Options{
					ClientProvider: cpClientProvider,
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
					ReconcileInterval: 250 * time.Millisecond,
				})
			})
			return wd.RegisterHandlers(svc)
		},
	})
	require.NoError(t, err)

	mgrCP, err := daprhost.NewManager(daprhost.Options{
		StateDir:             filepath.Join(rootDir, "cp-state"),
		AppID:                "sofa-cp-server-it",
		AppPort:              cpInternalPort,
		NameResolutionDBPath: nrDBPath,
	})
	require.NoError(t, err)
	srv.SetAppAPIToken(mgrCP.AppAPIToken())

	require.NoError(t, srv.Start(ctx))
	defer func() {
		_ = srv.Stop(context.Background())
	}()

	cpClient, err := mgrCP.Start(ctx)
	require.NoError(t, err)
	cpClientRef.Store(&cpClient)
	defer func() {
		_ = mgrCP.Stop(context.Background())
	}()

	require.NoError(t, wd.Start(ctx))
	defer func() {
		_ = wd.Stop(context.Background())
	}()

	publicBase := "http://" + srv.PublicAddr()

	// 4. Public /actors/* must be 404.
	actResp, err := http.Get(publicBase + "/actors/ConfigActor/global/method/ListMetadata")
	require.NoError(t, err)
	_ = actResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, actResp.StatusCode)

	// 5. Submit .sofaplan before configuring GEMINI_API_KEY -> 422 missing_keys.
	runID := "run-srv-it-1"
	submitRunMultipart := func() *http.Response {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		require.NoError(t, mw.WriteField("namespace", "default"))
		require.NoError(t, mw.WriteField("run_id", runID))
		fw, fErr := mw.CreateFormFile("plan", "plan.sofaplan")
		require.NoError(t, fErr)
		_, fErr = fw.Write(planBytes)
		require.NoError(t, fErr)
		require.NoError(t, mw.Close())

		req, rErr := http.NewRequest(http.MethodPost, publicBase+"/api/v1/runs", &buf)
		require.NoError(t, rErr)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, dErr := http.DefaultClient.Do(req)
		require.NoError(t, dErr)
		return resp
	}

	resp422 := submitRunMultipart()
	assert.Equal(t, http.StatusUnprocessableEntity, resp422.StatusCode)
	var pfErr struct {
		MissingKeys []string `json:"missing_keys"`
	}
	require.NoError(t, json.NewDecoder(resp422.Body).Decode(&pfErr))
	_ = resp422.Body.Close()
	assert.Equal(t, []string{"GEMINI_API_KEY"}, pfErr.MissingKeys)

	// 6. Configure GEMINI_API_KEY via PUT /api/v1/secrets and verify write-only GET /api/v1/secrets.
	putSecReq, _ := http.NewRequest(http.MethodPut, publicBase+"/api/v1/secrets", strings.NewReader(`{"scope":"global","name":"GEMINI_API_KEY","value":"it-gemini-key"}`))
	putSecReq.Header.Set("Authorization", "Bearer "+apiToken)
	putSecResp, err := http.DefaultClient.Do(putSecReq)
	require.NoError(t, err)
	_ = putSecResp.Body.Close()
	assert.Equal(t, http.StatusOK, putSecResp.StatusCode)

	getSecReq, _ := http.NewRequest(http.MethodGet, publicBase+"/api/v1/secrets?scope=global", nil)
	getSecReq.Header.Set("Authorization", "Bearer "+apiToken)
	getSecResp, err := http.DefaultClient.Do(getSecReq)
	require.NoError(t, err)
	secRaw, _ := io.ReadAll(getSecResp.Body)
	_ = getSecResp.Body.Close()
	assert.Equal(t, http.StatusOK, getSecResp.StatusCode)
	assert.NotContains(t, string(secRaw), "it-gemini-key")
	assert.NotContains(t, string(secRaw), `"value"`)

	// 7. Connect to /api/v1/ws?run_id=<runID> via browser subprotocol auth BEFORE starting the Run.
	wsURL := "ws://" + srv.PublicAddr() + "/api/v1/ws?run_id=" + url.QueryEscape(runID)
	b64Token := base64.RawURLEncoding.EncodeToString([]byte(apiToken))
	wsConn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"sofa.v1", "sofa.bearer." + b64Token},
	})
	require.NoError(t, err)
	assert.Equal(t, "sofa.v1", wsConn.Subprotocol())

	var (
		wsMu     sync.Mutex
		wsEvents []server.WSEvent
	)
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		for {
			_, data, rErr := wsConn.Read(ctx)
			if rErr != nil {
				return
			}
			var ev server.WSEvent
			if json.Unmarshal(data, &ev) == nil {
				wsMu.Lock()
				wsEvents = append(wsEvents, ev)
				wsMu.Unlock()
			}
		}
	}()

	// 8. Submit Run -> 201 Created!
	resp201 := submitRunMultipart()
	assert.Equal(t, http.StatusCreated, resp201.StatusCode)
	_ = resp201.Body.Close()

	// 9. Wait for Run to reach SUCCEEDED and WebSocket to receive live worker + run events!
	var runDetail server.RunDetailResponse
	require.Eventually(t, func() bool {
		req, _ := http.NewRequest(http.MethodGet, publicBase+"/api/v1/runs/"+runID, nil)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, gErr := http.DefaultClient.Do(req)
		if gErr != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		if dErr := json.NewDecoder(resp.Body).Decode(&runDetail); dErr != nil {
			return false
		}
		return runDetail.Status == run.RunStatusSucceeded
	}, 20*time.Second, 200*time.Millisecond)

	// Verify sensitive output masked by default and revealed with ?reveal_sensitive=true.
	assert.True(t, runDetail.Outputs["secret_note"].Sensitive)
	assert.Equal(t, "<sensitive>", runDetail.Outputs["secret_note"].Value)
	assert.Equal(t, git.WorkBranchName(runID, "worker.build"), runDetail.Outputs["work_branch"].Value)

	revReq, _ := http.NewRequest(http.MethodGet, publicBase+"/api/v1/runs/"+runID+"?reveal_sensitive=true", nil)
	revReq.Header.Set("Authorization", "Bearer "+apiToken)
	revResp, err := http.DefaultClient.Do(revReq)
	require.NoError(t, err)
	var revDetail server.RunDetailResponse
	require.NoError(t, json.NewDecoder(revResp.Body).Decode(&revDetail))
	_ = revResp.Body.Close()
	assert.Equal(t, "top-secret-output-note", revDetail.Outputs["secret_note"].Value)

	// Verify live WebSocket received events from PubSub.
	okWS := assert.Eventually(t, func() bool {
		wsMu.Lock()
		defer wsMu.Unlock()
		hasWorkerLog := false
		hasRunSucceeded := false
		for _, ev := range wsEvents {
			if ev.Topic == daemon.EventsTopic && strings.Contains(string(ev.Payload), "building html report") {
				hasWorkerLog = true
			}
			if ev.Topic == run.DefaultRunEventsTopic && strings.Contains(string(ev.Payload), string(run.RunStatusSucceeded)) {
				hasRunSucceeded = true
			}
		}
		return hasWorkerLog && hasRunSucceeded
	}, 10*time.Second, 100*time.Millisecond)
	if !okWS {
		wsMu.Lock()
		t.Logf("received %d wsEvents: %+v", len(wsEvents), wsEvents)
		wsMu.Unlock()
		subReq, _ := http.NewRequest(http.MethodGet, "http://"+srv.InternalAddr()+"/dapr/subscribe", nil)
		subReq.Header.Set(common.APITokenKey, mgrCP.AppAPIToken())
		if subResp, sErr := http.DefaultClient.Do(subReq); sErr == nil {
			b, _ := io.ReadAll(subResp.Body)
			_ = subResp.Body.Close()
			t.Logf("/dapr/subscribe status=%d body=%s", subResp.StatusCode, string(b))
		}
		t.Fatalf("expected both worker log and run SUCCEEDED events on WebSocket")
	}

	_ = wsConn.Close(websocket.StatusNormalClosure, "done")
	<-wsDone

	// 10. Mint preview ticket and fetch artifact content with forced CSP headers.
	var artID string
	for _, n := range runDetail.Nodes {
		if n.CanonicalAddress == "worker.build" && len(n.Artifacts) > 0 {
			artID = n.Artifacts[0].ID
			break
		}
	}
	require.NotEmpty(t, artID)

	tReq, _ := http.NewRequest(http.MethodPost, publicBase+"/api/v1/runs/"+runID+"/artifacts/"+artID+"/preview-ticket", nil)
	tReq.Header.Set("Authorization", "Bearer "+apiToken)
	tResp, err := http.DefaultClient.Do(tReq)
	require.NoError(t, err)
	var ticketResp struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.NewDecoder(tResp.Body).Decode(&ticketResp))
	_ = tResp.Body.Close()
	require.NotEmpty(t, ticketResp.Ticket)

	cResp, err := http.Get(publicBase + "/api/v1/runs/" + runID + "/artifacts/" + artID + "/content?ticket=" + url.QueryEscape(ticketResp.Ticket))
	require.NoError(t, err)
	cBody, _ := io.ReadAll(cResp.Body)
	_ = cResp.Body.Close()
	assert.Equal(t, http.StatusOK, cResp.StatusCode)
	assert.Equal(t, "<html><body><h1>Artifact Report</h1></body></html>", string(cBody))
	assert.Equal(t, expectedCSPHeader, cResp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", cResp.Header.Get("X-Content-Type-Options"))

	// 11. Verify JSONL logs streamed over GET /api/v1/runs/{id}/nodes/worker.build/logs.
	lReq, _ := http.NewRequest(http.MethodGet, publicBase+"/api/v1/runs/"+runID+"/nodes/worker.build/logs", nil)
	lReq.Header.Set("Authorization", "Bearer "+apiToken)
	lResp, err := http.DefaultClient.Do(lReq)
	require.NoError(t, err)
	lBody, _ := io.ReadAll(lResp.Body)
	_ = lResp.Body.Close()
	assert.Equal(t, http.StatusOK, lResp.StatusCode)
	assert.Contains(t, string(lBody), "building html report")

	// 12. DELETE /api/v1/runs/{run_id} and verify full cleanup of branch & blobs while main remains.
	dReq, _ := http.NewRequest(http.MethodDelete, publicBase+"/api/v1/runs/"+runID, nil)
	dReq.Header.Set("Authorization", "Bearer "+apiToken)
	dResp, err := http.DefaultClient.Do(dReq)
	require.NoError(t, err)
	_ = dResp.Body.Close()
	assert.Equal(t, http.StatusOK, dResp.StatusCode)

	_, statErr := blobStore.Stat(ctx, fmt.Sprintf("runs/%s/plan/plan.sofaplan", runID))
	assert.ErrorIs(t, statErr, blob.ErrNotFound)
	mainSHA := runGitIT(t, bareDir, "rev-parse", "--verify", "refs/heads/main")
	assert.NotEmpty(t, mainSHA)
	workBranch := git.WorkBranchName(runID, "worker.build")
	checkCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+workBranch)
	checkCmd.Dir = bareDir
	assert.Error(t, checkCmd.Run(), "intermediate work branch %s must be deleted", workBranch)

	// Explicit orderly teardown so zero orphan scheduler/daprd processes remain.
	require.NoError(t, wd.Stop(ctx))
	require.NoError(t, mgrCP.Stop(ctx))
	require.NoError(t, srv.Stop(ctx))
}
