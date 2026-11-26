# Research Notes: Dynamic Workers and Deterministic Merge Blocks

## 1. `golden` `HclBlock.ExpandDynamicBlocks` Limitations

Inspection of `golden/hcl_block.go` confirms:
- `ExpandDynamicBlocks` calls `forEachValue.CanIterateElements()` and `forEachValue.ElementIterator()`, which fail on Plan-time `cty.UnknownVal` collections produced by `jsondecodewithtype(worker.discover.result, ...)`.
- `ExpandDynamicBlocks` hardcodes the iterator variable name to `block.Labels[0]` (`"worker"`) and ignores any `iterator = <name>` attribute.
- Therefore, `DynamicWorkerBlock` MUST implement `golden.CustomDecode` and process `dynamic "worker"` directly, while still allowing the virtual `WorkerBlock` inside `content` to call `ExpandDynamicBlocks` for nested `dynamic "gatekeeper"` blocks.

## 2. `golden.SingleValueBlock` with `Type() == ""`

Inspection of `golden/base_config.go` (`EvalContext`) and `golden/block.go` (`SingleValues`) confirms:
- Blocks implementing `golden.SingleValueBlock` (`SourceBlock`, `PreserveBlock`, `DynamicWorkerBlock`, `MergeBlock`) populate `ctx.Variables[blockType][block.Name()] = block.Value()` directly without an intermediate `""` subtype key.
- Therefore, `SofaConfig.EvalContext()` does NOT require special unwrapping for `dynamic_worker` or `merge` (unlike `worker`, which implements `golden.Valuable`).

## 3. `golden.dagWalker` and `hclsyntax.Walk`

Inspection of `golden/dag.go` and `golden/dag_walker.go` confirms:
- `dag.buildDag` walks `b.HclBlock().Body` recursively with `hclsyntax.Walk`, automatically creating DAG edges for references inside `dynamic "worker" { for_each = ...; content { ... } }` and `merge` attributes/gatekeepers.
- Because `iterator` is not a registered block type in `golden.refIters`, `dagWalker` ignores `check.*` traversals without error; when `iterator` is omitted (`"worker"`), `worker.key` and `worker.value` look up `"worker.key"` / `"worker.value"`, which do not exist in the DAG (`GetChildren` returns err and `dagWalker` continues cleanly), while `worker.discover.work_branch` creates the real DAG edge from `worker.discover` to `dynamic_worker.<name>`.

## 4. `ValidateWorkerFallbackTargets` Address Alignment

Inspection of `internal/planner/worker_fallback.go` shows:
- `ValidateWorkerFallbackTargets(workerAddress, targets)` checks `dependencies[workerAddress]` in `allBlocks` (from `c.GetVertices()`).
- Because a `dynamic_worker` vertex in `c.GetVertices()` has address `"dynamic_worker." + name` (not `"worker." + name`), `DynamicWorkerBlock.ExecuteDuringPlan()` must pass `d.Address()` (`"dynamic_worker." + name`) to `reg.ValidateWorkerFallbackTargets`, and `walkPositiveBlockTraversals` must treat `dynamic_worker` blocks with `skipFallback = true` so `fallback.target` inside `content` is excluded from positive ancestry.

## 5. `hclwrite` + `hclsyntax` Dual AST Cloning for Root Defaults

Inspection of `internal/planner/defaults.go` and `golden/hcl_block.go` confirms:
- `golden.CloneHclSyntaxBlock` deep-clones nested `Body.Blocks` and `Body.Attributes`.
- `cloneWriteBlock` reconstructs the matching `hclwrite.Block` tree in the same child order so `dynamic_worker -> dynamic "worker" -> content` can be updated in both ASTs without mutating the caller's original `*golden.HclBlock`.
