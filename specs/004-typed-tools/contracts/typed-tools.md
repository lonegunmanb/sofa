# Typed Tools Contracts

## `internal/spec`

```go
type Condition struct {
    Expression   string `json:"expression"`
    ErrorMessage string `json:"error_message"`
}
func (c Condition) Evaluate(variables map[string]cty.Value, functions map[string]function.Function) (bool, error)

type Issue struct {
    ID         string  `json:"id,omitempty"`
    Code       string  `json:"code"`
    Message    string  `json:"message"`
    Path       *string `json:"path,omitempty"`
    RepairHint *string `json:"repair_hint,omitempty"`
}
func (i Issue) Validate() error

type ToolResponse[T any] struct {
    Accepted bool    `json:"accepted"`
    Output   *T      `json:"output,omitempty"`
    Issues   []Issue `json:"issues,omitempty"`
}
func (r ToolResponse[T]) Validate() error
```

## `internal/tool/gotool`

```go
type Analysis struct {
    InputType  cty.Type
    OutputType cty.Type
    QuotePaths [][]string
}
func Analyze(source string) (Analysis, error)
func AnalyzeWithDependencies(source string, dependencies []string) (Analysis, error)
```

## `internal/tool/spec`

```go
type Constraint struct { /* typeValue cty.Type, defaults *typeexpr.Defaults */ }
func NewConstraint(typeValue cty.Type) Constraint
func NewConstraintWithDefaults(typeValue cty.Type, defaults *typeexpr.Defaults) Constraint
func (c Constraint) Type() cty.Type
func (c Constraint) Apply(value cty.Value) (cty.Value, error)
func (c Constraint) JSONSchema() (map[string]any, error)
func JSONSchema(typeValue cty.Type) (map[string]any, error)

// GoToolBlock & ExternalToolBlock implement golden.PlanBlock, golden.CustomDecode, golden.SingleValueBlock
// and override Id() string, BaseValues() map[string]cty.Value, CanonicalAddress() string.
// Both expose InputConstraint() Constraint and OutputConstraint() Constraint.
```

## `internal/config` HCL Function

- `tool_name(tool_ref)`: Accepts a wholly-known `cty.Object` with valid `kind` (`"go"`, `"external"`, `"builtin"`) and `address`, returning its `id` (or normalized address fallback) with outer sensitivity marks preserved.
