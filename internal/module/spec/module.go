package spec

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/lonegunmanb/golden"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

// ModulePlanRequest carries decoded module call metadata and inputs to the planner.
type ModulePlanRequest struct {
	Address     string
	Name        string
	Source      string
	Parallelism *int
	Timeout     *time.Duration
	TimeoutRaw  string
	Inputs      map[string]cty.Value
	InputExprs  map[string]string
	InputAttrs  map[string]*golden.HclAttribute
}

// ModulePlanner plans a child module directory for a ModuleBlock.
type ModulePlanner interface {
	PlanChildModule(req ModulePlanRequest) (ModulePlan, error)
}

// ModuleBlock implements the top-level `module "<name>"` HCL block.
type ModuleBlock struct {
	*golden.BaseBlock
	Source      string
	Parallelism *int
	Timeout     *time.Duration
	TimeoutRaw  string
	Inputs      map[string]cty.Value
	InputExprs  map[string]string

	inputAttrs           map[string]*golden.HclAttribute
	hasDynamicPrecond    bool
	dynamicPreconditions []*golden.HclBlock

	mu           sync.RWMutex
	plannedReady bool
	planned      ModulePlan
}

func (*ModuleBlock) Type() string            { return "" }
func (*ModuleBlock) BlockType() string       { return "module" }
func (*ModuleBlock) AddressLength() int      { return 2 }
func (*ModuleBlock) CanExecutePrePlan() bool { return false }

// Decode validates module schema, static metadata, preconditions, and business inputs.
func (b *ModuleBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error {
	b.Source = ""
	b.Parallelism = nil
	b.Timeout = nil
	b.TimeoutRaw = ""
	b.Inputs = nil
	b.InputExprs = nil
	b.inputAttrs = nil
	b.hasDynamicPrecond = false
	b.dynamicPreconditions = nil

	b.mu.Lock()
	b.plannedReady = false
	b.planned = ModulePlan{}
	b.mu.Unlock()

	if len(block.Labels) != 2 {
		return fmt.Errorf("module block must have exactly one name")
	}
	if err := validateModuleLabel(b.Name()); err != nil {
		return err
	}
	hasDynamic, err := validateModuleSchema(block, b.Name())
	if err != nil {
		return err
	}
	if hasDynamic {
		expanded, err := block.ExpandDynamicBlocks(baseCtx)
		if err != nil {
			return err
		}
		var preconds []*golden.HclBlock
		for _, nested := range expanded.NestedBlocks() {
			if nested.Type != "precondition" {
				return fmt.Errorf("module %q only supports dynamic \"precondition\" blocks", b.Name())
			}
			preconds = append(preconds, nested)
		}
		b.hasDynamicPrecond = true
		b.dynamicPreconditions = preconds
	}

	source, err := decodeLiteralSource(block, b.Name())
	if err != nil {
		return err
	}
	parallelism, err := decodeModuleParallelism(block, baseCtx, b.Name())
	if err != nil {
		return err
	}
	timeout, timeoutRaw, err := decodeModuleTimeout(block, baseCtx, b.Name())
	if err != nil {
		return err
	}

	inputs := make(map[string]cty.Value)
	inputExprs := make(map[string]string)
	inputAttrs := make(map[string]*golden.HclAttribute)
	for name, attr := range block.Attributes() {
		if isModuleFixedAttribute(name) || golden.MetaAttributeNames.Contains(name) {
			continue
		}
		val, diags := attr.Expr.Value(baseCtx)
		if diags.HasErrors() {
			return diags
		}
		if hasCapsuleType(val.Type()) {
			return fmt.Errorf("module %q input %q must not use a capsule type", b.Name(), name)
		}
		inputs[name] = val
		inputExprs[name] = attr.ExprString()
		inputAttrs[name] = attr
	}

	b.Source = source
	b.Parallelism = parallelism
	b.Timeout = timeout
	b.TimeoutRaw = timeoutRaw
	b.Inputs = inputs
	b.InputExprs = inputExprs
	b.inputAttrs = inputAttrs
	return nil
}

// PreConditionCheck evaluates static preconditions or expanded dynamic preconditions.
func (b *ModuleBlock) PreConditionCheck(ctx *hcl.EvalContext) ([]golden.PreCondition, error) {
	if !b.hasDynamicPrecond {
		return b.BaseBlock.PreConditionCheck(ctx)
	}
	var failedChecks []golden.PreCondition
	var err error
	for _, pre := range b.dynamicPreconditions {
		cond := golden.PreCondition{Body: pre.Body}
		diag := gohcl.DecodeBody(pre.Body, ctx, &cond)
		if diag.HasErrors() {
			err = multierror.Append(err, diag.Errs()...)
			continue
		}
		if !cond.Condition {
			failedChecks = append(failedChecks, cond)
		}
	}
	return failedChecks, err
}

// ExecuteDuringPlan invokes ModulePlanner to recursively plan the child module.
func (b *ModuleBlock) ExecuteDuringPlan() error {
	if b.BaseBlock == nil || b.Config() == nil {
		return fmt.Errorf("module %q requires a ModulePlanner config", b.Name())
	}
	planner, ok := b.Config().(ModulePlanner)
	if !ok {
		return fmt.Errorf("module %q requires a ModulePlanner config", b.Name())
	}
	var parallelism *int
	if b.Parallelism != nil {
		p := *b.Parallelism
		parallelism = &p
	}
	var timeout *time.Duration
	if b.Timeout != nil {
		d := *b.Timeout
		timeout = &d
	}
	planned, err := planner.PlanChildModule(ModulePlanRequest{
		Address:     b.Address(),
		Name:        b.Name(),
		Source:      b.Source,
		Parallelism: parallelism,
		Timeout:     timeout,
		TimeoutRaw:  b.TimeoutRaw,
		Inputs:      maps.Clone(b.Inputs),
		InputExprs:  maps.Clone(b.InputExprs),
		InputAttrs:  maps.Clone(b.inputAttrs),
	})
	if err != nil {
		return fmt.Errorf("planning module %s: %w", b.Address(), err)
	}
	b.mu.Lock()
	b.planned = planned
	b.plannedReady = true
	b.mu.Unlock()
	return nil
}

// Value exposes the child module's planned outputs as a cty.ObjectVal, or
// cty.DynamicVal prior to ExecuteDuringPlan().
func (b *ModuleBlock) Value() cty.Value {
	b.mu.RLock()
	ready := b.plannedReady
	outputs := b.planned.Outputs
	b.mu.RUnlock()
	if !ready {
		return cty.DynamicVal
	}
	if len(outputs) == 0 {
		return cty.EmptyObjectVal
	}
	values := make(map[string]cty.Value, len(outputs))
	for name, output := range outputs {
		values[name] = output.Value
	}
	return cty.ObjectVal(values)
}

// PlannedModule returns a deep-copied snapshot of the planned ModulePlan.
func (b *ModuleBlock) PlannedModule() ModulePlan {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.planned.Clone()
}

func validateModuleLabel(label string) error {
	if err := workerspec.ValidateSafeLabel(label, "module", true); err != nil {
		return err
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return fmt.Errorf("module name %q must not contain control characters", label)
		}
	}
	return nil
}

