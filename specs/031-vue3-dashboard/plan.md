# Implementation Plan: Vue 3 Security Dashboard & Single-Binary `go:embed` Human Decision Workbench (`031-vue3-dashboard`)

**Branch**: `031-vue3-dashboard` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Implement Stage 3 Slice `031` across three coordinated parts:
1. **`internal/webui/` & `internal/cli/server_cmd.go` & `scripts/build-webui.go`**: Go `//go:embed all:dist` SPA static handler with unbuilt fallback placeholder, MIME-accurate static asset serving, missing-asset `404` protection, reserved API/Dapr prefix exclusion, default `UIHandler` wiring in `sofa server [--dev]`, and cross-platform `scripts/build-webui.go` using `npm.cmd` on Windows.
2. **`web/` (Vue 3 + TypeScript + Vite + Vitest)**: Lean, dependency-light workbench (`vue` + lightweight reactive History router, `typescript`, `vite`, `@vitejs/plugin-vue`, `vitest`, `@vue/test-utils`, `jsdom`) implementing `sessionStorage`-only auth, `Sec-WebSocket-Protocol` `sofa.v1` + `sofa.bearer.<base64url>` live streaming, Runs & DAG topology, Node Inspector drawer (checkpoints, diary, live + historical JSONL logs, interjections, rerun, artifact preview), Decisions Center (version-bound `human_approval` approve & upstream fallback), and 3-tier `Env` / write-only `Secret` settings.
3. **Security Red-Line (`ArtifactSandboxPreview.vue`)**: Ticket-minted preview URL rendered strictly inside `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer" :src="previewUrl">` with zero `allow-same-origin`, `allow-top-navigation`, `allow-popups`, or `allow-forms`.

## Technical Context

- **Backend**: Go 1.27 (`embed`, `io/fs`, `mime`, `net/http`, `path`), `internal/cli/server_cmd.go`
- **Frontend**: Vue 3 (`<script setup lang="ts">`), TypeScript, Vite 6, Vitest + `@vue/test-utils` + `jsdom`
- **Windows Tooling Constraint**: Invoke `npm.cmd` / `npx.cmd` when `runtime.GOOS == "windows"` in `scripts/build-webui.go` and verification commands.

## Project Structure

```text
sofa/
├── internal/
│   ├── cli/
│   │   └── server_cmd.go                      # Default UIHandler = webui.NewHandler() when opts.UIHandler == nil
│   └── webui/
│       ├── dist/
│       │   └── .keep                          # Tracked placeholder guaranteeing //go:embed all:dist compiles
│       ├── handler.go                         # NewHandler(), NewHandlerFromFS(), SPA History + fallback + reserved prefix guard
│       └── handler_test.go                    # Tier 1 Go tests for unbuilt & built SPA modes
├── scripts/
│   └── build-webui.go                         # Runs npm(.cmd) --prefix web run build & syncs web/dist -> internal/webui/dist
└── web/
    ├── index.html
    ├── package.json
    ├── tsconfig.json
    ├── vite.config.ts
    └── src/
        ├── main.ts
        ├── style.css
        ├── App.vue                             # Top bar, History navigation, Token sessionStorage dialog, WS indicator
        ├── router.ts                           # Lightweight History API reactive router (/, /runs, /runs/:id, /decisions, /settings)
        ├── api/
        │   ├── types.ts                        # Exact REST & WS TypeScript types matching internal/server & actors
        │   └── client.ts                       # sessionStorage token, fetch Bearer client, WS subprotocol client, mintPreviewUrl
        ├── components/
        │   ├── DagGraph.vue                    # Topological layering + module/dynamic_worker grouping + edge connectors
        │   ├── NodeInspectorDrawer.vue         # Epoch/Git/Checkpoints/Diary/Logs/Artifacts/Rerun/Interjection drawer
        │   ├── ArtifactSandboxPreview.vue      # Strict <iframe sandbox="allow-scripts" referrerpolicy="no-referrer"> preview
        │   └── DecisionCardItem.vue            # request_decision / pause_for_human / version-bound human_approval card
        ├── views/
        │   ├── RunsView.vue                    # Runs table, status filter, Run actions + selected RunDetailView
        │   ├── RunDetailView.vue               # Run controls (Pause planned/immediate, Resume, Delete), DAG, Outputs (Reveal Sensitive)
        │   ├── DecisionsView.vue               # Pending & historical decision queue with WS auto-refresh
        │   └── SettingsView.vue                # Global/Namespace scope Env + write-only Secret CRUD
        └── __tests__/
            ├── sandbox-preview.spec.ts         # Security red-line iframe sandbox assertions
            ├── run-dag-ws.spec.ts              # WS subprotocol auth, live node status/epoch & log append tests
            └── decisions-and-secrets.spec.ts   # human_approval (commit_sha, artifact_ids), write-only secrets, sessionStorage tests
```
