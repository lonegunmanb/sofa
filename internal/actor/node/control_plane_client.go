package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	daprclient "github.com/dapr/go-sdk/client"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
)

// RemoteNodeControlPlane implements runner.NodeControlPlane over a NodeActor ClientStub,
// translating lossless StaleEpoch / ErrorCode RPC responses back into Go sentinel errors.
type RemoteNodeControlPlane struct {
	stub  *ClientStub
	epoch uint64
}

var _ runner.NodeControlPlane = (*RemoteNodeControlPlane)(nil)

// NewRemoteNodeControlPlane wraps a NodeActor ClientStub and optional execution epoch
// (used to fence DrainInterjections when epoch > 0).
func NewRemoteNodeControlPlane(stub *ClientStub, epoch uint64) (*RemoteNodeControlPlane, error) {
	if stub == nil {
		return nil, errors.New("node: client stub is required")
	}
	return &RemoteNodeControlPlane{
		stub:  stub,
		epoch: epoch,
	}, nil
}

// NewRemoteNodeControlPlaneForNode constructs a Dapr-backed RemoteNodeControlPlane for (runID, canonicalNodeAddr, epoch).
func NewRemoteNodeControlPlaneForNode(client daprclient.Client, runID, canonicalNodeAddr string, epoch uint64) (*RemoteNodeControlPlane, error) {
	if client == nil {
		return nil, errors.New("node: dapr client is required")
	}
	stub, err := NewClientStubForNode(runID, canonicalNodeAddr, client)
	if err != nil {
		return nil, err
	}
	return NewRemoteNodeControlPlane(stub, epoch)
}

// RegisterCheckpointBranch verifies epoch fencing and registers the epoch checkpoint branch before push.
func (c *RemoteNodeControlPlane) RegisterCheckpointBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(repoURL) == "" {
		return errors.New("repository url is required")
	}
	if _, err := git.NormalizeBranchName(branch); err != nil {
		return err
	}
	if !git.IsValidCommitSHA(baseSHA) {
		return fmt.Errorf("%w: %q", git.ErrInvalidBaseSHA, baseSHA)
	}
	resp, err := c.stub.RegisterCheckpointBranch(ensureCtx(ctx), &RegisterCheckpointBranchReq{
		Epoch:   epoch,
		RepoURL: repoURL,
		Branch:  branch,
		BaseSHA: baseSHA,
	})
	if err != nil {
		return err
	}
	return decodeFencedResp(resp)
}

// RegisterWorkBranch verifies epoch fencing and registers the node's final work_branch before push.
func (c *RemoteNodeControlPlane) RegisterWorkBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(repoURL) == "" {
		return errors.New("repository url is required")
	}
	if _, err := git.NormalizeBranchName(branch); err != nil {
		return err
	}
	if !git.IsValidCommitSHA(baseSHA) {
		return fmt.Errorf("%w: %q", git.ErrInvalidBaseSHA, baseSHA)
	}
	resp, err := c.stub.RegisterWorkBranch(ensureCtx(ctx), &RegisterWorkBranchReq{
		Epoch:   epoch,
		RepoURL: repoURL,
		Branch:  branch,
		BaseSHA: baseSHA,
	})
	if err != nil {
		return err
	}
	return decodeFencedResp(resp)
}

// BeginCheckpoint verifies epoch fencing, allocates a sequence number, and persists a StatusPending diary record.
func (c *RemoteNodeControlPlane) BeginCheckpoint(ctx context.Context, reqEpoch uint64, branch, initialSHA string, diary session.DiaryInput) (uint64, error) {
	if err := checkCtx(ctx); err != nil {
		return 0, err
	}
	if _, err := git.NormalizeBranchName(branch); err != nil {
		return 0, err
	}
	if !git.IsValidCommitSHA(initialSHA) {
		return 0, fmt.Errorf("%w: initial_sha %q", git.ErrInvalidBaseSHA, initialSHA)
	}
	if err := diary.Validate(); err != nil {
		return 0, err
	}
	resp, err := c.stub.BeginCheckpoint(ensureCtx(ctx), &BeginCheckpointReq{
		Epoch:      reqEpoch,
		Branch:     branch,
		InitialSHA: initialSHA,
		Diary:      diary,
	})
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, errors.New("node: nil BeginCheckpoint response")
	}
	if resp.StaleEpoch || resp.ErrorCode == errCodeStaleEpoch {
		return 0, checkpoint.ErrStaleEpoch
	}
	if resp.ErrorCode != "" {
		return 0, mapErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return resp.Seq, nil
}

// AppendFinalSHA verifies epoch fencing and records finalSHA onto a pending checkpoint record.
func (c *RemoteNodeControlPlane) AppendFinalSHA(ctx context.Context, reqEpoch, seq uint64, finalSHA string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	if !git.IsValidCommitSHA(finalSHA) {
		return fmt.Errorf("%w: final_sha %q", git.ErrInvalidBaseSHA, finalSHA)
	}
	resp, err := c.stub.AppendFinalSHA(ensureCtx(ctx), &AppendFinalSHAReq{
		Epoch:    reqEpoch,
		Seq:      seq,
		FinalSHA: finalSHA,
	})
	if err != nil {
		return err
	}
	return decodeFencedResp(resp)
}

