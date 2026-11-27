package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonegunmanb/golden"
	actorcfg "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	appcfg "github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

const (
	shaBase  = "1111111111111111111111111111111111111111"
	shaW1    = "2222222222222222222222222222222222222222"
	shaW2    = "3333333333333333333333333333333333333333"
	shaW3    = "4444444444444444444444444444444444444444"
	shaMerge = "5555555555555555555555555555555555555555"
	shaRew   = "6666666666666666666666666666666666666666"
)

type memoryStateManager struct {
	mu        sync.Mutex
	persisted map[string][]byte
	staged    map[string]stagedEntry
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
	return nil
}

func (m *memoryStateManager) Flush(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staged = make(map[string]stagedEntry)
}

type fakeNodeRecord struct {
	epoch        uint64
	status       node.NodeStatus
	lastReq      *node.StartOrResumeReq
	startCalls   int
	pauseCalls   []node.PauseNodeReq
	advanceCalls []node.AdvanceForReworkReq
	resetCalls   []node.ResetForRerunReq
	cards        []node.DecisionCard
}

type fakeNodeController struct {
	mu      sync.Mutex
	records map[string]*fakeNodeRecord
}

func newFakeNodeController() *fakeNodeController {
	return &fakeNodeController{records: make(map[string]*fakeNodeRecord)}
}

func (f *fakeNodeController) key(runID, addr string) string {
	return runID + "::" + addr
}

func (f *fakeNodeController) getOrInit(runID, addr string) *fakeNodeRecord {
	k := f.key(runID, addr)
	rec, ok := f.records[k]
	if !ok {
		rec = &fakeNodeRecord{epoch: 1, status: node.StatusPending}
		f.records[k] = rec
	}
	return rec
}

func (f *fakeNodeController) StartOrResume(_ context.Context, runID, canonicalNodeAddr string, req *node.StartOrResumeReq) (*node.StartOrResumeResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.getOrInit(runID, canonicalNodeAddr)
	if rec.status == node.StatusPaused || rec.status == node.StatusFailed || rec.status == node.StatusWaitingHuman {
		rec.epoch++
	}
	rec.status = node.StatusRunning
	rec.lastReq = req
	rec.startCalls++
	return &node.StartOrResumeResp{
		Accepted: true,
		Epoch:    rec.epoch,
		Status:   node.StatusRunning,
	}, nil
}

func (f *fakeNodeController) PauseNode(_ context.Context, runID, canonicalNodeAddr string, req *node.PauseNodeReq) (*node.PauseNodeResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.getOrInit(runID, canonicalNodeAddr)
	rec.pauseCalls = append(rec.pauseCalls, *req)
	if req.Mode == "immediate" {
		if rec.status == node.StatusRunning || rec.status == node.StatusQueuedSlot {
			rec.epoch++
		}
		rec.status = node.StatusPaused
	} else if rec.status == node.StatusQueuedSlot || rec.status == node.StatusPending {
		rec.status = node.StatusPaused
	}
	return &node.PauseNodeResp{
		Paused:        true,
		Epoch:         rec.epoch,
		Status:        rec.status,
		AbortedWorker: req.Mode == "immediate",
	}, nil
}

func (f *fakeNodeController) AdvanceForRework(_ context.Context, runID, canonicalNodeAddr string, req *node.AdvanceForReworkReq) (*node.AdvanceForReworkResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.getOrInit(runID, canonicalNodeAddr)
	rec.epoch++
	rec.status = node.StatusPending
	rec.advanceCalls = append(rec.advanceCalls, *req)
	return &node.AdvanceForReworkResp{
		Epoch:  rec.epoch,
		Status: node.StatusPending,
	}, nil
}

func (f *fakeNodeController) ResetForRerun(_ context.Context, runID, canonicalNodeAddr string, req *node.ResetForRerunReq) (*node.ResetForRerunResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.getOrInit(runID, canonicalNodeAddr)
	rec.epoch++
	rec.status = node.StatusPending
	rec.resetCalls = append(rec.resetCalls, *req)
	return &node.ResetForRerunResp{
		Epoch:  rec.epoch,
		Status: node.StatusPending,
	}, nil
}

func (f *fakeNodeController) GetNodeState(_ context.Context, runID, canonicalNodeAddr string) (*node.GetNodeStateResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.getOrInit(runID, canonicalNodeAddr)
	return &node.GetNodeStateResp{
		RunID:         runID,
		NodeAddr:      canonicalNodeAddr,
		CurrentEpoch:  rec.epoch,
		Status:        rec.status,
		DecisionCards: slices.Clone(rec.cards),
	}, nil
}

func (f *fakeNodeController) snapshot(runID, addr string) fakeNodeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.getOrInit(runID, addr)
	cp := *rec
	cp.pauseCalls = slices.Clone(rec.pauseCalls)
	cp.advanceCalls = slices.Clone(rec.advanceCalls)
	cp.resetCalls = slices.Clone(rec.resetCalls)
	return cp
}

type fakeMergeController struct {
	mu           sync.Mutex
	enqueued     []merge.EnqueueCandidateReq
	cancelCalls  []merge.CancelByRunReq
	targetStates map[string]*merge.GetMergeStateResp
}

func newFakeMergeController() *fakeMergeController {
	return &fakeMergeController{targetStates: make(map[string]*merge.GetMergeStateResp)}
}

func (f *fakeMergeController) EnqueueCandidate(_ context.Context, repoURL, effectiveTargetBranch string, req *merge.EnqueueCandidateReq) (*merge.EnqueueCandidateResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, prev := range f.enqueued {
		if prev.RunID == req.RunID &&
			prev.MergeNodeAddr == req.MergeNodeAddr &&
			prev.MergeEpoch == req.MergeEpoch &&
			prev.SourceNodeAddr == req.SourceNodeAddr &&
			prev.SourceEpoch == req.SourceEpoch {
			return &merge.EnqueueCandidateResp{
				Accepted:     true,
				Deduplicated: true,
				Ticket: merge.CandidateTicket{
					RequestID:       fmt.Sprintf("req-%s-e%d", req.SourceNodeAddr, req.SourceEpoch),
					RunID:           req.RunID,
					MergeNodeAddr:   req.MergeNodeAddr,
					MergeEpoch:      req.MergeEpoch,
					SourceNodeAddr:  req.SourceNodeAddr,
					SourceEpoch:     req.SourceEpoch,
					TargetBranch:    effectiveTargetBranch,
					CandidateBranch: req.CandidateBranch,
					Status:          merge.TicketStatusPending,
				},
			}, nil
		}
	}
	f.enqueued = append(f.enqueued, *req)
	return &merge.EnqueueCandidateResp{
		Accepted: true,
		Ticket: merge.CandidateTicket{
			RequestID:       fmt.Sprintf("req-%s-e%d", req.SourceNodeAddr, req.SourceEpoch),
			RunID:           req.RunID,
			MergeNodeAddr:   req.MergeNodeAddr,
			MergeEpoch:      req.MergeEpoch,
			SourceNodeAddr:  req.SourceNodeAddr,
			SourceEpoch:     req.SourceEpoch,
			TargetBranch:    effectiveTargetBranch,
			CandidateBranch: req.CandidateBranch,
			Status:          merge.TicketStatusPending,
		},
	}, nil
}

func (f *fakeMergeController) CancelByRun(_ context.Context, _, _ string, req *merge.CancelByRunReq) (*merge.CancelByRunResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, *req)
	return &merge.CancelByRunResp{CancelledPending: 1}, nil
}

func (f *fakeMergeController) GetMergeState(_ context.Context, repoURL, effectiveTargetBranch string) (*merge.GetMergeStateResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := git.NormalizeRepoURL(repoURL) + "\x00" + effectiveTargetBranch
	if st, ok := f.targetStates[k]; ok {
		return st, nil
	}
	return &merge.GetMergeStateResp{RepoURL: repoURL, TargetBranch: effectiveTargetBranch}, nil
}

