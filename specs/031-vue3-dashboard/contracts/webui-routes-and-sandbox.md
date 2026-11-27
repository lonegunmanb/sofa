# Contracts: WebUI Routes, Auth Subprotocols & Untrusted HTML Sandbox (`031-vue3-dashboard`)

## 1. Embedded Static & SPA Routing Contract (`internal/webui`)

| Request Path | Condition | Response |
| :--- | :--- | :--- |
| `/api`, `/api/*`, `/actors/*`, `/dapr/*`, `/healthz` | Any | `404 Not Found` (never handled by WebUI) |
| Any non-reserved path | `index.html` missing in `distFS` | `200 OK`, `Content-Type: text/html; charset=utf-8` built-in placeholder explaining `go run ./scripts/build-webui.go` |
| `/assets/index-xxx.js` (or regular file in `distFS`) | File exists in `distFS` | `200 OK`, accurate MIME type (`application/javascript`, `text/css`, etc.) |
| `/assets/missing.js` (static asset extension) | File missing in `distFS` | `404 Not Found` |
| `/`, `/runs`, `/runs/run-123`, `/decisions`, `/settings` | `index.html` present in `distFS` | `200 OK`, `Content-Type: text/html; charset=utf-8` serving `index.html` |

## 2. Browser Token & WebSocket Auth Contract (`web/src/api/client.ts`)

- **Storage**: `window.sessionStorage` key `sofa_api_token` ONLY. Never `window.localStorage` or `document.cookie`.
- **REST**: When token is non-empty, send `Authorization: Bearer <token>`.
- **WebSocket**:
  - URL: `${wsOrigin}/api/v1/ws` (plus `?run_id=<encodeURIComponent(runId)>` when filtered).
  - Protocols: `['sofa.v1', 'sofa.bearer.' + base64UrlNoPad(token)]` when token is non-empty; `['sofa.v1']` otherwise. Never put token in query string.

## 3. Untrusted HTML Artifact Preview Contract (`ArtifactSandboxPreview.vue`)

1. Mint scoped preview ticket: `POST /api/v1/runs/{run_id}/artifacts/{artifact_id}/preview-ticket` -> `{ ticket, expires_at }` (or `preview_url` if provided).
2. Preview URL: `/api/v1/runs/{run_id}/artifacts/{artifact_id}/content?ticket=<encodeURIComponent(ticket)>`.
3. Rendered `<iframe>` MUST have:
   - `sandbox="allow-scripts"`
   - `referrerpolicy="no-referrer"`
   - `:src="previewUrl"`
   - MUST NOT contain `allow-same-origin`, `allow-top-navigation`, `allow-popups`, or `allow-forms`.

## 4. Version-Bound `human_approval` Response Contract

- **Approve**: `POST /api/v1/decisions/{card_id}/respond` with `{ run_id, node_addr, approved: true, commit_sha, artifact_ids, answer_text }`.
- **Return Upstream Fallback**: `POST /api/v1/decisions/{card_id}/respond` with `{ run_id, node_addr, approved: false, returned_upstream: true, return_fallback, commit_sha, artifact_ids, answer_text }`.
- **Reject for Rework**: `POST /api/v1/decisions/{card_id}/respond` with `{ run_id, node_addr, approved: false, commit_sha, artifact_ids, answer_text }`.
