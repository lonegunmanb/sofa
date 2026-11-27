# Quickstart: `029-rest-ws-api-server`

## Tier 1 Unit & Contract Verification (Fast, Offline)

```powershell
go test -v ./internal/server/... -count=1
```

Verifies:
1. Startup rejection of tokenless non-loopback or non-dev server configs.
2. `--dev` `Origin: null` and `Origin: https://evil.example` -> `403 Forbidden`.
3. Public port `/actors/...` and `/dapr/...` -> `404 Not Found`.
4. Bearer token & `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<b64url>` authentication.
5. Short-lived HMAC-SHA256 preview ticket scoping & expiry.
6. Second-0 `422` `missing_keys` preflight gate on `POST /api/v1/runs`.
7. Recursive `<sensitive>` output masking vs `?reveal_sensitive=true`.
8. Write-only `GET /api/v1/secrets` and `409 Conflict` on Env/Secret conflict & shared-branch rerun.
9. Forced CSP sandbox headers on `/artifacts/{id}/content`.
10. `/api/v1/ws` global vs `run_id`-filtered fan-out.

## Tier 2 Real-Dapr Integration Verification

```powershell
go test -v -tags=integration ./internal/server/... -count=1
```

Spins up real `daprhost.Manager`, dual-port `server.Server`, `WorkerDaemon`, and a bare Git repository to exercise `POST /api/v1/runs` (`422` missing secret) -> `PUT /api/v1/secrets` -> `/api/v1/ws` subscription -> `POST /api/v1/runs` (`201`) -> live WS events -> `SUCCEEDED` -> CSP artifact preview -> `DELETE /api/v1/runs/{id}` full cleanup.
