package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

const nodeStateKey = "node_actor_v1"

type registeredBranchEntry struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
	Epoch   uint64 `json:"epoch"`
}

type persistedNodeState struct {
	RunID                        string                           `json:"run_id,omitempty"`
	NodeAddr                     string                           `json:"node_addr,omitempty"`
	CurrentEpoch                 uint64                           `json:"current_epoch"`
	Status                       NodeStatus                       `json:"status"`
	ManualPaused                 bool                             `json:"manual_paused,omitempty"`
	LeaseID                      string                           `json:"lease_id,omitempty"`
	LeaseKey                     string                           `json:"lease_key,omitempty"`
	WorkerProcessID              string                           `json:"worker_process_id,omitempty"`
	WorkerAppID                  string                           `json:"worker_app_id,omitempty"`
	Dispatch                     *DispatchSpec                    `json:"dispatch,omitempty"`
	NextCheckpointSeq            uint64                           `json:"next_checkpoint_seq"`
	Checkpoints                  []checkpoint.CheckpointRecord    `json:"checkpoints,omitempty"`
	RegisteredCheckpointBranches map[string]registeredBranchEntry `json:"registered_checkpoint_branches,omitempty"`
	RegisteredWorkBranches       map[string]registeredBranchEntry `json:"registered_work_branches,omitempty"`
	PendingInterjections         []session.HumanInterjection      `json:"pending_interjections,omitempty"`
	InterjectionLog              []InterjectionRecord             `json:"interjection_log,omitempty"`
	QCLedgerSnapshot             []byte                           `json:"qc_ledger_snapshot,omitempty"`
	PendingPublication           *runner.PendingPublication       `json:"pending_publication,omitempty"`
	Approval                     *runner.ApprovalRecord           `json:"approval,omitempty"`
	DecisionCards                []DecisionCard                   `json:"decision_cards,omitempty"`
	Outcome                      *runner.NodeOutcome              `json:"outcome,omitempty"`
	LastError                    string                           `json:"last_error,omitempty"`
}

func defaultNodeState(actorID string) persistedNodeState {
	runID, nodeAddr, _ := DecodeNodeActorID(actorID)
	return persistedNodeState{
		RunID:                        runID,
		NodeAddr:                     nodeAddr,
		CurrentEpoch:                 1,
		Status:                       StatusPending,
		NextCheckpointSeq:            1,
		RegisteredCheckpointBranches: make(map[string]registeredBranchEntry),
		RegisteredWorkBranches:       make(map[string]registeredBranchEntry),
	}
}

func (s *persistedNodeState) ensureDefaults(actorID string) {
	if s.CurrentEpoch == 0 {
		s.CurrentEpoch = 1
	}
	if s.Status == "" {
		s.Status = StatusPending
	}
	if s.NextCheckpointSeq == 0 {
		s.NextCheckpointSeq = uint64(len(s.Checkpoints) + 1)
	}
	if s.RunID == "" || s.NodeAddr == "" {
		if runID, nodeAddr, err := DecodeNodeActorID(actorID); err == nil {
			if s.RunID == "" {
				s.RunID = runID
			}
			if s.NodeAddr == "" {
				s.NodeAddr = nodeAddr
			}
		}
	}
	if s.RegisteredCheckpointBranches == nil {
		s.RegisteredCheckpointBranches = make(map[string]registeredBranchEntry)
	}
	if s.RegisteredWorkBranches == nil {
		s.RegisteredWorkBranches = make(map[string]registeredBranchEntry)
	}
}

func (s persistedNodeState) clone() persistedNodeState {
	out := s
	out.Dispatch = cloneDispatchSpec(s.Dispatch)
	out.Checkpoints = cloneCheckpointRecords(s.Checkpoints)
	out.RegisteredCheckpointBranches = maps.Clone(s.RegisteredCheckpointBranches)
	out.RegisteredWorkBranches = maps.Clone(s.RegisteredWorkBranches)
	out.PendingInterjections = slices.Clone(s.PendingInterjections)
	out.InterjectionLog = cloneInterjectionLog(s.InterjectionLog)
	out.QCLedgerSnapshot = append([]byte(nil), s.QCLedgerSnapshot...)
	out.PendingPublication = clonePendingPublication(s.PendingPublication)
	out.Approval = cloneApprovalRecord(s.Approval)
	out.DecisionCards = cloneDecisionCards(s.DecisionCards)
	out.Outcome = cloneNodeOutcome(s.Outcome)
	return out
}

// Options configures a NodeActor's external boundaries, clocks, and retry cadence.
type Options struct {
	ClientProvider    func() daprclient.Client
	ConfigResolver    ConfigResolver
	SlotManager       SlotManager
	WorkerDispatcher  WorkerDispatcher
	RunNotifier       RunNotifier
	ReminderScheduler ReminderScheduler
	Clock             func() time.Time
	SlotRetryInterval time.Duration
	ReminderName      string
}

// NodeActor is the authoritative Dapr Actor state machine for a single static or dynamic_worker node.
type NodeActor struct {
	actor.ServerImplBaseCtx

	mu                sync.Mutex
	clientProvider    func() daprclient.Client
	configResolver    ConfigResolver
	slotManager       SlotManager
	workerDispatcher  WorkerDispatcher
	runNotifier       RunNotifier
	reminderScheduler ReminderScheduler
	clock             func() time.Time
	slotRetryInterval time.Duration
	reminderName      string
}

// NewNodeActor constructs a NodeActor with defaults and optional test/Dapr boundary overrides.
func NewNodeActor(opts Options) *NodeActor {
	interval := opts.SlotRetryInterval
	if interval <= 0 {
		interval = DefaultSlotRetryInterval
	}
	remName := strings.TrimSpace(opts.ReminderName)
	if remName == "" {
		remName = DefaultSlotRetryReminderName
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}

	a := &NodeActor{
		clientProvider:    opts.ClientProvider,
		configResolver:    opts.ConfigResolver,
		slotManager:       opts.SlotManager,
		workerDispatcher:  opts.WorkerDispatcher,
		runNotifier:       opts.RunNotifier,
		reminderScheduler: opts.ReminderScheduler,
		clock:             clock,
		slotRetryInterval: interval,
		reminderName:      remName,
	}
	if opts.ClientProvider != nil {
		if a.configResolver == nil {
			a.configResolver = &daprConfigResolver{clientProvider: opts.ClientProvider}
		}
		if a.slotManager == nil {
			a.slotManager = &daprSlotManager{clientProvider: opts.ClientProvider}
		}
		if a.workerDispatcher == nil {
			a.workerDispatcher = &daprWorkerDispatcher{clientProvider: opts.ClientProvider}
		}
		if a.runNotifier == nil {
			a.runNotifier = &daprRunNotifier{clientProvider: opts.ClientProvider}
		}
		if a.reminderScheduler == nil {
			a.reminderScheduler = &daprReminderScheduler{clientProvider: opts.ClientProvider}
		}
	}
	return a
}

// Type implements actor.ServerContext.
func (a *NodeActor) Type() string {
	return ActorType
}

// SetStateManager binds state persistence to daprhost.NewActorStateManager when clientProvider
// supplies a Dapr client, while preserving direct StateManagerContext injection in unit tests.
func (a *NodeActor) SetStateManager(sm actor.StateManagerContext) {
	if a.clientProvider != nil {
		if c := a.clientProvider(); c != nil {
			if bound := daprhost.NewActorStateManager(a.Type(), a.ID(), c); bound != nil {
				a.ServerImplBaseCtx.SetStateManager(bound)
				return
			}
		}
	}
	a.ServerImplBaseCtx.SetStateManager(sm)
}

// StartOrResume persists the node's DispatchSpec, resolves credentials, and initiates non-blocking slot dispatch.
func (a *NodeActor) StartOrResume(ctx context.Context, req *StartOrResumeReq) (*StartOrResumeResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: start_or_resume request is nil")
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	if req.RunID != "" {
		state.RunID = strings.TrimSpace(req.RunID)
	}
	if req.NodeAddr != "" {
		state.NodeAddr = strings.TrimSpace(req.NodeAddr)
	}
	if strings.TrimSpace(state.RunID) == "" || strings.TrimSpace(state.NodeAddr) == "" {
		a.mu.Unlock()
		return nil, errors.New("node: run_id and node_addr are required")
	}

	if state.Status == StatusRunning && !req.ForceResume && !state.ManualPaused {
		resp := &StartOrResumeResp{
			Accepted:        true,
			Epoch:           state.CurrentEpoch,
			Status:          state.Status,
			LeaseID:         state.LeaseID,
			WorkerProcessID: state.WorkerProcessID,
			WorkerAppID:     state.WorkerAppID,
		}
		a.mu.Unlock()
		return resp, nil
	}

	state.Dispatch = mergeDispatchSpec(state.Dispatch, state.RunID, state.NodeAddr, req)
	if strings.TrimSpace(state.Dispatch.RepoURL) == "" {
		a.mu.Unlock()
		return nil, errors.New("node: repo_url is required")
	}

	if state.ManualPaused || state.Status == StatusWaitingHuman || state.Status == StatusPaused || state.Status == StatusFailed || (req.ForceResume && state.Status == StatusRunning) {
		state.CurrentEpoch++
		clearLeaseLocked(&state)
	}
	state.ManualPaused = false
	state.LastError = ""
	state.Status = StatusQueuedSlot
	targetEpoch := state.CurrentEpoch
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	return a.dispatchCurrentEpoch(ctx, targetEpoch, false)
}

// ReminderCall handles persistent QUEUED_SLOT retry ticks with explicit Set+Save persistence.
func (a *NodeActor) ReminderCall(_ string, _ []byte, _ string, _ string) {
	ctx := context.Background()

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return
	}
	if state.Status != StatusQueuedSlot || state.ManualPaused || state.Dispatch == nil {
		remSched := a.reminderScheduler
		actorID := a.effectiveActorID()
		remName := a.reminderName
		a.mu.Unlock()
		if remSched != nil && actorID != "" {
			_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
		}
		return
	}
	targetEpoch := state.CurrentEpoch
	a.mu.Unlock()

	_, _ = a.dispatchCurrentEpoch(ctx, targetEpoch, true)
}

