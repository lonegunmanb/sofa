# Feature Specification: Dynamic Workers and Deterministic Merge Blocks

**Feature Branch**: `006-dynamic-worker-and-merge`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 006 from `docs/stage1/006-dynamic-worker-and-merge.md` and `docs/hcl-contract.md`.

## User Scenarios & Testing

### User Story 1 - Plan a `dynamic_worker` over Apply-time typed collections (Priority: P1)

A workflow author defines a `dynamic_worker "<name>"` block containing `parallelism`, `serial`, and a single `dynamic "worker"` block with `for_each`, optional `iterator`, and a single `content` block. Even when `for_each` evaluates to a `cty.UnknownVal` (such as `jsondecodewithtype(worker.discover.result, ...).checks`), planning derives the element value type, injects `iterator` (`<name>.key` and `<name>.value`) into a child evaluation context without clobbering existing `worker.<name>` references when `iterator` is omitted, validates `content` as a virtual `WorkerBlock`, generates `ContentTemplate`, and exposes `.workers` and `.work_branches`.

**Independent Test**: Plan `dynamic_worker` blocks with explicit `iterator = check` and omitted `iterator` (alongside `based_source = worker.discover.work_branch`), `for_each = {}`, invalid `for_each` types (`null`, `123`, `"abc"`, `["a", "b"]`, `toset([1, 2])`), reserved `iterator` names (`var`, `worker`, `source`, `merge`, string literal `"check"`), and typed field checks where `${check.value.rule}` passes and `${check.value.wrong_field}` fails at Plan time.

### User Story 2 - Plan a deterministic `merge` block with strict source responsibility (Priority: P1)

A workflow author defines a `merge "<name>"` block to serially integrate candidate branches (`sources`) onto `based_source` (optionally targeting an explicit `target_branch`), with deterministic `strategy = "rebase"`, `max_reworks`, `human_approval`, and optional `gatekeeper` checks. `merge` strictly forbids LLM/conflict-repair attributes or blocks (`model`, `prompt`, `system_prompt`, `harness`, `model_provider`, `conflict_resolution`, `qc`, `fallback`, `retry`), enforces non-empty static `sources` while allowing unknown dynamic `work_branches` / `concat(...)`, enforces `sandbox` when any `gatekeeper` is present, and guards against overwriting unpreserved `source` branches.

**Independent Test**: Plan `merge` blocks with valid defaults, static and dynamic `sources`, `target_branch` checks (known branch names and unpreserved vs preserved `source.branch`), unsupported LLM/conflict attributes/blocks, and conditional `gatekeeper` + `sandbox` validation.

### User Story 3 - Wire `dynamic_worker` and `merge` into root defaults, DAG, fallback ancestry, and cold-start planning (Priority: P1)

`SofaConfig` registers `DynamicWorkerBlock` and `MergeBlock`, applies orthogonal `0`/`1`/`>=2` root defaults (`sandbox`, `harness`, `model_provider`, respecting explicit attributes including `model_provider = null`) to `dynamic_worker`'s `content` block and conditional root `sandbox` default to `merge` (only when `gatekeeper` or `dynamic "gatekeeper"` is present), propagates `SourceURL` and positive ancestry for `fallback` across `worker -> dynamic_worker -> merge -> worker -> preserve -> output`, and supports cold-start loading without prior explicit registration.

**Independent Test**: Plan the full `source -> worker.discover -> dynamic_worker.verify_notifications -> merge.integrate -> worker.post_merge -> preserve.final -> output` pipeline, test root default permutations on both `dynamic_worker` and `merge`, and run a cold-start subprocess test `TestColdStartLoadDynamicWorkerAndMergeBeforeNewSofaConfig`.

### Edge Cases

- `DynamicWorkerBlock` MUST NOT invoke `golden`'s `HclBlock.ExpandDynamicBlocks` on `dynamic_worker` itself because `for_each` can be unknown and `iterator` is supported; however, nested `dynamic` blocks inside `content` (e.g. `dynamic "gatekeeper"`) are still expanded by `WorkerBlock.Decode`.
- `iterator` MUST be extracted via `hcl.AbsTraversalForExpr`, never `iteratorAttr.Expr.Value(ctx)`; string literals or multi-step traversals fail planning.
- When `iterator` is omitted (default `"worker"`), existing `ctx.Variables["worker"]` attributes are preserved alongside `"key"` and `"value"` (rejecting only if a static worker is literally named `"key"` or `"value"`).
- Known empty object `{}` in `for_each` is valid (0 instances); unknown `map(dynamic)` or non-map/object/set-of-string collections fail planning.
- `DynamicWorkerBlock.Value()` exposes `.workers[...].result` if and only if `content` configures a terminating tool (`terminate_tool_id` or `tool_use { terminate = true }`).
- `merge.sources` rejects implicit number/bool-to-string coercion, `null` elements, and empty static lists (`[]`), while allowing unknown `list(string)` / `set(string)` (such as `dynamic_worker.<name>.work_branches`).
- `merge.target_branch` rejects unknown strings, invalid branch names, and unpreserved `source.branch` values for the resolved repository.
- `merge` without `gatekeeper` requires no `sandbox` (and triggers no root `sandbox` resolution even if 0 or >=2 sandboxes exist); `merge` with `gatekeeper` or `dynamic "gatekeeper"` requires a `sandbox`.

