package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/session"
)

// LocalNodeStateStoreOptions configures a LocalNodeStateStore.
type LocalNodeStateStoreOptions struct {
	InitialEpoch uint64
	Ledger       *git.Ledger
	OwnerNode    string
	StatePath    string
}

type registeredBranchEntry struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
	Epoch   uint64 `json:"epoch"`
}

type persistedState struct {
	Epoch              uint64                           `json:"epoch"`
	Records            []CheckpointRecord               `json:"records,omitempty"`
	RegisteredBranches map[string]registeredBranchEntry `json:"registered_branches,omitempty"`
}

// LocalNodeStateStore is a thread-safe in-memory and optional JSON-file-backed NodeStateStore
// enforcing strict monotonic execution epoch fencing.
type LocalNodeStateStore struct {
	mu                 sync.RWMutex
	epoch              uint64
	ledger             *git.Ledger
	ownerNode          string
	statePath          string
	records            []CheckpointRecord
	registeredBranches map[string]registeredBranchEntry
}

var _ NodeStateStore = (*LocalNodeStateStore)(nil)

// NewLocalNodeStateStore initializes a LocalNodeStateStore, reloading persisted state if StatePath exists.
func NewLocalNodeStateStore(opts LocalNodeStateStoreOptions) (*LocalNodeStateStore, error) {
	if opts.Ledger != nil && strings.TrimSpace(opts.OwnerNode) == "" {
		return nil, errors.New("owner_node is required when ledger is configured")
	}

	epoch := opts.InitialEpoch
	if epoch == 0 {
		epoch = 1
	}

	statePath := strings.TrimSpace(opts.StatePath)
	store := &LocalNodeStateStore{
		epoch:              epoch,
		ledger:             opts.Ledger,
		ownerNode:          strings.TrimSpace(opts.OwnerNode),
		statePath:          statePath,
		registeredBranches: make(map[string]registeredBranchEntry),
	}

	if statePath != "" {
		absPath, err := filepath.Abs(statePath)
		if err != nil {
			return nil, fmt.Errorf("resolve checkpoint state path %q: %w", statePath, err)
		}
		store.statePath = filepath.Clean(absPath)
		if err := os.MkdirAll(filepath.Dir(store.statePath), 0o700); err != nil {
			return nil, fmt.Errorf("create checkpoint state directory: %w", err)
		}

		data, err := os.ReadFile(store.statePath)
		if err == nil {
			var loaded persistedState
			if uerr := json.Unmarshal(data, &loaded); uerr != nil {
				return nil, fmt.Errorf("decode checkpoint state %q: %w", store.statePath, uerr)
			}
			if loaded.Epoch == 0 {
				return nil, fmt.Errorf("invalid persisted epoch 0 in %q", store.statePath)
			}
			store.epoch = loaded.Epoch
			store.records = cloneRecordSlice(loaded.Records)
			if loaded.RegisteredBranches != nil {
				store.registeredBranches = loaded.RegisteredBranches
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read checkpoint state %q: %w", store.statePath, err)
		}
	}

	return store, nil
}

// CurrentEpoch returns the store's current authoritative execution epoch.
func (s *LocalNodeStateStore) CurrentEpoch() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.epoch
}

// AdvanceEpoch monotonically advances the authoritative epoch, fencing all older epochs.
func (s *LocalNodeStateStore) AdvanceEpoch(nextEpoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if nextEpoch <= s.epoch {
		return fmt.Errorf("next epoch %d must be greater than current epoch %d", nextEpoch, s.epoch)
	}
	if err := persistStoreState(s.statePath, persistedState{
		Epoch:              nextEpoch,
		Records:            s.records,
		RegisteredBranches: s.registeredBranches,
	}); err != nil {
		return err
	}
	s.epoch = nextEpoch
	return nil
}

