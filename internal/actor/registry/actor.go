package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/daprhost"
)

const registryStateKey = "process_registry_v1"

type persistedRegistryState struct {
	Processes map[string]ProcessRecord `json:"processes,omitempty"`
	Leases    map[string]SlotLease     `json:"leases,omitempty"`
	LeaseKeys map[string]string        `json:"lease_keys,omitempty"`
}

func (s persistedRegistryState) clone() persistedRegistryState {
	return persistedRegistryState{
		Processes: maps.Clone(s.Processes),
		Leases:    maps.Clone(s.Leases),
		LeaseKeys: maps.Clone(s.LeaseKeys),
	}
}

func (s *persistedRegistryState) ensureMaps() {
	if s.Processes == nil {
		s.Processes = make(map[string]ProcessRecord)
	}
	if s.Leases == nil {
		s.Leases = make(map[string]SlotLease)
	}
	if s.LeaseKeys == nil {
		s.LeaseKeys = make(map[string]string)
	}
}

// Options configures ProcessRegistryActor timeouts, clocks, and Dapr boundaries.
type Options struct {
	ClientProvider    func() daprclient.Client
	Prober            Prober
	DeadNotifier      DeadNotifier
	ReminderScheduler ReminderScheduler
	Clock             func() time.Time
	HeartbeatInterval time.Duration
	SuspectTimeout    time.Duration
	ProbeInterval     time.Duration
	DeadThreshold     int
	PingTimeout       time.Duration
	ReminderName      string
}

// ProcessRegistryActor tracks worker liveness across HEALTHY -> SUSPECT -> DEAD and
// allocates least-load execution slots with lease-key idempotency.
type ProcessRegistryActor struct {
	actor.ServerImplBaseCtx

	mu                sync.Mutex
	clientProvider    func() daprclient.Client
	prober            Prober
	deadNotifier      DeadNotifier
	reminderScheduler ReminderScheduler
	clock             func() time.Time
	heartbeatInterval time.Duration
	suspectTimeout    time.Duration
	probeInterval     time.Duration
	deadThreshold     int
	pingTimeout       time.Duration
	reminderName      string
	reminderEnsured   bool
}

// NewProcessRegistryActor constructs a ProcessRegistryActor with defaults and optional test overrides.
func NewProcessRegistryActor(opts Options) *ProcessRegistryActor {
	hb := opts.HeartbeatInterval
	if hb <= 0 {
		hb = DefaultHeartbeatInterval
	}
	suspect := opts.SuspectTimeout
	if suspect <= 0 {
		suspect = DefaultSuspectTimeout
	}
	probe := opts.ProbeInterval
	if probe <= 0 {
		probe = DefaultProbeInterval
	}
	deadThresh := opts.DeadThreshold
	if deadThresh <= 0 {
		deadThresh = DefaultDeadThreshold
	}
	pingTimeout := opts.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 2 * time.Second
		if probe > 0 && probe < pingTimeout {
			pingTimeout = probe
		}
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	reminderName := strings.TrimSpace(opts.ReminderName)
	if reminderName == "" {
		reminderName = DefaultReminderName
	}

	a := &ProcessRegistryActor{
		clientProvider:    opts.ClientProvider,
		prober:            opts.Prober,
		deadNotifier:      opts.DeadNotifier,
		reminderScheduler: opts.ReminderScheduler,
		clock:             clock,
		heartbeatInterval: hb,
		suspectTimeout:    suspect,
		probeInterval:     probe,
		deadThreshold:     deadThresh,
		pingTimeout:       pingTimeout,
		reminderName:      reminderName,
	}
	if a.prober == nil && opts.ClientProvider != nil {
		a.prober = &daprProber{clientProvider: opts.ClientProvider}
	}
	if a.deadNotifier == nil && opts.ClientProvider != nil {
		a.deadNotifier = &daprDeadNotifier{clientProvider: opts.ClientProvider}
	}
	if a.reminderScheduler == nil && opts.ClientProvider != nil {
		a.reminderScheduler = &daprReminderScheduler{clientProvider: opts.ClientProvider}
	}
	return a
}

// Type implements actor.ServerContext.
func (a *ProcessRegistryActor) Type() string {
	return ActorType
}

