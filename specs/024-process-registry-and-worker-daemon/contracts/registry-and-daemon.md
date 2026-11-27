# Contract: `internal/actor/registry` & `internal/worker/daemon`

## 1. `internal/actor/registry`

```go
package registry

const (
	ActorType                   = "ProcessRegistryActor"
	GlobalActorID               = "global"
	DefaultWorkerMaxConcurrency = 10
	DefaultDevMaxConcurrency    = 2
	DefaultHeartbeatInterval    = 5 * time.Second
	DefaultSuspectTimeout       = 15 * time.Second
	DefaultProbeInterval        = 3 * time.Second
	DefaultDeadThreshold        = 3
)

type ProcessStatus string

const (
	StatusHealthy ProcessStatus = "HEALTHY"
	StatusSuspect ProcessStatus = "SUSPECT"
	StatusDead    ProcessStatus = "DEAD"
)
```

Actor RPC methods (all `func(context.Context, *Req) (*Resp, error)`):
- `RegisterProcess(context.Context, *RegisterProcessReq) (*RegisterProcessResp, error)`
- `Heartbeat(context.Context, *HeartbeatReq) (*HeartbeatResp, error)`
- `ReportSuspect(context.Context, *ReportSuspectReq) (*ReportSuspectResp, error)`
- `ClaimSlot(context.Context, *ClaimSlotReq) (*ClaimSlotResp, error)`
- `ReleaseSlot(context.Context, *ReleaseSlotReq) (*ReleaseSlotResp, error)`
- `ListProcesses(context.Context, *ListProcessesReq) (*ListProcessesResp, error)`

Reminder & Dead Notification Contracts:
- `ProcessRegistryActor` implements `actor.ReminderCallee` (`ReminderCall(name string, state []byte, dueTime string, period string)`) and explicitly persists state (`sm.Set` + `sm.Save`).
- `DeadNotifier` invokes `OnWorkerDead` on `(OwnerActorType, OwnerActorID)` with `WorkerDeadNotification{LeaseID, LeaseKey, ProcessID, AppID, Reason}`.
- `NewClientStub(daprclient.Client) *ClientStub` wires all 6 actor methods via `ImplActorClientStub`.

## 2. `internal/worker/daemon`

Dapr Service Invocation routes registered via `(*WorkerDaemon).RegisterHandlers(svc common.Service) error`:
- `ping`: `PingRequest{ProcessID}` -> `PingResponse{OK, ProcessID, AppID, ActiveTasks}` (errors if `req.ProcessID != ""` and mismatches active `ProcessID`).
- `start_node`: `StartNodeRequest` -> `StartNodeResponse{Accepted, AlreadyRunning, ProcessID}` (non-blocking async dispatch; `LookupEnv` strictly backed by `ResolvedEnv` map only; `NodeOutcomeReport` carries `Aborted: true` when cancelled by `abort_node`).
- `abort_node`: `AbortNodeRequest{RunID, NodeAddr, Epoch}` -> `AbortNodeResponse{Aborted}` (idempotent cancellation).
- `run_merge_candidate`: `RunMergeCandidateRequest` -> `RunMergeCandidateResponse{Accepted, AlreadyRunning, ProcessID}` (non-blocking async dispatch; `MergeOutcomeReport` carries `Aborted: true` when cancelled by `abort_merge`; idempotently releases slot).
- `abort_merge`: `AbortMergeRequest{RequestID}` -> `AbortMergeResponse{Aborted}` (idempotent cancellation of in-flight merge candidate).

PubSub & BlobStore:
- PubSub component `"sofa-pubsub"`, topic `"sofa-events"`.
- BlobStore log key: `runs/<sanitized_run_id>/logs/<node_key>/e<epoch>.jsonl` (guaranteed valid under `blob.ValidateKey`).