// OnWorkerDead handles ProcessRegistryActor DEAD notifications, advancing epoch and taking over on a healthy worker.
func (a *NodeActor) OnWorkerDead(ctx context.Context, notice *registry.WorkerDeadNotification) (*OnWorkerDeadResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if notice == nil {
		return nil, errors.New("node: worker dead notification is nil")
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	matchesLease := (notice.LeaseID != "" && notice.LeaseID == state.LeaseID) ||
		(notice.LeaseID == "" && notice.ProcessID != "" && notice.ProcessID == state.WorkerProcessID)
	if state.Status != StatusRunning || state.ManualPaused || !matchesLease {
		resp := &OnWorkerDeadResp{
			Handled: false,
			Epoch:   state.CurrentEpoch,
			Status:  state.Status,
		}
		a.mu.Unlock()
		return resp, nil
	}

	state.CurrentEpoch++
	clearLeaseLocked(&state)
	state.Status = StatusQueuedSlot
	state.LastError = "worker dead: " + strings.TrimSpace(notice.Reason)
	newEpoch := state.CurrentEpoch
	runID, nodeAddr := state.RunID, state.NodeAddr
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	notifier := a.runNotifier
	now := a.clock()
	a.mu.Unlock()

	if notifier != nil {
		_ = notifier.NotifyNodeEvent(ctx, NodeEventNotification{
			RunID:        runID,
			NodeAddr:     nodeAddr,
			Epoch:        newEpoch,
			Status:       StatusQueuedSlot,
			ErrorMessage: state.LastError,
			OccurredAt:   now,
		})
	}

	dispResp, dispErr := a.dispatchCurrentEpoch(ctx, newEpoch, true)
	if dispErr != nil {
		return &OnWorkerDeadResp{
			Handled: true,
			Epoch:   newEpoch,
			Status:  StatusQueuedSlot,
		}, dispErr
	}
	return &OnWorkerDeadResp{
		Handled: true,
		Epoch:   dispResp.Epoch,
		Status:  dispResp.Status,
	}, nil
}

// PauseNode applies planned or immediate pause semantics; immediate pause increments epoch and aborts any active worker.
func (a *NodeActor) PauseNode(ctx context.Context, req *PauseNodeReq) (*PauseNodeResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: pause request is nil")
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "planned"
	}
	if mode != "planned" && mode != "immediate" {
		return nil, fmt.Errorf("node: unsupported pause mode %q", req.Mode)
	}

	var (
		needAbort    bool
		abortAppID   string
		abortReq     daemon.AbortNodeRequest
		releaseLease string
		releaseKey   string
		stopReminder bool
		notifyEvent  *NodeEventNotification
	)

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	state.ManualPaused = true
	if isTerminalStatus(state.Status) {
		resp := &PauseNodeResp{
			Paused: true,
			Epoch:  state.CurrentEpoch,
			Status: state.Status,
		}
		if err := a.saveStateLocked(ctx, state); err != nil {
			a.mu.Unlock()
			return nil, err
		}
		a.mu.Unlock()
		return resp, nil
	}

	if mode == "immediate" {
		if state.Status == StatusRunning || state.Status == StatusQueuedSlot {
			prevEpoch := state.CurrentEpoch
			if state.Status == StatusRunning && state.WorkerAppID != "" {
				needAbort = true
				abortAppID = state.WorkerAppID
				abortReq = daemon.AbortNodeRequest{
					RunID:    state.RunID,
					NodeAddr: state.NodeAddr,
					Epoch:    prevEpoch,
				}
			}
			releaseLease = state.LeaseID
			releaseKey = state.LeaseKey
			stopReminder = true
			state.CurrentEpoch++
			clearLeaseLocked(&state)
		}
		state.Status = StatusPaused
		if reason := strings.TrimSpace(req.Reason); reason != "" {
			state.LastError = reason
		}
		notifyEvent = &NodeEventNotification{
			RunID:        state.RunID,
			NodeAddr:     state.NodeAddr,
			Epoch:        state.CurrentEpoch,
			Status:       state.Status,
			ErrorMessage: state.LastError,
			OccurredAt:   a.clock(),
		}
	} else {
		// planned pause: let RUNNING node finish its current execution, halt queued or pending work.
		if state.Status == StatusQueuedSlot || state.Status == StatusPending {
			stopReminder = state.Status == StatusQueuedSlot
			state.Status = StatusPaused
		}
	}

	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	resp := &PauseNodeResp{
		Paused:        true,
		Epoch:         state.CurrentEpoch,
		Status:        state.Status,
		AbortedWorker: needAbort,
	}
	dispatcher := a.workerDispatcher
	slotMgr := a.slotManager
	remSched := a.reminderScheduler
	notifier := a.runNotifier
	actorID := a.effectiveActorID()
	remName := a.reminderName
	a.mu.Unlock()

	if stopReminder && remSched != nil && actorID != "" {
		_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
	}
	if needAbort && dispatcher != nil {
		_, _ = dispatcher.AbortNode(ctx, abortAppID, abortReq)
	}
	if (releaseLease != "" || releaseKey != "") && slotMgr != nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
			LeaseID:  releaseLease,
			LeaseKey: releaseKey,
		})
	}
	_ = notifyEvent
	_ = notifier

	return resp, nil
}

// AdvanceForRework fences prior epochs, aborts any active worker, updates BaseSHA/BasedSource,
// clears pending publication/approval, and injects the rework reason as a high-priority interjection.
func (a *NodeActor) AdvanceForRework(ctx context.Context, req *AdvanceForReworkReq) (*AdvanceForReworkResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: advance for rework request is nil")
	}

	var (
		needAbort    bool
		abortAppID   string
		abortReq     daemon.AbortNodeRequest
		releaseLease string
		releaseKey   string
	)

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	prevEpoch := state.CurrentEpoch
	if state.Status == StatusRunning && state.WorkerAppID != "" {
		needAbort = true
		abortAppID = state.WorkerAppID
		abortReq = daemon.AbortNodeRequest{
			RunID:    state.RunID,
			NodeAddr: state.NodeAddr,
			Epoch:    prevEpoch,
		}
	}
	releaseLease = state.LeaseID
	releaseKey = state.LeaseKey

	state.CurrentEpoch++
	clearLeaseLocked(&state)
	state.Status = StatusPending
	state.ManualPaused = false
	state.PendingPublication = nil
	state.Approval = nil
	state.Outcome = nil
	state.LastError = ""
	now := a.clock()
	invalidatePendingCardsLocked(&state, now, "superseded by rework")

	if state.Dispatch == nil {
		state.Dispatch = &DispatchSpec{
			RunID:    state.RunID,
			NodeAddr: state.NodeAddr,
		}
	}
	baseChanged := false
	if sha := strings.TrimSpace(req.BaseSHA); sha != "" {
		if sha != state.Dispatch.BaseSHA {
			baseChanged = true
		}
		state.Dispatch.BaseSHA = sha
	}
	if src := strings.TrimSpace(req.BasedSource); src != "" {
		if src != state.Dispatch.BasedSource {
			baseChanged = true
		}
		state.Dispatch.BasedSource = src
	}
	if req.ClearCheckpoints || baseChanged {
		state.Checkpoints = nil
		state.QCLedgerSnapshot = nil
	}

	if reason := strings.TrimSpace(req.Reason); reason != "" {
		appendInterjectionLocked(&state, "", "[Rework Required] "+reason, false, now)
	}

	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	resp := &AdvanceForReworkResp{
		Epoch:         state.CurrentEpoch,
		Status:        state.Status,
		AbortedWorker: needAbort,
	}
	dispatcher := a.workerDispatcher
	slotMgr := a.slotManager
	remSched := a.reminderScheduler
	notifier := a.runNotifier
	actorID := a.effectiveActorID()
	remName := a.reminderName
	runID, nodeAddr := state.RunID, state.NodeAddr
	a.mu.Unlock()

	if remSched != nil && actorID != "" {
		_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
	}
	if needAbort && dispatcher != nil {
		_, _ = dispatcher.AbortNode(ctx, abortAppID, abortReq)
	}
	if (releaseLease != "" || releaseKey != "") && slotMgr != nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
			LeaseID:  releaseLease,
			LeaseKey: releaseKey,
		})
	}
	_ = notifier
	_ = runID
	_ = nodeAddr

	return resp, nil
}

// ResetForRerun increments epoch, aborts any in-flight worker, and clears execution state for a fresh rerun.
func (a *NodeActor) ResetForRerun(ctx context.Context, req *ResetForRerunReq) (*ResetForRerunResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		req = &ResetForRerunReq{}
	}

	var (
		needAbort    bool
		abortAppID   string
		abortReq     daemon.AbortNodeRequest
		releaseLease string
		releaseKey   string
	)

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	prevEpoch := state.CurrentEpoch
	if state.Status == StatusRunning && state.WorkerAppID != "" {
		needAbort = true
		abortAppID = state.WorkerAppID
		abortReq = daemon.AbortNodeRequest{
			RunID:    state.RunID,
			NodeAddr: state.NodeAddr,
			Epoch:    prevEpoch,
		}
	}
	releaseLease = state.LeaseID
	releaseKey = state.LeaseKey

	state.CurrentEpoch++
	clearLeaseLocked(&state)
	state.Status = StatusPending
	state.ManualPaused = false
	state.Checkpoints = nil
	state.QCLedgerSnapshot = nil
	state.PendingPublication = nil
	state.Approval = nil
	state.Outcome = nil
	state.LastError = ""
	if req.ClearInterjections {
		state.PendingInterjections = nil
	}
	now := a.clock()
	invalidatePendingCardsLocked(&state, now, "reset for rerun")

	if state.Dispatch != nil {
		if req.ClearBaseSHA {
			state.Dispatch.BaseSHA = ""
		}
		if sha := strings.TrimSpace(req.BaseSHA); sha != "" {
			state.Dispatch.BaseSHA = sha
		}
		if src := strings.TrimSpace(req.BasedSource); src != "" {
			state.Dispatch.BasedSource = src
		}
	}

	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	resp := &ResetForRerunResp{
		Epoch:         state.CurrentEpoch,
		Status:        state.Status,
		AbortedWorker: needAbort,
	}
	dispatcher := a.workerDispatcher
	slotMgr := a.slotManager
	remSched := a.reminderScheduler
	notifier := a.runNotifier
	actorID := a.effectiveActorID()
	remName := a.reminderName
	runID, nodeAddr := state.RunID, state.NodeAddr
	a.mu.Unlock()

	if remSched != nil && actorID != "" {
		_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
	}
	if needAbort && dispatcher != nil {
		_, _ = dispatcher.AbortNode(ctx, abortAppID, abortReq)
	}
	if (releaseLease != "" || releaseKey != "") && slotMgr != nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
			LeaseID:  releaseLease,
			LeaseKey: releaseKey,
		})
	}
	_ = notifier
	_ = runID
	_ = nodeAddr

	return resp, nil
}

