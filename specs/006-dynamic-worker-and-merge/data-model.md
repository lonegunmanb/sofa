# Data Model: Dynamic Workers and Deterministic Merge Blocks

## `DynamicConfig` (`internal/worker/spec/dynamic.go`)

| Field | Type / Default | Rules |
|---|---|---|
| `Name` | `string` | Non-empty safe label of `dynamic_worker "<name>"` |
| `Parallelism` | `*int` (optional) | `nil` when omitted or `null`; otherwise known finite whole integer `> 0` |
| `Serial` | `bool` (default `false`) | Known `cty.Bool`; `null` resolves to `false` |
| `ForEach` | `cty.Value` (required) | Non-null `map(<T>)` (non-capsule, non-untyped-unknown), `set(string)`, or `object(...)` (known `{}` valid) |
| `ForEachExpr` | `string` | Raw HCL expression string of `dynamic "worker".for_each` |
| `IteratorName` | `string` (default `"worker"`) | Unquoted single-step valid HCL identifier; explicit values reject reserved root names |
| `Content` | `Config` | Virtual `WorkerBlock` planned `Config` (including resolved `SourceURL`, `Artifacts`, `Imports`, `Gatekeepers`, `QC`, `Fallbacks`) |
| `ContentTemplate` | `string` | Normalized parseable HCL object expression generated via `buildDeferredExpression` |

### `DynamicWorkerBlock.Value()`

- Pre-plan: `cty.DynamicVal`
- Post-plan:
  - `address`: `cty.StringVal(b.Address())`
  - `kind`: `cty.StringVal("dynamic_worker")`
  - `name`: `cty.StringVal(b.Name())`
  - `workers`: `cty.UnknownVal(cty.Map(workerItemType))` where `workerItemType` has `work_branch: String`, `commit_sha: String`, `artifact: Object(artName -> artifactValueType)` (or `EmptyObject`), plus `result: String` iff `content` configures a terminating tool
  - `work_branches`: `cty.UnknownVal(cty.List(cty.String))`

## `merge/spec.Config` (`internal/merge/spec/schema.go`)

| Field | Type / Default | Rules |
|---|---|---|
| `Name` | `string` | Non-empty safe label of `merge "<name>"` |
| `BasedSource` | `cty.Value` (required) | Non-null `cty.String` (known valid branch name or unknown upstream string) |
| `BasedSourceExpr` | `string` | Raw HCL expression string of `based_source` |
| `SourceURL` | `string` (derived) | Resolved in `ExecuteDuringPlan()` from `based_source` and `sources` (following `local.*`, `source.*`, `worker.*`, `dynamic_worker.*`, `merge.*`, or single root `source`) |
| `TargetBranch` | `*string` (optional) | `nil` when omitted/`null`; otherwise wholly known valid branch name not targeting an unpreserved `source.branch` for the repository |
| `Sources` | `cty.Value` (required) | Non-null `list(string)`, `set(string)`, or string `tuple`; rejects empty static collections; known elements must pass `ValidateBranchName` |
| `SourcesExpr` | `string` | Raw HCL expression string of `sources` |
| `Strategy` | `string` (default `"rebase"`) | Known string `"rebase"` |
| `MaxReworks` | `int` (default `2`) | Known whole integer `>= 0` |
| `HumanApproval` | `string` (default `"optional"`) | Known string in `{"optional", "required", "disabled"}` |
| `Sandbox` | `cty.Value` | Wholly known `kind == "sandbox"` object with non-empty `address` when set; required iff `len(Gatekeepers) > 0`; `NullVal(EmptyObject)` when omitted/`null` without gatekeepers |
| `Gatekeepers` | `[]workerspec.GatekeeperConfig` | 0..N unique-named gatekeepers (including `dynamic "gatekeeper"` expansion) |

### `MergeBlock.Value()`

- Pre-plan: `cty.DynamicVal`
- Post-plan:
  - `address`: `cty.StringVal(b.Address())`
  - `kind`: `cty.StringVal("merge")`
  - `name`: `cty.StringVal(b.Name())`
  - `work_branch`: `cty.UnknownVal(cty.String)`
  - `commit_sha`: `cty.UnknownVal(cty.String)`