// CompleteCheckpoint verifies epoch fencing, stores ArtifactsSnapshot, and marks StatusComplete.
func (c *RemoteNodeControlPlane) CompleteCheckpoint(ctx context.Context, reqEpoch, seq uint64, finalSHA string, artifactsSnap *blob.ObjectMeta) error {
	if err := checkCtx(ctx); err != nil {
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
	resp, err := c.stub.CompleteCheckpoint(ensureCtx(ctx), &CompleteCheckpointReq{
		Epoch:             reqEpoch,
		Seq:               seq,
		FinalSHA:          finalSHA,
		ArtifactsSnapshot: artifactsSnap,
	})
	if err != nil {
		return err
	}
	return decodeFencedResp(resp)
}

// ListCheckpoints returns all checkpoint records ordered newest-to-oldest.
func (c *RemoteNodeControlPlane) ListCheckpoints(ctx context.Context) ([]checkpoint.CheckpointRecord, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	resp, err := c.stub.ListCheckpoints(ensureCtx(ctx), &ListCheckpointsReq{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return cloneCheckpointRecords(resp.Records), nil
}

// DrainInterjections atomically drains pending human interjections, translating stale epoch responses to ErrStaleEpoch.
func (c *RemoteNodeControlPlane) DrainInterjections(ctx context.Context) ([]session.HumanInterjection, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	resp, err := c.stub.DrainInterjections(ensureCtx(ctx), &DrainInterjectionsReq{Epoch: c.epoch})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	if resp.StaleEpoch {
		return nil, checkpoint.ErrStaleEpoch
	}
	return resp.Items, nil
}

// HasPendingInterjections reports whether undrained operator interjections are queued.
func (c *RemoteNodeControlPlane) HasPendingInterjections(ctx context.Context) (bool, error) {
	if err := checkCtx(ctx); err != nil {
		return false, err
	}
	resp, err := c.stub.HasPendingInterjections(ensureCtx(ctx), &HasPendingInterjectionsReq{})
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, nil
	}
	return resp.HasPending, nil
}

// SaveQCLedgerSnapshot persists the QC ledger snapshot under strict epoch fencing.
func (c *RemoteNodeControlPlane) SaveQCLedgerSnapshot(ctx context.Context, epoch uint64, snapshot []byte) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	resp, err := c.stub.SaveQCLedgerSnapshot(ensureCtx(ctx), &SaveQCLedgerSnapshotReq{
		Epoch:    epoch,
		Snapshot: append([]byte(nil), snapshot...),
	})
	if err != nil {
		return err
	}
	return decodeFencedResp(resp)
}

// LoadQCLedgerSnapshot loads the saved QC ledger snapshot bytes.
func (c *RemoteNodeControlPlane) LoadQCLedgerSnapshot(ctx context.Context) ([]byte, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	resp, err := c.stub.LoadQCLedgerSnapshot(ensureCtx(ctx), &LoadQCLedgerSnapshotReq{})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Snapshot) == 0 {
		return nil, nil
	}
	return append([]byte(nil), resp.Snapshot...), nil
}

// SavePendingPublication persists version-bound pending publication metadata under strict epoch fencing.
func (c *RemoteNodeControlPlane) SavePendingPublication(ctx context.Context, epoch uint64, pending *runner.PendingPublication) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	resp, err := c.stub.SavePendingPublication(ensureCtx(ctx), &SavePendingPublicationReq{
		Epoch:   epoch,
		Pending: clonePendingPublication(pending),
	})
	if err != nil {
		return err
	}
	return decodeFencedResp(resp)
}

// LoadPendingPublication loads any persisted PendingPublication.
func (c *RemoteNodeControlPlane) LoadPendingPublication(ctx context.Context) (*runner.PendingPublication, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	resp, err := c.stub.LoadPendingPublication(ensureCtx(ctx), &LoadPendingPublicationReq{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return clonePendingPublication(resp.Pending), nil
}

// GetApproval loads the persisted ApprovalRecord, if any.
func (c *RemoteNodeControlPlane) GetApproval(ctx context.Context) (*runner.ApprovalRecord, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	resp, err := c.stub.GetApproval(ensureCtx(ctx), &GetApprovalReq{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return cloneApprovalRecord(resp.Approval), nil
}

func decodeFencedResp(resp *EpochFencedResp) error {
	if resp == nil {
		return errors.New("node: nil actor response")
	}
	if resp.StaleEpoch || resp.ErrorCode == errCodeStaleEpoch {
		return checkpoint.ErrStaleEpoch
	}
	if resp.ErrorCode != "" {
		return mapErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	if !resp.OK {
		return errors.New("node: operation rejected")
	}
	return nil
}

func mapErrorCode(code, msg string) error {
	switch code {
	case errCodeStaleEpoch:
		return checkpoint.ErrStaleEpoch
	case errCodeInvalidBaseSHA:
		return fmt.Errorf("%w: %s", git.ErrInvalidBaseSHA, msg)
	case errCodeProtectedSourceBranch:
		return fmt.Errorf("%w: %s", git.ErrProtectedSourceBranch, msg)
	case errCodeBranchOwnershipConflict:
		return fmt.Errorf("%w: %s", git.ErrBranchOwnershipConflict, msg)
	case errCodeBlobDigestMismatch:
		return fmt.Errorf("%w: %s", blob.ErrDigestMismatch, msg)
	case errCodeBlobInvalidKey:
		return fmt.Errorf("%w: %s", blob.ErrInvalidKey, msg)
	default:
		if msg != "" {
			return errors.New(msg)
		}
		return errors.New("node: remote error " + code)
	}
}

func checkCtx(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func ensureCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
