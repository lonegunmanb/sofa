# Data Model: Recursive `module` Planning, Unknown Input Relay, and Orthogonal Singleton Inheritance

## `internal/module/spec`

### `Output` (`output.go`)

| Field | Type | Description |
|---|---|---|
| `Name` | `string` | Output block name |
| `Value` | `cty.Value` | Planned output value (preserving `cty.UnknownVal` and `golden.SensitiveMark`) |
| `Description` | `*string` | Optional output description |
| `Sensitive` | `bool` | Whether output or its value carries sensitivity |
| `Expression` | `string` | Raw HCL `value` expression string |
| `SourceURLs` | `[]string` | Distinct upstream repository URLs referenced by the output expression |

### `Plan` (`plan.go`)

| Field | Type | Description |
|---|---|---|
| `Directory` | `string` | Normalized module directory path |
| `BundlePath` | `string` | Bundle-relative slash path (`"config"` or `"modules/<labels...>"`), empty if unset |
| `Outputs` | `map[string]Output` | Planned outputs keyed by output name |
| `Modules` | `map[string]ModulePlan` | Planned child modules keyed by local module block address |
| `DeferredValidations` | `map[string][]corespec.Condition` | Variable `validation` conditions deferred because input contained unknowns |
| `ChildConfig` | `any` | Reference to planned child `*planner.SofaConfig` for downstream `.sofaplan` serialization |

### `ModulePlan` (`plan.go`)

Embeds `Plan`, plus:

| Field | Type | Description |
|---|---|---|
| `Address` | `string` | Parent-local block address (e.g. `module.audit` or `module.audit[east]`) |
| `Name` | `string` | Module block label (`audit`) |
| `CanonicalAddress` | `string` | Globally unique canonical address (`module.audit` or `module.outer.module.audit[east]`) |
| `Source` | `string` | Literal `source` string declared on the `module` block |
| `Parallelism` | `*int` | Optional positive parallelism limit (`nil` if unset/null) |
| `Timeout` | `*time.Duration` | Optional positive timeout duration (`nil` if unset/null) |
| `TimeoutRaw` | `string` | Raw trimmed duration string when `Timeout != nil` |
| `Inputs` | `map[string]cty.Value` | Evaluated input values passed from caller (with sensitivity marks preserved) |
| `InputExprs` | `map[string]string` | Raw HCL expression strings for each input attribute |

### `ModuleBlock` & `ModulePlanner` (`module.go`)

- `ModuleBlock` embeds `*golden.BaseBlock` and implements `golden.CustomDecode`, `golden.PlanBlock`, `golden.SingleValueBlock`.
- `ModulePlanRequest` passes `Address`, `Name`, `Source`, `Parallelism`, `Timeout`, `TimeoutRaw`, `Inputs`, `InputExprs`, and `InputAttrs` (`map[string]*golden.HclAttribute`) to `ModulePlanner.PlanChildModule`.

## `internal/planner`

### `InheritedSingleton` & `InheritedSingletons` (`defaults.go`)

| Field | Type | Description |
|---|---|---|
| `Status` | `SingletonStatus` | `SingletonNone`, `SingletonUnique`, or `SingletonAmbiguous` |
| `Name` | `string` | Singleton block name when `SingletonUnique` |
| `Value` | `cty.Value` | Planned `cty.Object` value with canonical `"address"` when `SingletonUnique` |
| `Names` | `[]string` | Declared names when `SingletonAmbiguous` |
| `Origin` | `string` | Human-readable origin (`"root module"` or `"module.outer"`) |

### `SofaConfig` Additions (`config.go`, `module.go`)

| Field | Type | Description |
|---|---|---|
| `modulesDirectory` | `string` | Optional `.sofa/modules` directory for initialized modules |
| `bundlePath` | `string` | Optional bundle-relative `path.module` (`"config"` / `"modules/..."`) |
| `addressPrefix` | `string` | Canonical module prefix (empty at root) |
| `moduleStack` | `[]string` | Normalized directory stack for cycle detection |
| `incomingInherited` | `InheritedSingletons` | Ancestor singletons inherited per dimension |
| `injectedSingletons` | `InheritedSingletons` | Dimensions actually bound from `incomingInherited` (0 local blocks, unique ancestor) |
| `variableSourceURLs` | `map[string][]string` | Upstream `SourceURL`s bound per child `variable` |
| `forEachModuleNames` | `map[string]struct{}` | Local `module` block names that declared `for_each` |
