package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/stretchr/testify/require"
)

const (
	testSHA0 = "0000000000000000000000000000000000000000"
	testSHA1 = "1111111111111111111111111111111111111111"
	testSHA2 = "2222222222222222222222222222222222222222"
	testSHA3 = "3333333333333333333333333333333333333333"
)

type fakeGitAdapter struct {
	mu                 sync.Mutex
	headSHA            string
	statusPaths        []git.ChangedPath
	statusErr          error
	commitSHA          string
	commitCommitted    bool
	commitErr          error
	onCommitAllAndPush func(remoteBranch string)
	branchContains     map[string]map[string]bool // branch -> sha -> contained
	prepareCalls       []prepareCall
	commitCalls        int
}

type prepareCall struct {
	repoURL     string
	fetchBranch string
	pinSHA      string
	destDir     string
}

func newFakeGitAdapter(initialHead string) *fakeGitAdapter {
	return &fakeGitAdapter{
		headSHA:        initialHead,
		branchContains: make(map[string]map[string]bool),
	}
}

func (f *fakeGitAdapter) ResolveBranchSHA(_ context.Context, _, _ string, _ git.AuthConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headSHA, nil
}

func (f *fakeGitAdapter) PrepareWorkspace(_ context.Context, repoURL, fetchBranch, pinSHA, destDir string, _ git.AuthConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls = append(f.prepareCalls, prepareCall{
		repoURL:     repoURL,
		fetchBranch: fetchBranch,
		pinSHA:      pinSHA,
		destDir:     destDir,
	})
	return os.MkdirAll(destDir, 0o755)
}

func (f *fakeGitAdapter) HeadSHA(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headSHA, nil
}

func (f *fakeGitAdapter) StatusPaths(_ context.Context, _ string) ([]git.ChangedPath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	out := make([]git.ChangedPath, len(f.statusPaths))
	copy(out, f.statusPaths)
	return out, nil
}

func (f *fakeGitAdapter) RevertPaths(_ context.Context, _ string, _ []string) error {
	return nil
}

func (f *fakeGitAdapter) CommitAllAndPush(_ context.Context, _, _, remoteBranch, _ string, _ git.AuthConfig) (string, bool, error) {
	f.mu.Lock()
	f.commitCalls++
	cb := f.onCommitAllAndPush
	err := f.commitErr
	committed := f.commitCommitted
	sha := f.commitSHA
	if sha == "" {
		sha = f.headSHA
	}
	f.mu.Unlock()

	if cb != nil {
		cb(remoteBranch)
	}
	if err != nil {
		return "", false, err
	}

	f.mu.Lock()
	if f.branchContains[remoteBranch] == nil {
		f.branchContains[remoteBranch] = make(map[string]bool)
	}
	f.branchContains[remoteBranch][sha] = true
	f.headSHA = sha
	f.mu.Unlock()

	return sha, committed, nil
}

func (f *fakeGitAdapter) PushCommit(_ context.Context, _, _, remoteBranch, commitSHA, _ string, _ git.AuthConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branchContains[remoteBranch] == nil {
		f.branchContains[remoteBranch] = make(map[string]bool)
	}
	f.branchContains[remoteBranch][commitSHA] = true
	return nil
}

func (f *fakeGitAdapter) BranchContainsSHA(_ context.Context, _, remoteBranch, targetSHA string, _ git.AuthConfig) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	shas := f.branchContains[remoteBranch]
	if shas == nil || !shas[targetSHA] {
		return "", false, nil
	}
	return targetSHA, true, nil
}

func (f *fakeGitAdapter) DeleteRemoteBranch(_ context.Context, _, _ string, _ git.AuthConfig) error {
	return nil
}

func (f *fakeGitAdapter) RebaseCandidate(_ context.Context, _, _, _, _ string, _ git.AuthConfig) (git.RebaseOutcome, error) {
	return git.RebaseOutcome{}, nil
}

type spyStateStore struct {
	inner            checkpoint.NodeStateStore
	mu               sync.Mutex
	registerCalls    int
	beginCalls       int
	appendFinalCalls int
	completeCalls    int
}

func (s *spyStateStore) RegisterCheckpointBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error {
	s.mu.Lock()
	s.registerCalls++
	s.mu.Unlock()
	return s.inner.RegisterCheckpointBranch(ctx, epoch, repoURL, branch, baseSHA)
}

