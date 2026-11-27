# Implementation Plan: Move Worker/Dynamic-Worker `gatekeeper` Into `qc` (`034-qc-nested-gatekeeper`)

**Branch**: `034-qc-nested-gatekeeper` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/034-qc-nested-gatekeeper/spec.md)

## Summary

Move `gatekeeper` blocks from `worker` / `dynamic_worker.dynamic.content` into `qc`, add `qc.max_gatekeeper_reworks` (default 3, >= 0), make `qc.criteria` optional when at least one `gatekeeper` is present, execute gatekeepers inside the resolved QC sandbox before optional LLM QC, and update Plan export, deferred HCL expressions, Apply-time evaluation, docs, and examples while leaving `merge` unchanged.

## Technical Context

- **Language / Dependencies**: Go 1.27, `github.com/lonegunmanb/golden` (recursive `ExpandDynamicBlocks`, `MetaAttributeNames`, `MetaNestedBlockNames`), `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty`.
- **Testing**:
  - Tier 1 (`go test ./... -count=1`): `internal/worker/spec`, `internal/planner`, `internal/applyeval`, `internal/worker/runner`, `examples`.
  - Tier 2 (`go test -tags=integration ./internal/worker/runner/... -count=1`): real Podman container tests.
- **Constraints**:
  - Do not `git commit` or `git push`.
  - Never re-implement `golden` capabilities.
  - Keep `merge` block semantics untouched while continuing to share `workerspec.DecodeGatekeeper`.

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored under `specs/034-qc-nested-gatekeeper/` prior to production code changes.
- **Principle II (Strict TDD)**: Failing Tier 1 tests written and verified failing before implementing production changes.
- **Principle III (Pure Core & Real Boundaries)**: `internal/worker/spec` and `internal/applyeval` stay pure; `WorkerRunner` tests use fake `SandboxEngine` and scripted `SessionLauncher`; Tier 2 tests exercise real Podman.
- **Principle VI (Research-First & Surgical Sizing)**: Inspected `golden` `ExpandDynamicBlocks`, `worker/spec`, `merge/spec`, `planner/export`, `applyeval`, `runner`, `actor/node`, and `examples` before scoping.

## Complexity Tracking

No constitution exceptions required.
