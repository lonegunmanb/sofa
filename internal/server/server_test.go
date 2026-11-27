package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dapr/go-sdk/service/common"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	appcfg "github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	"github.com/lonegunmanb/sofa/internal/server"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

const expectedCSPHeader = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'"

// memBackend implements server.Backend deterministically in memory for Tier 1 unit tests.
type memBackend struct {
	mu sync.Mutex

	runs            map[string]*run.GetRunStateResp
	runSummaries    map[string]run.RunSummary
	nodes           map[string]*node.GetNodeStateResp // key: runID + "\x00" + nodeAddr
	envs            map[string]config.EnvEntry        // key: scope|ns|runID|name
	secrets         map[string]secretRecord           // key: scope|ns|runID|name
	startCalls      []run.StartRunReq
	pauseCalls      []string
	resumeCalls     []string
	deleteCalls     []string
	rerunCalls      []run.RerunNodeReq
	submitCalls     []node.SubmitDecisionReq
	interjectCalls  []node.EnqueueInterjectionReq
	sharedMerged    map[string]bool // key: runID + "\x00" + nodeAddr
	invalidDecision map[string]error
}

type secretRecord struct {
	meta  config.SecretMeta
	value string
}

func newMemBackend() *memBackend {
	return &memBackend{
		runs:            make(map[string]*run.GetRunStateResp),
		runSummaries:    make(map[string]run.RunSummary),
		nodes:           make(map[string]*node.GetNodeStateResp),
		envs:            make(map[string]config.EnvEntry),
		secrets:         make(map[string]secretRecord),
		sharedMerged:    make(map[string]bool),
		invalidDecision: make(map[string]error),
	}
}

func cfgKey(scope config.Scope, ns, runID, name string) string {
	return string(scope) + "|" + config.NormalizeNamespace(ns) + "|" + runID + "|" + name
}

func (b *memBackend) ListRuns(_ context.Context, req *run.ListRunsReq) (*run.ListRunsResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]run.RunSummary, 0, len(b.runSummaries))
	for _, s := range b.runSummaries {
		if req != nil && req.Namespace != "" && s.Namespace != req.Namespace {
			continue
		}
		if req != nil && req.Status != "" && s.Status != req.Status {
			continue
		}
		out = append(out, s)
	}
	return &run.ListRunsResp{Runs: out}, nil
}

