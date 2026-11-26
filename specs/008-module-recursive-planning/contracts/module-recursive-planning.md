# Contract: Recursive `module` Planning, Unknown Input Relay, and Orthogonal Singleton Inheritance

## 1. `ModuleBlock` HCL Schema (`module "<name>"`)

- **Labels**: Single label `<name>`; must be non-empty, single-segment (rejects `.`, `..`, `/`, `\`, control characters).
- **Fixed Meta-Attributes** (must be wholly known during Plan):
  - `source` (required): literal non-whitespace `string` with zero variable references (`len(attr.Expr.Variables()) == 0`).
  - `parallelism` (optional): known positive whole integer (`> 0`), or `null` (treated as unset).
  - `timeout` (optional): known positive Go duration string (`time.ParseDuration` with `d > 0`), or `null` (treated as unset).
- **Golden Meta-Attributes & Nested Blocks**:
  - `for_each` and `depends_on` supported via `golden.MetaAttributeNames`.
  - `precondition` and `dynamic "precondition"` supported via `golden.MetaNestedBlockNames`; all other nested blocks or `dynamic "<other>"` rejected.
- **Business Inputs**:
  - Any other attribute name maps to a child module `variable "<name>"`.
  - Accepts known values, `cty.UnknownVal` (typed or dynamic unknown if convertible to declared variable type), composite values containing unknowns, and `golden.SensitiveMark`; rejects capsule types.

## 2. Child `variable` Binding & Validation Contract

1. Every input passed by the caller must correspond to a declared `variable` in the child module; undeclared inputs fail.
2. Every declared `variable` in the child module must declare a concrete `type` (`specpkg.ValidateType`).
3. If caller omits an input, the variable's `default` is used; if no `default` exists, planning fails with missing required input variable.
4. `nullable` defaults to `true`. If caller passes `null` and `nullable = false`, planning fails; if `nullable = true` and `default` exists, `default` is substituted; otherwise `cty.NullVal(varType)` is bound.
5. `typeexpr.Defaults.Apply` (when outer container is known and non-null) and `convert.Convert(val, varType)` validate and normalize both known and unknown values while preserving sensitivity marks via `sensitiveVariables`.
6. When `!converted.IsWhollyKnown()` and `validation` blocks exist, each `validation` block is statically checked (`error_message` known non-empty string, `condition` evaluates without errors to `cty.Bool`, known `false` fails immediately, unknown `bool` appends `corespec.Condition` to `Plan.DeferredValidations`).
7. Host `SOFA_VAR_*` env vars and child-directory `.sofavars` / `*.auto.sofavars` files never affect child module variable bindings.

## 3. Orthogonal Singleton Inheritance (`sandbox`, `harness`, `model_provider`)

For each dimension `D` independently:
1. If child declares `>= 1` blocks of `D`: resolve locally (`1` = local default, `>= 2` = local ambiguity error) and export child's `D` status to grandchildren.
2. If child declares `0` blocks of `D`: pass ancestor's `D` status through to grandchildren; consumers omitting `D` bind to `SingletonUnique` (with ancestor's canonical `"address"`), allow `model_provider = null`, or fail with canonical consumer address errors on `SingletonNone` (`sandbox`/`harness`) or `SingletonAmbiguous`.

## 4. `CanonicalAddress`, `BundlePath`, and `SourceURL` Lineage

- `CanonicalAddress(addr)` returns `addr` at root and `addressPrefix + "." + addr` in child modules.
- `path.module` evaluates to `filepath.ToSlash(BundlePath)` when `BundlePath != ""` (`"config"` at root, `"modules/<labels...>"` in children, ignoring `[key]` `for_each` suffixes), or `filepath.ToSlash(Directory)` otherwise.
- `SourceURL` lineage propagates from caller expressions -> child `var.<name>` -> child `worker`/`dynamic_worker`/`merge` -> child `output.<name>` -> parent `module.<name>[*].<output>`.
