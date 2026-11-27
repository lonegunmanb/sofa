# Data Model: `internal/applyeval` (`027-apply-deferred-evaluator`)

## 1. Runtime State Inputs (`context.go`)

- **`SourceRuntimeState`**: `Address`, `Name`, `URL`, `Branch`, `CommitSHA`
- **`ArtifactRuntimeState`**: `ID`, `Name`, `Type`, `RelPath`, `ContainerPath`, `Description`, `Required`, `NonEmpty`
- **`WorkerRuntimeResult`**: `Address`, `Name`, `WorkBranch`, `CommitSHA`, `Result *string`, `Artifacts map[string]ArtifactRuntimeState`
- **`DynamicWorkerRuntimeResult`**: `Address`, `Name`, `Keys []string`, `Instances map[string]WorkerRuntimeResult`
- **`MergeRuntimeResult`**: `Address`, `Name`, `WorkBranch`, `CommitSHA`
- **`ModuleRuntimeResult`**: `Address`, `Outputs map[string]cty.Value`
- **`RuntimeState`**:
  - `Sources map[string]SourceRuntimeState`
  - `Workers map[string]WorkerRuntimeResult`
  - `DynamicWorkers map[string]DynamicWorkerRuntimeResult`
  - `Merges map[string]MergeRuntimeResult`
  - `Modules map[string]ModuleRuntimeResult`
- **`EvalOption`**:
  - `WithBaseDir(dir string)` — sets `basedir` for `config.Functions(baseDir)` (defaults to `planned.Directory()`)
  - `WithParentPlans(parents ...*plan.Plan)` — supplies ancestor plans (outermost-to-immediate) for inherited singleton `sandbox`, `harness`, and `model_provider` propagation.

## 2. Public `cty.Value` Schemas

- `source.<name>`: `{ address, kind = "source", name, url, branch, commit_sha }`
- `worker.<name>`: `{ address, kind = "worker", name, work_branch, commit_sha, artifact = object({ ... }), [result = string] }` (`for_each` instances grouped under `worker.<name>[<key>]`)
- `dynamic_worker.<name>`: `{ address, kind = "dynamic_worker", name, workers = map(workerItemType), work_branches = list(string) }` (sorted by `Keys`)
- `merge.<name>`: `{ address, kind = "merge", name, work_branch, commit_sha }` (`for_each` instances grouped under `merge.<name>[<key>]`)
- `module.<name>`: `cty.ObjectVal(outputs)` (`for_each` instances grouped under `module.<name>[<key>]`)

## 3. Materialized Node & Module Outputs (`eval.go`)

- **`MaterializedMerge`**:
  - `Name string`
  - `BasedSource string`
  - `SourceURL string`
  - `TargetBranch *string`
  - `Sources []string`
  - `SourcesKnown bool`
  - `Strategy string`
  - `MaxReworks int`
  - `HumanApproval string`
  - `Sandbox cty.Value`
  - `Gatekeepers []workerspec.GatekeeperConfig`
- **`BoundModule`**:
  - `Plan *plan.Plan` (updated immutable child plan with `Context["var"]` bound to materialized inputs)
  - `Inputs map[string]cty.Value` (materialized input map preserving `SensitiveMark`)
  - `VarObject cty.Value` (full child `var` `cty.ObjectVal` including defaults & `sensitive` marks)
