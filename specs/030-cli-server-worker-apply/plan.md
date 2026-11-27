# Implementation Plan: Full-Lifecycle CLI Extensions (`030-cli-server-worker-apply`)

**Branch**: `030-cli-server-worker-apply` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Extend `internal/cli` and `cmd/sofa` with the complete Stage 3 CLI surface: shared REST + WebSocket client (`client.go`), daemon entrypoints (`server_cmd.go`, `worker_cmd.go`), dual-input `sofa apply` with local `planner`/`golden` compilation and `Ctrl+C` immediate pause (`apply_cmd.go`), 3-tier `env`/`secret` management (`config_cmd.go`), and full lifecycle ops commands (`ops_cmd.go`).

## Technical Context

- **Language/Version**: Go 1.24+ (`github.com/lonegunmanb/sofa`)
- **Primary Dependencies**: `github.com/spf13/cobra`, `github.com/coder/websocket`, `github.com/dapr/go-sdk`, `github.com/lonegunmanb/golden`, `internal/server`, `internal/daprhost`, `internal/worker/daemon`, `internal/worker/runner`, `internal/blob`, `internal/blob/s3`, `internal/planner`, `internal/project`
- **Testing**: Tier 1 unit tests (`internal/cli/cli_apply_test.go` with `httptest` + WebSocket + `Options.LookupEnv`); Tier 2 real `sofa server --dev` integration test (`internal/cli/cli_integration_test.go`, `//go:build integration`)
- **Constraints**:
  - Reuse `planner.NewSofaConfig`, `project.Init`, `buildCliFlagVariables`, and `plan.SaveArchive` directly; never re-implement `golden` HCL/variable logic.
  - Preserve `NormalizeArgs` `-var`/`-var-file` rewriting and all Stage 1 `init`/`schema`/`plan` tests.
  - On `ctx.Done()` in foreground `sofa apply`, issue `POST /api/v1/runs/{id}/pause?mode=immediate` with a fresh `context.WithTimeout(context.Background(), 10*time.Second)` before returning `ExitInterrupt` (`130`).

## Project Structure

```text
internal/cli/
├── command.go              # Options (with UIHandler), NormalizeArgs, Run, NewCommand wiring
├── client.go               # Shared HTTP + WebSocket API client & typed error decoding
├── server_cmd.go           # sofa server [--dev] wiring (daprhost, 6 actors, optional WorkerDaemon, BlobStore)
├── worker_cmd.go           # sofa worker standalone compute daemon wiring
├── apply_cmd.go            # sofa apply [path] (file or local dir compile, preflight 422, WS follow, Ctrl+C pause)
├── config_cmd.go           # sofa env & sofa secret subcommands
├── ops_cmd.go              # runs, logs, pause, resume, rerun, delete, interject, decisions subcommands
├── command_test.go         # Existing Stage 1 CLI tests (preserved)
├── cli_apply_test.go       # Tier 1 unit tests for Slice 030
└── cli_integration_test.go # Tier 2 real `sofa server --dev` subprocess E2E test (//go:build integration)
```

## Key Design Decisions

1. **Dual-Mode `BlobStore` Builder**: Shared helper `openBlobStore(ctx, blobOpts)` in `internal/cli` constructs `*blob.LocalStore` when `--s3-bucket` is empty (defaulting `--blob-dir` to `<state-dir>/blobs` or explicit `--blob-dir`), or `*s3.Store` via `s3.NewStore` when `--s3-bucket` is provided.
2. **Actor & Worker Wiring in `sofa server --dev`**: Matches `internal/server/server_integration_test.go` using `daprhost.NewManager`, registering all 6 Control Plane Actors (`ConfigActor`, `ProcessRegistryActor`, `NodeActor`, `MergeActor`, `OperatorActor`, `RunActor`) plus co-located `WorkerDaemon` with real `PodmanEngine`, `git.NewCLIAdapter`, `artifact.NewService`, and `runner.ContainerSessionLauncher`. Invokes `OperatorActor.RecoverActiveRuns` immediately after startup.
3. **Standalone `sofa server` and `sofa worker`**: Connect to external `daprd` using `daprclient.NewClientWithPort` (when `--dapr-grpc-port` / `DAPR_GRPC_PORT` is set) or `daprclient.NewClient()`. Standalone `sofa worker` protects its `daprhttp.NewServiceWithMux` internal listener with `dapr-api-token` checking when `APP_API_TOKEN` is set.
4. **Local Directory Compilation in `sofa apply [dir]`**: Copies any existing `.sofa/plugins.lock.json` into a temp state dir if present, calls `project.Init` into the temp state dir to snapshot HCL + install modules + lock plugins, runs `planner.NewSofaConfig` + `Init` + `RunPlan` + `ExportPlan` + `plan.SaveArchive` with `-var`/`-var-file`, and uploads the resulting `.sofaplan` bytes via `POST /api/v1/runs`.
5. **Foreground Follow Race-Free Loop**: Immediately after `POST /api/v1/runs` returns `201`, dials `/api/v1/ws?run_id=<id>`, prints live node log deltas and run status transitions, also checks `GET /api/v1/runs/{id}` initially and periodically (catching ultra-fast runs that complete before WS subscription), prints `decision_id` guidance on `WAITING_HUMAN`, and catches `ctx.Done()` to send immediate pause before exiting `130`.