func (f *fakeMergeController) enqueuedSnapshots() []merge.EnqueueCandidateReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.enqueued)
}

func (f *fakeMergeController) cancelSnapshots() []merge.CancelByRunReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cancelCalls)
}

type fakeGitAdapter struct {
	mu              sync.Mutex
	branchSHAs      map[string]string
	deletedBranches []git.BranchTarget
	pushedCommits   []string
}

func newFakeGitAdapter() *fakeGitAdapter {
	return &fakeGitAdapter{
		branchSHAs: make(map[string]string),
	}
}

func (g *fakeGitAdapter) ResolveBranchSHA(_ context.Context, repoURL, branch string, _ git.AuthConfig) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha, ok := g.branchSHAs[repoURL+"\x00"+branch]; ok {
		return sha, nil
	}
	if sha, ok := g.branchSHAs[branch]; ok {
		return sha, nil
	}
	return shaBase, nil
}

func (g *fakeGitAdapter) PrepareWorkspace(_ context.Context, _, _, _, destDir string, _ git.AuthConfig) error {
	return os.MkdirAll(destDir, 0o755)
}
func (g *fakeGitAdapter) HeadSHA(_ context.Context, _ string) (string, error) { return shaBase, nil }
func (g *fakeGitAdapter) StatusPaths(_ context.Context, _ string) ([]git.ChangedPath, error) {
	return nil, nil
}
func (g *fakeGitAdapter) RevertPaths(_ context.Context, _ string, _ []string) error { return nil }
func (g *fakeGitAdapter) CommitAllAndPush(_ context.Context, _, _, _, _ string, _ git.AuthConfig) (string, bool, error) {
	return shaBase, false, nil
}
func (g *fakeGitAdapter) PushCommit(_ context.Context, _, _, remoteBranch, commitSHA, _ string, _ git.AuthConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pushedCommits = append(g.pushedCommits, remoteBranch+"@"+commitSHA)
	return nil
}
func (g *fakeGitAdapter) BranchContainsSHA(_ context.Context, repoURL, remoteBranch, targetSHA string, _ git.AuthConfig) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sha := g.branchSHAs[repoURL+"\x00"+remoteBranch]
	return sha, sha != "" && sha == targetSHA, nil
}
func (g *fakeGitAdapter) DeleteRemoteBranch(_ context.Context, repoURL, remoteBranch string, _ git.AuthConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deletedBranches = append(g.deletedBranches, git.BranchTarget{RepoURL: repoURL, Branch: remoteBranch})
	return nil
}
func (g *fakeGitAdapter) RebaseCandidate(_ context.Context, _, _, _, _ string, _ git.AuthConfig) (git.RebaseOutcome, error) {
	return git.RebaseOutcome{}, nil
}

func (g *fakeGitAdapter) deleted() []git.BranchTarget {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.deletedBranches)
}

type fakeConfigManager struct {
	mu           sync.Mutex
	values       map[string]string
	deletedRuns  []string
	resolveCalls []actorcfg.ResolveForNodeReq
}

func newFakeConfigManager() *fakeConfigManager {
	return &fakeConfigManager{values: make(map[string]string)}
}

func (c *fakeConfigManager) ResolveForNode(_ context.Context, req *actorcfg.ResolveForNodeReq) (*actorcfg.ResolveForNodeResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolveCalls = append(c.resolveCalls, *req)
	out := make(map[string]string, len(req.Keys))
	var missing []string
	for _, k := range req.Keys {
		if v, ok := req.RunOverrides[k]; ok {
			out[k] = v
		} else if v, ok := c.values[k]; ok {
			out[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	return &actorcfg.ResolveForNodeResp{Values: out, MissingKeys: missing}, nil
}

func (c *fakeConfigManager) DeleteRunScope(_ context.Context, req *actorcfg.DeleteRunScopeReq) (*actorcfg.DeleteRunScopeResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedRuns = append(c.deletedRuns, req.RunID)
	return &actorcfg.DeleteRunScopeResp{DeletedCount: 1}, nil
}

type fakeReminderScheduler struct {
	mu         sync.Mutex
	registered map[string]bool
	unregCalls int
}

func newFakeReminderScheduler() *fakeReminderScheduler {
	return &fakeReminderScheduler{registered: make(map[string]bool)}
}

func (r *fakeReminderScheduler) EnsureReconcileReminder(_ context.Context, actorType, actorID, reminderName string, _, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registered[actorType+"/"+actorID+"/"+reminderName] = true
	return nil
}

func (r *fakeReminderScheduler) UnregisterReconcileReminder(_ context.Context, actorType, actorID, reminderName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.registered, actorType+"/"+actorID+"/"+reminderName)
	r.unregCalls++
	return nil
}

type fakeEventPublisher struct {
	mu     sync.Mutex
	events []any
}

func (p *fakeEventPublisher) PublishEvent(_ context.Context, _, _ string, data any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, data)
	return nil
}

type operatorAdapter struct {
	op *run.OperatorActor
}

func (o *operatorAdapter) UpsertRun(ctx context.Context, req *run.UpsertRunReq) (*run.UpsertRunResp, error) {
	return o.op.UpsertRun(ctx, req)
}

func (o *operatorAdapter) RemoveRun(ctx context.Context, req *run.RemoveRunReq) (*run.RemoveRunResp, error) {
	return o.op.RemoveRun(ctx, req)
}

func buildAndStoreSofaPlan(
	t *testing.T,
	ctx context.Context,
	store blob.Store,
	runID string,
	files map[string]string,
) (blob.ObjectMeta, *plan.Plan) {
	t.Helper()
	configDir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(configDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(strings.TrimLeft(content, "\n")), 0o600))
	}

	loaded, diags, err := appcfg.LoadDirectoryContext(ctx, configDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:    ctx,
		Directory:  configDir,
		BundlePath: "config",
	})
	require.NoError(t, cfg.Init(loaded.Blocks))
	require.NoError(t, cfg.RunPlan())

	planned, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "plan.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, planned, plan.ArchiveSource{
		ConfigDir: configDir,
	}))

	f, err := os.Open(archivePath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	meta, err := store.Put(ctx, fmt.Sprintf("runs/%s/plan/plan.sofaplan", runID), f, "")
	require.NoError(t, err)
	return meta, planned
}

type runHarness struct {
	ctx        context.Context
	runID      string
	blobStore  *blob.LocalStore
	nodeCtrl   *fakeNodeController
	mergeCtrl  *fakeMergeController
	gitAdapter *fakeGitAdapter
	cfgMgr     *fakeConfigManager
	remSched   *fakeReminderScheduler
	events     *fakeEventPublisher
	opActor    *run.OperatorActor
	runActor   *run.RunActor
	runSM      *memoryStateManager
	runClient  run.RunClient
}

func newRunHarness(t *testing.T, runID string) *runHarness {
	t.Helper()
	ctx := context.Background()
	bs, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	nodeCtrl := newFakeNodeController()
	mergeCtrl := newFakeMergeController()
	gitAdapter := newFakeGitAdapter()
	cfgMgr := newFakeConfigManager()
	remSched := newFakeReminderScheduler()
	events := &fakeEventPublisher{}

	var runActorRef *run.RunActor
	opActor := run.NewOperatorActor(run.OperatorOptions{
		RunReconciler: &funcReconciler{fn: func(ctx context.Context, id string, req *run.ReconcileReq) (*run.ReconcileResp, error) {
			if runActorRef != nil && id == runID {
				return runActorRef.Reconcile(ctx, req)
			}
			return &run.ReconcileResp{RunID: id}, nil
		}},
	})
	opActor.SetID(run.GlobalOperatorActorID)
	opActor.SetStateManager(newMemoryStateManager())

	bundleRoot := filepath.Join(t.TempDir(), "bundles")
	runSM := newMemoryStateManager()
	runActor := run.NewRunActor(run.RunOptions{
		BlobStore:         bs,
		BundleCacheRoot:   bundleRoot,
		NodeController:    nodeCtrl,
		MergeController:   mergeCtrl,
		OperatorIndex:     &operatorAdapter{op: opActor},
		ConfigManager:     cfgMgr,
		EventPublisher:    events,
		ReminderScheduler: remSched,
		GitAdapter:        gitAdapter,
	})
	runActor.SetID(runID)
	runActor.SetStateManager(runSM)
	runActorRef = runActor

	return &runHarness{
		ctx:        ctx,
		runID:      runID,
		blobStore:  bs,
		nodeCtrl:   nodeCtrl,
		mergeCtrl:  mergeCtrl,
		gitAdapter: gitAdapter,
		cfgMgr:     cfgMgr,
		remSched:   remSched,
		events:     events,
		opActor:    opActor,
		runActor:   runActor,
		runSM:      runSM,
		runClient:  run.NewDirectRunClient(runActor),
	}
}

