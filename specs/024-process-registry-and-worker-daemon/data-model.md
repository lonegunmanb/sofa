# Data Model: `internal/actor/registry` & `internal/worker/daemon` (`024-process-registry-and-worker-daemon`)

## 1. `ProcessRegistryActor` Constants & Enums

| Constant / Type | Value | Notes |
| :--- | :--- | :--- |
| `ActorType` | `"ProcessRegistryActor"` | Dapr Actor Type |
| `GlobalActorID` | `"global"` | Singleton Actor ID |
| `DefaultWorkerMaxConcurrency` | `10` | Standalone `sofa worker` default slots |
| `DefaultDevMaxConcurrency` | `2` | Co-located `sofa server --dev` default slots |
| `DefaultHeartbeatInterval` | `5s` | Worker heartbeat interval |
| `DefaultSuspectTimeout` | `15s` | `HEALTHY` -> `SUSPECT` silence threshold |
| `DefaultProbeInterval` | `3s` | `ReminderCall` active probe period |
| `DefaultDeadThreshold` | `3` | Consecutive failed `ping`s before `DEAD` |
| `StatusHealthy` | `"HEALTHY"` | Eligible for `ClaimSlot` |
| `StatusSuspect` | `"SUSPECT"` | Paused from new `ClaimSlot`; in-flight leases retained; actively probed |
| `StatusDead` | `"DEAD"` | Terminal for `ProcessID`; leases purged; `OnWorkerDead` sent |

## 2. Persisted `ProcessRegistryActor` State (`process_registry_v1`)

```go
type persistedRegistryState struct {
    Processes map[string]ProcessRecord `json:"processes,omitempty"` // keyed by ProcessID
    Leases    map[string]SlotLease     `json:"leases,omitempty"`    // keyed by LeaseID
    LeaseKeys map[string]string        `json:"lease_keys,omitempty"`// LeaseKey -> LeaseID
}
```

### State Transitions & Invariants

1. **Worker Lifecycle (`ProcessID`)**:
   - `RegisterProcess`: New `ProcessID` enters `StatusHealthy` with `LastHeartbeatAt = now`. Previously `DEAD` `ProcessID`s are rejected (no resurrection).
   - `Heartbeat`: If missing or `StatusDead`, returns `{Status: StatusDead, FenceRequired: true}`. If `HEALTHY` or `SUSPECT`, refreshes `LastHeartbeatAt = now`, sets `StatusHealthy`, resets `ConsecutiveFailures = 0`.
   - `ReportSuspect`: Transitions `HEALTHY` -> `SUSPECT` immediately (`DEAD` remains `DEAD`).
   - `ReminderCall`:
     - Any `HEALTHY` process with `now.Sub(LastHeartbeatAt) >= SuspectTimeout` becomes `SUSPECT`.
     - Every `SUSPECT` process is probed via `ping(AppID, ProcessID)`:
       - On success: `StatusHealthy`, `ConsecutiveFailures = 0`, `LastHeartbeatAt = now`.
       - On failure (if not refreshed by concurrent `Heartbeat` while probe was in flight): `ConsecutiveFailures++`; if `ConsecutiveFailures >= DeadThreshold`, transitions to `StatusDead`, deletes all its leases, sets `ActiveSlots = 0`, and emits `WorkerDeadNotification` to each lease owner.
2. **Slot Leases (`LeaseKey` & `LeaseID`)**:
   - `ClaimSlot`: Idempotent by `LeaseKey` while the owning worker is non-`DEAD`. Otherwise selects `StatusHealthy` worker with matching `Arch` (if specified), `ActiveSlots < MaxConcurrency`, and minimum `ActiveSlots / MaxConcurrency` ratio.
   - `ReleaseSlot`: Idempotent by `LeaseID` or `LeaseKey`; decrements `ActiveSlots` (floored at 0) and removes lease indices.

## 3. `WorkerDaemon` Runtime State & Log Pipeline

- **Identity**: `processID` (UUID, rotated on self-fence), `appID` (`sofa-worker-<process_id>` or fixed `--dev` AppID), `arch`, `maxConcurrency`.
- **In-Flight Tables**:
  - `nodeTasks map[string]context.CancelFunc` keyed by `runID + "\x00" + nodeAddr + "\x00" + strconv.FormatUint(epoch, 10)`
  - `mergeTasks map[string]context.CancelFunc` keyed by `requestID`
- **Log Pipeline**:
  - Publishes `LogEvent` JSON to Dapr PubSub (`sofa-pubsub` / `sofa-events`)
  - Appends JSONL lines to a bounded temp file and uploads via `BlobStore.Put` to `runs/<sanitized_run_id>/logs/<node_key>/e<epoch>.jsonl` on `sofa_checkpoint` tool completions and on node exit.
