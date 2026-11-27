package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

const mergeStateKey = "merge_actor_v1"

type persistedMergeState struct {
	RepoURL                 string            `json:"repo_url,omitempty"`
	TargetBranch            string            `json:"target_branch,omitempty"`
	SharedTargetBranch      bool              `json:"shared_target_branch,omitempty"`
	LatestTargetSHA         string            `json:"latest_target_sha,omitempty"`
	LatestTargetEpoch       uint64            `json:"latest_target_epoch,omitempty"`
	InFlight                *CandidateTicket  `json:"in_flight,omitempty"`
	Queue                   []CandidateTicket `json:"queue,omitempty"`
	History                 []CandidateTicket `json:"history,omitempty"`
	RegisteredMergeBranches map[string]bool   `json:"registered_merge_branches,omitempty"`
}

func (s *persistedMergeState) ensureDefaults() {
	if s.RegisteredMergeBranches == nil {
		s.RegisteredMergeBranches = make(map[string]bool)
	}
}

func (s persistedMergeState) clone() persistedMergeState {
	out := s
	if s.InFlight != nil {
		cp := cloneCandidateTicket(*s.InFlight)
		out.InFlight = &cp
	}
	out.Queue = cloneCandidateTickets(s.Queue)
	out.History = cloneCandidateTickets(s.History)
	out.RegisteredMergeBranches = maps.Clone(s.RegisteredMergeBranches)
	if out.RegisteredMergeBranches == nil {
		out.RegisteredMergeBranches = make(map[string]bool)
	}
	return out
}

// Options configures a MergeActor's external boundaries, clock, and retry cadence.
type Options struct {
	ClientProvider    func() daprclient.Client
	ConfigResolver    ConfigResolver
	SlotManager       SlotManager
	WorkerDispatcher  MergeWorkerDispatcher
	RunNotifier       RunMergeNotifier
	ReminderScheduler ReminderScheduler
	Clock             func() time.Time
	SlotRetryInterval time.Duration
	ReminderName      string
}

// MergeActor manages a persistent FIFO queue and strict single in-flight serial integration
// loop for a single (repo_url, target_branch) pair.
type MergeActor struct {
	actor.ServerImplBaseCtx

	mu                sync.Mutex
	dispatching       bool
	dispatchWake      bool
	clientProvider    func() daprclient.Client
	configResolver    ConfigResolver
	slotManager       SlotManager
	workerDispatcher  MergeWorkerDispatcher
	runNotifier       RunMergeNotifier
	reminderScheduler ReminderScheduler
	clock             func() time.Time
	slotRetryInterval time.Duration
	reminderName      string
}

// NewMergeActor constructs a MergeActor with defaults and optional test/Dapr boundary overrides.
func NewMergeActor(opts Options) *MergeActor {
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

	a := &MergeActor{
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
			a.workerDispatcher = &daprMergeWorkerDispatcher{clientProvider: opts.ClientProvider}
		}
		if a.runNotifier == nil {
			a.runNotifier = &daprRunMergeNotifier{clientProvider: opts.ClientProvider}
		}
		if a.reminderScheduler == nil {
			a.reminderScheduler = &daprReminderScheduler{clientProvider: opts.ClientProvider}
		}
	}
	return a
}

// Type implements actor.ServerContext.
func (a *MergeActor) Type() string {
	return ActorType
}

