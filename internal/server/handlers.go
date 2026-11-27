package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	daprclient "github.com/dapr/go-sdk/client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/applyeval"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const (
	defaultMaxUploadBytes int64 = 256 << 20
	sensitiveMaskLiteral        = "<sensitive>"
)

// Backend defines the Control Plane Actor boundary used by HTTP handlers.
type Backend interface {
	ListRuns(ctx context.Context, req *run.ListRunsReq) (*run.ListRunsResp, error)
	StartRun(ctx context.Context, req *run.StartRunReq) (*run.StartRunResp, error)
	GetRunState(ctx context.Context, runID string) (*run.GetRunStateResp, error)
	PauseRun(ctx context.Context, runID string, req *run.PauseRunReq) (*run.PauseRunResp, error)
	ResumeRun(ctx context.Context, runID string) (*run.ResumeRunResp, error)
	DeleteRun(ctx context.Context, runID string) (*run.DeleteRunResp, error)
	RerunNode(ctx context.Context, runID string, req *run.RerunNodeReq) (*run.RerunNodeResp, error)
	ListDecisions(ctx context.Context, runID string) (*run.ListDecisionsResp, error)
	GetNodeState(ctx context.Context, runID, nodeAddr string) (*node.GetNodeStateResp, error)
	EnqueueInterjection(ctx context.Context, runID, nodeAddr string, req *node.EnqueueInterjectionReq) (*node.EnqueueInterjectionResp, error)
	SubmitDecision(ctx context.Context, runID, nodeAddr string, req *node.SubmitDecisionReq) (*node.SubmitDecisionResp, error)
	PreflightCheck(ctx context.Context, req *config.PreflightCheckReq) (*config.PreflightCheckResp, error)
	UpsertConfig(ctx context.Context, req *config.UpsertEntriesReq) (*config.UpsertEntriesResp, error)
	DeleteConfig(ctx context.Context, req *config.DeleteEntryReq) (*config.DeleteEntryResp, error)
	ListConfig(ctx context.Context, req *config.ListMetadataReq) (*config.ListMetadataResp, error)
	DeleteRunScope(ctx context.Context, runID string) (*config.DeleteRunScopeResp, error)
}

type daprBackend struct {
	clientProvider func() daprclient.Client
}

// NewDaprBackend constructs a Backend wired to Dapr Control Plane Actors via clientProvider.
func NewDaprBackend(clientProvider func() daprclient.Client) Backend {
	return &daprBackend{clientProvider: clientProvider}
}

func (b *daprBackend) client() (daprclient.Client, error) {
	if b.clientProvider == nil {
		return nil, errors.New("server: dapr client provider is nil")
	}
	c := b.clientProvider()
	if c == nil {
		return nil, errors.New("server: dapr client unavailable")
	}
	return c, nil
}

func (b *daprBackend) ListRuns(ctx context.Context, req *run.ListRunsReq) (*run.ListRunsResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	if req == nil {
		req = &run.ListRunsReq{}
	}
	return run.NewOperatorClientStub(c).ListRuns(ctx, req)
}

func (b *daprBackend) StartRun(ctx context.Context, req *run.StartRunReq) (*run.StartRunResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(req.RunID, c)
	if err != nil {
		return nil, err
	}
	return rc.StartRun(ctx, req)
}

func (b *daprBackend) GetRunState(ctx context.Context, runID string) (*run.GetRunStateResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(runID, c)
	if err != nil {
		return nil, err
	}
	return rc.GetRunState(ctx, &run.GetRunStateReq{})
}

func (b *daprBackend) PauseRun(ctx context.Context, runID string, req *run.PauseRunReq) (*run.PauseRunResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(runID, c)
	if err != nil {
		return nil, err
	}
	return rc.PauseRun(ctx, req)
}

func (b *daprBackend) ResumeRun(ctx context.Context, runID string) (*run.ResumeRunResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(runID, c)
	if err != nil {
		return nil, err
	}
	return rc.ResumeRun(ctx, &run.ResumeRunReq{})
}

func (b *daprBackend) DeleteRun(ctx context.Context, runID string) (*run.DeleteRunResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(runID, c)
	if err != nil {
		return nil, err
	}
	return rc.DeleteRun(ctx, &run.DeleteRunReq{})
}

func (b *daprBackend) RerunNode(ctx context.Context, runID string, req *run.RerunNodeReq) (*run.RerunNodeResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(runID, c)
	if err != nil {
		return nil, err
	}
	return rc.RerunNode(ctx, req)
}

func (b *daprBackend) ListDecisions(ctx context.Context, runID string) (*run.ListDecisionsResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	rc, err := run.NewDaprRunClient(runID, c)
	if err != nil {
		return nil, err
	}
	return rc.ListDecisions(ctx, &run.ListDecisionsReq{})
}

func (b *daprBackend) GetNodeState(ctx context.Context, runID, nodeAddr string) (*node.GetNodeStateResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	stub, err := node.NewClientStubForNode(runID, nodeAddr, c)
	if err != nil {
		return nil, err
	}
	return stub.GetState(ctx, &node.GetNodeStateReq{})
}