// ReportNodeOutcome enforces epoch fencing, releases slots, updates node status, and creates DecisionCards on WAITING_HUMAN.
func (a *NodeActor) ReportNodeOutcome(ctx context.Context, req *daemon.NodeOutcomeReport) (*ReportNodeOutcomeResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: outcome report is nil")
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	if req.Epoch != state.CurrentEpoch {
		resp := &ReportNodeOutcomeResp{
			Accepted:     false,
			StaleEpoch:   true,
			CurrentEpoch: state.CurrentEpoch,
			Status:       state.Status,
		}
		a.mu.Unlock()
		return resp, nil
	}

	if state.LeaseID != "" && req.LeaseID != "" && req.LeaseID != state.LeaseID {
		resp := &ReportNodeOutcomeResp{
			Accepted:     false,
			StaleEpoch:   true,
			CurrentEpoch: state.CurrentEpoch,
			Status:       state.Status,
		}
		a.mu.Unlock()
		return resp, nil
	}

	releaseLease := nonEmpty(req.LeaseID, state.LeaseID)
	releaseKey := nonEmpty(req.LeaseKey, state.LeaseKey)
	clearLeaseLocked(&state)

	if req.Aborted {
		_ = a.saveStateLocked(ctx, state)
		resp := &ReportNodeOutcomeResp{
			Accepted:       false,
			IgnoredAborted: true,
			CurrentEpoch:   state.CurrentEpoch,
			Status:         state.Status,
		}
		slotMgr := a.slotManager
		a.mu.Unlock()
		if (releaseLease != "" || releaseKey != "") && slotMgr != nil {
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
				LeaseID:  releaseLease,
				LeaseKey: releaseKey,
			})
		}
		return resp, nil
	}

	now := a.clock()
	var createdCard *DecisionCard

	switch req.Outcome.Status {
	case runner.StatusSucceeded:
		state.Status = StatusSucceeded
		state.ManualPaused = false
		state.Outcome = cloneNodeOutcome(&req.Outcome)
		state.LastError = ""
		invalidatePendingCardsLocked(&state, now, "node succeeded")

	case runner.StatusWaitingHuman:
		state.Status = StatusWaitingHuman
		state.Outcome = cloneNodeOutcome(&req.Outcome)
		state.LastError = ""
		invalidatePendingCardsLocked(&state, now, "superseded by new human pause")

		card := buildDecisionCard(state.RunID, state.NodeAddr, state.CurrentEpoch, req.Outcome, now)
		if card.Kind == DecisionKindHumanApproval {
			// Invalidate any stale approval whose (CommitSHA, ArtifactIDs) no longer matches the new output.
			if state.Approval != nil && (state.Approval.CommitSHA != card.CommitSHA || !slices.Equal(state.Approval.ArtifactIDs, card.ArtifactIDs)) {
				state.Approval = nil
				for i := range state.DecisionCards {
					if state.DecisionCards[i].Kind == DecisionKindHumanApproval &&
						state.DecisionCards[i].Status == DecisionStatusAnswered {
						state.DecisionCards[i].Status = DecisionStatusInvalidated
						state.DecisionCards[i].InvalidatedReason = "superseded by new commit/artifacts"
						state.DecisionCards[i].UpdatedAt = now
					}
				}
			}
		}
		state.DecisionCards = append(state.DecisionCards, card)
		copied := cloneDecisionCard(card)
		createdCard = &copied

	case runner.StatusFallbackTriggered:
		state.Status = StatusFallbackTriggered
		state.ManualPaused = false
		state.Outcome = cloneNodeOutcome(&req.Outcome)
		state.LastError = strings.TrimSpace(req.Outcome.FallbackReason)
		invalidatePendingCardsLocked(&state, now, "fallback triggered")

	default:
		state.Status = StatusFailed
		state.Outcome = cloneNodeOutcome(&req.Outcome)
		state.LastError = nonEmpty(req.Outcome.FailureReason, req.ErrMessage)
	}

	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}

	notify := NodeEventNotification{
		RunID:        state.RunID,
		NodeAddr:     state.NodeAddr,
		Epoch:        state.CurrentEpoch,
		Status:       state.Status,
		Outcome:      cloneNodeOutcome(state.Outcome),
		DecisionCard: createdCard,
		ErrorMessage: state.LastError,
		OccurredAt:   now,
	}
	resp := &ReportNodeOutcomeResp{
		Accepted:     true,
		CurrentEpoch: state.CurrentEpoch,
		Status:       state.Status,
		DecisionCard: createdCard,
	}
	slotMgr := a.slotManager
	notifier := a.runNotifier
	remSched := a.reminderScheduler
	actorID := a.effectiveActorID()
	remName := a.reminderName
	a.mu.Unlock()

	if remSched != nil && actorID != "" {
		_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
	}
	if (releaseLease != "" || releaseKey != "") && slotMgr != nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
			LeaseID:  releaseLease,
			LeaseKey: releaseKey,
		})
	}
	if notifier != nil {
		_ = notifier.NotifyNodeEvent(ctx, notify)
	}
	return resp, nil
}

// SubmitDecision validates a pending DecisionCard (including exact (CommitSHA, ArtifactIDs) binding for human_approval),
// persists the human decision/interjection, and auto-resumes ONLY if the node and Run are not manually paused.
func (a *NodeActor) SubmitDecision(ctx context.Context, req *SubmitDecisionReq) (*SubmitDecisionResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: submit decision request is nil")
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	cardIdx, err := findPendingDecisionCardIndex(state.DecisionCards, req.CardID)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	card := state.DecisionCards[cardIdx]
	now := a.clock()

	switch card.Kind {
	case DecisionKindHumanApproval:
		if strings.TrimSpace(req.CommitSHA) != "" && req.CommitSHA != card.CommitSHA {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: commit_sha mismatch: card=%s got=%s", ErrInvalidDecision, card.CommitSHA, req.CommitSHA)
		}
		if req.ArtifactIDs != nil && !slices.Equal(req.ArtifactIDs, card.ArtifactIDs) {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: artifact_ids mismatch: card=%v got=%v", ErrInvalidDecision, card.ArtifactIDs, req.ArtifactIDs)
		}
		if state.PendingPublication != nil &&
			(state.PendingPublication.CommitSHA != card.CommitSHA || !slices.Equal(state.PendingPublication.ArtifactIDs, card.ArtifactIDs)) {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: card %s is superseded by pending publication commit %s", ErrInvalidDecision, card.CardID, state.PendingPublication.CommitSHA)
		}

		approved := false
		if req.Approved != nil {
			approved = *req.Approved
		}
		if !approved && !req.ReturnedUpstream && strings.TrimSpace(req.AnswerText) == "" {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: human_approval requires approved=true, returned_upstream=true, or rejection feedback in answer_text", ErrInvalidDecision)
		}
		if approved && req.ReturnedUpstream {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: approved and returned_upstream cannot both be true", ErrInvalidDecision)
		}

		card.Approved = &approved
		card.ReturnedUpstream = req.ReturnedUpstream
		card.ReturnFallback = strings.TrimSpace(req.ReturnFallback)
		card.AnswerText = strings.TrimSpace(req.AnswerText)
		card.Status = DecisionStatusAnswered
		card.UpdatedAt = now
		card.AnsweredAt = &now
		state.DecisionCards[cardIdx] = card

		state.Approval = &runner.ApprovalRecord{
			Approved:         approved,
			ReturnedUpstream: req.ReturnedUpstream,
			ReturnFallback:   strings.TrimSpace(req.ReturnFallback),
			CommitSHA:        card.CommitSHA,
			ArtifactIDs:      slices.Clone(card.ArtifactIDs),
		}
		if !approved && !req.ReturnedUpstream {
			// Inject operator revision instructions so WorkerRunner runs a rework Work turn instead of looping.
			appendInterjectionLocked(&state, "", "[Human Approval Rejected] "+strings.TrimSpace(req.AnswerText), false, now)
		}

	case DecisionKindRequestDecision, DecisionKindPauseForHuman:
		selected := strings.TrimSpace(req.SelectedOption)
		answer := strings.TrimSpace(req.AnswerText)
		if selected == "" && answer == "" {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: selected_option or answer_text is required", ErrInvalidDecision)
		}
		card.SelectedOption = selected
		card.AnswerText = answer
		card.Status = DecisionStatusAnswered
		card.UpdatedAt = now
		card.AnsweredAt = &now
		state.DecisionCards[cardIdx] = card

		msg := formatDecisionInterjection(card)
		appendInterjectionLocked(&state, "", msg, false, now)
		confirmAwaitingInterjectionsLocked(&state, now)

	default:
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: unsupported card kind %q", ErrInvalidDecision, card.Kind)
	}

	isManuallyPaused := state.ManualPaused || state.Status == StatusPaused || req.RunPaused
	answeredCard := cloneDecisionCard(card)

	if isManuallyPaused {
		if err := a.saveStateLocked(ctx, state); err != nil {
			a.mu.Unlock()
			return nil, err
		}
		notifier := a.runNotifier
		notify := NodeEventNotification{
			RunID:        state.RunID,
			NodeAddr:     state.NodeAddr,
			Epoch:        state.CurrentEpoch,
			Status:       state.Status,
			DecisionCard: &answeredCard,
			OccurredAt:   now,
		}
		resp := &SubmitDecisionResp{
			Accepted:    true,
			Card:        &answeredCard,
			Epoch:       state.CurrentEpoch,
			Status:      state.Status,
			AutoResumed: false,
		}
		a.mu.Unlock()
		if notifier != nil {
			_ = notifier.NotifyNodeEvent(ctx, notify)
		}
		return resp, nil
	}

	state.CurrentEpoch++
	clearLeaseLocked(&state)
	state.Status = StatusQueuedSlot
	newEpoch := state.CurrentEpoch
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	notifier := a.runNotifier
	notify := NodeEventNotification{
		RunID:        state.RunID,
		NodeAddr:     state.NodeAddr,
		Epoch:        newEpoch,
		Status:       StatusQueuedSlot,
		DecisionCard: &answeredCard,
		OccurredAt:   now,
	}
	a.mu.Unlock()

	if notifier != nil {
		_ = notifier.NotifyNodeEvent(ctx, notify)
	}

	dispResp, dispErr := a.dispatchCurrentEpoch(ctx, newEpoch, true)
	if dispErr != nil {
		return &SubmitDecisionResp{
			Accepted:    true,
			Card:        &answeredCard,
			Epoch:       newEpoch,
			Status:      StatusQueuedSlot,
			AutoResumed: false,
		}, dispErr
	}
	return &SubmitDecisionResp{
		Accepted:    true,
		Card:        &answeredCard,
		Epoch:       dispResp.Epoch,
		Status:      dispResp.Status,
		AutoResumed: true,
	}, nil
}