// SetStateManager binds state persistence to daprhost.NewActorStateManager when clientProvider
// supplies a Dapr client, while preserving direct StateManagerContext injection in unit tests.
func (a *MergeActor) SetStateManager(sm actor.StateManagerContext) {
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

// EnqueueCandidate idempotently enqueues a candidate branch by (RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch),
// persists the FIFO queue, triggers non-blocking dispatch if idle, and returns immediately.
func (a *MergeActor) EnqueueCandidate(ctx context.Context, req *EnqueueCandidateReq) (*EnqueueCandidateResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("merge: enqueue candidate request is nil")
	}

	runID := strings.TrimSpace(req.RunID)
	mergeAddr := strings.TrimSpace(req.MergeNodeAddr)
	sourceAddr := strings.TrimSpace(req.SourceNodeAddr)
	repoURL := strings.TrimSpace(req.RepoURL)
	if runID == "" || mergeAddr == "" || sourceAddr == "" {
		return nil, errors.New("merge: run_id, merge_node_addr, and source_node_addr are required")
	}
	if req.SourceEpoch == 0 {
		return nil, errors.New("merge: source_epoch must be > 0")
	}
	if repoURL == "" {
		return nil, errors.New("merge: repo_url is required")
	}
	cleanCandidate, err := git.NormalizeBranchName(req.CandidateBranch)
	if err != nil {
		return nil, fmt.Errorf("merge: invalid candidate_branch: %w", err)
	}

	targetBranch, shared, err := EffectiveTargetBranch(runID, mergeAddr, req.TargetBranch)
	if err != nil {
		return nil, err
	}

	mergeEpoch := req.MergeEpoch
	if mergeEpoch == 0 {
		mergeEpoch = 1
	}

	tokenRef := cloneStrPtr(req.TokenRef)
	sshKeyRef := cloneStrPtr(req.SSHKeyRef)
	if tokenRef == nil && req.Auth.TokenRef != nil {
		tokenRef = cloneStrPtr(req.Auth.TokenRef)
	}
	if sshKeyRef == nil && req.Auth.SSHKeyRef != nil {
		sshKeyRef = cloneStrPtr(req.Auth.SSHKeyRef)
	}
	if tokenRef != nil && sshKeyRef != nil {
		return nil, errors.New("merge: token_ref and ssh_key_ref are mutually exclusive")
	}

	var explicitPtr *string
	if shared {
		b := targetBranch
		explicitPtr = &b
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	if existing, ok := findActiveOrTerminalTicket(state, runID, mergeAddr, mergeEpoch, sourceAddr, req.SourceEpoch); ok {
		a.mu.Unlock()
		return &EnqueueCandidateResp{
			Accepted:     true,
			Deduplicated: true,
			Ticket:       existing,
		}, nil
	}

	now := a.clock()
	ticket := CandidateTicket{
		RequestID:          uuid.NewString(),
		RunID:              runID,
		MergeNodeAddr:      mergeAddr,
		MergeEpoch:         mergeEpoch,
		SourceNodeAddr:     sourceAddr,
		SourceEpoch:        req.SourceEpoch,
		RepoURL:            repoURL,
		BasedSource:        strings.TrimSpace(req.BasedSource),
		BaseSHA:            strings.TrimSpace(req.BaseSHA),
		TargetBranch:       targetBranch,
		ExplicitTarget:     explicitPtr,
		SharedTargetBranch: shared,
		CandidateBranch:    cleanCandidate,
		Namespace:          strings.TrimSpace(req.Namespace),
		RunOverrides:       maps.Clone(req.RunOverrides),
		RequiredEnvKeys:    slices.Clone(req.RequiredEnvKeys),
		TokenRef:           tokenRef,
		SSHKeyRef:          sshKeyRef,
		Auth: git.AuthConfig{
			TokenRef:  cloneStrPtr(tokenRef),
			SSHKeyRef: cloneStrPtr(sshKeyRef),
		},
		RequiredArch: strings.TrimSpace(req.RequiredArch),
		Sandbox:      req.Sandbox,
		Gatekeepers:  cloneGatekeepers(req.Gatekeepers),
		PlanBlobKey:  req.PlanBlobKey,
		PlanSHA256:   req.PlanSHA256,
		BundlePath:   req.BundlePath,
		ImportsDir:   req.ImportsDir,
		Status:       TicketStatusPending,
		EnqueuedAt:   now,
		UpdatedAt:    now,
	}

	if state.RepoURL == "" {
		state.RepoURL = repoURL
	}
	if state.TargetBranch == "" {
		state.TargetBranch = targetBranch
		state.SharedTargetBranch = shared
	}
	if !shared && req.BranchPreRegistered {
		state.RegisteredMergeBranches[branchRegKey(runID, mergeAddr, mergeEpoch)] = true
	}
	state.Queue = append(state.Queue, ticket)
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	if err := a.tryDispatchNext(ctx, true); err != nil {
		return nil, err
	}

	if updated, ok := a.snapshotTicket(ctx, ticket.RequestID); ok {
		ticket = updated
	}
	return &EnqueueCandidateResp{
		Accepted: true,
		Ticket:   ticket,
	}, nil
}

// ReportCandidateResult validates RequestID/LeaseID against the active IN_FLIGHT ticket, updates
// LatestTargetSHA on success, preserves target on rework_source, notifies RunActor, and advances the queue.
func (a *MergeActor) ReportCandidateResult(ctx context.Context, report *daemon.MergeOutcomeReport) (*ReportCandidateResultResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if report == nil {
		return nil, errors.New("merge: candidate result report is nil")
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	if state.InFlight == nil || state.InFlight.RequestID != report.RequestID {
		resp := &ReportCandidateResultResp{
			Accepted:        false,
			IgnoredStale:    true,
			LatestTargetSHA: state.LatestTargetSHA,
		}
		a.mu.Unlock()
		return resp, nil
	}
	if state.InFlight.LeaseID != "" && report.LeaseID != "" && report.LeaseID != state.InFlight.LeaseID {
		resp := &ReportCandidateResultResp{
			Accepted:        false,
			IgnoredStale:    true,
			LatestTargetSHA: state.LatestTargetSHA,
		}
		a.mu.Unlock()
		return resp, nil
	}

	ticket := cloneCandidateTicket(*state.InFlight)
	releaseLeaseID := nonEmpty(report.LeaseID, ticket.LeaseID)
	releaseLeaseKey := nonEmpty(report.LeaseKey, ticket.LeaseKey)
	state.InFlight = nil

	now := a.clock()
	var notice *MergeCandidateNotification

	switch {
	case report.Aborted:
		ticket.Status = TicketStatusCancelled
		ticket.ErrorMessage = nonEmpty(report.ErrMessage, "merge candidate aborted")
		ticket.UpdatedAt = now
		state.History = append(state.History, ticket)

	case strings.TrimSpace(report.ErrMessage) != "":
		ticket.Status = TicketStatusFailed
		ticket.ErrorMessage = strings.TrimSpace(report.ErrMessage)
		ticket.UpdatedAt = now
		state.History = append(state.History, ticket)
		res := report.Result
		if res.TargetBranch == "" {
			res.TargetBranch = ticket.TargetBranch
		}
		if res.LatestTargetSHA == "" {
			res.LatestTargetSHA = state.LatestTargetSHA
		}
		notice = &MergeCandidateNotification{
			RequestID:              ticket.RequestID,
			RunID:                  ticket.RunID,
			MergeNodeAddr:          ticket.MergeNodeAddr,
			MergeEpoch:             ticket.MergeEpoch,
			SourceNodeAddr:         ticket.SourceNodeAddr,
			SourceEpoch:            ticket.SourceEpoch,
			CandidateBranch:        ticket.CandidateBranch,
			TargetBranch:           nonEmpty(res.TargetBranch, ticket.TargetBranch),
			SharedTargetBranch:     ticket.SharedTargetBranch,
			MergedIntoSharedBranch: false,
			Result:                 res,
			ErrorMessage:           ticket.ErrorMessage,
			OccurredAt:             now,
		}

	case report.Result.Status == runner.MergeStatusSucceeded:
		mergedSHA := nonEmpty(report.Result.MergedCommitSHA, report.Result.LatestTargetSHA)
		if mergedSHA != "" {
			state.LatestTargetSHA = mergedSHA
			state.LatestTargetEpoch = ticket.MergeEpoch
		}
		ticket.Status = TicketStatusCompleted
		ticket.MergedCommitSHA = report.Result.MergedCommitSHA
		ticket.LatestTargetSHA = nonEmpty(report.Result.LatestTargetSHA, state.LatestTargetSHA)
		ticket.ErrorMessage = ""
		ticket.UpdatedAt = now
		state.History = append(state.History, ticket)

		res := report.Result
		if res.TargetBranch == "" {
			res.TargetBranch = ticket.TargetBranch
		}
		if res.LatestTargetSHA == "" {
			res.LatestTargetSHA = state.LatestTargetSHA
		}
		notice = &MergeCandidateNotification{
			RequestID:              ticket.RequestID,
			RunID:                  ticket.RunID,
			MergeNodeAddr:          ticket.MergeNodeAddr,
			MergeEpoch:             ticket.MergeEpoch,
			SourceNodeAddr:         ticket.SourceNodeAddr,
			SourceEpoch:            ticket.SourceEpoch,
			CandidateBranch:        ticket.CandidateBranch,
			TargetBranch:           nonEmpty(res.TargetBranch, ticket.TargetBranch),
			SharedTargetBranch:     ticket.SharedTargetBranch,
			MergedIntoSharedBranch: ticket.SharedTargetBranch,
			Result:                 res,
			OccurredAt:             now,
		}

	case report.Result.Status == runner.MergeStatusReworkSource:
		if sha := strings.TrimSpace(report.Result.LatestTargetSHA); sha != "" {
			state.LatestTargetSHA = sha
			state.LatestTargetEpoch = ticket.MergeEpoch
		}
		ticket.Status = TicketStatusReworkRequired
		ticket.LatestTargetSHA = nonEmpty(report.Result.LatestTargetSHA, state.LatestTargetSHA)
		ticket.ErrorMessage = report.Result.FailureReport
		ticket.UpdatedAt = now
		state.History = append(state.History, ticket)

		res := report.Result
		if res.TargetBranch == "" {
			res.TargetBranch = ticket.TargetBranch
		}
		if res.LatestTargetSHA == "" {
			res.LatestTargetSHA = state.LatestTargetSHA
		}
		notice = &MergeCandidateNotification{
			RequestID:              ticket.RequestID,
			RunID:                  ticket.RunID,
			MergeNodeAddr:          ticket.MergeNodeAddr,
			MergeEpoch:             ticket.MergeEpoch,
			SourceNodeAddr:         ticket.SourceNodeAddr,
			SourceEpoch:            ticket.SourceEpoch,
			CandidateBranch:        ticket.CandidateBranch,
			TargetBranch:           nonEmpty(res.TargetBranch, ticket.TargetBranch),
			SharedTargetBranch:     ticket.SharedTargetBranch,
			MergedIntoSharedBranch: false,
			Result:                 res,
			ErrorMessage:           report.Result.FailureReport,
			OccurredAt:             now,
		}

	default:
		ticket.Status = TicketStatusFailed
		ticket.ErrorMessage = fmt.Sprintf("unexpected merge status %q", report.Result.Status)
		ticket.UpdatedAt = now
		state.History = append(state.History, ticket)
		res := report.Result
		if res.TargetBranch == "" {
			res.TargetBranch = ticket.TargetBranch
		}
		if res.LatestTargetSHA == "" {
			res.LatestTargetSHA = state.LatestTargetSHA
		}
		notice = &MergeCandidateNotification{
			RequestID:              ticket.RequestID,
			RunID:                  ticket.RunID,
			MergeNodeAddr:          ticket.MergeNodeAddr,
			MergeEpoch:             ticket.MergeEpoch,
			SourceNodeAddr:         ticket.SourceNodeAddr,
			SourceEpoch:            ticket.SourceEpoch,
			CandidateBranch:        ticket.CandidateBranch,
			TargetBranch:           nonEmpty(res.TargetBranch, ticket.TargetBranch),
			SharedTargetBranch:     ticket.SharedTargetBranch,
			MergedIntoSharedBranch: false,
			Result:                 res,
			ErrorMessage:           ticket.ErrorMessage,
			OccurredAt:             now,
		}
	}

	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	latestSHA := state.LatestTargetSHA
	slotMgr := a.slotManager
	notifier := a.runNotifier
	a.mu.Unlock()

	if (releaseLeaseID != "" || releaseLeaseKey != "") && slotMgr != nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
			LeaseID:  releaseLeaseID,
			LeaseKey: releaseLeaseKey,
		})
	}
	if notice != nil && notifier != nil {
		_ = notifier.OnMergeCandidateResult(ctx, *notice)
	}
	_ = a.tryDispatchNext(ctx, false)

	return &ReportCandidateResultResp{
		Accepted:        !report.Aborted,
		IgnoredStale:    report.Aborted,
		Status:          ticket.Status,
		LatestTargetSHA: latestSHA,
	}, nil
}