## Requirements

- **FR-001**: `DynamicWorkerBlock` MUST implement `golden.CustomDecode`, `golden.PlanBlock`, and `golden.SingleValueBlock` (`Type() == ""`, `BlockType() == "dynamic_worker"`, `AddressLength() == 2`, `CanExecutePrePlan() == false`) with safe non-empty label validation.
- **FR-002**: `DynamicWorkerBlock` MUST allow only `parallelism` (`> 0` known whole int, `null` -> `nil`), `serial` (known bool, default `false`), `golden.MetaAttributeNames`, `precondition`, and **exactly 1** `dynamic "worker"` block containing only `for_each` (required), `iterator` (optional), and **exactly 1** unlabeled `content` block.
- **FR-003**: `iterator` MUST be parsed via `hcl.AbsTraversalForExpr` as a single valid identifier, rejecting reserved root names when explicitly provided, and defaulting to `"worker"` (merging `"key"` and `"value"` into existing `worker` object) when omitted.
- **FR-004**: `for_each` MUST accept known/unknown `map(<T>)` (non-capsule, non-untyped-unknown), `set(string)`, and `object(...)` (homogeneous type or heterogeneous `DynamicVal`, with known `{}` mapping to `UnknownVal(String)`), rejecting all other types and `null`.
- **FR-005**: `content` MUST be validated as a virtual `WorkerBlock` under the child `EvalContext` (catching attribute/type/reference errors such as `${check.value.wrong_field}` at Plan time), normalized into `ContentTemplate` via `buildDeferredExpression`, and exposed via deep-copied `DynamicWorkerConfig() DynamicConfig`.
- **FR-006**: `DynamicWorkerBlock.Value()` MUST return `cty.DynamicVal` pre-plan and `{ address, kind = "dynamic_worker", name, workers: UnknownVal(Map(workerItemType)), work_branches: UnknownVal(List(String)) }` post-plan, including `"result": String` in `workerItemType` iff `content` declares a terminating tool.
- **FR-007**: `MergeBlock` MUST implement `golden.CustomDecode`, `golden.PlanBlock`, and `golden.SingleValueBlock` (`Type() == ""`, `BlockType() == "merge"`, `AddressLength() == 2`, `CanExecutePrePlan() == false`) with safe non-empty label validation.
- **FR-008**: `MergeBlock` MUST allow only `based_source`, `target_branch`, `sources`, `strategy`, `max_reworks`, `human_approval`, `sandbox`, `golden.MetaAttributeNames`, `gatekeeper "<name>"` (reusing `workerspec.DecodeGatekeeper`), `dynamic "gatekeeper"`, and `precondition`; any LLM/conflict-resolution attribute or block MUST fail with an `unsupported` error.
- **FR-009**: `MergeBlock` MUST validate `based_source` (non-null string), `target_branch` (`null` or wholly known valid branch name not targeting an unpreserved `source.branch`), `sources` (non-null string list/set/tuple, rejecting empty static lists), `strategy` (`"rebase"`), `max_reworks` (`>= 0`, default `2`), `human_approval` (`"optional"|"required"|"disabled"`, default `"optional"`), conditional `sandbox` (required iff `len(Gatekeepers) > 0`), `SourceURL` resolution, and deep-copied `MergeConfig() Config`.
- **FR-010**: `MergeBlock.Value()` MUST return `cty.DynamicVal` pre-plan and `{ address, kind = "merge", name, work_branch: UnknownVal(String), commit_sha: UnknownVal(String) }` post-plan.
- **FR-011**: `internal/planner` MUST register `DynamicWorkerBlock` and `MergeBlock`, inject orthogonal root defaults into `dynamic_worker`'s `content` block and conditional root `sandbox` default into `merge` blocks with gatekeepers, propagate `SourceURL` across `dynamic_worker` and `merge`, and validate `fallback` positive ancestry across `dynamic_worker` and `merge` blocks.

## Success Criteria

- **SC-001**: All unit tests in `internal/worker/spec/dynamic_test.go` and `internal/merge/spec/schema_test.go` pass following Red-Green-Refactor.
- **SC-002**: All end-to-end planner, root defaults, fallback ancestry, and cold-start subprocess tests in `internal/planner/dynamic_worker_merge_test.go` and `internal/planner/cold_start_test.go` pass.
- **SC-003**: `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` pass with zero errors.
- **SC-004**: `internal/worker/spec` and `internal/merge/spec` remain decoupled from `internal/planner` and `internal/source` via consumer interfaces.
