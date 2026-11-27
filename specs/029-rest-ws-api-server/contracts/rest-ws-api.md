# Contract: Unified REST JSON + WebSocket API (`029-rest-ws-api-server`)

## 1. Security & Port Isolation Invariants

- **Public Listener (`PublicAddr`, default `127.0.0.1:8080`)**:
  - Mounts ONLY `/healthz`, `/api/v1/*`, and optional `UIHandler`.
  - `/actors/*` and `/dapr/*` MUST return `404 Not Found`.
  - Startup refuses empty `APIToken` unless `DevMode == true && isLoopbackListenAddr(PublicAddr)` (`127.0.0.1` or `::1`).
- **Internal Dapr Listener (`InternalAddr`, `127.0.0.1:<daprAppPort>`)**:
  - Protected by `dapr-api-token` (`APP_API_TOKEN`) constant-time middleware.
  - Mounts `daprhttp.NewServiceWithMux`, Control Plane Actors, and PubSub subscriptions (`sofa-events`, `sofa.run.events`).
- **Authentication**:
  - REST `/api/v1/*`: `Authorization: Bearer <token>` (constant-time comparison). Exception: `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content` also accepts valid `?ticket=<hmac-ticket>` scoped strictly to `(run_id, artifact_id)`.
  - WebSocket `GET /api/v1/ws`: accepts `Authorization: Bearer <token>` OR `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<base64url(token)>` (server echoes `sofa.v1`). Query-string `?token=` is NOT accepted.
- **Anti-CSRF `Origin` Guard (Active Always, Including `--dev` Mode)**:
  - On all `POST`, `PUT`, `PATCH`, `DELETE` requests under `/api/v1/*` and `GET /api/v1/ws`: if `Origin` header is present, `Origin: null` or `originURL.Host != r.Host` -> `403 Forbidden`.

## 2. Endpoints

| Method & Path | Request | Response / Status Codes |
| :--- | :--- | :--- |
| `GET /healthz` | — | `200 {"status":"ok"}` |
| `POST /api/v1/runs` | `multipart/form-data` (`plan` file, optional `namespace`, `run_id`, `envs` JSON, `secrets` JSON) OR `application/octet-stream` (`?namespace=&run_id=`) | `201 {"run_id","namespace","status"}`; `422 {"error","missing_keys"}` on preflight failure; `400` invalid plan; `409` env/secret conflict |
| `GET /api/v1/runs` | `?namespace=&status=` | `200 {"runs":[...]}` |
| `GET /api/v1/runs/{run_id}` | `?reveal_sensitive=true` | `200 RunDetailResponse` (masked `"<sensitive>"` unless `reveal_sensitive=true`); `404` |
| `POST /api/v1/runs/{run_id}/pause` | `?mode=planned\|immediate` OR `{"mode":"planned\|immediate","reason":"..."}` | `200 PauseRunResp`; `400`/`404`/`409` |
| `POST /api/v1/runs/{run_id}/resume` | `{}` | `200 ResumeRunResp`; `404`/`409` |
| `DELETE /api/v1/runs/{run_id}` | — | `200 DeleteRunResp`; `404`/`409` |
| `GET /api/v1/runs/{run_id}/nodes/{node_id}` | URL-decoded `{node_id}` | `200 NodeDetailResponse`; `404` |
| `GET /api/v1/runs/{run_id}/nodes/{node_id}/logs` | `?epoch=<optional>` | `200 application/x-ndjson` stream from `daemon.NodeLogBlobKey`; `404` |
| `POST /api/v1/runs/{run_id}/nodes/{node_id}/rerun` | `{"reason":"..."}` | `200 RerunNodeResp`; `409` on `ErrAlreadyMergedSharedBranch`; `404` |
| `POST /api/v1/runs/{run_id}/nodes/{node_id}/diary-messages` | `{"message":"...","wait_for_confirmation":bool}` | `200 EnqueueInterjectionResp`; `400`/`404` |
| `GET /api/v1/decisions` | `?run_id=&status=` | `200 {"decisions":[...]}` |
| `POST /api/v1/decisions/{id}/respond` | `SubmitDecisionReq` (optional `run_id`/`node_addr`) | `200 SubmitDecisionResp`; `404` card not found; `409` invalid/superseded decision; `400` validation error |
| `GET /api/v1/envs` | `?scope=global\|namespace&namespace=` | `200 {"scope","namespace","envs":[EnvEntry]}` |
| `PUT /api/v1/envs` | `{"scope","namespace","name","value"}` (or `entries`) | `200 {"updated":[...]}`; `400`; `409` on `ErrKindConflict` |
| `DELETE /api/v1/envs` | `?name=&scope=&namespace=` or JSON body | `200 {"deleted":bool}`; `400` |
| `GET /api/v1/secrets` | `?scope=global\|namespace&namespace=` | `200 {"scope","namespace","secrets":[SecretMeta]}` (NEVER includes `value`) |
| `PUT /api/v1/secrets` | `{"scope","namespace","name","value"}` (or `entries`) | `200 {"updated":[...]}`; `400`; `409` on `ErrKindConflict` |
| `DELETE /api/v1/secrets` | `?name=&scope=&namespace=` or JSON body | `200 {"deleted":bool}`; `400` |
| `POST /api/v1/runs/{run_id}/artifacts/{artifact_id}/preview-ticket` | — | `200 {"ticket","expires_at"}`; `404` |
| `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content` | `?ticket=<optional>&entry=<optional>` | `200` artifact stream + forced `Content-Security-Policy` + `X-Content-Type-Options: nosniff`; `401`/`404` |
| `GET /api/v1/ws` | `?run_id=<optional>` | `101 Switching Protocols` WebSocket event stream |