// GetState returns a deep copy of the NodeActor's authoritative persisted state.
func (a *NodeActor) GetState(ctx context.Context, _ *GetNodeStateReq) (*GetNodeStateResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	ckpts := cloneCheckpointRecords(state.Checkpoints)
	sort.Slice(ckpts, func(i, j int) bool { return ckpts[i].Seq > ckpts[j].Seq })

	return &GetNodeStateResp{
		RunID:                state.RunID,
		NodeAddr:             state.NodeAddr,
		CurrentEpoch:         state.CurrentEpoch,
		Status:               state.Status,
		ManualPaused:         state.ManualPaused,
		LeaseID:              state.LeaseID,
		LeaseKey:             state.LeaseKey,
		WorkerProcessID:      state.WorkerProcessID,
		WorkerAppID:          state.WorkerAppID,
		LastError:            state.LastError,
		Checkpoints:          ckpts,
		PendingInterjections: slices.Clone(state.PendingInterjections),
		InterjectionLog:      cloneInterjectionLog(state.InterjectionLog),
		DecisionCards:        cloneDecisionCards(state.DecisionCards),
		PendingPublication:   clonePendingPublication(state.PendingPublication),
		Approval:             cloneApprovalRecord(state.Approval),
		Outcome:              cloneNodeOutcome(state.Outcome),
	}, nil
}

// RegisterCheckpointBranch enforces epoch fencing and registers an e<epoch> checkpoint branch via RunNotifier before push.
func (a *NodeActor) RegisterCheckpointBranch(ctx context.Context, req *RegisterCheckpointBranchReq) (*EpochFencedResp, error) {
	return a.registerManagedBranch(ctx, req.Epoch, req.RepoURL, req.Branch, req.BaseSHA, git.KindCheckpointBranch)
}

// RegisterWorkBranch enforces epoch fencing and registers the node's final work_branch via RunNotifier before push.
func (a *NodeActor) RegisterWorkBranch(ctx context.Context, req *RegisterWorkBranchReq) (*EpochFencedResp, error) {
	return a.registerManagedBranch(ctx, req.Epoch, req.RepoURL, req.Branch, req.BaseSHA, git.KindWorkBranch)
}

func (a *NodeActor) registerManagedBranch(ctx context.Context, reqEpoch uint64, repoURL, branch, baseSHA string, kind git.BranchKind) (*EpochFencedResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(repoURL) == "" {
		return errorResp(errCodeInvalidInput, "repository url is required"), nil
	}
	cleanBranch, err := git.NormalizeBranchName(branch)
	if err != nil {
		return errorResp(errCodeInvalidInput, err.Error()), nil
	}
	if !git.IsValidCommitSHA(baseSHA) {
		return errorResp(errCodeInvalidBaseSHA, baseSHA), nil
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if reqEpoch != state.CurrentEpoch {
		resp := staleEpochResp(state.CurrentEpoch)
		a.mu.Unlock()
		return resp, nil
	}

	branchMap := state.RegisteredWorkBranches
	if kind == git.KindCheckpointBranch {
		branchMap = state.RegisteredCheckpointBranches
	}
	key := branchRegistrationKey(repoURL, cleanBranch)
	if existing, ok := branchMap[key]; ok {
		if reqEpoch < existing.Epoch {
			resp := staleEpochResp(state.CurrentEpoch)
			a.mu.Unlock()
			return resp, nil
		}
		if existing.Epoch == reqEpoch && existing.BaseSHA != baseSHA {
			a.mu.Unlock()
			return errorResp(errCodeConflict, fmt.Sprintf("branch %q already registered at epoch %d with base_sha %s (got %s)",
				cleanBranch, reqEpoch, existing.BaseSHA, baseSHA)), nil
		}
	}
	runID := state.RunID
	nodeAddr := state.NodeAddr
	notifier := a.runNotifier
	a.mu.Unlock()

	if notifier != nil {
		if regErr := notifier.RegisterManagedBranch(ctx, RegisterManagedBranchReq{
			RunID:    runID,
			NodeAddr: nodeAddr,
			Epoch:    reqEpoch,
			RepoURL:  strings.TrimSpace(repoURL),
			Branch:   cleanBranch,
			BaseSHA:  baseSHA,
			Kind:     kind,
		}); regErr != nil {
			return mapDomainErrToResp(regErr), nil
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	state, err = a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if reqEpoch != state.CurrentEpoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}

	entry := registeredBranchEntry{
		RepoURL: strings.TrimSpace(repoURL),
		Branch:  cleanBranch,
		BaseSHA: baseSHA,
		Epoch:   reqEpoch,
	}
	if kind == git.KindCheckpointBranch {
		if state.RegisteredCheckpointBranches == nil {
			state.RegisteredCheckpointBranches = make(map[string]registeredBranchEntry)
		}
		state.RegisteredCheckpointBranches[key] = entry
	} else {
		if state.RegisteredWorkBranches == nil {
			state.RegisteredWorkBranches = make(map[string]registeredBranchEntry)
		}
		state.RegisteredWorkBranches[key] = entry
	}
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &EpochFencedResp{OK: true, CurrentEpoch: state.CurrentEpoch}, nil
}

// BeginCheckpoint verifies req.Epoch == CurrentEpoch, allocates a monotonic Seq, and persists a StatusPending record.
func (a *NodeActor) BeginCheckpoint(ctx context.Context, req *BeginCheckpointReq) (*BeginCheckpointResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: begin checkpoint request is nil")
	}
	cleanBranch, err := git.NormalizeBranchName(req.Branch)
	if err != nil {
		return &BeginCheckpointResp{ErrorCode: errCodeInvalidInput, ErrorMessage: err.Error()}, nil
	}
	if !git.IsValidCommitSHA(req.InitialSHA) {
		return &BeginCheckpointResp{ErrorCode: errCodeInvalidBaseSHA, ErrorMessage: "initial_sha " + req.InitialSHA}, nil
	}
	if err := req.Diary.Validate(); err != nil {
		return &BeginCheckpointResp{ErrorCode: errCodeInvalidInput, ErrorMessage: err.Error()}, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if req.Epoch != state.CurrentEpoch {
		return &BeginCheckpointResp{
			StaleEpoch:   true,
			CurrentEpoch: state.CurrentEpoch,
			ErrorCode:    errCodeStaleEpoch,
		}, nil
	}

	seq := state.NextCheckpointSeq
	state.NextCheckpointSeq++
	state.Checkpoints = append(state.Checkpoints, checkpoint.CheckpointRecord{
		Seq:        seq,
		Epoch:      req.Epoch,
		Branch:     cleanBranch,
		InitialSHA: req.InitialSHA,
		Diary:      cloneDiaryInput(req.Diary),
		Status:     checkpoint.StatusPending,
	})
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &BeginCheckpointResp{Seq: seq, CurrentEpoch: state.CurrentEpoch}, nil
}

// AppendFinalSHA verifies req.Epoch == CurrentEpoch and appends a new Git commit SHA onto a pending checkpoint.
func (a *NodeActor) AppendFinalSHA(ctx context.Context, req *AppendFinalSHAReq) (*EpochFencedResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: append final sha request is nil")
	}
	if !git.IsValidCommitSHA(req.FinalSHA) {
		return errorResp(errCodeInvalidBaseSHA, "final_sha "+req.FinalSHA), nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if req.Epoch != state.CurrentEpoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}
	idx := findCheckpointIndex(state.Checkpoints, req.Seq)
	if idx < 0 {
		return errorResp(errCodeNotFound, fmt.Sprintf("checkpoint seq %d not found", req.Seq)), nil
	}
	if state.Checkpoints[idx].Epoch != req.Epoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}
	if state.Checkpoints[idx].Status != checkpoint.StatusPending {
		return errorResp(errCodeConflict, fmt.Sprintf("checkpoint seq %d has status %s, expected %s", req.Seq, state.Checkpoints[idx].Status, checkpoint.StatusPending)), nil
	}

	state.Checkpoints[idx].FinalSHA = req.FinalSHA
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &EpochFencedResp{OK: true, CurrentEpoch: state.CurrentEpoch}, nil
}

// CompleteCheckpoint verifies req.Epoch == CurrentEpoch, records ArtifactsSnapshot, and marks StatusComplete.
func (a *NodeActor) CompleteCheckpoint(ctx context.Context, req *CompleteCheckpointReq) (*EpochFencedResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: complete checkpoint request is nil")
	}
	if !git.IsValidCommitSHA(req.FinalSHA) {
		return errorResp(errCodeInvalidBaseSHA, "final_sha "+req.FinalSHA), nil
	}
	if req.ArtifactsSnapshot != nil {
		if err := blob.ValidateKey(req.ArtifactsSnapshot.Key); err != nil {
			return errorResp(errCodeBlobInvalidKey, err.Error()), nil
		}
		if !blob.IsValidSHA256Hex(req.ArtifactsSnapshot.SHA256) {
			return errorResp(errCodeBlobDigestMismatch, fmt.Sprintf("invalid snapshot sha256 %q", req.ArtifactsSnapshot.SHA256)), nil
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if req.Epoch != state.CurrentEpoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}
	idx := findCheckpointIndex(state.Checkpoints, req.Seq)
	if idx < 0 {
		return errorResp(errCodeNotFound, fmt.Sprintf("checkpoint seq %d not found", req.Seq)), nil
	}
	if state.Checkpoints[idx].Epoch != req.Epoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}
	if state.Checkpoints[idx].Status != checkpoint.StatusPending {
		return errorResp(errCodeConflict, fmt.Sprintf("checkpoint seq %d has status %s, expected %s", req.Seq, state.Checkpoints[idx].Status, checkpoint.StatusPending)), nil
	}
	if state.Checkpoints[idx].FinalSHA != "" && state.Checkpoints[idx].FinalSHA != req.FinalSHA {
		return errorResp(errCodeConflict, fmt.Sprintf("checkpoint seq %d final_sha mismatch: recorded %s, got %s", req.Seq, state.Checkpoints[idx].FinalSHA, req.FinalSHA)), nil
	}

	state.Checkpoints[idx].FinalSHA = req.FinalSHA
	state.Checkpoints[idx].ArtifactsSnapshot = cloneObjectMeta(req.ArtifactsSnapshot)
	state.Checkpoints[idx].Status = checkpoint.StatusComplete
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &EpochFencedResp{OK: true, CurrentEpoch: state.CurrentEpoch}, nil
}

