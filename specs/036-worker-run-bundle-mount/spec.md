# Feature Specification: Worker Run-Scoped `.sofaplan` Bundle Extraction, Control-Plane Decoupling, and Deferred Cleanup (`036-worker-run-bundle-mount`)

**Feature Branch**: `036-worker-run-bundle-mount`

**Created**: 2026-11-27

**Status**: Approved

**Input**: `internal/actor/run/run_actor.go`, `internal/actor/run/types.go`, `internal/actor/node/actor.go`, `internal/actor/node/types.go`, `internal/actor/merge/actor.go`, `internal/actor/merge/types.go`, `internal/actor/registry/actor.go`, `internal/worker/daemon/daemon.go`, `internal/server/server.go`, `internal/server/handlers.go`, `internal/cli/server_cmd.go`, `internal/cli/worker_cmd.go`, `internal/plan/archive.go`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Control-Plane / Worker Dispatch Host-Path Decoupling (Priority: P1)

In distributed deployments where control plane (`sofa server`) and compute workers (`sofa worker`) run on different hosts or OSes, the control plane MUST NOT send control-plane host filesystem paths (`BundleDir`, `TempRoot`, `PluginResolve.PluginDir`) or control-plane `HostOS` in node or merge dispatch payloads:
- `server.RunStartDefaults`, `run.StartRunReq`, `run.persistedRunState`, `node.StartOrResumeReq`, `node.DispatchSpec`, `merge.EnqueueCandidateReq`, `merge.CandidateTicket`, `daemon.StartNodeRequest`, and `daemon.RunMergeCandidateRequest` MUST NOT carry control-plane `BundleDir`, `TempRoot`, or `HostOS`, and `daemon.PluginResolveConfig` MUST NOT carry `PluginDir`.
- Instead, `StartOrResumeReq`, `DispatchSpec`, `EnqueueCandidateReq`, `CandidateTicket`, `StartNodeRequest`, and `RunMergeCandidateRequest` carry `PlanBlobKey`, `PlanSHA256`, and `BundlePath` (the module's bundle-relative path `"config"` or `"modules/<name>"` from `currentPlan.BundlePath()`).
- `WorkerDaemon` MUST derive `HostOS` from its own `runtime.GOOS`, `TempRoot` from its own `Options.TempDir` (`--temp-dir` / `<state-dir>/worker-tmp`), and `PluginResolveOpts.PluginDir` from its own `Options.PluginDir` (`--plugin-dir`), and MUST source `PluginResolveOpts.LockFile` from the extracted `.sofaplan` archive's `plugins.lock.json` on the worker side (falling back to `req.PluginResolve.LockFile` only when no `PlanBlobKey` is provided in synthetic unit tests).
- Co-located `sofa server --dev` continues to work seamlessly using the embedded `WorkerDaemon`'s `BlobStore`, `TempDir`, and `PluginDir`.

**Independent Test**: Unit tests in `internal/actor/run/run_test.go` and `internal/worker/daemon/daemon_test.go` verify dispatched `StartOrResumeReq` / `EnqueueCandidateReq` carry `PlanBlobKey`, `PlanSHA256`, and `BundlePath` with zero control-plane host paths, and `WorkerDaemon` populates `HostOS = runtime.GOOS`, `TempRoot = Options.TempDir`, `PluginResolveOpts.PluginDir = Options.PluginDir`, and `PluginResolveOpts.LockFile` from the extracted archive's `plugins.lock.json`.

---

### User Story 2 - Worker Run-Scoped `.sofaplan` Download, Singleflight Extraction, and Reuse (Priority: P1)

When `WorkerDaemon` receives `start_node` or `run_merge_candidate` with a non-empty `PlanBlobKey`:
- It lazily downloads the `.sofaplan` archive from its configured shared `BlobStore` by `PlanBlobKey`, verifies the downloaded bytes match `PlanSHA256` (when specified), and extracts it using `plan.LoadArchive` into `<TempDir>/bundles/<sanitized run_id>/` (defaulting `<TempDir>` to `os.TempDir()` when `Options.TempDir` is empty).
- Extraction is staged and marked with a completion marker containing `PlanSHA256` only after `plan.LoadArchive` succeeds.
- Concurrent tasks of the same run on the same worker share a single download/extraction via `golang.org/x/sync/singleflight` (non-cancellable by any single caller abort while still respecting caller context on wait).
- Subsequent tasks (later DAG nodes, resume, rerun) on the same worker find the completion marker with matching `PlanSHA256` and reuse the extracted directory without re-downloading from `BlobStore`.
- If a completion marker exists with a DIFFERENT `PlanSHA256` (or the downloaded blob fails SHA256 verification), the worker fails the task clearly and reports the failure back to `NodeActor` / `MergeActor`.
- **BundleDir Hand-Off Semantics (to Slice 037)**: Extraction unpacks the entire `.sofaplan` root (`config/`, `modules/`, `external_skills/`, `plugins.lock.json`) into `<TempDir>/bundles/<sanitized run_id>/`. To preserve existing container `/.sofa/bundle` mount semantics without breaking `config/` or `modules/<name>/` relative references in this slice, the worker computes `BundleDir` using the same subdirectory resolution as `bundleDirForPlan` (`<extractRoot>/<BundlePath>` when present, falling back to `<extractRoot>/config` then `<extractRoot>`), and passes that path to `NodeExecutor` and `MergeExecutor`. Relocating `/.sofa/bundle` to mount the extract root and rewriting `skill_directories` to `/.sofa/bundle/...` is explicitly deferred to Slice 037.

**Independent Test**: Unit tests in `internal/worker/daemon/daemon_test.go` verify:
- Two concurrent `start_node` tasks for the same run trigger only 1 `BlobStore.Get` and 1 extraction via `singleflight`.
- A third task (node or merge) reuses the extracted directory without calling `BlobStore.Get`.
- A SHA256 mismatch against the marker or downloaded blob fails the task clearly and reports failure.
- `run_merge_candidate` uses the same run-scoped extraction and passes worker-computed `BundleDir`, `HostOS`, and `TempRoot` to `MergeExecutor`.

---

### User Story 3 - Deferred Cleanup Only on `sofa delete` (Priority: P1)

Worker bundle directories (`<TempDir>/bundles/<sanitized run_id>/`) MUST NEVER be removed when individual node/merge tasks finish or when a run reaches `SUCCEEDED`, `FAILED`, or `PAUSED`:
- `WorkerDaemon` exposes a Dapr Service Invocation handler `"cleanup_run"` (`CleanupRunRequest{RunID}` -> `CleanupRunResponse{Removed, Busy}`):
  - If any active node or merge task for `run_id` is currently running on that worker, it refuses cleanup and returns `{removed: false, busy: true}` without touching the directory.
  - If `<TempDir>/bundles/<sanitized run_id>/` does not exist, it returns `{removed: false, busy: false}` without error.
  - Otherwise it removes `<TempDir>/bundles/<sanitized run_id>/` and returns `{removed: true, busy: false}`.
- Only `RunActor.DeleteRun` triggers worker bundle cleanup:
  - It enumerates registered workers via `ProcessRegistryActor.ListProcesses` and invokes `cleanup_run` on each unique non-`DEAD` (`HEALTHY` or `SUSPECT`) worker `AppID`.
  - Cleanup uses a bounded timeout, ignores/logs errors best-effort, NEVER fails `DeleteRun` on unreachable/busy workers, NEVER blocks `DeleteRun` beyond the bounded timeout, and NEVER deadlocks Dapr actor turn locks.
  - `RunActor.DeleteRun` also continues to remove its own control-plane `BundleExtractDir`.

**Independent Test**:
- Tier 1 tests in `internal/worker/daemon/daemon_test.go` verify `cleanup_run` removes the bundle directory, returns `busy: true` when a task of that run is active, and returns `removed: false` without error for unknown runs.
- Tier 1 tests in `internal/actor/run/run_test.go` verify `DeleteRun` calls `cleanup_run` on `HEALTHY` and `SUSPECT` workers (skipping `DEAD` workers and deduplicating `AppID`s) using fake registry + fake invoker, and does not block past `WorkerCleanupTimeout` when a worker hangs.
- Tier 2 integration tests in `internal/cli/...`, `internal/server/...`, and `internal/worker/daemon/...` pass, including real `sofa server --dev` E2E where `sofa delete` removes `<state>/worker-tmp/bundles/<run_id>`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Remove control-plane `HostOS`, `TempRoot`, `BundleDir`, and `PluginResolve.PluginDir` from `server.RunStartDefaults`, `run.StartRunReq`, `run.persistedRunState`, `node.StartOrResumeReq`, `node.DispatchSpec`, `merge.EnqueueCandidateReq`, `merge.CandidateTicket`, `daemon.StartNodeRequest`, `daemon.RunMergeCandidateRequest`, and `daemon.PluginResolveConfig`.
- **FR-002**: Carry `PlanBlobKey`, `PlanSHA256`, and `BundlePath` across `RunActor` -> `NodeActor`/`MergeActor` -> `WorkerDaemon` dispatch requests (`StartOrResumeReq`, `EnqueueCandidateReq`, `StartNodeRequest`, `RunMergeCandidateRequest`).
- **FR-003**: `WorkerDaemon` MUST lazily download `.sofaplan` from `Options.BlobStore`, verify `PlanSHA256`, extract via `plan.LoadArchive` into `<TempDir>/bundles/<sanitized run_id>/` with an atomic completion marker, deduplicate concurrent extractions per run with `golang.org/x/sync/singleflight`, reuse valid cached extractions, fail clearly on SHA mismatch, read `plugins.lock.json` from the extracted archive into `PluginResolveOpts.LockFile`, and populate `HostOS = runtime.GOOS`, `TempRoot = Options.TempDir`, `PluginResolveOpts.PluginDir = Options.PluginDir`, and `BundleDir` (computed worker-side preserving `BundlePath` subdirectory semantics as hand-off to Slice 037).
- **FR-004**: `WorkerDaemon` MUST handle `"cleanup_run"` Dapr Service Invocation (`{run_id}` -> `{removed, busy}`), refusing busy runs (`busy: true`), returning `{removed: false}` for absent directories without error, and removing `<TempDir>/bundles/<sanitized run_id>/` otherwise.
- **FR-005**: `RunActor.DeleteRun` MUST enumerate `HEALTHY`/`SUSPECT` workers via `ProcessRegistryActor.ListProcesses` and best-effort invoke `cleanup_run` on each unique worker `AppID` under a bounded timeout without blocking delete beyond timeout or deadlocking actor turn locks, in addition to removing its control-plane `BundleExtractDir`.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` returns empty.
- **SC-002**: `go vet ./...` passes cleanly.
- **SC-003**: `go test ./... -count=1` passes 100%.
- **SC-004**: `go test -tags=integration ./internal/cli/... ./internal/server/... ./internal/worker/daemon/... -count=1` passes 100% and verifies `sofa delete` removes `<state>/worker-tmp/bundles/<run_id>`.
- **SC-005**: `git status` shows only Slice `036` changes (uncommitted for parent review).
