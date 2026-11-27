package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/lonegunmanb/sofa/internal/daprhost"
)

const operatorStateKey = "operator_actor_v1"

type persistedOperatorState struct {
	Runs map[string]RunSummary `json:"runs,omitempty"`
}

// OperatorOptions configures OperatorActor("global") dependencies.
type OperatorOptions struct {
	ClientProvider func() daprclient.Client
	RunReconciler  RunReconciler
	Clock          func() time.Time
}

// OperatorActor maintains the persistent Run index and wakes active Runs on control-plane recovery.
type OperatorActor struct {
	actor.ServerImplBaseCtx

	mu   sync.Mutex
	opts OperatorOptions
}

// NewOperatorActor constructs an OperatorActor singleton instance.
func NewOperatorActor(opts OperatorOptions) *OperatorActor {
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.RunReconciler == nil && opts.ClientProvider != nil {
		opts.RunReconciler = NewDaprRunReconciler(opts.ClientProvider)
	}
	return &OperatorActor{opts: opts}
}

// Type implements actor.ServerContext.
func (a *OperatorActor) Type() string {
	return OperatorActorType
}

// SetStateManager binds daprhost.NewActorStateManager when ClientProvider yields a Dapr client,
// preserving direct StateManagerContext injection in Tier 1 unit tests.
func (a *OperatorActor) SetStateManager(sm actor.StateManagerContext) {
	if a.opts.ClientProvider != nil {
		if c := a.opts.ClientProvider(); c != nil {
			if bound := daprhost.NewActorStateManager(a.Type(), a.ID(), c); bound != nil {
				a.ServerImplBaseCtx.SetStateManager(bound)
				return
			}
		}
	}
	a.ServerImplBaseCtx.SetStateManager(sm)
}

func (a *OperatorActor) loadStateLocked(ctx context.Context) (persistedOperatorState, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return persistedOperatorState{}, errors.New("operator: state manager not initialized")
	}
	var st persistedOperatorState
	ok, err := sm.Contains(ctx, operatorStateKey)
	if err != nil {
		return persistedOperatorState{}, fmt.Errorf("operator: check state: %w", err)
	}
	if ok {
		if err := sm.Get(ctx, operatorStateKey, &st); err != nil {
			return persistedOperatorState{}, fmt.Errorf("operator: load state: %w", err)
		}
	}
	if st.Runs == nil {
		st.Runs = make(map[string]RunSummary)
	}
	return st, nil
}

func (a *OperatorActor) saveStateLocked(ctx context.Context, st persistedOperatorState) error {
	sm := a.GetStateManager()
	if sm == nil {
		return errors.New("operator: state manager not initialized")
	}
	if err := sm.Set(ctx, operatorStateKey, st); err != nil {
		return fmt.Errorf("operator: stage state: %w", err)
	}
	if err := sm.Save(ctx); err != nil {
		return fmt.Errorf("operator: save state: %w", err)
	}
	return nil
}

