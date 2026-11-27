# Feature Specification: Vue 3 Security Dashboard & Single-Binary `go:embed` Human Decision Workbench (`031-vue3-dashboard`)

**Feature Branch**: `031-vue3-dashboard`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `031` blueprint (`docs/stage3/031-vue3-dashboard.md`), `docs/stage3/README.md`, `docs/architecture-contract.md`, `internal/server/`, `internal/cli/server_cmd.go`, `internal/actor/run/types.go`, `internal/actor/node/types.go`, `internal/actor/config/types.go`, and `internal/worker/daemon/daemon.go`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Single-Binary `go:embed` Serving & Zero-Block Unbuilt Fallback (Priority: P1)

Operators start `sofa server --dev` or `sofa server` and access the Vue 3 workbench on the same public listener port without deploying a separate static web server, while Go builds and unit tests (`go test ./...`) never require `node_modules` or a prebuilt frontend bundle to succeed.
- **`internal/webui` Handler**: Exposes `NewHandler() http.Handler` (backed by `//go:embed all:dist`, with tracked `internal/webui/dist/.keep`) and `NewHandlerFromFS(distFS fs.FS) http.Handler`.
- **Reserved Prefix Guard**: Never intercepts `/api`, `/api/*`, `/actors/*`, `/dapr/*`, or `/healthz` (returning `404` if invoked directly).
- **Unbuilt Fallback Mode**: When `index.html` is absent in `distFS`, serves a clean built-in HTML status page (`200 OK`, `Content-Type: text/html; charset=utf-8`) explaining the build command (`go run ./scripts/build-webui.go`).
- **Built SPA Mode**: Serves existing static files in `distFS` with accurate MIME types (`application/javascript`, `text/css`, etc.), returns `404` for missing static asset extensions (`.js`, `.css`, `.map`, `.png`, `.svg`, `.ico`, `.woff2`), and falls back to `index.html` (`200 OK`, `Content-Type: text/html; charset=utf-8`) for SPA History routes (`/`, `/runs`, `/runs/run-123`, `/decisions`, `/settings`).
- **CLI Default Wiring & Build Script**: `internal/cli/server_cmd.go` passes `webui.NewHandler()` when `opts.UIHandler == nil` in both `runDevServer` and `runDistributedServer`. `scripts/build-webui.go` builds `web/` via `npm.cmd --prefix web run build` (Windows) / `npm --prefix web run build` (Unix) and atomically syncs `web/dist/` into `internal/webui/dist/` while preserving `.keep`.

**Independent Test**: Tier 1 Go tests in `internal/webui/handler_test.go` covering unbuilt fallback `distFS`, built SPA `distFS` (`/runs/123` History fallback, static MIME types, missing `.js` 404, reserved `/api/v1/unknown` / `/actors/foo` / `/dapr/config` 404), and `NewHandler()` smoke check.

---

### User Story 2 - Live Runs List, DAG Topology & Node Inspector with Real-Time WebSocket Updates (Priority: P1)

Operators inspect all workflow Runs, filter by status, trigger Run-level lifecycle controls (`Pause (Planned)`, `Pause (Immediate)`, `Resume`, `Delete`), explore the directed node graph (static `worker`, expanded `dynamic_worker` instance cards, `merge`, and `module` prefixes), and open the Node Inspector drawer to view epoch history, git branch/commit SHAs, checkpoints, structured diary timeline, incremental live + historical JSONL logs, and published artifacts, plus submit single-node `Rerun` and live human diary interjections (`wait_for_confirmation`).
- **API & WebSocket Client (`web/src/api/client.ts`)**: Stores `SOFA_API_TOKEN` exclusively in `window.sessionStorage` (never `localStorage` or cookies), sends `Authorization: Bearer <token>` on `/api/v1/*` requests, connects `/api/v1/ws` using WebSocket subprotocols `['sofa.v1', 'sofa.bearer.' + base64UrlEncode(token)]` when a token is set (or `['sofa.v1']` when empty in `--dev` mode), and updates Run/Node status, epoch badges, and live `sofa-events` log entries immediately.
- **Run Outputs Panel**: Displays Run outputs with sensitive values masked by default and an explicit `Reveal Sensitive` toggle calling `GET /api/v1/runs/{run_id}?reveal_sensitive=true`.

**Independent Test**: Vitest + Vue Test Utils tests in `web/src/__tests__/run-dag-ws.spec.ts` verifying `Sec-WebSocket-Protocol` `sofa.v1, sofa.bearer.<base64url>` negotiation, live node status/epoch badge updates on WebSocket `sofa.run.events` frames, and incremental `sofa-events` log appending in the Node Inspector drawer.

---

### User Story 3 - Zero-Trust Sandboxed HTML Preview, Version-Bound Human Decisions & Write-Only Secrets (Priority: P1)

