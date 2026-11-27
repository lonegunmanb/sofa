# Data Model: `NodeActor` (`025-node-actor-and-bridge`)

## 1. Actor Addressing

- **ActorType**: `"NodeActor"`
- **ActorID**: `na-<sanitized_run_id>-<sanitized_node_addr>__<base64_rawurl(len(runID) + ":" + runID + canonicalNodeAddr)>`
  - Character set strictly limited to `[A-Za-z0-9_-]`.
  - Prefix uses `[A-Za-z0-9-]` only (never `_`), guaranteeing `"__"` uniquely separates the readable prefix from the lossless `base64.RawURLEncoding` payload.

## 2. Node Status State Machine (`NodeStatus`)

- `PENDING`: Initialized or reset for rework/rerun, awaiting dispatch.
- `QUEUED_SLOT`: Waiting for an available worker slot (`ClaimSlot` returned `Granted: false` or `StartNode` failed and reported suspect); retry driven by Dapr reminder.
- `RUNNING`: Slot leased (`LeaseID`, `WorkerProcessID`, `WorkerAppID`) and `start_node` accepted at `CurrentEpoch`.
- `WAITING_HUMAN`: Execution paused awaiting human response on an active `PENDING` `DecisionCard`.
- `SUCCEEDED`: Terminal node completion with `WorkBranch`, `CommitSHA`, `ResultJSON`, and `Artifacts`.
- `FALLBACK_TRIGGERED`: Node escalated or exhausted gatekeeper/QC or was returned upstream by human to a declared fallback target.
- `FAILED`: Execution failed (or missing required credentials at dispatch).
- `PAUSED`: Manually paused (`ManualPaused == true`); never dispatches containers until resumed.

## 3. Core Persisted State (`persistedNodeState` under key `"node_actor_v1"`)

- `RunID string`, `NodeAddr string`
- `CurrentEpoch uint64` (starts at `1`, monotonically increments on `OnWorkerDead`, `PauseNode("immediate")`, `AdvanceForRework`, `ResetForRerun`, `SubmitDecision` auto-resume, and `StartOrResume` from `WAITING_HUMAN`/`PAUSED`/`FAILED`)
- `Status NodeStatus`, `ManualPaused bool`
- `LeaseID`, `LeaseKey`, `WorkerProcessID`, `WorkerAppID`
- `Dispatch *DispatchSpec` (latest persisted dispatch config for `ReminderCall`, `OnWorkerDead`, and `SubmitDecision` auto-resume)
- `NextCheckpointSeq uint64`
- `Checkpoints []checkpoint.CheckpointRecord`
- `RegisteredCheckpointBranches map[string]registeredBranchRecord`
- `RegisteredWorkBranches map[string]registeredBranchRecord`
- `PendingInterjections []session.HumanInterjection`
- `InterjectionLog []InterjectionRecord` (`ID`, `Message`, `WaitForConfirmation`, `Status`: `PENDING` | `DRAINED` | `AWAITING_CONFIRMATION` | `CONFIRMED`, timestamps)
- `QCLedgerSnapshot []byte`
- `PendingPublication *runner.PendingPublication`
- `Approval *runner.ApprovalRecord`
- `DecisionCards []DecisionCard`
- `Outcome *runner.NodeOutcome`
- `LastError string`

## 4. `DecisionCard`

- `CardID string`
- `RunID string`, `NodeAddr string`, `Epoch uint64`
- `Kind DecisionCardKind`: `"request_decision"` | `"pause_for_human"` | `"human_approval"`
- `Status DecisionCardStatus`: `"PENDING"` | `"ANSWERED"` | `"INVALIDATED"`
- Question/Context/Options/Recommended (for `request_decision`), `PauseReason` (for `pause_for_human`), `CommitSHA` / `ArtifactIDs` / `Artifacts` / `ResultJSON` (for `human_approval`)
- Operator response fields: `SelectedOption`, `AnswerText`, `Approved`, `ReturnedUpstream`, `ReturnFallback`, `InvalidatedReason`, `CreatedAt`, `UpdatedAt`, `AnsweredAt`.