func (b *daprBackend) EnqueueInterjection(ctx context.Context, runID, nodeAddr string, req *node.EnqueueInterjectionReq) (*node.EnqueueInterjectionResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	stub, err := node.NewClientStubForNode(runID, nodeAddr, c)
	if err != nil {
		return nil, err
	}
	return stub.EnqueueInterjection(ctx, req)
}

func (b *daprBackend) SubmitDecision(ctx context.Context, runID, nodeAddr string, req *node.SubmitDecisionReq) (*node.SubmitDecisionResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	stub, err := node.NewClientStubForNode(runID, nodeAddr, c)
	if err != nil {
		return nil, err
	}
	resp, err := stub.SubmitDecision(ctx, req)
	if err != nil {
		return resp, translateNodeError(err)
	}
	return resp, nil
}

func (b *daprBackend) PreflightCheck(ctx context.Context, req *config.PreflightCheckReq) (*config.PreflightCheckResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	resp, err := config.NewClientStub(c).PreflightCheck(ctx, req)
	if err != nil {
		return resp, translateConfigError(err)
	}
	return resp, nil
}

func (b *daprBackend) UpsertConfig(ctx context.Context, req *config.UpsertEntriesReq) (*config.UpsertEntriesResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	resp, err := config.NewClientStub(c).UpsertEntries(ctx, req)
	if err != nil {
		return resp, translateConfigError(err)
	}
	return resp, nil
}

func (b *daprBackend) DeleteConfig(ctx context.Context, req *config.DeleteEntryReq) (*config.DeleteEntryResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	resp, err := config.NewClientStub(c).DeleteEntry(ctx, req)
	if err != nil {
		return resp, translateConfigError(err)
	}
	return resp, nil
}

func (b *daprBackend) ListConfig(ctx context.Context, req *config.ListMetadataReq) (*config.ListMetadataResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	resp, err := config.NewClientStub(c).ListMetadata(ctx, req)
	if err != nil {
		return resp, translateConfigError(err)
	}
	return resp, nil
}

func (b *daprBackend) DeleteRunScope(ctx context.Context, runID string) (*config.DeleteRunScopeResp, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	resp, err := config.NewClientStub(c).DeleteRunScope(ctx, &config.DeleteRunScopeReq{RunID: runID})
	if err != nil {
		return resp, translateConfigError(err)
	}
	return resp, nil
}

func translateConfigError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case errors.Is(err, config.ErrKindConflict) || strings.Contains(msg, config.ErrKindConflict.Error()):
		return fmt.Errorf("%w: %s", config.ErrKindConflict, msg)
	case errors.Is(err, config.ErrInvalidKeyName) || strings.Contains(msg, config.ErrInvalidKeyName.Error()):
		return fmt.Errorf("%w: %s", config.ErrInvalidKeyName, msg)
	case errors.Is(err, config.ErrInvalidScope) || strings.Contains(msg, config.ErrInvalidScope.Error()):
		return fmt.Errorf("%w: %s", config.ErrInvalidScope, msg)
	default:
		return err
	}
}

func translateNodeError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if errors.Is(err, node.ErrInvalidDecision) || strings.Contains(msg, node.ErrInvalidDecision.Error()) {
		return fmt.Errorf("%w: %s", node.ErrInvalidDecision, msg)
	}
	return err
}

// RunOutputView is the REST JSON representation of one Run output value.
type RunOutputView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Sensitive   bool   `json:"sensitive"`
	Unknown     bool   `json:"unknown,omitempty"`
	Value       any    `json:"value,omitempty"`
}

// RunDetailResponse is the REST JSON representation of GET /api/v1/runs/{run_id}.
type RunDetailResponse struct {
	RunID        string                         `json:"run_id"`
	Namespace    string                         `json:"namespace"`
	Status       run.RunStatus                  `json:"status"`
	ManualPaused bool                           `json:"manual_paused"`
	PlanBlobKey  string                         `json:"plan_blob_key,omitempty"`
	PlanSHA256   string                         `json:"plan_sha256,omitempty"`
	ErrorMessage string                         `json:"error_message,omitempty"`
	Nodes        []run.NodeRunSummary           `json:"nodes,omitempty"`
	Outputs      map[string]RunOutputView       `json:"outputs,omitempty"`
	Sources      []applyeval.SourceRuntimeState `json:"sources,omitempty"`
	Preserves    []git.PreserveRef              `json:"preserves,omitempty"`
	Ledger       git.LedgerSnapshot             `json:"ledger,omitempty"`
	CreatedAt    time.Time                      `json:"created_at"`
	UpdatedAt    time.Time                      `json:"updated_at"`
}

