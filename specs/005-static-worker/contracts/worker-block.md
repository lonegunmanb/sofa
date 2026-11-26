# Worker Block Contracts

## `internal/worker/spec` Public API

```go
type ModuleToolInfo struct {
	ID         string
	InputType  cty.Type
	OutputType cty.Type
}

type ModuleRegistry interface {
	LookupRegisteredTool(id string) (ModuleToolInfo, bool)
	HasRegisteredMCPTool(id string) bool
	HasRegisteredMCPResource(id string) bool
	LookupSourceURL(name string) (string, bool)
	SingleRootSourceURL() (string, int)
	LookupWorkerSourceURLs(address string) ([]string, bool)
	ResolveLocalExpr(name string) (hcl.Expression, bool)
	ValidateWorkerFallbackTargets(workerAddress string, targets []string) error
}

type WorkerBlock struct {
	*golden.BaseBlock
	// unexported planned Config, artifacts, importPaths
}

func (w *WorkerBlock) Type() string            // ""
func (w *WorkerBlock) BlockType() string       // "worker"
func (w *WorkerBlock) AddressLength() int      // 2
func (w *WorkerBlock) CanExecutePrePlan() bool // false
func (w *WorkerBlock) EvalContext() *hcl.EvalContext
func (w *WorkerBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error
func (w *WorkerBlock) ExecuteDuringPlan() error
func (w *WorkerBlock) Values() map[string]cty.Value
func (w *WorkerBlock) WorkerConfig() Config // deep-copied snapshot via Config.Clone()
```

## `WorkerBlock.Values()` Schema

- `work_branch`: `cty.UnknownVal(cty.String)`
- `commit_sha`: `cty.UnknownVal(cty.String)`
- `artifact`: `cty.ObjectVal` mapping each `<name>` to `{ id: UnknownVal(String), name, kind: "artifact", type, path, description, required, non_empty }`
- `result`: `cty.UnknownVal(cty.String)` — **present only when `terminate_tool_id != ""` or any `tool_use` has `terminate == true`**

## Worker Local EvalContext

- `artifact("<name>")`: returns the declared artifact object in the current worker; fails planning with a diagnostic if `<name>` is null, blank, or undeclared in this worker.
- `import_artifact`: `cty.ObjectVal` mapping each declared `import_artifact "<name>"` to `{ path: StringVal(effectivePath), desc: StringVal(desc) }`.

## `internal/planner` Contracts

- `SofaConfig` implements `workerspec.ModuleRegistry`.
- `SofaConfig.Init` runs `applyRootDefaults` (orthogonal `sandbox`, `harness`, `model_provider` AST injection when omitted; never overwriting explicit attributes including `model_provider = null`) and `validateWorkerFallbacks` (syntax + positive ancestry across `locals` and `depends_on`).
- `SofaConfig.EvalContext()` unwraps `worker[""]` produced by `golden.Valuable`.
