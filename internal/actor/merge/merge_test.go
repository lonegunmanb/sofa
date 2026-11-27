package merge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/git"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
	sha4 = "4444444444444444444444444444444444444444"
)

type memoryStateManager struct {
	mu        sync.Mutex
	persisted map[string][]byte
	staged    map[string]stagedEntry
	saveCount int
}

type stagedEntry struct {
	remove bool
	data   []byte
}

func newMemoryStateManager() *memoryStateManager {
	return &memoryStateManager{
		persisted: make(map[string][]byte),
		staged:    make(map[string]stagedEntry),
	}
}

func (m *memoryStateManager) Add(_ context.Context, stateName string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.staged[stateName]; ok {
		return fmt.Errorf("duplicate staged key %q", stateName)
	}
	if _, ok := m.persisted[stateName]; ok {
		return fmt.Errorf("duplicate persisted key %q", stateName)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.staged[stateName] = stagedEntry{data: raw}
	return nil
}

func (m *memoryStateManager) Get(_ context.Context, stateName string, reply any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.staged[stateName]; ok {
		if entry.remove {
			return fmt.Errorf("state %q marked for removal", stateName)
		}
		return json.Unmarshal(entry.data, reply)
	}
	raw, ok := m.persisted[stateName]
	if !ok {
		return fmt.Errorf("state %q not found", stateName)
	}
	return json.Unmarshal(raw, reply)
}

func (m *memoryStateManager) Set(_ context.Context, stateName string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.staged[stateName] = stagedEntry{data: raw}
	return nil
}

func (m *memoryStateManager) SetWithTTL(ctx context.Context, stateName string, value any, _ time.Duration) error {
	return m.Set(ctx, stateName, value)
}

func (m *memoryStateManager) Remove(_ context.Context, stateName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staged[stateName] = stagedEntry{remove: true}
	return nil
}

func (m *memoryStateManager) Contains(_ context.Context, stateName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.staged[stateName]; ok {
		return !entry.remove, nil
	}
	_, ok := m.persisted[stateName]
	return ok, nil
}

func (m *memoryStateManager) Save(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, entry := range m.staged {
		if entry.remove {
			delete(m.persisted, k)
		} else {
			m.persisted[k] = append([]byte(nil), entry.data...)
		}
	}
	m.staged = make(map[string]stagedEntry)
	m.saveCount++
	return nil
}

func (m *memoryStateManager) Flush(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staged = make(map[string]stagedEntry)
}

func (m *memoryStateManager) SaveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCount
}

type fakeConfigResolver struct {
	mu     sync.Mutex
	values map[string]string
	err    error
	calls  []config.ResolveForNodeReq
}

