# Implementation Plan: Dynamic Workers and Deterministic Merge Blocks

**Branch**: `006-dynamic-worker-and-merge` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Implement Plan-time decoding, type derivation, root-default injection, cross-block `SourceURL` / `fallback` positive-ancestry propagation, and value projection for `dynamic_worker "<name>"` (`internal/worker/spec/dynamic.go`) and deterministic `merge "<name>"` (`internal/merge/spec/schema.go`), wired into `internal/planner/{config,defaults,worker_fallback}.go`.

## Technical Context

- **Language/Version**: Go 1.27.3
- **Core Dependencies**: `github.com/lonegunmanb/golden`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty/cty`
- **Testing**: Pure offline Tier 1 unit, planner, and cold-start subprocess tests (`go test ./... -count=1`)
- **Upstream Dependencies Consumed**: `002` (`internal/source`), `003` (`internal/sandbox/spec`, `internal/provider`, `internal/planner/defaults.go`), `004` (`internal/tool/spec`), `004b` (`internal/harness/spec`), `005` (`internal/worker/spec` `WorkerBlock`, `GatekeeperConfig`, `DecodeGatekeeper`, `buildDeferredExpression`, `ValidateBlockReference`)

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts under `specs/006-dynamic-worker-and-merge/` prior to implementation.
- **Principle II (Strict TDD)**: Failing Red tests written in `internal/worker/spec/dynamic_test.go`, `internal/merge/spec/schema_test.go`, `internal/planner/dynamic_worker_merge_test.go`, and `internal/planner/cold_start_test.go` before production code.
- **Principle III (Pure Core & Decoupled Interfaces)**: `internal/merge/spec` defines its own `ModuleRegistry` consumer interface implemented by `*planner.SofaConfig`, reuses `workerspec.DecodeGatekeeper` / `GatekeeperConfig` / `ValidateBlockReference`, and has zero imports from `internal/planner` or `internal/source`.
- **Principle VI (Research-First & Golden Reuse)**: `DynamicWorkerBlock` implements `golden.CustomDecode` solely for its Apply-time `cty.UnknownVal` `dynamic "worker"` iteration + `iterator` attribute (explicitly approved in `AGENTS.md` §5 and `docs/stage1/006-dynamic-worker-and-merge.md` §3.1), while delegating `content` validation to `WorkerBlock` (including `ExpandDynamicBlocks` for nested `dynamic "gatekeeper"`) and delegating `dynamic "gatekeeper"` expansion in `MergeBlock` to `block.ExpandDynamicBlocks(ctx)`.

## Design Decisions

1. **Dynamic Worker `for_each` & `iterator` Handling (`internal/worker/spec/dynamic.go`)**:
   - Inspect raw `block.NestedBlocks()` directly without calling `block.ExpandDynamicBlocks` on `dynamic_worker`.
   - Extract `iterator` strictly with `hcl.AbsTraversalForExpr(iteratorAttr.Expr)`; reject non-single-step traversals, invalid identifiers, and reserved root names when explicitly provided.
   - When `iterator` is omitted (`"worker"`), clone `ctx.Variables["worker"]`'s attributes (if present as a known non-null object), reject conflicts with static workers named `"key"` or `"value"`, and add `"key"` + `"value"` so both `${worker.key}`/`${worker.value}` and `${worker.discover.work_branch}` resolve cleanly.
   - Construct a synthetic `*golden.HclBlock` (`Type: "worker"`, `Labels: ["", b.Name()]`) and run `WorkerBlock.Decode`, `buildDeferredExpression` (unconditionally for `ContentTemplate`), and `ExecuteDuringPlan()` checks (passing `dynamic_worker.<name>` to `ValidateWorkerFallbackTargets`).
2. **Deterministic `MergeBlock` (`internal/merge/spec/schema.go`)**:
   - Export `GatekeeperConfig`, `DecodeGatekeeper`, `ValidateBlockReference`, and shared `cty` helpers from `internal/worker/spec` to avoid duplication.
   - Support `dynamic "gatekeeper"` via `block.ExpandDynamicBlocks(ctx)`.
   - Enforce strict `sources` type validation (string list/set/tuple; disallow empty static lists; allow unknown `list(string)`/`set(string)`), conditional `sandbox` requirement when gatekeepers are present, `SourceURL` consistency across `based_source` and `sources`, and unpreserved `source.branch` protection on `target_branch`.
3. **Planner Wiring (`internal/planner/{config,defaults,worker_fallback}.go`)**:
   - Register `&workerspec.DynamicWorkerBlock{}` and `&mergespec.MergeBlock{}` in `RegisterBlocks()`.
   - Inject orthogonal root defaults into `dynamic_worker`'s `dynamic "worker" -> content` block (cloning both syntax and `hclwrite` trees cleanly) and conditional `sandbox` default into `merge` blocks that declare `gatekeeper` or `dynamic "gatekeeper"`.
   - Extend `LookupWorkerSourceURLs` / `ModuleRegistry` and `worker_fallback.go` (`skipFallback` for `dynamic_worker`, `Init`-time fallback checking inside `dynamic_worker`, and positive-ancestry traversal across `dynamic_worker` and `merge`).

## Files

```text
internal/worker/spec/{schema.go,gatekeeper.go,dynamic.go,dynamic_test.go}
internal/merge/spec/{schema.go,schema_test.go}
internal/planner/{config.go,defaults.go,worker_fallback.go,dynamic_worker_merge_test.go,cold_start_test.go}
specs/006-dynamic-worker-and-merge/{spec,plan,research,data-model,quickstart,tasks}.md
specs/006-dynamic-worker-and-merge/contracts/dynamic-worker-and-merge.md
specs/006-dynamic-worker-and-merge/checklists/requirements.md
```

## Post-Design Check

All Constitution I–VI checks pass without unapproved exceptions.