// CancelByRun removes matching PENDING tickets, aborts and releases any matching IN_FLIGHT ticket,
// and immediately dispatches the next remaining uncancelled ticket in the queue.
func (a *MergeActor) CancelByRun(ctx context.Context, req *CancelByRunReq) (*CancelByRunResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("merge: cancel by run request is nil")
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		return nil, errors.New("merge: run_id is required")
	}
	mergeAddr := strings.TrimSpace(req.MergeNodeAddr)
	sourceAddr := strings.TrimSpace(req.SourceNodeAddr)
	reason := nonEmpty(req.Reason, "cancelled by run")

	var (
		needAbort      bool
		abortAppID     string
		abortReqID     string
		releaseLeaseID string
		releaseKey     string
	)

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	now := a.clock()
	remaining := make([]CandidateTicket, 0, len(state.Queue))
	cancelledPending := 0
	for _, t := range state.Queue {
		if matchesCancelFilter(t, runID, mergeAddr, sourceAddr) {
			cancelledPending++
			c := cloneCandidateTicket(t)
			c.Status = TicketStatusCancelled
			c.ErrorMessage = reason
			c.UpdatedAt = now
			state.History = append(state.History, c)
			continue
		}
		remaining = append(remaining, t)
	}
	state.Queue = remaining

	cancelledInFlight := false
	if state.InFlight != nil && matchesCancelFilter(*state.InFlight, runID, mergeAddr, sourceAddr) {
		cancelledInFlight = true
		c := cloneCandidateTicket(*state.InFlight)
		abortReqID = c.RequestID
		abortAppID = c.WorkerAppID
		releaseLeaseID = c.LeaseID
		releaseKey = c.LeaseKey
		if abortAppID != "" {
			needAbort = true
		}
		c.Status = TicketStatusCancelled
		c.ErrorMessage = reason
		c.LeaseID = ""
		c.LeaseKey = ""
		c.WorkerProcessID = ""
		c.WorkerAppID = ""
		c.UpdatedAt = now
		state.History = append(state.History, c)
		state.InFlight = nil
	}

	if cancelledPending > 0 || cancelledInFlight {
		if err := a.saveStateLocked(ctx, state); err != nil {
			a.mu.Unlock()
			return nil, err
		}
	}
	dispatcher := a.workerDispatcher
	slotMgr := a.slotManager
	a.mu.Unlock()

	abortedWorker := false
	if needAbort && dispatcher != nil {
		abortResp, aErr := dispatcher.AbortMergeCandidate(ctx, abortAppID, daemon.AbortMergeRequest{
			RequestID: abortReqID,
		})
		if aErr == nil && abortResp.Aborted {
			abortedWorker = true
		}
	}
	if (releaseLeaseID != "" || releaseKey != "") && slotMgr != nil {
		_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{
			LeaseID:  releaseLeaseID,
			LeaseKey: releaseKey,
		})
	}
	_ = a.tryDispatchNext(ctx, true)

	return &CancelByRunResp{
		CancelledPending:  cancelledPending,
		CancelledInFlight: cancelledInFlight,
		AbortedWorker:     abortedWorker,
		InFlightRequestID: abortReqID,
	}, nil
}

