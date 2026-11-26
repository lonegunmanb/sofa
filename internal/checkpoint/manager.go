package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/session"
)

// ManagerConfig configures a checkpoint Manager for a single node execution identity.
type ManagerConfig struct {
	RunID       string
	NodeAddr    string
	RepoURL     string
	Auth        git.AuthConfig
	StateStore  NodeStateStore
	GitAdapter  git.Adapter
	BlobStore   blob.Store
	ArtifactSvc *artifact.Service
}

// RecoveryResult describes a three-way-aligned checkpoint restored onto a new machine/epoch.
type RecoveryResult struct {
	Record               CheckpointRecord
	NewCheckpointBranch  string
	ChronologicalDiaries []CheckpointRecord
	DiaryPromptContext   string
}

// Manager orchestrates epoch-scoped two-phase checkpoint persistence and aligned cross-machine recovery.
type Manager struct {
	runID       string
	nodeAddr    string
	repoURL     string
	auth        git.AuthConfig
	stateStore  NodeStateStore
	gitAdapter  git.Adapter
	blobStore   blob.Store
	artifactSvc *artifact.Service

	mu               sync.Mutex
	registeredEpochs map[uint64]string
}

// NewManager validates cfg and constructs a checkpoint Manager.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if strings.TrimSpace(cfg.RunID) == "" {
		return nil, errors.New("checkpoint: run_id is required")
	}
	if strings.TrimSpace(cfg.NodeAddr) == "" {
		return nil, errors.New("checkpoint: node_addr is required")
	}
	if strings.TrimSpace(cfg.RepoURL) == "" {
		return nil, errors.New("checkpoint: repo_url is required")
	}
	if cfg.StateStore == nil {
		return nil, errors.New("checkpoint: state_store is required")
	}
	if cfg.GitAdapter == nil {
		return nil, errors.New("checkpoint: git_adapter is required")
	}
	if cfg.BlobStore == nil {
		return nil, errors.New("checkpoint: blob_store is required")
	}
	artSvc := cfg.ArtifactSvc
	if artSvc == nil {
		artSvc = artifact.NewService(cfg.BlobStore, artifact.ServiceOptions{})
	}
	return &Manager{
		runID:            strings.TrimSpace(cfg.RunID),
		nodeAddr:         strings.TrimSpace(cfg.NodeAddr),
		repoURL:          strings.TrimSpace(cfg.RepoURL),
		auth:             cfg.Auth,
		stateStore:       cfg.StateStore,
		gitAdapter:       cfg.GitAdapter,
		blobStore:        cfg.BlobStore,
		artifactSvc:      artSvc,
		registeredEpochs: make(map[uint64]string),
	}, nil
}

