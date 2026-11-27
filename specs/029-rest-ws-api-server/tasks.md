# Tasks: Dual-Port REST/WebSocket API Server & CSP Preview Gateway (`029-rest-ws-api-server`)

**Input**: Design documents from `/specs/029-rest-ws-api-server/`
**Prerequisites**: `plan.md`, `spec.md`, `research.md`, `data-model.md`, `contracts/rest-ws-api.md`, `quickstart.md`

## Phase 1: Setup (Dependencies & Package Layout)

- [x] T001 Add `github.com/coder/websocket@v1.8.14` to `go.mod` for RFC 6455 WebSocket upgrades and subprotocol negotiation.
- [x] T002 Create `internal/server/` package skeleton (`auth.go`, `ws_hub.go`, `handlers.go`, `server.go`) aligned with `specs/029-rest-ws-api-server/data-model.md` and `contracts/rest-ws-api.md`.

---

## Phase 2: User Story 1 - Security Boundaries, Dual-Port Isolation, Auth & Origin Guard (Priority: P1)

**Goal**: Enforce startup safety checks, dual-port isolation, Bearer + WS subprotocol auth, always-on anti-CSRF `Origin` validation, and HMAC-SHA256 preview tickets.

**Independent Test**: `go test -v -run TestSecurity ./internal/server/... -count=1`

### Tests for User Story 1 (Mandatory Red Phase) ⚠️

- [x] T003 [US1] Write failing Tier 1 tests in `internal/server/server_test.go` covering: non-loopback/non-dev tokenless startup rejection; `--dev` `Origin: null` and `Origin: https://evil.example` -> `403`; public `/actors/*` and `/dapr/*` -> `404`; Bearer auth and `Sec-WebSocket-Protocol` (`sofa.v1, sofa.bearer.<b64url>`) auth; preview ticket mint/verify/expiry/cross-artifact rejection.

### Implementation for User Story 1 (Green Phase)

- [x] T004 [P] [US1] Implement `internal/server/auth.go`: `isLoopbackListenAddr`, `ValidateListenSecurity`, constant-time `bearerAuthMiddleware`, `extractWebSocketAuth`, `originAntiCSRFMiddleware`, and `previewTicketSigner`.
- [x] T005 [US1] Implement dual-port `Server` wiring in `internal/server/server.go` (`Config`, `New`, `Start`, `Stop`, `PublicHandler`, `InternalHandler`, `APP_API_TOKEN` internal middleware, zero `/actors/*` or `/dapr/*` on public router).

---

## Phase 3: User Story 2 - `.sofaplan` Submission, Second-0 Preflight, Run Lifecycle & Sensitive Masking (Priority: P1)

**Goal**: Support `multipart/form-data` & `application/octet-stream` `.sofaplan` upload with second-0 `PreflightCheck` (`422`), Run lifecycle (`GET`, `pause` query/JSON, `resume`, `DELETE`), and recursive `<sensitive>` masking.

**Independent Test**: `go test -v -run TestRuns ./internal/server/... -count=1`

### Tests for User Story 2 (Mandatory Red Phase) ⚠️

- [x] T006 [US2] Write failing Tier 1 tests in `internal/server/server_test.go` covering: `POST /api/v1/runs` missing secret `422` (`missing_keys`) with zero `BlobStore`/`RunActor` side effects; valid `multipart/form-data` (with Run-scoped `envs`/`secrets`) and `application/octet-stream` -> `201`; `GET /api/v1/runs/{id}` recursive `"<sensitive>"` masking vs `?reveal_sensitive=true`; `pause` (`?mode=immediate` & JSON body), `resume`, and `DELETE`.

### Implementation for User Story 2 (Green Phase)

- [x] T007 [US2] Implement `Backend` interfaces, `DaprBackend`, `.sofaplan` upload + `ExtractPlanEnvRefs` + `PreflightCheck`, recursive `cty.Value` `<sensitive>` masking, and Run lifecycle handlers in `internal/server/handlers.go`.

---

## Phase 4: User Story 3 - Node Inspection, Logs, Rerun, Interjections, Decisions & Envs/Secrets CRUD (Priority: P1)

**Goal**: Implement node details + URL-decoded `node_id`, JSONL log streaming, `409` rerun, diary interjections, decision listing/responding, and global/namespace Env & write-only Secret CRUD.

**Independent Test**: `go test -v -run TestNodes ./internal/server/... -count=1`

### Tests for User Story 3 (Mandatory Red Phase) ⚠️

- [x] T008 [US3] Write failing Tier 1 tests in `internal/server/server_test.go` covering: URL-decoded `node_id` (`module.a.dynamic_worker.b%5Bk%5D`), JSONL log streaming (`?epoch=`), `POST .../rerun` returning `409` on `ErrAlreadyMergedSharedBranch`, `POST .../diary-messages`, `GET /api/v1/decisions` & `POST /api/v1/decisions/{id}/respond` (including `409` on invalid/superseded decision and manual-pause respect), `GET /api/v1/secrets` omitting `value`, and `409` on `ErrKindConflict`.

### Implementation for User Story 3 (Green Phase)

- [x] T009 [US3] Implement Node, Logs, Rerun, Diary-Message, Decision, and Envs/Secrets CRUD handlers in `internal/server/handlers.go`.

---

## Phase 5: User Story 4 - CSP Artifact Preview Gateway, WebSocket PubSub Hub & Tier 2 Integration (Priority: P1)

**Goal**: Stream artifacts with forced CSP sandbox headers, fan out Dapr PubSub `sofa-events` & `sofa.run.events` over `/api/v1/ws`, and verify end-to-end against real `daprhost.Manager`.

**Independent Test**: `go test -v ./internal/server/... -count=1` and `go test -v -tags=integration ./internal/server/... -count=1`

### Tests for User Story 4 (Mandatory Red Phase) ⚠️

- [x] T010 [P] [US4] Write failing Tier 1 tests in `internal/server/server_test.go` for artifact preview CSP + `X-Content-Type-Options: nosniff` headers (both zip payload and raw blob, ticket & Bearer auth) and `WSHub` global vs `run_id`-filtered fan-out + slow-client isolation.
- [x] T011 [P] [US4] Write Tier 2 integration test in `internal/server/server_integration_test.go` (`//go:build integration`) spinning up real `daprhost.Manager`, dual-port `server.Server`, `WorkerDaemon`, and bare Git repo (`422` -> `PUT /api/v1/secrets` -> `/api/v1/ws` -> `POST /api/v1/runs` -> `SUCCEEDED` -> CSP preview -> `DELETE` cleanup).

### Implementation for User Story 4 (Green Phase)

- [x] T012 [US4] Implement `internal/server/ws_hub.go` (`WSHub`, bounded channels, `run_id` filtering, `HandlePubSubEvent`, Dapr topic subscriptions for `sofa-events` and `sofa.run.events`).
- [x] T013 [US4] Implement `handleMintPreviewTicket` and `handleArtifactContent` (with forced CSP + `X-Content-Type-Options: nosniff` headers) and `/api/v1/ws` handler in `internal/server/handlers.go`.

---

## Phase 6: Polish & Quality Gates

- [x] T014 Run `go mod tidy`, `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, `go test -v -tags=integration ./internal/server/... -count=1`, and confirm zero orphan `scheduler`/`daprd` processes remain.
