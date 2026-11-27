# Feature Specification: Dual-Port REST/WebSocket API Server & CSP Preview Gateway (`029-rest-ws-api-server`)

**Feature Branch**: `029-rest-ws-api-server`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `029` blueprint (`docs/stage3/029-rest-ws-api-server.md`), `docs/architecture-contract.md`, `internal/actor/{run,node,config,registry,merge}`, `internal/worker/daemon/daemon.go`, `internal/daprhost/{components,manager}.go`, `internal/plan/{archive,model}.go`, `internal/blob/store.go`, `internal/artifact/service.go`, and `internal/spec/cty.go`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Dual-Port Isolation, Startup Safety, Bearer/Subprotocol Auth, Anti-CSRF Origin Guard, and Scoped Preview Tickets (Priority: P1)

Operators and automation clients connect to `sofa server` over a public HTTP/WebSocket listener (`127.0.0.1:8080` by default), while `daprd` communicates with Control Plane Actors and PubSub callbacks exclusively over a physically separated internal callback listener (`127.0.0.1:<daprAppPort>`).
- **Startup Listen-Address Safety**: Constructing the server fails immediately if `APIToken` (`SOFA_API_TOKEN`) is empty unless `DevMode` is enabled AND `PublicAddr` resolves to a strict loopback IP (`127.0.0.1` or `::1`).
- **Physical Port Isolation**: The public listener exposes only `/healthz`, `/api/v1/*`, and an optional UI fallback handler; any request to `/actors/*` or `/dapr/*` on the public listener MUST return `404 Not Found`. The internal listener enforces `dapr-api-token` (`APP_API_TOKEN`) authentication and mounts Dapr Actor factories, PubSub topic handlers, and optional local dev worker handlers.
- **Bearer & WebSocket Subprotocol Authentication**: When `APIToken` is set, `/api/v1/*` requires constant-time `Authorization: Bearer <token>`, and `/api/v1/ws` accepts either `Authorization: Bearer <token>` (CLI) or `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<base64url-token>` (browser), echoing `sofa.v1` without ever accepting long-lived tokens in URL query parameters.
- **Strict Anti-CSRF `Origin` Guard (Active Even in `--dev` Mode)**: On all mutating `/api/v1/*` requests (`POST`, `PUT`, `PATCH`, `DELETE`) and `GET /api/v1/ws`, if an `Origin` header is present, `Origin: null` and any `Origin` whose `host[:port]` does not match the request `Host` header MUST be rejected with `403 Forbidden`.
- **Short-Lived HMAC-SHA256 Preview Tickets**: `POST /api/v1/runs/{run_id}/artifacts/{artifact_id}/preview-ticket` mints a short-lived HMAC-SHA256 signed ticket scoped strictly to `(run_id, artifact_id)` so `<iframe sandbox="allow-scripts">` can fetch `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content?ticket=...` read-only without exposing `SOFA_API_TOKEN`.

**Why this priority**: Security boundaries (port isolation, startup token enforcement, anti-CSRF `Origin` checks, and sandboxed preview isolation) are non-negotiable runtime invariants under Constitution Principle IV.

**Independent Test**: Tier 1 unit tests in `internal/server/server_test.go` verifying startup rejection on non-loopback/non-dev tokenless configs, `403 Forbidden` on `Origin: null` and `Origin: https://evil.example` even in `--dev` mode, `404 Not Found` on public `/actors/...` and `/dapr/...`, Bearer/WS-subprotocol auth, and preview ticket scoping/expiration.

**Acceptance Scenarios**:

1. **Given** `APIToken` is empty and `PublicAddr` is `0.0.0.0:8080` (with or without `DevMode`), **When** `New` is called, **Then** server construction fails immediately with an error.
2. **Given** `DevMode: true` and `PublicAddr: 127.0.0.1:0` with empty `APIToken`, **When** a client sends `POST /api/v1/secrets` or `POST /api/v1/decisions/c1/respond` or `GET /api/v1/ws` with `Origin: null` or `Origin: https://evil.example`, **Then** the server responds with `403 Forbidden`.
3. **Given** the public server router, **When** a request targets `/actors/ConfigActor/global/method/ResolveForNode` or `/dapr/subscribe`, **Then** it responds with `404 Not Found`.
4. **Given** `APIToken` is configured, **When** a client connects to `/api/v1/ws` using `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<b64url(token)>`, **Then** the upgrade succeeds with `Sec-WebSocket-Protocol: sofa.v1`; mismatched or missing tokens return `401 Unauthorized`.

---

### User Story 2 - `.sofaplan` Submission with Second-0 Preflight Validation, Run Lifecycle, and Sensitive Output Masking (Priority: P1)