func (b *memBackend) StartRun(_ context.Context, req *run.StartRunReq) (*run.StartRunResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startCalls = append(b.startCalls, *req)
	ns := req.Namespace
	if ns == "" {
		ns = run.DefaultNamespace
	}
	now := time.Now().UTC()
	b.runSummaries[req.RunID] = run.RunSummary{
		RunID:       req.RunID,
		Namespace:   ns,
		Status:      run.RunStatusRunning,
		PlanBlobKey: req.PlanBlobKey,
		PlanSHA256:  req.PlanSHA256,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	b.runs[req.RunID] = &run.GetRunStateResp{
		RunID:       req.RunID,
		Namespace:   ns,
		Status:      run.RunStatusRunning,
		PlanBlobKey: req.PlanBlobKey,
		PlanSHA256:  req.PlanSHA256,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return &run.StartRunResp{
		Accepted: true,
		RunID:    req.RunID,
		Status:   run.RunStatusRunning,
	}, nil
}

func (b *memBackend) GetRunState(_ context.Context, runID string) (*run.GetRunStateResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.runs[runID]
	if !ok {
		return nil, run.ErrRunNotFound
	}
	cp := *st
	return &cp, nil
}

func (b *memBackend) PauseRun(_ context.Context, runID string, req *run.PauseRunReq) (*run.PauseRunResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.runs[runID]
	if !ok {
		return nil, run.ErrRunNotFound
	}
	mode := "planned"
	if req != nil && req.Mode != "" {
		mode = req.Mode
	}
	b.pauseCalls = append(b.pauseCalls, runID+":"+mode)
	st.ManualPaused = true
	if mode == "immediate" {
		st.Status = run.RunStatusPaused
	} else {
		st.Status = run.RunStatusPausing
	}
	if sum, exists := b.runSummaries[runID]; exists {
		sum.Status = st.Status
		b.runSummaries[runID] = sum
	}
	return &run.PauseRunResp{Paused: true, RunID: runID, Status: st.Status}, nil
}

func (b *memBackend) ResumeRun(_ context.Context, runID string) (*run.ResumeRunResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.runs[runID]
	if !ok {
		return nil, run.ErrRunNotFound
	}
	b.resumeCalls = append(b.resumeCalls, runID)
	st.ManualPaused = false
	st.Status = run.RunStatusRunning
	if sum, exists := b.runSummaries[runID]; exists {
		sum.Status = st.Status
		b.runSummaries[runID] = sum
	}
	return &run.ResumeRunResp{Resumed: true, RunID: runID, Status: st.Status}, nil
}

func (b *memBackend) DeleteRun(_ context.Context, runID string) (*run.DeleteRunResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.runs[runID]; !ok {
		return nil, run.ErrRunNotFound
	}
	b.deleteCalls = append(b.deleteCalls, runID)
	delete(b.runs, runID)
	delete(b.runSummaries, runID)
	return &run.DeleteRunResp{Deleted: true, Status: run.RunStatusDeleted}, nil
}

func (b *memBackend) RerunNode(_ context.Context, runID string, req *run.RerunNodeReq) (*run.RerunNodeResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.runs[runID]
	if !ok {
		return nil, run.ErrRunNotFound
	}
	b.rerunCalls = append(b.rerunCalls, *req)
	if b.sharedMerged[runID+"\x00"+req.NodeAddr] {
		return nil, run.ErrAlreadyMergedSharedBranch
	}
	st.Status = run.RunStatusRunning
	return &run.RerunNodeResp{Accepted: true, RunStatus: run.RunStatusRunning}, nil
}

func (b *memBackend) ListDecisions(_ context.Context, runID string) (*run.ListDecisionsResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.runs[runID]; !ok {
		return nil, run.ErrRunNotFound
	}
	var cards []node.DecisionCard
	prefix := runID + "\x00"
	for k, n := range b.nodes {
		if strings.HasPrefix(k, prefix) {
			cards = append(cards, n.DecisionCards...)
		}
	}
	return &run.ListDecisionsResp{RunID: runID, Decisions: cards}, nil
}

func (b *memBackend) GetNodeState(_ context.Context, runID, nodeAddr string) (*node.GetNodeStateResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[runID+"\x00"+nodeAddr]
	if !ok {
		return nil, run.ErrNodeNotFound
	}
	cp := *n
	return &cp, nil
}

func (b *memBackend) EnqueueInterjection(_ context.Context, runID, nodeAddr string, req *node.EnqueueInterjectionReq) (*node.EnqueueInterjectionResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[runID+"\x00"+nodeAddr]
	if !ok {
		return nil, run.ErrNodeNotFound
	}
	b.interjectCalls = append(b.interjectCalls, *req)
	rec := node.InterjectionRecord{
		ID:                  "inj-1",
		Message:             req.Message,
		WaitForConfirmation: req.WaitForConfirmation,
		Status:              node.InterjectionStatusPending,
		CreatedAt:           time.Now().UTC(),
	}
	n.InterjectionLog = append(n.InterjectionLog, rec)
	n.PendingInterjections = append(n.PendingInterjections, session.HumanInterjection{
		ID:                  rec.ID,
		Message:             rec.Message,
		WaitForConfirmation: rec.WaitForConfirmation,
	})
	return &node.EnqueueInterjectionResp{
		ID:                  rec.ID,
		WaitForConfirmation: rec.WaitForConfirmation,
		Status:              rec.Status,
		QueuedCount:         len(n.PendingInterjections),
	}, nil
}

func (b *memBackend) SubmitDecision(_ context.Context, runID, nodeAddr string, req *node.SubmitDecisionReq) (*node.SubmitDecisionResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.submitCalls = append(b.submitCalls, *req)
	if err, ok := b.invalidDecision[req.CardID]; ok {
		return nil, err
	}
	n, ok := b.nodes[runID+"\x00"+nodeAddr]
	if !ok {
		return nil, fmt.Errorf("%w: card %q not found", node.ErrInvalidDecision, req.CardID)
	}
	for i := range n.DecisionCards {
		if n.DecisionCards[i].CardID == req.CardID {
			if n.DecisionCards[i].Status != node.DecisionStatusPending {
				return nil, fmt.Errorf("%w: decision card %q already %s", node.ErrInvalidDecision, req.CardID, n.DecisionCards[i].Status)
			}
			now := time.Now().UTC()
			n.DecisionCards[i].Status = node.DecisionStatusAnswered
			n.DecisionCards[i].SelectedOption = req.SelectedOption
			n.DecisionCards[i].AnswerText = req.AnswerText
			n.DecisionCards[i].Approved = req.Approved
			n.DecisionCards[i].AnsweredAt = &now
			autoResumed := !req.RunPaused && !n.ManualPaused
			if autoResumed {
				n.CurrentEpoch++
				n.Status = node.StatusQueuedSlot
			}
			cardCopy := n.DecisionCards[i]
			return &node.SubmitDecisionResp{
				Accepted:    true,
				Card:        &cardCopy,
				Epoch:       n.CurrentEpoch,
				Status:      n.Status,
				AutoResumed: autoResumed,
			}, nil
		}
	}
	return nil, fmt.Errorf("%w: decision card %q not found", node.ErrInvalidDecision, req.CardID)
}

func (b *memBackend) PreflightCheck(_ context.Context, req *config.PreflightCheckReq) (*config.PreflightCheckResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var missing []string
	ns := config.NormalizeNamespace(req.Namespace)
	for _, k := range req.RequiredKeys {
		if req.RunOverrides != nil {
			if _, ok := req.RunOverrides[k]; ok {
				continue
			}
		}
		if req.RunID != "" {
			if _, ok := b.envs[cfgKey(config.ScopeRun, "", req.RunID, k)]; ok {
				continue
			}
			if _, ok := b.secrets[cfgKey(config.ScopeRun, "", req.RunID, k)]; ok {
				continue
			}
		}
		if _, ok := b.envs[cfgKey(config.ScopeNamespace, ns, "", k)]; ok {
			continue
		}
		if _, ok := b.secrets[cfgKey(config.ScopeNamespace, ns, "", k)]; ok {
			continue
		}
		if _, ok := b.envs[cfgKey(config.ScopeGlobal, "", "", k)]; ok {
			continue
		}
		if _, ok := b.secrets[cfgKey(config.ScopeGlobal, "", "", k)]; ok {
			continue
		}
		missing = append(missing, k)
	}
	return &config.PreflightCheckResp{OK: len(missing) == 0, MissingKeys: missing}, nil
}

func (b *memBackend) UpsertConfig(_ context.Context, req *config.UpsertEntriesReq) (*config.UpsertEntriesResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ns := req.Namespace
	if req.Scope == config.ScopeNamespace {
		ns = config.NormalizeNamespace(ns)
	} else {
		ns = ""
	}
	var updated []string
	now := time.Now().UTC()
	for _, e := range req.Entries {
		if err := config.ValidateKeyName(e.Name); err != nil {
			return nil, err
		}
		k := cfgKey(req.Scope, ns, req.RunID, e.Name)
		if e.IsSecret {
			if _, exists := b.envs[k]; exists {
				return nil, fmt.Errorf("%w: %s already exists as env", config.ErrKindConflict, e.Name)
			}
			b.secrets[k] = secretRecord{
				meta:  config.SecretMeta{Name: e.Name, Scope: req.Scope, Namespace: ns, RunID: req.RunID, UpdatedAt: now},
				value: e.Value,
			}
		} else {
			if _, exists := b.secrets[k]; exists {
				return nil, fmt.Errorf("%w: %s already exists as secret", config.ErrKindConflict, e.Name)
			}
			b.envs[k] = config.EnvEntry{Name: e.Name, Value: e.Value, Scope: req.Scope, Namespace: ns, RunID: req.RunID, UpdatedAt: now}
		}
		updated = append(updated, e.Name)
	}
	return &config.UpsertEntriesResp{Updated: updated}, nil
}

func (b *memBackend) DeleteConfig(_ context.Context, req *config.DeleteEntryReq) (*config.DeleteEntryResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ns := req.Namespace
	if req.Scope == config.ScopeNamespace {
		ns = config.NormalizeNamespace(ns)
	} else {
		ns = ""
	}
	k := cfgKey(req.Scope, ns, req.RunID, req.Name)
	if _, ok := b.envs[k]; ok {
		delete(b.envs, k)
		return &config.DeleteEntryResp{Deleted: true}, nil
	}
	if _, ok := b.secrets[k]; ok {
		delete(b.secrets, k)
		return &config.DeleteEntryResp{Deleted: true}, nil
	}
	return &config.DeleteEntryResp{Deleted: false}, nil
}

func (b *memBackend) ListConfig(_ context.Context, req *config.ListMetadataReq) (*config.ListMetadataResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ns := req.Namespace
	if req.Scope == config.ScopeNamespace {
		ns = config.NormalizeNamespace(ns)
	} else {
		ns = ""
	}
	prefix := string(req.Scope) + "|" + config.NormalizeNamespace(ns) + "|" + req.RunID + "|"
	var envs []config.EnvEntry
	var secrets []config.SecretMeta
	for k, e := range b.envs {
		if strings.HasPrefix(k, prefix) {
			envs = append(envs, e)
		}
	}
	for k, s := range b.secrets {
		if strings.HasPrefix(k, prefix) {
			secrets = append(secrets, s.meta)
		}
	}
	return &config.ListMetadataResp{Scope: req.Scope, Namespace: ns, RunID: req.RunID, Envs: envs, Secrets: secrets}, nil
}

func (b *memBackend) DeleteRunScope(_ context.Context, runID string) (*config.DeleteRunScopeResp, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for k, e := range b.envs {
		if e.Scope == config.ScopeRun && e.RunID == runID {
			delete(b.envs, k)
			count++
		}
	}
	for k, s := range b.secrets {
		if s.meta.Scope == config.ScopeRun && s.meta.RunID == runID {
			delete(b.secrets, k)
			count++
		}
	}
	return &config.DeleteRunScopeResp{DeletedCount: count}, nil
}

func buildTestSofaplanBytes(t *testing.T, hclBody string) []byte {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()
	cfgDir := filepath.Join(tmpDir, "cfg")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "main.sofa.hcl"), []byte(hclBody), 0o600))

	loadedCtx, diags, err := appcfg.LoadDirectoryContext(ctx, cfgDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:    ctx,
		Directory:  cfgDir,
		BundlePath: "config",
	})
	require.NoError(t, cfg.Init(loadedCtx.Blocks))
	require.NoError(t, cfg.RunPlan())
	planned, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	archivePath := filepath.Join(tmpDir, "plan.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, planned, plan.ArchiveSource{ConfigDir: cfgDir}))
	raw, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	return raw
}