// RegisterCheckpointBranch registers an epoch-scoped checkpoint branch after verifying epoch fencing.
func (s *LocalNodeStateStore) RegisterCheckpointBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(repoURL) == "" {
		return errors.New("repository url is required")
	}
	cleanBranch, err := git.NormalizeBranchName(branch)
	if err != nil {
		return err
	}
	if !git.IsValidCommitSHA(baseSHA) {
		return fmt.Errorf("%w: %q", git.ErrInvalidBaseSHA, baseSHA)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if epoch != s.epoch {
		return ErrStaleEpoch
	}

	key := branchRegistrationKey(repoURL, cleanBranch)
	if existing, ok := s.registeredBranches[key]; ok {
		if existing.Epoch == epoch {
			if existing.BaseSHA != baseSHA {
				return fmt.Errorf("checkpoint branch %q already registered at epoch %d with base_sha %s (got %s)",
					cleanBranch, epoch, existing.BaseSHA, baseSHA)
			}
			if s.ledger != nil {
				return s.ledger.Register(repoURL, cleanBranch, baseSHA, git.KindCheckpointBranch, s.ownerNode, epoch)
			}
			return nil
		}
		if epoch < existing.Epoch {
			return ErrStaleEpoch
		}
	}

	if s.ledger != nil {
		if err := s.ledger.Register(repoURL, cleanBranch, baseSHA, git.KindCheckpointBranch, s.ownerNode, epoch); err != nil {
			return err
		}
	}

	nextBranches := cloneBranchMap(s.registeredBranches)
	nextBranches[key] = registeredBranchEntry{
		RepoURL: strings.TrimSpace(repoURL),
		Branch:  cleanBranch,
		BaseSHA: baseSHA,
		Epoch:   epoch,
	}
	if err := persistStoreState(s.statePath, persistedState{
		Epoch:              s.epoch,
		Records:            s.records,
		RegisteredBranches: nextBranches,
	}); err != nil {
		return err
	}
	s.registeredBranches = nextBranches
	return nil
}