// ListCheckpoints returns deep copies of all checkpoint records ordered newest-to-oldest (Seq descending).
func (a *NodeActor) ListCheckpoints(ctx context.Context, _ *ListCheckpointsReq) (*ListCheckpointsResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	out := cloneCheckpointRecords(state.Checkpoints)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Seq > out[j].Seq
	})
	return &ListCheckpointsResp{Records: out}, nil
}

// EnqueueInterjection appends a live human interjection to the FIFO queue and audit log.
func (a *NodeActor) EnqueueInterjection(ctx context.Context, req *EnqueueInterjectionReq) (*EnqueueInterjectionResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: enqueue interjection request is nil")
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		return nil, errors.New("node: interjection message is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	rec := appendInterjectionLocked(&state, req.ID, msg, req.WaitForConfirmation, a.clock())
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &EnqueueInterjectionResp{
		ID:                  rec.ID,
		WaitForConfirmation: rec.WaitForConfirmation,
		Status:              rec.Status,
		QueuedCount:         len(state.PendingInterjections),
	}, nil
}

// DrainInterjections atomically drains pending human interjections; when req.Epoch > 0, enforces epoch fencing.
func (a *NodeActor) DrainInterjections(ctx context.Context, req *DrainInterjectionsReq) (*DrainInterjectionsResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if req != nil && req.Epoch > 0 && req.Epoch != state.CurrentEpoch {
		return &DrainInterjectionsResp{
			StaleEpoch:   true,
			CurrentEpoch: state.CurrentEpoch,
		}, nil
	}
	if len(state.PendingInterjections) == 0 {
		return &DrainInterjectionsResp{CurrentEpoch: state.CurrentEpoch}, nil
	}

	drained := slices.Clone(state.PendingInterjections)
	state.PendingInterjections = nil
	now := a.clock()
	drainedIDs := make(map[string]struct{}, len(drained))
	for _, item := range drained {
		drainedIDs[item.ID] = struct{}{}
	}
	for i := range state.InterjectionLog {
		if _, ok := drainedIDs[state.InterjectionLog[i].ID]; ok {
			state.InterjectionLog[i].DrainedAt = &now
			if state.InterjectionLog[i].WaitForConfirmation {
				state.InterjectionLog[i].Status = InterjectionStatusAwaitingConfirmation
			} else {
				state.InterjectionLog[i].Status = InterjectionStatusDrained
			}
		}
	}
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &DrainInterjectionsResp{
		Items:        drained,
		CurrentEpoch: state.CurrentEpoch,
	}, nil
}

// HasPendingInterjections reports whether undrained interjections remain in the queue.
func (a *NodeActor) HasPendingInterjections(ctx context.Context, _ *HasPendingInterjectionsReq) (*HasPendingInterjectionsResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	return &HasPendingInterjectionsResp{HasPending: len(state.PendingInterjections) > 0}, nil
}

// SaveQCLedgerSnapshot persists the cross-round QC ledger snapshot under strict epoch fencing.
func (a *NodeActor) SaveQCLedgerSnapshot(ctx context.Context, req *SaveQCLedgerSnapshotReq) (*EpochFencedResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: save qc ledger snapshot request is nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if req.Epoch != state.CurrentEpoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}
	state.QCLedgerSnapshot = append([]byte(nil), req.Snapshot...)
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &EpochFencedResp{OK: true, CurrentEpoch: state.CurrentEpoch}, nil
}

// LoadQCLedgerSnapshot returns a defensive copy of the saved QC ledger snapshot.
func (a *NodeActor) LoadQCLedgerSnapshot(ctx context.Context, _ *LoadQCLedgerSnapshotReq) (*LoadQCLedgerSnapshotResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	return &LoadQCLedgerSnapshotResp{Snapshot: append([]byte(nil), state.QCLedgerSnapshot...)}, nil
}

// SavePendingPublication persists or clears version-bound approval target metadata under strict epoch fencing.
func (a *NodeActor) SavePendingPublication(ctx context.Context, req *SavePendingPublicationReq) (*EpochFencedResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("node: save pending publication request is nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if req.Epoch != state.CurrentEpoch {
		return staleEpochResp(state.CurrentEpoch), nil
	}
	state.PendingPublication = clonePendingPublication(req.Pending)
	if err := a.saveStateLocked(ctx, state); err != nil {
		return nil, err
	}
	return &EpochFencedResp{OK: true, CurrentEpoch: state.CurrentEpoch}, nil
}

// LoadPendingPublication returns a defensive copy of any persisted PendingPublication.
func (a *NodeActor) LoadPendingPublication(ctx context.Context, _ *LoadPendingPublicationReq) (*LoadPendingPublicationResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	return &LoadPendingPublicationResp{Pending: clonePendingPublication(state.PendingPublication)}, nil
}

// GetApproval returns a defensive copy of the persisted human ApprovalRecord, if any.
func (a *NodeActor) GetApproval(ctx context.Context, _ *GetApprovalReq) (*GetApprovalResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	return &GetApprovalResp{Approval: cloneApprovalRecord(state.Approval)}, nil
}

func (a *NodeActor) dispatchCurrentEpoch(ctx context.Context, expectedEpoch uint64, notifyRun bool) (*StartOrResumeResp, error) {
	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if state.CurrentEpoch != expectedEpoch || state.ManualPaused || state.Dispatch == nil || isTerminalStatus(state.Status) {
		resp := &StartOrResumeResp{
			Accepted:        false,
			Epoch:           state.CurrentEpoch,
			Status:          state.Status,
			LeaseID:         state.LeaseID,
			WorkerProcessID: state.WorkerProcessID,
			WorkerAppID:     state.WorkerAppID,
		}
		a.mu.Unlock()
		return resp, nil
	}

	spec := cloneDispatchSpec(state.Dispatch)
	runID := state.RunID
	nodeAddr := state.NodeAddr
	actorID := a.effectiveActorID()
	cfgResolver := a.configResolver
	slotMgr := a.slotManager
	dispatcher := a.workerDispatcher
	remSched := a.reminderScheduler
	notifier := a.runNotifier
	if !notifyRun {
		notifier = nil
	}
	retryInterval := a.slotRetryInterval
	remName := a.reminderName
	a.mu.Unlock()

	// Step 1: Resolve minimal managed env + Git credentials outside actor mutex.
	keys := collectRequiredKeys(spec)
	var resolvedEnv map[string]string
	if len(keys) > 0 {
		if cfgResolver == nil {
			return a.failDispatch(ctx, expectedEpoch, notifyRun, "node: config resolver unavailable for required keys")
		}
		resResp, resErr := cfgResolver.ResolveForNode(ctx, &config.ResolveForNodeReq{
			RunID:        runID,
			Namespace:    spec.Namespace,
			RunOverrides: maps.Clone(spec.RunOverrides),
			Keys:         keys,
		})
		if resErr != nil {
			return a.failDispatch(ctx, expectedEpoch, notifyRun, fmt.Sprintf("resolve node credentials: %v", resErr))
		}
		if resResp == nil || len(resResp.MissingKeys) > 0 {
			missing := []string(nil)
			if resResp != nil {
				missing = resResp.MissingKeys
			}
			return a.failDispatch(ctx, expectedEpoch, notifyRun, fmt.Sprintf("missing required managed env/secret keys: %s", strings.Join(missing, ", ")))
		}
		resolvedEnv = maps.Clone(resResp.Values)
	}

	// Step 2: Claim least-load worker slot outside actor mutex.
	if slotMgr == nil {
		return a.failDispatch(ctx, expectedEpoch, notifyRun, "node: slot manager unavailable")
	}
	leaseKey := fmt.Sprintf("%s/%s/e%d", runID, nodeAddr, expectedEpoch)
	claimResp, claimErr := slotMgr.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       leaseKey,
		OwnerActorType: ActorType,
		OwnerActorID:   actorID,
		RequiredArch:   spec.RequiredArch,
	})
	if claimErr != nil || claimResp == nil || !claimResp.Granted {
		reason := "no healthy worker with available capacity"
		if claimErr != nil {
			reason = claimErr.Error()
		} else if claimResp != nil && claimResp.Reason != "" {
			reason = claimResp.Reason
		}
		return a.enterQueuedSlot(ctx, expectedEpoch, reason, remSched, notifier, actorID, remName, retryInterval)
	}

	// Step 3: Commit RUNNING + lease info BEFORE calling StartNode so concurrent Outcome callbacks match.
	a.mu.Lock()
	state, err = a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
		return nil, err
	}
	if state.CurrentEpoch != expectedEpoch || state.ManualPaused || isTerminalStatus(state.Status) {
		resp := &StartOrResumeResp{
			Accepted: false,
			Epoch:    state.CurrentEpoch,
			Status:   state.Status,
		}
		a.mu.Unlock()
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
		return resp, nil
	}

	state.Status = StatusRunning
	state.LeaseID = claimResp.LeaseID
	state.LeaseKey = claimResp.LeaseKey
	state.WorkerProcessID = claimResp.ProcessID
	state.WorkerAppID = claimResp.AppID
	state.LastError = ""
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
		return nil, err
	}
	a.mu.Unlock()

	if dispatcher == nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
		return a.failDispatch(ctx, expectedEpoch, notifyRun, "node: worker dispatcher unavailable")
	}

	startReq := buildDaemonStartNodeRequest(spec, expectedEpoch, claimResp, resolvedEnv)
	startResp, startErr := dispatcher.StartNode(ctx, claimResp.AppID, startReq)
	if startErr != nil || !startResp.Accepted {
		errMsg := "worker rejected start_node"
		if startErr != nil {
			errMsg = startErr.Error()
		}
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
		_, _ = slotMgr.ReportSuspect(ctx, &registry.ReportSuspectReq{ProcessID: claimResp.ProcessID, Reason: errMsg})
		return a.enterQueuedSlot(ctx, expectedEpoch, errMsg, remSched, notifier, actorID, remName, retryInterval)
	}

	if remSched != nil && actorID != "" {
		_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
	}
	if notifier != nil {
		_ = notifier.NotifyNodeEvent(ctx, NodeEventNotification{
			RunID:      runID,
			NodeAddr:   nodeAddr,
			Epoch:      expectedEpoch,
			Status:     StatusRunning,
			OccurredAt: a.clock(),
		})
	}

	return &StartOrResumeResp{
		Accepted:        true,
		Epoch:           expectedEpoch,
		Status:          StatusRunning,
		LeaseID:         claimResp.LeaseID,
		WorkerProcessID: claimResp.ProcessID,
		WorkerAppID:     claimResp.AppID,
	}, nil
}

