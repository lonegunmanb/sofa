package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/provider"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
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
	out := make(map[string]string)
	var missing []string
	for _, k := range req.Keys {
		if v, ok := f.values[k]; ok {
			out[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	return &config.ResolveForNodeResp{
		Values:      out,
		MissingKeys: missing,
	}, nil
}

type fakeSlotManager struct {
	mu             sync.Mutex
	claimResponses []registry.ClaimSlotResp
	claimCalls     []registry.ClaimSlotReq
	releaseCalls   []registry.ReleaseSlotReq
	suspectCalls   []registry.ReportSuspectReq
	nextLeaseSeq   int
}

func (f *fakeSlotManager) ClaimSlot(_ context.Context, req *registry.ClaimSlotReq) (*registry.ClaimSlotResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls = append(f.claimCalls, *req)
	if len(f.claimResponses) > 0 {
		resp := f.claimResponses[0]
		f.claimResponses = f.claimResponses[1:]
		return &resp, nil
	}
	f.nextLeaseSeq++
	return &registry.ClaimSlotResp{
		Granted:   true,
		LeaseID:   fmt.Sprintf("lease-%d", f.nextLeaseSeq),
		LeaseKey:  req.LeaseKey,
		ProcessID: fmt.Sprintf("proc-%d", f.nextLeaseSeq),
		AppID:     fmt.Sprintf("sofa-worker-proc-%d", f.nextLeaseSeq),
	}, nil
}

func (f *fakeSlotManager) ReleaseSlot(_ context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, *req)
	return &registry.ReleaseSlotResp{Released: true}, nil
}

func (f *fakeSlotManager) ReportSuspect(_ context.Context, req *registry.ReportSuspectReq) (*registry.ReportSuspectResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspectCalls = append(f.suspectCalls, *req)
	return &registry.ReportSuspectResp{Status: registry.StatusSuspect}, nil
}

type startNodeCall struct {
	AppID string
	Req   daemon.StartNodeRequest
}

type abortNodeCall struct {
	AppID string
	Req   daemon.AbortNodeRequest
}

type fakeWorkerDispatcher struct {
	mu          sync.Mutex
	startCalls  []startNodeCall
	abortCalls  []abortNodeCall
	startErrs   []error
	startResult *daemon.StartNodeResponse
}

func (f *fakeWorkerDispatcher) StartNode(_ context.Context, appID string, req daemon.StartNodeRequest) (daemon.StartNodeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, startNodeCall{AppID: appID, Req: req})
	if len(f.startErrs) > 0 {
		err := f.startErrs[0]
		f.startErrs = f.startErrs[1:]
		if err != nil {
			return daemon.StartNodeResponse{}, err
		}
	}
	if f.startResult != nil {
		return *f.startResult, nil
	}
	return daemon.StartNodeResponse{Accepted: true, ProcessID: req.ProcessID}, nil
}

func (f *fakeWorkerDispatcher) AbortNode(_ context.Context, appID string, req daemon.AbortNodeRequest) (daemon.AbortNodeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortCalls = append(f.abortCalls, abortNodeCall{AppID: appID, Req: req})
	return daemon.AbortNodeResponse{Aborted: true}, nil
}

type fakeRunNotifier struct {
	mu           sync.Mutex
	branches     []node.RegisterManagedBranchReq
	events       []node.NodeEventNotification
	branchErr    error
	branchErrMap map[string]error
}

func (f *fakeRunNotifier) RegisterManagedBranch(_ context.Context, req node.RegisterManagedBranchReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.branchErrMap[req.Branch]; err != nil {
		return err
	}
	if f.branchErr != nil {
		return f.branchErr
	}
	f.branches = append(f.branches, req)
	return nil
}