func (f *fakeConfigResolver) ResolveForNode(_ context.Context, req *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, *req)
	if f.err != nil {
		return nil, f.err
	}
	vals := make(map[string]string)
	var missing []string
	for _, k := range req.Keys {
		if v, ok := f.values[k]; ok {
			vals[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	return &config.ResolveForNodeResp{Values: vals, MissingKeys: missing}, nil
}

type fakeSlotManager struct {
	mu       sync.Mutex
	capacity int
	active   map[string]registry.SlotLease
	suspects []registry.ReportSuspectReq
	releases []registry.ReleaseSlotReq
	claims   []registry.ClaimSlotReq
	seq      int
	nextPID  string
	nextApp  string
}

func newFakeSlotManager(capacity int) *fakeSlotManager {
	return &fakeSlotManager{
		capacity: capacity,
		active:   make(map[string]registry.SlotLease),
		nextPID:  "proc-1",
		nextApp:  "sofa-worker-proc-1",
	}
}

func (m *fakeSlotManager) ClaimSlot(_ context.Context, req *registry.ClaimSlotReq) (*registry.ClaimSlotResp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims = append(m.claims, *req)
	for _, l := range m.active {
		if l.LeaseKey == req.LeaseKey {
			return &registry.ClaimSlotResp{
				Granted:   true,
				LeaseID:   l.LeaseID,
				LeaseKey:  l.LeaseKey,
				ProcessID: l.ProcessID,
				AppID:     l.AppID,
			}, nil
		}
	}
	if len(m.active) >= m.capacity {
		return &registry.ClaimSlotResp{
			Granted: false,
			Reason:  "no healthy worker with available capacity",
		}, nil
	}
	m.seq++
	lid := fmt.Sprintf("lease-%d", m.seq)
	lease := registry.SlotLease{
		LeaseID:        lid,
		LeaseKey:       req.LeaseKey,
		ProcessID:      m.nextPID,
		AppID:          m.nextApp,
		OwnerActorType: req.OwnerActorType,
		OwnerActorID:   req.OwnerActorID,
	}
	m.active[lid] = lease
	return &registry.ClaimSlotResp{
		Granted:   true,
		LeaseID:   lease.LeaseID,
		LeaseKey:  lease.LeaseKey,
		ProcessID: lease.ProcessID,
		AppID:     lease.AppID,
	}, nil
}

func (m *fakeSlotManager) ReleaseSlot(_ context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releases = append(m.releases, *req)
	released := false
	if req.LeaseID != "" {
		if _, ok := m.active[req.LeaseID]; ok {
			delete(m.active, req.LeaseID)
			released = true
		}
	} else if req.LeaseKey != "" {
		for id, l := range m.active {
			if l.LeaseKey == req.LeaseKey {
				delete(m.active, id)
				released = true
				break
			}
		}
	}
	return &registry.ReleaseSlotResp{Released: released}, nil
}

func (m *fakeSlotManager) ReportSuspect(_ context.Context, req *registry.ReportSuspectReq) (*registry.ReportSuspectResp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suspects = append(m.suspects, *req)
	return &registry.ReportSuspectResp{Status: registry.StatusSuspect}, nil
}

func (m *fakeSlotManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

type dispatchedMergeCall struct {
	AppID string
	Req   daemon.RunMergeCandidateRequest
}

type abortedMergeCall struct {
	AppID string
	Req   daemon.AbortMergeRequest
}

type fakeMergeWorkerDispatcher struct {
	mu       sync.Mutex
	started  []dispatchedMergeCall
	aborted  []abortedMergeCall
	startErr error
	onStart  func(appID string, req daemon.RunMergeCandidateRequest) (daemon.RunMergeCandidateResponse, error)
}

func (d *fakeMergeWorkerDispatcher) RunMergeCandidate(_ context.Context, appID string, req daemon.RunMergeCandidateRequest) (daemon.RunMergeCandidateResponse, error) {
	d.mu.Lock()
	d.started = append(d.started, dispatchedMergeCall{AppID: appID, Req: req})
	onStart := d.onStart
	err := d.startErr
	d.mu.Unlock()

	if onStart != nil {
		return onStart(appID, req)
	}
	if err != nil {
		return daemon.RunMergeCandidateResponse{}, err
	}
	return daemon.RunMergeCandidateResponse{Accepted: true, ProcessID: req.ProcessID}, nil
}

func (d *fakeMergeWorkerDispatcher) AbortMergeCandidate(_ context.Context, appID string, req daemon.AbortMergeRequest) (daemon.AbortMergeResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.aborted = append(d.aborted, abortedMergeCall{AppID: appID, Req: req})
	return daemon.AbortMergeResponse{Aborted: true}, nil
}

func (d *fakeMergeWorkerDispatcher) Started() []dispatchedMergeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]dispatchedMergeCall, len(d.started))
	copy(out, d.started)
	return out
}

func (d *fakeMergeWorkerDispatcher) Aborted() []abortedMergeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]abortedMergeCall, len(d.aborted))
	copy(out, d.aborted)
	return out
}

type fakeRunMergeNotifier struct {
	mu            sync.Mutex
	branches      []merge.RegisterManagedBranchReq
	notifications []merge.MergeCandidateNotification
	callOrder     []string
	regErr        error
}

func (n *fakeRunMergeNotifier) RegisterManagedBranch(_ context.Context, req merge.RegisterManagedBranchReq) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.callOrder = append(n.callOrder, "register:"+req.Branch)
	if n.regErr != nil {
		return n.regErr
	}
	n.branches = append(n.branches, req)
	return nil
}

func (n *fakeRunMergeNotifier) OnMergeCandidateResult(_ context.Context, notice merge.MergeCandidateNotification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.callOrder = append(n.callOrder, "result:"+notice.SourceNodeAddr)
	n.notifications = append(n.notifications, notice)
	return nil
}

func (n *fakeRunMergeNotifier) Branches() []merge.RegisterManagedBranchReq {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]merge.RegisterManagedBranchReq, len(n.branches))
	copy(out, n.branches)
	return out
}

func (n *fakeRunMergeNotifier) Notifications() []merge.MergeCandidateNotification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]merge.MergeCandidateNotification, len(n.notifications))
	copy(out, n.notifications)
	return out
}

type fakeReminderScheduler struct {
	mu           sync.Mutex
	registered   []string
	unregistered []string
}

func (r *fakeReminderScheduler) RegisterSlotRetryReminder(_ context.Context, actorType, actorID, reminderName string, _, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registered = append(r.registered, actorType+"/"+actorID+"/"+reminderName)
	return nil
}

func (r *fakeReminderScheduler) UnregisterSlotRetryReminder(_ context.Context, actorType, actorID, reminderName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unregistered = append(r.unregistered, actorType+"/"+actorID+"/"+reminderName)
	return nil
}