// OnWorkerDead resets an active IN_FLIGHT ticket back to the front of Queue as PENDING when
// its worker is declared DEAD, clears the stale lease, persists state, and re-dispatches.
func (a *MergeActor) OnWorkerDead(ctx context.Context, notice *registry.WorkerDeadNotification) (*OnWorkerDeadResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if notice == nil {
		return nil, errors.New("merge: worker dead notification is nil")
	}

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	if state.InFlight == nil {
		a.mu.Unlock()
		return &OnWorkerDeadResp{Handled: false}, nil
	}

	matchesLease := (notice.LeaseID != "" && notice.LeaseID == state.InFlight.LeaseID) ||
		(notice.LeaseID == "" && notice.ProcessID != "" && notice.ProcessID == state.InFlight.WorkerProcessID)
	if !matchesLease {
		reqID := state.InFlight.RequestID
		a.mu.Unlock()
		return &OnWorkerDeadResp{Handled: false, RequestID: reqID}, nil
	}

	ticket := cloneCandidateTicket(*state.InFlight)
	ticket.Status = TicketStatusPending
	ticket.LeaseID = ""
	ticket.LeaseKey = ""
	ticket.WorkerProcessID = ""
	ticket.WorkerAppID = ""
	ticket.ErrorMessage = "worker dead: " + strings.TrimSpace(notice.Reason)
	ticket.UpdatedAt = a.clock()

	state.InFlight = nil
	state.Queue = append([]CandidateTicket{ticket}, state.Queue...)
	if err := a.saveStateLocked(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	_ = a.tryDispatchNext(ctx, false)
	return &OnWorkerDeadResp{
		Handled:   true,
		RequestID: ticket.RequestID,
	}, nil
}

// ReminderCall retries queued dispatch outside the actor mutex and explicitly persists state via Set+Save.
func (a *MergeActor) ReminderCall(_ string, _ []byte, _ string, _ string) {
	ctx := context.Background()

	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return
	}
	if state.InFlight != nil || len(state.Queue) == 0 {
		idleEmpty := state.InFlight == nil && len(state.Queue) == 0
		remSched := a.reminderScheduler
		actorID := a.effectiveActorID()
		remName := a.reminderName
		a.mu.Unlock()
		if idleEmpty && remSched != nil && actorID != "" {
			_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
		}
		return
	}
	a.mu.Unlock()

	_ = a.tryDispatchNext(ctx, false)
}

// GetState returns a defensive snapshot of the MergeActor's queue, IN_FLIGHT ticket, and LatestTargetSHA.
func (a *MergeActor) GetState(ctx context.Context, _ *GetMergeStateReq) (*GetMergeStateResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	var inFlight *CandidateTicket
	if state.InFlight != nil {
		cp := cloneCandidateTicket(*state.InFlight)
		inFlight = &cp
	}
	return &GetMergeStateResp{
		RepoURL:            state.RepoURL,
		TargetBranch:       state.TargetBranch,
		SharedTargetBranch: state.SharedTargetBranch,
		LatestTargetSHA:    state.LatestTargetSHA,
		InFlight:           inFlight,
		Queue:              cloneCandidateTickets(state.Queue),
		History:            cloneCandidateTickets(state.History),
	}, nil
}