type funcReconciler struct {
	fn func(context.Context, string, *run.ReconcileReq) (*run.ReconcileResp, error)
}

func (f *funcReconciler) Reconcile(ctx context.Context, runID string, req *run.ReconcileReq) (*run.ReconcileResp, error) {
	return f.fn(ctx, runID, req)
}

func (h *runHarness) rebuildColdRunActor(t *testing.T) {
	t.Helper()
	// Simulate cold actor activation using the exact same persisted Actor state & BlobStore (new BundleCacheRoot!).
	fresh := run.NewRunActor(run.RunOptions{
		BlobStore:         h.blobStore,
		BundleCacheRoot:   filepath.Join(t.TempDir(), "cold-bundles"),
		NodeController:    h.nodeCtrl,
		MergeController:   h.mergeCtrl,
		OperatorIndex:     &operatorAdapter{op: h.opActor},
		ConfigManager:     h.cfgMgr,
		EventPublisher:    h.events,
		ReminderScheduler: h.remSched,
		GitAdapter:        h.gitAdapter,
	})
	fresh.SetID(h.runID)
	fresh.SetStateManager(h.runSM)
	h.runActor = fresh
	h.runClient = run.NewDirectRunClient(fresh)
}

func (h *runHarness) completeNode(t *testing.T, canonAddr, workBranch, commitSHA string, resultJSON []byte, arts ...artifact.PublishedArtifact) {
	t.Helper()
	rec := h.nodeCtrl.snapshot(h.runID, canonAddr)
	h.nodeCtrl.mu.Lock()
	h.nodeCtrl.getOrInit(h.runID, canonAddr).status = node.StatusSucceeded
	h.nodeCtrl.mu.Unlock()

	_, err := h.runActor.OnNodeEvent(h.ctx, &node.NodeEventNotification{
		RunID:    h.runID,
		NodeAddr: canonAddr,
		Epoch:    rec.epoch,
		Status:   node.StatusSucceeded,
		Outcome: &runner.NodeOutcome{
			Status:     runner.StatusSucceeded,
			WorkBranch: workBranch,
			CommitSHA:  commitSHA,
			ResultJSON: resultJSON,
			Artifacts:  arts,
		},
		OccurredAt: time.Now().UTC(),
	})
	require.NoError(t, err)
}

func TestOperatorActor_IndexAndRecoverActiveRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	var woken []string
	op := run.NewOperatorActor(run.OperatorOptions{
		RunReconciler: &funcReconciler{fn: func(_ context.Context, runID string, _ *run.ReconcileReq) (*run.ReconcileResp, error) {
			mu.Lock()
			woken = append(woken, runID)
			mu.Unlock()
			return &run.ReconcileResp{RunID: runID}, nil
		}},
	})
	op.SetID(run.GlobalOperatorActorID)
	op.SetStateManager(newMemoryStateManager())

	now := time.Now().UTC()
	for _, item := range []run.RunSummary{
		{RunID: "run-running", Namespace: "prod", Status: run.RunStatusRunning, CreatedAt: now.Add(-4 * time.Minute)},
		{RunID: "run-pausing", Namespace: "prod", Status: run.RunStatusPausing, CreatedAt: now.Add(-3 * time.Minute)},
		{RunID: "run-paused", Namespace: "prod", Status: run.RunStatusPaused, CreatedAt: now.Add(-2 * time.Minute)},
		{RunID: "run-done", Namespace: "dev", Status: run.RunStatusSucceeded, CreatedAt: now.Add(-1 * time.Minute)},
		{RunID: "run-failed", Namespace: "dev", Status: run.RunStatusFailed, CreatedAt: now},
	} {
		_, err := op.UpsertRun(ctx, &run.UpsertRunReq{Summary: item})
		require.NoError(t, err)
	}

	got, err := op.GetRunSummary(ctx, &run.GetRunSummaryReq{RunID: "run-running"})
	require.NoError(t, err)
	assert.True(t, got.Found)
	assert.Equal(t, run.RunStatusRunning, got.Summary.Status)

	prodList, err := op.ListRuns(ctx, &run.ListRunsReq{Namespace: "prod"})
	require.NoError(t, err)
	require.Len(t, prodList.Runs, 3)

	recResp, err := op.RecoverActiveRuns(ctx, &run.RecoverActiveRunsReq{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"run-running", "run-pausing"}, recResp.RecoveredRunIDs)
	assert.ElementsMatch(t, []string{"run-running", "run-pausing"}, woken)

	_, err = op.RemoveRun(ctx, &run.RemoveRunReq{RunID: "run-paused"})
	require.NoError(t, err)
	missing, err := op.GetRunSummary(ctx, &run.GetRunSummaryReq{RunID: "run-paused"})
	require.NoError(t, err)
	assert.False(t, missing.Found)
}

