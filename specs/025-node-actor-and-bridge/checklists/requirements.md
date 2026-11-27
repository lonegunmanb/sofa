# Requirements Quality Checklist: `025-node-actor-and-bridge`

- [x] Actor Type `"NodeActor"` and URL-safe lossless `EncodeNodeActorID` / `DecodeNodeActorID` (`[A-Za-z0-9_-]`) specified.
- [x] Lossless `ErrStaleEpoch` propagation across `dapr/go-sdk` `InvokeActor` via `StaleEpoch: true` + `nil` Go error specified.
- [x] Explicit `Set` + `Save` persistence in `ReminderCall` and lock-free external RPC execution specified.
- [x] Git credential materialization (`TokenRef` / `SSHKeyRef` -> `ResolvedEnv` + `StartNodeRequest.Auth`) specified.
- [x] All three `DecisionCard` forms (`request_decision`, `pause_for_human`, `human_approval`), `(CommitSHA, ArtifactIDs)` exact binding/invalidation, and manual-pause guard in `SubmitDecision` specified.
- [x] `AdvanceForRework` and `ResetForRerun` aligned with `028` (`RunActor`) and `029` (`API Server`).
- [x] Strict TDD task ordering (Red -> Green -> Refactor) documented in `tasks.md`.