func newTestMergeActor(t *testing.T, actorID string, slotMgr *fakeSlotManager, disp *fakeMergeWorkerDispatcher, cfg *fakeConfigResolver, notifier *fakeRunMergeNotifier, rem *fakeReminderScheduler) (*merge.MergeActor, *memoryStateManager) {
	t.Helper()
	sm := newMemoryStateManager()
	a := merge.NewMergeActor(merge.Options{
		ConfigResolver:    cfg,
		SlotManager:       slotMgr,
		WorkerDispatcher:  disp,
		RunNotifier:       notifier,
		ReminderScheduler: rem,
		SlotRetryInterval: 50 * time.Millisecond,
	})
	a.SetID(actorID)
	a.SetStateManager(sm)
	return a, sm
}

func TestMergeActorIDAndEffectiveTargetBranch(t *testing.T) {
	t.Parallel()

	// Default target branch when explicitTarget is nil or whitespace.
	bDefault, shared, err := merge.EffectiveTargetBranch("run-1", "merge.integrate", nil)
	require.NoError(t, err)
	assert.False(t, shared)
	assert.Equal(t, git.WorkBranchName("run-1", "merge.integrate"), bDefault)

	blank := "   "
	bBlank, sharedBlank, err := merge.EffectiveTargetBranch("run-1", "merge.integrate", &blank)
	require.NoError(t, err)
	assert.False(t, sharedBlank)
	assert.Equal(t, bDefault, bBlank)

	// Explicit shared target branch normalizes refs/heads/ prefix and marks shared=true.
	explicit := "refs/heads/release/v1"
	bShared, isShared, err := merge.EffectiveTargetBranch("run-1", "merge.integrate", &explicit)
	require.NoError(t, err)
	assert.True(t, isShared)
	assert.Equal(t, "release/v1", bShared)

	badBranch := "HEAD"
	_, _, err = merge.EffectiveTargetBranch("run-1", "merge.integrate", &badBranch)
	require.Error(t, err)

	// MergeActorID is deterministic 64-char lowercase hex and normalizes URLs & branches.
	id1, err := merge.MergeActorID("https://github.com/Org/Repo.git/", "refs/heads/release/v1")
	require.NoError(t, err)
	id2, err := merge.MergeActorID("HTTPS://GITHUB.COM/Org/Repo", "release/v1")
	require.NoError(t, err)
	assert.Equal(t, id1, id2)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), id1)

	idOther, err := merge.MergeActorID("https://github.com/Org/Repo", "main")
	require.NoError(t, err)
	assert.NotEqual(t, id1, idOther)
}