// UpsertRun inserts or updates a RunSummary in persistent state.
func (a *OperatorActor) UpsertRun(ctx context.Context, req *UpsertRunReq) (*UpsertRunResp, error) {
	if req == nil {
		return nil, errors.New("operator: upsert request is nil")
	}
	runID := strings.TrimSpace(req.Summary.RunID)
	if runID == "" {
		return nil, errors.New("operator: run_id is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	st, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}

	now := a.opts.Clock()
	summary := req.Summary
	summary.RunID = runID
	summary.Namespace = normalizeNamespace(summary.Namespace)
	if existing, ok := st.Runs[runID]; ok {
		if summary.CreatedAt.IsZero() {
			summary.CreatedAt = existing.CreatedAt
		}
		if summary.PlanBlobKey == "" {
			summary.PlanBlobKey = existing.PlanBlobKey
		}
		if summary.PlanSHA256 == "" {
			summary.PlanSHA256 = existing.PlanSHA256
		}
	}
	if summary.CreatedAt.IsZero() {
		summary.CreatedAt = now
	}
	if summary.UpdatedAt.IsZero() {
		summary.UpdatedAt = now
	}
	st.Runs[runID] = summary
	if err := a.saveStateLocked(ctx, st); err != nil {
		return nil, err
	}
	return &UpsertRunResp{
		OK:      true,
		Summary: summary,
	}, nil
}

// GetRunSummary retrieves a single RunSummary by run_id.
func (a *OperatorActor) GetRunSummary(ctx context.Context, req *GetRunSummaryReq) (*GetRunSummaryResp, error) {
	if req == nil || strings.TrimSpace(req.RunID) == "" {
		return &GetRunSummaryResp{Found: false, ErrorCode: ErrCodeNotFound}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	st, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	summary, ok := st.Runs[strings.TrimSpace(req.RunID)]
	if !ok {
		return &GetRunSummaryResp{Found: false, ErrorCode: ErrCodeNotFound}, nil
	}
	return &GetRunSummaryResp{
		Found:   true,
		Summary: summary,
	}, nil
}

// ListRuns returns indexed RunSummary items optionally filtered by Namespace and/or Status.
func (a *OperatorActor) ListRuns(ctx context.Context, req *ListRunsReq) (*ListRunsResp, error) {
	if req == nil {
		req = &ListRunsReq{}
	}
	nsFilter := strings.TrimSpace(req.Namespace)
	statusFilter := req.Status

	a.mu.Lock()
	defer a.mu.Unlock()

	st, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	runs := make([]RunSummary, 0, len(st.Runs))
	for _, item := range st.Runs {
		if nsFilter != "" && normalizeNamespace(item.Namespace) != nsFilter {
			continue
		}
		if statusFilter != "" && item.Status != statusFilter {
			continue
		}
		runs = append(runs, item)
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.After(runs[j].CreatedAt)
		}
		return runs[i].RunID < runs[j].RunID
	})
	return &ListRunsResp{Runs: runs}, nil
}

// RemoveRun removes a RunSummary from persistent index (invoked only by DeleteRun).
func (a *OperatorActor) RemoveRun(ctx context.Context, req *RemoveRunReq) (*RemoveRunResp, error) {
	if req == nil || strings.TrimSpace(req.RunID) == "" {
		return &RemoveRunResp{Removed: false}, nil
	}
	runID := strings.TrimSpace(req.RunID)

	a.mu.Lock()
	defer a.mu.Unlock()

	st, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := st.Runs[runID]; !ok {
		return &RemoveRunResp{Removed: false}, nil
	}
	delete(st.Runs, runID)
	if err := a.saveStateLocked(ctx, st); err != nil {
		return nil, err
	}
	return &RemoveRunResp{Removed: true}, nil
}

// RecoverActiveRuns wakes all RUNNING and PAUSING RunActors via Reconcile after control-plane restart.
func (a *OperatorActor) RecoverActiveRuns(ctx context.Context, req *RecoverActiveRunsReq) (*RecoverActiveRunsResp, error) {
	if req == nil {
		req = &RecoverActiveRunsReq{}
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "operator-recovery"
	}

	a.mu.Lock()
	st, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	var toWake []string
	for id, item := range st.Runs {
		if item.Status == RunStatusRunning || item.Status == RunStatusPausing {
			toWake = append(toWake, id)
		}
	}
	sort.Strings(toWake)
	reconciler := a.opts.RunReconciler
	a.mu.Unlock()

	resp := &RecoverActiveRunsResp{}
	for _, runID := range toWake {
		if reconciler == nil {
			resp.RecoveredRunIDs = append(resp.RecoveredRunIDs, runID)
			continue
		}
		if _, recErr := reconciler.Reconcile(ctx, runID, &ReconcileReq{Reason: reason}); recErr != nil {
			if resp.FailedRuns == nil {
				resp.FailedRuns = make(map[string]string)
			}
			resp.FailedRuns[runID] = recErr.Error()
			continue
		}
		resp.RecoveredRunIDs = append(resp.RecoveredRunIDs, runID)
	}
	return resp, nil
}

func normalizeNamespace(ns string) string {
	trimmed := strings.TrimSpace(ns)
	if trimmed == "" {
		return DefaultNamespace
	}
	return trimmed
}