// SaveCheckpoint executes the three-phase epoch-fenced checkpoint save protocol:
// pre-check writable_paths -> idempotent epoch branch registration -> Phase 1 pending diary ->
// Phase 2 Git commit/push + AppendFinalSHA + non-Git artifacts snapshot -> Phase 3 alignment verification + CompleteCheckpoint.
func (m *Manager) SaveCheckpoint(
	ctx context.Context,
	epoch uint64,
	workspaceDir string,
	artifactsDir string,
	writablePaths []string,
	diary session.DiaryInput,
) (CheckpointRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CheckpointRecord{}, err
	}
	if epoch == 0 {
		return CheckpointRecord{}, errors.New("checkpoint: epoch must be greater than 0")
	}
	if strings.TrimSpace(workspaceDir) == "" {
		return CheckpointRecord{}, errors.New("checkpoint: workspace_dir is required")
	}
	if err := diary.Validate(); err != nil {
		return CheckpointRecord{}, err
	}

	// Pre-check: never commit or record checkpoints with unauthorized workspace modifications.
	changed, err := m.gitAdapter.StatusPaths(ctx, workspaceDir)
	if err != nil {
		return CheckpointRecord{}, fmt.Errorf("checkpoint pre-check status: %w", err)
	}
	if unauth := git.FindUnauthorizedPaths(changed, writablePaths); len(unauth) > 0 {
		return CheckpointRecord{}, fmt.Errorf("checkpoint blocked: unauthorized workspace paths outside writable_paths: %s", strings.Join(unauth, ", "))
	}

	initialSHA, err := m.gitAdapter.HeadSHA(ctx, workspaceDir)
	if err != nil {
		return CheckpointRecord{}, fmt.Errorf("checkpoint read HEAD sha: %w", err)
	}
	if !git.IsValidCommitSHA(initialSHA) {
		return CheckpointRecord{}, fmt.Errorf("%w: %q", git.ErrInvalidBaseSHA, initialSHA)
	}

	ckptBranch := git.CheckpointBranchName(m.runID, m.nodeAddr, epoch)
	if err := m.ensureEpochBranchRegistered(ctx, epoch, ckptBranch, initialSHA); err != nil {
		return CheckpointRecord{}, err
	}

	// Phase 1: Persist StatusPending diary with initialSHA before mutating remote branch.
	seq, err := m.stateStore.BeginCheckpoint(ctx, epoch, ckptBranch, initialSHA, diary)
	if err != nil {
		return CheckpointRecord{}, err
	}

	// Phase 2: Commit (if dirty) and push to e<epoch>, append new FinalSHA if changed, and snapshot /.sofa/artifacts.
	commitMsg := formatCheckpointCommitMessage(m.runID, m.nodeAddr, epoch, seq, diary)
	finalSHA, committed, err := m.gitAdapter.CommitAllAndPush(ctx, workspaceDir, m.repoURL, ckptBranch, commitMsg, m.auth)
	if err != nil {
		return CheckpointRecord{}, fmt.Errorf("checkpoint commit and push: %w", err)
	}
	if !git.IsValidCommitSHA(finalSHA) {
		return CheckpointRecord{}, fmt.Errorf("checkpoint commit returned invalid sha %q", finalSHA)
	}

	if committed && finalSHA != initialSHA {
		if err := m.stateStore.AppendFinalSHA(ctx, epoch, seq, finalSHA); err != nil {
			return CheckpointRecord{}, err
		}
	}

	var artifactsSnap *blob.ObjectMeta
	if strings.TrimSpace(artifactsDir) != "" {
		ckptBlobKey := m.checkpointBlobKey(seq, epoch)
		artifactsSnap, err = m.artifactSvc.SnapshotStagingDir(ctx, ckptBlobKey, artifactsDir)
		if err != nil {
			return CheckpointRecord{}, fmt.Errorf("checkpoint snapshot artifacts: %w", err)
		}
	}

	// Phase 3: Three-way alignment verification (Git remote branch + BlobStore + epoch fence).
	_, contains, err := m.gitAdapter.BranchContainsSHA(ctx, m.repoURL, ckptBranch, finalSHA, m.auth)
	if err != nil {
		return CheckpointRecord{}, fmt.Errorf("checkpoint verify remote branch: %w", err)
	}
	if !contains {
		return CheckpointRecord{}, fmt.Errorf("checkpoint alignment failed: remote branch %q does not contain sha %s", ckptBranch, finalSHA)
	}

	if artifactsSnap != nil {
		st, err := m.blobStore.Stat(ctx, artifactsSnap.Key)
		if err != nil {
			return CheckpointRecord{}, fmt.Errorf("checkpoint verify artifact blob %q: %w", artifactsSnap.Key, err)
		}
		if st.SHA256 != artifactsSnap.SHA256 {
			return CheckpointRecord{}, fmt.Errorf("%w: checkpoint artifact snapshot %q expected %s, got %s",
				blob.ErrDigestMismatch, artifactsSnap.Key, artifactsSnap.SHA256, st.SHA256)
		}
	}

	if err := m.stateStore.CompleteCheckpoint(ctx, epoch, seq, finalSHA, artifactsSnap); err != nil {
		return CheckpointRecord{}, err
	}

	return CheckpointRecord{
		Seq:               seq,
		Epoch:             epoch,
		Branch:            ckptBranch,
		InitialSHA:        initialSHA,
		FinalSHA:          finalSHA,
		ArtifactsSnapshot: cloneObjectMeta(artifactsSnap),
		Diary:             cloneDiaryInput(diary),
		Status:            StatusComplete,
	}, nil
}

