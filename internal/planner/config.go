package planner

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/lonegunmanb/go-defaults"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	mcpspec "github.com/lonegunmanb/sofa/internal/mcp/spec"
	modulespec "github.com/lonegunmanb/sofa/internal/module/spec"
	providerpkg "github.com/lonegunmanb/sofa/internal/provider"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	sourcespec "github.com/lonegunmanb/sofa/internal/source"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/zclconf/go-cty/cty"
)

type ConfigArgs struct {
	Context                  context.Context
	Directory                string
	VarConfigDir             *string
	IgnoreUnknownVariables   bool
	CliFlagAssignedVariables []golden.CliFlagAssignedVariables
}

type SofaConfig struct {
	*golden.BaseConfig
	directory          string
	sensitiveVariables map[string]struct{}
}

var registerBlocksOnce sync.Once

func init() {
	RegisterBlocks()
}

// RegisterBlocks installs every sofa block type before HCL blocks are adapted.
func RegisterBlocks() {
	registerBlocksOnce.Do(func() {
		defaults.SetDefaults(&struct{}{})
		golden.RegisterBlock(&sourcespec.SourceBlock{})
		golden.RegisterBlock(&sourcespec.PreserveBlock{})
		golden.RegisterBlock(&providerpkg.ModelProviderBlock{})
		golden.RegisterBlock(&mcpspec.ServerBlock{})
		golden.RegisterBlock(&sandboxspec.SandboxBlock{})
		golden.RegisterBlock(&toolspec.GoToolBlock{})
		golden.RegisterBlock(&toolspec.ExternalToolBlock{})
	})
}

func NewSofaConfig(args ConfigArgs) *SofaConfig {
	if args.Context == nil {
		args.Context = context.Background()
	}
	if absolute, err := filepath.Abs(args.Directory); err == nil {
		args.Directory = filepath.Clean(absolute)
	}

	RegisterBlocks()
	base := config.NewBaseConfig(golden.NewBaseConfigArgs{
		Basedir:                  args.Directory,
		VarConfigDir:             args.VarConfigDir,
		Ctx:                      args.Context,
		DslAbbreviation:          "sofa",
		DslFullName:              "sofa",
		IgnoreUnknownVariables:   args.IgnoreUnknownVariables,
		CliFlagAssignedVariables: args.CliFlagAssignedVariables,
	})
	return &SofaConfig{
		BaseConfig:         base,
		directory:          args.Directory,
		sensitiveVariables: make(map[string]struct{}),
	}
}

// Init validates sofa-specific declaration prerequisites and delegates graph
// construction and pre-plan work to golden.
func (c *SofaConfig) Init(blocks []*golden.HclBlock) error {
	preparedBlocks, err := applyRootDefaults(blocks)
	if err != nil {
		return err
	}
	if err := c.scanVariableDeclarations(preparedBlocks); err != nil {
		return err
	}
	return golden.InitConfig(c, preparedBlocks)
}

func (c *SofaConfig) scanVariableDeclarations(blocks []*golden.HclBlock) error {
	c.sensitiveVariables = make(map[string]struct{})
	for _, block := range blocks {
		if block == nil || block.Type != "variable" {
			continue
		}
		if len(block.Labels) != 2 {
			continue
		}
		name := block.Labels[1]

		typeAttribute, ok := block.Body.Attributes["type"]
		if !ok {
			return fmt.Errorf("variable %q MUST declare a concrete type", name)
		}
		variableType, _, diags := typeexpr.TypeConstraintWithDefaults(typeAttribute.Expr)
		if diags.HasErrors() {
			return fmt.Errorf("variable %q has an invalid type: %s", name, diags.Error())
		}
		if err := specpkg.ValidateType(variableType); err != nil {
			return fmt.Errorf("variable %q: %w", name, err)
		}

		sensitiveAttribute, ok := block.Body.Attributes["sensitive"]
		if !ok {
			continue
		}
		sensitive, diags := sensitiveAttribute.Expr.Value(nil)
		if diags.HasErrors() {
			return fmt.Errorf("variable %q sensitive flag: %s", name, diags.Error())
		}
		if sensitive.IsNull() || !sensitive.IsKnown() || !sensitive.Type().Equals(cty.Bool) {
			return fmt.Errorf("variable %q sensitive flag MUST be a known boolean", name)
		}
		if sensitive.True() {
			c.sensitiveVariables[name] = struct{}{}
		}
	}
	return nil
}

// EvalContext adds sofa's path object and sensitivity marks to golden's native
// evaluation namespace.
func (c *SofaConfig) EvalContext() *hcl.EvalContext {
	ctx := c.BaseConfig.EvalContext()
	if ctx == nil {
		return nil
	}
	if ctx.Variables == nil {
		ctx.Variables = make(map[string]cty.Value)
	}

	if variableValue, ok := ctx.Variables["var"]; ok &&
		!variableValue.IsNull() &&
		variableValue.Type().IsObjectType() {
		values := make(map[string]cty.Value, len(variableValue.AsValueMap()))
		for name, value := range variableValue.AsValueMap() {
			if _, sensitive := c.sensitiveVariables[name]; sensitive {
				value = specpkg.MarkSensitive(value)
			}
			values[name] = value
		}
		ctx.Variables["var"] = cty.ObjectVal(values)
	}

	ctx.Variables["path"] = cty.ObjectVal(map[string]cty.Value{
		"module": cty.StringVal(filepath.ToSlash(c.directory)),
	})
	return ctx
}

// Outputs returns output snapshots ordered by output name.
func (c *SofaConfig) Outputs() []modulespec.Output {
	blocks := golden.Blocks[*golden.OutputBlock](c)
	outputs := make([]modulespec.Output, 0, len(blocks))
	for _, block := range blocks {
		var description *string
		if nativeDescription := block.Description(); nativeDescription != nil {
			value := *nativeDescription
			description = &value
		}
		outputs = append(outputs, modulespec.Output{
			Name:        block.Name(),
			Value:       block.Value(),
			Description: description,
			Sensitive:   block.Sensitive(),
			Expression:  block.ValueExprString(),
		})
	}
	sort.Slice(outputs, func(i, j int) bool {
		return outputs[i].Name < outputs[j].Name
	})
	return outputs
}

var _ golden.Config = (*SofaConfig)(nil)
