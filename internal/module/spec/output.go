package spec

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
)

// Output is the stable, inspectable representation of an output declaration.
type Output struct {
	Name        string
	Value       cty.Value
	Description *string
	Sensitive   bool
}

// OutputBlock implements the sofa output "<name>" block.
type OutputBlock struct {
	*golden.BaseBlock
	Expression  cty.Value `hcl:"value"`
	Description *string   `hcl:"description,optional"`
	Sensitive   bool      `hcl:"sensitive,optional"`
	planned     cty.Value
}

func (o *OutputBlock) Type() string            { return "" }
func (o *OutputBlock) BlockType() string       { return "output" }
func (o *OutputBlock) AddressLength() int      { return 2 }
func (o *OutputBlock) CanExecutePrePlan() bool { return false }

// BaseDecode validates optional attributes without allowing HCL's safe
// conversions to silently turn numbers into strings or strings into booleans.
func (o *OutputBlock) BaseDecode(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	if attribute, ok := block.Body.Attributes["description"]; ok {
		value, diags := attribute.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		if !value.IsKnown() || value.IsNull() || !value.Type().Equals(cty.String) {
			return fmt.Errorf("output %q description MUST be a known non-null string", o.Name())
		}
	}
	if attribute, ok := block.Body.Attributes["sensitive"]; ok {
		value, diags := attribute.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		if !value.IsKnown() || value.IsNull() || !value.Type().Equals(cty.Bool) {
			return fmt.Errorf("output %q sensitive MUST be a known non-null boolean", o.Name())
		}
	}
	return nil
}

func (o *OutputBlock) ExecuteDuringPlan() error {
	if o.Expression == cty.NilVal {
		return fmt.Errorf("output %q MUST declare a value", o.Name())
	}
	if _, exists := o.HclBlock().Attributes()["description"]; exists {
		if o.Description == nil {
			return fmt.Errorf("output %q description MUST be a known non-null string", o.Name())
		}
	}
	if o.Description != nil && *o.Description == "" {
		return fmt.Errorf("output %q description MUST not be empty", o.Name())
	}

	o.planned = o.Expression
	if o.Sensitive {
		o.planned = specpkg.MarkSensitive(o.planned)
	}
	return nil
}

func (o *OutputBlock) Value() cty.Value {
	if o.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return o.planned
}

func (o *OutputBlock) Output() Output {
	value := o.Value()
	return Output{
		Name:        o.Name(),
		Value:       value,
		Description: o.Description,
		Sensitive:   o.Sensitive || specpkg.IsSensitive(value),
	}
}

var _ golden.PlanBlock = (*OutputBlock)(nil)
var _ golden.SingleValueBlock = (*OutputBlock)(nil)
var _ golden.BaseDecode = (*OutputBlock)(nil)