// SetStateManager binds state persistence to daprhost.NewActorStateManager when clientProvider
// supplies a Dapr client, while preserving direct StateManagerContext injection in Tier 1 unit tests.
func (a *ProcessRegistryActor) SetStateManager(sm actor.StateManagerContext) {
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

// RegisterProcess registers a healthy worker process and ensures the liveness sweep reminder is active.
func (a *ProcessRegistryActor) RegisterProcess(ctx context.Context, req *RegisterProcessReq) (*RegisterProcessResp, error) {
	if req == nil {
		return nil, errors.New("registry: register request is nil")
	}
	processID := strings.TrimSpace(req.ProcessID)
	appID := strings.TrimSpace(req.AppID)
	if processID == "" {
		return nil, errors.New("registry: process_id is required")
	}
	if appID == "" {
		return nil, errors.New("registry: app_id is required")
	}
	maxConc := req.MaxConcurrency
	if maxConc <= 0 {
		maxConc = DefaultWorkerMaxConcurrency
	}
	arch := strings.TrimSpace(req.Arch)

	a.mu.Lock()
	state, err := a.loadState(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	state.ensureMaps()

	if existing, ok := state.Processes[processID]; ok {
		if existing.Status == StatusDead {
			a.mu.Unlock()
			return nil, fmt.Errorf("registry: process_id %q is DEAD and cannot be re-registered; rotate process_id", processID)
		}
		existing.AppID = appID
		existing.MaxConcurrency = maxConc
		if arch != "" {
			existing.Arch = arch
		}
		existing.Status = StatusHealthy
		existing.LastHeartbeatAt = a.clock()
		existing.ConsecutiveFailures = 0
		existing.LastProbeError = ""
		state.Processes[processID] = existing
	} else {
		now := a.clock()
		state.Processes[processID] = ProcessRecord{
			ProcessID:       processID,
			AppID:           appID,
			MaxConcurrency:  maxConc,
			Arch:            arch,
			Status:          StatusHealthy,
			RegisteredAt:    now,
			LastHeartbeatAt: now,
		}
	}

	if err := a.saveState(ctx, state); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	needReminder := !a.reminderEnsured && a.reminderScheduler != nil
	actorID := a.effectiveActorID()
	a.mu.Unlock()

	if needReminder {
		if err := a.reminderScheduler.EnsureProbeReminder(ctx, ActorType, actorID, a.reminderName, a.probeInterval, a.probeInterval); err != nil {
			return nil, fmt.Errorf("registry: register liveness reminder: %w", err)
		}
		a.mu.Lock()
		a.reminderEnsured = true
		a.mu.Unlock()
	}

	return &RegisterProcessResp{
		Registered:          true,
		Status:              StatusHealthy,
		HeartbeatIntervalMs: a.heartbeatInterval.Milliseconds(),
	}, nil
}

// Heartbeat refreshes a worker's liveness timestamp or forces self-fencing when unknown or DEAD.
func (a *ProcessRegistryActor) Heartbeat(ctx context.Context, req *HeartbeatReq) (*HeartbeatResp, error) {
	if req == nil {
		return nil, errors.New("registry: heartbeat request is nil")
	}
	processID := strings.TrimSpace(req.ProcessID)
	if processID == "" {
		return nil, errors.New("registry: process_id is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadState(ctx)
	if err != nil {
		return nil, err
	}
	proc, exists := state.Processes[processID]
	if !exists || proc.Status == StatusDead {
		return &HeartbeatResp{
			Status:        StatusDead,
			FenceRequired: true,
		}, nil
	}

	proc.Status = StatusHealthy
	proc.LastHeartbeatAt = a.clock()
	proc.ConsecutiveFailures = 0
	proc.LastProbeError = ""
	state.Processes[processID] = proc

	if err := a.saveState(ctx, state); err != nil {
		return nil, err
	}
	return &HeartbeatResp{
		Status:        StatusHealthy,
		FenceRequired: false,
	}, nil
}

// ReportSuspect transitions a HEALTHY worker to SUSPECT after a dispatch failure without purging active leases.
func (a *ProcessRegistryActor) ReportSuspect(ctx context.Context, req *ReportSuspectReq) (*ReportSuspectResp, error) {
	if req == nil {
		return nil, errors.New("registry: report suspect request is nil")
	}
	processID := strings.TrimSpace(req.ProcessID)
	if processID == "" {
		return nil, errors.New("registry: process_id is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadState(ctx)
	if err != nil {
		return nil, err
	}
	proc, exists := state.Processes[processID]
	if !exists {
		return &ReportSuspectResp{Status: StatusDead}, nil
	}
	if proc.Status == StatusHealthy {
		proc.Status = StatusSuspect
		if reason := strings.TrimSpace(req.Reason); reason != "" {
			proc.LastProbeError = reason
		}
		state.Processes[processID] = proc
		if err := a.saveState(ctx, state); err != nil {
			return nil, err
		}
	}
	return &ReportSuspectResp{Status: proc.Status}, nil
}

// ClaimSlot idempotently returns an existing valid lease for LeaseKey or allocates one on
// the HEALTHY matching-arch worker with the lowest utilization ratio (ActiveSlots / MaxConcurrency).
func (a *ProcessRegistryActor) ClaimSlot(ctx context.Context, req *ClaimSlotReq) (*ClaimSlotResp, error) {
	if req == nil {
		return nil, errors.New("registry: claim slot request is nil")
	}
	leaseKey := strings.TrimSpace(req.LeaseKey)
	if leaseKey == "" {
		return nil, errors.New("registry: lease_key is required")
	}
	ownerType := strings.TrimSpace(req.OwnerActorType)
	ownerID := strings.TrimSpace(req.OwnerActorID)
	if ownerType == "" || ownerID == "" {
		return nil, errors.New("registry: owner_actor_type and owner_actor_id are required")
	}
	requiredArch := strings.TrimSpace(req.RequiredArch)

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.ensureMaps()

	// Idempotent lookup by LeaseKey when the owning worker is still non-DEAD.
	if existingLeaseID, ok := state.LeaseKeys[leaseKey]; ok {
		if existingLease, found := state.Leases[existingLeaseID]; found {
			if proc, procOK := state.Processes[existingLease.ProcessID]; procOK && proc.Status != StatusDead {
				return &ClaimSlotResp{
					Granted:   true,
					LeaseID:   existingLease.LeaseID,
					LeaseKey:  existingLease.LeaseKey,
					ProcessID: existingLease.ProcessID,
					AppID:     existingLease.AppID,
				}, nil
			}
			// Stale lease referencing a DEAD or missing worker: clean it up before fresh allocation.
			a.removeLeaseLocked(&state, existingLeaseID)
		} else {
			delete(state.LeaseKeys, leaseKey)
		}
	}

	selected, found := selectLeastLoadedWorker(state.Processes, requiredArch)
	if !found {
		return &ClaimSlotResp{
			Granted: false,
			Reason:  "no healthy worker with available capacity",
		}, nil
	}

	lease := SlotLease{
		LeaseID:        uuid.NewString(),
		LeaseKey:       leaseKey,
		ProcessID:      selected.ProcessID,
		AppID:          selected.AppID,
		OwnerActorType: ownerType,
		OwnerActorID:   ownerID,
		AcquiredAt:     a.clock(),
	}
	selected.ActiveSlots++
	state.Processes[selected.ProcessID] = selected
	state.Leases[lease.LeaseID] = lease
	state.LeaseKeys[lease.LeaseKey] = lease.LeaseID

	if err := a.saveState(ctx, state); err != nil {
		return nil, err
	}
	return &ClaimSlotResp{
		Granted:   true,
		LeaseID:   lease.LeaseID,
		LeaseKey:  lease.LeaseKey,
		ProcessID: lease.ProcessID,
		AppID:     lease.AppID,
	}, nil
}

// ReleaseSlot idempotently releases an active slot lease by LeaseID or LeaseKey.
func (a *ProcessRegistryActor) ReleaseSlot(ctx context.Context, req *ReleaseSlotReq) (*ReleaseSlotResp, error) {
	if req == nil {
		return nil, errors.New("registry: release slot request is nil")
	}
	leaseID := strings.TrimSpace(req.LeaseID)
	leaseKey := strings.TrimSpace(req.LeaseKey)
	if leaseID == "" && leaseKey == "" {
		return nil, errors.New("registry: lease_id or lease_key is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.ensureMaps()

	if leaseID == "" {
		leaseID = state.LeaseKeys[leaseKey]
	}
	if leaseID == "" {
		return &ReleaseSlotResp{Released: false}, nil
	}
	if _, exists := state.Leases[leaseID]; !exists {
		return &ReleaseSlotResp{Released: false}, nil
	}

	a.removeLeaseLocked(&state, leaseID)
	if err := a.saveState(ctx, state); err != nil {
		return nil, err
	}
	return &ReleaseSlotResp{Released: true}, nil
}

// ListProcesses returns sorted snapshots of all registered worker processes and active leases.
func (a *ProcessRegistryActor) ListProcesses(ctx context.Context, _ *ListProcessesReq) (*ListProcessesResp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.loadState(ctx)
	if err != nil {
		return nil, err
	}

	procs := make([]ProcessRecord, 0, len(state.Processes))
	for _, p := range state.Processes {
		procs = append(procs, p)
	}
	sort.Slice(procs, func(i, j int) bool {
		return procs[i].ProcessID < procs[j].ProcessID
	})

	leases := make([]SlotLease, 0, len(state.Leases))
	for _, l := range state.Leases {
		leases = append(leases, l)
	}
	sort.Slice(leases, func(i, j int) bool {
		return leases[i].LeaseID < leases[j].LeaseID
	})

	return &ListProcessesResp{
		Processes: procs,
		Leases:    leases,
	}, nil
}

// ReminderCall executes periodic liveness sweeps:
//  1. HEALTHY workers whose heartbeat elapsed >= SuspectTimeout transition to SUSPECT.
//  2. SUSPECT workers are actively probed via ping outside the actor mutex.
//  3. Probe success restores HEALTHY; DeadThreshold consecutive failures transition to DEAD,
//     purge all active leases, explicitly persist state (Set + Save, since go-sdk InvokeReminder
//     does NOT auto-save), and dispatch OnWorkerDead notifications to affected lease owners.
func (a *ProcessRegistryActor) ReminderCall(_ string, _ []byte, _ string, _ string) {
	ctx := context.Background()

	type probeTarget struct {
		processID           string
		appID               string
		snapshotHeartbeated time.Time
	}

	a.mu.Lock()
	state, err := a.loadState(ctx)
	if err != nil {
		a.mu.Unlock()
		return
	}
	state.ensureMaps()

	now := a.clock()
	changed := false
	targets := make([]probeTarget, 0)

	for pid, proc := range state.Processes {
		if proc.Status == StatusHealthy && now.Sub(proc.LastHeartbeatAt) >= a.suspectTimeout {
			proc.Status = StatusSuspect
			state.Processes[pid] = proc
			changed = true
		}
		if proc.Status == StatusSuspect {
			targets = append(targets, probeTarget{
				processID:           proc.ProcessID,
				appID:               proc.AppID,
				snapshotHeartbeated: proc.LastHeartbeatAt,
			})
		}
	}
	if changed {
		_ = a.saveState(ctx, state)
	}
	prober := a.prober
	notifier := a.deadNotifier
	a.mu.Unlock()

	if len(targets) == 0 || prober == nil {
		return
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].processID < targets[j].processID
	})

	type probeOutcome struct {
		target probeTarget
		err    error
	}
	outcomes := make([]probeOutcome, 0, len(targets))
	for _, tgt := range targets {
		pCtx, cancel := context.WithTimeout(ctx, a.pingTimeout)
		pErr := prober.Ping(pCtx, tgt.appID, tgt.processID)
		cancel()
		outcomes = append(outcomes, probeOutcome{target: tgt, err: pErr})
	}

	type pendingNotice struct {
		ownerType string
		ownerID   string
		notice    WorkerDeadNotification
	}
	var notifications []pendingNotice

	a.mu.Lock()
	state, err = a.loadState(ctx)
	if err != nil {
		a.mu.Unlock()
		return
	}
	state.ensureMaps()

	probeTime := a.clock()
	stateMutated := false

	for _, out := range outcomes {
		proc, ok := state.Processes[out.target.processID]
		if !ok || proc.Status != StatusSuspect {
			continue
		}
		// If a fresh Heartbeat arrived while Ping was in flight, do not overwrite it with a stale failure.
		if proc.LastHeartbeatAt.After(out.target.snapshotHeartbeated) {
			continue
		}

		if out.err == nil {
			proc.Status = StatusHealthy
			proc.ConsecutiveFailures = 0
			proc.LastProbeError = ""
			if probeTime.After(proc.LastHeartbeatAt) {
				proc.LastHeartbeatAt = probeTime
			}
			state.Processes[proc.ProcessID] = proc
			stateMutated = true
			continue
		}

		proc.ConsecutiveFailures++
		proc.LastProbeError = out.err.Error()
		stateMutated = true

		if proc.ConsecutiveFailures >= a.deadThreshold {
			proc.Status = StatusDead
			proc.ActiveSlots = 0
			state.Processes[proc.ProcessID] = proc

			reason := fmt.Sprintf("probe threshold exceeded (%d): %s", a.deadThreshold, out.err.Error())
			purged := purgeProcessLeasesLocked(&state, proc.ProcessID)
			for _, lease := range purged {
				notifications = append(notifications, pendingNotice{
					ownerType: lease.OwnerActorType,
					ownerID:   lease.OwnerActorID,
					notice: WorkerDeadNotification{
						LeaseID:   lease.LeaseID,
						LeaseKey:  lease.LeaseKey,
						ProcessID: lease.ProcessID,
						AppID:     lease.AppID,
						Reason:    reason,
					},
				})
			}
		} else {
			proc.Status = StatusSuspect
			state.Processes[proc.ProcessID] = proc
		}
	}

	if stateMutated {
		_ = a.saveState(ctx, state)
	}
	a.mu.Unlock()

	if notifier != nil {
		for _, n := range notifications {
			nCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = notifier.NotifyWorkerDead(nCtx, n.ownerType, n.ownerID, n.notice)
			cancel()
		}
	}
}

func selectLeastLoadedWorker(processes map[string]ProcessRecord, requiredArch string) (ProcessRecord, bool) {
	var (
		best  ProcessRecord
		found bool
	)
	for _, proc := range processes {
		if proc.Status != StatusHealthy {
			continue
		}
		if proc.MaxConcurrency <= 0 || proc.ActiveSlots >= proc.MaxConcurrency {
			continue
		}
		if requiredArch != "" && !strings.EqualFold(strings.TrimSpace(proc.Arch), requiredArch) {
			continue
		}
		if !found || isBetterWorker(proc, best) {
			best = proc
			found = true
		}
	}
	return best, found
}

func isBetterWorker(candidate, current ProcessRecord) bool {
	// Compare utilization ratios candidate.ActiveSlots/candidate.MaxConcurrency < current.ActiveSlots/current.MaxConcurrency
	lhs := int64(candidate.ActiveSlots) * int64(current.MaxConcurrency)
	rhs := int64(current.ActiveSlots) * int64(candidate.MaxConcurrency)
	if lhs != rhs {
		return lhs < rhs
	}
	remCand := candidate.MaxConcurrency - candidate.ActiveSlots
	remCurr := current.MaxConcurrency - current.ActiveSlots
	if remCand != remCurr {
		return remCand > remCurr
	}
	return candidate.ProcessID < current.ProcessID
}

func (a *ProcessRegistryActor) removeLeaseLocked(state *persistedRegistryState, leaseID string) {
	lease, ok := state.Leases[leaseID]
	if !ok {
		return
	}
	delete(state.Leases, leaseID)
	if mappedID, exists := state.LeaseKeys[lease.LeaseKey]; exists && mappedID == leaseID {
		delete(state.LeaseKeys, lease.LeaseKey)
	}
	if proc, exists := state.Processes[lease.ProcessID]; exists {
		if proc.ActiveSlots > 0 {
			proc.ActiveSlots--
		}
		state.Processes[lease.ProcessID] = proc
	}
}

func purgeProcessLeasesLocked(state *persistedRegistryState, processID string) []SlotLease {
	purged := make([]SlotLease, 0)
	for leaseID, lease := range state.Leases {
		if lease.ProcessID != processID {
			continue
		}
		purged = append(purged, lease)
		delete(state.Leases, leaseID)
		if mappedID, ok := state.LeaseKeys[lease.LeaseKey]; ok && mappedID == leaseID {
			delete(state.LeaseKeys, lease.LeaseKey)
		}
	}
	sort.Slice(purged, func(i, j int) bool {
		return purged[i].LeaseID < purged[j].LeaseID
	})
	return purged
}

func (a *ProcessRegistryActor) effectiveActorID() string {
	if id := strings.TrimSpace(a.ID()); id != "" {
		return id
	}
	return GlobalActorID
}

func (a *ProcessRegistryActor) loadState(ctx context.Context) (persistedRegistryState, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return persistedRegistryState{}, errors.New("registry: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	exists, err := sm.Contains(ctx, registryStateKey)
	if err != nil {
		return persistedRegistryState{}, fmt.Errorf("registry: check actor state: %w", err)
	}
	if !exists {
		return persistedRegistryState{}, nil
	}
	var doc persistedRegistryState
	if err := sm.Get(ctx, registryStateKey, &doc); err != nil {
		return persistedRegistryState{}, fmt.Errorf("registry: load actor state: %w", err)
	}
	return doc.clone(), nil
}

func (a *ProcessRegistryActor) saveState(ctx context.Context, state persistedRegistryState) error {
	sm := a.GetStateManager()
	if sm == nil {
		return errors.New("registry: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := sm.Set(ctx, registryStateKey, state.clone()); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("registry: stage actor state: %w", err)
	}
	if err := sm.Save(ctx); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("registry: save actor state: %w", err)
	}
	return nil
}

type daprProber struct {
	clientProvider func() daprclient.Client
}

func (p *daprProber) Ping(ctx context.Context, appID, processID string) error {
	c := p.clientProvider()
	if c == nil {
		return errors.New("registry: dapr client unavailable for ping")
	}
	payload, err := json.Marshal(PingRequest{ProcessID: processID})
	if err != nil {
		return err
	}
	raw, err := c.InvokeMethodWithContent(ctx, appID, "ping", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        payload,
	})
	if err != nil {
		return err
	}
	var resp PingResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode ping response from %s: %w", appID, err)
	}
	if !resp.OK || resp.ProcessID != processID {
		return fmt.Errorf("ping mismatch from %s: ok=%v got_process_id=%q want=%q", appID, resp.OK, resp.ProcessID, processID)
	}
	return nil
}

type daprDeadNotifier struct {
	clientProvider func() daprclient.Client
}

func (n *daprDeadNotifier) NotifyWorkerDead(ctx context.Context, ownerActorType, ownerActorID string, notice WorkerDeadNotification) error {
	c := n.clientProvider()
	if c == nil {
		return errors.New("registry: dapr client unavailable for OnWorkerDead")
	}
	if strings.TrimSpace(ownerActorType) == "" || strings.TrimSpace(ownerActorID) == "" {
		return nil
	}
	data, err := json.Marshal(notice)
	if err != nil {
		return err
	}
	_, err = c.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: ownerActorType,
		ActorID:   ownerActorID,
		Method:    "OnWorkerDead",
		Data:      data,
	})
	return err
}

type daprReminderScheduler struct {
	clientProvider func() daprclient.Client
}

func (s *daprReminderScheduler) EnsureProbeReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error {
	c := s.clientProvider()
	if c == nil {
		return errors.New("registry: dapr client unavailable for reminder registration")
	}
	return c.RegisterActorReminder(ctx, &daprclient.RegisterActorReminderRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Name:      reminderName,
		DueTime:   dueTime.String(),
		Period:    period.String(),
	})
}

// ClientStub provides a type-safe Dapr Actor client wrapper over ProcessRegistryActor("global")
// populated via daprclient.Client.ImplActorClientStub.
type ClientStub struct {
	actorID         string
	RegisterProcess func(context.Context, *RegisterProcessReq) (*RegisterProcessResp, error)
	Heartbeat       func(context.Context, *HeartbeatReq) (*HeartbeatResp, error)
	ReportSuspect   func(context.Context, *ReportSuspectReq) (*ReportSuspectResp, error)
	ClaimSlot       func(context.Context, *ClaimSlotReq) (*ClaimSlotResp, error)
	ReleaseSlot     func(context.Context, *ReleaseSlotReq) (*ReleaseSlotResp, error)
	ListProcesses   func(context.Context, *ListProcessesReq) (*ListProcessesResp, error)
}

// Type implements actor.Client.
func (c *ClientStub) Type() string {
	return ActorType
}

// ID implements actor.Client.
func (c *ClientStub) ID() string {
	if c.actorID == "" {
		return GlobalActorID
	}
	return c.actorID
}

// NewClientStub constructs a ClientStub bound to client via ImplActorClientStub.
func NewClientStub(client daprclient.Client) *ClientStub {
	stub := &ClientStub{actorID: GlobalActorID}
	if client != nil {
		client.ImplActorClientStub(stub)
	}
	return stub
}
