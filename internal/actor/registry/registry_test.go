package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

var _ actor.StateManagerContext = (*memoryStateManager)(nil)

type fakeProber struct {
	mu      sync.Mutex
	errByID map[string]error
	calls   []string
}

func (p *fakeProber) Ping(_ context.Context, appID, processID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, processID+"@"+appID)
	if err, ok := p.errByID[processID]; ok {
		return err
	}
	return nil
}

func (p *fakeProber) setErr(processID string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.errByID == nil {
		p.errByID = make(map[string]error)
	}
	if err == nil {
		delete(p.errByID, processID)
	} else {
		p.errByID[processID] = err
	}
}

type deadNoticeCall struct {
	OwnerActorType string
	OwnerActorID   string
	Notice         registry.WorkerDeadNotification
}

type fakeDeadNotifier struct {
	mu    sync.Mutex
	calls []deadNoticeCall
}

func (n *fakeDeadNotifier) NotifyWorkerDead(_ context.Context, ownerActorType, ownerActorID string, notice registry.WorkerDeadNotification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, deadNoticeCall{
		OwnerActorType: ownerActorType,
		OwnerActorID:   ownerActorID,
		Notice:         notice,
	})
	return nil
}

func (n *fakeDeadNotifier) Calls() []deadNoticeCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]deadNoticeCall, len(n.calls))
	copy(out, n.calls)
	return out
}

type fakeReminderScheduler struct {
	mu    sync.Mutex
	calls int
}