func TestRunActor_StaticToDynamicWithModuleParallelismStreamingMergeAndOutputs(t *testing.T) {
	t.Parallel()
	h := newRunHarness(t, "run-stream-001")

	meta, _ := buildAndStoreSofaPlan(t, h.ctx, h.blobStore, h.runID, map[string]string{
		"main.sofa.hcl": `
variable "secret_tok" {
  type      = string
  sensitive = true
  default   = "tok-super-secret"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

go_tool "discover_checks" {
  description = "Discover checks"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Checks map[string]string ` + "`json:\"checks\"`" + `
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "discover" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Discover"
  terminate_tool_id = go_tool.discover_checks.id
}

module "verify_mod" {
  source      = "./modules/verify"
  parallelism = 2
  base_branch = worker.discover.work_branch
  tasks_json  = worker.discover.result
}

preserve "final" {
  branch = module.verify_mod.merged_branch
}

output "final_branch" {
  value = module.verify_mod.merged_branch
}

output "secret_out" {
  value     = var.secret_tok
  sensitive = true
}
`,
		"modules/verify/main.sofa.hcl": `
variable "base_branch" {
  type = string
}

variable "tasks_json" {
  type = string
}

dynamic_worker "checks" {
  dynamic "worker" {
    for_each = jsondecodewithtype(var.tasks_json, object({ checks = map(string) })).checks
    iterator = item
    content {
      based_source  = var.base_branch
      model         = "gpt-5"
      system_prompt = "Check ${item.key}"
      prompt        = "Run ${item.value}"
    }
  }
}

merge "integrate" {
  based_source = var.base_branch
  sources      = dynamic_worker.checks.work_branches
}

output "merged_branch" {
  value = merge.integrate.work_branch
}
`,
	})

	startResp, err := h.runActor.StartRun(h.ctx, &run.StartRunReq{
		RunID:       h.runID,
		Namespace:   "default",
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
	})
	require.NoError(t, err)
	assert.True(t, startResp.Accepted)
	assert.Equal(t, run.RunStatusRunning, startResp.Status)

	// Only worker.discover should be dispatched initially.
	discRec := h.nodeCtrl.snapshot(h.runID, "worker.discover")
	assert.Equal(t, node.StatusRunning, discRec.status)
	assert.Equal(t, shaBase, discRec.lastReq.BaseSHA)

	// Complete worker.discover -> module.verify_mod binds inputs and expands dynamic_worker.checks into 3 keys (k1, k2, k3).
	// Because module.verify_mod has parallelism = 2, ONLY 2 instances (k1, k2) may dispatch initially!
	discBranch := git.WorkBranchName(h.runID, "worker.discover")
	h.completeNode(t, "worker.discover", discBranch, shaW1, []byte(`{"checks":{"k1":"one","k2":"two","k3":"three"}}`))

	k1Addr := "module.verify_mod.dynamic_worker.checks[k1]"
	k2Addr := "module.verify_mod.dynamic_worker.checks[k2]"
	k3Addr := "module.verify_mod.dynamic_worker.checks[k3]"
	assert.Equal(t, node.StatusRunning, h.nodeCtrl.snapshot(h.runID, k1Addr).status)
	assert.Equal(t, node.StatusRunning, h.nodeCtrl.snapshot(h.runID, k2Addr).status)
	assert.Equal(t, node.StatusPending, h.nodeCtrl.snapshot(h.runID, k3Addr).status, "module.parallelism = 2 must hold k3 PENDING")

	// Simulate cold restart before k1 finishes! Verify persisted DynamicKeys survive cold re-hydration without re-evaluation.
	h.rebuildColdRunActor(t)

	// Complete ONLY k1 -> streaming merge MUST enqueue k1 immediately BEFORE k2/k3 complete, AND k3 must now dispatch!
	k1Branch := git.WorkBranchName(h.runID, k1Addr)
	h.completeNode(t, k1Addr, k1Branch, shaW1, nil)

	enq := h.mergeCtrl.enqueuedSnapshots()
	require.Len(t, enq, 1, "k1 must stream into MergeActor immediately before k2/k3 finish")
	assert.Equal(t, k1Addr, enq[0].SourceNodeAddr)
	assert.Equal(t, k1Branch, enq[0].CandidateBranch)
	assert.Equal(t, node.StatusRunning, h.nodeCtrl.snapshot(h.runID, k3Addr).status, "slot freed by k1 must dispatch k3")

	// Complete k2 and k3 -> both stream into MergeActor.
	k2Branch := git.WorkBranchName(h.runID, k2Addr)
	k3Branch := git.WorkBranchName(h.runID, k3Addr)
	h.completeNode(t, k2Addr, k2Branch, shaW2, nil)
	h.completeNode(t, k3Addr, k3Branch, shaW3, nil)

	enq = h.mergeCtrl.enqueuedSnapshots()
	require.Len(t, enq, 3)

	// Deliver MergeActor results for k1, k2, k3.
	mergeCanon := "module.verify_mod.merge.integrate"
	mergeBranch := git.WorkBranchName(h.runID, mergeCanon)
	for _, item := range []struct {
		src, cand string
	}{
		{k1Addr, k1Branch},
		{k2Addr, k2Branch},
		{k3Addr, k3Branch},
	} {
		_, mErr := h.runActor.OnMergeCandidateResult(h.ctx, &merge.MergeCandidateNotification{
			RunID:              h.runID,
			MergeNodeAddr:      mergeCanon,
			MergeEpoch:         1,
			SourceNodeAddr:     item.src,
			SourceEpoch:        1,
			CandidateBranch:    item.cand,
			TargetBranch:       mergeBranch,
			SharedTargetBranch: false,
			Result: runner.MergeCandidateResult{
				Status:          runner.MergeStatusSucceeded,
				TargetBranch:    mergeBranch,
				LatestTargetSHA: shaMerge,
				MergedCommitSHA: shaMerge,
			},
			OccurredAt: time.Now().UTC(),
		})
		require.NoError(t, mErr)
	}

	// Verify Run reached SUCCEEDED and top-level outputs (including Sensitive metadata) are evaluated.
	stateResp, err := h.runClient.GetRunState(h.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusSucceeded, stateResp.Status)

	finalOut, ok := stateResp.Outputs["final_branch"]
	require.True(t, ok)
	assert.False(t, finalOut.Sensitive)
	val, err := finalOut.DecodeValue()
	require.NoError(t, err)
	assert.Equal(t, mergeBranch, val.AsString())

	secretOut, ok := stateResp.Outputs["secret_out"]
	require.True(t, ok)
	assert.True(t, secretOut.Sensitive)
	assert.True(t, secretOut.HasMarks)
	secVal, err := secretOut.DecodeValue()
	require.NoError(t, err)
	assert.True(t, corespec.IsSensitive(secVal))
	unmarkedSec, _ := secVal.UnmarkDeep()
	assert.Equal(t, "tok-super-secret", unmarkedSec.AsString())

	// Deferred cleanup iron law: SUCCEEDED run deletes 0 branches and 0 blobs!
	assert.Empty(t, h.gitAdapter.deleted())
	_, err = h.blobStore.Stat(h.ctx, meta.Key)
	require.NoError(t, err)
}

