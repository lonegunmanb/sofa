# Implementation Plan: Worker Run-Scoped `.sofaplan` Bundle Extraction, Control-Plane Decoupling, and Deferred Cleanup (`036-worker-run-bundle-mount`)

**Branch**: `036-worker-run-bundle-mount` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/036-worker-run-bundle-mount/spec.md)

## Summary

Decouple control-plane host paths (`BundleDir`, `TempRoot`, `HostOS`, `PluginResolve.PluginDir`) from worker dispatch across `server`, `cli`, `run`, `node`, `merge`, and `daemon`; add `PlanBlobKey`, `PlanSHA256`, and `BundlePath` to node and merge dispatch payloads; implement worker-side lazy `.sofaplan` download, SHA256 verification, `singleflight`-deduplicated `plan.LoadArchive` extraction into `<TempDir>/bundles/<sanitized run_id>/` with completion marker reuse and worker-side `plugins.lock.json` loading; and add `cleanup_run` on `WorkerDaemon` invoked best-effort with bounded timeout by `RunActor.DeleteRun` via `ProcessRegistryActor.ListProcesses`.

## Technical Context

- **Language / Dependencies**: Go 1.27, `golang.org/x/sync/singleflight` (already present in `go.mod` module graph), `archive/zip` / `plan.LoadArchive`, Dapr Go SDK actor & service invocation.
- **Testing**:
  - Tier 1 (`go test ./... -count=1`): unit tests in `internal/worker/daemon`, `internal/actor/run`, `internal/actor/node`, `internal/actor/merge`, `internal/server`, `internal/cli`.
  - Tier 2 (`go test -tags=integration ./internal/cli/... ./internal/server/... ./internal/worker/daemon/... -count=1`): real `daprhost` + `sofa server --dev` E2E verification including `<state>/worker-tmp/bundles/<run_id>` creation and `sofa delete` removal.
- **Constraints**:
  - Do not `git commit` or `git push`.
  - Respect Dapr non-reentrant actor turn-lock rules.
  - Preserve existing container `/.sofa/bundle` mount semantics (worker resolves `<extract>/<BundlePath>` with fallback to `<extract>/config` / `<extract>`); container mount relocation belongs to Slice 037.

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored under `specs/036-worker-run-bundle-mount/` before production edits.
- **Principle II (Strict TDD)**: Failing tests written first in `daemon_test.go` and `run_test.go` and confirmed failing before implementing production changes.
- **Principle III (Pure Core & Real Boundaries)**: Explicit consumer-boundary interfaces (`ProcessLister`, `WorkerBundleCleaner`) on `RunActor`; real `blob.LocalStore` and `plan.SaveArchive`/`plan.LoadArchive` in daemon unit tests and real `daprhost` in Tier 2 tests.
- **Principle VI (Research-First & Surgical Sizing)**: Inspected `run_actor.go`, `node/actor.go`, `merge/actor.go`, `registry/actor.go`, `daemon.go`, `server/handlers.go`, `cli/server_cmd.go`, `cli/worker_cmd.go`, `plan/archive.go`, `sandbox/runtime/spec.go`, `worker/runner/worker_runner.go`, and `tool/runtime/executor.go` prior to planning.

## Complexity Tracking

No constitution exceptions required.
