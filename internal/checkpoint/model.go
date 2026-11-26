package checkpoint

import (
	"context"
	"errors"

	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/session"
)

// Status represents the lifecycle completion state of a two-phase checkpoint record.
type Status string

const (
	// StatusPending indicates Phase 1 diary persistence has started, before three-way alignment completes.
	StatusPending Status = "PENDING"
	// StatusComplete indicates Git branch commit, diary final SHA, and BlobStore snapshot are all aligned.
	StatusComplete Status = "COMPLETE"
)

var (
	// ErrStaleEpoch is returned when a superseded worker epoch attempts to mutate control-plane state.
	ErrStaleEpoch = errors.New("checkpoint: stale execution epoch")
	// ErrNoAlignedCheckpoint is returned when no three-way-aligned complete checkpoint exists for recovery.
	ErrNoAlignedCheckpoint = errors.New("checkpoint: no aligned checkpoint found")
)

// CheckpointRecord stores metadata and structured diary context for a single two-phase checkpoint.
type CheckpointRecord struct {
	Seq               uint64             `json:"seq"`
	Epoch             uint64             `json:"epoch"`
	Branch            string             `json:"branch"`
	InitialSHA        string             `json:"initial_sha"`
	FinalSHA          string             `json:"final_sha"`
	ArtifactsSnapshot *blob.ObjectMeta   `json:"artifacts_snapshot,omitempty"`
	Diary             session.DiaryInput `json:"diary"`
	Status            Status             `json:"status"`
}

// Clone returns a deep copy of r so callers cannot mutate store-owned slices or pointers.
func (r CheckpointRecord) Clone() CheckpointRecord {
	out := r
	out.ArtifactsSnapshot = cloneObjectMeta(r.ArtifactsSnapshot)
	out.Diary = cloneDiaryInput(r.Diary)
	return out
}

// NodeStateStore abstracts control-plane persistence and epoch fencing for node checkpoints.
type NodeStateStore interface {
	// RegisterCheckpointBranch registers an e<epoch> checkpoint branch in the Run branch ledger before first push.
	RegisterCheckpointBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error
	// BeginCheckpoint verifies reqEpoch == currentEpoch, allocates the next Seq, and persists a StatusPending record.
	BeginCheckpoint(ctx context.Context, reqEpoch uint64, branch, initialSHA string, diary session.DiaryInput) (uint64, error)
	// AppendFinalSHA verifies reqEpoch == currentEpoch and records a new Git commit SHA onto an existing record.
	AppendFinalSHA(ctx context.Context, reqEpoch, seq uint64, finalSHA string) error
	// CompleteCheckpoint verifies reqEpoch == currentEpoch, stores ArtifactsSnapshot, and marks StatusComplete.
	CompleteCheckpoint(ctx context.Context, reqEpoch, seq uint64, finalSHA string, artifactsSnap *blob.ObjectMeta) error
	// ListCheckpoints returns all checkpoint records ordered newest-to-oldest (Seq descending).
	ListCheckpoints(ctx context.Context) ([]CheckpointRecord, error)
}

func cloneObjectMeta(meta *blob.ObjectMeta) *blob.ObjectMeta {
	if meta == nil {
		return nil
	}
	cp := *meta
	return &cp
}

func cloneDiaryInput(d session.DiaryInput) session.DiaryInput {
	return session.DiaryInput{
		Completed:     append([]string(nil), d.Completed...),
		Hypotheses:    append([]string(nil), d.Hypotheses...),
		Decisions:     append([]string(nil), d.Decisions...),
		TestEvidence:  append([]string(nil), d.TestEvidence...),
		OpenQuestions: append([]string(nil), d.OpenQuestions...),
		NextSteps:     append([]string(nil), d.NextSteps...),
	}
}
