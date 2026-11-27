package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/cli"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/server"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mapLookupEnv(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func buildUnitTestSofaplan(t *testing.T, workDir, hclBody string) []byte {
	t.Helper()
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	writeCLIFile(t, filepath.Join(cfgDir, "main.sofa.hcl"), hclBody)

	loaded := loadTestHCLDir(t, cfgDir)
	outPath := filepath.Join(workDir, "bundle.sofaplan")
	require.NoError(t, plan.SaveArchive(outPath, loaded, plan.ArchiveSource{ConfigDir: cfgDir}))
	raw, err := os.ReadFile(outPath)
	require.NoError(t, err)
	return raw
}

func loadTestHCLDir(t *testing.T, cfgDir string) *plan.Plan {
	t.Helper()
	ctx := t.Context()
	tmpWork := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	opts := cli.Options{WorkingDir: tmpWork}
	require.Equal(t, cli.ExitSuccess, cli.Run(ctx, []string{"init", cfgDir, "--state-dir", ".sofa"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	planFile := filepath.Join(tmpWork, "fixture.sofaplan")
	require.Equal(t, cli.ExitSuccess, cli.Run(ctx, []string{"plan", "--state-dir", ".sofa", "--out", planFile}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	loaded, err := plan.LoadArchive(planFile, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	return loaded.Plan
}

func TestApplyDirectoryCompileVarsAnd422MissingKeysGuidance(t *testing.T) {
	workspace := t.TempDir()
	srcDir := filepath.Join(workspace, "wf")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	writeCLIFile(t, filepath.Join(srcDir, "main.sofa.hcl"), `
variable "model" {
  type = string
}

variable "env_tag" {
  type = string
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "sb" {
  runtime = "podman"
  image   = "alpine:latest"
}

model_provider "openai" {
  type        = "openai"
  endpoint    = "https://api.openai.com/v1"
  api_key_ref = "GEMINI_API_KEY"
}

harness "scripted" {}

worker "build" {
  based_source  = source.project.branch
  model         = var.model
  system_prompt = "tag=${var.env_tag}"
}
`)

	t.Setenv("SOFA_VAR_env_tag", "from-sofa-var")

	var (
		mu           sync.Mutex
		gotAuth      string
		gotNamespace string
		gotPlanBytes []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs" {
			require.NoError(t, r.ParseMultipartForm(32<<20))
			f, _, err := r.FormFile("plan")
			require.NoError(t, err)
			defer func() { _ = f.Close() }()
			planBytes, err := io.ReadAll(f)
			require.NoError(t, err)

			mu.Lock()
			gotAuth = r.Header.Get("Authorization")
			gotNamespace = r.FormValue("namespace")
			gotPlanBytes = planBytes
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":        "missing required environment variables or secrets",
				"missing_keys": []string{"GEMINI_API_KEY", "GITHUB_TOKEN"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	opts := cli.Options{
		WorkingDir: workspace,
		LookupEnv: mapLookupEnv(map[string]string{
			"SOFA_SERVER_ADDR": srv.URL,
			"SOFA_API_TOKEN":   "tok-from-env",
		}),
	}

	var outBuf, errBuf bytes.Buffer
	code := cli.Run(t.Context(), []string{
		"apply",
		"./wf",
		"--namespace", "prod",
		"-var", "model=gpt-5-mini",
	}, strings.NewReader(""), &outBuf, &errBuf, opts)

	assert.Equal(t, cli.ExitFailure, code)
	mu.Lock()
	assert.Equal(t, "Bearer tok-from-env", gotAuth)
	assert.Equal(t, "prod", gotNamespace)
	require.NotEmpty(t, gotPlanBytes)
	mu.Unlock()

	// Verify compiled archive contains resolved -var and SOFA_VAR_* values.
	tmpPlan := filepath.Join(t.TempDir(), "uploaded.sofaplan")
	require.NoError(t, os.WriteFile(tmpPlan, gotPlanBytes, 0o600))
	loaded, err := plan.LoadArchive(tmpPlan, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	byAddr := make(map[string]plan.NodeSpec, len(loaded.Plan.Nodes()))
	for _, n := range loaded.Plan.Nodes() {
		byAddr[n.Address] = n
	}
	workerNode, ok := byAddr["worker.build"]
	require.True(t, ok, "expected worker.build in compiled plan")
	assert.Equal(t, "gpt-5-mini", workerNode.Config.GetAttr("model").AsString())
	assert.Equal(t, "tag=from-sofa-var", workerNode.Config.GetAttr("system_prompt").AsString())

	combinedErr := errBuf.String() + outBuf.String()
	assert.Contains(t, combinedErr, "GEMINI_API_KEY")
	assert.Contains(t, combinedErr, "GITHUB_TOKEN")
	assert.Contains(t, combinedErr, "sofa secret set --from-env")
}

func TestApplyPrebuiltSofaplanDetachAndOverrides(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	buildUnitTestSofaplan(t, workspace, `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}
sandbox "sb" {
  runtime = "podman"
  image   = "alpine:latest"
}
harness "scripted" {}
worker "build" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "Build"
}
`)

	var (
		gotEnvs      map[string]string
		gotSecrets   map[string]string
		gotNamespace string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer flag-token", r.Header.Get("Authorization"))
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/runs", r.URL.Path)
		require.NoError(t, r.ParseMultipartForm(32<<20))
		gotNamespace = r.FormValue("namespace")
		require.NoError(t, json.Unmarshal([]byte(r.FormValue("envs")), &gotEnvs))
		require.NoError(t, json.Unmarshal([]byte(r.FormValue("secrets")), &gotSecrets))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":    "run-detached-1",
			"namespace": gotNamespace,
			"status":    "RUNNING",
		})
	}))
	defer srv.Close()

	opts := cli.Options{
		WorkingDir: workspace,
		LookupEnv: mapLookupEnv(map[string]string{
			"SOFA_SERVER_ADDR": "http://127.0.0.1:9999",
			"SOFA_API_TOKEN":   "env-token",
			"MY_SECRET_KEY":    "super-secret-from-env",
		}),
	}

	var outBuf, errBuf bytes.Buffer
	code := cli.Run(t.Context(), []string{
		"apply",
		"./bundle.sofaplan",
		"--server", srv.URL,
		"--token", "flag-token",
		"--namespace", "staging",
		"--env", "LOG_LEVEL=debug",
		"--secret-env", "MY_SECRET_KEY",
		"-d",
	}, strings.NewReader(""), &outBuf, &errBuf, opts)

	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", errBuf.String())
	assert.Contains(t, outBuf.String(), "run-detached-1")
	assert.NotContains(t, outBuf.String(), "super-secret-from-env")
	assert.Equal(t, "staging", gotNamespace)
	assert.Equal(t, map[string]string{"LOG_LEVEL": "debug"}, gotEnvs)
	assert.Equal(t, map[string]string{"MY_SECRET_KEY": "super-secret-from-env"}, gotSecrets)
}

func TestApplyForegroundCancelSendsImmediatePauseAndReturns130(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	buildUnitTestSofaplan(t, workspace, `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}
sandbox "sb" {
  runtime = "podman"
  image   = "alpine:latest"
}
harness "scripted" {}
worker "build" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "Build"
}
`)

	wsConnected := make(chan struct{})
	var pauseCalled atomic.Bool
	var pauseMode atomic.Value
	var pauseHadValidCtx atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run_id":    "run-fg-cancel",
				"namespace": "default",
				"status":    "RUNNING",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/run-fg-cancel":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.RunDetailResponse{
				RunID:     "run-fg-cancel",
				Namespace: "default",
				Status:    run.RunStatusRunning,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/ws":
			require.Equal(t, "Bearer ws-tok", r.Header.Get("Authorization"))
			conn, err := websocket.Accept(w, r, nil)
			require.NoError(t, err)
			defer func() { _ = conn.CloseNow() }()

			logPayload, _ := json.Marshal(daemon.LogEvent{
				RunID:    "run-fg-cancel",
				NodeAddr: "worker.build",
				Epoch:    1,
				Kind:     "assistant_message_delta",
				Text:     "streaming worker log line",
			})
			evBytes, _ := json.Marshal(server.WSEvent{
				Topic:    daemon.EventsTopic,
				Type:     "assistant_message_delta",
				RunID:    "run-fg-cancel",
				NodeAddr: "worker.build",
				Epoch:    1,
				Payload:  logPayload,
			})
			_ = conn.Write(r.Context(), websocket.MessageText, evBytes)
			select {
			case <-wsConnected:
			default:
				close(wsConnected)
			}
			<-r.Context().Done()
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/run-fg-cancel/pause":
			mode := r.URL.Query().Get("mode")
			if mode == "" {
				var body struct {
					Mode string `json:"mode"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mode = body.Mode
			}
			pauseMode.Store(mode)
			pauseCalled.Store(true)
			pauseHadValidCtx.Store(r.Context().Err() == nil)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(run.PauseRunResp{
				Paused: true,
				RunID:  "run-fg-cancel",
				Status: run.RunStatusPaused,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		<-wsConnected
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var outBuf, errBuf bytes.Buffer
	code := cli.Run(ctx, []string{
		"apply",
		"./bundle.sofaplan",
		"--server", srv.URL,
		"--token", "ws-tok",
	}, strings.NewReader(""), &outBuf, &errBuf, cli.Options{WorkingDir: workspace})

	assert.Equal(t, cli.ExitInterrupt, code)
	assert.True(t, pauseCalled.Load(), "cancelling foreground apply must call POST /pause")
	assert.Equal(t, "immediate", pauseMode.Load())
	assert.True(t, pauseHadValidCtx.Load())
	assert.Contains(t, outBuf.String(), "streaming worker log line")
}

func TestSecretAndEnvSubcommandsNeverLeakPlaintext(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		putPayloads []map[string]any
		deletedKey  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/secrets":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			putPayloads = append(putPayloads, body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config.UpsertEntriesResp{Updated: []string{"GEMINI_API_KEY", "GITHUB_TOKEN"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/secrets":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"scope":     "namespace",
				"namespace": "team-a",
				"secrets": []config.SecretMeta{
					{Name: "GITHUB_TOKEN", Scope: config.ScopeNamespace, Namespace: "team-a", UpdatedAt: time.Unix(1700000000, 0).UTC()},
				},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/secrets/"):
			mu.Lock()
			deletedKey = strings.TrimPrefix(r.URL.Path, "/api/v1/secrets/")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config.DeleteEntryResp{Deleted: true})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/envs":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config.UpsertEntriesResp{Updated: []string{"FOO"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/envs":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"scope": "global",
				"envs": []config.EnvEntry{
					{Name: "FOO", Value: "bar", Scope: config.ScopeGlobal, UpdatedAt: time.Unix(1700000000, 0).UTC()},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	opts := cli.Options{
		WorkingDir: t.TempDir(),
		LookupEnv: mapLookupEnv(map[string]string{
			"SOFA_SERVER_ADDR": srv.URL,
			"GITHUB_TOKEN":     "ghp_super_secret_value_123",
			"GEMINI_API_KEY":   "AIza_super_secret_value_456",
		}),
	}

	var outBuf, errBuf bytes.Buffer
	code := cli.Run(t.Context(), []string{
		"secret", "set", "--namespace", "team-a", "--from-env", "GITHUB_TOKEN", "GEMINI_API_KEY",
	}, strings.NewReader(""), &outBuf, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())
	assert.NotContains(t, outBuf.String(), "ghp_super_secret_value_123")
	assert.NotContains(t, outBuf.String(), "AIza_super_secret_value_456")

	mu.Lock()
	require.Len(t, putPayloads, 1)
	assert.Equal(t, "namespace", putPayloads[0]["scope"])
	assert.Equal(t, "team-a", putPayloads[0]["namespace"])
	mu.Unlock()

	outBuf.Reset()
	errBuf.Reset()
	code = cli.Run(t.Context(), []string{"secret", "list", "--namespace", "team-a"}, strings.NewReader(""), &outBuf, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())
	assert.Contains(t, outBuf.String(), "GITHUB_TOKEN")
	assert.NotContains(t, outBuf.String(), "ghp_super_secret_value_123")

	outBuf.Reset()
	code = cli.Run(t.Context(), []string{"secret", "delete", "--namespace", "team-a", "GITHUB_TOKEN"}, strings.NewReader(""), &outBuf, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())
	mu.Lock()
	assert.Equal(t, "GITHUB_TOKEN", deletedKey)
	mu.Unlock()

	// Missing env var fails before sending partial writes.
	outBuf.Reset()
	errBuf.Reset()
	code = cli.Run(t.Context(), []string{"secret", "set", "--from-env", "MISSING_VAR"}, strings.NewReader(""), &outBuf, &errBuf, opts)
	assert.Equal(t, cli.ExitFailure, code)
	assert.Contains(t, errBuf.String(), "MISSING_VAR")

	// Env set & list.
	outBuf.Reset()
	errBuf.Reset()
	code = cli.Run(t.Context(), []string{"env", "set", "FOO=bar"}, strings.NewReader(""), &outBuf, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())
	outBuf.Reset()
	code = cli.Run(t.Context(), []string{"env", "list"}, strings.NewReader(""), &outBuf, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())
	assert.Contains(t, outBuf.String(), "FOO")
	assert.Contains(t, outBuf.String(), "bar")
}

func TestOpsAndDecisionsRespondAutoPopulatesCardMetadata(t *testing.T) {
	t.Parallel()

	var (
		mu              sync.Mutex
		gotRespondBody  map[string]any
		gotInterject    node.EnqueueInterjectionReq
		gotPauseMode    string
		gotRevealQuery  string
		gotLogEpoch     string
		gotRerunCalled  bool
		gotResumeCalled bool
		gotDeleteCalled bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs":
			_ = json.NewEncoder(w).Encode(run.ListRunsResp{
				Runs: []run.RunSummary{{RunID: "run-1", Namespace: "default", Status: run.RunStatusWaitingHuman}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/run-1":
			mu.Lock()
			gotRevealQuery = r.URL.Query().Get("reveal_sensitive")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(server.RunDetailResponse{
				RunID:     "run-1",
				Namespace: "default",
				Status:    run.RunStatusSucceeded,
				Outputs: map[string]server.RunOutputView{
					"secret_out": {Name: "secret_out", Sensitive: true, Value: "revealed-val"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/run-1/nodes/worker.build/logs":
			mu.Lock()
			gotLogEpoch = r.URL.Query().Get("epoch")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"kind":"assistant_message","text":"hello log"}`+"\n")
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/run-1/pause":
			var body struct {
				Mode string `json:"mode"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			gotPauseMode = body.Mode
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(run.PauseRunResp{Paused: true, RunID: "run-1", Status: run.RunStatusPaused})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/run-1/resume":
			mu.Lock()
			gotResumeCalled = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(run.ResumeRunResp{Resumed: true, RunID: "run-1", Status: run.RunStatusRunning})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/run-1/nodes/worker.build/rerun":
			mu.Lock()
			gotRerunCalled = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(run.RerunNodeResp{Accepted: true, RunStatus: run.RunStatusRunning})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/runs/run-1":
			mu.Lock()
			gotDeleteCalled = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(run.DeleteRunResp{Deleted: true, Status: run.RunStatusDeleted})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/run-1/nodes/worker.build/diary-messages":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotInterject))
			_ = json.NewEncoder(w).Encode(node.EnqueueInterjectionResp{ID: "ij-1", Status: node.InterjectionStatusPending})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/decisions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"decisions": []node.DecisionCard{
					{
						CardID:      "dec-approve-1",
						RunID:       "run-1",
						NodeAddr:    "worker.build",
						Kind:        node.DecisionKindHumanApproval,
						Status:      node.DecisionStatusPending,
						CommitSHA:   "0123456789abcdef0123456789abcdef01234567",
						ArtifactIDs: []string{"art-1", "art-2"},
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/decisions/dec-approve-1/respond":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			gotRespondBody = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(node.SubmitDecisionResp{Accepted: true, Status: node.StatusSucceeded})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	opts := cli.Options{
		WorkingDir: t.TempDir(),
		LookupEnv:  mapLookupEnv(map[string]string{"SOFA_SERVER_ADDR": srv.URL}),
	}

	var outBuf, errBuf bytes.Buffer
	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"runs"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Contains(t, outBuf.String(), "run-1")

	outBuf.Reset()
	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"runs", "get", "run-1", "--reveal-sensitive"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Equal(t, "true", gotRevealQuery)
	assert.Contains(t, outBuf.String(), "revealed-val")

	outBuf.Reset()
	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"logs", "run-1", "worker.build", "--epoch", "2"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Equal(t, "2", gotLogEpoch)
	assert.Contains(t, outBuf.String(), "hello log")

	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"pause", "--mode", "immediate", "run-1"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Equal(t, "immediate", gotPauseMode)

	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"resume", "run-1"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.True(t, gotResumeCalled)

	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"rerun", "run-1", "worker.build"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.True(t, gotRerunCalled)

	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"interject", "run-1", "worker.build", "-m", "check tests", "--wait"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Equal(t, "check tests", gotInterject.Message)
	assert.True(t, gotInterject.WaitForConfirmation)

	outBuf.Reset()
	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"decisions", "list"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Contains(t, outBuf.String(), "dec-approve-1")

	outBuf.Reset()
	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"decisions", "respond", "dec-approve-1", "--approve", "--comment", "looks good"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.Equal(t, "run-1", gotRespondBody["run_id"])
	assert.Equal(t, "worker.build", gotRespondBody["node_addr"])
	assert.Equal(t, true, gotRespondBody["approved"])
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", gotRespondBody["commit_sha"])
	assert.Equal(t, []any{"art-1", "art-2"}, gotRespondBody["artifact_ids"])
	assert.Equal(t, "looks good", gotRespondBody["answer_text"])

	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"delete", "run-1"}, strings.NewReader(""), &outBuf, &errBuf, opts), errBuf.String())
	assert.True(t, gotDeleteCalled)
}

func TestServerSecurityRejectsMissingAPITokenWhenNotDevLoopback(t *testing.T) {
	t.Parallel()

	var outBuf, errBuf bytes.Buffer
	code := cli.Run(t.Context(), []string{
		"server",
		"--listen", "127.0.0.1:0",
	}, strings.NewReader(""), &outBuf, &errBuf, cli.Options{
		WorkingDir: t.TempDir(),
		LookupEnv:  mapLookupEnv(map[string]string{}),
	})
	assert.Equal(t, cli.ExitFailure, code)
	assert.Contains(t, errBuf.String(), "api_token")
}

func TestApplyDirectoryBundlesExternalSkillDirsRelativeToSourceDir(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	wfDir := filepath.Join(workspace, "wf")
	extDir := filepath.Join(workspace, "ext-skills")
	require.NoError(t, os.MkdirAll(wfDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(extDir, "shared"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(extDir, "shared", "SKILL.md"), []byte("# Shared\n"), 0o644))

	writeCLIFile(t, filepath.Join(wfDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}
sandbox "sb" {
  runtime = "podman"
  image   = "alpine:latest"
}
harness "local" {}
worker "build" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Build"
  skill_directories = ["../ext-skills"]
}
`)

	var (
		mu           sync.Mutex
		gotPlanBytes []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs" {
			require.NoError(t, r.ParseMultipartForm(32<<20))
			f, _, err := r.FormFile("plan")
			require.NoError(t, err)
			defer func() { _ = f.Close() }()
			planBytes, err := io.ReadAll(f)
			require.NoError(t, err)
			mu.Lock()
			gotPlanBytes = planBytes
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run_id":    "run-ext-1",
				"namespace": "default",
				"status":    "RUNNING",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	opts := cli.Options{WorkingDir: workspace}
	var outBuf, errBuf bytes.Buffer
	code := cli.Run(t.Context(), []string{"apply", "./wf", "--server", srv.URL, "--token", "t", "-d"}, strings.NewReader(""), &outBuf, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", errBuf.String())

	mu.Lock()
	raw := gotPlanBytes
	mu.Unlock()
	require.NotEmpty(t, raw)

	tmpPlan := filepath.Join(t.TempDir(), "uploaded.sofaplan")
	require.NoError(t, os.WriteFile(tmpPlan, raw, 0o600))
	loaded, err := plan.LoadArchive(tmpPlan, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	var workerNode plan.NodeSpec
	for _, n := range loaded.Plan.Nodes() {
		if n.Address == "worker.build" {
			workerNode = n
		}
	}
	require.Equal(t, "worker.build", workerNode.Address)
	var dirs []string
	for it := workerNode.Config.GetAttr("skill_directories").ElementIterator(); it.Next(); {
		_, elem := it.Element()
		dirs = append(dirs, elem.AsString())
	}
	require.Len(t, dirs, 1)
	assert.True(t, strings.HasPrefix(dirs[0], "external_skills/"))
	extracted, err := os.ReadFile(filepath.Join(loaded.ExtractDir, filepath.FromSlash(dirs[0]), "shared", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Shared\n", string(extracted))
}
