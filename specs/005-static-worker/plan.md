# Implementation Plan: Static Workers and Nested Execution Contracts

**Branch**: `005-static-worker` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Remediate and complete the `worker "<name>"` block in `internal/worker/spec` and `internal/planner` so it enforces strict HCL/cty diagnostics, full `docs/hcl-contract.md` schema parity (`permission`, `max_protocol_attempts`, `timeout`, `tool_call_quota`, `allowed_tools`, `disallowed_tools`, `skill_directories`, `skills`, `disabled_skills`, `retry`), strict block-reference `kind` checks, `fallback.artifacts = [artifact(...)]`, native `golden` `dynamic` nested block expansion, complete `DeferredExpression` serialization, `ExecuteDuringPlan()` module tool/MCP registration and `based_source` repository URL resolution, `locals`/instance-aware `fallback` positive ancestry validation, and deep-copied `WorkerBlock.WorkerConfig() Config` snapshots.

## Technical Context

- **Language/Version**: Go 1.27.3
- **Core Dependencies**: `github.com/lonegunmanb/golden`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty/cty`
- **Testing**: Pure offline Tier 1 unit, planner, and cold-start subprocess tests (`go test ./... -count=1`)
- **Upstream Dependencies Consumed**: `001` (`internal/config`, `internal/spec`), `002` (`internal/source`), `003` (`internal/provider`, `internal/mcp/spec`, `internal/sandbox/spec`, `internal/planner/defaults.go`), `004` (`internal/tool/spec`), `004b` (`internal/harness/spec`)

## Constitution Check

- **Principle I (Spec-First)**: Complete `spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/worker-block.md`, `quickstart.md`, and TDD-ordered `tasks.md` under `specs/005-static-worker/`.
- **Principle II (Strict TDD)**: Failing Red tests added before production fixes for every defect and acceptance requirement.
- **Principle III (Pure Core & Decoupled Interfaces)**: `internal/worker/spec` defines a `ModuleRegistry` consumer interface implemented by `*planner.SofaConfig`; zero direct imports from `internal/worker/spec` to `internal/planner`, `internal/tool/spec`, `internal/mcp/spec`, or `internal/source`.
- **Principle VI (Research-First & Golden Reuse)**: `WorkerBlock` delegates `variable`, `locals`, `for_each`, `depends_on`, `precondition`, `dynamic` (`ExpandDynamicBlocks`), and DAG traversal directly to `golden`. Only `CustomDecode` is implemented to support Plan-time `cty.UnknownVal` deferred fields per frozen Stage 1 blueprint `docs/stage1/005-static-worker.md`.

## Design Decisions

1. **Zero Swallowed Diagnostics & Strict `cty` Helpers**:
   - Remove `evalAttr`. Every `Expr.Value(ctx)` returns `diags` directly on error.
   - Dedicated strict helpers (`evalKnownString`, `evalAllowedUnknownString`, `evalWholeInt`, `evalKnownBool`, `evalStringSlice`, `evalIntMap`, `validateBlockReference`) inspect `UnmarkDeep()`, reject unknown/null where prohibited, reject implicit number/bool-to-string coercion, and check `bf.IsInt()` + `int` bounds.
2. **Nested Block Schema & `golden` `dynamic` Expansion**:
   - Pre-validate `dynamic` block labels against allowed worker nested block types (`artifact`, `import_artifact`, `gatekeeper`, `qc`, `fallback`, `tool_use`, `retry`) and pre-seed static `artifact`/`import_artifact` context so `block.ExpandDynamicBlocks(ctx)` evaluates `artifact(...)` and `import_artifact.*` cleanly inside `dynamic` blocks and sibling blocks.
   - When `ExpandDynamicBlocks` turns `fallback.target` into a `LiteralValueExpr`, recover the original traversal from `targetAttr.ExprString()` via `hclsyntax.ParseExpression`.
3. **Complete `DeferredExpression` (`internal/worker/spec/deferred.go`)**:
   - Triggered whenever `based_source`, `system_prompt`, `prompt`, any `import_artifact.sources`, or any `tool_use.input` is not wholly known.
   - Serializes all non-meta top-level attributes and all nested blocks (`retry`, `artifact`, `import_artifact` with effective `path`, `tool_use`, `gatekeeper`, `qc`, `fallback` with quoted string `target`), verified by `hclsyntax.ParseExpression`.
4. **Module-Scoped Checks & URL Resolution in `ExecuteDuringPlan()`**:
   - `WorkerBlock.ExecuteDuringPlan()` queries `w.Config().(ModuleRegistry)` to verify registered `go_tool`/`external_tool` IDs, `mcp_server` tool/resource IDs, `tool_use.input` field names/types against `InputType`, `based_source` repository `SourceURL` (following `local.*`, `source.*`, upstream `worker.*`, or single root `source`), and expanded `fallback` target existence + positive ancestry.
   - `internal/planner/worker_fallback.go` validates `Init`-time fallback syntax and positive ancestry across `local.*` and `depends_on` while respecting `for_each` base addresses, and `SofaConfig` validates post-expansion instance ancestry during `ExecuteDuringPlan()`.
5. **Immutable Snapshots**:
   - `WorkerBlock` stores planned config unexported (`planned Config`) and exposes `WorkerConfig() Config` returning `w.planned.Clone()`.

## Files

```text
internal/worker/spec/{artifact,deferred,errors,fallback,gatekeeper,qc,schema}.go
internal/worker/spec/worker_test.go
internal/planner/{config,defaults,worker_fallback}.go
internal/planner/{cold_start_test,worker_test}.go
specs/005-static-worker/{spec,plan,research,data-model,quickstart,tasks}.md
specs/005-static-worker/contracts/worker-block.md
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