func TestMergeActor_ConcurrentEnqueueSerialDispatchAndConflictProgression(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorID, err := merge.MergeActorID("https://example.com/org/repo.git", git.WorkBranchName("run-1", "merge.int"))
	require.NoError(t, err)

	slots := newFakeSlotManager(4)
	disp := &fakeMergeWorkerDispatcher{}
	notifier := &fakeRunMergeNotifier{}
	rem := &fakeReminderScheduler{}
	a, _ := newTestMergeActor(t, actorID, slots, disp, &fakeConfigResolver{}, notifier, rem)

	// Concurrently enqueue W1, W2, W3.
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, item := range []struct {
		src    string
		branch string
	}{
		{"worker.w1", "sofa/work/run-1/w1"},
		{"worker.w2", "sofa/work/run-1/w2"},
		{"worker.w3", "sofa/work/run-1/w3"},
	} {
		wg.Add(1)
		go func(src, branch string) {
			defer wg.Done()
			resp, eErr := a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
				RunID:           "run-1",
				MergeNodeAddr:   "merge.int",
				MergeEpoch:      1,
				SourceNodeAddr:  src,
				SourceEpoch:     1,
				RepoURL:         "https://example.com/org/repo.git",
				BasedSource:     "main",
				BaseSHA:         sha1,
				CandidateBranch: branch,
				Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
			})
			if eErr != nil {
				errs <- eErr
				return
			}
			if !resp.Accepted {
				errs <- errors.New("expected Accepted=true")
			}
		}(item.src, item.branch)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}

	// Strictly 1 IN_FLIGHT at a time; remaining 2 are PENDING in Queue.
	started := disp.Started()
	require.Len(t, started, 1)
	first := started[0].Req

	st1, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	require.NotNil(t, st1.InFlight)
	assert.Equal(t, first.RequestID, st1.InFlight.RequestID)
	assert.Equal(t, merge.TicketStatusInFlight, st1.InFlight.Status)
	assert.Len(t, st1.Queue, 2)
	assert.Equal(t, 1, slots.ActiveCount())

	secondQueued := st1.Queue[0]
	thirdQueued := st1.Queue[1]

	// First candidate fails rebase due to conflict -> MergeStatusReworkSource.
	// Target branch must remain unchanged and second candidate MUST immediately dispatch!
	rep1, err := a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       first.RequestID,
		RunID:           "run-1",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: first.CandidateBranch,
		LeaseID:         first.LeaseID,
		LeaseKey:        first.LeaseKey,
		ProcessID:       first.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusReworkSource,
			TargetBranch:    git.WorkBranchName("run-1", "merge.int"),
			LatestTargetSHA: sha1,
			ConflictFiles:   []string{"shared.txt"},
			FailureReport:   "rebase conflict on shared.txt",
		},
	})
	require.NoError(t, err)
	assert.True(t, rep1.Accepted)
	assert.False(t, rep1.IgnoredStale)

	notifs := notifier.Notifications()
	require.Len(t, notifs, 1)
	assert.Equal(t, first.RequestID, notifs[0].RequestID)
	assert.Equal(t, runner.MergeStatusReworkSource, notifs[0].Result.Status)
	assert.Equal(t, sha1, notifs[0].Result.LatestTargetSHA)
	assert.Equal(t, []string{"shared.txt"}, notifs[0].Result.ConflictFiles)
	assert.False(t, notifs[0].MergedIntoSharedBranch)

	// Second candidate must already be IN_FLIGHT!
	started = disp.Started()
	require.Len(t, started, 2)
	assert.Equal(t, secondQueued.RequestID, started[1].Req.RequestID)

	st2, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	require.NotNil(t, st2.InFlight)
	assert.Equal(t, secondQueued.RequestID, st2.InFlight.RequestID)
	require.Len(t, st2.Queue, 1)
	assert.Equal(t, thirdQueued.RequestID, st2.Queue[0].RequestID)

	// Second candidate succeeds -> LatestTargetSHA advances to sha2 -> third candidate dispatches!
	_, err = a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       started[1].Req.RequestID,
		RunID:           "run-1",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: started[1].Req.CandidateBranch,
		LeaseID:         started[1].Req.LeaseID,
		LeaseKey:        started[1].Req.LeaseKey,
		ProcessID:       started[1].Req.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    git.WorkBranchName("run-1", "merge.int"),
			LatestTargetSHA: sha2,
			MergedCommitSHA: sha2,
		},
	})
	require.NoError(t, err)

	started = disp.Started()
	require.Len(t, started, 3)
	assert.Equal(t, thirdQueued.RequestID, started[2].Req.RequestID)

	// Third candidate succeeds -> LatestTargetSHA advances to sha3 -> queue empty.
	_, err = a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       started[2].Req.RequestID,
		RunID:           "run-1",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: started[2].Req.CandidateBranch,
		LeaseID:         started[2].Req.LeaseID,
		LeaseKey:        started[2].Req.LeaseKey,
		ProcessID:       started[2].Req.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    git.WorkBranchName("run-1", "merge.int"),
			LatestTargetSHA: sha3,
			MergedCommitSHA: sha3,
		},
	})
	require.NoError(t, err)

	stFinal, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	assert.Nil(t, stFinal.InFlight)
	assert.Empty(t, stFinal.Queue)
	assert.Equal(t, sha3, stFinal.LatestTargetSHA)
	assert.Equal(t, 0, slots.ActiveCount())
}

func TestMergeActor_IdempotentEnqueueAndSourceEpochIncrement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorID, err := merge.MergeActorID("https://example.com/org/repo.git", git.WorkBranchName("run-idem", "merge.int"))
	require.NoError(t, err)

	slots := newFakeSlotManager(2)
	disp := &fakeMergeWorkerDispatcher{}
	notifier := &fakeRunMergeNotifier{}
	a, _ := newTestMergeActor(t, actorID, slots, disp, &fakeConfigResolver{}, notifier, &fakeReminderScheduler{})

	req1 := &merge.EnqueueCandidateReq{
		RunID:           "run-idem",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.a",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/org/repo.git",
		BasedSource:     "main",
		BaseSHA:         sha1,
		CandidateBranch: "sofa/work/run-idem/a",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	}

	r1, err := a.EnqueueCandidate(ctx, req1)
	require.NoError(t, err)
	assert.True(t, r1.Accepted)
	assert.False(t, r1.Deduplicated)
	require.Len(t, disp.Started(), 1)

	// Duplicate while IN_FLIGHT is deduplicated.
	r1Dup, err := a.EnqueueCandidate(ctx, req1)
	require.NoError(t, err)
	assert.True(t, r1Dup.Accepted)
	assert.True(t, r1Dup.Deduplicated)
	assert.Equal(t, r1.Ticket.RequestID, r1Dup.Ticket.RequestID)
	assert.Len(t, disp.Started(), 1)

	// Report REWORK_REQUIRED with LatestTargetSHA = sha2.
	started := disp.Started()[0].Req
	_, err = a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       started.RequestID,
		RunID:           "run-idem",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: started.CandidateBranch,
		LeaseID:         started.LeaseID,
		ProcessID:       started.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusReworkSource,
			TargetBranch:    git.WorkBranchName("run-idem", "merge.int"),
			LatestTargetSHA: sha2,
			GatekeeperFailure: &sandboxruntime.GatekeeperFailure{
				Name:     "unit_test",
				ExitCode: 1,
			},
			FailureReport: "gatekeeper unit_test exited 1",
		},
	})
	require.NoError(t, err)

	// Duplicate after REWORK_REQUIRED at same SourceEpoch=1 is still deduplicated.
	r1DupAfter, err := a.EnqueueCandidate(ctx, req1)
	require.NoError(t, err)
	assert.True(t, r1DupAfter.Accepted)
	assert.True(t, r1DupAfter.Deduplicated)
	assert.Equal(t, merge.TicketStatusReworkRequired, r1DupAfter.Ticket.Status)
	assert.Len(t, disp.Started(), 1)

	// After source repairs and increments SourceEpoch -> 2, enqueue creates a new ticket & dispatches!
	req2 := *req1
	req2.SourceEpoch = 2
	req2.BaseSHA = sha2
	r2, err := a.EnqueueCandidate(ctx, &req2)
	require.NoError(t, err)
	assert.True(t, r2.Accepted)
	assert.False(t, r2.Deduplicated)
	assert.NotEqual(t, r1.Ticket.RequestID, r2.Ticket.RequestID)
	require.Len(t, disp.Started(), 2)
}