// NodeDetailResponse is the REST JSON representation of GET /api/v1/runs/{run_id}/nodes/{node_id}.
type NodeDetailResponse struct {
	RunID                string                        `json:"run_id"`
	NodeAddr             string                        `json:"node_addr"`
	Summary              *run.NodeRunSummary           `json:"summary,omitempty"`
	CurrentEpoch         uint64                        `json:"current_epoch"`
	Status               node.NodeStatus               `json:"status"`
	ManualPaused         bool                          `json:"manual_paused"`
	WorkerProcessID      string                        `json:"worker_process_id,omitempty"`
	WorkerAppID          string                        `json:"worker_app_id,omitempty"`
	LastError            string                        `json:"last_error,omitempty"`
	Checkpoints          []checkpoint.CheckpointRecord `json:"checkpoints,omitempty"`
	PendingInterjections []session.HumanInterjection   `json:"pending_interjections,omitempty"`
	InterjectionLog      []node.InterjectionRecord     `json:"interjection_log,omitempty"`
	DecisionCards        []node.DecisionCard           `json:"decision_cards,omitempty"`
	Artifacts            []artifact.PublishedArtifact  `json:"artifacts,omitempty"`
	PendingPublication   *runner.PendingPublication    `json:"pending_publication,omitempty"`
	Approval             *runner.ApprovalRecord        `json:"approval,omitempty"`
	Outcome              *runner.NodeOutcome           `json:"outcome,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func decodeRunBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, run.ErrAlreadyMergedSharedBranch):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, run.ErrRunNotFound), errors.Is(err, run.ErrNodeNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, run.ErrRunConflict):
		writeJSONError(w, http.StatusConflict, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Backend == nil || s.cfg.BlobStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "server backend or blob store unavailable")
		return
	}

	tmpDir, err := os.MkdirTemp("", "sofa-upload-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	planPath := filepath.Join(tmpDir, "plan.sofaplan")
	namespace := ""
	runID := ""
	envs := make(map[string]string)
	secrets := make(map[string]string)

	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid multipart payload: %v", err))
			return
		}
		namespace = strings.TrimSpace(r.FormValue("namespace"))
		runID = strings.TrimSpace(r.FormValue("run_id"))
		if rawEnvs := strings.TrimSpace(r.FormValue("envs")); rawEnvs != "" {
			if err := json.Unmarshal([]byte(rawEnvs), &envs); err != nil {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid envs JSON: %v", err))
				return
			}
		}
		if rawSecrets := strings.TrimSpace(r.FormValue("secrets")); rawSecrets != "" {
			if err := json.Unmarshal([]byte(rawSecrets), &secrets); err != nil {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid secrets JSON: %v", err))
				return
			}
		}
		file, _, err := r.FormFile("plan")
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("missing plan file field: %v", err))
			return
		}
		defer func() { _ = file.Close() }()
		if err := copyBoundedToFile(file, planPath, s.cfg.MaxUploadBytes); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
		namespace = strings.TrimSpace(r.URL.Query().Get("namespace"))
		runID = strings.TrimSpace(r.URL.Query().Get("run_id"))
		if err := copyBoundedToFile(r.Body, planPath, s.cfg.MaxUploadBytes); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	namespace = config.NormalizeNamespace(namespace)
	if runID == "" {
		runID = "run-" + uuid.NewString()
	}
	if err := blob.ValidateKey(fmt.Sprintf("runs/%s/plan/plan.sofaplan", runID)); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid run_id %q: %v", runID, err))
		return
	}

	runOverrides := make(map[string]string, len(envs)+len(secrets))
	entries := make([]config.EntryInput, 0, len(envs)+len(secrets))
	for k, v := range envs {
		if err := config.ValidateKeyName(k); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, dup := secrets[k]; dup {
			writeJSONError(w, http.StatusConflict, fmt.Sprintf("key %q cannot be specified in both envs and secrets", k))
			return
		}
		runOverrides[k] = v
		entries = append(entries, config.EntryInput{Name: k, Value: v, IsSecret: false})
	}
	for k, v := range secrets {
		if err := config.ValidateKeyName(k); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		runOverrides[k] = v
		entries = append(entries, config.EntryInput{Name: k, Value: v, IsSecret: true})
	}

	loaded, err := plan.LoadArchive(planPath, filepath.Join(tmpDir, "extract"), plan.LoadArchiveOptions{})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid .sofaplan archive: %v", err))
		return
	}

	reqKeys := config.ExtractPlanEnvRefs(loaded.Plan)
	pf, err := s.cfg.Backend.PreflightCheck(r.Context(), &config.PreflightCheckReq{
		Namespace:    namespace,
		RunID:        runID,
		RunOverrides: runOverrides,
		RequiredKeys: reqKeys,
	})
	if err != nil {
		if errors.Is(err, config.ErrInvalidKeyName) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pf != nil && (!pf.OK || len(pf.MissingKeys) > 0) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":        "missing required environment variables or secrets",
			"missing_keys": pf.MissingKeys,
		})
		return
	}

	if len(entries) > 0 {
		if _, err := s.cfg.Backend.UpsertConfig(r.Context(), &config.UpsertEntriesReq{
			Scope:   config.ScopeRun,
			RunID:   runID,
			Entries: entries,
		}); err != nil {
			if errors.Is(err, config.ErrKindConflict) {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	planSHA, err := sha256HexFile(planPath)
	if err != nil {
		if len(entries) > 0 {
			_, _ = s.cfg.Backend.DeleteRunScope(r.Context(), runID)
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.Open(planPath)
	if err != nil {
		if len(entries) > 0 {
			_, _ = s.cfg.Backend.DeleteRunScope(r.Context(), runID)
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	planKey := fmt.Sprintf("runs/%s/plan/plan.sofaplan", runID)
	meta, putErr := s.cfg.BlobStore.Put(r.Context(), planKey, f, planSHA)
	_ = f.Close()
	if putErr != nil {
		if len(entries) > 0 {
			_, _ = s.cfg.Backend.DeleteRunScope(r.Context(), runID)
		}
		writeJSONError(w, http.StatusInternalServerError, putErr.Error())
		return
	}

	startResp, err := s.cfg.Backend.StartRun(r.Context(), &run.StartRunReq{
		RunID:                 runID,
		Namespace:             namespace,
		PlanBlobKey:           meta.Key,
		PlanSHA256:            meta.SHA256,
		RequiredArch:          s.cfg.RunDefaults.RequiredArch,
		TargetPlatform:        s.cfg.RunDefaults.TargetPlatform,
		MaxGatekeeperReworks:  s.cfg.RunDefaults.MaxGatekeeperReworks,
		MaxArtifactReworks:    s.cfg.RunDefaults.MaxArtifactReworks,
		StallTimeout:          s.cfg.RunDefaults.StallTimeout,
		WatchdogCheckInterval: s.cfg.RunDefaults.WatchdogCheckInterval,
	})
	if err != nil || startResp == nil || !startResp.Accepted {
		_ = s.cfg.BlobStore.Delete(r.Context(), meta.Key)
		if len(entries) > 0 {
			_, _ = s.cfg.Backend.DeleteRunScope(r.Context(), runID)
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		msg := "failed to start run"
		if startResp != nil && startResp.ErrorMessage != "" {
			msg = startResp.ErrorMessage
		}
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"run_id":    startResp.RunID,
		"namespace": namespace,
		"status":    startResp.Status,
	})
}

func copyBoundedToFile(src io.Reader, dstPath string, maxBytes int64) error {
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(src, maxBytes+1))
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("plan payload is empty")
	}
	if n > maxBytes {
		return fmt.Errorf("plan payload exceeds maximum size %d bytes", maxBytes)
	}
	return nil
}

func sha256HexFile(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
	status := run.RunStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	resp, err := s.cfg.Backend.ListRuns(r.Context(), &run.ListRunsReq{
		Namespace: ns,
		Status:    status,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runs := []run.RunSummary{}
	if resp != nil && resp.Runs != nil {
		runs = resp.Runs
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	st, err := s.cfg.Backend.GetRunState(r.Context(), runID)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	reveal := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("reveal_sensitive")), "true")
	outputs, err := formatRunOutputs(st.Outputs, reveal)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, RunDetailResponse{
		RunID:        st.RunID,
		Namespace:    st.Namespace,
		Status:       st.Status,
		ManualPaused: st.ManualPaused,
		PlanBlobKey:  st.PlanBlobKey,
		PlanSHA256:   st.PlanSHA256,
		ErrorMessage: st.ErrorMessage,
		Nodes:        st.Nodes,
		Outputs:      outputs,
		Sources:      st.Sources,
		Preserves:    st.Preserves,
		Ledger:       st.Ledger,
		CreatedAt:    st.CreatedAt,
		UpdatedAt:    st.UpdatedAt,
	})
}

func formatRunOutputs(raw map[string]run.EvaluatedOutput, reveal bool) (map[string]RunOutputView, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]RunOutputView, len(raw))
	for name, eo := range raw {
		val, err := eo.DecodeValue()
		if err != nil {
			return nil, fmt.Errorf("decode output %q: %w", name, err)
		}
		isSensitive := eo.Sensitive || eo.HasMarks || len(eo.SensitivePaths) > 0 || (val != cty.NilVal && val.HasMarkDeep(golden.SensitiveMark))
		view := RunOutputView{
			Name:        eo.Name,
			Description: eo.Description,
			Sensitive:   isSensitive,
			Unknown:     eo.Unknown,
		}
		if view.Name == "" {
			view.Name = name
		}
		if isSensitive && !reveal && (eo.Sensitive || (val != cty.NilVal && val.HasMark(golden.SensitiveMark))) {
			view.Value = sensitiveMaskLiteral
			out[name] = view
			continue
		}
		if eo.Unknown || val == cty.NilVal || !val.IsKnown() {
			out[name] = view
			continue
		}
		rendered, err := renderCtyValue(val, reveal)
		if err != nil {
			return nil, fmt.Errorf("render output %q: %w", name, err)
		}
		view.Value = rendered
		out[name] = view
	}
	return out, nil
}

func renderCtyValue(val cty.Value, reveal bool) (any, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	if !reveal && val.HasMark(golden.SensitiveMark) {
		return sensitiveMaskLiteral, nil
	}
	unmarked, _ := val.Unmark()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsKnown() {
		return nil, nil
	}
	t := unmarked.Type()
	switch {
	case t.IsObjectType() || t.IsMapType():
		m := make(map[string]any)
		for it := unmarked.ElementIterator(); it.Next(); {
			k, elem := it.Element()
			kUnmarked, _ := k.UnmarkDeep()
			v, err := renderCtyValue(elem, reveal)
			if err != nil {
				return nil, err
			}
			m[kUnmarked.AsString()] = v
		}
		return m, nil
	case t.IsListType() || t.IsTupleType() || t.IsSetType():
		arr := make([]any, 0, unmarked.LengthInt())
		for it := unmarked.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			v, err := renderCtyValue(elem, reveal)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	default:
		deepUnmarked, _ := unmarked.UnmarkDeep()
		encoded, err := json.Marshal(ctyjson.SimpleJSONValue{Value: deepUnmarked})
		if err != nil {
			return nil, err
		}
		var native any
		if err := json.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		return native, nil
	}
}

func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	var req run.PauseRunReq
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
			return
		}
	}
	if qMode := strings.TrimSpace(r.URL.Query().Get("mode")); qMode != "" {
		req.Mode = qMode
	}
	if strings.TrimSpace(req.Mode) == "" {
		req.Mode = "planned"
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "planned" && mode != "immediate" {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid pause mode %q", req.Mode))
		return
	}
	req.Mode = mode

	resp, err := s.cfg.Backend.PauseRun(r.Context(), runID, &req)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	resp, err := s.cfg.Backend.ResumeRun(r.Context(), runID)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	resp, err := s.cfg.Backend.DeleteRun(r.Context(), runID)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func decodeNodeIDParam(r *http.Request) string {
	raw := chi.URLParam(r, "node_id")
	if unescaped, err := url.PathUnescape(raw); err == nil {
		return unescaped
	}
	return raw
}

func findNodeSummary(st *run.GetRunStateResp, nodeAddr string) *run.NodeRunSummary {
	if st == nil {
		return nil
	}
	for i := range st.Nodes {
		if st.Nodes[i].CanonicalAddress == nodeAddr || st.Nodes[i].Address == nodeAddr {
			cp := st.Nodes[i]
			return &cp
		}
	}
	return nil
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	nodeAddr := decodeNodeIDParam(r)

	runState, err := s.cfg.Backend.GetRunState(r.Context(), runID)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	sum := findNodeSummary(runState, nodeAddr)
	if sum == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found in run %q", nodeAddr, runID))
		return
	}
	canonical := sum.CanonicalAddress
	if canonical == "" {
		canonical = nodeAddr
	}

	detail := NodeDetailResponse{
		RunID:        runID,
		NodeAddr:     canonical,
		Summary:      sum,
		CurrentEpoch: sum.CurrentEpoch,
		Status:       sum.Status,
		LastError:    sum.ErrorMessage,
		Artifacts:    sum.Artifacts,
	}

	if sum.Kind == "worker" {
		if ns, nErr := s.cfg.Backend.GetNodeState(r.Context(), runID, canonical); nErr == nil && ns != nil {
			detail.CurrentEpoch = ns.CurrentEpoch
			detail.Status = ns.Status
			detail.ManualPaused = ns.ManualPaused
			detail.WorkerProcessID = ns.WorkerProcessID
			detail.WorkerAppID = ns.WorkerAppID
			if ns.LastError != "" {
				detail.LastError = ns.LastError
			}
			detail.Checkpoints = ns.Checkpoints
			detail.PendingInterjections = ns.PendingInterjections
			detail.InterjectionLog = ns.InterjectionLog
			detail.DecisionCards = ns.DecisionCards
			detail.PendingPublication = ns.PendingPublication
			detail.Approval = ns.Approval
			detail.Outcome = ns.Outcome
			if len(detail.Artifacts) == 0 && ns.Outcome != nil {
				detail.Artifacts = ns.Outcome.Artifacts
			}
		}
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleGetNodeLogs(w http.ResponseWriter, r *http.Request) {
	if s.cfg.BlobStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "blob store unavailable")
		return
	}
	runID := chi.URLParam(r, "run_id")
	nodeAddr := decodeNodeIDParam(r)

	var epoch uint64
	if rawEpoch := strings.TrimSpace(r.URL.Query().Get("epoch")); rawEpoch != "" {
		parsed, err := strconv.ParseUint(rawEpoch, 10, 64)
		if err != nil || parsed == 0 {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid epoch %q", rawEpoch))
			return
		}
		epoch = parsed
	} else {
		runState, err := s.cfg.Backend.GetRunState(r.Context(), runID)
		if err != nil {
			decodeRunBackendError(w, err)
			return
		}
		sum := findNodeSummary(runState, nodeAddr)
		if sum == nil {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found in run %q", nodeAddr, runID))
			return
		}
		nodeAddr = sum.CanonicalAddress
		epoch = sum.CurrentEpoch
		if ns, err := s.cfg.Backend.GetNodeState(r.Context(), runID, nodeAddr); err == nil && ns != nil && ns.CurrentEpoch > 0 {
			epoch = ns.CurrentEpoch
		}
		if epoch == 0 {
			epoch = 1
		}
	}

	logKey := daemon.NodeLogBlobKey(runID, nodeAddr, epoch)
	rc, _, err := s.cfg.BlobStore.Get(r.Context(), logKey, "")
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("logs not found for %s epoch %d", nodeAddr, epoch))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleRerunNode(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	nodeAddr := decodeNodeIDParam(r)
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
			return
		}
	}
	resp, err := s.cfg.Backend.RerunNode(r.Context(), runID, &run.RerunNodeReq{
		NodeAddr: nodeAddr,
		Reason:   body.Reason,
	})
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePostDiaryMessage(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	nodeAddr := decodeNodeIDParam(r)
	var req node.EnqueueInterjectionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSONError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Verify run & node exist before enqueueing on NodeActor.
	runState, err := s.cfg.Backend.GetRunState(r.Context(), runID)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	sum := findNodeSummary(runState, nodeAddr)
	if sum == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found in run %q", nodeAddr, runID))
		return
	}

	resp, err := s.cfg.Backend.EnqueueInterjection(r.Context(), runID, sum.CanonicalAddress, &req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListDecisions(w http.ResponseWriter, r *http.Request) {
	runIDFilter := strings.TrimSpace(r.URL.Query().Get("run_id"))
	statusFilter := node.DecisionCardStatus(strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))))

	var runIDs []string
	if runIDFilter != "" {
		runIDs = []string{runIDFilter}
	} else {
		listResp, err := s.cfg.Backend.ListRuns(r.Context(), &run.ListRunsReq{})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if listResp != nil {
			for _, rs := range listResp.Runs {
				if rs.Status != run.RunStatusDeleted {
					runIDs = append(runIDs, rs.RunID)
				}
			}
		}
	}

	decisions := make([]node.DecisionCard, 0)
	for _, rid := range runIDs {
		resp, err := s.cfg.Backend.ListDecisions(r.Context(), rid)
		if err != nil {
			if runIDFilter != "" {
				decodeRunBackendError(w, err)
				return
			}
			continue
		}
		if resp == nil {
			continue
		}
		for _, card := range resp.Decisions {
			if statusFilter != "" && card.Status != statusFilter {
				continue
			}
			decisions = append(decisions, card)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"decisions": decisions})
}

func (s *Server) handleRespondDecision(w http.ResponseWriter, r *http.Request) {
	cardID := chi.URLParam(r, "id")
	var body struct {
		RunID            string   `json:"run_id,omitempty"`
		NodeAddr         string   `json:"node_addr,omitempty"`
		SelectedOption   string   `json:"selected_option,omitempty"`
		AnswerText       string   `json:"answer_text,omitempty"`
		Approved         *bool    `json:"approved,omitempty"`
		ReturnedUpstream bool     `json:"returned_upstream,omitempty"`
		ReturnFallback   string   `json:"return_fallback,omitempty"`
		CommitSHA        string   `json:"commit_sha,omitempty"`
		ArtifactIDs      []string `json:"artifact_ids,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
		return
	}

	runID := strings.TrimSpace(body.RunID)
	nodeAddr := strings.TrimSpace(body.NodeAddr)
	if runID == "" || nodeAddr == "" {
		foundRun, foundNode, found, err := s.locateDecisionOwner(r.Context(), cardID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("decision %q not found", cardID))
			return
		}
		runID = foundRun
		nodeAddr = foundNode
	}

	runPaused := false
	if st, err := s.cfg.Backend.GetRunState(r.Context(), runID); err == nil && st != nil {
		runPaused = st.ManualPaused || st.Status == run.RunStatusPaused || st.Status == run.RunStatusPausing
	}

	resp, err := s.cfg.Backend.SubmitDecision(r.Context(), runID, nodeAddr, &node.SubmitDecisionReq{
		CardID:           cardID,
		SelectedOption:   body.SelectedOption,
		AnswerText:       body.AnswerText,
		Approved:         body.Approved,
		ReturnedUpstream: body.ReturnedUpstream,
		ReturnFallback:   body.ReturnFallback,
		CommitSHA:        body.CommitSHA,
		ArtifactIDs:      body.ArtifactIDs,
		RunPaused:        runPaused,
	})
	if err != nil {
		if errors.Is(err, node.ErrInvalidDecision) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "not found") {
				writeJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			if strings.Contains(msg, "required") || strings.Contains(msg, "cannot both") {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) locateDecisionOwner(ctx context.Context, cardID string) (runID, nodeAddr string, found bool, err error) {
	listResp, err := s.cfg.Backend.ListRuns(ctx, &run.ListRunsReq{})
	if err != nil {
		return "", "", false, err
	}
	if listResp == nil {
		return "", "", false, nil
	}
	var fallbackRun, fallbackNode string
	for _, rs := range listResp.Runs {
		if rs.Status == run.RunStatusDeleted {
			continue
		}
		decResp, dErr := s.cfg.Backend.ListDecisions(ctx, rs.RunID)
		if dErr != nil || decResp == nil {
			continue
		}
		for _, c := range decResp.Decisions {
			if c.CardID == cardID {
				if c.Status == node.DecisionStatusPending {
					return c.RunID, c.NodeAddr, true, nil
				}
				fallbackRun = c.RunID
				fallbackNode = c.NodeAddr
			}
		}
	}
	if fallbackRun != "" {
		return fallbackRun, fallbackNode, true, nil
	}
	return "", "", false, nil
}

func parseConfigScope(scopeStr, nsStr string) (config.Scope, string, error) {
	s := strings.ToLower(strings.TrimSpace(scopeStr))
	ns := strings.TrimSpace(nsStr)
	if s == "" {
		if ns != "" {
			return config.ScopeNamespace, config.NormalizeNamespace(ns), nil
		}
		return config.ScopeGlobal, "", nil
	}
	switch config.Scope(s) {
	case config.ScopeGlobal:
		if ns != "" {
			return "", "", fmt.Errorf("%w: namespace must be empty for global scope", config.ErrInvalidScope)
		}
		return config.ScopeGlobal, "", nil
	case config.ScopeNamespace:
		return config.ScopeNamespace, config.NormalizeNamespace(ns), nil
	default:
		return "", "", fmt.Errorf("%w: unsupported scope %q (allowed: global, namespace)", config.ErrInvalidScope, scopeStr)
	}
}

func (s *Server) handleListEnvs(w http.ResponseWriter, r *http.Request) {
	s.handleListConfigKind(w, r, false)
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	s.handleListConfigKind(w, r, true)
}

func (s *Server) handleListConfigKind(w http.ResponseWriter, r *http.Request, isSecret bool) {
	scope, ns, err := parseConfigScope(r.URL.Query().Get("scope"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.cfg.Backend.ListConfig(r.Context(), &config.ListMetadataReq{
		Scope:     scope,
		Namespace: ns,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if isSecret {
		secrets := []config.SecretMeta{}
		if resp != nil && resp.Secrets != nil {
			secrets = resp.Secrets
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"scope":     scope,
			"namespace": ns,
			"secrets":   secrets,
		})
		return
	}
	envs := []config.EnvEntry{}
	if resp != nil && resp.Envs != nil {
		envs = resp.Envs
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":     scope,
		"namespace": ns,
		"envs":      envs,
	})
}

type configUpsertPayload struct {
	Scope     string `json:"scope,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Value     string `json:"value,omitempty"`
	Entries   []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"entries,omitempty"`
}

func (s *Server) handleUpsertEnvs(w http.ResponseWriter, r *http.Request) {
	s.handleUpsertConfigKind(w, r, false)
}

func (s *Server) handleUpsertSecrets(w http.ResponseWriter, r *http.Request) {
	s.handleUpsertConfigKind(w, r, true)
}

func (s *Server) handleUpsertConfigKind(w http.ResponseWriter, r *http.Request, isSecret bool) {
	var body configUpsertPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
		return
	}
	scope, ns, err := parseConfigScope(body.Scope, body.Namespace)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pathName := strings.TrimSpace(chi.URLParam(r, "name")); pathName != "" && body.Name == "" {
		body.Name = pathName
	}

	var entries []config.EntryInput
	if len(body.Entries) > 0 {
		entries = make([]config.EntryInput, 0, len(body.Entries))
		for _, item := range body.Entries {
			entries = append(entries, config.EntryInput{Name: item.Name, Value: item.Value, IsSecret: isSecret})
		}
	} else if strings.TrimSpace(body.Name) != "" {
		entries = []config.EntryInput{{Name: body.Name, Value: body.Value, IsSecret: isSecret}}
	} else {
		writeJSONError(w, http.StatusBadRequest, "name or entries is required")
		return
	}

	resp, err := s.cfg.Backend.UpsertConfig(r.Context(), &config.UpsertEntriesReq{
		Scope:     scope,
		Namespace: ns,
		Entries:   entries,
	})
	if err != nil {
		switch {
		case errors.Is(err, config.ErrKindConflict):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, config.ErrInvalidKeyName), errors.Is(err, config.ErrInvalidScope):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteEnvs(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteConfigKind(w, r, false)
}

func (s *Server) handleDeleteSecrets(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteConfigKind(w, r, true)
}

func (s *Server) handleDeleteConfigKind(w http.ResponseWriter, r *http.Request, isSecret bool) {
	var body struct {
		Scope     string `json:"scope,omitempty"`
		Namespace string `json:"namespace,omitempty"`
		Name      string `json:"name,omitempty"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
			return
		}
	}
	if qScope := strings.TrimSpace(r.URL.Query().Get("scope")); qScope != "" && body.Scope == "" {
		body.Scope = qScope
	}
	if qNS := strings.TrimSpace(r.URL.Query().Get("namespace")); qNS != "" && body.Namespace == "" {
		body.Namespace = qNS
	}
	if pathName := strings.TrimSpace(chi.URLParam(r, "name")); pathName != "" && body.Name == "" {
		body.Name = pathName
	}
	if qName := strings.TrimSpace(r.URL.Query().Get("name")); qName != "" && body.Name == "" {
		body.Name = qName
	}

	scope, ns, err := parseConfigScope(body.Scope, body.Namespace)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.ValidateKeyName(body.Name); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	meta, err := s.cfg.Backend.ListConfig(r.Context(), &config.ListMetadataReq{
		Scope:     scope,
		Namespace: ns,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if meta != nil {
		if isSecret {
			for _, env := range meta.Envs {
				if env.Name == body.Name {
					writeJSONError(w, http.StatusConflict, fmt.Sprintf("key %q is an env, not a secret", body.Name))
					return
				}
			}
		} else {
			for _, sec := range meta.Secrets {
				if sec.Name == body.Name {
					writeJSONError(w, http.StatusConflict, fmt.Sprintf("key %q is a secret, not an env", body.Name))
					return
				}
			}
		}
	}

	resp, err := s.cfg.Backend.DeleteConfig(r.Context(), &config.DeleteEntryReq{
		Scope:     scope,
		Namespace: ns,
		Name:      body.Name,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) findArtifactInRun(ctx context.Context, runID, artifactID string) (artifact.PublishedArtifact, bool, error) {
	st, err := s.cfg.Backend.GetRunState(ctx, runID)
	if err != nil {
		return artifact.PublishedArtifact{}, false, err
	}
	for _, n := range st.Nodes {
		for _, art := range n.Artifacts {
			if art.ID == artifactID {
				return art, true, nil
			}
		}
	}
	if decResp, dErr := s.cfg.Backend.ListDecisions(ctx, runID); dErr == nil && decResp != nil {
		for _, card := range decResp.Decisions {
			for _, art := range card.Artifacts {
				if art.ID == artifactID {
					return art, true, nil
				}
			}
		}
	}
	return artifact.PublishedArtifact{}, false, nil
}

func (s *Server) handleMintPreviewTicket(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	artifactID := chi.URLParam(r, "artifact_id")
	if _, found, err := s.findArtifactInRun(r.Context(), runID, artifactID); err != nil {
		decodeRunBackendError(w, err)
		return
	} else if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("artifact %q not found in run %q", artifactID, runID))
		return
	}

	ticket, expiresAt, err := s.MintPreviewTicket(runID, artifactID, s.cfg.PreviewTicketTTL)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_at": expiresAt,
	})
}

func (s *Server) handleArtifactContent(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	artifactID := chi.URLParam(r, "artifact_id")

	// Authorize via either Bearer token (when APIToken set) OR scoped HMAC preview ticket.
	if s.cfg.APIToken != "" {
		authorized := false
		if tok, ok := extractBearerToken(r); ok && constantTimeEqual(tok, s.cfg.APIToken) {
			authorized = true
		} else if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			if err := s.VerifyPreviewTicket(ticket, runID, artifactID); err == nil {
				authorized = true
			}
		}
		if !authorized {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	} else if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
		if err := s.VerifyPreviewTicket(ticket, runID, artifactID); err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
	}

	if s.cfg.BlobStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "blob store unavailable")
		return
	}
	art, found, err := s.findArtifactInRun(r.Context(), runID, artifactID)
	if err != nil {
		decodeRunBackendError(w, err)
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("artifact %q not found in run %q", artifactID, runID))
		return
	}

	blobKey := strings.TrimSpace(art.BlobKey)
	if blobKey == "" {
		blobKey = fmt.Sprintf("runs/%s/artifacts/%s.zip", runID, artifactID)
	}
	rc, _, err := s.cfg.BlobStore.Get(r.Context(), blobKey, art.SHA256)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "artifact blob not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rawBytes, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		writeJSONError(w, http.StatusInternalServerError, readErr.Error())
		return
	}

	payload, contentType, err := extractArtifactPreviewContent(art, rawBytes, strings.TrimSpace(r.URL.Query().Get("entry")))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}

	w.Header().Set("Content-Security-Policy", ArtifactPreviewCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func extractArtifactPreviewContent(art artifact.PublishedArtifact, rawBytes []byte, entryQuery string) ([]byte, string, error) {
	isZip := len(rawBytes) >= 4 && bytes.Equal(rawBytes[:4], []byte{'P', 'K', 0x03, 0x04})
	if !isZip {
		return rawBytes, detectArtifactContentType(art.RelPath, rawBytes), nil
	}

	zr, err := zip.NewReader(bytes.NewReader(rawBytes), int64(len(rawBytes)))
	if err != nil {
		return rawBytes, detectArtifactContentType(art.RelPath, rawBytes), nil
	}

	if art.Type == "directory" {
		targetEntry := ""
		if entryQuery != "" {
			clean := path.Clean(strings.TrimPrefix(entryQuery, "/"))
			if clean == "." || strings.HasPrefix(clean, "..") {
				return nil, "", errors.New("invalid entry path")
			}
			targetEntry = "tree/" + clean
		} else {
			for _, f := range zr.File {
				if f.Name == "tree/index.html" {
					targetEntry = f.Name
					break
				}
			}
		}
		if targetEntry == "" {
			names := make([]string, 0, len(zr.File))
			for _, f := range zr.File {
				if !strings.HasSuffix(f.Name, "/") {
					names = append(names, strings.TrimPrefix(f.Name, "tree/"))
				}
			}
			listing, _ := json.Marshal(map[string]any{"entries": names})
			return listing, "application/json", nil
		}
		for _, f := range zr.File {
			if f.Name == targetEntry && !f.Mode().IsDir() {
				data, err := readZipFileBytes(f)
				if err != nil {
					return nil, "", err
				}
				return data, detectArtifactContentType(targetEntry, data), nil
			}
		}
		return nil, "", fmt.Errorf("artifact entry %q not found", entryQuery)
	}

	for _, f := range zr.File {
		if f.Name == "payload" && !f.Mode().IsDir() {
			data, err := readZipFileBytes(f)
			if err != nil {
				return nil, "", err
			}
			return data, detectArtifactContentType(art.RelPath, data), nil
		}
	}
	return rawBytes, detectArtifactContentType(art.RelPath, rawBytes), nil
}

func readZipFileBytes(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, artifact.DefaultMaxSingleFileBytes))
}

func detectArtifactContentType(relPath string, data []byte) string {
	if ext := filepath.Ext(relPath); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return http.DetectContentType(data)
}

// Unused import safeguard check for maps.
var _ = maps.Clone[map[string]string]