func TestRunActor_FallbackCascadeAndMaxCyclesExceeded(t *testing.T) {
	t.Parallel()
	h := newRunHarness(t, "run-fallback-001")

	meta, _ := buildAndStoreSofaPlan(t, h.ctx, h.blobStore, h.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "step1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "Step 1"
}

worker "step2" {
  based_source  = worker.step1.work_branch
  model         = "gpt-5"
  system_prompt = "Step 2"
}

worker "step3" {
  based_source  = worker.step2.work_branch
  model         = "gpt-5"
  system_prompt = "Step 3"

  artifact "diag" {
    type        = "file"
    path        = "diag.txt"
    description = "diag"
  }

  fallback "rework_step1" {
    on          = ["qc_exhausted"]
    target      = worker.step1
    max_reworks = 1
    artifacts   = [artifact("diag")]
  }
}
`,
	})

	_, err := h.runActor.StartRun(h.ctx, &run.StartRunReq{
		RunID:       h.runID,
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
	})
	require.NoError(t, err)

	b1 := git.WorkBranchName(h.runID, "worker.step1")
	b2 := git.WorkBranchName(h.runID, "worker.step2")
	h.completeNode(t, "worker.step1", b1, shaW1, nil)
	h.completeNode(t, "worker.step2", b2, shaW2, nil)
	assert.Equal(t, node.StatusRunning, h.nodeCtrl.snapshot(h.runID, "worker.step3").status)

	// First fallback from step3 -> step1 (within max_reworks = 1):
	// step1 must AdvanceForRework to epoch=2 with FallbackArtifacts; step2 & step3 must ResetForRerun to epoch=2.
	fbArt := artifact.PublishedArtifact{ID: "art_diag_1", Name: "diag", Type: "file", RelPath: "diag.txt"}
	_, err = h.runActor.OnNodeEvent(h.ctx, &node.NodeEventNotification{
		RunID:    h.runID,
		NodeAddr: "worker.step3",
		Epoch:    1,
		Status:   node.StatusFallbackTriggered,
		Outcome: &runner.NodeOutcome{
			Status:            runner.StatusFallbackTriggered,
			FallbackName:      "rework_step1",
			FallbackTarget:    "worker.step1",
			FallbackReason:    "qc_exhausted",
			FallbackArtifacts: []artifact.PublishedArtifact{fbArt},
		},
	})
	require.NoError(t, err)

	s1 := h.nodeCtrl.snapshot(h.runID, "worker.step1")
	s2 := h.nodeCtrl.snapshot(h.runID, "worker.step2")
	s3 := h.nodeCtrl.snapshot(h.runID, "worker.step3")
	assert.Equal(t, uint64(2), s1.epoch)
	assert.Equal(t, node.StatusRunning, s1.status)
	require.Len(t, s1.advanceCalls, 1)
	assert.Equal(t, "qc_exhausted", s1.advanceCalls[0].Reason)
	assert.Len(t, s1.advanceCalls[0].FallbackArtifacts, 1)

	assert.Equal(t, uint64(2), s2.epoch)
	assert.Equal(t, node.StatusPending, s2.status)
	require.Len(t, s2.resetCalls, 1)

	assert.Equal(t, uint64(2), s3.epoch)
	assert.Equal(t, node.StatusPending, s3.status)
	require.Len(t, s3.resetCalls, 1)

	// Re-complete step1 & step2 at epoch=2, then trigger fallback a SECOND time -> exceeds max_reworks=1 -> Run FAILED!
	h.completeNode(t, "worker.step1", b1, shaW1, nil)
	h.completeNode(t, "worker.step2", b2, shaW2, nil)
	_, err = h.runActor.OnNodeEvent(h.ctx, &node.NodeEventNotification{
		RunID:    h.runID,
		NodeAddr: "worker.step3",
		Epoch:    2,
		Status:   node.StatusFallbackTriggered,
		Outcome: &runner.NodeOutcome{
			Status:         runner.StatusFallbackTriggered,
			FallbackName:   "rework_step1",
			FallbackTarget: "worker.step1",
			FallbackReason: "qc_exhausted",
		},
	})
	require.NoError(t, err)

	st, err := h.runClient.GetRunState(h.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusFailed, st.Status)
	assert.Contains(t, st.ErrorMessage, "max_reworks")
}

func TestRunActor_MergeReworkSourceSingleInstanceIsolation(t *testing.T) {
	t.Parallel()
	h := newRunHarness(t, "run-merge-rework-001")

	meta, _ := buildAndStoreSofaPlan(t, h.ctx, h.blobStore, h.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

dynamic_worker "tasks" {
  dynamic "worker" {
    for_each = toset(["a", "b"])
    content {
      based_source  = source.project.branch
      model         = "gpt-5"
      system_prompt = "Do ${worker.key}"
    }
  }
}

merge "integrate" {
  based_source = source.project.branch
  sources      = dynamic_worker.tasks.work_branches
  max_reworks  = 2
}
`,
	})

	_, err := h.runActor.StartRun(h.ctx, &run.StartRunReq{
		RunID:       h.runID,
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
	})
	require.NoError(t, err)

	aAddr := "dynamic_worker.tasks[a]"
	bAddr := "dynamic_worker.tasks[b]"
	aBranch := git.WorkBranchName(h.runID, aAddr)
	bBranch := git.WorkBranchName(h.runID, bAddr)
	h.completeNode(t, aAddr, aBranch, shaW1, nil)
	h.completeNode(t, bAddr, bBranch, shaW2, nil)

	mergeBranch := git.WorkBranchName(h.runID, "merge.integrate")

	// Sibling "a" merges cleanly.
	_, err = h.runActor.OnMergeCandidateResult(h.ctx, &merge.MergeCandidateNotification{
		RunID:          h.runID,
		MergeNodeAddr:  "merge.integrate",
		MergeEpoch:     1,
		SourceNodeAddr: aAddr,
		SourceEpoch:    1,
		TargetBranch:   mergeBranch,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    mergeBranch,
			LatestTargetSHA: shaMerge,
			MergedCommitSHA: shaMerge,
		},
	})
	require.NoError(t, err)

	// Sibling "b" hits conflict -> ONLY "b" must advance to epoch=2 with BaseSHA = shaMerge; "a" stays untouched at epoch=1!
	_, err = h.runActor.OnMergeCandidateResult(h.ctx, &merge.MergeCandidateNotification{
		RunID:          h.runID,
		MergeNodeAddr:  "merge.integrate",
		MergeEpoch:     1,
		SourceNodeAddr: bAddr,
		SourceEpoch:    1,
		TargetBranch:   mergeBranch,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusReworkSource,
			TargetBranch:    mergeBranch,
			LatestTargetSHA: shaMerge,
			FailureReport:   "rebase conflict in shared.txt",
		},
	})
	require.NoError(t, err)

	aRec := h.nodeCtrl.snapshot(h.runID, aAddr)
	bRec := h.nodeCtrl.snapshot(h.runID, bAddr)
	assert.Equal(t, uint64(1), aRec.epoch)
	assert.Empty(t, aRec.advanceCalls)
	assert.Equal(t, node.StatusSucceeded, aRec.status)

	assert.Equal(t, uint64(2), bRec.epoch)
	assert.Equal(t, node.StatusRunning, bRec.status)
	require.Len(t, bRec.advanceCalls, 1)
	assert.Equal(t, shaMerge, bRec.advanceCalls[0].BaseSHA)
	assert.Equal(t, mergeBranch, bRec.advanceCalls[0].BasedSource)
	assert.Equal(t, shaMerge, bRec.lastReq.BaseSHA)

	// Complete reworked "b" at epoch=2 -> re-enqueues into MergeActor and succeeds!
	h.completeNode(t, bAddr, bBranch, shaRew, nil)
	enq := h.mergeCtrl.enqueuedSnapshots()
	require.Len(t, enq, 3)
	assert.Equal(t, bAddr, enq[2].SourceNodeAddr)
	assert.Equal(t, uint64(2), enq[2].SourceEpoch)

	_, err = h.runActor.OnMergeCandidateResult(h.ctx, &merge.MergeCandidateNotification{
		RunID:          h.runID,
		MergeNodeAddr:  "merge.integrate",
		MergeEpoch:     1,
		SourceNodeAddr: bAddr,
		SourceEpoch:    2,
		TargetBranch:   mergeBranch,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    mergeBranch,
			LatestTargetSHA: shaRew,
			MergedCommitSHA: shaRew,
		},
	})
	require.NoError(t, err)

	st, err := h.runClient.GetRunState(h.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusSucceeded, st.Status)
}