// RecoverLatestAligned scans checkpoints newest-to-oldest, discards incomplete or unaligned candidates,
// registers the new epoch branch from the aligned FinalSHA, checks out workspace code, restores
// /.sofa/artifacts, and returns chronological diary history up to the aligned checkpoint.
func (m *Manager) RecoverLatestAligned(
	ctx context.Context,
	newEpoch uint64,
	destWorkspaceDir string,
	destArtifactsDir string,
) (RecoveryResult, error) {
	if err := checkContext(ctx); err != nil {
		return RecoveryResult{}, err
	}
	if newEpoch == 0 {
		return RecoveryResult{}, errors.New("checkpoint: new_epoch must be greater than 0")
	}
	if strings.TrimSpace(destWorkspaceDir) == "" {
		return RecoveryResult{}, errors.New("checkpoint: dest_workspace_dir is required")
	}

	records, err := m.stateStore.ListCheckpoints(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	if len(records) == 0 {
		return RecoveryResult{}, ErrNoAlignedCheckpoint
	}

	// Ensure descending Seq order regardless of custom NodeStateStore implementation.
	ordered := cloneRecordSlice(records)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Seq > ordered[j].Seq
	})

	for _, rec := range ordered {
		if rec.Status != StatusComplete || !git.IsValidCommitSHA(rec.FinalSHA) {
			continue
		}

		_, contains, err := m.gitAdapter.BranchContainsSHA(ctx, m.repoURL, rec.Branch, rec.FinalSHA, m.auth)
		if err != nil {
			return RecoveryResult{}, fmt.Errorf("recovery verify remote branch %q: %w", rec.Branch, err)
		}
		if !contains {
			continue
		}

		if rec.ArtifactsSnapshot != nil {
			st, statErr := m.blobStore.Stat(ctx, rec.ArtifactsSnapshot.Key)
			if errors.Is(statErr, blob.ErrNotFound) {
				continue
			}
			if statErr != nil {
				return RecoveryResult{}, fmt.Errorf("recovery stat artifact blob %q: %w", rec.ArtifactsSnapshot.Key, statErr)
			}
			if st.SHA256 != rec.ArtifactsSnapshot.SHA256 {
				continue
			}
		}

		newBranch := git.CheckpointBranchName(m.runID, m.nodeAddr, newEpoch)
		if err := m.stateStore.RegisterCheckpointBranch(ctx, newEpoch, m.repoURL, newBranch, rec.FinalSHA); err != nil {
			return RecoveryResult{}, err
		}
		m.markEpochBranchRegistered(newEpoch, newBranch)

		if err := m.gitAdapter.PrepareWorkspace(ctx, m.repoURL, rec.Branch, rec.FinalSHA, destWorkspaceDir, m.auth); err != nil {
			return RecoveryResult{}, fmt.Errorf("recovery prepare workspace: %w", err)
		}

		if rec.ArtifactsSnapshot != nil {
			if strings.TrimSpace(destArtifactsDir) == "" {
				return RecoveryResult{}, errors.New("checkpoint: dest_artifacts_dir is required to restore snapshot")
			}
			if err := m.artifactSvc.RestoreStagingDir(ctx, *rec.ArtifactsSnapshot, destArtifactsDir); err != nil {
				return RecoveryResult{}, fmt.Errorf("recovery restore artifacts: %w", err)
			}
		}

		var chronological []CheckpointRecord
		for _, cand := range ordered {
			if cand.Seq <= rec.Seq && cand.Status == StatusComplete {
				chronological = append(chronological, cand.Clone())
			}
		}
		sort.Slice(chronological, func(i, j int) bool {
			return chronological[i].Seq < chronological[j].Seq
		})

		return RecoveryResult{
			Record:               rec.Clone(),
			NewCheckpointBranch:  newBranch,
			ChronologicalDiaries: chronological,
			DiaryPromptContext:   FormatDiaryPromptContext(chronological),
		}, nil
	}

	return RecoveryResult{}, ErrNoAlignedCheckpoint
}