func newTestServer(t *testing.T, modify func(*server.Config)) (*server.Server, *memBackend, blob.Store) {
	t.Helper()
	backend := newMemBackend()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	cfg := server.Config{
		PublicAddr: "127.0.0.1:0",
		APIToken:   "test-secret-token-123",
		Backend:    backend,
		BlobStore:  store,
	}
	if modify != nil {
		modify(&cfg)
	}
	srv, err := server.New(cfg)
	require.NoError(t, err)
	return srv, backend, store
}

func TestSecurity_ListenAddressRefusal(t *testing.T) {
	backend := newMemBackend()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	// Non-loopback without APIToken must fail even when DevMode is true.
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080", "192.168.1.20:8080", "localhost:8080"} {
		_, err := server.New(server.Config{
			PublicAddr: addr,
			DevMode:    true,
			APIToken:   "",
			Backend:    backend,
			BlobStore:  store,
		})
		require.Error(t, err, "expected New to refuse non-loopback address %q without APIToken", addr)
	}

	// Loopback without APIToken must fail when DevMode is false.
	_, err = server.New(server.Config{
		PublicAddr: "127.0.0.1:8080",
		DevMode:    false,
		APIToken:   "",
		Backend:    backend,
		BlobStore:  store,
	})
	require.Error(t, err)

	// Loopback IPv4 & IPv6 with DevMode=true and empty APIToken must succeed.
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		srv, err := server.New(server.Config{
			PublicAddr: addr,
			DevMode:    true,
			APIToken:   "",
			Backend:    backend,
			BlobStore:  store,
		})
		require.NoError(t, err, "expected loopback %q in DevMode to succeed", addr)
		_ = srv
	}
}