func (a *NodeActor) enterQueuedSlot(
	ctx context.Context,
	expectedEpoch uint64,
	reason string,
	remSched ReminderScheduler,
	notifier RunNotifier,
	actorID, remName string,
	retryInterval time.Duration,
) (*StartOrResumeResp, error) {
	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if state.CurrentEpoch != expectedEpoch || state.ManualPaused || isTerminalStatus(state.Status) {
		resp := &StartOrResumeResp{
			Accepted: false,
			Epoch:    state.CurrentEpoch,
			Status:   state.Status,
		}
		a.mu.Unlock()
		return resp, nil
	}
	clearLeaseLocked(&state)
	state.Status = StatusQueuedSlot
	state.LastError = reason
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	runID, nodeAddr := state.RunID, state.NodeAddr
	a.mu.Unlock()

	if remSched != nil && actorID != "" {
		_ = remSched.RegisterSlotRetryReminder(ctx, ActorType, actorID, remName, retryInterval, retryInterval)
	}
	if notifier != nil {
		_ = notifier.NotifyNodeEvent(ctx, NodeEventNotification{
			RunID:        runID,
			NodeAddr:     nodeAddr,
			Epoch:        expectedEpoch,
			Status:       StatusQueuedSlot,
			ErrorMessage: reason,
			OccurredAt:   a.clock(),
		})
	}
	return &StartOrResumeResp{
		Accepted:     true,
		Epoch:        expectedEpoch,
		Status:       StatusQueuedSlot,
		ErrorMessage: reason,
	}, nil
}

func (a *NodeActor) failDispatch(ctx context.Context, expectedEpoch uint64, notifyRun bool, reason string) (*StartOrResumeResp, error) {
	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if state.CurrentEpoch != expectedEpoch {
		resp := &StartOrResumeResp{
			Accepted: false,
			Epoch:    state.CurrentEpoch,
			Status:   state.Status,
		}
		a.mu.Unlock()
		return resp, nil
	}
	clearLeaseLocked(&state)
	state.Status = StatusFailed
	state.LastError = reason
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	notifier := a.runNotifier
	runID, nodeAddr := state.RunID, state.NodeAddr
	a.mu.Unlock()

	if notifyRun && notifier != nil {
		_ = notifier.NotifyNodeEvent(ctx, NodeEventNotification{
			RunID:        runID,
			NodeAddr:     nodeAddr,
			Epoch:        expectedEpoch,
			Status:       StatusFailed,
			ErrorMessage: reason,
			OccurredAt:   a.clock(),
		})
	}
	return &StartOrResumeResp{
		Accepted:     false,
		Epoch:        expectedEpoch,
		Status:       StatusFailed,
		ErrorMessage: reason,
	}, nil
}

func (a *NodeActor) effectiveActorID() string {
	return strings.TrimSpace(a.ID())
}