func (s *spyStateStore) BeginCheckpoint(ctx context.Context, reqEpoch uint64, branch, initialSHA string, diary session.DiaryInput) (uint64, error) {
	s.mu.Lock()
	s.beginCalls++
	s.mu.Unlock()
	return s.inner.BeginCheckpoint(ctx, reqEpoch, branch, initialSHA, diary)
}

func (s *spyStateStore) AppendFinalSHA(ctx context.Context, reqEpoch, seq uint64, finalSHA string) error {
	s.mu.Lock()
	s.appendFinalCalls++
	s.mu.Unlock()
	return s.inner.AppendFinalSHA(ctx, reqEpoch, seq, finalSHA)
}

func (s *spyStateStore) CompleteCheckpoint(ctx context.Context, reqEpoch, seq uint64, finalSHA string, artifactsSnap *blob.ObjectMeta) error {
	s.mu.Lock()
	s.completeCalls++
	s.mu.Unlock()
	return s.inner.CompleteCheckpoint(ctx, reqEpoch, seq, finalSHA, artifactsSnap)
}

func (s *spyStateStore) ListCheckpoints(ctx context.Context) ([]checkpoint.CheckpointRecord, error) {
	return s.inner.ListCheckpoints(ctx)
}

func sampleDiary(tag string) session.DiaryInput {
	return session.DiaryInput{
		Completed:    []string{"completed " + tag},
		Decisions:    []string{"decision " + tag},
		TestEvidence: []string{"test " + tag},
		NextSteps:    []string{"next " + tag},
	}
}

func TestLocalNodeStateStoreEpochFencingAndPersistence(t *testing.T) {
	ctx := context.Background()
	repoURL := "https://github.com/acme/repo.git"
	nodeAddr := `worker.impl["core"]`
	ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
	statePath := filepath.Join(t.TempDir(), "state", "node.json")

	store, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
		InitialEpoch: 1,
		Ledger:       ledger,
		OwnerNode:    nodeAddr,
		StatePath:    statePath,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(1), store.CurrentEpoch())

	branchE1 := git.CheckpointBranchName("run-1", nodeAddr, 1)
	require.NoError(t, store.RegisterCheckpointBranch(ctx, 1, repoURL, branchE1, testSHA0))
	require.NoError(t, ledger.EnsureRegistered(repoURL, branchE1))

	// Repeat registration at same epoch + same baseSHA is idempotent.
	require.NoError(t, store.RegisterCheckpointBranch(ctx, 1, repoURL, branchE1, testSHA0))

	seq1, err := store.BeginCheckpoint(ctx, 1, branchE1, testSHA0, sampleDiary("seq1"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), seq1)
	require.NoError(t, store.AppendFinalSHA(ctx, 1, seq1, testSHA1))
	snap1 := &blob.ObjectMeta{
		Key:       "runs/run-1/checkpoints/node/seq-000001-e1-artifacts.zip",
		SHA256:    strings.Repeat("a", 64),
		SizeBytes: 12,
	}
	require.NoError(t, store.CompleteCheckpoint(ctx, 1, seq1, testSHA1, snap1))

	seq2, err := store.BeginCheckpoint(ctx, 1, branchE1, testSHA1, sampleDiary("seq2"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), seq2)

	// ListCheckpoints returns newest-to-oldest deep copies.
	recs, err := store.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	require.Equal(t, uint64(2), recs[0].Seq)
	require.Equal(t, checkpoint.StatusPending, recs[0].Status)
	require.Equal(t, uint64(1), recs[1].Seq)
	require.Equal(t, checkpoint.StatusComplete, recs[1].Status)

	recs[1].Diary.Completed[0] = "tampered"
	recs[1].ArtifactsSnapshot.SHA256 = "tampered"
	recsAgain, err := store.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Equal(t, "completed seq1", recsAgain[1].Diary.Completed[0])
	require.Equal(t, strings.Repeat("a", 64), recsAgain[1].ArtifactsSnapshot.SHA256)

	// AdvanceEpoch enforces strict monotonicity and fences all epoch-1 writes.
	require.Error(t, store.AdvanceEpoch(1))
	require.NoError(t, store.AdvanceEpoch(2))
	require.Equal(t, uint64(2), store.CurrentEpoch())

	branchE2 := git.CheckpointBranchName("run-1", nodeAddr, 2)
	require.ErrorIs(t, store.RegisterCheckpointBranch(ctx, 1, repoURL, branchE2, testSHA1), checkpoint.ErrStaleEpoch)
	_, err = store.BeginCheckpoint(ctx, 1, branchE1, testSHA1, sampleDiary("zombie"))
	require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)
	require.ErrorIs(t, store.AppendFinalSHA(ctx, 1, seq2, testSHA2), checkpoint.ErrStaleEpoch)
	require.ErrorIs(t, store.CompleteCheckpoint(ctx, 1, seq2, testSHA2, nil), checkpoint.ErrStaleEpoch)

	// Reloading from StatePath preserves epoch=2 (even if InitialEpoch=1 is passed) and all records.
	reloaded, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
		InitialEpoch: 1,
		StatePath:    statePath,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(2), reloaded.CurrentEpoch())
	reloadedRecs, err := reloaded.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Len(t, reloadedRecs, 2)
	require.Equal(t, uint64(2), reloadedRecs[0].Seq)
	require.Equal(t, checkpoint.StatusPending, reloadedRecs[0].Status)
	require.Equal(t, uint64(1), reloadedRecs[1].Seq)
	require.Equal(t, testSHA1, reloadedRecs[1].FinalSHA)
	require.Equal(t, snap1.SHA256, reloadedRecs[1].ArtifactsSnapshot.SHA256)
}

