# Research: Static Workers and Nested Execution Contracts

## Decision 1: `golden` `Valuable` + `CustomDecode` and `worker` Namespace Unwrapping

- **Decision**: `WorkerBlock` implements `golden.Valuable` (`Type() == ""`, `BlockType() == "worker"`, `AddressLength() == 2`), and `SofaConfig.EvalContext()` unwraps `ctx.Variables["worker"].GetAttr("")`.
- **Rationale**: `golden.valuesWithEmptyForEach` groups non-`SingleValueBlock` blocks by `b.Type()`, producing a nested `""` attribute (`worker[""].<name>`). Unwrapping that empty key once in `SofaConfig.EvalContext()` lets HCL authors write `worker.<name>.work_branch` and `worker.<name>["key"].work_branch` naturally while preserving native `golden` `for_each` expansion.

## Decision 2: `golden` `ExpandDynamicBlocks` with Labeled Nested Blocks and `fallback.target`

- **Decision**: Pre-validate `dynamic` block labels against valid worker nested block types (`artifact`, `import_artifact`, `gatekeeper`, `qc`, `fallback`, `tool_use`, `retry`) and pre-build the worker's `artifact()`/`import_artifact` context before calling `block.ExpandDynamicBlocks(ctx)`. In `decodeFallback`, if `targetAttr.Expr` was evaluated to `*hclsyntax.LiteralValueExpr` by `ExpandDynamicBlocks`, parse `targetAttr.ExprString()` with `hclsyntax.ParseExpression` before extracting `hcl.AbsTraversalForExpr`.
- **Rationale**: `golden.HclBlock.ExpandDynamicBlocks` indexes `block.Labels[0]` without length checks and eagerly evaluates all sibling nested block attributes into `LiteralValueExpr` while preserving `ExprString()` tokens on the underlying `hclwrite.Attribute`. Pre-validating labels avoids panics, pre-seeding worker locals allows `dynamic "gatekeeper"` and sibling blocks to reference `artifact(...)` / `import_artifact.*`, and falling back to `ExprString()` keeps `target = "worker.tasks"` rejected while preserving `target = worker.tasks`.

## Decision 3: `cty` Strict Scalar Validation Without Implicit Conversion

- **Decision**: Validate scalar `cty.Value` types directly on `UnmarkDeep()` using `Type().Equals(cty.String)`, `Type().Equals(cty.Bool)`, and `Type().Equals(cty.Number)` with `AsBigFloat().IsInt()` + `int` bounds instead of `convert.Convert`.
- **Rationale**: `convert.Convert` silently coerces numbers/booleans into strings (and vice versa for collections) and truncates/accepts non-integers if unchecked. Direct type inspection guarantees `"123"` vs `123`, `1.5`, `true`, `null`, and `UnknownVal` are diagnosed deterministically while preserving outer/nested `golden.SensitiveMark` on retained `cty.Value` fields.

## Decision 4: Consumer Interface Decoupling for `ExecuteDuringPlan()`

- **Decision**: Define `ModuleRegistry` and `ModuleToolInfo` in `internal/worker/spec` and implement them on `*planner.SofaConfig`.
- **Rationale**: Keeps `internal/worker/spec` free of `internal/planner`, `internal/tool/spec`, `internal/mcp/spec`, and `internal/source` imports, matching `001`–`004b` layering while enabling full Plan-time verification of registered tool/MCP IDs, `tool_use.input` constraints, `based_source` repository `SourceURL` resolution, and post-expansion `fallback` positive ancestry.

## Decision 5: `tool_call_quota` & `allowed_tools` / `disallowed_tools` Parity with `r42`

- **Decision**: Enforce `r42` quota rules (`>= 0` whole integers, non-blank keys, reject `mcp_tool_*`, require any `tool_*` key to be configured in the session's `tool_ids`/`terminate_tool_id`/`tool_use`) and `validateMandatoryTool` when `terminate_tool_id` or `tool_use { terminate = true }` is configured (`allowed_tools` non-null must permit it; `disallowed_tools` must not block it). Distinguish `allowed_tools = null` (nil slice) from `allowed_tools = []` (non-nil empty slice).
- **Rationale**: Preserves exact Stage 1 / `r42` session tool gate guarantees during Plan.

## Decision 6: Two-Phase `fallback` Positive-Ancestry Validation

- **Decision**: Validate `fallback` syntax and base-worker positive ancestry (walking `local.*` blocks and `depends_on`, excluding `fallback` blocks) during `SofaConfig.Init`, and validate exact expanded-instance existence (`worker.<name>[<key>]`) and positive ancestry in `WorkerBlock.ExecuteDuringPlan()` via `SofaConfig`.
- **Rationale**: Catching unrelated/self fallback targets during `Init` preserves fast graph error reporting before `for_each` expansion, while post-expansion checks in `ExecuteDuringPlan()` catch unknown `for_each` keys and per-instance ancestry mismatches.
