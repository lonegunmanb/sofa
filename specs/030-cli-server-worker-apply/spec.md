# Feature Specification: Full-Lifecycle CLI Extensions (`030-cli-server-worker-apply`)

**Feature Branch**: `030-cli-server-worker-apply`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `030` blueprint (`docs/stage3/030-cli-server-worker-apply.md`), `docs/stage3/README.md`, `docs/architecture-contract.md`, `internal/cli/command.go`, `internal/server/`, `internal/daprhost/manager.go`, `internal/worker/daemon/daemon.go`, `internal/actor/{config,registry,node,merge,run}`, `internal/blob/s3/store.go`, `internal/planner/`, and `internal/project/`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Unified REST/WebSocket Client & Managed Credentials (`env` / `secret`) (Priority: P1)

Operators authenticate and manage three-tier environment variables and write-only secrets from the CLI without leaking secret plaintext in shell history or terminal output.
- **Connection Precedence**: All client subcommands accept `--server <url>` (falling back to `SOFA_SERVER_ADDR`, defaulting to `http://127.0.0.1:8080`) and `--token <token>` (falling back to `SOFA_API_TOKEN`, sending `Authorization: Bearer <token>` on REST and WebSocket `/api/v1/ws` connections).
- **`sofa env` CRUD**: `sofa env set [--namespace <ns>] KEY=VALUE...`, `sofa env list [--namespace <ns>] [--run <run_id>]`, and `sofa env delete [--namespace <ns>] KEY...`.
- **`sofa secret` Write-Only CRUD**: `sofa secret set [--namespace <ns>] --from-env KEY1 [KEY2...]` (or `KEY=VALUE`) reads values from the client environment via `Options.LookupEnv` (failing before any write if a key is missing) and uploads via `PUT /api/v1/secrets` without ever printing secret values; `sofa secret list` prints only metadata (`name`, `scope`, `namespace`, `updated_at`); `sofa secret delete` deletes keys cleanly.

**Independent Test**: Tier 1 unit tests in `internal/cli/cli_apply_test.go` verifying `--server`/`SOFA_SERVER_ADDR` and `--token`/`SOFA_API_TOKEN` precedence, `sofa secret set --from-env` reading `Options.LookupEnv`, and `sofa secret list` never printing secret values.

---

### User Story 2 - `sofa apply` Dual Input, Second-0 `422` Diagnostics, Foreground Streaming & `Ctrl+C` Immediate Pause (Priority: P1)

Operators submit either a prebuilt `.sofaplan` archive or a local HCL configuration directory (initialized or uninitialized) via `sofa apply [path]`.
- **Dual Input**: If `path` is a regular file, upload its `.sofaplan` bytes directly to `POST /api/v1/runs`. If `path` is omitted (default `.`) or a directory, compile `.sofaplan` locally in a temporary workspace using `project.Init` + `planner.NewSofaConfig` + `plan.SaveArchive` (supporting `-var`, `-var-file`, `.sofavars`, and `SOFA_VAR_*`) before uploading.
- **Run-Tier Overrides & Preflight `422` Formatting**: Support `--namespace`, `--env KEY=VALUE`, `--secret-env KEY`, and `--secret KEY=VALUE`. When the server returns `422` with `missing_keys`, format every missing key and instruct the user to run `sofa secret set --from-env` (with `--namespace` when non-default) or `sofa env set`, exiting with `ExitFailure`.
- **Detach vs Foreground Follow & `Ctrl+C` Immediate Pause**: With `-d / --detach`, print `run_id` and exit `0` immediately. By default, attach to `/api/v1/ws?run_id=<id>` (and check `GET /api/v1/runs/{id}`), stream node transitions and incremental logs, print pending `decision_id` hints when `WAITING_HUMAN`, print outputs (masked by default) on `SUCCEEDED`, or exit `1` on `FAILED`. On context cancellation (`Ctrl+C`), use a fresh timeout context to send `POST /api/v1/runs/{id}/pause?mode=immediate` before returning `ExitInterrupt` (`130`).

**Independent Test**: Tier 1 unit tests in `internal/cli/cli_apply_test.go` covering directory compilation + `422 missing_keys` guidance, prebuilt `.sofaplan -d` upload, and `ctx` cancellation during foreground streaming triggering `POST /api/v1/runs/{id}/pause?mode=immediate`.

---

### User Story 3 - Three Daemon Roles (`sofa server --dev`, `sofa server`, `sofa worker`) & Full Ops Subcommands (Priority: P1)