func TestSaveCheckpointCleanAndDirtyWorkspaces(t *testing.T) {
	ctx := context.Background()
	repoURL := "https://github.com/acme/repo.git"
	runID := "run-1"
	nodeAddr := "worker.impl"

	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
	ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
	rawStore, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
		InitialEpoch: 1,
		Ledger:       ledger,
		OwnerNode:    nodeAddr,
	})
	require.NoError(t, err)
	spyStore := &spyStateStore{inner: rawStore}
	fakeGit := newFakeGitAdapter(testSHA0)

	mgr, err := checkpoint.NewManager(checkpoint.ManagerConfig{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		RepoURL:     repoURL,
		StateStore:  spyStore,
		GitAdapter:  fakeGit,
		BlobStore:   blobStore,
		ArtifactSvc: artSvc,
	})
	require.NoError(t, err)

	wsDir := filepath.Join(t.TempDir(), "workspace")
	artDir := filepath.Join(t.TempDir(), "artifacts")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))

	// 1) Clean workspace: committed == false, finalSHA == initialSHA -> AppendFinalSHA NOT called.
	fakeGit.commitCommitted = false
	fakeGit.commitSHA = testSHA0
	rec1, err := mgr.SaveCheckpoint(ctx, 1, wsDir, artDir, []string{"cmd/**"}, sampleDiary("clean"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), rec1.Seq)
	require.Equal(t, testSHA0, rec1.InitialSHA)
	require.Equal(t, testSHA0, rec1.FinalSHA)
	require.Equal(t, checkpoint.StatusComplete, rec1.Status)
	require.Nil(t, rec1.ArtifactsSnapshot)
	require.Equal(t, 1, spyStore.registerCalls)
	require.Equal(t, 0, spyStore.appendFinalCalls)
	require.Equal(t, 1, spyStore.completeCalls)

	// 2) Dirty workspace + artifacts: committed == true, finalSHA != initialSHA -> AppendFinalSHA called.
	// Also verifies second checkpoint in same epoch does not fail Ledger registration.
	require.NoError(t, os.MkdirAll(artDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(artDir, "draft.md"), []byte("# Draft v1\n"), 0o644))
	fakeGit.statusPaths = []git.ChangedPath{{Path: "cmd/app.go"}}
	fakeGit.commitCommitted = true
	fakeGit.commitSHA = testSHA1

	rec2, err := mgr.SaveCheckpoint(ctx, 1, wsDir, artDir, []string{"cmd/**"}, sampleDiary("dirty"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), rec2.Seq)
	require.Equal(t, testSHA0, rec2.InitialSHA)
	require.Equal(t, testSHA1, rec2.FinalSHA)
	require.Equal(t, checkpoint.StatusComplete, rec2.Status)
	require.NotNil(t, rec2.ArtifactsSnapshot)
	require.Equal(t, 1, spyStore.registerCalls)
	require.Equal(t, 1, spyStore.appendFinalCalls)
	require.Equal(t, 2, spyStore.completeCalls)

	st, err := blobStore.Stat(ctx, rec2.ArtifactsSnapshot.Key)
	require.NoError(t, err)
	require.Equal(t, rec2.ArtifactsSnapshot.SHA256, st.SHA256)
}