Operators submit `.sofaplan` archives via `POST /api/v1/runs` (either `multipart/form-data` with optional `namespace`, `run_id`, JSON `envs`, JSON `secrets`, or `application/octet-stream` with query `?namespace=&run_id=`), inspect Run lists and details, pause/resume execution, and delete Runs with full deferred cleanup.
- **Second-0 Preflight Gate**: Before persisting any Run-scoped config, `BlobStore` object, or `RunActor` state, the server unpacks/validates the `.sofaplan` archive in a temporary directory, extracts required env/secret keys via `config.ExtractPlanEnvRefs(loaded.Plan)`, and invokes `ConfigActor.PreflightCheck`. Missing keys immediately return `422 Unprocessable Entity` with `{"error": "...", "missing_keys": [...]}`.
- **Run Submission**: Once preflight passes, request-scoped `envs`/`secrets` are persisted into `ConfigActor` (`ScopeRun`), the archive is uploaded to `BlobStore` at `runs/<run_id>/plan/plan.sofaplan`, `RunActor.StartRun` is invoked, and `201 Created` returns `{"run_id": "...", "status": "..."}`.
- **Sensitive Output Masking**: `GET /api/v1/runs/{run_id}` decodes `EvaluatedOutput` via `DecodeValue()` and masks any top-level `Sensitive` output or nested `golden.SensitiveMark` attribute/element as `"<sensitive>"` unless `?reveal_sensitive=true` is explicitly requested.
- **Pause, Resume, Delete**: `POST /api/v1/runs/{run_id}/pause` accepts `mode` (`"planned"` | `"immediate"`) from either query `?mode=...` or JSON body `{"mode":"..."}`; `POST /api/v1/runs/{run_id}/resume` resumes paused/failed Runs; `DELETE /api/v1/runs/{run_id}` invokes `RunActor.DeleteRun`.

**Why this priority**: Run submission, second-0 preflight rejection, lifecycle controls, and output masking form the core operational surface shared by `030` CLI and `031` Web Dashboard.

**Independent Test**: Tier 1 unit tests verifying `422` `missing_keys` rejection with zero `BlobStore`/`RunActor` writes, `multipart/form-data` + `application/octet-stream` submission, recursive `"<sensitive>"` masking vs `?reveal_sensitive=true`, query and JSON `pause` modes, `resume`, and `DELETE`.

**Acceptance Scenarios**:

1. **Given** a `.sofaplan` referencing `GEMINI_API_KEY` which is not configured in any tier or request override, **When** `POST /api/v1/runs` is called, **Then** it returns `422 Unprocessable Entity` with `missing_keys: ["GEMINI_API_KEY"]` and creates no `BlobStore` object or `RunActor` record.
2. **Given** a completed Run with a top-level sensitive output and an object output containing a nested `golden.SensitiveMark` field, **When** `GET /api/v1/runs/{run_id}` is called without `?reveal_sensitive=true`, **Then** both the top-level sensitive value and the nested sensitive field are returned as `"<sensitive>"` while non-sensitive sibling fields remain visible; **When** `?reveal_sensitive=true` is passed, **Then** plaintext values are returned.

---

### User Story 3 - Node Inspection, JSONL Log Streaming, Single-Node Rerun, Interjections, Decisions, and Managed Envs/Secrets CRUD (Priority: P1)

Operators inspect individual nodes, stream archived epoch JSONL logs, trigger single-node reruns, inject live diary interjections, respond to human decision cards, and manage global/namespace configuration and write-only secrets.
- **Node Inspection & Logs**: `GET /api/v1/runs/{run_id}/nodes/{node_id}` (URL-decoding `node_id` so bracketed dynamic addresses like `module.a.dynamic_worker.b%5Bk%5D` resolve cleanly) combines `RunActor` summary + `NodeActor.GetState` (checkpoints, diary, interjections, decision cards, artifacts). `GET /api/v1/runs/{run_id}/nodes/{node_id}/logs?epoch=<optional>` streams `daemon.NodeLogBlobKey(run_id, node_addr, epoch)` from `BlobStore`.
- **Single-Node Rerun**: `POST /api/v1/runs/{run_id}/nodes/{node_id}/rerun` calls `RunActor.RerunNode` and translates `run.ErrAlreadyMergedSharedBranch` into `409 Conflict`.
- **Diary Interjections**: `POST /api/v1/runs/{run_id}/nodes/{node_id}/diary-messages` calls `NodeActor.EnqueueInterjection`.
- **Human Decisions**: `GET /api/v1/decisions` (`?run_id=&status=`) aggregates decision cards across runs; `POST /api/v1/decisions/{id}/respond` locates the owning `(run_id, node_addr)`, reads Run manual-pause state, calls `NodeActor.SubmitDecision`, and maps `node.ErrInvalidDecision` conflicts/mismatches cleanly (`409 Conflict` / `400 Bad Request`).
- **Managed Envs & Write-Only Secrets**: `GET/PUT/DELETE /api/v1/envs` and `GET/PUT/DELETE /api/v1/secrets` manage `ConfigActor("global")` across `global` and `namespace` scopes. `GET /api/v1/secrets` NEVER includes a `value` field, and intra-tier kind conflicts (`config.ErrKindConflict`) return `409 Conflict`.

