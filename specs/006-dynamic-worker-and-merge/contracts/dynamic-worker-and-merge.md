# Contracts: Dynamic Workers and Deterministic Merge Blocks

## `internal/worker/spec` Additions

```go
type DynamicConfig struct {
	Name            string
	Parallelism     *int
	Serial          bool
	ForEach         cty.Value
	ForEachExpr     string
	IteratorName    string
	Content         Config
	ContentTemplate string
}

type DynamicWorkerBlock struct {
	*golden.BaseBlock
	// unexported planned DynamicConfig, virtualWorker, plannedValue
}

func (d *DynamicWorkerBlock) Type() string            // ""
func (d *DynamicWorkerBlock) BlockType() string       // "dynamic_worker"
func (d *DynamicWorkerBlock) AddressLength() int      // 2
func (d *DynamicWorkerBlock) CanExecutePrePlan() bool // false
func (d *DynamicWorkerBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error
func (d *DynamicWorkerBlock) ExecuteDuringPlan() error
func (d *DynamicWorkerBlock) Value() cty.Value
func (d *DynamicWorkerBlock) DynamicWorkerConfig() DynamicConfig // deep copy via Clone()
```

Exported shared helpers in `internal/worker/spec` for `internal/merge/spec`:
- `DecodeGatekeeper(block *golden.HclBlock, ctx *hcl.EvalContext) (GatekeeperConfig, error)`
- `ValidateBlockReference(value cty.Value, expectedKind, scope string) error`
- `ValidateSafeLabel(label, scope string, forbidPathTokens bool) error`

## `internal/merge/spec` Public API

```go
type SourceBranchInfo struct {
	URL    string
	Branch string
}

type PreservedBranchInfo struct {
	URL    string
	Branch string
}

type ModuleRegistry interface {
	LookupSourceURL(name string) (string, bool)
	SingleRootSourceURL() (string, int)
	LookupUpstreamSourceURLs(address string) ([]string, bool)
	ResolveLocalExpr(name string) (hcl.Expression, bool)
	SourceBranches() []SourceBranchInfo
	PreservedKnownBranches() []PreservedBranchInfo
}

type Config struct {
	Name            string
	BasedSource     cty.Value
	BasedSourceExpr string
	SourceURL       string
	TargetBranch    *string
	Sources         cty.Value
	SourcesExpr     string
	Strategy        string
	MaxReworks      int
	HumanApproval   string
	Sandbox         cty.Value
	Gatekeepers     []workerspec.GatekeeperConfig
}

type MergeBlock struct {
	*golden.BaseBlock
	// unexported planned Config, plannedValue
}

func (m *MergeBlock) Type() string            // ""
func (m *MergeBlock) BlockType() string       // "merge"
func (m *MergeBlock) AddressLength() int      // 2
func (m *MergeBlock) CanExecutePrePlan() bool // false
func (m *MergeBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error
func (m *MergeBlock) ExecuteDuringPlan() error
func (m *MergeBlock) Value() cty.Value
func (m *MergeBlock) MergeConfig() Config // deep copy via Clone()
```

## `internal/planner` Additions

- `RegisterBlocks()` registers `&workerspec.DynamicWorkerBlock{}` and `&mergespec.MergeBlock{}`.
- `SofaConfig` implements `mergespec.ModuleRegistry` and resolves `dynamic_worker.<name>` and `merge.<name>` URLs in `LookupWorkerSourceURLs` / `LookupUpstreamSourceURLs`.
- `applyRootDefaults` injects missing `sandbox`, `harness`, and `model_provider` into `dynamic_worker`'s `dynamic "worker" -> content` block (consumer address `"dynamic_worker." + name`) and injects missing `sandbox` into `merge` blocks that declare at least one `gatekeeper` or `dynamic "gatekeeper"` block (consumer address `"merge." + name`).
- `worker_fallback.go` excludes `fallback` blocks inside `dynamic_worker` from positive ancestry and validates `dynamic_worker` fallback targets at `Init` and `ExecuteDuringPlan()` time.
