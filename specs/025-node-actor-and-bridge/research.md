# Research: `NodeActor` & `RemoteNodeControlPlane` (`025-node-actor-and-bridge`)

## Findings from Upstream & Codebase Inspection

1. **`dapr/go-sdk@v1.16.0` Error Swallowing on `InvokeActor` / `ImplActorClientStub`**:
   - When an Actor server method returns a non-nil Go `error`, the Dapr runtime wraps the HTTP/gRPC failure into `ErrActorInvokeFailed`, discarding the original Go error type and message.
   - **Decision**: All epoch-fenced and domain-validated `NodeActor` methods return `(resp, nil)` for domain rejections (`StaleEpoch: true`, plus `ErrorCode` / `ErrorMessage`) and only return Go `error` for infrastructure/state-store or nil-request failures. `RemoteNodeControlPlane` inspects `resp.StaleEpoch` and `resp.ErrorCode` to restore `checkpoint.ErrStaleEpoch`, `git.ErrInvalidBaseSHA`, `git.ErrProtectedSourceBranch`, `git.ErrBranchOwnershipConflict`, `blob.ErrDigestMismatch`, and `blob.ErrInvalidKey`.

2. **`dapr/go-sdk@v1.16.0` `ReminderCall` Non-Auto-Save**:
   - Verified in `internal/actor/registry/actor.go` and `dapr/go-sdk/actor`: `InvokeReminder` does NOT automatically call `SaveState` after `ReminderCall` returns.
   - **Decision**: `NodeActor.ReminderCall` explicitly calls `saveState` (`sm.Set` + `sm.Save`) on every state mutation and releases `NodeActor.mu` before making external `ResolveForNode`, `ClaimSlot`, `StartNode`, `NotifyNodeEvent`, or reminder API calls.

3. **`dapr/go-sdk@v1.16.0` `ImplActorClientStub` Signature Rules**:
   - Inspecting `client/actor.go`, `ImplActorClientStub` requires struct fields typed as `func(context.Context, *Req) (*Resp, error)` (or `error`) with exported field names matching the server's exported method names.
   - **Decision**: Every `NodeActor` RPC uses a single pointer request `*Req` and single pointer response `(*Resp, error)`, and `ClientStub` mirrors them 1:1.

4. **`WorkerRunner` Interaction with `NodeControlPlane` (`internal/worker/runner/worker_runner.go`)**:
   - `WorkerRunner.Run` calls:
     - `RecoverLatestAligned` -> `ListCheckpoints`, `RegisterCheckpointBranch`
     - `LoadQCLedgerSnapshot`, `SaveQCLedgerSnapshot`
     - `LoadPendingPublication`, `SavePendingPublication`, `HasPendingInterjections`
     - `GateController` -> `DrainInterjections`, `SaveCheckpoint` (`RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, `CompleteCheckpoint`)
     - `handleApprovalAndPublish` -> `GetApproval`, `SavePendingPublication`, `RegisterWorkBranch`
   - When `PendingPublication` exists and `HumanApproval == "required"` on recovery (`epoch+1`), `WorkerRunner` short-circuits directly to `handleApprovalAndPublish` ONLY if `pendingPub.CommitSHA == recRes.Record.FinalSHA` AND `!HasPendingInterjections`! If human answered a question/feedback (injecting an interjection) or rejected approval with instructions, `HasPendingInterjections` is `true`, so `WorkerRunner` clears `PendingPublication` and enters the Work turn loop with the human's interjection! If human approved (`Approved == true` matching `(CommitSHA, ArtifactIDs)`), `HasPendingInterjections` is `false`, so `WorkerRunner` short-circuits directly to `RegisterWorkBranch` + `PushCommit` + `StatusSucceeded` without even launching a Work container!
   - **Decision**: For `DecisionKindHumanApproval` with `Approved == true` or `ReturnedUpstream == true`, `SubmitDecision` writes `ApprovalRecord` and does NOT enqueue a `HumanInterjection`, allowing `WorkerRunner`'s fast short-circuit on `epoch+1`! For `DecisionKindRequestDecision`, `DecisionKindPauseForHuman`, or `human_approval` rejection with feedback (`Approved == false && !ReturnedUpstream`), `SubmitDecision` enqueues a high-priority `HumanInterjection` so `WorkerRunner` runs a new Work turn with the answer!

5. **Git Credential Materialization (`internal/git/cli.go` & `internal/worker/daemon/daemon.go`)**:
   - `git.CLIAdapter.buildAuthEnv` resolves `*auth.TokenRef` / `*auth.SSHKeyRef` via `CLIAdapter.LookupEnv` (which in `WorkerDaemon` is `makeIsolatedLookupEnv(req.ResolvedEnv)`).
   - **Decision**: `StartOrResumeReq` accepts `RequiredEnvKeys []string` plus optional `TokenRef *string` / `SSHKeyRef *string` (and `Auth git.AuthConfig`). `NodeActor` includes any referenced `TokenRef`/`SSHKeyRef` key names in `ConfigResolver.ResolveForNode`, populates `StartNodeRequest.ResolvedEnv` with all resolved plaintext values, and populates `StartNodeRequest.Auth` (`git.AuthConfig`) with the corresponding `TokenRef`/`SSHKeyRef`.

6. **Cross-Epoch BaseSHA Upgrade (`AdvanceForRework` / `ResetForRerun`)**:
   - `WorkerRunner` recovers from `ListCheckpoints` whenever `RecoverLatestAligned` finds an aligned checkpoint, ignoring `req.BaseSHA`.
   - **Decision**: When `AdvanceForRework` updates `BaseSHA` (e.g. `MergeReworkSource` upgrading to `LatestTargetSHA`) or `ClearCheckpoints` is requested, or when `ResetForRerun` runs, `NodeActor` clears `Checkpoints` (while preserving monotonic `NextCheckpointSeq`) so the next epoch starts cleanly from the new `BaseSHA`/`BasedSource` with the rework reason injected as a high-priority interjection.