func TestSaveCheckpointPreCheckBlocksUnauthorizedPaths(t *testing.T) {
	ctx := context.Background()
	blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	rawStore, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{InitialEpoch: 1})
	require.NoError(t, err)
	spyStore := &spyStateStore{inner: rawStore}
	fakeGit := newFakeGitAdapter(testSHA0)
	fakeGit.statusPaths = []git.ChangedPath{
		{Path: "cmd/app.go"},
		{Path: "README.md"},
	}

	mgr, err := checkpoint.NewManager(checkpoint.ManagerConfig{
		RunID:      "run-1",
		NodeAddr:   "worker.impl",
		RepoURL:    "https://github.com/acme/repo.git",
		StateStore: spyStore,
		GitAdapter: fakeGit,
		BlobStore:  blobStore,
	})
	require.NoError(t, err)

	_, err = mgr.SaveCheckpoint(ctx, 1, t.TempDir(), t.TempDir(), []string{"cmd/**"}, sampleDiary("blocked"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "README.md")
	require.Equal(t, 0, spyStore.registerCalls)
	require.Equal(t, 0, spyStore.beginCalls)
	require.Equal(t, 0, fakeGit.commitCalls)

	recs, err := rawStore.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Empty(t, recs)
}

func TestSaveCheckpointZombieFencingMidCommit(t *testing.T) {
	ctx := context.Background()

	t.Run("dirty commit fenced at AppendFinalSHA", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		store, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(testSHA0)
		fakeGit.commitCommitted = true
		fakeGit.commitSHA = testSHA1
		fakeGit.onCommitAllAndPush = func(_ string) {
			require.NoError(t, store.AdvanceEpoch(2))
		}

		mgr, err := checkpoint.NewManager(checkpoint.ManagerConfig{
			RunID:      "run-1",
			NodeAddr:   "worker.impl",
			RepoURL:    "https://github.com/acme/repo.git",
			StateStore: store,
			GitAdapter: fakeGit,
			BlobStore:  blobStore,
		})
		require.NoError(t, err)

		_, err = mgr.SaveCheckpoint(ctx, 1, t.TempDir(), t.TempDir(), nil, sampleDiary("zombie-dirty"))
		require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)
		require.Equal(t, uint64(2), store.CurrentEpoch())

		recs, err := store.ListCheckpoints(ctx)
		require.NoError(t, err)
		require.Len(t, recs, 1)
		require.Equal(t, checkpoint.StatusPending, recs[0].Status)
		require.Empty(t, recs[0].FinalSHA)
	})

	t.Run("clean commit fenced at CompleteCheckpoint", func(t *testing.T) {
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		store, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{InitialEpoch: 1})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(testSHA0)
		fakeGit.commitCommitted = false
		fakeGit.commitSHA = testSHA0
		fakeGit.onCommitAllAndPush = func(_ string) {
			require.NoError(t, store.AdvanceEpoch(2))
		}

		mgr, err := checkpoint.NewManager(checkpoint.ManagerConfig{
			RunID:      "run-1",
			NodeAddr:   "worker.impl",
			RepoURL:    "https://github.com/acme/repo.git",
			StateStore: store,
			GitAdapter: fakeGit,
			BlobStore:  blobStore,
		})
		require.NoError(t, err)

		_, err = mgr.SaveCheckpoint(ctx, 1, t.TempDir(), t.TempDir(), nil, sampleDiary("zombie-clean"))
		require.ErrorIs(t, err, checkpoint.ErrStaleEpoch)

		recs, err := store.ListCheckpoints(ctx)
		require.NoError(t, err)
		require.Len(t, recs, 1)
		require.Equal(t, checkpoint.StatusPending, recs[0].Status)
	})
}