// FormatDiaryPromptContext renders chronological checkpoint records (Seq, Epoch, FinalSHA, and
// structured diary sections) into prompt context for resumed/recovered agent sessions.
func FormatDiaryPromptContext(records []CheckpointRecord) string {
	if len(records) == 0 {
		return ""
	}
	ordered := cloneRecordSlice(records)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Seq < ordered[j].Seq
	})

	var b strings.Builder
	b.WriteString("## Recovered Checkpoint Diary History\n")
	for _, rec := range ordered {
		fmt.Fprintf(&b, "\n### Checkpoint #%d (Epoch %d, Commit %s)\n", rec.Seq, rec.Epoch, rec.FinalSHA)
		writeDiarySection(&b, "Completed", rec.Diary.Completed)
		writeDiarySection(&b, "Decisions", rec.Diary.Decisions)
		writeDiarySection(&b, "Hypotheses", rec.Diary.Hypotheses)
		writeDiarySection(&b, "Test Evidence", rec.Diary.TestEvidence)
		writeDiarySection(&b, "Open Questions", rec.Diary.OpenQuestions)
		writeDiarySection(&b, "Next Steps", rec.Diary.NextSteps)
	}
	return strings.TrimSpace(b.String())
}

func (m *Manager) ensureEpochBranchRegistered(ctx context.Context, epoch uint64, ckptBranch, initialSHA string) error {
	m.mu.Lock()
	alreadyCached := m.registeredEpochs[epoch] == ckptBranch
	m.mu.Unlock()
	if alreadyCached {
		return nil
	}

	// Also skip re-registration if this epoch already has records in the store (e.g., new Manager on same epoch).
	existing, err := m.stateStore.ListCheckpoints(ctx)
	if err != nil {
		return err
	}
	for _, rec := range existing {
		if rec.Epoch == epoch && rec.Branch == ckptBranch {
			m.markEpochBranchRegistered(epoch, ckptBranch)
			return nil
		}
	}

	if err := m.stateStore.RegisterCheckpointBranch(ctx, epoch, m.repoURL, ckptBranch, initialSHA); err != nil {
		return err
	}
	m.markEpochBranchRegistered(epoch, ckptBranch)
	return nil
}

func (m *Manager) markEpochBranchRegistered(epoch uint64, ckptBranch string) {
	m.mu.Lock()
	m.registeredEpochs[epoch] = ckptBranch
	m.mu.Unlock()
}

func (m *Manager) checkpointBlobKey(seq, epoch uint64) string {
	return fmt.Sprintf("runs/%s/checkpoints/%s/seq-%06d-e%d-artifacts.zip",
		sanitizeBlobRunSegment(m.runID),
		sanitizeBlobNodeSegment(m.nodeAddr),
		seq,
		epoch,
	)
}

func sanitizeBlobRunSegment(runID string) string {
	var b strings.Builder
	prevDash := false
	for i := 0; i < len(runID); i++ {
		c := runID[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "run"
	}
	return out
}

func sanitizeBlobNodeSegment(nodeAddr string) string {
	sum := sha256.Sum256([]byte(nodeAddr))
	shortHash := hex.EncodeToString(sum[:4])

	var b strings.Builder
	prevUnderscore := false
	for i := 0; i < len(nodeAddr); i++ {
		c := nodeAddr[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	prefix := strings.Trim(b.String(), "_-")
	if prefix == "" {
		prefix = "node"
	}
	if len(prefix) > 64 {
		prefix = strings.TrimRight(prefix[:64], "_-")
		if prefix == "" {
			prefix = "node"
		}
	}
	return prefix + "_" + shortHash
}

func formatCheckpointCommitMessage(runID, nodeAddr string, epoch, seq uint64, diary session.DiaryInput) string {
	summary := "checkpoint"
	if len(diary.Completed) > 0 {
		summary = strings.TrimSpace(diary.Completed[0])
	} else if len(diary.NextSteps) > 0 {
		summary = strings.TrimSpace(diary.NextSteps[0])
	}
	return fmt.Sprintf("sofa(ckpt): run=%s node=%s epoch=%d seq=%d - %s", runID, nodeAddr, epoch, seq, summary)
}

func writeDiarySection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "- %s:\n", title)
	for _, item := range items {
		fmt.Fprintf(b, "  - %s\n", strings.TrimSpace(item))
	}
}
