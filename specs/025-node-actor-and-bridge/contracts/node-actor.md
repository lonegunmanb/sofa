# Contract: `NodeActor` RPCs & `RemoteNodeControlPlane` (`025-node-actor-and-bridge`)

## 1. Actor Identity & Type

- `ActorType`: `"NodeActor"`
- `EncodeNodeActorID(runID, canonicalNodeAddr string) (string, error)`
- `DecodeNodeActorID(actorID string) (runID, canonicalNodeAddr string, err error)`

## 2. Lifecycle & Orchestration RPCs (`func(context.Context, *Req) (*Resp, error)`)

| Method | Request | Response | Key Semantics |
| :--- | :--- | :--- | :--- |
| `StartOrResume` | `*StartOrResumeReq` | `*StartOrResumeResp` | Persists `DispatchSpec`, resolves env + Git auth via `ConfigResolver`, claims slot; returns `QUEUED_SLOT` + reminder if full, or `RUNNING` after `StartNode`. |
| `OnWorkerDead` | `*registry.WorkerDeadNotification` | `*OnWorkerDeadResp` | If `RUNNING` and matches active `(LeaseID, WorkerProcessID)`, `CurrentEpoch++`, clears lease, re-enters slot dispatch. |
| `PauseNode` | `*PauseNodeReq` | `*PauseNodeResp` | Sets `ManualPaused=true`; `immediate` increments `CurrentEpoch++`, aborts active worker, releases slot, sets `PAUSED`. |
| `AdvanceForRework` | `*AdvanceForReworkReq` | `*AdvanceForReworkResp` | Increments `CurrentEpoch++`, aborts active worker, updates `BaseSHA`/`BasedSource`, clears `PendingPublication`/`Approval`, clears `Checkpoints` if `BaseSHA` changed or requested, injects rework reason interjection. |
| `ResetForRerun` | `*ResetForRerunReq` | `*ResetForRerunResp` | Increments `CurrentEpoch++`, aborts active worker, resets state to `PENDING`, clears checkpoints/publication/approval/QC snapshot, invalidates pending cards. |
| `ReportNodeOutcome` | `*daemon.NodeOutcomeReport` | `*ReportNodeOutcomeResp` | Epoch-fenced (`StaleEpoch: true` on mismatch); ignores `Aborted: true`; releases slot; transitions state and creates `DecisionCard` on `WAITING_HUMAN`, invalidating mismatched `ApprovalRecord`. |
| `SubmitDecision` | `*SubmitDecisionReq` | `*SubmitDecisionResp` | Validates pending card and `(CommitSHA, ArtifactIDs)` match; records `ApprovalRecord` or high-priority interjection; auto-resumes (`epoch++`) ONLY if not manually paused. |
| `GetState` | `*GetNodeStateReq` | `*GetNodeStateResp` | Returns full node snapshot (epoch, status, checkpoints, interjections, decision cards, pending publication, approval, outcome). |

## 3. Epoch-Fenced `NodeControlPlane` RPCs

All mutating control-plane RPCs return Go `err == nil` with `StaleEpoch: true` when `req.Epoch != CurrentEpoch`, and structured `ErrorCode` for validation errors, so `RemoteNodeControlPlane` restores `checkpoint.ErrStaleEpoch` and sentinel errors over Dapr Actor RPC:

- `RegisterCheckpointBranch(*RegisterCheckpointBranchReq) (*EpochFencedResp, error)` — verifies epoch + calls `RunNotifier.RegisterManagedBranch(..., KindCheckpointBranch)`.
- `RegisterWorkBranch(*RegisterWorkBranchReq) (*EpochFencedResp, error)` — verifies epoch + calls `RunNotifier.RegisterManagedBranch(..., KindWorkBranch)`.
- `BeginCheckpoint(*BeginCheckpointReq) (*BeginCheckpointResp, error)`
- `AppendFinalSHA(*AppendFinalSHAReq) (*EpochFencedResp, error)`
- `CompleteCheckpoint(*CompleteCheckpointReq) (*EpochFencedResp, error)`
- `ListCheckpoints(*ListCheckpointsReq) (*ListCheckpointsResp, error)`
- `EnqueueInterjection(*EnqueueInterjectionReq) (*EnqueueInterjectionResp, error)`
- `DrainInterjections(*DrainInterjectionsReq) (*DrainInterjectionsResp, error)`
- `HasPendingInterjections(*HasPendingInterjectionsReq) (*HasPendingInterjectionsResp, error)`
- `SaveQCLedgerSnapshot(*SaveQCLedgerSnapshotReq) (*EpochFencedResp, error)`
- `LoadQCLedgerSnapshot(*LoadQCLedgerSnapshotReq) (*LoadQCLedgerSnapshotResp, error)`
- `SavePendingPublication(*SavePendingPublicationReq) (*EpochFencedResp, error)`
- `LoadPendingPublication(*LoadPendingPublicationReq) (*LoadPendingPublicationResp, error)`
- `GetApproval(*GetApprovalReq) (*GetApprovalResp, error)`

## 4. `RemoteNodeControlPlane` Mapping

- Compile-time check: `var _ runner.NodeControlPlane = (*RemoteNodeControlPlane)(nil)`
- `resp.StaleEpoch == true` -> `checkpoint.ErrStaleEpoch`
- `resp.ErrorCode`:
  - `"STALE_EPOCH"` -> `checkpoint.ErrStaleEpoch`
  - `"INVALID_BASE_SHA"` -> `fmt.Errorf("%w: %s", git.ErrInvalidBaseSHA, resp.ErrorMessage)`
  - `"PROTECTED_SOURCE_BRANCH"` -> `fmt.Errorf("%w: %s", git.ErrProtectedSourceBranch, resp.ErrorMessage)`
  - `"BRANCH_OWNERSHIP_CONFLICT"` -> `fmt.Errorf("%w: %s", git.ErrBranchOwnershipConflict, resp.ErrorMessage)`
  - `"BLOB_DIGEST_MISMATCH"` -> `fmt.Errorf("%w: %s", blob.ErrDigestMismatch, resp.ErrorMessage)`
  - `"BLOB_INVALID_KEY"` -> `fmt.Errorf("%w: %s", blob.ErrInvalidKey, resp.ErrorMessage)`