func (f *fakeRunNotifier) NotifyNodeEvent(_ context.Context, ev node.NodeEventNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

type fakeReminderScheduler struct {
	mu              sync.Mutex
	registeredNames []string
	unregistered    []string
}

func (f *fakeReminderScheduler) RegisterSlotRetryReminder(_ context.Context, _, _, reminderName string, _, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registeredNames = append(f.registeredNames, reminderName)
	return nil
}

func (f *fakeReminderScheduler) UnregisterSlotRetryReminder(_ context.Context, _, _, reminderName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered = append(f.unregistered, reminderName)
	return nil
}

func newDirectStub(actorID string, a *node.NodeActor) *node.ClientStub {
	return &node.ClientStub{
		ActorID:                  actorID,
		StartOrResume:            a.StartOrResume,
		OnWorkerDead:             a.OnWorkerDead,
		PauseNode:                a.PauseNode,
		AdvanceForRework:         a.AdvanceForRework,
		ResetForRerun:            a.ResetForRerun,
		ReportNodeOutcome:        a.ReportNodeOutcome,
		SubmitDecision:           a.SubmitDecision,
		GetState:                 a.GetState,
		RegisterCheckpointBranch: a.RegisterCheckpointBranch,
		RegisterWorkBranch:       a.RegisterWorkBranch,
		BeginCheckpoint:          a.BeginCheckpoint,
		AppendFinalSHA:           a.AppendFinalSHA,
		CompleteCheckpoint:       a.CompleteCheckpoint,
		ListCheckpoints:          a.ListCheckpoints,
		EnqueueInterjection:      a.EnqueueInterjection,
		DrainInterjections:       a.DrainInterjections,
		HasPendingInterjections:  a.HasPendingInterjections,
		SaveQCLedgerSnapshot:     a.SaveQCLedgerSnapshot,
		LoadQCLedgerSnapshot:     a.LoadQCLedgerSnapshot,
		SavePendingPublication:   a.SavePendingPublication,
		LoadPendingPublication:   a.LoadPendingPublication,
		GetApproval:              a.GetApproval,
	}
}

type testRig struct {
	actorID   string
	sm        *memoryStateManager
	cfg       *fakeConfigResolver
	slots     *fakeSlotManager
	workers   *fakeWorkerDispatcher
	runNotify *fakeRunNotifier
	reminders *fakeReminderScheduler
	actor     *node.NodeActor
	stub      *node.ClientStub
}

func newTestRig(t *testing.T, runID, nodeAddr string) *testRig {
	t.Helper()
	actorID, err := node.EncodeNodeActorID(runID, nodeAddr)
	require.NoError(t, err)

	sm := newMemoryStateManager()
	cfg := &fakeConfigResolver{values: map[string]string{}}
	slots := &fakeSlotManager{}
	workers := &fakeWorkerDispatcher{}
	runNotify := &fakeRunNotifier{branchErrMap: map[string]error{}}
	reminders := &fakeReminderScheduler{}

	makeActor := func() *node.NodeActor {
		a := node.NewNodeActor(node.Options{
			ConfigResolver:    cfg,
			SlotManager:       slots,
			WorkerDispatcher:  workers,
			RunNotifier:       runNotify,
			ReminderScheduler: reminders,
			SlotRetryInterval: 50 * time.Millisecond,
		})
		a.SetID(actorID)
		a.SetStateManager(sm)
		return a
	}

	a := makeActor()
	return &testRig{
		actorID:   actorID,
		sm:        sm,
		cfg:       cfg,
		slots:     slots,
		workers:   workers,
		runNotify: runNotify,
		reminders: reminders,
		actor:     a,
		stub:      newDirectStub(actorID, a),
	}
}

func baseStartReq(runID, nodeAddr string) *node.StartOrResumeReq {
	return &node.StartOrResumeReq{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		Namespace:   "default",
		RepoURL:     "https://example.com/repo.git",
		BasedSource: "main",
		BaseSHA:     sha1,
		Worker: workerspec.Config{
			HumanApproval: "optional",
		},
	}
}

func TestEncodeDecodeNodeActorID_URLSafeLosslessRoundTrip(t *testing.T) {
	urlSafeRe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

	cases := []struct {
		runID    string
		nodeAddr string
	}{
		{"run-1", "worker.impl"},
		{"run_2026/11", `dynamic_worker.x["a/b"]`},
		{"run:special", `module.child["k\"quote/slash[1] % space"]`},
		{"run__double", `dynamic_worker.matrix["测试/路径"]`},
	}

	for _, tc := range cases {
		id, err := node.EncodeNodeActorID(tc.runID, tc.nodeAddr)
		require.NoError(t, err)
		assert.Regexp(t, urlSafeRe, id, "actor ID must only contain [A-Za-z0-9_-]")
		assert.NotContains(t, id, "/")
		assert.NotContains(t, id, "%")
		assert.NotContains(t, id, `"`)
		assert.NotContains(t, id, " ")

		gotRun, gotAddr, err := node.DecodeNodeActorID(id)
		require.NoError(t, err)
		assert.Equal(t, tc.runID, gotRun)
		assert.Equal(t, tc.nodeAddr, gotAddr)
	}

	_, err := node.EncodeNodeActorID("", "worker.impl")
	require.Error(t, err)
	_, err = node.EncodeNodeActorID("run-1", "   ")
	require.Error(t, err)
	_, _, err = node.DecodeNodeActorID("invalid-without-separator")
	require.Error(t, err)
}

func TestNodeActor_EpochFencingAcrossAllControlPlaneRPCs(t *testing.T) {
	ctx := context.Background()
	runID := "run-fence"
	nodeAddr := `dynamic_worker.x["a/b"]`
	rig := newTestRig(t, runID, nodeAddr)

	startResp, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)
	assert.Equal(t, uint64(1), startResp.Epoch)
	assert.Equal(t, node.StatusRunning, startResp.Status)

	// Pause immediately and resume -> advances authoritative epoch from 1 to 2.
	pauseResp, err := rig.actor.PauseNode(ctx, &node.PauseNodeReq{Mode: "immediate", Reason: "preempt"})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), pauseResp.Epoch)
	assert.Equal(t, node.StatusPaused, pauseResp.Status)
	assert.True(t, pauseResp.AbortedWorker)

	resumeResp, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, resumeResp.Epoch, uint64(2))
	activeEpoch := resumeResp.Epoch

	activeCP, err := node.NewRemoteNodeControlPlane(rig.stub, activeEpoch)
	require.NoError(t, err)
	zombieCP, err := node.NewRemoteNodeControlPlane(rig.stub, 1)
	require.NoError(t, err)

	// Active epoch creates checkpoint seq=1, QC snapshot, pending publication, and queued interjection.
	ckptBranch := git.CheckpointBranchName(runID, nodeAddr, activeEpoch)
	require.NoError(t, activeCP.RegisterCheckpointBranch(ctx, activeEpoch, "https://example.com/repo.git", ckptBranch, sha1))
	diary := session.DiaryInput{Completed: []string{"step1"}, NextSteps: []string{"step2"}}
	seq, err := activeCP.BeginCheckpoint(ctx, activeEpoch, ckptBranch, sha1, diary)
	require.NoError(t, err)
	require.NoError(t, activeCP.SaveQCLedgerSnapshot(ctx, activeEpoch, []byte(`{"epoch":2}`)))
	require.NoError(t, activeCP.SavePendingPublication(ctx, activeEpoch, &runner.PendingPublication{
		CommitSHA:   sha2,
		BaseSHA:     sha1,
		ArtifactIDs: []string{"art-1"},
	}))
	_, err = rig.actor.EnqueueInterjection(ctx, &node.EnqueueInterjectionReq{Message: "keep going"})
	require.NoError(t, err)

	// Zombie worker at epoch=1 MUST get checkpoint.ErrStaleEpoch on every mutating RPC and MUST NOT pollute activeEpoch state.
	err = zombieCP.RegisterCheckpointBranch(ctx, 1, "https://example.com/repo.git", git.CheckpointBranchName(runID, nodeAddr, 1), sha1)
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	_, err = zombieCP.BeginCheckpoint(ctx, 1, git.CheckpointBranchName(runID, nodeAddr, 1), sha1, diary)
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	err = zombieCP.AppendFinalSHA(ctx, 1, seq, sha3)
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	err = zombieCP.CompleteCheckpoint(ctx, 1, seq, sha3, &blob.ObjectMeta{
		Key:       "runs/run-fence/checkpoints/x/seq-000001-e1-artifacts.zip",
		SHA256:    strings.Repeat("a", 64),
		SizeBytes: 12,
	})
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	err = zombieCP.SaveQCLedgerSnapshot(ctx, 1, []byte(`{"zombie":true}`))
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	err = zombieCP.SavePendingPublication(ctx, 1, &runner.PendingPublication{CommitSHA: sha3})
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	err = zombieCP.RegisterWorkBranch(ctx, 1, "https://example.com/repo.git", git.WorkBranchName(runID, nodeAddr), sha1)
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	_, err = zombieCP.DrainInterjections(ctx)
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

	outResp, err := rig.actor.ReportNodeOutcome(ctx, &daemon.NodeOutcomeReport{
		RunID:    runID,
		NodeAddr: nodeAddr,
		Epoch:    1,
		Outcome:  runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: "zombie failure"},
	})
	require.NoError(t, err)
	assert.True(t, outResp.StaleEpoch)

	// Active epoch state must remain completely intact.
	qcSnap, err := activeCP.LoadQCLedgerSnapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"epoch":2}`), qcSnap)

	pub, err := activeCP.LoadPendingPublication(ctx)
	require.NoError(t, err)
	require.NotNil(t, pub)
	assert.Equal(t, sha2, pub.CommitSHA)

	items, err := activeCP.DrainInterjections(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "keep going", items[0].Message)

	ckpts, err := activeCP.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Len(t, ckpts, 1)
	assert.Equal(t, checkpoint.StatusPending, ckpts[0].Status)
	assert.Empty(t, ckpts[0].FinalSHA)
}

func TestNodeActor_QueuedSlotReminderRetryAndExplicitPersistence(t *testing.T) {
	ctx := context.Background()
	runID := "run-queue"
	nodeAddr := "worker.build"
	rig := newTestRig(t, runID, nodeAddr)

	rig.slots.claimResponses = []registry.ClaimSlotResp{
		{Granted: false, Reason: "all workers full"},
		{Granted: false, Reason: "still full"},
		{Granted: true, LeaseID: "lease-ready", LeaseKey: "k1", ProcessID: "proc-ready", AppID: "sofa-worker-proc-ready"},
	}

	resp, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)
	assert.Equal(t, node.StatusQueuedSlot, resp.Status)
	assert.Equal(t, uint64(1), resp.Epoch)
	assert.NotEmpty(t, rig.reminders.registeredNames)
	assert.Empty(t, rig.workers.startCalls)

	// First reminder tick: still saturated -> remains QUEUED_SLOT and persists explicitly.
	savesBefore := rig.sm.SaveCount()
	rig.actor.ReminderCall(node.DefaultSlotRetryReminderName, nil, "", "")
	stateAfterFirstTick, err := rig.actor.GetState(ctx, &node.GetNodeStateReq{})
	require.NoError(t, err)
	assert.Equal(t, node.StatusQueuedSlot, stateAfterFirstTick.Status)

	// Second reminder tick: slot granted -> transitions to RUNNING, dispatches StartNode,
	// unregisters reminder, and explicitly persists state (verified via fresh NodeActor reload).
	rig.actor.ReminderCall(node.DefaultSlotRetryReminderName, nil, "", "")
	assert.Greater(t, rig.sm.SaveCount(), savesBefore)
	require.Len(t, rig.workers.startCalls, 1)
	assert.Equal(t, "sofa-worker-proc-ready", rig.workers.startCalls[0].AppID)
	assert.NotEmpty(t, rig.reminders.unregistered)

	// Rehydrate a brand-new NodeActor from the same memoryStateManager to verify explicit Set+Save inside ReminderCall.
	reloaded := node.NewNodeActor(node.Options{})
	reloaded.SetID(rig.actorID)
	reloaded.SetStateManager(rig.sm)
	reloadedState, err := reloaded.GetState(ctx, &node.GetNodeStateReq{})
	require.NoError(t, err)
	assert.Equal(t, node.StatusRunning, reloadedState.Status)
	assert.Equal(t, "lease-ready", reloadedState.LeaseID)
	assert.Equal(t, "proc-ready", reloadedState.WorkerProcessID)
	assert.Equal(t, "sofa-worker-proc-ready", reloadedState.WorkerAppID)

	// StartNode failure must release slot, ReportSuspect, and fall back to QUEUED_SLOT.
	rig2 := newTestRig(t, runID, "worker.suspect")
	rig2.workers.startErrs = []error{errors.New("connection refused")}
	resp2, err := rig2.actor.StartOrResume(ctx, baseStartReq(runID, "worker.suspect"))
	require.NoError(t, err)
	assert.Equal(t, node.StatusQueuedSlot, resp2.Status)
	require.Len(t, rig2.slots.releaseCalls, 1)
	require.Len(t, rig2.slots.suspectCalls, 1)
	assert.Equal(t, "proc-1", rig2.slots.suspectCalls[0].ProcessID)
}

func TestNodeActor_OnWorkerDeadTakeover(t *testing.T) {
	ctx := context.Background()
	runID := "run-dead"
	nodeAddr := "worker.code"
	rig := newTestRig(t, runID, nodeAddr)

	startResp, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)
	assert.Equal(t, node.StatusRunning, startResp.Status)
	assert.Equal(t, uint64(1), startResp.Epoch)
	assert.Equal(t, "lease-1", startResp.LeaseID)

	// Mismatched/stale dead notification must be ignored.
	staleResp, err := rig.actor.OnWorkerDead(ctx, &registry.WorkerDeadNotification{
		LeaseID:   "lease-old",
		ProcessID: "proc-old",
		AppID:     "sofa-worker-proc-old",
		Reason:    "stale",
	})
	require.NoError(t, err)
	assert.False(t, staleResp.Handled)
	assert.Equal(t, uint64(1), staleResp.Epoch)

	// Matching active lease notification increments epoch and re-dispatches to new worker.
	deadResp, err := rig.actor.OnWorkerDead(ctx, &registry.WorkerDeadNotification{
		LeaseID:   startResp.LeaseID,
		ProcessID: startResp.WorkerProcessID,
		AppID:     startResp.WorkerAppID,
		Reason:    "ping failed 3 times",
	})
	require.NoError(t, err)
	assert.True(t, deadResp.Handled)
	assert.Equal(t, uint64(2), deadResp.Epoch)
	assert.Equal(t, node.StatusRunning, deadResp.Status)

	require.Len(t, rig.workers.startCalls, 2)
	assert.Equal(t, uint64(2), rig.workers.startCalls[1].Req.Epoch)
	assert.Equal(t, "proc-2", rig.workers.startCalls[1].Req.ProcessID)
}

func TestNodeActor_GitCredentialMaterialization(t *testing.T) {
	ctx := context.Background()
	runID := "run-creds"
	nodeAddr := "worker.git"
	rig := newTestRig(t, runID, nodeAddr)

	tokenRef := "GITHUB_TOKEN"
	rig.cfg.values = map[string]string{
		"GEMINI_API_KEY": "gem-secret-val",
		"GITHUB_TOKEN":   "ghp-secret-val",
		"UNRELATED_KEY":  "must-not-leak",
	}

	req := baseStartReq(runID, nodeAddr)
	req.RequiredEnvKeys = []string{"GEMINI_API_KEY"}
	req.TokenRef = &tokenRef

	resp, err := rig.actor.StartOrResume(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, node.StatusRunning, resp.Status)

	require.Len(t, rig.workers.startCalls, 1)
	dispatched := rig.workers.startCalls[0].Req
	assert.Equal(t, map[string]string{
		"GEMINI_API_KEY": "gem-secret-val",
		"GITHUB_TOKEN":   "ghp-secret-val",
	}, dispatched.ResolvedEnv)
	require.NotNil(t, dispatched.Auth.TokenRef)
	assert.Equal(t, "GITHUB_TOKEN", *dispatched.Auth.TokenRef)
	assert.Nil(t, dispatched.Auth.SSHKeyRef)

	// Missing required credential must fail cleanly without calling StartNode.
	rigMissing := newTestRig(t, runID, "worker.missing")
	reqMissing := baseStartReq(runID, "worker.missing")
	reqMissing.TokenRef = &tokenRef
	respMissing, err := rigMissing.actor.StartOrResume(ctx, reqMissing)
	require.NoError(t, err)
	assert.Equal(t, node.StatusFailed, respMissing.Status)
	assert.Contains(t, respMissing.ErrorMessage, "GITHUB_TOKEN")
	assert.Empty(t, rigMissing.workers.startCalls)
}

func TestNodeActor_HumanApprovalVersionBindingAndAutoInvalidation(t *testing.T) {
	ctx := context.Background()
	runID := "run-approval"
	nodeAddr := "worker.deploy"
	rig := newTestRig(t, runID, nodeAddr)

	req := baseStartReq(runID, nodeAddr)
	req.Worker.HumanApproval = "required"
	startResp, err := rig.actor.StartOrResume(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), startResp.Epoch)

	// Epoch 1 produces commit sha1 + artifact ["art-1"] -> WAITING_HUMAN + PENDING DecisionCard.
	out1, err := rig.actor.ReportNodeOutcome(ctx, &daemon.NodeOutcomeReport{
		RunID:     runID,
		NodeAddr:  nodeAddr,
		Epoch:     1,
		LeaseID:   startResp.LeaseID,
		ProcessID: startResp.WorkerProcessID,
		Outcome: runner.NodeOutcome{
			Status: runner.StatusWaitingHuman,
			PendingApproval: &runner.ApprovalTarget{
				CommitSHA:   sha1,
				ArtifactIDs: []string{"art-1"},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, node.StatusWaitingHuman, out1.Status)
	require.NotNil(t, out1.DecisionCard)
	assert.Equal(t, node.DecisionKindHumanApproval, out1.DecisionCard.Kind)
	assert.Equal(t, node.DecisionStatusPending, out1.DecisionCard.Status)
	card1ID := out1.DecisionCard.CardID

	// Mismatched CommitSHA or ArtifactIDs MUST be rejected.
	approved := true
	_, err = rig.actor.SubmitDecision(ctx, &node.SubmitDecisionReq{
		CardID:      card1ID,
		Approved:    &approved,
		CommitSHA:   sha2,
		ArtifactIDs: []string{"art-1"},
	})
	require.Error(t, err)

	_, err = rig.actor.SubmitDecision(ctx, &node.SubmitDecisionReq{
		CardID:      card1ID,
		Approved:    &approved,
		CommitSHA:   sha1,
		ArtifactIDs: []string{"art-other"},
	})
	require.Error(t, err)

	// Exact match succeeds and auto-resumes at epoch=2.
	sub1, err := rig.actor.SubmitDecision(ctx, &node.SubmitDecisionReq{
		CardID:      card1ID,
		Approved:    &approved,
		CommitSHA:   sha1,
		ArtifactIDs: []string{"art-1"},
	})
	require.NoError(t, err)
	assert.True(t, sub1.Accepted)
	assert.True(t, sub1.AutoResumed)
	assert.Equal(t, uint64(2), sub1.Epoch)

	cp2, err := node.NewRemoteNodeControlPlane(rig.stub, 2)
	require.NoError(t, err)
	appRec, err := cp2.GetApproval(ctx)
	require.NoError(t, err)
	require.NotNil(t, appRec)
	assert.True(t, appRec.Approved)
	assert.Equal(t, sha1, appRec.CommitSHA)

	// If an interjection during epoch=2 causes new commit sha2 + artifacts ["art-1", "art-2"] to be produced,
	// the old approval for sha1 MUST be invalidated automatically and a new WAITING_HUMAN card created.
	state2, err := rig.actor.GetState(ctx, &node.GetNodeStateReq{})
	require.NoError(t, err)
	out2, err := rig.actor.ReportNodeOutcome(ctx, &daemon.NodeOutcomeReport{
		RunID:     runID,
		NodeAddr:  nodeAddr,
		Epoch:     2,
		LeaseID:   state2.LeaseID,
		ProcessID: state2.WorkerProcessID,
		Outcome: runner.NodeOutcome{
			Status: runner.StatusWaitingHuman,
			PendingApproval: &runner.ApprovalTarget{
				CommitSHA:   sha2,
				ArtifactIDs: []string{"art-1", "art-2"},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, node.StatusWaitingHuman, out2.Status)
	require.NotNil(t, out2.DecisionCard)
	assert.NotEqual(t, card1ID, out2.DecisionCard.CardID)
	assert.Equal(t, sha2, out2.DecisionCard.CommitSHA)

	appAfter, err := cp2.GetApproval(ctx)
	require.NoError(t, err)
	assert.Nil(t, appAfter, "stale approval for sha1 must be cleared when sha2 is produced")

	// Submitting decision against superseded/old card1ID must fail.
	_, err = rig.actor.SubmitDecision(ctx, &node.SubmitDecisionReq{
		CardID:      card1ID,
		Approved:    &approved,
		CommitSHA:   sha1,
		ArtifactIDs: []string{"art-1"},
	})
	require.Error(t, err)
}

func TestNodeActor_SubmitDecisionManualPauseGuardAndPauseForHuman(t *testing.T) {
	ctx := context.Background()
	runID := "run-human"
	nodeAddr := "worker.design"
	rig := newTestRig(t, runID, nodeAddr)

	startResp, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)

	// Operator enqueues a wait_for_confirmation interjection.
	enq, err := rig.actor.EnqueueInterjection(ctx, &node.EnqueueInterjectionReq{
		Message:             "Confirm API style?",
		WaitForConfirmation: true,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, enq.ID)
	assert.True(t, enq.WaitForConfirmation)

	// Worker finishes turn via sofa_pause_for_human -> WAITING_HUMAN card created.
	outResp, err := rig.actor.ReportNodeOutcome(ctx, &daemon.NodeOutcomeReport{
		RunID:     runID,
		NodeAddr:  nodeAddr,
		Epoch:     1,
		LeaseID:   startResp.LeaseID,
		ProcessID: startResp.WorkerProcessID,
		Outcome: runner.NodeOutcome{
			Status:      runner.StatusWaitingHuman,
			PauseReason: "Answered API style question, awaiting confirmation",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, node.StatusWaitingHuman, outResp.Status)
	require.NotNil(t, outResp.DecisionCard)
	assert.Equal(t, node.DecisionKindPauseForHuman, outResp.DecisionCard.Kind)

	// Manually pause the node (or Run) before human submits decision.
	_, err = rig.actor.PauseNode(ctx, &node.PauseNodeReq{Mode: "planned", Reason: "run paused"})
	require.NoError(t, err)

	startCallsBefore := len(rig.workers.startCalls)
	subResp, err := rig.actor.SubmitDecision(ctx, &node.SubmitDecisionReq{
		CardID:     outResp.DecisionCard.CardID,
		AnswerText: "Proceed with REST JSON",
		RunPaused:  true,
	})
	require.NoError(t, err)
	assert.True(t, subResp.Accepted)
	assert.False(t, subResp.AutoResumed)
	assert.Equal(t, node.DecisionStatusAnswered, subResp.Card.Status)
	assert.Equal(t, startCallsBefore, len(rig.workers.startCalls), "must never start worker container while paused")

	// When RunActor later resumes, StartOrResume clears ManualPaused and dispatches with the queued answer interjection.
	resumed, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)
	assert.Equal(t, node.StatusRunning, resumed.Status)
	assert.Equal(t, uint64(2), resumed.Epoch)

	cp2, err := node.NewRemoteNodeControlPlane(rig.stub, 2)
	require.NoError(t, err)
	interjs, err := cp2.DrainInterjections(ctx)
	require.NoError(t, err)
	require.Len(t, interjs, 2) // original enqueued if undrained + decision answer
	assert.Contains(t, interjs[len(interjs)-1].Message, "Proceed with REST JSON")
}

func TestNodeActor_BranchLedgerSyncAndReworkRerun(t *testing.T) {
	ctx := context.Background()
	runID := "run-rework"
	nodeAddr := "worker.feature"
	rig := newTestRig(t, runID, nodeAddr)

	_, err := rig.actor.StartOrResume(ctx, baseStartReq(runID, nodeAddr))
	require.NoError(t, err)

	// RunNotifier error on protected branch must block RegisterWorkBranch synchronously.
	rig.runNotify.branchErrMap["main"] = git.ErrProtectedSourceBranch
	cp1, err := node.NewRemoteNodeControlPlane(rig.stub, 1)
	require.NoError(t, err)
	err = cp1.RegisterWorkBranch(ctx, 1, "https://example.com/repo.git", "main", sha1)
	require.ErrorIs(t, err, git.ErrProtectedSourceBranch)

	workBranch := git.WorkBranchName(runID, nodeAddr)
	require.NoError(t, cp1.RegisterWorkBranch(ctx, 1, "https://example.com/repo.git", workBranch, sha1))
	require.Len(t, rig.runNotify.branches, 1)
	assert.Equal(t, git.KindWorkBranch, rig.runNotify.branches[0].Kind)

	// Save pending publication & approval at epoch=1, then trigger AdvanceForRework with upgraded BaseSHA=sha2.
	require.NoError(t, cp1.SavePendingPublication(ctx, 1, &runner.PendingPublication{CommitSHA: sha1}))
	reworkResp, err := rig.actor.AdvanceForRework(ctx, &node.AdvanceForReworkReq{
		BaseSHA: sha2,
		Reason:  "Merge conflict in shared.txt against latest main",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), reworkResp.Epoch)
	assert.Equal(t, node.StatusPending, reworkResp.Status)
	assert.True(t, reworkResp.AbortedWorker)
	require.Len(t, rig.workers.abortCalls, 1)

	pubAfterRework, err := cp1.LoadPendingPublication(ctx)
	require.NoError(t, err)
	assert.Nil(t, pubAfterRework)

	// ResetForRerun increments epoch again and leaves clean PENDING state.
	rerunResp, err := rig.actor.ResetForRerun(ctx, &node.ResetForRerunReq{Reason: "manual rerun"})
	require.NoError(t, err)
	assert.Equal(t, uint64(3), rerunResp.Epoch)
	assert.Equal(t, node.StatusPending, rerunResp.Status)

	// Re-dispatch after rework carries upgraded BaseSHA=sha2 and rework reason interjection.
	dispatchAfter, err := rig.actor.StartOrResume(ctx, &node.StartOrResumeReq{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		RepoURL:     "https://example.com/repo.git",
		BasedSource: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(3), dispatchAfter.Epoch)
	lastStart := rig.workers.startCalls[len(rig.workers.startCalls)-1]
	assert.Equal(t, sha2, lastStart.Req.BaseSHA)

	cp3, err := node.NewRemoteNodeControlPlane(rig.stub, 3)
	require.NoError(t, err)
	drained, err := cp3.DrainInterjections(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, drained)
	assert.True(t, slices.ContainsFunc(drained, func(item session.HumanInterjection) bool {
		return strings.Contains(item.Message, "Merge conflict in shared.txt")
	}))
}

func TestNodeActor_DispatchSpecImportsAndProviderHeadersJSONRoundTrip(t *testing.T) {
	importSources := cty.TupleVal([]cty.Value{
		cty.ObjectVal(map[string]cty.Value{
			"id":   cty.StringVal("art-1"),
			"name": cty.StringVal("design_spec"),
		}),
	})
	nestedMarkedHeaders := cty.MapVal(map[string]cty.Value{
		"X-Api-Key": cty.StringVal("hdr-secret").Mark(golden.SensitiveMark),
		"X-Trace":   cty.StringVal("trace-1"),
	})
	rootMarkedHeaders := cty.MapVal(map[string]cty.Value{
		"Authorization": cty.StringVal("Bearer root-secret"),
	}).Mark(golden.SensitiveMark)

	spec := node.DispatchSpec{
		RunID:    "run-json-codec",
		NodeAddr: "worker.impl",
		RepoURL:  "https://example.com/repo.git",
		Worker: workerspec.Config{
			Imports: map[string]workerspec.ImportArtifact{
				"design_spec": {
					Name:    "design_spec",
					Desc:    "imported spec",
					Path:    "design_spec.md",
					Sources: importSources,
				},
			},
		},
		ModelProviders: map[string]provider.Config{
			"nested": {
				Type:     provider.TypeOpenAI,
				Endpoint: "https://api.example.com",
				Headers:  nestedMarkedHeaders,
			},
			"root": {
				Type:     provider.TypeAnthropic,
				Endpoint: "https://anthropic.example.com",
				Headers:  rootMarkedHeaders,
			},
		},
	}

	rawSpec, err := json.Marshal(spec)
	require.NoError(t, err)

	var decodedSpec node.DispatchSpec
	require.NoError(t, json.Unmarshal(rawSpec, &decodedSpec))

	// Also verify daemon.StartNodeRequest round-trip.
	startReq := daemon.StartNodeRequest{
		RunID:          spec.RunID,
		NodeAddr:       spec.NodeAddr,
		Epoch:          1,
		RepoURL:        spec.RepoURL,
		Worker:         decodedSpec.Worker,
		ModelProviders: decodedSpec.ModelProviders,
	}
	rawStart, err := json.Marshal(startReq)
	require.NoError(t, err)

	var decodedStart daemon.StartNodeRequest
	require.NoError(t, json.Unmarshal(rawStart, &decodedStart))

	gotImport := decodedStart.Worker.Imports["design_spec"]
	require.NotEqual(t, cty.NilVal, gotImport.Sources)
	require.Equal(t, 1, gotImport.Sources.LengthInt())
	assert.Equal(t, "art-1", gotImport.Sources.Index(cty.NumberIntVal(0)).GetAttr("id").AsString())

	gotNested := decodedStart.ModelProviders["nested"]
	normNested, err := provider.NormalizeHeaders(gotNested.Headers)
	require.NoError(t, err)
	assert.True(t, provider.HeadersSensitive(normNested), "nested sensitive mark must survive JSON round-trip")
	unmarkedNested, _ := normNested.UnmarkDeep()
	assert.Equal(t, "hdr-secret", unmarkedNested.Index(cty.StringVal("X-Api-Key")).AsString())
	assert.Equal(t, "trace-1", unmarkedNested.Index(cty.StringVal("X-Trace")).AsString())
	assert.True(t, normNested.Index(cty.StringVal("X-Api-Key")).HasMark(golden.SensitiveMark))
	assert.False(t, normNested.Index(cty.StringVal("X-Trace")).HasMark(golden.SensitiveMark))

	gotRoot := decodedStart.ModelProviders["root"]
	normRoot, err := provider.NormalizeHeaders(gotRoot.Headers)
	require.NoError(t, err)
	assert.True(t, provider.HeadersSensitive(normRoot), "root sensitive mark must survive JSON round-trip")
	assert.True(t, normRoot.HasMark(golden.SensitiveMark))
	unmarkedRoot, _ := normRoot.UnmarkDeep()
	assert.Equal(t, "Bearer root-secret", unmarkedRoot.Index(cty.StringVal("Authorization")).AsString())
}
