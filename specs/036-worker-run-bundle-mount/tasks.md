# Tasks: Worker Run-Scoped `.sofaplan` Bundle Extraction, Control-Plane Decoupling, and Deferred Cleanup (`036-worker-run-bundle-mount`)

**Input**: Design documents from `/specs/036-worker-run-bundle-mount/`
**Prerequisites**: `spec.md`, `plan.md`

## Phase 1: Tests First (Red Phase — MUST fail before implementation)

- [x] T001 [US1][US2] Write failing tests in `internal/worker/daemon/daemon_test.go` for:
  - `start_node` with `PlanBlobKey` / `PlanSHA256` / `BundlePath` downloading & extracting once for two concurrent tasks of the same run (`singleflight`), reusing on a third task without re-downloading, failing clearly on SHA256 mismatch (both blob mismatch and existing marker SHA mismatch), passing `BundleDir` under `<TempDir>/bundles/<run_id>/`, sourcing `HostOS = runtime.GOOS`, `TempRoot = Options.TempDir`, `PluginResolveOpts.PluginDir = Options.PluginDir`, and `PluginResolveOpts.LockFile` from the extracted archive's `plugins.lock.json`.
  - `run_merge_candidate` using the same run-scoped extraction and passing worker-computed `BundleDir`, `HostOS`, and `TempRoot`.
- [x] T002 [US3] Write failing tests in `internal/worker/daemon/daemon_test.go` for `cleanup_run` removing `<TempDir>/bundles/<run_id>/`, refusing with `{removed: false, busy: true}` while a node or merge task for that run is active, and returning `{removed: false, busy: false}` without error for an unknown run.
- [x] T003 [US1][US3] Write failing tests in `internal/actor/run/run_test.go` verifying:
  - `RunActor` node and merge dispatches carry `PlanBlobKey`, `PlanSHA256`, `BundlePath` and contain zero control-plane host paths (`BundleDir`, `TempRoot`, `HostOS`, `PluginResolve.PluginDir` removed).
  - `RunActor.DeleteRun` enumerates workers via `ProcessLister.ListProcesses`, invokes `WorkerBundleCleaner.CleanupRun` on `HEALTHY` and `SUSPECT` workers (deduplicating `AppID`s and skipping `DEAD`), ignores errors/busy responses, and does not block past `WorkerCleanupTimeout` when a worker hangs.
- [x] T004 Run `go test ./internal/worker/daemon ./internal/actor/run -count=1` and confirm the new tests fail as expected.

## Phase 2: Implementation (Green Phase)

- [x] T005 [US1] Decouple dispatch and server/CLI structs: remove `HostOS`, `TempRoot`, `BundleDir`, and `PluginResolve.PluginDir` from `server.RunStartDefaults`, `internal/server/handlers.go`, `internal/cli/server_cmd.go`, `run.StartRunReq`, `run.persistedRunState`, `node.StartOrResumeReq`, `node.DispatchSpec`, `merge.EnqueueCandidateReq`, `merge.CandidateTicket`, `daemon.StartNodeRequest`, `daemon.RunMergeCandidateRequest`, and `daemon.PluginResolveConfig`; wire `PlanBlobKey`, `PlanSHA256`, and `BundlePath` end-to-end across `RunActor` -> `NodeActor`/`MergeActor` -> `WorkerDaemon`.
- [x] T006 [US2][US3] Implement run-scoped `.sofaplan` download/verify/`singleflight` extraction, completion marker reuse, `plugins.lock.json` loading, `bundleDirForExtract`, worker-local `HostOS`/`TempRoot`/`PluginDir`, and `"cleanup_run"` handler (with `activeTask.runID` busy guard) in `internal/worker/daemon/daemon.go`.
- [x] T007 [US3] Add `ProcessLister` and `WorkerBundleCleaner` interfaces + Dapr adapters in `internal/actor/run/types.go`, wire them into `RunOptions`/`NewRunActor`, and invoke bounded-timeout worker `cleanup_run` in `RunActor.DeleteRun`.
- [x] T008 [US1][US2][US3] Update existing Tier 1 and Tier 2 test fixtures (`daemon_test.go`, `node_integration_test.go`, `merge_integration_test.go`, `run_integration_test.go`, `server_integration_test.go`, `cli_integration_test.go`) and assert in `cli_integration_test.go` that `sofa delete` removes `<state>/worker-tmp/bundles/<run_id>`.

## Phase 3: Verification & Polish

- [x] T009 Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, `go test -tags=integration ./internal/cli/... ./internal/server/... ./internal/worker/daemon/... -count=1`, and `git status` to verify clean completion without committing.