func TestMergeActor_SharedTargetBranchVsDefaultLedgerRegistration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// 1. Default Run-exclusive branch (!SharedTargetBranch): MUST register KindMergeBranch before dispatch and report MergedIntoSharedBranch=false.
	defBranch := git.WorkBranchName("run-def", "merge.int")
	defID, err := merge.MergeActorID("https://example.com/repo.git", defBranch)
	require.NoError(t, err)

	defSlots := newFakeSlotManager(2)
	defDisp := &fakeMergeWorkerDispatcher{}
	defNotif := &fakeRunMergeNotifier{}
	defActor, _ := newTestMergeActor(t, defID, defSlots, defDisp, &fakeConfigResolver{}, defNotif, &fakeReminderScheduler{})

	_, err = defActor.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-def",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.w1",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		BasedSource:     "main",
		BaseSHA:         sha1,
		CandidateBranch: "sofa/work/run-def/w1",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)

	branches := defNotif.Branches()
	require.Len(t, branches, 1)
	assert.Equal(t, git.KindMergeBranch, branches[0].Kind)
	assert.Equal(t, defBranch, branches[0].Branch)
	assert.Equal(t, sha1, branches[0].BaseSHA)

	defStarted := defDisp.Started()
	require.Len(t, defStarted, 1)
	assert.Nil(t, defStarted[0].Req.TargetBranch)

	_, err = defActor.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       defStarted[0].Req.RequestID,
		RunID:           "run-def",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: defStarted[0].Req.CandidateBranch,
		LeaseID:         defStarted[0].Req.LeaseID,
		ProcessID:       defStarted[0].Req.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    defBranch,
			LatestTargetSHA: sha2,
			MergedCommitSHA: sha2,
		},
	})
	require.NoError(t, err)

	defNotifs := defNotif.Notifications()
	require.Len(t, defNotifs, 1)
	assert.False(t, defNotifs[0].SharedTargetBranch)
	assert.False(t, defNotifs[0].MergedIntoSharedBranch)

	// Rerun of Run-exclusive merge node at MergeEpoch: 2 with same upstream SourceEpoch: 1 MUST NOT deduplicate,
	// MUST re-register KindMergeBranch with Epoch: 2, and MUST execute cleanly.
	rerunResp, err := defActor.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-def",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      2,
		SourceNodeAddr:  "worker.w1",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		BasedSource:     "main",
		BaseSHA:         sha1,
		CandidateBranch: "sofa/work/run-def/w1",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)
	assert.True(t, rerunResp.Accepted)
	assert.False(t, rerunResp.Deduplicated)
	assert.NotEqual(t, defStarted[0].Req.RequestID, rerunResp.Ticket.RequestID)

	branchesAfterRerun := defNotif.Branches()
	require.Len(t, branchesAfterRerun, 2)
	assert.Equal(t, git.KindMergeBranch, branchesAfterRerun[1].Kind)
	assert.Equal(t, uint64(2), branchesAfterRerun[1].Epoch)
	assert.Equal(t, defBranch, branchesAfterRerun[1].Branch)
	assert.Equal(t, sha1, branchesAfterRerun[1].BaseSHA)

	defStartedAfter := defDisp.Started()
	require.Len(t, defStartedAfter, 2)
	assert.Equal(t, uint64(2), defStartedAfter[1].Req.Epoch)
	assert.Equal(t, rerunResp.Ticket.RequestID, defStartedAfter[1].Req.RequestID)

	_, err = defActor.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       defStartedAfter[1].Req.RequestID,
		RunID:           "run-def",
		MergeNodeAddr:   "merge.int",
		Epoch:           2,
		CandidateBranch: defStartedAfter[1].Req.CandidateBranch,
		LeaseID:         defStartedAfter[1].Req.LeaseID,
		ProcessID:       defStartedAfter[1].Req.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    defBranch,
			LatestTargetSHA: sha3,
			MergedCommitSHA: sha3,
		},
	})
	require.NoError(t, err)
	require.Len(t, defNotif.Notifications(), 2)
	assert.Equal(t, uint64(2), defNotif.Notifications()[1].MergeEpoch)
	assert.Equal(t, sha3, defNotif.Notifications()[1].Result.MergedCommitSHA)

	// 2. Explicit shared target_branch != null (SharedTargetBranch=true): MUST NOT register KindMergeBranch, and sets MergedIntoSharedBranch=true on success!
	sharedTarget := "integration"
	sharedID, err := merge.MergeActorID("https://example.com/repo.git", sharedTarget)
	require.NoError(t, err)

	sharedSlots := newFakeSlotManager(2)
	sharedDisp := &fakeMergeWorkerDispatcher{}
	sharedNotif := &fakeRunMergeNotifier{}
	sharedActor, _ := newTestMergeActor(t, sharedID, sharedSlots, sharedDisp, &fakeConfigResolver{}, sharedNotif, &fakeReminderScheduler{})

	_, err = sharedActor.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-shared",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.w1",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		BasedSource:     "main",
		BaseSHA:         sha1,
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-shared/w1",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)
	assert.Empty(t, sharedNotif.Branches(), "shared target_branch must never be registered in Run branch ledger for deletion")

	sharedStarted := sharedDisp.Started()
	require.Len(t, sharedStarted, 1)
	require.NotNil(t, sharedStarted[0].Req.TargetBranch)
	assert.Equal(t, "integration", *sharedStarted[0].Req.TargetBranch)

	_, err = sharedActor.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       sharedStarted[0].Req.RequestID,
		RunID:           "run-shared",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: sharedStarted[0].Req.CandidateBranch,
		LeaseID:         sharedStarted[0].Req.LeaseID,
		ProcessID:       sharedStarted[0].Req.ProcessID,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    "integration",
			LatestTargetSHA: sha2,
			MergedCommitSHA: sha2,
		},
	})
	require.NoError(t, err)

	sharedNotifs := sharedNotif.Notifications()
	require.Len(t, sharedNotifs, 1)
	assert.True(t, sharedNotifs[0].SharedTargetBranch)
	assert.True(t, sharedNotifs[0].MergedIntoSharedBranch)
}

