# Research: `029-rest-ws-api-server`

## 1. Existing Actor, Daemon & Storage Contracts Verified

- **`OperatorActor` (`internal/actor/run/operator_actor.go`, `types.go`)**:
  - `NewOperatorClientStub(daprClient)` provides `UpsertRun`, `GetRunSummary`, `ListRuns`, `RemoveRun`, `RecoverActiveRuns`.
  - `ListRunsReq` accepts `Namespace` and `Status`; `ListRunsResp.Runs` is sorted newest-first (`CreatedAt` desc, `RunID` asc).
- **`RunActor` (`internal/actor/run/run_actor.go`, `types.go`)**:
  - `NewDaprRunClient(runID, daprClient)` returns `run.RunClient` with lossless translation of `ErrAlreadyMergedSharedBranch`, `ErrRunNotFound`, `ErrNodeNotFound`, `ErrRunConflict`.
  - `StartRunReq` takes `RunID`, `Namespace`, `PlanBlobKey` (default `runs/<run_id>/plan/plan.sofaplan`), `PlanSHA256`, `RunOverrides`, plus optional platform/plugin/rework settings.
  - `PauseRunReq` takes `Mode` (`"planned"` | `"immediate"`) and `Reason`.
  - `GetRunStateResp` includes `Outputs map[string]EvaluatedOutput`, `Nodes []NodeRunSummary`, `Sources`, `Preserves`, `Ledger`, `ManualPaused`, `Status`.
  - `EvaluatedOutput.DecodeValue()` reconstructs `cty.Value` preserving both top-level `EvaluatedOutput.Sensitive` and nested `golden.SensitiveMark` paths (`SensitivePaths`).
  - `RunActor` publishes lifecycle notifications to `DefaultPubSubName = "sofa-pubsub"` and `DefaultRunEventsTopic = "sofa.run.events"`.
- **`NodeActor` (`internal/actor/node/actor.go`, `types.go`)**:
  - `NewClientStubForNode(runID, canonicalNodeAddr, daprClient)` constructs URL-safe encoded `NodeActor` stub.
  - `GetState` returns `GetNodeStateResp` (`Checkpoints` with `Diary`, `PendingInterjections`, `InterjectionLog`, `DecisionCards`, `Outcome`, etc.).
  - `EnqueueInterjection` queues human diary interjections (`Message`, `WaitForConfirmation`).
  - `SubmitDecision` validates `DecisionCard` (including `CommitSHA`/`ArtifactIDs` for `human_approval`), updates `Approval`, honors `RunPaused`, and returns `ErrInvalidDecision` on missing/non-pending/mismatched submissions.
- **`ConfigActor` (`internal/actor/config/{types,actor,plan_refs}.go`)**:
  - `NewClientStub(daprClient)` binds `ConfigActor("global")`.
  - `ExtractPlanEnvRefs(loaded.Plan)` returns sorted deduplicated host env/secret keys required by the plan.
  - `PreflightCheck` validates `RunOverrides > Run > Namespace > Global` at second 0 without returning values.
  - `ListMetadata` returns `Envs []EnvEntry` and `Secrets []SecretMeta` (structurally omits `Value`).
  - `ErrKindConflict` (`"config: key sensitivity kind conflict in same scope"`) is returned by `UpsertEntries` on Env/Secret same-scope collision. Note that across Dapr `InvokeActor` RPC, errors arrive wrapped as gRPC/HTTP strings, so the Dapr client adapter must inspect `strings.Contains(err.Error(), config.ErrKindConflict.Error())` (and `ErrInvalidKeyName`, `ErrInvalidScope`) to restore sentinel errors.
- **`WorkerDaemon` (`internal/worker/daemon/daemon.go`)**:
  - Publishes `LogEvent` JSON payloads to `PubSubComponentName = "sofa-pubsub"`, `EventsTopic = "sofa-events"`.
  - Archives per-epoch JSONL logs to `BlobStore` under `daemon.NodeLogBlobKey(runID, nodeAddr, epoch)` (`runs/<run_id>/logs/<node_key>/e<epoch>.jsonl`).
- **`Artifact Service` (`internal/artifact/service.go`)**:
  - `Publish` stores artifacts at `runs/<run_id>/artifacts/<id>.zip` where `Type == "file"` stores entry `"payload"` and `Type == "directory"` stores `"tree/<relpath>"`.
  - `GET .../artifacts/{artifact_id}/content` should support both real `.zip` artifact bundles (extracting `"payload"` or `"tree/<entry>"`) and direct raw blobs in `BlobStore`, always attaching forced CSP + `X-Content-Type-Options: nosniff` headers.

## 2. WebSocket Library Choice

- **Decision**: Add `github.com/coder/websocket` (`v1.8.14`).
- **Rationale**: Lightweight, idiomatic `context.Context`-aware RFC 6455 implementation with native `AcceptOptions{Subprotocols: []string{"sofa.v1", ...}}` support and no global state or reflection quirks.
- **Alternatives Considered**: `github.com/gorilla/websocket` (older API lacking first-class `context.Context` read/write cancellation).

## 3. Dual-Port & Internal Dapr Callback Design

- **Decision**: `internal/server` owns both a `chi.Mux`-backed public HTTP/WS `http.Server` and an optional `daprhttp.NewServiceWithMux`-backed internal Dapr callback server (`127.0.0.1:<port>`).
- **Rationale**: Physically separates `/api/v1/*` and `/healthz` from `/actors/*`, `/dapr/*`, and PubSub routes so external clients can never invoke Actors directly or bypass `Bearer`/`Origin` middleware.
- **Subprotocol Auth**: Browser clients send `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<base64url-token>`; the server validates the `sofa.bearer.<...>` entry using constant-time comparison and accepts the upgrade with `Subprotocols: []string{"sofa.v1"}` so the browser receives `Sec-WebSocket-Protocol: sofa.v1` and never exposes the bearer token in URL query strings.
- **Preview Ticket**: HMAC-SHA256 over `base64url(JSON{run_id, artifact_id, exp})`, signed with an explicit or derived server secret (`APIToken` when set, or ephemeral random 32-byte key in dev mode), validated in constant time on `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content?ticket=...`.