func (a *MergeActor) tryDispatchNext(ctx context.Context, asyncNotify bool) error {
	a.mu.Lock()
	if a.dispatching {
		a.dispatchWake = true
		a.mu.Unlock()
		return nil
	}
	a.dispatching = true
	a.mu.Unlock()

	for {
		a.mu.Lock()
		a.dispatchWake = false
		state, err := a.loadStateLocked(ctx)
		if err != nil {
			a.dispatching = false
			a.mu.Unlock()
			return err
		}
		if state.InFlight != nil {
			a.dispatching = false
			a.mu.Unlock()
			return nil
		}
		if len(state.Queue) == 0 {
			remSched := a.reminderScheduler
			actorID := a.effectiveActorID()
			remName := a.reminderName
			a.dispatching = false
			a.mu.Unlock()
			if remSched != nil && actorID != "" {
				_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
			}
			return nil
		}

		ticket := cloneCandidateTicket(state.Queue[0])
		regKey := branchRegKey(ticket.RunID, ticket.MergeNodeAddr, ticket.MergeEpoch)
		needRegisterBranch := !ticket.SharedTargetBranch && !state.RegisteredMergeBranches[regKey]
		latestSHA := state.LatestTargetSHA
		latestEpoch := state.LatestTargetEpoch
		actorID := a.effectiveActorID()
		cfgResolver := a.configResolver
		slotMgr := a.slotManager
		dispatcher := a.workerDispatcher
		notifier := a.runNotifier
		remSched := a.reminderScheduler
		retryInterval := a.slotRetryInterval
		remName := a.reminderName
		a.mu.Unlock()

		// Step 1: Resolve minimal managed env + Git credentials outside actor mutex.
		keys := collectRequiredKeys(ticket)
		var resolvedEnv map[string]string
		if len(keys) > 0 {
			if cfgResolver == nil {
				a.failHeadTicket(ctx, ticket.RequestID, "merge: config resolver unavailable for required keys", notifier, asyncNotify)
				continue
			}
			resResp, resErr := cfgResolver.ResolveForNode(ctx, &config.ResolveForNodeReq{
				RunID:        ticket.RunID,
				Namespace:    ticket.Namespace,
				RunOverrides: maps.Clone(ticket.RunOverrides),
				Keys:         keys,
			})
			if resErr != nil {
				a.failHeadTicket(ctx, ticket.RequestID, fmt.Sprintf("resolve merge credentials: %v", resErr), notifier, asyncNotify)
				continue
			}
			if resResp == nil || len(resResp.MissingKeys) > 0 {
				var missing []string
				if resResp != nil {
					missing = resResp.MissingKeys
				}
				a.failHeadTicket(ctx, ticket.RequestID, fmt.Sprintf("missing required managed env/secret keys: %s", strings.Join(missing, ", ")), notifier, asyncNotify)
				continue
			}
			resolvedEnv = maps.Clone(resResp.Values)
		}

		// Step 2: Register KindMergeBranch in RunActor ledger BEFORE dispatching default Run-exclusive merges.
		if needRegisterBranch && notifier != nil {
			pinBase := strings.TrimSpace(ticket.BaseSHA)
			if pinBase == "" && (latestEpoch == 0 || latestEpoch == ticket.MergeEpoch) {
				pinBase = latestSHA
			}
			if regErr := notifier.RegisterManagedBranch(ctx, RegisterManagedBranchReq{
				RunID:    ticket.RunID,
				NodeAddr: ticket.MergeNodeAddr,
				Epoch:    ticket.MergeEpoch,
				RepoURL:  ticket.RepoURL,
				Branch:   ticket.TargetBranch,
				BaseSHA:  pinBase,
				Kind:     git.KindMergeBranch,
			}); regErr != nil {
				a.failHeadTicket(ctx, ticket.RequestID, fmt.Sprintf("register merge branch %q: %v", ticket.TargetBranch, regErr), notifier, asyncNotify)
				continue
			}

			a.mu.Lock()
			state, err = a.loadStateLocked(ctx)
			if err != nil {
				a.dispatching = false
				a.mu.Unlock()
				return err
			}
			state.RegisteredMergeBranches[regKey] = true
			if err := a.saveStateLocked(ctx, state); err != nil {
				a.dispatching = false
				a.mu.Unlock()
				return err
			}
			a.mu.Unlock()
		}

		// Step 3: Claim least-load worker slot outside actor mutex.
		if slotMgr == nil {
			a.failHeadTicket(ctx, ticket.RequestID, "merge: slot manager unavailable", notifier, asyncNotify)
			continue
		}
		nextAttempt := ticket.Attempt + 1
		leaseKey := fmt.Sprintf("merge/%s/%s/a%d", nonEmpty(actorID, "merge"), ticket.RequestID, nextAttempt)
		claimResp, claimErr := slotMgr.ClaimSlot(ctx, &registry.ClaimSlotReq{
			LeaseKey:       leaseKey,
			OwnerActorType: ActorType,
			OwnerActorID:   actorID,
			RequiredArch:   ticket.RequiredArch,
		})
		if claimErr != nil || claimResp == nil || !claimResp.Granted {
			if remSched != nil && actorID != "" {
				_ = remSched.RegisterSlotRetryReminder(ctx, ActorType, actorID, remName, retryInterval, retryInterval)
			}
			a.mu.Lock()
			wake := a.dispatchWake
			headChanged := false
			if wake {
				if stCheck, sErr := a.loadStateLocked(ctx); sErr == nil {
					headChanged = len(stCheck.Queue) > 0 && stCheck.Queue[0].RequestID != ticket.RequestID
				}
			}
			a.dispatchWake = false
			if !headChanged {
				a.dispatching = false
				a.mu.Unlock()
				return nil
			}
			a.mu.Unlock()
			continue
		}

		// Step 4: Commit IN_FLIGHT + LeaseID BEFORE calling RunMergeCandidate so immediate callbacks match.
		a.mu.Lock()
		state, err = a.loadStateLocked(ctx)
		if err != nil {
			a.dispatching = false
			a.mu.Unlock()
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
			return err
		}
		if state.InFlight != nil {
			a.dispatching = false
			a.mu.Unlock()
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
			return nil
		}
		if len(state.Queue) == 0 || state.Queue[0].RequestID != ticket.RequestID {
			a.mu.Unlock()
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
			continue
		}

		committed := cloneCandidateTicket(state.Queue[0])
		state.Queue = state.Queue[1:]
		committed.Status = TicketStatusInFlight
		committed.Attempt = nextAttempt
		committed.LeaseID = claimResp.LeaseID
		committed.LeaseKey = claimResp.LeaseKey
		committed.WorkerProcessID = claimResp.ProcessID
		committed.WorkerAppID = claimResp.AppID
		committed.ErrorMessage = ""
		committed.UpdatedAt = a.clock()
		state.InFlight = &committed
		if err := a.saveStateLocked(ctx, state); err != nil {
			a.dispatching = false
			a.mu.Unlock()
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
			return err
		}
		a.mu.Unlock()

		if dispatcher == nil {
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
			a.mu.Lock()
			if st, lerr := a.loadStateLocked(ctx); lerr == nil && st.InFlight != nil && st.InFlight.RequestID == committed.RequestID {
				st.InFlight = nil
				committed.Status = TicketStatusFailed
				committed.ErrorMessage = "merge: worker dispatcher unavailable"
				committed.UpdatedAt = a.clock()
				st.History = append(st.History, committed)
				_ = a.saveStateLocked(ctx, st)
			}
			a.mu.Unlock()
			if notifier != nil {
				deliverMergeResultNotification(ctx, notifier, MergeCandidateNotification{
					RequestID:          committed.RequestID,
					RunID:              committed.RunID,
					MergeNodeAddr:      committed.MergeNodeAddr,
					MergeEpoch:         committed.MergeEpoch,
					SourceNodeAddr:     committed.SourceNodeAddr,
					SourceEpoch:        committed.SourceEpoch,
					CandidateBranch:    committed.CandidateBranch,
					TargetBranch:       committed.TargetBranch,
					SharedTargetBranch: committed.SharedTargetBranch,
					ErrorMessage:       "merge: worker dispatcher unavailable",
					OccurredAt:         a.clock(),
				}, asyncNotify)
			}
			continue
		}

		runReq := buildRunMergeCandidateRequest(actorID, committed, claimResp, resolvedEnv)
		runResp, runErr := dispatcher.RunMergeCandidate(ctx, claimResp.AppID, runReq)
		if runErr != nil || !runResp.Accepted {
			errMsg := "worker rejected run_merge_candidate"
			if runErr != nil {
				errMsg = runErr.Error()
			}
			_, _ = slotMgr.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: claimResp.LeaseID, LeaseKey: claimResp.LeaseKey})
			_, _ = slotMgr.ReportSuspect(ctx, &registry.ReportSuspectReq{ProcessID: claimResp.ProcessID, Reason: errMsg})

			a.mu.Lock()
			st, lerr := a.loadStateLocked(ctx)
			if lerr == nil && st.InFlight != nil && st.InFlight.RequestID == committed.RequestID && st.InFlight.LeaseID == claimResp.LeaseID {
				requeued := cloneCandidateTicket(*st.InFlight)
				requeued.Status = TicketStatusPending
				requeued.LeaseID = ""
				requeued.LeaseKey = ""
				requeued.WorkerProcessID = ""
				requeued.WorkerAppID = ""
				requeued.ErrorMessage = errMsg
				requeued.UpdatedAt = a.clock()
				st.InFlight = nil
				st.Queue = append([]CandidateTicket{requeued}, st.Queue...)
				_ = a.saveStateLocked(ctx, st)
			}
			a.mu.Unlock()

			if remSched != nil && actorID != "" {
				_ = remSched.RegisterSlotRetryReminder(ctx, ActorType, actorID, remName, retryInterval, retryInterval)
			}
			a.mu.Lock()
			a.dispatchWake = false
			a.dispatching = false
			a.mu.Unlock()
			return nil
		}

		if remSched != nil && actorID != "" {
			_ = remSched.UnregisterSlotRetryReminder(ctx, ActorType, actorID, remName)
		}

		a.mu.Lock()
		wake := a.dispatchWake
		a.dispatchWake = false
		if !wake {
			a.dispatching = false
			a.mu.Unlock()
			return nil
		}
		a.mu.Unlock()
	}
}