func TestMergeActor_SlotSaturationReminderAndDispatchFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorID, err := merge.MergeActorID("https://example.com/repo.git", "integration")
	require.NoError(t, err)

	// Capacity = 0 initially (saturated).
	slots := newFakeSlotManager(0)
	disp := &fakeMergeWorkerDispatcher{}
	rem := &fakeReminderScheduler{}
	a, sm := newTestMergeActor(t, actorID, slots, disp, &fakeConfigResolver{}, &fakeRunMergeNotifier{}, rem)

	sharedTarget := "integration"
	resp, err := a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-sat",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.w1",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-sat/w1",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)
	assert.True(t, resp.Accepted)
	assert.Empty(t, disp.Started())
	assert.NotEmpty(t, rem.registered)

	// Open capacity and simulate RunMergeCandidate failing first, then succeeding on next reminder.
	slots.mu.Lock()
	slots.capacity = 1
	slots.mu.Unlock()
	disp.mu.Lock()
	disp.startErr = errors.New("worker rpc timeout")
	disp.mu.Unlock()

	savesBefore := sm.SaveCount()
	a.ReminderCall(merge.DefaultSlotRetryReminderName, nil, "50ms", "50ms")
	assert.Greater(t, sm.SaveCount(), savesBefore, "ReminderCall must explicitly Set+Save state")
	assert.Equal(t, 0, slots.ActiveCount(), "failed RunMergeCandidate must release slot immediately")
	require.Len(t, slots.suspects, 1)

	stAfterFail, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	assert.Nil(t, stAfterFail.InFlight)
	require.Len(t, stAfterFail.Queue, 1)
	assert.Equal(t, merge.TicketStatusPending, stAfterFail.Queue[0].Status)

	// Clear dispatch error and trigger ReminderCall again -> succeeds!
	disp.mu.Lock()
	disp.startErr = nil
	disp.mu.Unlock()

	a.ReminderCall(merge.DefaultSlotRetryReminderName, nil, "50ms", "50ms")
	stAfterOK, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	require.NotNil(t, stAfterOK.InFlight)
	assert.Empty(t, stAfterOK.Queue)
	assert.NotEmpty(t, rem.unregistered)
}

