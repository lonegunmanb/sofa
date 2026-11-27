# Specification Quality Checklist: `ProcessRegistryActor` & `WorkerDaemon`

- [x] `ProcessRegistryActor` (`ActorType = "ProcessRegistryActor"`, `GlobalActorID = "global"`) exports `DefaultWorkerMaxConcurrency = 10`, `DefaultDevMaxConcurrency = 2`, and `StatusHealthy`/`StatusSuspect`/`StatusDead`
- [x] All 6 Actor RPC methods use single-pointer signatures and are exposed via `NewClientStub` (`ImplActorClientStub`)
- [x] `ReminderCall` explicitly persists state via `Set` + `Save` (accounting for `dapr/go-sdk` `InvokeReminder` not auto-saving)
- [x] `ClaimSlot` implements `LeaseKey` idempotency, `StatusHealthy` + `Arch` filtering, lowest utilization ratio selection, and non-blocking `Granted: false`
- [x] `Heartbeat` returns `StatusDead` / `FenceRequired: true` for unknown or dead `ProcessID` without resurrecting
- [x] `WorkerDaemon` mounts `ping`, `start_node`, `abort_node`, `run_merge_candidate`, isolates `LookupEnv` to `ResolvedEnv`, streams JSONL to PubSub + `BlobStore`, and self-fences + rotates `ProcessID` on `DEAD`
- [x] `daprhost` supports optional `NameResolutionDBPath` (`nameresolution` `sqlite`) for stable Windows multi-`daprd` local invocation
- [x] Tier 1 unit tests and Tier 2 multi-`daprd` + Dapr Reminder failover integration tests (`//go:build integration`) are specified