func TestRunActor_RerunSharedBranchConflictVsExclusiveCascade(t *testing.T) {
	t.Parallel()

	// Part 1: Explicit shared target_branch -> Rerun rejected with ErrAlreadyMergedSharedBranch (409).
	hShared := newRunHarness(t, "run-rerun-shared")
	metaShared, _ := buildAndStoreSofaPlan(t, hShared.ctx, hShared.blobStore, hShared.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "w1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "W1"
}

merge "shared_int" {
  based_source  = source.project.branch
  target_branch = "integration/shared"
  sources       = [worker.w1.work_branch]
}
`,
	})
	_, err := hShared.runActor.StartRun(hShared.ctx, &run.StartRunReq{
		RunID:       hShared.runID,
		PlanBlobKey: metaShared.Key,
		PlanSHA256:  metaShared.SHA256,
	})
	require.NoError(t, err)

	w1Branch := git.WorkBranchName(hShared.runID, "worker.w1")
	hShared.completeNode(t, "worker.w1", w1Branch, shaW1, nil)
	_, err = hShared.runActor.OnMergeCandidateResult(hShared.ctx, &merge.MergeCandidateNotification{
		RunID:                  hShared.runID,
		MergeNodeAddr:          "merge.shared_int",
		MergeEpoch:             1,
		SourceNodeAddr:         "worker.w1",
		SourceEpoch:            1,
		TargetBranch:           "integration/shared",
		SharedTargetBranch:     true,
		MergedIntoSharedBranch: true,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    "integration/shared",
			LatestTargetSHA: shaMerge,
			MergedCommitSHA: shaMerge,
		},
	})
	require.NoError(t, err)

	rerunResp, rerunErr := hShared.runActor.RerunNode(hShared.ctx, &run.RerunNodeReq{NodeAddr: "worker.w1"})
	require.NoError(t, rerunErr, "actor method returns lossless error metadata on RerunNodeResp")
	assert.False(t, rerunResp.Accepted)
	assert.True(t, rerunResp.AlreadyMergedSharedBranch)
	assert.Equal(t, run.ErrCodeAlreadyMergedSharedBranch, rerunResp.ErrorCode)
	require.ErrorIs(t, run.DecodeRerunNodeError(rerunResp, rerunErr), run.ErrAlreadyMergedSharedBranch)

	_, clientErr := hShared.runClient.RerunNode(hShared.ctx, &run.RerunNodeReq{NodeAddr: "worker.w1"})
	require.ErrorIs(t, clientErr, run.ErrAlreadyMergedSharedBranch)

	// Part 2: Run-exclusive merge -> Rerun on worker.w1 cancels merge, increments MergeEpoch & worker epoch, and succeeds.
	hExcl := newRunHarness(t, "run-rerun-excl")
	metaExcl, _ := buildAndStoreSofaPlan(t, hExcl.ctx, hExcl.blobStore, hExcl.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "w1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "W1"
}

merge "local_int" {
  based_source = source.project.branch
  sources      = [worker.w1.work_branch]
}
`,
	})
	_, err = hExcl.runActor.StartRun(hExcl.ctx, &run.StartRunReq{
		RunID:       hExcl.runID,
		PlanBlobKey: metaExcl.Key,
		PlanSHA256:  metaExcl.SHA256,
	})
	require.NoError(t, err)

	exclW1Branch := git.WorkBranchName(hExcl.runID, "worker.w1")
	exclMergeBranch := git.WorkBranchName(hExcl.runID, "merge.local_int")
	hExcl.completeNode(t, "worker.w1", exclW1Branch, shaW1, nil)
	_, err = hExcl.runActor.OnMergeCandidateResult(hExcl.ctx, &merge.MergeCandidateNotification{
		RunID:          hExcl.runID,
		MergeNodeAddr:  "merge.local_int",
		MergeEpoch:     1,
		SourceNodeAddr: "worker.w1",
		SourceEpoch:    1,
		TargetBranch:   exclMergeBranch,
		Result: runner.MergeCandidateResult{
			Status:          runner.MergeStatusSucceeded,
			TargetBranch:    exclMergeBranch,
			LatestTargetSHA: shaMerge,
			MergedCommitSHA: shaMerge,
		},
	})
	require.NoError(t, err)

	okResp, err := hExcl.runClient.RerunNode(hExcl.ctx, &run.RerunNodeReq{NodeAddr: "worker.w1"})
	require.NoError(t, err)
	assert.True(t, okResp.Accepted)
	assert.Equal(t, uint64(2), hExcl.nodeCtrl.snapshot(hExcl.runID, "worker.w1").epoch)
	assert.NotEmpty(t, hExcl.mergeCtrl.cancelSnapshots())

	hExcl.completeNode(t, "worker.w1", exclW1Branch, shaRew, nil)
	enq := hExcl.mergeCtrl.enqueuedSnapshots()
	require.Len(t, enq, 2)
	assert.Equal(t, uint64(2), enq[1].MergeEpoch)
	assert.Equal(t, uint64(2), enq[1].SourceEpoch)
}

