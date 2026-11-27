# Tasks: Vue 3 Security Dashboard & Single-Binary `go:embed` Human Decision Workbench (`031-vue3-dashboard`)

## Phase 1: Setup & Embedded Router Foundation (Go TDD Red -> Green)
- [X] T001 Create `internal/webui/dist/.keep` and write failing Tier 1 tests in `internal/webui/handler_test.go` covering:
  - Unbuilt `distFS` (no `index.html`) returning `200 text/html` status page with `go run ./scripts/build-webui.go` instructions
  - Built SPA `distFS` serving `/runs/123` History route fallback as `200 text/html`
  - Built SPA `distFS` serving `/assets/app.js` with JavaScript MIME type and `/assets/app.css` with `text/css`
  - Missing static asset `/assets/missing.js` returning `404`
  - Reserved prefixes (`/api/v1/unknown`, `/actors/foo`, `/dapr/config`, `/healthz`) returning `404`
  - `NewHandler()` embedded handler smoke check
- [X] T002 Run `go test ./internal/webui/...` to confirm Red failure before implementation
- [X] T003 Implement `internal/webui/handler.go` (`NewHandler`, `NewHandlerFromFS`, `//go:embed all:dist`) and wire default `webui.NewHandler()` in `internal/cli/server_cmd.go` when `opts.UIHandler == nil`
- [X] T004 Run `go test ./internal/webui/... ./internal/cli/...` to verify Green pass

## Phase 2: Frontend Workspace Setup & Failing Vitest Specs (Vue TDD Red First)
- [X] T005 Initialize `web/` project configuration (`package.json`, `tsconfig.json`, `vite.config.ts`, `index.html`) and install dependencies via `npm.cmd --prefix web install`
- [X] T006 [P] Write failing Vitest spec `web/src/__tests__/sandbox-preview.spec.ts` asserting `ArtifactSandboxPreview.vue` mints preview ticket, sets `src` to ticketed `/content?ticket=...` URL, and renders `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer">` strictly without `allow-same-origin`, `allow-top-navigation`, or `allow-popups`
- [X] T007 [P] Write failing Vitest spec `web/src/__tests__/run-dag-ws.spec.ts` verifying `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<base64url>` negotiation from `sessionStorage`, live `sofa.run.events` status/epoch badge updates on `RunDetailView` / `DagGraph`, and live `sofa-events` log appending in `NodeInspectorDrawer`
- [X] T008 [P] Write failing Vitest spec `web/src/__tests__/decisions-and-secrets.spec.ts` verifying `human_approval` Approve & Return Upstream Fallback payloads bind exact `(commit_sha, artifact_ids)`, `SettingsView.vue` renders secret input as `type="password"` and secret list without plaintext values, and token storage uses `window.sessionStorage` exclusively (never `localStorage`)
- [X] T009 Run `npm.cmd --prefix web test -- --run` to confirm Red failure before Vue implementation

## Phase 3: Frontend Implementation & Build Sync Script (Green)
- [X] T010 Implement `web/src/api/types.ts` and `web/src/api/client.ts` (`sessionStorage` token, Bearer `fetch`, WS `sofa.v1` + `sofa.bearer.<base64url>`, `mintPreviewUrl`, REST helpers)
- [X] T011 Implement `web/src/router.ts`, `web/src/style.css`, `web/src/main.ts`, and `web/src/App.vue` (top nav bar, token session dialog, WS status pill)
- [X] T012 Implement `web/src/components/ArtifactSandboxPreview.vue` (strict `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer">`), `DagGraph.vue`, `NodeInspectorDrawer.vue`, and `DecisionCardItem.vue`
- [X] T013 Implement `web/src/views/RunsView.vue`, `RunDetailView.vue`, `DecisionsView.vue`, and `SettingsView.vue`
- [X] T014 Run `npm.cmd --prefix web test -- --run` to verify all Vitest specs turn Green
- [X] T015 Implement `scripts/build-webui.go` (invoking `npm.cmd --prefix web run build` on Windows, atomically syncing `web/dist/` into `internal/webui/dist/` while keeping `.keep`)

## Phase 4: Full Verification Gates
- [X] T016 Run `go run ./scripts/build-webui.go` to build and embed the Vue 3 SPA into `internal/webui/dist/`
- [X] T017 Verify `npm.cmd --prefix web test -- --run`, `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `git status`