// BeginCheckpoint verifies reqEpoch == currentEpoch, allocates the next Seq, and persists a StatusPending record.
func (s *LocalNodeStateStore) BeginCheckpoint(ctx context.Context, reqEpoch uint64, branch, initialSHA string, diary session.DiaryInput) (uint64, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	cleanBranch, err := git.NormalizeBranchName(branch)
	if err != nil {
		return 0, err
	}
	if !git.IsValidCommitSHA(initialSHA) {
		return 0, fmt.Errorf("%w: initial_sha %q", git.ErrInvalidBaseSHA, initialSHA)
	}
	if err := diary.Validate(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if reqEpoch != s.epoch {
		return 0, ErrStaleEpoch
	}

	seq := uint64(len(s.records) + 1)
	rec := CheckpointRecord{
		Seq:        seq,
		Epoch:      reqEpoch,
		Branch:     cleanBranch,
		InitialSHA: initialSHA,
		Diary:      cloneDiaryInput(diary),
		Status:     StatusPending,
	}
	nextRecords := append(cloneRecordSlice(s.records), rec)
	if err := persistStoreState(s.statePath, persistedState{
		Epoch:              s.epoch,
		Records:            nextRecords,
		RegisteredBranches: s.registeredBranches,
	}); err != nil {
		return 0, err
	}
	s.records = nextRecords
	return seq, nil
}

// AppendFinalSHA verifies reqEpoch == currentEpoch and records a new Git commit SHA onto a pending record.
func (s *LocalNodeStateStore) AppendFinalSHA(ctx context.Context, reqEpoch, seq uint64, finalSHA string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if !git.IsValidCommitSHA(finalSHA) {
		return fmt.Errorf("%w: final_sha %q", git.ErrInvalidBaseSHA, finalSHA)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if reqEpoch != s.epoch {
		return ErrStaleEpoch
	}
	idx, err := findRecordIndex(s.records, seq)
	if err != nil {
		return err
	}
	if s.records[idx].Epoch != reqEpoch {
		return ErrStaleEpoch
	}
	if s.records[idx].Status != StatusPending {
		return fmt.Errorf("checkpoint seq %d has status %s, expected %s", seq, s.records[idx].Status, StatusPending)
	}

	nextRecords := cloneRecordSlice(s.records)
	nextRecords[idx].FinalSHA = finalSHA
	if err := persistStoreState(s.statePath, persistedState{
		Epoch:              s.epoch,
		Records:            nextRecords,
		RegisteredBranches: s.registeredBranches,
	}); err != nil {
		return err
	}
	s.records = nextRecords
	return nil
}

// CompleteCheckpoint verifies reqEpoch == currentEpoch, stores ArtifactsSnapshot, and marks StatusComplete.
func (s *LocalNodeStateStore) CompleteCheckpoint(ctx context.Context, reqEpoch, seq uint64, finalSHA string, artifactsSnap *blob.ObjectMeta) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if !git.IsValidCommitSHA(finalSHA) {
		return fmt.Errorf("%w: final_sha %q", git.ErrInvalidBaseSHA, finalSHA)
	}
	if artifactsSnap != nil {
		if err := blob.ValidateKey(artifactsSnap.Key); err != nil {
			return err
		}
		if !blob.IsValidSHA256Hex(artifactsSnap.SHA256) {
			return fmt.Errorf("%w: invalid snapshot sha256 %q", blob.ErrDigestMismatch, artifactsSnap.SHA256)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if reqEpoch != s.epoch {
		return ErrStaleEpoch
	}
	idx, err := findRecordIndex(s.records, seq)
	if err != nil {
		return err
	}
	if s.records[idx].Epoch != reqEpoch {
		return ErrStaleEpoch
	}
	if s.records[idx].Status != StatusPending {
		return fmt.Errorf("checkpoint seq %d has status %s, expected %s", seq, s.records[idx].Status, StatusPending)
	}
	if s.records[idx].FinalSHA != "" && s.records[idx].FinalSHA != finalSHA {
		return fmt.Errorf("checkpoint seq %d final_sha mismatch: recorded %s, got %s", seq, s.records[idx].FinalSHA, finalSHA)
	}

	nextRecords := cloneRecordSlice(s.records)
	nextRecords[idx].FinalSHA = finalSHA
	nextRecords[idx].ArtifactsSnapshot = cloneObjectMeta(artifactsSnap)
	nextRecords[idx].Status = StatusComplete
	if err := persistStoreState(s.statePath, persistedState{
		Epoch:              s.epoch,
		Records:            nextRecords,
		RegisteredBranches: s.registeredBranches,
	}); err != nil {
		return err
	}
	s.records = nextRecords
	return nil
}

// ListCheckpoints returns deep copies of all checkpoint records ordered from newest to oldest (Seq descending).
func (s *LocalNodeStateStore) ListCheckpoints(ctx context.Context) ([]CheckpointRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := cloneRecordSlice(s.records)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Seq > out[j].Seq
	})
	return out, nil
}

func branchRegistrationKey(repoURL, cleanBranch string) string {
	return git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch
}

func findRecordIndex(records []CheckpointRecord, seq uint64) (int, error) {
	for i := range records {
		if records[i].Seq == seq {
			return i, nil
		}
	}
	return -1, fmt.Errorf("checkpoint seq %d not found", seq)
}

func cloneRecordSlice(records []CheckpointRecord) []CheckpointRecord {
	if len(records) == 0 {
		return nil
	}
	out := make([]CheckpointRecord, len(records))
	for i, r := range records {
		out[i] = r.Clone()
	}
	return out
}

func cloneBranchMap(src map[string]registeredBranchEntry) map[string]registeredBranchEntry {
	out := make(map[string]registeredBranchEntry, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func persistStoreState(statePath string, state persistedState) error {
	if statePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(statePath)
	tmpFile, err := os.CreateTemp(dir, ".ckpt-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp checkpoint state file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	_, writeErr := tmpFile.Write(data)
	closeErr := tmpFile.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write temp checkpoint state file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restrict temp checkpoint state permissions: %w", err)
	}
	if err := os.Rename(tmpPath, statePath); err != nil {
		return fmt.Errorf("commit checkpoint state %q: %w", statePath, err)
	}
	cleanup = false
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
