# Tasks: Full-Lifecycle CLI Extensions (`030-cli-server-worker-apply`)

## Phase 1: Foundational & Spec Alignment
- [X] T001 Inspect `internal/cli/command.go` and add `UIHandler http.Handler` to `cli.Options` while preserving `NormalizeArgs` and Stage 1 `init`/`schema`/`plan` commands

## Phase 2: User Stories 1, 2 & 3 — Tier 1 TDD (Red First)
- [X] T002 [P] Write failing Tier 1 tests in `internal/cli/cli_apply_test.go` covering:
  - `sofa apply ./dir` local compilation (`-var`, `SOFA_VAR_*`) + `422 missing_keys` guidance recommending `sofa secret set --from-env`
  - `sofa apply ./bundle.sofaplan -d` direct `.sofaplan` upload without recompilation
  - Foreground `sofa apply` WebSocket streaming + `ctx` cancellation sending `POST /api/v1/runs/{id}/pause?mode=immediate` before returning `ExitInterrupt` (`130`)
  - `sofa secret set --from-env` reading `Options.LookupEnv` + `sofa secret list` omitting secret plaintext + `sofa env set/list/delete`
  - `sofa runs`, `sofa runs get --reveal-sensitive`, `sofa logs`, `sofa pause`, `sofa resume`, `sofa rerun`, `sofa delete`, `sofa interject -m ... --wait`, and `sofa decisions list` / `sofa decisions respond --approve` (auto-populating `run_id`, `node_addr`, `commit_sha`, `artifact_ids`)
  - `sofa server` security rejection when non-dev/non-loopback without `SOFA_API_TOKEN`
- [X] T003 Run `go test ./internal/cli/...` to confirm Red failure before implementation

## Phase 3: Core Implementation (Green)
- [X] T004 Implement `internal/cli/client.go` (REST + WebSocket client, `--server`/`SOFA_SERVER_ADDR`, `--token`/`SOFA_API_TOKEN`, `APIError` decoding)
- [X] T005 Implement `internal/cli/apply_cmd.go` (dual input file vs directory compile via `project`/`planner`/`plan`, `422 missing_keys` formatting, `-d/--detach`, WS follow loop, `Ctrl+C` immediate pause)
- [X] T006 Implement `internal/cli/config_cmd.go` (`sofa env` and `sofa secret` subcommands)
- [X] T007 Implement `internal/cli/ops_cmd.go` (`runs`, `logs`, `pause`, `resume`, `rerun`, `delete`, `interject`, `decisions`)
- [X] T008 Implement `internal/cli/server_cmd.go` and `internal/cli/worker_cmd.go` (`sofa server [--dev]`, `sofa worker`, `blobStoreFlags` for local & S3, `daprhost` + 6 actors + `WorkerDaemon` + `RecoverActiveRuns`)
- [X] T009 Wire all new subcommands into `internal/cli/command.go` and verify `go test ./internal/cli/...` turns Green

## Phase 4: Tier 2 Integration Test (Red -> Green) & Verification
- [X] T010 Write Tier 2 real `sofa server --dev` subprocess E2E test in `internal/cli/cli_integration_test.go` (`//go:build integration`)
- [X] T011 Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -tags=integration ./internal/cli/... -count=1 -v`
- [X] T012 Verify clean process teardown and `git status`