Operators safely preview untrusted Agent-generated HTML artifacts inside an isolated sandboxed `iframe`, review and act on pending human decisions (including `(commit_sha, artifact_ids)`-bound `human_approval`), and manage three-tier `Env` / write-only `Secret` entries.
- **Untrusted HTML Sandbox Preview (`web/src/components/ArtifactSandboxPreview.vue`)**: Mints a scoped preview ticket via `POST /api/v1/runs/{run_id}/artifacts/{artifact_id}/preview-ticket`, builds `/api/v1/runs/{run_id}/artifacts/{artifact_id}/content?ticket=<ticket>`, and renders strictly `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer" :src="previewUrl">` — **NEVER** including `allow-same-origin`, `allow-top-navigation`, `allow-popups`, or `allow-forms`.
- **Human Decisions Center (`DecisionsView.vue` & `DecisionCardItem.vue`)**: Lists `GET /api/v1/decisions?status=PENDING`, supports option selection (`selected_option`) and free-text replies (`answer_text`), and for `kind === "human_approval"` explicitly displays the locked `commit_sha` and `artifact_ids` fingerprint with one-click **Approve** (`{ run_id, node_addr, approved: true, commit_sha, artifact_ids, answer_text }`) and **Return Upstream Fallback** (`{ run_id, node_addr, approved: false, returned_upstream: true, return_fallback, commit_sha, artifact_ids, answer_text }`).
- **3-Tier Managed `Env` / `Secret` Settings (`SettingsView.vue`)**: Switches between `Global` and `Namespace` scopes, performs `Env` CRUD (`/api/v1/envs`), and manages write-only `Secret` CRUD (`/api/v1/secrets`) using `type="password"` inputs and rendering only `name`, `scope`, `namespace`, and `updated_at` (never secret plaintext).

**Independent Test**: Vitest + Vue Test Utils specs in `web/src/__tests__/sandbox-preview.spec.ts` and `web/src/__tests__/decisions-and-secrets.spec.ts` asserting the exact `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer">` attributes (without `allow-same-origin`), exact `(commit_sha, artifact_ids)` payload binding on `human_approval` approve and fallback return, write-only secret rendering with `type="password"`, and `sessionStorage`-only token persistence.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/webui/handler.go` MUST embed `all:dist` with `internal/webui/dist/.keep`, expose `NewHandler()` and `NewHandlerFromFS(fs.FS)`, refuse `/api`, `/actors`, `/dapr`, `/healthz`, render an unbuilt fallback HTML page when `index.html` is absent, serve static files with accurate MIME types, return `404` for missing static asset extensions, and serve `index.html` fallback for SPA History routes.
- **FR-002**: `internal/cli/server_cmd.go` MUST default `Server.Config.UIHandler` to `webui.NewHandler()` in both `runDevServer` and `runDistributedServer` when `opts.UIHandler == nil`.
- **FR-003**: `scripts/build-webui.go` MUST run `npm.cmd --prefix web run build` on Windows (`npm` on non-Windows) and atomically mirror `web/dist` into `internal/webui/dist` while preserving `.keep`.
- **FR-004**: `web/src/api/client.ts` MUST store Bearer tokens strictly in `window.sessionStorage` (never `localStorage` or cookies), authenticate REST calls with `Authorization: Bearer <token>`, negotiate WebSockets via `['sofa.v1', 'sofa.bearer.' + base64UrlEncode(token)]`, and implement `mintPreviewUrl(runId, artifactId)` via `POST /api/v1/runs/{run_id}/artifacts/{artifact_id}/preview-ticket`.
- **FR-005**: `web/src/components/ArtifactSandboxPreview.vue` MUST render untrusted HTML previews exclusively with `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer" :src="previewUrl">` and MUST NEVER include `allow-same-origin`, `allow-top-navigation`, `allow-popups`, or `allow-forms`.
- **FR-006**: `web/` MUST provide Top Bar + Token session dialog, Runs List + Run detail DAG topology (`worker`, `dynamic_worker`, `merge`, `module` grouping) + Outputs (`Reveal Sensitive`), Node Inspector Drawer (epoch/git/checkpoints/diary/live+historical JSONL logs/artifacts/rerun/interjection with `wait_for_confirmation`), Decisions Center (`request_decision`, `pause_for_human`, version-bound `human_approval` approve & upstream fallback), and Settings (`Global`/`Namespace` `Env` + write-only `Secret` CRUD).

## Success Criteria *(mandatory)*

- **SC-001**: `npm.cmd --prefix web test -- --run` passes all Vitest specs (sandbox red-line, WS live DAG/logs, version-bound decisions, write-only secrets & `sessionStorage` auth).
- **SC-002**: `go run ./scripts/build-webui.go` cleanly builds `web/dist` and syncs into `internal/webui/dist/`.
- **SC-003**: `gofmt -l .` is empty, `go vet ./...` passes, and `go test ./... -count=1` passes all Go packages without requiring `node_modules`.