func (r *fakeReminderScheduler) EnsureProbeReminder(_ context.Context, _, _, _ string, _, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func newTestActor(
	t *testing.T,
	clock func() time.Time,
	prober registry.Prober,
	notifier registry.DeadNotifier,
	sched registry.ReminderScheduler,
) (*registry.ProcessRegistryActor, *memoryStateManager) {
	t.Helper()
	sm := newMemoryStateManager()
	a := registry.NewProcessRegistryActor(registry.Options{
		Prober:            prober,
		DeadNotifier:      notifier,
		ReminderScheduler: sched,
		Clock:             clock,
		SuspectTimeout:    15 * time.Second,
		ProbeInterval:     3 * time.Second,
		DeadThreshold:     3,
	})
	a.SetID(registry.GlobalActorID)
	a.SetStateManager(sm)
	return a, sm
}

func TestProcessRegistryActor_LeastLoadArchAndSaturation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	assert.Equal(t, "ProcessRegistryActor", registry.ActorType)
	assert.Equal(t, "global", registry.GlobalActorID)
	assert.Equal(t, 10, registry.DefaultWorkerMaxConcurrency)
	assert.Equal(t, 2, registry.DefaultDevMaxConcurrency)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	sched := &fakeReminderScheduler{}
	a, _ := newTestActor(t, func() time.Time { return now }, &fakeProber{}, &fakeDeadNotifier{}, sched)

	// Register w1 (max=10) first and load it with 5 active slots.
	reg1, err := a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p1",
		AppID:          "sofa-worker-p1",
		MaxConcurrency: registry.DefaultWorkerMaxConcurrency,
		Arch:           "linux/amd64",
	})
	require.NoError(t, err)
	assert.True(t, reg1.Registered)
	assert.Equal(t, registry.StatusHealthy, reg1.Status)
	assert.GreaterOrEqual(t, sched.calls, 1)

	for i := range 5 {
		cl, cerr := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
			LeaseKey:       fmt.Sprintf("pre-p1-%d", i),
			OwnerActorType: "NodeActor",
			OwnerActorID:   fmt.Sprintf("n%d", i),
			RequiredArch:   "linux/amd64",
		})
		require.NoError(t, cerr)
		require.True(t, cl.Granted)
		assert.Equal(t, "p1", cl.ProcessID)
	}

	// Register w2 (max=10) and allocate 1 slot to it -> 1/10 vs 5/10.
	_, err = a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p2",
		AppID:          "sofa-worker-p2",
		MaxConcurrency: 10,
		Arch:           "linux/amd64",
	})
	require.NoError(t, err)

	cl2, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "first-p2",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-p2-1",
		RequiredArch:   "linux/amd64",
	})
	require.NoError(t, err)
	require.True(t, cl2.Granted)
	assert.Equal(t, "p2", cl2.ProcessID)

	// Next ClaimSlot must still choose p2 (active=1/10) over p1 (active=5/10).
	cl3, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "second-p2",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-p2-2",
		RequiredArch:   "linux/amd64",
	})
	require.NoError(t, err)
	require.True(t, cl3.Granted)
	assert.Equal(t, "p2", cl3.ProcessID)

	// Register arm64 worker p3 (max=2) -> RequiredArch "linux/arm64" selects p3 exclusively.
	_, err = a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p3-arm",
		AppID:          "sofa-worker-p3",
		MaxConcurrency: registry.DefaultDevMaxConcurrency,
		Arch:           "linux/arm64",
	})
	require.NoError(t, err)

	arm1, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "arm-1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-arm-1",
		RequiredArch:   "linux/arm64",
	})
	require.NoError(t, err)
	require.True(t, arm1.Granted)
	assert.Equal(t, "p3-arm", arm1.ProcessID)

	// Unsupported arch returns Granted: false without error.
	winClaim, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "win-1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-win",
		RequiredArch:   "windows/amd64",
	})
	require.NoError(t, err)
	assert.False(t, winClaim.Granted)

	// Fill remaining arm64 slot, then verify saturation returns Granted: false.
	arm2, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "arm-2",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-arm-2",
		RequiredArch:   "linux/arm64",
	})
	require.NoError(t, err)
	require.True(t, arm2.Granted)

	armFull, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "arm-3",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-arm-3",
		RequiredArch:   "linux/arm64",
	})
	require.NoError(t, err)
	assert.False(t, armFull.Granted)

	// Heterogeneous capacity ratio check: p-big (max=10, active=4 -> 40%) vs p-small (max=2, active=1 -> 50%).
	// Release one slot from p3-arm so it is 1/2 (50%), and register p4-arm (max=10) with 4/10 (40%).
	rel, err := a.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: arm1.LeaseID})
	require.NoError(t, err)
	assert.True(t, rel.Released)

	_, err = a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p4-arm",
		AppID:          "sofa-worker-p4",
		MaxConcurrency: 10,
		Arch:           "linux/arm64",
	})
	require.NoError(t, err)
	for i := range 4 {
		c, cerr := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
			LeaseKey:       fmt.Sprintf("pre-p4-%d", i),
			OwnerActorType: "NodeActor",
			OwnerActorID:   fmt.Sprintf("n-p4-%d", i),
			RequiredArch:   "linux/arm64",
		})
		require.NoError(t, cerr)
		require.True(t, c.Granted)
		assert.Equal(t, "p4-arm", c.ProcessID)
	}
	// Now p4-arm is 4/10 (40%) and p3-arm is 1/2 (50%). Next claim must select p4-arm!
	ratioClaim, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "arm-ratio-check",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "n-ratio",
		RequiredArch:   "linux/arm64",
	})
	require.NoError(t, err)
	require.True(t, ratioClaim.Granted)
	assert.Equal(t, "p4-arm", ratioClaim.ProcessID)
}

func TestProcessRegistryActor_IdempotentClaimAndRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	a, _ := newTestActor(t, func() time.Time { return now }, &fakeProber{}, &fakeDeadNotifier{}, &fakeReminderScheduler{})

	_, err := a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p1",
		AppID:          "sofa-worker-p1",
		MaxConcurrency: 2,
		Arch:           "linux/amd64",
	})
	require.NoError(t, err)

	c1, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/worker.a/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-1__worker-a",
	})
	require.NoError(t, err)
	require.True(t, c1.Granted)
	require.NotEmpty(t, c1.LeaseID)

	// Duplicate ClaimSlot with same LeaseKey must return the exact same LeaseID without consuming a 2nd slot.
	c1Dup, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/worker.a/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-1__worker-a",
	})
	require.NoError(t, err)
	assert.True(t, c1Dup.Granted)
	assert.Equal(t, c1.LeaseID, c1Dup.LeaseID)
	assert.Equal(t, c1.ProcessID, c1Dup.ProcessID)
	assert.Equal(t, c1.AppID, c1Dup.AppID)

	list, err := a.ListProcesses(ctx, &registry.ListProcessesReq{})
	require.NoError(t, err)
	require.Len(t, list.Processes, 1)
	assert.Equal(t, 1, list.Processes[0].ActiveSlots)

	// ReleaseSlot is idempotent.
	r1, err := a.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: c1.LeaseID})
	require.NoError(t, err)
	assert.True(t, r1.Released)

	r2, err := a.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseID: c1.LeaseID})
	require.NoError(t, err)
	assert.False(t, r2.Released)

	r3, err := a.ReleaseSlot(ctx, &registry.ReleaseSlotReq{LeaseKey: "run-1/worker.a/e1"})
	require.NoError(t, err)
	assert.False(t, r3.Released)

	listAfter, err := a.ListProcesses(ctx, &registry.ListProcessesReq{})
	require.NoError(t, err)
	assert.Equal(t, 0, listAfter.Processes[0].ActiveSlots)
}