func TestRunActor_PausePlannedImmediateResumeAndDeferredDeleteCleanup(t *testing.T) {
	t.Parallel()
	h := newRunHarness(t, "run-pause-del-001")

	meta, _ := buildAndStoreSofaPlan(t, h.ctx, h.blobStore, h.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "w1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "W1"
}

worker "w2" {
  based_source  = worker.w1.work_branch
  model         = "gpt-5"
  system_prompt = "W2"
}

preserve "keep_w1" {
  branch = worker.w1.work_branch
}
`,
	})
	_, err := h.runActor.StartRun(h.ctx, &run.StartRunReq{
		RunID:       h.runID,
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
	})
	require.NoError(t, err)

	// Planned pause while w1 is RUNNING -> Run enters PAUSING; w1 completes -> Run enters PAUSED without starting w2!
	pResp, err := h.runClient.PauseRun(h.ctx, &run.PauseRunReq{Mode: "planned"})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusPausing, pResp.Status)

	w1Branch := git.WorkBranchName(h.runID, "worker.w1")
	ckpt1 := git.CheckpointBranchName(h.runID, "worker.w1", 1)
	// Register branches in RunActor ledger just like NodeActor does in production.
	_, err = h.runActor.RegisterManagedBranch(h.ctx, &run.RegisterManagedBranchReq{
		RunID: h.runID, NodeAddr: "worker.w1", Epoch: 1,
		RepoURL: "https://github.com/example/sofa.git", Branch: ckpt1, BaseSHA: shaBase, Kind: git.KindCheckpointBranch,
	})
	require.NoError(t, err)
	_, err = h.runActor.RegisterManagedBranch(h.ctx, &run.RegisterManagedBranchReq{
		RunID: h.runID, NodeAddr: "worker.w1", Epoch: 1,
		RepoURL: "https://github.com/example/sofa.git", Branch: w1Branch, BaseSHA: shaBase, Kind: git.KindWorkBranch,
	})
	require.NoError(t, err)

	h.completeNode(t, "worker.w1", w1Branch, shaW1, nil)
	st, err := h.runClient.GetRunState(h.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusPaused, st.Status)
	assert.Equal(t, node.StatusPending, h.nodeCtrl.snapshot(h.runID, "worker.w2").status)

	// ResumeRun -> dispatches w2; then Pause("immediate") -> aborts w2 & increments epoch -> ResumeRun -> w2 finishes -> SUCCEEDED!
	_, err = h.runClient.ResumeRun(h.ctx, &run.ResumeRunReq{})
	require.NoError(t, err)
	assert.Equal(t, node.StatusRunning, h.nodeCtrl.snapshot(h.runID, "worker.w2").status)

	immResp, err := h.runClient.PauseRun(h.ctx, &run.PauseRunReq{Mode: "immediate"})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusPaused, immResp.Status)
	assert.Equal(t, uint64(2), h.nodeCtrl.snapshot(h.runID, "worker.w2").epoch)

	_, err = h.runClient.ResumeRun(h.ctx, &run.ResumeRunReq{})
	require.NoError(t, err)

	w2Branch := git.WorkBranchName(h.runID, "worker.w2")
	ckpt2 := git.CheckpointBranchName(h.runID, "worker.w2", 3)
	_, err = h.runActor.RegisterManagedBranch(h.ctx, &run.RegisterManagedBranchReq{
		RunID: h.runID, NodeAddr: "worker.w2", Epoch: 3,
		RepoURL: "https://github.com/example/sofa.git", Branch: ckpt2, BaseSHA: shaW1, Kind: git.KindCheckpointBranch,
	})
	require.NoError(t, err)
	_, err = h.runActor.RegisterManagedBranch(h.ctx, &run.RegisterManagedBranchReq{
		RunID: h.runID, NodeAddr: "worker.w2", Epoch: 3,
		RepoURL: "https://github.com/example/sofa.git", Branch: w2Branch, BaseSHA: shaW1, Kind: git.KindWorkBranch,
	})
	require.NoError(t, err)

	h.completeNode(t, "worker.w2", w2Branch, shaW2, nil)
	st, err = h.runClient.GetRunState(h.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusSucceeded, st.Status)

	// Zero deletions while SUCCEEDED!
	assert.Empty(t, h.gitAdapter.deleted())
	_, err = h.blobStore.Stat(h.ctx, meta.Key)
	require.NoError(t, err)

	// DeleteRun -> deletes ckpt1, ckpt2, w2Branch while PRESERVING main and keep_w1 (w1Branch),
	// deletes runs/<run_id>/ blobs, deletes ConfigActor run scope, and removes from OperatorActor!
	delResp, err := h.runClient.DeleteRun(h.ctx, &run.DeleteRunReq{})
	require.NoError(t, err)
	assert.True(t, delResp.Deleted)

	deletedBranches := make([]string, 0, len(h.gitAdapter.deleted()))
	for _, b := range h.gitAdapter.deleted() {
		deletedBranches = append(deletedBranches, b.Branch)
	}
	assert.NotContains(t, deletedBranches, "main")
	assert.NotContains(t, deletedBranches, w1Branch, "preserve block must protect worker.w1.work_branch")
	assert.ElementsMatch(t, []string{ckpt1, ckpt2, w2Branch}, deletedBranches)

	_, statErr := h.blobStore.Stat(h.ctx, meta.Key)
	assert.ErrorIs(t, statErr, blob.ErrNotFound)

	assert.Contains(t, h.cfgMgr.deletedRuns, h.runID)
	opSummary, err := h.opActor.GetRunSummary(h.ctx, &run.GetRunSummaryReq{RunID: h.runID})
	require.NoError(t, err)
	assert.False(t, opSummary.Found)
}

func TestRunActor_EmptyDynamicZeroCandidateMergeMaxReworksAndDecisions(t *testing.T) {
	t.Parallel()

	// Part 1: Empty dynamic_worker -> 0-candidate merge immediately SUCCEEDS without stalling.
	hEmpty := newRunHarness(t, "run-empty-dyn")
	metaEmpty, _ := buildAndStoreSofaPlan(t, hEmpty.ctx, hEmpty.blobStore, hEmpty.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

dynamic_worker "checks" {
  dynamic "worker" {
    for_each = tomap({})
    content {
      based_source  = source.project.branch
      model         = "gpt-5"
      system_prompt = "Run ${worker.key}"
    }
  }
}

merge "integrate" {
  based_source = source.project.branch
  sources      = dynamic_worker.checks.work_branches
}

output "merged" {
  value = merge.integrate.work_branch
}
`,
	})
	startResp, err := hEmpty.runClient.StartRun(hEmpty.ctx, &run.StartRunReq{
		RunID:       hEmpty.runID,
		PlanBlobKey: metaEmpty.Key,
		PlanSHA256:  metaEmpty.SHA256,
	})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusSucceeded, startResp.Status)
	assert.Empty(t, hEmpty.mergeCtrl.enqueuedSnapshots())

	stEmpty, err := hEmpty.runClient.GetRunState(hEmpty.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	outVal, err := stEmpty.Outputs["merged"].DecodeValue()
	require.NoError(t, err)
	assert.Equal(t, git.WorkBranchName(hEmpty.runID, "merge.integrate"), outVal.AsString())

	// Part 2: RegisterManagedBranch protects source branch, WAITING_HUMAN + ListDecisions,
	// and merge max_reworks exhaustion transitions Run to FAILED.
	hDec := newRunHarness(t, "run-dec-maxrew")
	metaDec, _ := buildAndStoreSofaPlan(t, hDec.ctx, hDec.blobStore, hDec.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "w1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "W1"
}

merge "integrate" {
  based_source = source.project.branch
  sources      = [worker.w1.work_branch]
  max_reworks  = 0
}
`,
	})
	_, err = hDec.runClient.StartRun(hDec.ctx, &run.StartRunReq{
		RunID:       hDec.runID,
		PlanBlobKey: metaDec.Key,
		PlanSHA256:  metaDec.SHA256,
	})
	require.NoError(t, err)

	// Attempting to register protected source branch "main" returns lossless ErrProtectedSourceBranch.
	regResp, regErr := hDec.runActor.RegisterManagedBranch(hDec.ctx, &run.RegisterManagedBranchReq{
		RunID: hDec.runID, NodeAddr: "worker.w1", Epoch: 1,
		RepoURL: "https://github.com/example/sofa.git", Branch: "main", BaseSHA: shaBase, Kind: git.KindWorkBranch,
	})
	require.NoError(t, regErr)
	require.ErrorIs(t, run.DecodeRegisterManagedBranchError(regResp, regErr), git.ErrProtectedSourceBranch)

	// Simulate WAITING_HUMAN + ListDecisions.
	hDec.nodeCtrl.mu.Lock()
	hDec.nodeCtrl.getOrInit(hDec.runID, "worker.w1").status = node.StatusWaitingHuman
	hDec.nodeCtrl.getOrInit(hDec.runID, "worker.w1").cards = []node.DecisionCard{
		{CardID: "card-1", RunID: hDec.runID, NodeAddr: "worker.w1", Epoch: 1, Status: node.DecisionStatusPending, Question: "Proceed?"},
	}
	hDec.nodeCtrl.mu.Unlock()
	_, err = hDec.runActor.OnNodeEvent(hDec.ctx, &node.NodeEventNotification{
		RunID: hDec.runID, NodeAddr: "worker.w1", Epoch: 1, Status: node.StatusWaitingHuman,
	})
	require.NoError(t, err)

	stDec, err := hDec.runClient.GetRunState(hDec.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusWaitingHuman, stDec.Status)

	decs, err := hDec.runClient.ListDecisions(hDec.ctx, &run.ListDecisionsReq{})
	require.NoError(t, err)
	require.Len(t, decs.Decisions, 1)
	assert.Equal(t, "card-1", decs.Decisions[0].CardID)

	// Complete w1 and trigger MergeStatusReworkSource with max_reworks=0 -> Run FAILED!
	w1Branch := git.WorkBranchName(hDec.runID, "worker.w1")
	hDec.completeNode(t, "worker.w1", w1Branch, shaW1, nil)
	_, err = hDec.runActor.OnMergeCandidateResult(hDec.ctx, &merge.MergeCandidateNotification{
		RunID:          hDec.runID,
		MergeNodeAddr:  "merge.integrate",
		MergeEpoch:     1,
		SourceNodeAddr: "worker.w1",
		SourceEpoch:    1,
		Result: runner.MergeCandidateResult{
			Status:        runner.MergeStatusReworkSource,
			FailureReport: "conflict",
		},
	})
	require.NoError(t, err)
	stFailed, err := hDec.runClient.GetRunState(hDec.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusFailed, stFailed.Status)
	assert.Contains(t, stFailed.ErrorMessage, "max_reworks")
}

type syncFailNodeController struct {
	*fakeNodeController
	failAddr string
	failMsg  string
}

func (s *syncFailNodeController) StartOrResume(_ context.Context, _ string, canonicalNodeAddr string, _ *node.StartOrResumeReq) (*node.StartOrResumeResp, error) {
	if canonicalNodeAddr == s.failAddr {
		return &node.StartOrResumeResp{
			Accepted:     false,
			Epoch:        1,
			Status:       node.StatusFailed,
			ErrorMessage: s.failMsg,
		}, nil
	}
	return &node.StartOrResumeResp{Accepted: true, Epoch: 1, Status: node.StatusRunning}, nil
}

type fakeProcessLister struct {
	mu        sync.Mutex
	processes []registry.ProcessRecord
	err       error
	calls     int
}

func (f *fakeProcessLister) ListProcesses(_ context.Context, _ *registry.ListProcessesReq) (*registry.ListProcessesResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &registry.ListProcessesResp{
		Processes: slices.Clone(f.processes),
	}, nil
}

type cleanupCall struct {
	AppID string
	Req   daemon.CleanupRunRequest
}

type fakeWorkerCleaner struct {
	mu     sync.Mutex
	calls  []cleanupCall
	onCall func(ctx context.Context, appID string, req daemon.CleanupRunRequest) (daemon.CleanupRunResponse, error)
}

func (f *fakeWorkerCleaner) CleanupRun(ctx context.Context, appID string, req daemon.CleanupRunRequest) (daemon.CleanupRunResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cleanupCall{AppID: appID, Req: req})
	fn := f.onCall
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, appID, req)
	}
	return daemon.CleanupRunResponse{Removed: true}, nil
}

func (f *fakeWorkerCleaner) snapshotCalls() []cleanupCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func TestRunActor_DispatchDecoupledAndDeleteRunCleansWorkerBundles(t *testing.T) {
	t.Parallel()
	h := newRunHarness(t, "run-decouple-del-001")

	procLister := &fakeProcessLister{
		processes: []registry.ProcessRecord{
			{ProcessID: "p-healthy-1", AppID: "sofa-worker-1", Status: registry.StatusHealthy},
			{ProcessID: "p-healthy-1b", AppID: "sofa-worker-1", Status: registry.StatusHealthy}, // duplicate AppID
			{ProcessID: "p-suspect-2", AppID: "sofa-worker-2", Status: registry.StatusSuspect},
			{ProcessID: "p-dead-3", AppID: "sofa-worker-3", Status: registry.StatusDead},
			{ProcessID: "p-hung-4", AppID: "sofa-worker-hung", Status: registry.StatusHealthy},
		},
	}
	hungRelease := make(chan struct{})
	defer close(hungRelease)
	workerCleaner := &fakeWorkerCleaner{
		onCall: func(ctx context.Context, appID string, _ daemon.CleanupRunRequest) (daemon.CleanupRunResponse, error) {
			if appID == "sofa-worker-hung" {
				select {
				case <-ctx.Done():
					return daemon.CleanupRunResponse{}, ctx.Err()
				case <-hungRelease:
				}
			}
			if appID == "sofa-worker-2" {
				return daemon.CleanupRunResponse{}, errors.New("suspect worker unreachable")
			}
			return daemon.CleanupRunResponse{Removed: true}, nil
		},
	}

	cpBundleRoot := filepath.Join(t.TempDir(), "cp-bundles")
	h.runActor = run.NewRunActor(run.RunOptions{
		BlobStore:            h.blobStore,
		BundleCacheRoot:      cpBundleRoot,
		NodeController:       h.nodeCtrl,
		MergeController:      h.mergeCtrl,
		OperatorIndex:        &operatorAdapter{op: h.opActor},
		ConfigManager:        h.cfgMgr,
		EventPublisher:       h.events,
		ReminderScheduler:    h.remSched,
		GitAdapter:           h.gitAdapter,
		ProcessLister:        procLister,
		WorkerCleaner:        workerCleaner,
		WorkerCleanupTimeout: 80 * time.Millisecond,
	})
	h.runActor.SetID(h.runID)
	h.runActor.SetStateManager(h.runSM)
	h.runClient = run.NewDirectRunClient(h.runActor)

	meta, _ := buildAndStoreSofaPlan(t, h.ctx, h.blobStore, h.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "w1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "W1"
}

module "sub" {
  source      = "./modules/sub"
  base_branch = worker.w1.work_branch
}

merge "final" {
  based_source = source.project.branch
  sources      = [worker.w1.work_branch]
}
`,
		"modules/sub/main.sofa.hcl": `
variable "base_branch" {
  type = string
}

worker "child" {
  based_source  = var.base_branch
  model         = "gpt-5"
  system_prompt = "Child"
}

merge "sub_merge" {
  based_source = var.base_branch
  sources      = [worker.child.work_branch]
}

output "child_branch" {
  value = merge.sub_merge.work_branch
}
`,
	})

	startResp, err := h.runClient.StartRun(h.ctx, &run.StartRunReq{
		RunID:       h.runID,
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
	})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusRunning, startResp.Status)

	w1Req := h.nodeCtrl.snapshot(h.runID, "worker.w1").lastReq
	require.NotNil(t, w1Req)
	assert.Equal(t, meta.Key, w1Req.PlanBlobKey)
	assert.Equal(t, meta.SHA256, w1Req.PlanSHA256)
	assert.Equal(t, "config", w1Req.BundlePath)

	w1Branch := git.WorkBranchName(h.runID, "worker.w1")
	h.completeNode(t, "worker.w1", w1Branch, shaW1, nil)

	childReq := h.nodeCtrl.snapshot(h.runID, "module.sub.worker.child").lastReq
	require.NotNil(t, childReq)
	assert.Equal(t, meta.Key, childReq.PlanBlobKey)
	assert.Equal(t, meta.SHA256, childReq.PlanSHA256)
	assert.Equal(t, "modules/sub", childReq.BundlePath)

	childBranch := git.WorkBranchName(h.runID, "module.sub.worker.child")
	h.completeNode(t, "module.sub.worker.child", childBranch, shaW2, nil)

	enq := h.mergeCtrl.enqueuedSnapshots()
	require.Len(t, enq, 2)
	byMerge := make(map[string]merge.EnqueueCandidateReq, len(enq))
	for _, e := range enq {
		byMerge[e.MergeNodeAddr] = e
	}
	assert.Equal(t, meta.Key, byMerge["merge.final"].PlanBlobKey)
	assert.Equal(t, meta.SHA256, byMerge["merge.final"].PlanSHA256)
	assert.Equal(t, "config", byMerge["merge.final"].BundlePath)
	assert.Equal(t, meta.Key, byMerge["module.sub.merge.sub_merge"].PlanBlobKey)
	assert.Equal(t, meta.SHA256, byMerge["module.sub.merge.sub_merge"].PlanSHA256)
	assert.Equal(t, "modules/sub", byMerge["module.sub.merge.sub_merge"].BundlePath)

	// No worker cleanup before DeleteRun.
	assert.Empty(t, workerCleaner.snapshotCalls())

	// DeleteRun invokes cleanup_run on HEALTHY/SUSPECT unique AppIDs, skips DEAD, ignores suspect errors, and bounds hung worker wait.
	delStart := time.Now()
	delResp, err := h.runClient.DeleteRun(h.ctx, &run.DeleteRunReq{})
	require.NoError(t, err)
	assert.True(t, delResp.Deleted)
	assert.Less(t, time.Since(delStart), 1*time.Second, "DeleteRun must not block indefinitely on hung workers")

	calls := workerCleaner.snapshotCalls()
	gotAppIDs := make([]string, 0, len(calls))
	for _, c := range calls {
		assert.Equal(t, h.runID, c.Req.RunID)
		gotAppIDs = append(gotAppIDs, c.AppID)
	}
	assert.ElementsMatch(t, []string{"sofa-worker-1", "sofa-worker-2", "sofa-worker-hung"}, gotAppIDs)
	assert.NotContains(t, gotAppIDs, "sofa-worker-3")

	// Control-plane extraction dir must also be removed.
	entries, err := os.ReadDir(cpBundleRoot)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRunActor_SyncStartOrResumeFailedTransitionsRunToFailed(t *testing.T) {
	t.Parallel()
	h := newRunHarness(t, "run-sync-fail-001")
	h.runActor = run.NewRunActor(run.RunOptions{
		BlobStore:         h.blobStore,
		BundleCacheRoot:   t.TempDir(),
		NodeController:    &syncFailNodeController{fakeNodeController: h.nodeCtrl, failAddr: "worker.w1", failMsg: "missing required managed env/secret keys: MISSING_SECRET"},
		MergeController:   h.mergeCtrl,
		OperatorIndex:     &operatorAdapter{op: h.opActor},
		ConfigManager:     h.cfgMgr,
		EventPublisher:    h.events,
		ReminderScheduler: h.remSched,
		GitAdapter:        h.gitAdapter,
	})
	h.runActor.SetID(h.runID)
	h.runActor.SetStateManager(h.runSM)
	h.runClient = run.NewDirectRunClient(h.runActor)

	meta, _ := buildAndStoreSofaPlan(t, h.ctx, h.blobStore, h.runID, map[string]string{
		"main.sofa.hcl": `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "w1" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "W1"
}
`,
	})

	startResp, err := h.runClient.StartRun(h.ctx, &run.StartRunReq{
		RunID:       h.runID,
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
	})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusFailed, startResp.Status)
	assert.Contains(t, startResp.ErrorMessage, "MISSING_SECRET")

	st, err := h.runClient.GetRunState(h.ctx, &run.GetRunStateReq{})
	require.NoError(t, err)
	assert.Equal(t, run.RunStatusFailed, st.Status)
	assert.Contains(t, st.ErrorMessage, "MISSING_SECRET")

	opSum, err := h.opActor.GetRunSummary(h.ctx, &run.GetRunSummaryReq{RunID: h.runID})
	require.NoError(t, err)
	require.True(t, opSum.Found)
	assert.Equal(t, run.RunStatusFailed, opSum.Summary.Status)
}

var _ = golden.SensitiveMark
var _ = cty.String
var _ = errors.Is