func (a *MergeActor) failHeadTicket(ctx context.Context, expectedReqID, errMsg string, notifier RunMergeNotifier, asyncNotify bool) {
	a.mu.Lock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return
	}
	if len(state.Queue) == 0 || state.Queue[0].RequestID != expectedReqID {
		a.mu.Unlock()
		return
	}
	failed := cloneCandidateTicket(state.Queue[0])
	state.Queue = state.Queue[1:]
	now := a.clock()
	failed.Status = TicketStatusFailed
	failed.ErrorMessage = errMsg
	failed.UpdatedAt = now
	state.History = append(state.History, failed)
	latestSHA := state.LatestTargetSHA
	_ = a.saveStateLocked(ctx, state)
	a.mu.Unlock()

	if notifier != nil {
		deliverMergeResultNotification(ctx, notifier, MergeCandidateNotification{
			RequestID:          failed.RequestID,
			RunID:              failed.RunID,
			MergeNodeAddr:      failed.MergeNodeAddr,
			MergeEpoch:         failed.MergeEpoch,
			SourceNodeAddr:     failed.SourceNodeAddr,
			SourceEpoch:        failed.SourceEpoch,
			CandidateBranch:    failed.CandidateBranch,
			TargetBranch:       failed.TargetBranch,
			SharedTargetBranch: failed.SharedTargetBranch,
			Result: runner.MergeCandidateResult{
				TargetBranch:    failed.TargetBranch,
				LatestTargetSHA: latestSHA,
			},
			ErrorMessage: errMsg,
			OccurredAt:   now,
		}, asyncNotify)
	}
}

func deliverMergeResultNotification(ctx context.Context, notifier RunMergeNotifier, notice MergeCandidateNotification, asyncNotify bool) {
	if notifier == nil {
		return
	}
	if asyncNotify {
		if _, isDapr := notifier.(*daprRunMergeNotifier); isDapr {
			// Dispatch outside EnqueueCandidate's call stack so RunActor's Dapr turn lock releases first.
			go func(n RunMergeNotifier, msg MergeCandidateNotification) {
				callCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = n.OnMergeCandidateResult(callCtx, msg)
			}(notifier, notice)
			return
		}
	}
	_ = notifier.OnMergeCandidateResult(ctx, notice)
}