func TestProcessRegistryActor_ThreeStateLivenessRecoveryAndDeadNotification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	prober := &fakeProber{}
	notifier := &fakeDeadNotifier{}
	a, sm := newTestActor(t, clock, prober, notifier, &fakeReminderScheduler{})

	_, err := a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p1",
		AppID:          "sofa-worker-p1",
		MaxConcurrency: 4,
		Arch:           "linux/amd64",
	})
	require.NoError(t, err)

	_, err = a.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      "p2",
		AppID:          "sofa-worker-p2",
		MaxConcurrency: 4,
		Arch:           "linux/amd64",
	})
	require.NoError(t, err)

	// Give p1 two leases owned by NodeActor and MergeActor.
	l1, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/worker.step1/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-1__step1",
	})
	require.NoError(t, err)
	require.True(t, l1.Granted)
	require.Equal(t, "p1", l1.ProcessID)

	// Claim another on p2 so p1 and p2 are tied (1/4 vs 1/4), then verify ReportSuspect/timeout isolates p1.
	l2, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/worker.step2/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-1__step2",
	})
	require.NoError(t, err)
	require.True(t, l2.Granted)
	require.Equal(t, "p2", l2.ProcessID)

	l3, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/merge.int/e1",
		OwnerActorType: "MergeActor",
		OwnerActorID:   "merge-hash-1",
	})
	require.NoError(t, err)
	require.True(t, l3.Granted)
	require.Equal(t, "p1", l3.ProcessID)

	// Advance 10s, p2 heartbeats, p1 does not. Advance another 6s -> p1 silent for 16s (> 15s SuspectTimeout).
	advance(10 * time.Second)
	hb2, err := a.Heartbeat(ctx, &registry.HeartbeatReq{ProcessID: "p2"})
	require.NoError(t, err)
	assert.Equal(t, registry.StatusHealthy, hb2.Status)
	assert.False(t, hb2.FenceRequired)

	advance(6 * time.Second)

	// ReminderCall moves p1 to SUSPECT and probes it; prober succeeds -> recovers immediately to HEALTHY.
	savesBefore := sm.SaveCount()
	a.ReminderCall(registry.DefaultReminderName, nil, "3s", "3s")
	assert.Greater(t, sm.SaveCount(), savesBefore, "ReminderCall MUST explicitly call StateManager.Save")

	list1, err := a.ListProcesses(ctx, &registry.ListProcessesReq{})
	require.NoError(t, err)
	byID := map[string]registry.ProcessRecord{}
	for _, p := range list1.Processes {
		byID[p.ProcessID] = p
	}
	assert.Equal(t, registry.StatusHealthy, byID["p1"].Status)
	assert.Equal(t, 2, byID["p1"].ActiveSlots)
	assert.Empty(t, notifier.Calls())

	// Now explicitly mark p1 SUSPECT via ReportSuspect and make p1's ping fail.
	rep, err := a.ReportSuspect(ctx, &registry.ReportSuspectReq{ProcessID: "p1", Reason: "rpc timeout"})
	require.NoError(t, err)
	assert.Equal(t, registry.StatusSuspect, rep.Status)

	// While SUSPECT, new ClaimSlot MUST skip p1 and allocate on p2.
	newSlot, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-2/worker.x/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-2__x",
	})
	require.NoError(t, err)
	require.True(t, newSlot.Granted)
	assert.Equal(t, "p2", newSlot.ProcessID)

	// Existing leases on p1 are preserved while SUSPECT (idempotent ClaimSlot returns p1's existing lease).
	idemSuspect, err := a.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/worker.step1/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-1__step1",
	})
	require.NoError(t, err)
	assert.Equal(t, l1.LeaseID, idemSuspect.LeaseID)
	assert.Equal(t, "p1", idemSuspect.ProcessID)

	// Fail ping 2 times -> still SUSPECT, no OnWorkerDead yet.
	prober.setErr("p1", errors.New("connection refused"))
	a.ReminderCall(registry.DefaultReminderName, nil, "3s", "3s")
	a.ReminderCall(registry.DefaultReminderName, nil, "3s", "3s")
	assert.Empty(t, notifier.Calls())

	// Keep p2 alive before 3rd reminder.
	_, err = a.Heartbeat(ctx, &registry.HeartbeatReq{ProcessID: "p2"})
	require.NoError(t, err)

	// 3rd failed probe reaches DeadThreshold (3) -> p1 becomes DEAD, leases purged, OnWorkerDead fired for both owners!
	savesBeforeDead := sm.SaveCount()
	a.ReminderCall(registry.DefaultReminderName, nil, "3s", "3s")
	assert.Greater(t, sm.SaveCount(), savesBeforeDead, "ReminderCall MUST explicitly persist DEAD state")

	// Reload via fresh actor on same StateManagerContext to prove persisted state (not just memory) was saved!
	reloaded, _ := newTestActor(t, clock, prober, notifier, &fakeReminderScheduler{})
	reloaded.SetStateManager(sm)

	listDead, err := reloaded.ListProcesses(ctx, &registry.ListProcessesReq{})
	require.NoError(t, err)
	byID = map[string]registry.ProcessRecord{}
	for _, p := range listDead.Processes {
		byID[p.ProcessID] = p
	}
	assert.Equal(t, registry.StatusDead, byID["p1"].Status)
	assert.Equal(t, 0, byID["p1"].ActiveSlots)

	deadCalls := notifier.Calls()
	require.Len(t, deadCalls, 2)
	assert.ElementsMatch(t, []deadNoticeCall{
		{
			OwnerActorType: "NodeActor",
			OwnerActorID:   "run-1__step1",
			Notice: registry.WorkerDeadNotification{
				LeaseID:   l1.LeaseID,
				LeaseKey:  l1.LeaseKey,
				ProcessID: "p1",
				AppID:     "sofa-worker-p1",
				Reason:    "probe threshold exceeded (3): connection refused",
			},
		},
		{
			OwnerActorType: "MergeActor",
			OwnerActorID:   "merge-hash-1",
			Notice: registry.WorkerDeadNotification{
				LeaseID:   l3.LeaseID,
				LeaseKey:  l3.LeaseKey,
				ProcessID: "p1",
				AppID:     "sofa-worker-p1",
				Reason:    "probe threshold exceeded (3): connection refused",
			},
		},
	}, deadCalls)

	// Heartbeat from DEAD p1 or unknown process MUST return StatusDead + FenceRequired: true and NEVER resurrect.
	hbDead, err := reloaded.Heartbeat(ctx, &registry.HeartbeatReq{ProcessID: "p1"})
	require.NoError(t, err)
	assert.Equal(t, registry.StatusDead, hbDead.Status)
	assert.True(t, hbDead.FenceRequired)

	hbUnknown, err := reloaded.Heartbeat(ctx, &registry.HeartbeatReq{ProcessID: "never-registered"})
	require.NoError(t, err)
	assert.Equal(t, registry.StatusDead, hbUnknown.Status)
	assert.True(t, hbUnknown.FenceRequired)

	// Re-registering the DEAD ProcessID must fail (must rotate ProcessID).
	_, err = reloaded.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID: "p1",
		AppID:     "sofa-worker-p1",
	})
	require.Error(t, err)

	// Re-claiming previously dead LeaseKey now allocates a fresh slot on healthy p2!
	reclaim, err := reloaded.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-1/worker.step1/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-1__step1",
	})
	require.NoError(t, err)
	require.True(t, reclaim.Granted)
	assert.Equal(t, "p2", reclaim.ProcessID)
	assert.NotEqual(t, l1.LeaseID, reclaim.LeaseID)
}