func (a *NodeActor) loadStateLocked(ctx context.Context) (persistedNodeState, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return persistedNodeState{}, errors.New("node: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	actorID := a.effectiveActorID()
	exists, err := sm.Contains(ctx, nodeStateKey)
	if err != nil {
		return persistedNodeState{}, fmt.Errorf("node: check actor state: %w", err)
	}
	if !exists {
		return defaultNodeState(actorID), nil
	}
	var doc persistedNodeState
	if err := sm.Get(ctx, nodeStateKey, &doc); err != nil {
		return persistedNodeState{}, fmt.Errorf("node: load actor state: %w", err)
	}
	doc.ensureDefaults(actorID)
	return doc.clone(), nil
}

func (a *NodeActor) saveStateLocked(ctx context.Context, state persistedNodeState) error {
	sm := a.GetStateManager()
	if sm == nil {
		return errors.New("node: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := sm.Set(ctx, nodeStateKey, state.clone()); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("node: stage actor state: %w", err)
	}
	if err := sm.Save(ctx); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("node: save actor state: %w", err)
	}
	return nil
}

func mergeDispatchSpec(prev *DispatchSpec, runID, nodeAddr string, req *StartOrResumeReq) *DispatchSpec {
	out := &DispatchSpec{
		RunID:    runID,
		NodeAddr: nodeAddr,
	}
	if prev != nil {
		out = cloneDispatchSpec(prev)
		out.RunID = runID
		out.NodeAddr = nodeAddr
	}
	if ns := strings.TrimSpace(req.Namespace); ns != "" {
		out.Namespace = ns
	}
	if req.RunOverrides != nil {
		out.RunOverrides = maps.Clone(req.RunOverrides)
	}
	if req.RequiredEnvKeys != nil {
		out.RequiredEnvKeys = slices.Clone(req.RequiredEnvKeys)
	}
	if req.TokenRef != nil {
		out.TokenRef = cloneStrPtr(req.TokenRef)
		out.SSHKeyRef = nil
	} else if req.SSHKeyRef != nil {
		out.SSHKeyRef = cloneStrPtr(req.SSHKeyRef)
		out.TokenRef = nil
	} else if req.Auth.TokenRef != nil {
		out.TokenRef = cloneStrPtr(req.Auth.TokenRef)
		out.SSHKeyRef = nil
	} else if req.Auth.SSHKeyRef != nil {
		out.SSHKeyRef = cloneStrPtr(req.Auth.SSHKeyRef)
		out.TokenRef = nil
	}
	if s := strings.TrimSpace(req.RepoURL); s != "" {
		out.RepoURL = s
	}
	if s := strings.TrimSpace(req.BasedSource); s != "" {
		out.BasedSource = s
	}
	if s := strings.TrimSpace(req.BaseSHA); s != "" {
		out.BaseSHA = s
	}
	if s := strings.TrimSpace(req.RequiredArch); s != "" {
		out.RequiredArch = s
	}
	if !isZeroWorkerConfig(req.Worker) {
		out.Worker = req.Worker
	}
	if req.SystemPrompt != "" {
		out.SystemPrompt = req.SystemPrompt
	}
	if req.Prompt != "" {
		out.Prompt = req.Prompt
	}
	if req.QCPrompt != "" {
		out.QCPrompt = req.QCPrompt
	}
	if req.Sandboxes != nil {
		out.Sandboxes = maps.Clone(req.Sandboxes)
	}
	if req.Harnesses != nil {
		out.Harnesses = maps.Clone(req.Harnesses)
	}
	if req.ModelProviders != nil {
		out.ModelProviders = maps.Clone(req.ModelProviders)
	}
	if req.MCPServers != nil {
		out.MCPServers = slices.Clone(req.MCPServers)
	}
	if req.MCPResources != nil {
		out.MCPResources = slices.Clone(req.MCPResources)
	}
	if req.Tools != nil {
		out.Tools = maps.Clone(req.Tools)
	}
	if req.ToolEnvs != nil {
		out.ToolEnvs = maps.Clone(req.ToolEnvs)
	}
	if req.Imports != nil {
		out.Imports = slices.Clone(req.Imports)
	}
	if req.BundleDir != "" {
		out.BundleDir = req.BundleDir
	}
	if req.TempRoot != "" {
		out.TempRoot = req.TempRoot
	}
	if req.HostOS != "" {
		out.HostOS = req.HostOS
	}
	if req.TargetPlatform != nil {
		p := *req.TargetPlatform
		out.TargetPlatform = &p
	}
	if req.PluginResolve.PluginDir != "" || req.PluginResolve.CacheRoot != "" || req.PluginResolve.WorkingDir != "" || len(req.PluginResolve.LockFile.Plugins) > 0 {
		out.PluginResolve = req.PluginResolve
	}
	if req.MaxGatekeeperReworks > 0 {
		out.MaxGatekeeperReworks = req.MaxGatekeeperReworks
	}
	if req.MaxArtifactReworks > 0 {
		out.MaxArtifactReworks = req.MaxArtifactReworks
	}
	if req.StallTimeout > 0 {
		out.StallTimeout = req.StallTimeout
	}
	if req.WatchdogCheckInterval > 0 {
		out.WatchdogCheckInterval = req.WatchdogCheckInterval
	}
	return out
}

func isZeroWorkerConfig(w workerspec.Config) bool {
	return w.HumanApproval == "" &&
		w.Model == "" &&
		w.Profile == "" &&
		w.ReasoningEffort == "" &&
		w.Permission == "" &&
		w.Sandbox == cty.NilVal &&
		w.Harness == cty.NilVal &&
		w.SystemPrompt == cty.NilVal &&
		w.Prompt == cty.NilVal &&
		len(w.WritablePaths) == 0 &&
		len(w.Artifacts) == 0 &&
		len(w.Gatekeepers) == 0 &&
		len(w.Fallbacks) == 0 &&
		w.QC == nil
}

func collectRequiredKeys(spec *DispatchSpec) []string {
	if spec == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(spec.RequiredEnvKeys)+2)
	for _, k := range spec.RequiredEnvKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			seen[trimmed] = struct{}{}
		}
	}
	if spec.TokenRef != nil {
		if trimmed := strings.TrimSpace(*spec.TokenRef); trimmed != "" {
			seen[trimmed] = struct{}{}
		}
	}
	if spec.SSHKeyRef != nil {
		if trimmed := strings.TrimSpace(*spec.SSHKeyRef); trimmed != "" {
			seen[trimmed] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func buildDaemonStartNodeRequest(spec *DispatchSpec, epoch uint64, claim *registry.ClaimSlotResp, resolvedEnv map[string]string) daemon.StartNodeRequest {
	auth := git.AuthConfig{
		TokenRef:  cloneStrPtr(spec.TokenRef),
		SSHKeyRef: cloneStrPtr(spec.SSHKeyRef),
	}
	return daemon.StartNodeRequest{
		ProcessID:             claim.ProcessID,
		LeaseID:               claim.LeaseID,
		LeaseKey:              claim.LeaseKey,
		RunID:                 spec.RunID,
		NodeAddr:              spec.NodeAddr,
		Epoch:                 epoch,
		RepoURL:               spec.RepoURL,
		BasedSource:           spec.BasedSource,
		BaseSHA:               spec.BaseSHA,
		Auth:                  auth,
		Worker:                spec.Worker,
		SystemPrompt:          spec.SystemPrompt,
		Prompt:                spec.Prompt,
		QCPrompt:              spec.QCPrompt,
		Sandboxes:             maps.Clone(spec.Sandboxes),
		Harnesses:             maps.Clone(spec.Harnesses),
		ModelProviders:        maps.Clone(spec.ModelProviders),
		MCPServers:            slices.Clone(spec.MCPServers),
		MCPResources:          slices.Clone(spec.MCPResources),
		Tools:                 maps.Clone(spec.Tools),
		ToolEnvs:              maps.Clone(spec.ToolEnvs),
		Imports:               slices.Clone(spec.Imports),
		ResolvedEnv:           maps.Clone(resolvedEnv),
		BundleDir:             spec.BundleDir,
		TempRoot:              spec.TempRoot,
		HostOS:                spec.HostOS,
		TargetPlatform:        spec.TargetPlatform,
		PluginResolve:         spec.PluginResolve,
		MaxGatekeeperReworks:  spec.MaxGatekeeperReworks,
		MaxArtifactReworks:    spec.MaxArtifactReworks,
		StallTimeout:          spec.StallTimeout,
		WatchdogCheckInterval: spec.WatchdogCheckInterval,
	}
}

func buildDecisionCard(runID, nodeAddr string, epoch uint64, out runner.NodeOutcome, now time.Time) DecisionCard {
	card := DecisionCard{
		CardID:    uuid.NewString(),
		RunID:     runID,
		NodeAddr:  nodeAddr,
		Epoch:     epoch,
		Status:    DecisionStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	switch {
	case out.DecisionRequest != nil:
		card.Kind = DecisionKindRequestDecision
		card.Question = out.DecisionRequest.Question
		card.Context = out.DecisionRequest.Context
		card.Options = slices.Clone(out.DecisionRequest.Options)
		card.Recommended = out.DecisionRequest.Recommended
	case out.PendingApproval != nil:
		card.Kind = DecisionKindHumanApproval
		card.CommitSHA = out.PendingApproval.CommitSHA
		card.ArtifactIDs = slices.Clone(out.PendingApproval.ArtifactIDs)
		card.Artifacts = slices.Clone(out.PendingApproval.Artifacts)
		card.ResultJSON = append([]byte(nil), out.PendingApproval.ResultJSON...)
	default:
		card.Kind = DecisionKindPauseForHuman
		card.PauseReason = nonEmpty(out.PauseReason, "paused for human input")
	}
	return card
}

func invalidatePendingCardsLocked(state *persistedNodeState, now time.Time, reason string) {
	for i := range state.DecisionCards {
		if state.DecisionCards[i].Status == DecisionStatusPending {
			state.DecisionCards[i].Status = DecisionStatusInvalidated
			state.DecisionCards[i].InvalidatedReason = reason
			state.DecisionCards[i].UpdatedAt = now
		}
	}
}

func findPendingDecisionCardIndex(cards []DecisionCard, cardID string) (int, error) {
	trimmedID := strings.TrimSpace(cardID)
	if trimmedID != "" {
		for i := range cards {
			if cards[i].CardID == trimmedID {
				if cards[i].Status != DecisionStatusPending {
					return -1, fmt.Errorf("%w: decision card %q has status %s, expected PENDING", ErrInvalidDecision, trimmedID, cards[i].Status)
				}
				return i, nil
			}
		}
		return -1, fmt.Errorf("%w: decision card %q not found", ErrInvalidDecision, trimmedID)
	}
	pendingIdx := -1
	for i := range cards {
		if cards[i].Status == DecisionStatusPending {
			if pendingIdx >= 0 {
				return -1, fmt.Errorf("%w: multiple pending decision cards exist; card_id is required", ErrInvalidDecision)
			}
			pendingIdx = i
		}
	}
	if pendingIdx < 0 {
		return -1, fmt.Errorf("%w: no pending decision card found", ErrInvalidDecision)
	}
	return pendingIdx, nil
}

func formatDecisionInterjection(card DecisionCard) string {
	switch {
	case card.SelectedOption != "" && card.AnswerText != "":
		return fmt.Sprintf("Human decision (%s): selected option %q — %s", nonEmpty(card.Question, card.PauseReason), card.SelectedOption, card.AnswerText)
	case card.SelectedOption != "":
		return fmt.Sprintf("Human decision (%s): selected option %q", nonEmpty(card.Question, card.PauseReason), card.SelectedOption)
	default:
		return fmt.Sprintf("Human response (%s): %s", nonEmpty(card.Question, card.PauseReason), card.AnswerText)
	}
}

func appendInterjectionLocked(state *persistedNodeState, explicitID, message string, waitForConfirmation bool, now time.Time) InterjectionRecord {
	id := strings.TrimSpace(explicitID)
	if id == "" {
		id = uuid.NewString()
	}
	state.PendingInterjections = append(state.PendingInterjections, session.HumanInterjection{
		ID:                  id,
		Message:             message,
		WaitForConfirmation: waitForConfirmation,
	})
	rec := InterjectionRecord{
		ID:                  id,
		Message:             message,
		WaitForConfirmation: waitForConfirmation,
		Status:              InterjectionStatusPending,
		CreatedAt:           now,
	}
	state.InterjectionLog = append(state.InterjectionLog, rec)
	return rec
}

func confirmAwaitingInterjectionsLocked(state *persistedNodeState, now time.Time) {
	for i := range state.InterjectionLog {
		if state.InterjectionLog[i].WaitForConfirmation && state.InterjectionLog[i].Status != InterjectionStatusConfirmed {
			state.InterjectionLog[i].Status = InterjectionStatusConfirmed
			state.InterjectionLog[i].ConfirmedAt = &now
		}
	}
}

func clearLeaseLocked(state *persistedNodeState) {
	state.LeaseID = ""
	state.LeaseKey = ""
	state.WorkerProcessID = ""
	state.WorkerAppID = ""
}

func isTerminalStatus(st NodeStatus) bool {
	return st == StatusSucceeded || st == StatusFallbackTriggered
}

func staleEpochResp(current uint64) *EpochFencedResp {
	return &EpochFencedResp{
		OK:           false,
		StaleEpoch:   true,
		CurrentEpoch: current,
		ErrorCode:    errCodeStaleEpoch,
		ErrorMessage: checkpoint.ErrStaleEpoch.Error(),
	}
}

func errorResp(code, msg string) *EpochFencedResp {
	return &EpochFencedResp{
		OK:           false,
		ErrorCode:    code,
		ErrorMessage: msg,
	}
}

func mapDomainErrToResp(err error) *EpochFencedResp {
	switch {
	case errors.Is(err, checkpoint.ErrStaleEpoch):
		return &EpochFencedResp{OK: false, StaleEpoch: true, ErrorCode: errCodeStaleEpoch, ErrorMessage: err.Error()}
	case errors.Is(err, git.ErrProtectedSourceBranch):
		return errorResp(errCodeProtectedSourceBranch, err.Error())
	case errors.Is(err, git.ErrBranchOwnershipConflict):
		return errorResp(errCodeBranchOwnershipConflict, err.Error())
	case errors.Is(err, git.ErrInvalidBaseSHA):
		return errorResp(errCodeInvalidBaseSHA, err.Error())
	default:
		return errorResp(errCodeInvalidInput, err.Error())
	}
}

func branchRegistrationKey(repoURL, cleanBranch string) string {
	return git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch
}

func findCheckpointIndex(records []checkpoint.CheckpointRecord, seq uint64) int {
	for i := range records {
		if records[i].Seq == seq {
			return i
		}
	}
	return -1
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
}

func cloneStrPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneObjectMeta(m *blob.ObjectMeta) *blob.ObjectMeta {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

func cloneDiaryInput(d session.DiaryInput) session.DiaryInput {
	return session.DiaryInput{
		Completed:     slices.Clone(d.Completed),
		Hypotheses:    slices.Clone(d.Hypotheses),
		Decisions:     slices.Clone(d.Decisions),
		TestEvidence:  slices.Clone(d.TestEvidence),
		OpenQuestions: slices.Clone(d.OpenQuestions),
		NextSteps:     slices.Clone(d.NextSteps),
	}
}

func cloneCheckpointRecords(src []checkpoint.CheckpointRecord) []checkpoint.CheckpointRecord {
	if len(src) == 0 {
		return nil
	}
	out := make([]checkpoint.CheckpointRecord, len(src))
	for i, r := range src {
		out[i] = r.Clone()
	}
	return out
}

func cloneInterjectionLog(src []InterjectionRecord) []InterjectionRecord {
	if len(src) == 0 {
		return nil
	}
	out := make([]InterjectionRecord, len(src))
	for i, item := range src {
		cp := item
		if item.DrainedAt != nil {
			t := *item.DrainedAt
			cp.DrainedAt = &t
		}
		if item.ConfirmedAt != nil {
			t := *item.ConfirmedAt
			cp.ConfirmedAt = &t
		}
		out[i] = cp
	}
	return out
}

func clonePendingPublication(p *runner.PendingPublication) *runner.PendingPublication {
	if p == nil {
		return nil
	}
	cp := *p
	cp.ArtifactIDs = slices.Clone(p.ArtifactIDs)
	cp.Artifacts = slices.Clone(p.Artifacts)
	cp.ResultJSON = append([]byte(nil), p.ResultJSON...)
	return &cp
}

func cloneApprovalRecord(a *runner.ApprovalRecord) *runner.ApprovalRecord {
	if a == nil {
		return nil
	}
	cp := *a
	cp.ArtifactIDs = slices.Clone(a.ArtifactIDs)
	return &cp
}

func cloneDecisionCard(c DecisionCard) DecisionCard {
	cp := c
	cp.Options = slices.Clone(c.Options)
	cp.ArtifactIDs = slices.Clone(c.ArtifactIDs)
	cp.Artifacts = slices.Clone(c.Artifacts)
	cp.ResultJSON = append([]byte(nil), c.ResultJSON...)
	if c.Approved != nil {
		b := *c.Approved
		cp.Approved = &b
	}
	if c.AnsweredAt != nil {
		t := *c.AnsweredAt
		cp.AnsweredAt = &t
	}
	return cp
}

func cloneDecisionCards(src []DecisionCard) []DecisionCard {
	if len(src) == 0 {
		return nil
	}
	out := make([]DecisionCard, len(src))
	for i, c := range src {
		out[i] = cloneDecisionCard(c)
	}
	return out
}

func cloneNodeOutcome(o *runner.NodeOutcome) *runner.NodeOutcome {
	if o == nil {
		return nil
	}
	cp := *o
	cp.ResultJSON = append([]byte(nil), o.ResultJSON...)
	cp.Artifacts = slices.Clone(o.Artifacts)
	cp.FallbackArtifacts = slices.Clone(o.FallbackArtifacts)
	if o.DecisionRequest != nil {
		dr := *o.DecisionRequest
		dr.Options = slices.Clone(dr.Options)
		cp.DecisionRequest = &dr
	}
	if o.PendingApproval != nil {
		pa := *o.PendingApproval
		pa.ArtifactIDs = slices.Clone(pa.ArtifactIDs)
		pa.Artifacts = slices.Clone(pa.Artifacts)
		pa.ResultJSON = append([]byte(nil), pa.ResultJSON...)
		cp.PendingApproval = &pa
	}
	return &cp
}

func cloneDispatchSpec(d *DispatchSpec) *DispatchSpec {
	if d == nil {
		return nil
	}
	cp := *d
	cp.RunOverrides = maps.Clone(d.RunOverrides)
	cp.RequiredEnvKeys = slices.Clone(d.RequiredEnvKeys)
	cp.TokenRef = cloneStrPtr(d.TokenRef)
	cp.SSHKeyRef = cloneStrPtr(d.SSHKeyRef)
	cp.Sandboxes = maps.Clone(d.Sandboxes)
	cp.Harnesses = maps.Clone(d.Harnesses)
	cp.ModelProviders = maps.Clone(d.ModelProviders)
	cp.MCPServers = slices.Clone(d.MCPServers)
	cp.MCPResources = slices.Clone(d.MCPResources)
	cp.Tools = maps.Clone(d.Tools)
	cp.ToolEnvs = maps.Clone(d.ToolEnvs)
	cp.Imports = slices.Clone(d.Imports)
	if d.TargetPlatform != nil {
		p := *d.TargetPlatform
		cp.TargetPlatform = &p
	}
	return &cp
}

// Dapr production boundary implementations.

type daprConfigResolver struct {
	clientProvider func() daprclient.Client
}

func (r *daprConfigResolver) ResolveForNode(ctx context.Context, req *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error) {
	c := r.clientProvider()
	if c == nil {
		return nil, errors.New("node: dapr client unavailable for ConfigActor")
	}
	return config.NewClientStub(c).ResolveForNode(ctx, req)
}

type daprSlotManager struct {
	clientProvider func() daprclient.Client
}

func (m *daprSlotManager) ClaimSlot(ctx context.Context, req *registry.ClaimSlotReq) (*registry.ClaimSlotResp, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("node: dapr client unavailable for ProcessRegistryActor")
	}
	return registry.NewClientStub(c).ClaimSlot(ctx, req)
}

func (m *daprSlotManager) ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("node: dapr client unavailable for ProcessRegistryActor")
	}
	return registry.NewClientStub(c).ReleaseSlot(ctx, req)
}

func (m *daprSlotManager) ReportSuspect(ctx context.Context, req *registry.ReportSuspectReq) (*registry.ReportSuspectResp, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("node: dapr client unavailable for ProcessRegistryActor")
	}
	return registry.NewClientStub(c).ReportSuspect(ctx, req)
}