func (a *MergeActor) snapshotTicket(ctx context.Context, requestID string) (CandidateTicket, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, err := a.loadStateLocked(ctx)
	if err != nil {
		return CandidateTicket{}, false
	}
	if state.InFlight != nil && state.InFlight.RequestID == requestID {
		return cloneCandidateTicket(*state.InFlight), true
	}
	for _, t := range state.Queue {
		if t.RequestID == requestID {
			return cloneCandidateTicket(t), true
		}
	}
	for i := len(state.History) - 1; i >= 0; i-- {
		if state.History[i].RequestID == requestID {
			return cloneCandidateTicket(state.History[i]), true
		}
	}
	return CandidateTicket{}, false
}

func (a *MergeActor) effectiveActorID() string {
	return strings.TrimSpace(a.ID())
}

func (a *MergeActor) loadStateLocked(ctx context.Context) (persistedMergeState, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return persistedMergeState{}, errors.New("merge: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	exists, err := sm.Contains(ctx, mergeStateKey)
	if err != nil {
		return persistedMergeState{}, fmt.Errorf("merge: check actor state: %w", err)
	}
	if !exists {
		st := persistedMergeState{}
		st.ensureDefaults()
		return st, nil
	}
	var doc persistedMergeState
	if err := sm.Get(ctx, mergeStateKey, &doc); err != nil {
		return persistedMergeState{}, fmt.Errorf("merge: load actor state: %w", err)
	}
	doc.ensureDefaults()
	return doc.clone(), nil
}

func (a *MergeActor) saveStateLocked(ctx context.Context, state persistedMergeState) error {
	sm := a.GetStateManager()
	if sm == nil {
		return errors.New("merge: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := sm.Set(ctx, mergeStateKey, state.clone()); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("merge: stage actor state: %w", err)
	}
	if err := sm.Save(ctx); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("merge: save actor state: %w", err)
	}
	return nil
}

func findActiveOrTerminalTicket(state persistedMergeState, runID, mergeAddr string, mergeEpoch uint64, sourceAddr string, sourceEpoch uint64) (CandidateTicket, bool) {
	matches := func(t CandidateTicket) bool {
		return t.RunID == runID &&
			t.MergeNodeAddr == mergeAddr &&
			t.MergeEpoch == mergeEpoch &&
			t.SourceNodeAddr == sourceAddr &&
			t.SourceEpoch == sourceEpoch
	}
	if state.InFlight != nil && matches(*state.InFlight) {
		return cloneCandidateTicket(*state.InFlight), true
	}
	for _, t := range state.Queue {
		if matches(t) {
			return cloneCandidateTicket(t), true
		}
	}
	for i := len(state.History) - 1; i >= 0; i-- {
		t := state.History[i]
		if (t.Status == TicketStatusCompleted || t.Status == TicketStatusReworkRequired) && matches(t) {
			return cloneCandidateTicket(t), true
		}
	}
	return CandidateTicket{}, false
}

func matchesCancelFilter(t CandidateTicket, runID, mergeAddr, sourceAddr string) bool {
	if t.RunID != runID {
		return false
	}
	if mergeAddr != "" && t.MergeNodeAddr != mergeAddr {
		return false
	}
	if sourceAddr != "" && t.SourceNodeAddr != sourceAddr {
		return false
	}
	return true
}

func branchRegKey(runID, mergeNodeAddr string, mergeEpoch uint64) string {
	return runID + "\x00" + mergeNodeAddr + "\x00" + strconv.FormatUint(mergeEpoch, 10)
}

func collectRequiredKeys(t CandidateTicket) []string {
	seen := make(map[string]struct{}, len(t.RequiredEnvKeys)+2)
	for _, k := range t.RequiredEnvKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			seen[trimmed] = struct{}{}
		}
	}
	if t.TokenRef != nil {
		if trimmed := strings.TrimSpace(*t.TokenRef); trimmed != "" {
			seen[trimmed] = struct{}{}
		}
	}
	if t.SSHKeyRef != nil {
		if trimmed := strings.TrimSpace(*t.SSHKeyRef); trimmed != "" {
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

func buildRunMergeCandidateRequest(actorID string, t CandidateTicket, claim *registry.ClaimSlotResp, resolvedEnv map[string]string) daemon.RunMergeCandidateRequest {
	var targetPtr *string
	if t.SharedTargetBranch {
		b := t.TargetBranch
		targetPtr = &b
	}
	return daemon.RunMergeCandidateRequest{
		ProcessID:       claim.ProcessID,
		MergeActorID:    actorID,
		RequestID:       t.RequestID,
		LeaseID:         claim.LeaseID,
		LeaseKey:        claim.LeaseKey,
		RunID:           t.RunID,
		MergeNodeAddr:   t.MergeNodeAddr,
		Epoch:           t.MergeEpoch,
		RepoURL:         t.RepoURL,
		BasedSource:     t.BasedSource,
		BaseSHA:         t.BaseSHA,
		TargetBranch:    targetPtr,
		CandidateBranch: t.CandidateBranch,
		Auth: git.AuthConfig{
			TokenRef:  cloneStrPtr(t.TokenRef),
			SSHKeyRef: cloneStrPtr(t.SSHKeyRef),
		},
		ResolvedEnv: maps.Clone(resolvedEnv),
		Sandbox:     t.Sandbox,
		Gatekeepers: cloneGatekeepers(t.Gatekeepers),
		PlanBlobKey: t.PlanBlobKey,
		PlanSHA256:  t.PlanSHA256,
		BundlePath:  t.BundlePath,
		ImportsDir:  t.ImportsDir,
	}
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

func cloneGatekeepers(src []workerspec.GatekeeperConfig) []workerspec.GatekeeperConfig {
	if len(src) == 0 {
		return nil
	}
	out := make([]workerspec.GatekeeperConfig, len(src))
	for i, gk := range src {
		out[i] = gk.Clone()
	}
	return out
}

func cloneCandidateTicket(t CandidateTicket) CandidateTicket {
	cp := t
	cp.ExplicitTarget = cloneStrPtr(t.ExplicitTarget)
	cp.RunOverrides = maps.Clone(t.RunOverrides)
	cp.RequiredEnvKeys = slices.Clone(t.RequiredEnvKeys)
	cp.TokenRef = cloneStrPtr(t.TokenRef)
	cp.SSHKeyRef = cloneStrPtr(t.SSHKeyRef)
	cp.Auth = git.AuthConfig{
		TokenRef:  cloneStrPtr(t.Auth.TokenRef),
		SSHKeyRef: cloneStrPtr(t.Auth.SSHKeyRef),
	}
	cp.Gatekeepers = cloneGatekeepers(t.Gatekeepers)
	return cp
}

func cloneCandidateTickets(src []CandidateTicket) []CandidateTicket {
	if len(src) == 0 {
		return nil
	}
	out := make([]CandidateTicket, len(src))
	for i, t := range src {
		out[i] = cloneCandidateTicket(t)
	}
	return out
}

// Dapr production boundary implementations.

type daprConfigResolver struct {
	clientProvider func() daprclient.Client
}

func (r *daprConfigResolver) ResolveForNode(ctx context.Context, req *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error) {
	c := r.clientProvider()
	if c == nil {
		return nil, errors.New("merge: dapr client unavailable for ConfigActor")
	}
	return config.NewClientStub(c).ResolveForNode(ctx, req)
}

type daprSlotManager struct {
	clientProvider func() daprclient.Client
}

func (m *daprSlotManager) ClaimSlot(ctx context.Context, req *registry.ClaimSlotReq) (*registry.ClaimSlotResp, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("merge: dapr client unavailable for ProcessRegistryActor")
	}
	return registry.NewClientStub(c).ClaimSlot(ctx, req)
}

func (m *daprSlotManager) ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("merge: dapr client unavailable for ProcessRegistryActor")
	}
	return registry.NewClientStub(c).ReleaseSlot(ctx, req)
}

