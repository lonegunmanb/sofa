---
description: "Stage 2 Slice 010 TDD tasks: remove sandbox.entrypoint, add qc.sandbox/profile, align /.sofa/{artifacts,imports} paths"
---

# Tasks

- [X] T001 [Red] Add and update failing unit tests for `sandbox.entrypoint` rejection and export omission in `internal/sandbox/spec/schema_test.go`, `internal/planner/sandbox_test.go`, and `internal/planner/export_test.go`:
  - `sandbox` block with `entrypoint = ["/bin/sh"]` and `entrypoint = null` rejected natively by `golden.Decode` (`unsupported` argument);
  - Exported `sandbox` `NodeSpec.Config` contains no `"entrypoint"` attribute after `ExportPlan` -> `Marshal` -> `Unmarshal` -> `Display`.
- [X] T002 [Red] Add failing unit tests for `qc.sandbox` and `qc.profile` overrides in `internal/worker/spec/worker_test.go`, `internal/worker/spec/dynamic_test.go`, `internal/planner/worker_test.go`, and `internal/planner/export_test.go`:
  - Omitted or `null` `qc.sandbox` defaults to `cty.NullVal(cty.EmptyObject)` and `qc.profile` defaults to `""`;
  - Valid `qc { sandbox = sandbox.sec_audit; profile = "opus-audit"; criteria = ... }` succeeds in both `worker` and `dynamic_worker`, establishes a DAG dependency edge to `sandbox.sec_audit`, preserves attributes in `DeferredExpression`/`ContentTemplate`, and exports `qc.sandbox` / `qc.profile` through `ExportPlan`;
  - Invalid `qc.sandbox` (e.g. `harness.local`) and blank `qc.profile` fail during plan decoding.
- [X] T003 [Red] Add and update failing unit tests for `/.sofa/artifacts` & `/.sofa/imports` path alignment and intra-set overlap validation in `internal/worker/spec/worker_test.go`, `internal/worker/spec/dynamic_test.go`, `internal/planner/worker_test.go`, `internal/planner/module_test.go`, and `internal/planner/export_test.go`:
  - `artifact("<name>").path` and `worker.<w>.artifact.<name>.path` evaluate to `"/.sofa/artifacts/<rel>"` while `.rel_path` equals `"<rel>"` (and update child module `variable "spec_artifact"` object constraint in `internal/planner/module_test.go`);
  - `import_artifact.<name>.path` evaluates to `"/.sofa/imports/<name>"` when `path` is omitted and `"/.sofa/imports/<rel>"` when `path` is specified, including in `DeferredExpression`;
  - Absolute/escaping `artifact.path` and `import_artifact.path` remain rejected;
  - Identical relative paths across `artifact` and `import_artifact` (e.g. `"spec.md"`) succeed, while intra-`artifact` and intra-`import_artifact` duplicates and parent/child overlaps (`"dir"` vs `"dir/a.txt"`) fail;
  - `ExportPlan` exports `"container_path"` alongside relative `"path"` on `worker.artifacts` and `worker.imports`.
- [X] T004 [Green] Remove `Entrypoint` from `internal/sandbox/spec/schema.go` and `encodeSandboxConfig` in `internal/planner/export.go` so `golden.Decode` rejects `entrypoint` natively.
- [X] T005 [Green] Add `Sandbox` and `Profile` support to `internal/worker/spec/qc.go` and `encodeWorkerConfig` (`qcVal`) in `internal/planner/export.go`.
- [X] T006 [Green] Implement `ContainerArtifactsRoot`, `ContainerImportsRoot`, `Artifact.ContainerPath()`, `EffectiveImportRelPath`, `ImportArtifact.ContainerPath()`, `.rel_path` on `artifactValue`, `matchesDeclaredArtifact` alignment in `internal/worker/spec/fallback.go`, separated intra-set path overlap checks in `internal/worker/spec/schema.go`, `import_artifact` container path in `internal/worker/spec/deferred.go`, and `"container_path"` export in `internal/planner/export.go`.
- [X] T007 [Refactor & Verify] Review `docs/hcl-contract.md` alignment, run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, and mark all tasks `[X]`.
