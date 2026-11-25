# Phase 0 Research: Configuration Foundation and Outputs

## Decision: Pin the inspected `golden` revision

**Decision**: Use `github.com/lonegunmanb/golden@v0.0.0-20261006003654-0f1df693e62c`, the latest revision inspected on 2026-11-25.

**Rationale**: The repository is available, module metadata resolves successfully, and its source exposes the required `AsHclBlocks`, `InitConfig`, `BaseConfig`, `BaseBlock`, `PlanBlock`, `SingleValueBlock`, `RegisterBlock`, `SensitiveMark`, native variable/local blocks, and `RunPlan` behavior. Pinning the inspected pseudo-version prevents a moving default branch from changing semantics mid-slice.

**Alternatives considered**:

- Follow the default branch dynamically: rejected because builds would not be reproducible.
- Reimplement the missing DAG or variable behavior locally: prohibited by the constitution and unnecessary because the upstream behavior exists.

## Decision: Delegate variables, locals, DAG construction, and traversal to `golden`

**Decision**: sofa only performs a preflight variable-type scan and delegates all evaluation to `golden.InitConfig` and `golden.RunPlan`.

**Rationale**: Source inspection confirms that `golden.InitConfig` constructs blocks, builds the DAG, and runs pre-plan work. `RunPlan` owns ordered traversal and cycle detection. `VariableBlock.ExecuteBeforePlan` parses the variable type, reads values with precedence, converts to the declared type, applies defaults, and runs validations. Duplicating any of this would diverge from upstream behavior.

**Alternatives considered**:

- Register custom variable/local blocks: rejected because `golden` already registers them in package initialization.
- Perform a second graph walk to order blocks: rejected because it duplicates DAG traversal and risks order drift.

## Decision: Reject missing or dynamic variable types before initialization

**Decision**: Scan parsed `variable` blocks before `golden.InitConfig`; require an explicit concrete `type`; use `typeexpr.TypeConstraintWithDefaults` and shared type validation.

**Rationale**: Current upstream `VariableBlock` treats a missing type as allowed and also accepts `cty.DynamicPseudoType`. The sofa contract requires a stricter boundary. Performing the scan before initialization guarantees the error occurs before any variable promotion or block evaluation.

**Alternatives considered**:

- Fork or monkey-patch upstream variable parsing: rejected as invasive and non-portable.
- Permit `any` and fail later: rejected because it produces weaker diagnostics and violates the frozen contract.

## Decision: Track sensitive variable names in `SofaConfig`

**Decision**: Parse each variable's optional `sensitive = true` during preflight and apply `golden.SensitiveMark` to that variable value inside an overridden `EvalContext()`.

**Rationale**: The mark automatically propagates through cty expressions and is recognized by `golden` formatting/redaction. A string mark constant is required; a private mark type would be invisible to upstream.

**Alternatives considered**:

- Mark only the final output: rejected because the source name and all downstream derived values need consistent sensitivity semantics.
- Redact by field name or string matching: rejected as fragile and outside the planning engine's mark model.

## Decision: Parse each file twice and derive blocks at the loader boundary

**Decision**: For each accepted file, parse the same bytes with `hclsyntax.ParseConfig` and `hclwrite.ParseConfig`, collect all diagnostics, and call `golden.AsHclBlocks` only when parsing succeeds.

**Rationale**: `golden.AsHclBlocks` needs corresponding syntax and write blocks and uses the write representation for expression token access. Parsing both representations in one bounded loader creates a stable adapter for the remainder of Stage 1.

**Alternatives considered**:

- Parse only syntax HCL: rejected because `golden` block adapters require write blocks.
- Keep open file handles in the loaded result: rejected because parsed ASTs are sufficient and immutable.

## Decision: Register blocks from planner package initialization

**Decision**: `internal/planner.RegisterBlocks()` owns all sofa block registration. A package `init()` calls it, and the function's `sync.Once` begins with `defaults.SetDefaults(&struct{}{})` before registering custom blocks. `NewSofaConfig` also calls the idempotent function defensively.

**Rationale**: `golden.AsHclBlocks` only adds the leading empty label for a one-label custom block after that block type is registered. A caller that needs to adapt blocks imports `internal/planner`, so its package initialization completes registration before `LoadDirectoryContext` runs. Keeping registration in `planner` preserves the frozen Stage 1 layout and prevents `internal/config` from depending on concrete block packages, which would create future import cycles as tool and worker specs consume config primitives.

**Alternatives considered**:

- Register from `planner.NewSofaConfig` only: rejected because cold-start loading would adapt custom blocks before their type is known.
- Introduce a shared registration package imported by `config`: rejected because it reverses the dependency direction and creates future import cycles.
- Register identical block sets in both packages: rejected because duplicate global state is difficult to keep consistent.

## Decision: Override helper functions on `BaseConfig`

**Decision**: Build the complete function map as `hclfuncs.Functions(basedir)` plus four overrides, then assign it to `BaseConfig.OverrideFunctions`.

**Rationale**: `BaseConfig.EmptyEvalContext()` merges the standard function set with overrides, preserving upstream functions while replacing or adding sofa-specific names.

**Alternatives considered**:

- Rebuild the whole function set: rejected because it would omit upstream functions and silently break configuration authors.
- Alter `hclfuncs` globally: rejected because function behavior would leak across independent configurations.

## Decision: Use cty function argument types to preserve unknown JSON result types

**Decision**: Define `jsondecodewithtype` with a string JSON argument and `typeexpr.TypeConstraintType` expression argument. Keep JSON unknown handling to the cty function framework's default return-type inference.

**Rationale**: The cty function framework can derive an unknown result with the exact declared return type when the JSON argument is unknown and the type expression is known. This is the primitive later dynamic workers need for static planning.

**Alternatives considered**:

- Decode unknown values eagerly: impossible because no JSON payload exists at plan time.
- Return a generic dynamic value: rejected because downstream blocks require precise static types.

## Decision: Model `output` as a named single-value planning block

**Decision**: Implement `Type() == ""`, `BlockType() == "output"`, and `AddressLength() == 2`; store value as `cty.Value`; retain optional description and sensitive flag; publish a stable snapshot through `SingleValueBlock`.

**Rationale**: `golden` automatically adds the leading empty label for single-label block types. A `cty.Value` field accepts unknown apply-time values without decoding them into Go strings. Single-value publication makes `output.<name>` available to later references and snapshots.

**Alternatives considered**:

- Decode value to a concrete Go type: rejected because values may be unknown and heterogeneous.
- Implement `Valuable` without the single-label rules: rejected because outputs do not support `for_each` and would receive an unnecessary nesting layer.

## Upstream inspection notes

- `golden` registers `variable` and `local` in package initialization; sofa must not register duplicates.
- `golden.InitConfig` builds the DAG and runs all pre-plan blocks; `golden.RunPlan` traverses it.
- `golden.SensitiveMark` is the string constant `"sensitive"`.
- `BaseBlock.EvalContext()` delegates to the owning `Config.EvalContext()`, so sofa's path and variable marks can be injected centrally.
- `RegisterBlock` reflects on the sample block and requires an embedded `BaseBlock` field.
- `AsHclBlocks` expects matching syntax/write block positions, reinforcing the need for dual parsing with deterministic file order.
- Custom one-label blocks are recognized by `AsHclBlocks` only after registration, so callers import `planner`, whose package initialization registers them before loading begins.

## Unresolved clarifications

None.