func TestMergeActor_OnWorkerDeadRequeuesHeadAndRejectsZombieReport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorID, err := merge.MergeActorID("https://example.com/repo.git", "integration")
	require.NoError(t, err)

	slots := newFakeSlotManager(2)
	slots.nextPID = "proc-dead"
	slots.nextApp = "sofa-worker-proc-dead"
	disp := &fakeMergeWorkerDispatcher{}
	notifier := &fakeRunMergeNotifier{}
	a, _ := newTestMergeActor(t, actorID, slots, disp, &fakeConfigResolver{}, notifier, &fakeReminderScheduler{})

	sharedTarget := "integration"
	for _, w := range []string{"worker.w1", "worker.w2"} {
		_, eErr := a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
			RunID:           "run-dead",
			MergeNodeAddr:   "merge.int",
			MergeEpoch:      1,
			SourceNodeAddr:  w,
			SourceEpoch:     1,
			RepoURL:         "https://example.com/repo.git",
			TargetBranch:    &sharedTarget,
			CandidateBranch: "sofa/work/run-dead/" + w,
			Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
		})
		require.NoError(t, eErr)
	}

	require.Len(t, disp.Started(), 1)
	deadCall := disp.Started()[0].Req
	assert.Equal(t, "proc-dead", deadCall.ProcessID)

	// Simulate ProcessRegistryActor purging dead worker's slot and switching healthy target worker.
	slots.mu.Lock()
	delete(slots.active, deadCall.LeaseID)
	slots.nextPID = "proc-healthy"
	slots.nextApp = "sofa-worker-proc-healthy"
	slots.mu.Unlock()

	deadResp, err := a.OnWorkerDead(ctx, &registry.WorkerDeadNotification{
		LeaseID:   deadCall.LeaseID,
		LeaseKey:  deadCall.LeaseKey,
		ProcessID: "proc-dead",
		AppID:     "sofa-worker-proc-dead",
		Reason:    "missed 3 probes",
	})
	require.NoError(t, err)
	assert.True(t, deadResp.Handled)
	assert.Equal(t, deadCall.RequestID, deadResp.RequestID)

	// W1 (NOT W2!) must be re-dispatched onto proc-healthy with a NEW LeaseID!
	require.Len(t, disp.Started(), 2)
	retryCall := disp.Started()[1].Req
	assert.Equal(t, deadCall.RequestID, retryCall.RequestID)
	assert.Equal(t, "proc-healthy", retryCall.ProcessID)
	assert.NotEqual(t, deadCall.LeaseID, retryCall.LeaseID)

	// Late zombie ReportCandidateResult with old LeaseID MUST be ignored!
	zombieResp, err := a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       deadCall.RequestID,
		RunID:           "run-dead",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: deadCall.CandidateBranch,
		LeaseID:         deadCall.LeaseID,
		ProcessID:       "proc-dead",
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    "integration",
			LatestTargetSHA: sha4,
			MergedCommitSHA: sha4,
		},
	})
	require.NoError(t, err)
	assert.False(t, zombieResp.Accepted)
	assert.True(t, zombieResp.IgnoredStale)

	// W1 is still IN_FLIGHT on proc-healthy, W2 still waiting in Queue.
	stMid, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	require.NotNil(t, stMid.InFlight)
	assert.Equal(t, retryCall.LeaseID, stMid.InFlight.LeaseID)
	require.Len(t, stMid.Queue, 1)

	// Healthy worker finishes W1 -> W2 immediately dispatches!
	okResp, err := a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       retryCall.RequestID,
		RunID:           "run-dead",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: retryCall.CandidateBranch,
		LeaseID:         retryCall.LeaseID,
		ProcessID:       "proc-healthy",
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    "integration",
			LatestTargetSHA: sha2,
			MergedCommitSHA: sha2,
		},
	})
	require.NoError(t, err)
	assert.True(t, okResp.Accepted)
	require.Len(t, disp.Started(), 3)
	assert.Equal(t, "sofa/work/run-dead/worker.w2", disp.Started()[2].Req.CandidateBranch)
}