func TestRecoverLatestAlignedFaultMatrix(t *testing.T) {
	ctx := context.Background()
	repoURL := "https://github.com/acme/repo.git"
	runID := "run-fault"
	nodeAddr := "worker.impl"

	setupSeq1 := func(t *testing.T) (*blob.LocalStore, *artifact.Service, *git.Ledger, *checkpoint.LocalNodeStateStore, *fakeGitAdapter, *checkpoint.Manager, checkpoint.CheckpointRecord) {
		t.Helper()
		blobStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
		ledger := git.NewLedger(git.SourceRef{URL: repoURL, Branch: "main"})
		store, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
			InitialEpoch: 1,
			Ledger:       ledger,
			OwnerNode:    nodeAddr,
		})
		require.NoError(t, err)
		fakeGit := newFakeGitAdapter(testSHA0)
		mgr, err := checkpoint.NewManager(checkpoint.ManagerConfig{
			RunID:       runID,
			NodeAddr:    nodeAddr,
			RepoURL:     repoURL,
			StateStore:  store,
			GitAdapter:  fakeGit,
			BlobStore:   blobStore,
			ArtifactSvc: artSvc,
		})
		require.NoError(t, err)

		// Empty store returns ErrNoAlignedCheckpoint.
		_, err = mgr.RecoverLatestAligned(ctx, 2, filepath.Join(t.TempDir(), "empty-ws"), filepath.Join(t.TempDir(), "empty-art"))
		require.ErrorIs(t, err, checkpoint.ErrNoAlignedCheckpoint)

		ws1 := filepath.Join(t.TempDir(), "ws1")
		art1 := filepath.Join(t.TempDir(), "art1")
		require.NoError(t, os.MkdirAll(ws1, 0o755))
		require.NoError(t, os.MkdirAll(art1, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(art1, "spec.md"), []byte("seq1 artifact\n"), 0o644))

		fakeGit.commitCommitted = true
		fakeGit.commitSHA = testSHA1
		rec1, err := mgr.SaveCheckpoint(ctx, 1, ws1, art1, nil, sampleDiary("seq1"))
		require.NoError(t, err)
		return blobStore, artSvc, ledger, store, fakeGit, mgr, rec1
	}

	assertRecoveredToSeq1 := func(t *testing.T, ledger *git.Ledger, fakeGit *fakeGitAdapter, mgr *checkpoint.Manager, rec1 checkpoint.CheckpointRecord) {
		t.Helper()
		destWs := filepath.Join(t.TempDir(), "recover-ws")
		destArt := filepath.Join(t.TempDir(), "recover-art")

		res, err := mgr.RecoverLatestAligned(ctx, 2, destWs, destArt)
		require.NoError(t, err)
		require.Equal(t, uint64(1), res.Record.Seq)
		require.Equal(t, testSHA1, res.Record.FinalSHA)
		expectedE2 := git.CheckpointBranchName(runID, nodeAddr, 2)
		require.Equal(t, expectedE2, res.NewCheckpointBranch)
		require.NoError(t, ledger.EnsureRegistered(repoURL, expectedE2))

		require.Len(t, fakeGit.prepareCalls, 1)
		require.Equal(t, rec1.Branch, fakeGit.prepareCalls[0].fetchBranch)
		require.Equal(t, testSHA1, fakeGit.prepareCalls[0].pinSHA)
		require.Equal(t, destWs, fakeGit.prepareCalls[0].destDir)

		restoredArt, err := os.ReadFile(filepath.Join(destArt, "spec.md"))
		require.NoError(t, err)
		require.Equal(t, "seq1 artifact\n", string(restoredArt))

		require.Len(t, res.ChronologicalDiaries, 1)
		require.Equal(t, uint64(1), res.ChronologicalDiaries[0].Seq)
		require.Contains(t, res.DiaryPromptContext, "Checkpoint #1")
		require.Contains(t, res.DiaryPromptContext, testSHA1)
		require.Contains(t, res.DiaryPromptContext, "completed seq1")
		require.NotContains(t, res.DiaryPromptContext, "seq2")
	}

	t.Run("Case A: Seq=2 crashed StatusPending after CommitAllAndPush", func(t *testing.T) {
		_, _, ledger, store, fakeGit, mgr, rec1 := setupSeq1(t)
		seq2, err := store.BeginCheckpoint(ctx, 1, rec1.Branch, testSHA1, sampleDiary("seq2"))
		require.NoError(t, err)
		require.Equal(t, uint64(2), seq2)
		// Simulate CommitAllAndPush + AppendFinalSHA completed, crash before CompleteCheckpoint.
		require.NoError(t, store.AppendFinalSHA(ctx, 1, seq2, testSHA2))
		fakeGit.branchContains[rec1.Branch][testSHA2] = true

		require.NoError(t, store.AdvanceEpoch(2))
		assertRecoveredToSeq1(t, ledger, fakeGit, mgr, rec1)
	})

	t.Run("Case B: Seq=2 StatusComplete in store but missing on remote branch", func(t *testing.T) {
		_, _, ledger, store, fakeGit, mgr, rec1 := setupSeq1(t)
		seq2, err := store.BeginCheckpoint(ctx, 1, rec1.Branch, testSHA1, sampleDiary("seq2"))
		require.NoError(t, err)
		require.NoError(t, store.AppendFinalSHA(ctx, 1, seq2, testSHA2))
		require.NoError(t, store.CompleteCheckpoint(ctx, 1, seq2, testSHA2, nil))
		// Do NOT add testSHA2 to fakeGit.branchContains[rec1.Branch].

		require.NoError(t, store.AdvanceEpoch(2))
		assertRecoveredToSeq1(t, ledger, fakeGit, mgr, rec1)
	})

	t.Run("Case C1: Seq=2 ArtifactsSnapshot deleted from BlobStore", func(t *testing.T) {
		blobStore, _, ledger, store, fakeGit, mgr, rec1 := setupSeq1(t)
		ws2 := filepath.Join(t.TempDir(), "ws2")
		art2 := filepath.Join(t.TempDir(), "art2")
		require.NoError(t, os.MkdirAll(ws2, 0o755))
		require.NoError(t, os.MkdirAll(art2, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(art2, "spec.md"), []byte("seq2 artifact\n"), 0o644))
		fakeGit.commitCommitted = true
		fakeGit.commitSHA = testSHA2

		rec2, err := mgr.SaveCheckpoint(ctx, 1, ws2, art2, nil, sampleDiary("seq2"))
		require.NoError(t, err)
		require.NotNil(t, rec2.ArtifactsSnapshot)

		require.NoError(t, blobStore.Delete(ctx, rec2.ArtifactsSnapshot.Key))
		require.NoError(t, store.AdvanceEpoch(2))
		assertRecoveredToSeq1(t, ledger, fakeGit, mgr, rec1)
	})

	t.Run("Case C2: Seq=2 ArtifactsSnapshot digest mismatch in BlobStore", func(t *testing.T) {
		blobStore, _, ledger, store, fakeGit, mgr, rec1 := setupSeq1(t)
		ws2 := filepath.Join(t.TempDir(), "ws2")
		art2 := filepath.Join(t.TempDir(), "art2")
		require.NoError(t, os.MkdirAll(ws2, 0o755))
		require.NoError(t, os.MkdirAll(art2, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(art2, "spec.md"), []byte("seq2 artifact\n"), 0o644))
		fakeGit.commitCommitted = true
		fakeGit.commitSHA = testSHA2

		rec2, err := mgr.SaveCheckpoint(ctx, 1, ws2, art2, nil, sampleDiary("seq2"))
		require.NoError(t, err)
		require.NotNil(t, rec2.ArtifactsSnapshot)

		_, err = blobStore.Put(ctx, rec2.ArtifactsSnapshot.Key, strings.NewReader("corrupted-bytes"), "")
		require.NoError(t, err)
		require.NoError(t, store.AdvanceEpoch(2))
		assertRecoveredToSeq1(t, ledger, fakeGit, mgr, rec1)
	})
}

func TestFormatDiaryPromptContextChronological(t *testing.T) {
	out := checkpoint.FormatDiaryPromptContext([]checkpoint.CheckpointRecord{
		{Seq: 2, Epoch: 2, FinalSHA: testSHA2, Diary: sampleDiary("second")},
		{Seq: 1, Epoch: 1, FinalSHA: testSHA1, Diary: sampleDiary("first")},
	})
	idx1 := strings.Index(out, "Checkpoint #1 (Epoch 1, Commit "+testSHA1+")")
	idx2 := strings.Index(out, "Checkpoint #2 (Epoch 2, Commit "+testSHA2+")")
	require.GreaterOrEqual(t, idx1, 0)
	require.Greater(t, idx2, idx1)
	require.Contains(t, out, "completed first")
	require.Contains(t, out, "completed second")
	require.Empty(t, checkpoint.FormatDiaryPromptContext(nil))
}