type daprWorkerDispatcher struct {
	clientProvider func() daprclient.Client
}

func (d *daprWorkerDispatcher) StartNode(ctx context.Context, appID string, req daemon.StartNodeRequest) (daemon.StartNodeResponse, error) {
	c := d.clientProvider()
	if c == nil {
		return daemon.StartNodeResponse{}, errors.New("node: dapr client unavailable for Worker start_node")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return daemon.StartNodeResponse{}, err
	}
	raw, err := c.InvokeMethodWithContent(ctx, appID, "start_node", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        payload,
	})
	if err != nil {
		return daemon.StartNodeResponse{}, err
	}
	var resp daemon.StartNodeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return daemon.StartNodeResponse{}, fmt.Errorf("decode start_node response: %w", err)
	}
	return resp, nil
}

func (d *daprWorkerDispatcher) AbortNode(ctx context.Context, appID string, req daemon.AbortNodeRequest) (daemon.AbortNodeResponse, error) {
	c := d.clientProvider()
	if c == nil {
		return daemon.AbortNodeResponse{}, errors.New("node: dapr client unavailable for Worker abort_node")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return daemon.AbortNodeResponse{}, err
	}
	raw, err := c.InvokeMethodWithContent(ctx, appID, "abort_node", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        payload,
	})
	if err != nil {
		return daemon.AbortNodeResponse{}, err
	}
	var resp daemon.AbortNodeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return daemon.AbortNodeResponse{}, fmt.Errorf("decode abort_node response: %w", err)
	}
	return resp, nil
}

type daprRunNotifier struct {
	clientProvider func() daprclient.Client
}

func (n *daprRunNotifier) RegisterManagedBranch(ctx context.Context, req RegisterManagedBranchReq) error {
	c := n.clientProvider()
	if c == nil || strings.TrimSpace(req.RunID) == "" {
		return nil
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = c.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: "RunActor",
		ActorID:   req.RunID,
		Method:    "RegisterManagedBranch",
		Data:      data,
	})
	return err
}

func (n *daprRunNotifier) NotifyNodeEvent(ctx context.Context, event NodeEventNotification) error {
	c := n.clientProvider()
	if c == nil || strings.TrimSpace(event.RunID) == "" {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = c.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: "RunActor",
		ActorID:   event.RunID,
		Method:    "OnNodeEvent",
		Data:      data,
	})
	return err
}

type daprReminderScheduler struct {
	clientProvider func() daprclient.Client
}

func (s *daprReminderScheduler) RegisterSlotRetryReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error {
	c := s.clientProvider()
	if c == nil {
		return errors.New("node: dapr client unavailable for reminder registration")
	}
	return c.RegisterActorReminder(ctx, &daprclient.RegisterActorReminderRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Name:      reminderName,
		DueTime:   dueTime.String(),
		Period:    period.String(),
	})
}

func (s *daprReminderScheduler) UnregisterSlotRetryReminder(ctx context.Context, actorType, actorID, reminderName string) error {
	c := s.clientProvider()
	if c == nil {
		return errors.New("node: dapr client unavailable for reminder unregistration")
	}
	return c.UnregisterActorReminder(ctx, &daprclient.UnregisterActorReminderRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Name:      reminderName,
	})
}

// ClientStub provides a type-safe Dapr Actor client wrapper over NodeActor
// populated via daprclient.Client.ImplActorClientStub.
type ClientStub struct {
	ActorID                  string
	StartOrResume            func(context.Context, *StartOrResumeReq) (*StartOrResumeResp, error)
	OnWorkerDead             func(context.Context, *registry.WorkerDeadNotification) (*OnWorkerDeadResp, error)
	PauseNode                func(context.Context, *PauseNodeReq) (*PauseNodeResp, error)
	AdvanceForRework         func(context.Context, *AdvanceForReworkReq) (*AdvanceForReworkResp, error)
	ResetForRerun            func(context.Context, *ResetForRerunReq) (*ResetForRerunResp, error)
	ReportNodeOutcome        func(context.Context, *daemon.NodeOutcomeReport) (*ReportNodeOutcomeResp, error)
	SubmitDecision           func(context.Context, *SubmitDecisionReq) (*SubmitDecisionResp, error)
	GetState                 func(context.Context, *GetNodeStateReq) (*GetNodeStateResp, error)
	RegisterCheckpointBranch func(context.Context, *RegisterCheckpointBranchReq) (*EpochFencedResp, error)
	RegisterWorkBranch       func(context.Context, *RegisterWorkBranchReq) (*EpochFencedResp, error)
	BeginCheckpoint          func(context.Context, *BeginCheckpointReq) (*BeginCheckpointResp, error)
	AppendFinalSHA           func(context.Context, *AppendFinalSHAReq) (*EpochFencedResp, error)
	CompleteCheckpoint       func(context.Context, *CompleteCheckpointReq) (*EpochFencedResp, error)
	ListCheckpoints          func(context.Context, *ListCheckpointsReq) (*ListCheckpointsResp, error)
	EnqueueInterjection      func(context.Context, *EnqueueInterjectionReq) (*EnqueueInterjectionResp, error)
	DrainInterjections       func(context.Context, *DrainInterjectionsReq) (*DrainInterjectionsResp, error)
	HasPendingInterjections  func(context.Context, *HasPendingInterjectionsReq) (*HasPendingInterjectionsResp, error)
	SaveQCLedgerSnapshot     func(context.Context, *SaveQCLedgerSnapshotReq) (*EpochFencedResp, error)
	LoadQCLedgerSnapshot     func(context.Context, *LoadQCLedgerSnapshotReq) (*LoadQCLedgerSnapshotResp, error)
	SavePendingPublication   func(context.Context, *SavePendingPublicationReq) (*EpochFencedResp, error)
	LoadPendingPublication   func(context.Context, *LoadPendingPublicationReq) (*LoadPendingPublicationResp, error)
	GetApproval              func(context.Context, *GetApprovalReq) (*GetApprovalResp, error)
}

// Type implements actor.Client.
func (c *ClientStub) Type() string {
	return ActorType
}

// ID implements actor.Client.
func (c *ClientStub) ID() string {
	return c.ActorID
}

// NewClientStub constructs a ClientStub for actorID bound to client via ImplActorClientStub.
func NewClientStub(actorID string, client daprclient.Client) *ClientStub {
	stub := &ClientStub{ActorID: actorID}
	if client != nil {
		client.ImplActorClientStub(stub)
	}
	return stub
}

// NewClientStubForNode encodes (runID, canonicalNodeAddr) and constructs a bound ClientStub.
func NewClientStubForNode(runID, canonicalNodeAddr string, client daprclient.Client) (*ClientStub, error) {
	actorID, err := EncodeNodeActorID(runID, canonicalNodeAddr)
	if err != nil {
		return nil, err
	}
	return NewClientStub(actorID, client), nil
}