func TestMergeActor_CancelByRunAbortsInFlightAndAdvancesOtherRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sharedTarget := "integration"
	actorID, err := merge.MergeActorID("https://example.com/repo.git", sharedTarget)
	require.NoError(t, err)

	slots := newFakeSlotManager(2)
	disp := &fakeMergeWorkerDispatcher{}
	notifier := &fakeRunMergeNotifier{}
	a, _ := newTestMergeActor(t, actorID, slots, disp, &fakeConfigResolver{}, notifier, &fakeReminderScheduler{})

	// Enqueue run-A (W1, W2) and run-B (W3) onto the shared integration target.
	_, err = a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-A",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.a1",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-A/a1",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)

	_, err = a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-A",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.a2",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-A/a2",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)

	rB, err := a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-B",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.b1",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-B/b1",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)

	require.Len(t, disp.Started(), 1)
	inFlightA1 := disp.Started()[0].Req

	// CancelByRun("run-A"): must cancel W2 (PENDING), abort W1 (IN_FLIGHT), release W1's slot, and immediately dispatch run-B (W3)!
	cancelResp, err := a.CancelByRun(ctx, &merge.CancelByRunReq{
		RunID:  "run-A",
		Reason: "immediate pause",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, cancelResp.CancelledPending)
	assert.True(t, cancelResp.CancelledInFlight)
	assert.True(t, cancelResp.AbortedWorker)
	assert.Equal(t, inFlightA1.RequestID, cancelResp.InFlightRequestID)

	aborted := disp.Aborted()
	require.Len(t, aborted, 1)
	assert.Equal(t, inFlightA1.RequestID, aborted[0].Req.RequestID)

	// run-B MUST now be IN_FLIGHT!
	require.Len(t, disp.Started(), 2)
	assert.Equal(t, rB.Ticket.RequestID, disp.Started()[1].Req.RequestID)

	// Late aborted report from run-A W1 is ignored without disrupting run-B.
	lateResp, err := a.ReportCandidateResult(ctx, &daemon.MergeOutcomeReport{
		RequestID:       inFlightA1.RequestID,
		RunID:           "run-A",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		CandidateBranch: inFlightA1.CandidateBranch,
		LeaseID:         inFlightA1.LeaseID,
		Aborted:         true,
	})
	require.NoError(t, err)
	assert.False(t, lateResp.Accepted)
	assert.True(t, lateResp.IgnoredStale)

	stAfter, err := a.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	require.NotNil(t, stAfter.InFlight)
	assert.Equal(t, rB.Ticket.RequestID, stAfter.InFlight.RequestID)
	assert.Empty(t, stAfter.Queue)

	// Resuming run-A after immediate pause can re-enqueue (run-A, merge.int, worker.a2, 1) without being blocked by CANCELLED history.
	rA2Resume, err := a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-A",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      2,
		SourceNodeAddr:  "worker.a2",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-A/a2",
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)
	assert.True(t, rA2Resume.Accepted)
	assert.False(t, rA2Resume.Deduplicated)
}

func TestMergeActor_GitCredentialMaterializationAndMissingKeyError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sharedTarget := "main"
	actorID, err := merge.MergeActorID("https://example.com/repo.git", sharedTarget)
	require.NoError(t, err)

	tokRef := "GIT_PUSH_TOKEN"
	cfg := &fakeConfigResolver{
		values: map[string]string{
			"GIT_PUSH_TOKEN": "resolved-secret-token",
		},
	}
	slots := newFakeSlotManager(2)
	disp := &fakeMergeWorkerDispatcher{}
	notifier := &fakeRunMergeNotifier{}
	a, _ := newTestMergeActor(t, actorID, slots, disp, cfg, notifier, &fakeReminderScheduler{})

	// First ticket asks for MISSING_KEY -> must notify error, dequeue, and proceed to second ticket!
	missingRef := "MISSING_KEY"
	_, err = a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-cred",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.bad",
		SourceEpoch:     1,
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-cred/bad",
		TokenRef:        &missingRef,
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)

	// Bad ticket must have been dequeued and reported to RunActor, leaving queue ready for valid ticket.
	notifs := notifier.Notifications()
	require.Len(t, notifs, 1)
	assert.Equal(t, "worker.bad", notifs[0].SourceNodeAddr)
	assert.Contains(t, strings.ToLower(notifs[0].ErrorMessage), "missing")
	assert.Empty(t, disp.Started())

	// Second ticket has valid TokenRef -> resolved into ResolvedEnv and Auth!
	_, err = a.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
		RunID:           "run-cred",
		MergeNodeAddr:   "merge.int",
		MergeEpoch:      1,
		SourceNodeAddr:  "worker.good",
		SourceEpoch:     1,
		Namespace:       "prod",
		RepoURL:         "https://example.com/repo.git",
		TargetBranch:    &sharedTarget,
		CandidateBranch: "sofa/work/run-cred/good",
		TokenRef:        &tokRef,
		Sandbox:         sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
	})
	require.NoError(t, err)

	started := disp.Started()
	require.Len(t, started, 1)
	require.NotNil(t, started[0].Req.Auth.TokenRef)
	assert.Equal(t, "GIT_PUSH_TOKEN", *started[0].Req.Auth.TokenRef)
	assert.Equal(t, "resolved-secret-token", started[0].Req.ResolvedEnv["GIT_PUSH_TOKEN"])
}