func TestSecurity_DevModeOriginAntiCSRFAndDualPortIsolation(t *testing.T) {
	srv, backend, _ := newTestServer(t, func(c *server.Config) {
		c.DevMode = true
		c.APIToken = ""
		c.InternalAddr = "127.0.0.1:0"
		c.AppAPIToken = "internal-dapr-secret"
	})
	backend.runs["run-1"] = &run.GetRunStateResp{RunID: "run-1", Namespace: "default", Status: run.RunStatusWaitingHuman}
	backend.runSummaries["run-1"] = run.RunSummary{RunID: "run-1", Namespace: "default", Status: run.RunStatusWaitingHuman}
	backend.nodes["run-1\x00worker.a"] = &node.GetNodeStateResp{
		RunID:    "run-1",
		NodeAddr: "worker.a",
		DecisionCards: []node.DecisionCard{{
			CardID: "dec-1", RunID: "run-1", NodeAddr: "worker.a", Kind: node.DecisionKindPauseForHuman, Status: node.DecisionStatusPending,
		}},
	}

	require.NoError(t, srv.Start(context.Background()))
	defer func() { _ = srv.Stop(context.Background()) }()
	publicBase := "http://" + srv.PublicAddr()
	internalBase := "http://" + srv.InternalAddr()

	// 1. Public port MUST NOT expose /actors/* or /dapr/* routes.
	for _, forbiddenPath := range []string{
		"/actors/ConfigActor/global/method/ResolveForNode",
		"/dapr/subscribe",
		"/dapr/config",
	} {
		resp, err := http.Get(publicBase + forbiddenPath)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "public path %s must return 404", forbiddenPath)
	}

	// 2. Even in --dev tokenless mode, Origin: null and cross-site Origin MUST return 403 on mutations and /api/v1/ws.
	for _, badOrigin := range []string{"null", "https://evil.example", "http://127.0.0.1:9999"} {
		req, err := http.NewRequest(http.MethodPut, publicBase+"/api/v1/secrets", strings.NewReader(`{"name":"FOO","value":"bar"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", badOrigin)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "PUT /api/v1/secrets with Origin %q must be 403", badOrigin)

		req2, err := http.NewRequest(http.MethodPost, publicBase+"/api/v1/decisions/dec-1/respond", strings.NewReader(`{"answer_text":"ok"}`))
		require.NoError(t, err)
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Origin", badOrigin)
		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		_ = resp2.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp2.StatusCode, "POST /decisions/respond with Origin %q must be 403", badOrigin)

		wsReq, err := http.NewRequest(http.MethodGet, publicBase+"/api/v1/ws", nil)
		require.NoError(t, err)
		wsReq.Header.Set("Origin", badOrigin)
		wsResp, err := http.DefaultClient.Do(wsReq)
		require.NoError(t, err)
		_ = wsResp.Body.Close()
		assert.Equal(t, http.StatusForbidden, wsResp.StatusCode, "GET /api/v1/ws with Origin %q must be 403", badOrigin)
	}

	// 3. Matching same-origin request succeeds in --dev mode.
	reqOK, err := http.NewRequest(http.MethodPut, publicBase+"/api/v1/secrets", strings.NewReader(`{"name":"GOOD_KEY","value":"v"}`))
	require.NoError(t, err)
	reqOK.Header.Set("Content-Type", "application/json")
	reqOK.Header.Set("Origin", publicBase)
	respOK, err := http.DefaultClient.Do(reqOK)
	require.NoError(t, err)
	_ = respOK.Body.Close()
	assert.Equal(t, http.StatusOK, respOK.StatusCode)

	// 4. Internal callback listener enforces APP_API_TOKEN.
	intUnauth, err := http.Get(internalBase + "/dapr/subscribe")
	require.NoError(t, err)
	_ = intUnauth.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, intUnauth.StatusCode)

	intAuthReq, _ := http.NewRequest(http.MethodGet, internalBase+"/dapr/subscribe", nil)
	intAuthReq.Header.Set(common.APITokenKey, "internal-dapr-secret")
	intAuthResp, err := http.DefaultClient.Do(intAuthReq)
	require.NoError(t, err)
	_ = intAuthResp.Body.Close()
	assert.Equal(t, http.StatusOK, intAuthResp.StatusCode)
}

func TestSecurity_BearerAndWSSubprotocolAuthAndPreviewTicket(t *testing.T) {
	srv, backend, store := newTestServer(t, func(c *server.Config) {
		c.PreviewTicketTTL = 200 * time.Millisecond
	})
	ctx := context.Background()
	_, err := store.Put(ctx, "runs/run-art/artifacts/art-1.raw", strings.NewReader("<h1>hello</h1>"), "")
	require.NoError(t, err)
	backend.runs["run-art"] = &run.GetRunStateResp{
		RunID: "run-art",
		Nodes: []run.NodeRunSummary{{
			CanonicalAddress: "worker.ui",
			Artifacts: []artifact.PublishedArtifact{{
				ID:      "art-1",
				Name:    "report",
				Type:    "file",
				RelPath: "report.html",
				BlobKey: "runs/run-art/artifacts/art-1.raw",
			}},
		}},
	}

	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	// Missing Bearer token -> 401
	resp, err := http.Get(ts.URL + "/api/v1/runs")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Valid Bearer token -> 200
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer test-secret-token-123")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Mint preview ticket for (run-art, art-1)
	ticketReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/run-art/artifacts/art-1/preview-ticket", nil)
	ticketReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	ticketResp, err := http.DefaultClient.Do(ticketReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, ticketResp.StatusCode)
	var ticketBody struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.NewDecoder(ticketResp.Body).Decode(&ticketBody))
	_ = ticketResp.Body.Close()
	require.NotEmpty(t, ticketBody.Ticket)

	// Ticket works for matching (run-art, art-1)
	artResp, err := http.Get(ts.URL + "/api/v1/runs/run-art/artifacts/art-1/content?ticket=" + url.QueryEscape(ticketBody.Ticket))
	require.NoError(t, err)
	bodyBytes, _ := io.ReadAll(artResp.Body)
	_ = artResp.Body.Close()
	assert.Equal(t, http.StatusOK, artResp.StatusCode)
	assert.Equal(t, "<h1>hello</h1>", string(bodyBytes))
	assert.Equal(t, expectedCSPHeader, artResp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", artResp.Header.Get("X-Content-Type-Options"))

	// Cross-artifact or cross-run ticket rejected -> 401
	crossResp, err := http.Get(ts.URL + "/api/v1/runs/run-art/artifacts/art-other/content?ticket=" + url.QueryEscape(ticketBody.Ticket))
	require.NoError(t, err)
	_ = crossResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, crossResp.StatusCode)

	// Wait for ticket expiry -> 401
	time.Sleep(260 * time.Millisecond)
	expResp, err := http.Get(ts.URL + "/api/v1/runs/run-art/artifacts/art-1/content?ticket=" + url.QueryEscape(ticketBody.Ticket))
	require.NoError(t, err)
	_ = expResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, expResp.StatusCode)

	// WebSocket auth: query string token rejected, subprotocol accepted and echoed as "sofa.v1"
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	_, queryWSResp, err := websocket.Dial(ctx, wsURL+"?token=test-secret-token-123", nil)
	require.Error(t, err)
	if queryWSResp != nil {
		assert.Equal(t, http.StatusUnauthorized, queryWSResp.StatusCode)
	}

	b64Token := base64.RawURLEncoding.EncodeToString([]byte("test-secret-token-123"))
	conn, wsResp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"sofa.v1", "sofa.bearer." + b64Token},
	})
	require.NoError(t, err)
	assert.Equal(t, "sofa.v1", conn.Subprotocol())
	if wsResp != nil {
		assert.Equal(t, "sofa.v1", wsResp.Header.Get("Sec-WebSocket-Protocol"))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestRuns_Preflight422AndMultipartAndOctetSubmission(t *testing.T) {
	srv, backend, store := newTestServer(t, nil)
	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	planBytes := buildTestSofaplanBytes(t, `
source "repo" {
  url    = "https://example.com/repo.git"
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
worker "step1" {
  based_source  = source.repo.branch
  model         = "gemini-2.5-pro"
  system_prompt = "hi"
}
`)

	// 1. Missing GEMINI_API_KEY -> 422 at second 0 with zero BlobStore or StartRun calls!
	req422, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs?run_id=run-missing", bytes.NewReader(planBytes))
	req422.Header.Set("Authorization", "Bearer test-secret-token-123")
	req422.Header.Set("Content-Type", "application/octet-stream")
	resp422, err := http.DefaultClient.Do(req422)
	require.NoError(t, err)
	defer func() { _ = resp422.Body.Close() }()
	assert.Equal(t, http.StatusUnprocessableEntity, resp422.StatusCode)

	var errBody struct {
		Error       string   `json:"error"`
		MissingKeys []string `json:"missing_keys"`
	}
	require.NoError(t, json.NewDecoder(resp422.Body).Decode(&errBody))
	assert.Equal(t, []string{"GEMINI_API_KEY"}, errBody.MissingKeys)
	assert.Empty(t, backend.startCalls, "StartRun must NOT be called when preflight fails")
	_, statErr := store.Stat(context.Background(), "runs/run-missing/plan/plan.sofaplan")
	assert.ErrorIs(t, statErr, blob.ErrNotFound, "BlobStore must NOT store plan when preflight fails")

	// 2. Multipart submission supplying Run-scoped secret satisfies preflight -> 201 Created!
	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	require.NoError(t, mw.WriteField("namespace", "team-alpha"))
	require.NoError(t, mw.WriteField("run_id", "run-mp-1"))
	require.NoError(t, mw.WriteField("envs", `{"LOG_LEVEL":"debug"}`))
	require.NoError(t, mw.WriteField("secrets", `{"GEMINI_API_KEY":"secret-val"}`))
	fw, err := mw.CreateFormFile("plan", "plan.sofaplan")
	require.NoError(t, err)
	_, err = fw.Write(planBytes)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req201, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs", &mpBuf)
	req201.Header.Set("Authorization", "Bearer test-secret-token-123")
	req201.Header.Set("Content-Type", mw.FormDataContentType())
	resp201, err := http.DefaultClient.Do(req201)
	require.NoError(t, err)
	defer func() { _ = resp201.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp201.StatusCode)

	var created struct {
		RunID  string        `json:"run_id"`
		Status run.RunStatus `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp201.Body).Decode(&created))
	assert.Equal(t, "run-mp-1", created.RunID)
	assert.Equal(t, run.RunStatusRunning, created.Status)
	_, statErr = store.Stat(context.Background(), "runs/run-mp-1/plan/plan.sofaplan")
	require.NoError(t, statErr)
}

func TestRuns_SensitiveOutputMaskingAndLifecycle(t *testing.T) {
	srv, backend, _ := newTestServer(t, nil)

	topSecret, err := run.EncodeWireValue(cty.StringVal("super-secret-value").Mark(golden.SensitiveMark))
	require.NoError(t, err)
	nestedObj, err := run.EncodeWireValue(cty.ObjectVal(map[string]cty.Value{
		"public_url": cty.StringVal("https://app.local"),
		"api_token":  cty.StringVal("tok-999").Mark(golden.SensitiveMark),
		"nested_map": cty.MapVal(map[string]cty.Value{
			"safe":   cty.StringVal("ok"),
			"hidden": cty.StringVal("classified").Mark(golden.SensitiveMark),
		}),
	}))
	require.NoError(t, err)

	backend.runs["run-sens"] = &run.GetRunStateResp{
		RunID:     "run-sens",
		Namespace: "default",
		Status:    run.RunStatusSucceeded,
		Outputs: map[string]run.EvaluatedOutput{
			"token": {
				Name:           "token",
				Sensitive:      true,
				HasMarks:       true,
				TypeJSON:       topSecret.TypeJSON,
				ValueJSON:      topSecret.ValueJSON,
				SensitivePaths: topSecret.SensitivePaths,
			},
			"bundle": {
				Name:           "bundle",
				Sensitive:      false,
				HasMarks:       nestedObj.HasMarks,
				TypeJSON:       nestedObj.TypeJSON,
				ValueJSON:      nestedObj.ValueJSON,
				SensitivePaths: nestedObj.SensitivePaths,
			},
		},
	}

	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	// Default GET masks both top-level sensitive and nested sensitive fields as "<sensitive>"
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-sens", nil)
	req.Header.Set("Authorization", "Bearer test-secret-token-123")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var detail struct {
		Outputs map[string]struct {
			Sensitive bool `json:"sensitive"`
			Value     any  `json:"value"`
		} `json:"outputs"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&detail))
	assert.True(t, detail.Outputs["token"].Sensitive)
	assert.Equal(t, "<sensitive>", detail.Outputs["token"].Value)

	bundleVal, ok := detail.Outputs["bundle"].Value.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://app.local", bundleVal["public_url"])
	assert.Equal(t, "<sensitive>", bundleVal["api_token"])
	nestedMap, ok := bundleVal["nested_map"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ok", nestedMap["safe"])
	assert.Equal(t, "<sensitive>", nestedMap["hidden"])

	// ?reveal_sensitive=true returns plaintext values
	reqRev, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-sens?reveal_sensitive=true", nil)
	reqRev.Header.Set("Authorization", "Bearer test-secret-token-123")
	respRev, err := http.DefaultClient.Do(reqRev)
	require.NoError(t, err)
	defer func() { _ = respRev.Body.Close() }()
	require.Equal(t, http.StatusOK, respRev.StatusCode)

	var revealed struct {
		Outputs map[string]struct {
			Value any `json:"value"`
		} `json:"outputs"`
	}
	require.NoError(t, json.NewDecoder(respRev.Body).Decode(&revealed))
	assert.Equal(t, "super-secret-value", revealed.Outputs["token"].Value)
	revBundle := revealed.Outputs["bundle"].Value.(map[string]any)
	assert.Equal(t, "tok-999", revBundle["api_token"])

	// Pause via query (?mode=immediate) and JSON ({"mode":"planned"}), Resume, and Delete
	pReq1, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/run-sens/pause?mode=immediate", nil)
	pReq1.Header.Set("Authorization", "Bearer test-secret-token-123")
	pResp1, err := http.DefaultClient.Do(pReq1)
	require.NoError(t, err)
	_ = pResp1.Body.Close()
	assert.Equal(t, http.StatusOK, pResp1.StatusCode)

	pReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/run-sens/pause", strings.NewReader(`{"mode":"planned"}`))
	pReq2.Header.Set("Authorization", "Bearer test-secret-token-123")
	pReq2.Header.Set("Content-Type", "application/json")
	pResp2, err := http.DefaultClient.Do(pReq2)
	require.NoError(t, err)
	_ = pResp2.Body.Close()
	assert.Equal(t, http.StatusOK, pResp2.StatusCode)
	assert.Equal(t, []string{"run-sens:immediate", "run-sens:planned"}, backend.pauseCalls)

	rReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/run-sens/resume", nil)
	rReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	rResp, err := http.DefaultClient.Do(rReq)
	require.NoError(t, err)
	_ = rResp.Body.Close()
	assert.Equal(t, http.StatusOK, rResp.StatusCode)

	dReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/runs/run-sens", nil)
	dReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	dResp, err := http.DefaultClient.Do(dReq)
	require.NoError(t, err)
	_ = dResp.Body.Close()
	assert.Equal(t, http.StatusOK, dResp.StatusCode)
	assert.Equal(t, []string{"run-sens"}, backend.deleteCalls)
}

func TestNodes_LogsRerunDiaryDecisionsAndConfigsCRUD(t *testing.T) {
	srv, backend, store := newTestServer(t, nil)
	ctx := context.Background()

	dynAddr := `module.a.dynamic_worker.b[k]`
	logKey := daemon.NodeLogBlobKey("run-n1", dynAddr, 2)
	_, err := store.Put(ctx, logKey, strings.NewReader(`{"kind":"assistant_message","text":"epoch2-log"}`+"\n"), "")
	require.NoError(t, err)

	backend.runs["run-n1"] = &run.GetRunStateResp{
		RunID:        "run-n1",
		Namespace:    "default",
		Status:       run.RunStatusWaitingHuman,
		ManualPaused: false,
		Nodes: []run.NodeRunSummary{{
			Address:          `dynamic_worker.b[k]`,
			CanonicalAddress: dynAddr,
			Kind:             "worker",
			Status:           node.StatusWaitingHuman,
			CurrentEpoch:     2,
		}},
	}
	backend.runSummaries["run-n1"] = run.RunSummary{
		RunID:     "run-n1",
		Namespace: "default",
		Status:    run.RunStatusWaitingHuman,
	}
	backend.nodes["run-n1\x00"+dynAddr] = &node.GetNodeStateResp{
		RunID:        "run-n1",
		NodeAddr:     dynAddr,
		CurrentEpoch: 2,
		Status:       node.StatusWaitingHuman,
		Checkpoints: []checkpoint.CheckpointRecord{{
			Seq:    1,
			Epoch:  2,
			Status: checkpoint.StatusComplete,
			Diary:  session.DiaryInput{Completed: []string{"step A"}},
		}},
		DecisionCards: []node.DecisionCard{{
			CardID:   "card-1",
			RunID:    "run-n1",
			NodeAddr: dynAddr,
			Epoch:    2,
			Kind:     node.DecisionKindRequestDecision,
			Status:   node.DecisionStatusPending,
			Question: "Pick DB?",
			Options:  []string{"postgres", "sqlite"},
		}},
	}
	backend.sharedMerged["run-n1\x00"+dynAddr] = true
	backend.invalidDecision["card-stale"] = fmt.Errorf("%w: commit_sha mismatch", node.ErrInvalidDecision)

	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	encodedAddr := url.PathEscape(dynAddr)

	// 1. URL-decoded node detail
	nReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-n1/nodes/"+encodedAddr, nil)
	nReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	nResp, err := http.DefaultClient.Do(nReq)
	require.NoError(t, err)
	defer func() { _ = nResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, nResp.StatusCode)
	var nodeDetail server.NodeDetailResponse
	require.NoError(t, json.NewDecoder(nResp.Body).Decode(&nodeDetail))
	assert.Equal(t, dynAddr, nodeDetail.NodeAddr)
	assert.Len(t, nodeDetail.Checkpoints, 1)

	// 2. JSONL logs default to current epoch (2)
	lReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-n1/nodes/"+encodedAddr+"/logs", nil)
	lReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	lResp, err := http.DefaultClient.Do(lReq)
	require.NoError(t, err)
	lBody, _ := io.ReadAll(lResp.Body)
	_ = lResp.Body.Close()
	assert.Equal(t, http.StatusOK, lResp.StatusCode)
	assert.Contains(t, string(lBody), "epoch2-log")

	// 3. Rerun on shared merged branch -> 409 Conflict
	rrReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/run-n1/nodes/"+encodedAddr+"/rerun", strings.NewReader(`{"reason":"retry"}`))
	rrReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	rrResp, err := http.DefaultClient.Do(rrReq)
	require.NoError(t, err)
	_ = rrResp.Body.Close()
	assert.Equal(t, http.StatusConflict, rrResp.StatusCode)

	// 4. Diary interjection
	dmReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/run-n1/nodes/"+encodedAddr+"/diary-messages", strings.NewReader(`{"message":"focus on tests","wait_for_confirmation":true}`))
	dmReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	dmResp, err := http.DefaultClient.Do(dmReq)
	require.NoError(t, err)
	_ = dmResp.Body.Close()
	assert.Equal(t, http.StatusOK, dmResp.StatusCode)
	require.Len(t, backend.interjectCalls, 1)
	assert.True(t, backend.interjectCalls[0].WaitForConfirmation)

	// 5. Decisions list & respond (and 409 on version mismatch)
	decListReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/decisions?status=PENDING", nil)
	decListReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	decListResp, err := http.DefaultClient.Do(decListReq)
	require.NoError(t, err)
	var decList struct {
		Decisions []node.DecisionCard `json:"decisions"`
	}
	require.NoError(t, json.NewDecoder(decListResp.Body).Decode(&decList))
	_ = decListResp.Body.Close()
	assert.Equal(t, http.StatusOK, decListResp.StatusCode)
	require.Len(t, decList.Decisions, 1)

	respDecReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/decisions/card-1/respond", strings.NewReader(`{"selected_option":"postgres"}`))
	respDecReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	respDec, err := http.DefaultClient.Do(respDecReq)
	require.NoError(t, err)
	_ = respDec.Body.Close()
	assert.Equal(t, http.StatusOK, respDec.StatusCode)

	staleReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/decisions/card-stale/respond", strings.NewReader(`{"run_id":"run-n1","node_addr":"`+dynAddr+`","approved":true}`))
	staleReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	staleResp, err := http.DefaultClient.Do(staleReq)
	require.NoError(t, err)
	_ = staleResp.Body.Close()
	assert.Equal(t, http.StatusConflict, staleResp.StatusCode)

	// 6. Envs & Write-Only Secrets CRUD + 409 on Env/Secret conflict
	putEnv, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/envs", strings.NewReader(`{"scope":"global","name":"SHARED_KEY","value":"plain"}`))
	putEnv.Header.Set("Authorization", "Bearer test-secret-token-123")
	putEnvResp, err := http.DefaultClient.Do(putEnv)
	require.NoError(t, err)
	_ = putEnvResp.Body.Close()
	assert.Equal(t, http.StatusOK, putEnvResp.StatusCode)

	putSecConflict, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/secrets", strings.NewReader(`{"scope":"global","name":"SHARED_KEY","value":"secret"}`))
	putSecConflict.Header.Set("Authorization", "Bearer test-secret-token-123")
	putSecConflictResp, err := http.DefaultClient.Do(putSecConflict)
	require.NoError(t, err)
	_ = putSecConflictResp.Body.Close()
	assert.Equal(t, http.StatusConflict, putSecConflictResp.StatusCode)

	putSecOK, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/secrets", strings.NewReader(`{"scope":"global","name":"API_SECRET","value":"top-secret-plaintext"}`))
	putSecOK.Header.Set("Authorization", "Bearer test-secret-token-123")
	putSecOKResp, err := http.DefaultClient.Do(putSecOK)
	require.NoError(t, err)
	_ = putSecOKResp.Body.Close()
	assert.Equal(t, http.StatusOK, putSecOKResp.StatusCode)

	getSecReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/secrets?scope=global", nil)
	getSecReq.Header.Set("Authorization", "Bearer test-secret-token-123")
	getSecResp, err := http.DefaultClient.Do(getSecReq)
	require.NoError(t, err)
	rawSecrets, _ := io.ReadAll(getSecResp.Body)
	_ = getSecResp.Body.Close()
	assert.Equal(t, http.StatusOK, getSecResp.StatusCode)
	assert.NotContains(t, string(rawSecrets), "top-secret-plaintext")
	assert.NotContains(t, string(rawSecrets), `"value"`)
}

func TestArtifacts_ZipPayloadAndCSPHeaders(t *testing.T) {
	srv, backend, store := newTestServer(t, nil)
	ctx := context.Background()

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	fw, err := zw.Create("payload")
	require.NoError(t, err)
	_, err = fw.Write([]byte("<html><body>ok</body></html>"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	blobKey := "runs/run-zip/artifacts/art-html.zip"
	_, err = store.Put(ctx, blobKey, bytes.NewReader(zipBuf.Bytes()), "")
	require.NoError(t, err)

	backend.runs["run-zip"] = &run.GetRunStateResp{
		RunID: "run-zip",
		Nodes: []run.NodeRunSummary{{
			CanonicalAddress: "worker.ui",
			Artifacts: []artifact.PublishedArtifact{{
				ID:      "art-html",
				Name:    "preview",
				Type:    "file",
				RelPath: "dist/preview.html",
				BlobKey: blobKey,
			}},
		}},
	}

	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-zip/artifacts/art-html/content", nil)
	req.Header.Set("Authorization", "Bearer test-secret-token-123")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "<html><body>ok</body></html>", string(body))
	assert.Equal(t, expectedCSPHeader, resp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
}

func TestWebSocket_GlobalAndRunFilteredPubSubFanOut(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	hdr := http.Header{"Authorization": []string{"Bearer test-secret-token-123"}}

	globalConn, _, err := websocket.Dial(ctx, wsBase, &websocket.DialOptions{HTTPHeader: hdr})
	require.NoError(t, err)
	defer func() { _ = globalConn.Close(websocket.StatusNormalClosure, "") }()

	runAConn, _, err := websocket.Dial(ctx, wsBase+"?run_id=run-a", &websocket.DialOptions{HTTPHeader: hdr})
	require.NoError(t, err)
	defer func() { _ = runAConn.Close(websocket.StatusNormalClosure, "") }()

	// Publish one event for run-b and one for run-a via Hub / PubSub handler.
	require.NoError(t, srv.HandlePubSubEvent(ctx, daemon.EventsTopic, []byte(`{"run_id":"run-b","node_addr":"worker.x","kind":"assistant_message","text":"from-b"}`)))
	require.NoError(t, srv.HandlePubSubEvent(ctx, run.DefaultRunEventsTopic, []byte(`{"run_id":"run-a","status":"RUNNING"}`)))

	readEvent := func(c *websocket.Conn) server.WSEvent {
		_, data, rErr := c.Read(ctx)
		require.NoError(t, rErr)
		var ev server.WSEvent
		require.NoError(t, json.Unmarshal(data, &ev))
		return ev
	}

	g1 := readEvent(globalConn)
	g2 := readEvent(globalConn)
	assert.Equal(t, "run-b", g1.RunID)
	assert.Equal(t, "run-a", g2.RunID)

	a1 := readEvent(runAConn)
	assert.Equal(t, "run-a", a1.RunID)

	// Ensure runAConn received nothing from run-b.
	shortCtx, shortCancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer shortCancel()
	_, _, extraErr := runAConn.Read(shortCtx)
	assert.True(t, errors.Is(extraErr, context.DeadlineExceeded))
}

func TestEdgeCases_DirArtifactUIBlockedPathsAndConfigDeletes(t *testing.T) {
	srv, backend, store := newTestServer(t, func(c *server.Config) {
		c.UIHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("spa-fallback"))
		})
	})

	// UIHandler must NOT shadow /api/*, /actors/*, or /dapr/* -> still 404!
	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	for _, blocked := range []string{"/actors/RunActor/r1/method/GetRunState", "/dapr/subscribe", "/api/v1/unknown", "/api/v2/foo", "/api"} {
		resp, err := http.Get(ts.URL + blocked)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "blocked path %s must return 404", blocked)
	}

	// SPA fallback works for normal UI routes.
	uiResp, err := http.Get(ts.URL + "/runs/r1")
	require.NoError(t, err)
	uiBody, _ := io.ReadAll(uiResp.Body)
	_ = uiResp.Body.Close()
	assert.Equal(t, http.StatusOK, uiResp.StatusCode)
	assert.Equal(t, "spa-fallback", string(uiBody))

	// Directory artifact zip with tree/index.html & tree/app.js
	ctx := context.Background()
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	for name, content := range map[string]string{
		"tree/index.html": "<html>index</html>",
		"tree/app.js":     "console.log(1);",
	} {
		fw, cErr := zw.Create(name)
		require.NoError(t, cErr)
		_, cErr = fw.Write([]byte(content))
		require.NoError(t, cErr)
	}
	require.NoError(t, zw.Close())
	_, err = store.Put(ctx, "runs/run-dir/artifacts/art-dir.zip", bytes.NewReader(zipBuf.Bytes()), "")
	require.NoError(t, err)
	backend.runs["run-dir"] = &run.GetRunStateResp{
		RunID: "run-dir",
		Nodes: []run.NodeRunSummary{{
			CanonicalAddress: "worker.ui",
			Artifacts: []artifact.PublishedArtifact{{
				ID:      "art-dir",
				Name:    "site",
				Type:    "directory",
				RelPath: "dist",
				BlobKey: "runs/run-dir/artifacts/art-dir.zip",
			}},
		}},
	}

	reqIdx, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-dir/artifacts/art-dir/content", nil)
	reqIdx.Header.Set("Authorization", "Bearer test-secret-token-123")
	respIdx, err := http.DefaultClient.Do(reqIdx)
	require.NoError(t, err)
	idxBody, _ := io.ReadAll(respIdx.Body)
	_ = respIdx.Body.Close()
	assert.Equal(t, http.StatusOK, respIdx.StatusCode)
	assert.Equal(t, "<html>index</html>", string(idxBody))
	assert.Equal(t, expectedCSPHeader, respIdx.Header.Get("Content-Security-Policy"))

	reqJS, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run-dir/artifacts/art-dir/content?entry=app.js", nil)
	reqJS.Header.Set("Authorization", "Bearer test-secret-token-123")
	respJS, err := http.DefaultClient.Do(reqJS)
	require.NoError(t, err)
	jsBody, _ := io.ReadAll(respJS.Body)
	_ = respJS.Body.Close()
	assert.Equal(t, http.StatusOK, respJS.StatusCode)
	assert.Equal(t, "console.log(1);", string(jsBody))

	// Env/Secret delete cross-kind safety & deletion
	putEnv, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/envs", strings.NewReader(`{"scope":"namespace","namespace":"ns1","name":"ENV_A","value":"1"}`))
	putEnv.Header.Set("Authorization", "Bearer test-secret-token-123")
	r1, err := http.DefaultClient.Do(putEnv)
	require.NoError(t, err)
	_ = r1.Body.Close()
	assert.Equal(t, http.StatusOK, r1.StatusCode)

	delWrongKind, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/secrets?scope=namespace&namespace=ns1&name=ENV_A", nil)
	delWrongKind.Header.Set("Authorization", "Bearer test-secret-token-123")
	r2, err := http.DefaultClient.Do(delWrongKind)
	require.NoError(t, err)
	_ = r2.Body.Close()
	assert.Equal(t, http.StatusConflict, r2.StatusCode)

	delRight, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/envs?scope=namespace&namespace=ns1&name=ENV_A", nil)
	delRight.Header.Set("Authorization", "Bearer test-secret-token-123")
	r3, err := http.DefaultClient.Do(delRight)
	require.NoError(t, err)
	_ = r3.Body.Close()
	assert.Equal(t, http.StatusOK, r3.StatusCode)
}