func (m *daprSlotManager) ReportSuspect(ctx context.Context, req *registry.ReportSuspectReq) (*registry.ReportSuspectResp, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("merge: dapr client unavailable for ProcessRegistryActor")
	}
	return registry.NewClientStub(c).ReportSuspect(ctx, req)
}

type daprMergeWorkerDispatcher struct {
	clientProvider func() daprclient.Client
}

func (d *daprMergeWorkerDispatcher) RunMergeCandidate(ctx context.Context, appID string, req daemon.RunMergeCandidateRequest) (daemon.RunMergeCandidateResponse, error) {
	c := d.clientProvider()
	if c == nil {
		return daemon.RunMergeCandidateResponse{}, errors.New("merge: dapr client unavailable for Worker run_merge_candidate")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return daemon.RunMergeCandidateResponse{}, err
	}
	raw, err := c.InvokeMethodWithContent(ctx, appID, "run_merge_candidate", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        payload,
	})
	if err != nil {
		return daemon.RunMergeCandidateResponse{}, err
	}
	var resp daemon.RunMergeCandidateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return daemon.RunMergeCandidateResponse{}, fmt.Errorf("decode run_merge_candidate response: %w", err)
	}
	return resp, nil
}

func (d *daprMergeWorkerDispatcher) AbortMergeCandidate(ctx context.Context, appID string, req daemon.AbortMergeRequest) (daemon.AbortMergeResponse, error) {
	c := d.clientProvider()
	if c == nil {
		return daemon.AbortMergeResponse{}, errors.New("merge: dapr client unavailable for Worker abort_merge")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return daemon.AbortMergeResponse{}, err
	}
	raw, err := c.InvokeMethodWithContent(ctx, appID, "abort_merge", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        payload,
	})
	if err != nil {
		return daemon.AbortMergeResponse{}, err
	}
	var resp daemon.AbortMergeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return daemon.AbortMergeResponse{}, fmt.Errorf("decode abort_merge response: %w", err)
	}
	return resp, nil
}

type daprRunMergeNotifier struct {
	clientProvider func() daprclient.Client
}

func (n *daprRunMergeNotifier) RegisterManagedBranch(ctx context.Context, req RegisterManagedBranchReq) error {
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

func (n *daprRunMergeNotifier) OnMergeCandidateResult(ctx context.Context, notice MergeCandidateNotification) error {
	c := n.clientProvider()
	if c == nil || strings.TrimSpace(notice.RunID) == "" {
		return nil
	}
	data, err := json.Marshal(notice)
	if err != nil {
		return err
	}
	_, err = c.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: "RunActor",
		ActorID:   notice.RunID,
		Method:    "OnMergeCandidateResult",
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
		return errors.New("merge: dapr client unavailable for reminder registration")
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
		return errors.New("merge: dapr client unavailable for reminder unregistration")
	}
	return c.UnregisterActorReminder(ctx, &daprclient.UnregisterActorReminderRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Name:      reminderName,
	})
}

// ClientStub provides a type-safe Dapr Actor client wrapper over MergeActor
// populated via daprclient.Client.ImplActorClientStub.
type ClientStub struct {
	ActorID               string
	EnqueueCandidate      func(context.Context, *EnqueueCandidateReq) (*EnqueueCandidateResp, error)
	ReportCandidateResult func(context.Context, *daemon.MergeOutcomeReport) (*ReportCandidateResultResp, error)
	CancelByRun           func(context.Context, *CancelByRunReq) (*CancelByRunResp, error)
	OnWorkerDead          func(context.Context, *registry.WorkerDeadNotification) (*OnWorkerDeadResp, error)
	GetState              func(context.Context, *GetMergeStateReq) (*GetMergeStateResp, error)
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

// NewClientStubForTarget computes MergeActorID(repoURL, effectiveTargetBranch) and constructs a bound ClientStub.
func NewClientStubForTarget(repoURL, effectiveTargetBranch string, client daprclient.Client) (*ClientStub, error) {
	actorID, err := MergeActorID(repoURL, effectiveTargetBranch)
	if err != nil {
		return nil, err
	}
	return NewClientStub(actorID, client), nil
}