Operators run single-node all-in-one factories (`sofa server --dev`), distributed control planes (`sofa server`), standalone compute workers (`sofa worker`), and full lifecycle ops commands (`runs`, `logs`, `pause`, `resume`, `rerun`, `delete`, `interject`, `decisions`).
- **`sofa server --dev`**: Starts local `daprhost.Manager`, registers all 6 Control Plane Actors plus a co-located `WorkerDaemon` (`--max-concurrency` default `2`), starts dual-port `server.Server` (wiring `opts.UIHandler`), invokes `OperatorActor.RecoverActiveRuns`, and performs clean ordered shutdown on `ctx.Done()`.
- **`sofa server` (distributed)**: Connects to external Dapr sidecar, enforces mandatory `APIToken` via `server.ValidateListenSecurity`, configures local or S3 `BlobStore`, registers the 6 Control Plane Actors on `--internal-listen`, invokes `RecoverActiveRuns`, and shuts down cleanly.
- **`sofa worker`**: Connects to external Dapr sidecar and shared `BlobStore`, exposes `WorkerDaemon` handlers with `APP_API_TOKEN` middleware (`--max-concurrency` default `10`), and shuts down cleanly.
- **Ops Subcommands**: `runs` / `runs get <run_id> [--reveal-sensitive]`, `logs <run_id> <node_id> [--epoch N]`, `pause [--mode planned|immediate] <run_id>`, `resume <run_id>`, `rerun <run_id> <node_id>`, `delete <run_id>`, `interject <run_id> <node_id> -m "..." [--wait]`, and `decisions [list | respond <decision_id> ...]` (auto-resolving `run_id`, `node_addr`, `commit_sha`, `artifact_ids` from pending decision cards when omitted).

**Independent Test**: Tier 1 unit tests in `internal/cli/cli_apply_test.go` for all ops subcommands and Tier 2 end-to-end integration test in `internal/cli/cli_integration_test.go` launching a real `sofa server --dev` subprocess with Podman, Git, Dapr, and `scriptedplugin`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/cli/client.go` MUST provide a shared REST + WebSocket client honoring `--server`/`SOFA_SERVER_ADDR` (default `http://127.0.0.1:8080`) and `--token`/`SOFA_API_TOKEN`, decoding structured server errors (`APIError` with `MissingKeys` on `422`).
- **FR-002**: `internal/cli/server_cmd.go` MUST implement both `sofa server --dev` (`--max-concurrency` default `2`, `daprhost.Manager` + 6 Actors + co-located `WorkerDaemon` + `RecoverActiveRuns` + `opts.UIHandler`) and `sofa server` (external Dapr + local/S3 `BlobStore` + mandatory `APIToken` + 6 Actors + `RecoverActiveRuns`).
- **FR-003**: `internal/cli/worker_cmd.go` MUST implement `sofa worker` (`--max-concurrency` default `10`, external Dapr + local/S3 `BlobStore` + `APP_API_TOKEN` middleware).
- **FR-004**: `internal/cli/apply_cmd.go` MUST support both `.sofaplan` files and local HCL directories (reusing `project.Init`, `planner.NewSofaConfig`, `buildCliFlagVariables`, and `plan.SaveArchive` without re-implementing `golden`), format `422 missing_keys`, support `-d/--detach`, stream WebSocket + status updates in foreground, and send `POST /api/v1/runs/{id}/pause?mode=immediate` on `ctx.Done()`.
- **FR-005**: `internal/cli/config_cmd.go` MUST implement `env` (`set`/`list`/`delete`) and `secret` (`set --from-env`/`set`/`list`/`delete`) without ever echoing secret plaintext.
- **FR-006**: `internal/cli/ops_cmd.go` MUST implement `runs`, `logs`, `pause`, `resume`, `rerun`, `delete`, `interject`, and `decisions` (`list`/`respond`).
- **FR-007**: `internal/cli/command.go` MUST preserve existing Stage 1 `init`/`schema`/`plan` behavior and `NormalizeArgs` `-var`/`-var-file` handling, and add `UIHandler http.Handler` to `cli.Options`.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` is empty and `go vet ./...` passes with zero issues.
- **SC-002**: `go test ./... -count=1` passes all Tier 1 tests, including all Stage 1 `internal/cli` tests and new `cli_apply_test.go` cases.
- **SC-003**: `go test -tags=integration ./internal/cli/... -count=1 -v` passes end-to-end against a real `sofa server --dev` child binary with Podman, Git, and Dapr, leaving zero orphan processes.