**Why this priority**: Completes the full control-plane operator workflow required for interactive debugging, human-in-the-loop approvals, and secret hygiene.

**Independent Test**: Tier 1 unit tests for URL-decoded dynamic node queries, JSONL log streaming, `409 Conflict` on shared-branch rerun, diary interjection forwarding, decision aggregation & submission, write-only `/api/v1/secrets`, and `409 Conflict` on Env/Secret kind collisions.

**Acceptance Scenarios**:

1. **Given** a `ConfigActor` with `API_KEY` stored as an `Env` in `global` scope, **When** `PUT /api/v1/secrets` tries to set `API_KEY` in `global` scope, **Then** the server returns `409 Conflict`.
2. **Given** secrets stored in `ConfigActor`, **When** `GET /api/v1/secrets` is called, **Then** the JSON response contains metadata (`name`, `scope`, `namespace`, `updated_at`) and strictly omits any `value` field.
3. **Given** a Run node already merged into a shared `target_branch`, **When** `POST /api/v1/runs/{run_id}/nodes/{node_id}/rerun` is called, **Then** the server returns `409 Conflict`.

---

### User Story 4 - Zero-Trust CSP Artifact Preview Gateway and Real-Time WebSocket Event Hub (Priority: P1)

The server streams artifacts under strict browser sandboxing headers and fans out live Dapr PubSub events over WebSockets.
- **Artifact CSP Preview**: `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content` streams the artifact content from `BlobStore` (extracting `"payload"` for `file` artifact zips or `?entry=` for `directory` zips, and supporting raw blobs) with forced headers:
  - `Content-Security-Policy: sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'`
  - `X-Content-Type-Options: nosniff`
- **WebSocket Event Hub**: The internal Dapr callback server subscribes on `daprhost.PubSubComponentName` (`"sofa-pubsub"`) to both `daemon.EventsTopic` (`"sofa-events"`) and `run.DefaultRunEventsTopic` (`"sofa.run.events"`). Incoming events are published to a thread-safe `WSHub` with bounded per-client send queues, supporting both global (`/api/v1/ws`) and run-filtered (`/api/v1/ws?run_id=<id>`) subscribers without slow-consumer blocking.

**Why this priority**: Prevents XSS/credential exfiltration from Agent-generated HTML artifacts and powers real-time CLI/UI progress streaming.

**Independent Test**: Tier 1 unit tests for exact CSP + `X-Content-Type-Options` headers on artifact preview and `WSHub` global vs `run_id`-filtered fan-out; plus Tier 2 integration test (`internal/server/server_integration_test.go`) against real `daprhost.Manager`, `WorkerDaemon`, and bare Git repo exercising `422` preflight -> `PUT /api/v1/secrets` -> `/api/v1/ws` live events -> `POST /api/v1/runs` -> `SUCCEEDED` -> CSP preview -> `DELETE /api/v1/runs/{id}` cleanup.

**Acceptance Scenarios**:

1. **Given** a published HTML artifact in `BlobStore`, **When** `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content` is called (with Bearer token or valid scoped preview ticket), **Then** the response includes the exact `Content-Security-Policy` sandbox header and `X-Content-Type-Options: nosniff`.
2. **Given** two WebSocket clients (one subscribed globally to `/api/v1/ws` and one filtered to `/api/v1/ws?run_id=run-a`), **When** PubSub events arrive for `run-a` and `run-b`, **Then** the global client receives both events while the `run-a` client receives only `run-a` events.

### Edge Cases