func validateModuleSchema(block *golden.HclBlock, name string) (hasDynamic bool, err error) {
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "dynamic" {
			if len(nested.Labels) != 1 || nested.Labels[0] != "precondition" {
				return false, fmt.Errorf("module %q only supports dynamic \"precondition\" blocks", name)
			}
			hasDynamic = true
			continue
		}
		if golden.MetaNestedBlockNames.Contains(nested.Type) {
			continue
		}
		return false, fmt.Errorf("unsupported block type %q in module %q", nested.Type, name)
	}
	return hasDynamic, nil
}

func decodeLiteralSource(block *golden.HclBlock, name string) (string, error) {
	attr, ok := block.Attributes()["source"]
	if !ok {
		return "", fmt.Errorf("module %q source is required", name)
	}
	if len(attr.Expr.Variables()) != 0 {
		return "", fmt.Errorf("module %q source must be a literal string without variable references", name)
	}
	val, diags := attr.Expr.Value(&hcl.EvalContext{})
	if diags.HasErrors() {
		return "", fmt.Errorf("module %q source: %w", name, diags)
	}
	if val.HasMarkDeep(golden.SensitiveMark) {
		return "", fmt.Errorf("module %q source must not be sensitive", name)
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", fmt.Errorf("module %q source must be a literal string", name)
	}
	source := unmarked.AsString()
	if strings.TrimSpace(source) == "" {
		return "", fmt.Errorf("module %q source must not be empty", name)
	}
	return source, nil
}

func decodeModuleParallelism(block *golden.HclBlock, ctx *hcl.EvalContext, name string) (*int, error) {
	attr, ok := block.Attributes()["parallelism"]
	if !ok {
		return nil, nil
	}
	val, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.Number) {
		return nil, fmt.Errorf("module %q parallelism must be a positive integer", name)
	}
	bf := unmarked.AsBigFloat()
	if bf.IsInf() || !bf.IsInt() {
		return nil, fmt.Errorf("module %q parallelism must be a positive integer", name)
	}
	i64, acc := bf.Int64()
	if acc != big.Exact || i64 <= 0 || i64 > int64(math.MaxInt) {
		return nil, fmt.Errorf("module %q parallelism must be a positive integer", name)
	}
	p := int(i64)
	return &p, nil
}

func decodeModuleTimeout(block *golden.HclBlock, ctx *hcl.EvalContext, name string) (*time.Duration, string, error) {
	attr, ok := block.Attributes()["timeout"]
	if !ok {
		return nil, "", nil
	}
	val, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, "", diags
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, "", nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return nil, "", fmt.Errorf("module %q timeout must be a positive duration string", name)
	}
	raw := strings.TrimSpace(unmarked.AsString())
	if raw == "" {
		return nil, "", fmt.Errorf("module %q timeout must not be empty", name)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return nil, "", fmt.Errorf("module %q timeout must be a positive duration", name)
	}
	return &parsed, raw, nil
}

func hasCapsuleType(t cty.Type) bool {
	if t == cty.NilType {
		return false
	}
	if t.IsCapsuleType() {
		return true
	}
	switch {
	case t.IsListType(), t.IsSetType(), t.IsMapType():
		return hasCapsuleType(t.ElementType())
	case t.IsTupleType():
		for _, elemType := range t.TupleElementTypes() {
			if hasCapsuleType(elemType) {
				return true
			}
		}
	case t.IsObjectType():
		for _, attrType := range t.AttributeTypes() {
			if hasCapsuleType(attrType) {
				return true
			}
		}
	}
	return false
}

func isModuleFixedAttribute(name string) bool {
	return name == "source" || name == "parallelism" || name == "timeout"
}

var _ golden.CustomDecode = (*ModuleBlock)(nil)
var _ golden.PlanBlock = (*ModuleBlock)(nil)
var _ golden.SingleValueBlock = (*ModuleBlock)(nil)