- What happens when `PUT /api/v1/envs` or `/api/v1/secrets` receives an invalid key name (failing `config.ValidateKeyName`) or invalid scope (`scope=run`)? Returns `400 Bad Request`.
- What happens when a WebSocket client stalls and fills its bounded send buffer? The hub drops/closes the slow subscriber cleanly without blocking Dapr PubSub acknowledgment or other subscribers.
- What happens when `POST /api/v1/runs` fails after writing Run-scoped config or uploading the `.sofaplan` blob because `RunActor.StartRun` fails? Best-effort rollback deletes the uploaded blob and Run-scoped config entries.
- What happens when `?ticket=` on `/artifacts/{artifact_id}/content` is expired, tampered with, or minted for a different `(run_id, artifact_id)`? Returns `401 Unauthorized`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/server/auth.go` MUST enforce startup listen-address safety (`APIToken` required unless `DevMode && isLoopbackListenAddr(PublicAddr)`), constant-time `Authorization: Bearer` validation on `/api/v1/*`, WebSocket authentication via `Authorization: Bearer` header or `Sec-WebSocket-Protocol` (`sofa.v1, sofa.bearer.<b64url>`), strict anti-CSRF `Origin` validation (rejecting `Origin: null` and host mismatches with `403` even in `--dev` mode) on mutating `/api/v1/*` and `/api/v1/ws`, and HMAC-SHA256 scoped preview tickets (`MintPreviewTicket` / `VerifyPreviewTicket`).
- **FR-002**: `internal/server/server.go` MUST implement dual-port physical isolation: internal Dapr callback server (`127.0.0.1:<daprAppPort>`) protected by `APP_API_TOKEN` mounting `daprhttp.NewServiceWithMux`, Actor factories, PubSub subscriptions, and optional local dev handlers; public server mounting only `/healthz`, `/api/v1/*`, and optional `UIHandler`, returning `404` on `/actors/*` and `/dapr/*`.
- **FR-003**: `internal/server/ws_hub.go` MUST implement a thread-safe WebSocket hub with bounded per-client send buffers, global and `run_id`-filtered subscriptions, clean disconnect cleanup, and Dapr PubSub topic handlers for both `daemon.EventsTopic` (`"sofa-events"`) and `run.DefaultRunEventsTopic` (`"sofa.run.events"`) on `daprhost.PubSubComponentName` (`"sofa-pubsub"`).
- **FR-004**: `internal/server/handlers.go` MUST implement all `/api/v1/*` REST endpoints (`runs`, `pause`, `resume`, `delete`, `nodes`, `logs`, `rerun`, `diary-messages`, `decisions`, `decisions/{id}/respond`, `envs`, `secrets`, `artifacts/{id}/preview-ticket`, `artifacts/{id}/content`, `ws`) backed by clean Go interfaces with default Dapr Actor + `BlobStore` implementations.
- **FR-005**: `internal/server/handlers.go` MUST enforce second-0 `ConfigActor.PreflightCheck` (`422` with `missing_keys`), recursive `"<sensitive>"` masking on `EvaluatedOutput` unless `?reveal_sensitive=true`, write-only `GET /api/v1/secrets`, `409 Conflict` on shared-branch `rerun` and `config.ErrKindConflict`, and forced CSP + `X-Content-Type-Options: nosniff` headers on artifact preview.

### Key Entities *(include if feature involves data)*

- **ServerConfig / Server**: Dual-port HTTP/WebSocket host lifecycle manager separating public management traffic from internal Dapr Actor/PubSub callbacks.
- **PreviewTicket**: Short-lived HMAC-SHA256 signed token binding `(run_id, artifact_id, expires_at)` for sandboxed iframe artifact fetching.
- **WSHub / EventEnvelope**: Thread-safe pubsub-to-WebSocket fan-out broker filtering normalized envelopes by `run_id`.
- **ControlPlaneBackend**: Consumer-defined Go interfaces abstracting `OperatorActor`, `RunActor`, `NodeActor`, `ConfigActor`, and `BlobStore` operations for Tier 1 unit testability and Dapr Tier 2 parity.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go test -v ./internal/server/... -count=1` passes all security, preflight, masking, write-only secrets, conflict, CSP, and WebSocket fan-out unit tests in under 5 seconds with zero external daemons.
- **SC-002**: `go test -v -tags=integration ./internal/server/... -count=1` passes end-to-end against real `daprhost.Manager`, dual-port `server.Server`, `WorkerDaemon`, and local bare Git repo (`422` preflight -> `PUT /api/v1/secrets` -> `/api/v1/ws` live stream -> `POST /api/v1/runs` -> `SUCCEEDED` -> CSP artifact preview -> `DELETE` cleanup) with zero leftover `scheduler`/`daprd` processes.
- **SC-003**: `gofmt -l .` is empty and `go vet ./...` passes cleanly across the entire repository.

## Assumptions

- `RunActor` and `NodeActor` are authoritative for Run/Node state, epoch fencing, and branch/blob cleanup on `DeleteRun`.
- Official `github.com/coder/websocket` handles RFC 6455 WebSocket upgrades and `Sec-WebSocket-Protocol` negotiation cleanly alongside `github.com/go-chi/chi/v5`.
- When `APIToken` is empty in `--dev` loopback mode, preview tickets are unnecessary (or use an ephemeral per-server HMAC secret if explicitly requested) and `?ticket=` is still accepted cleanly when minted.
