package planner

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/lonegunmanb/go-defaults"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcpspec "github.com/lonegunmanb/sofa/internal/mcp/spec"
	mergespec "github.com/lonegunmanb/sofa/internal/merge/spec"
	modulespec "github.com/lonegunmanb/sofa/internal/module/spec"
	providerspec "github.com/lonegunmanb/sofa/internal/provider"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	sourcespec "github.com/lonegunmanb/sofa/internal/source"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

type ConfigArgs struct {
	Context                  context.Context
	Directory                string
	ModulesDirectory         string
	BundlePath               string
	VarConfigDir             *string
	IgnoreUnknownVariables   bool
	CliFlagAssignedVariables []golden.CliFlagAssignedVariables
}

type SofaConfig struct {
	*golden.BaseConfig
	directory           string
	modulesDirectory    string
	bundlePath          string
	addressPrefix       string
	moduleStack         []string
	incomingInherited   InheritedSingletons
	injectedSingletons  InheritedSingletons
	sensitiveVariables  map[string]struct{}
	variableSourceURLs  map[string][]string
	forEachModuleNames  map[string]struct{}
	deferredValidations map[string][]specpkg.Condition
	parentConfig        *SofaConfig
	isChildModule       bool
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
		golden.RegisterBlock(&providerspec.ModelProviderBlock{})
		golden.RegisterBlock(&mcpspec.ServerBlock{})
		golden.RegisterBlock(&sandboxspec.SandboxBlock{})
		golden.RegisterBlock(&harnessspec.HarnessBlock{})
		golden.RegisterBlock(&toolspec.GoToolBlock{})
		golden.RegisterBlock(&toolspec.ExternalToolBlock{})
		golden.RegisterBlock(&workerspec.WorkerBlock{})
		golden.RegisterBlock(&workerspec.DynamicWorkerBlock{})
		golden.RegisterBlock(&mergespec.MergeBlock{})
		golden.RegisterBlock(&modulespec.ModuleBlock{})
	})
}

func NewSofaConfig(args ConfigArgs) *SofaConfig {
	if args.Context == nil {
		args.Context = context.Background()
	}
	if absolute, err := filepath.Abs(args.Directory); err == nil {
		args.Directory = filepath.Clean(absolute)
	}
	modulesDir := strings.TrimSpace(args.ModulesDirectory)
	if modulesDir != "" {
		if absolute, err := filepath.Abs(modulesDir); err == nil {
			modulesDir = filepath.Clean(absolute)
		}
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
	var stack []string
	if norm, err := normalizeModuleDir(args.Directory); err == nil {
		stack = []string{norm}
	} else if args.Directory != "" {
		stack = []string{args.Directory}
	}
	return &SofaConfig{
		BaseConfig:          base,
		directory:           args.Directory,
		modulesDirectory:    modulesDir,
		bundlePath:          filepath.ToSlash(strings.TrimSpace(args.BundlePath)),
		moduleStack:         stack,
		sensitiveVariables:  make(map[string]struct{}),
		variableSourceURLs:  make(map[string][]string),
		forEachModuleNames:  make(map[string]struct{}),
		deferredValidations: make(map[string][]specpkg.Condition),
	}
}

// Init validates sofa-specific declaration prerequisites and delegates graph
// construction and pre-plan work to golden.
func (c *SofaConfig) Init(blocks []*golden.HclBlock) error {
	preparedBlocks, usedInherited, err := applyModuleDefaults(
		blocks,
		c.addressPrefix,
		c.moduleDisplayName(),
		c.incomingInherited,
	)
	if err != nil {
		return err
	}
	c.injectedSingletons = usedInherited
	c.forEachModuleNames = make(map[string]struct{})
	seenModules := make(map[string]string)
	for _, block := range preparedBlocks {
		if block != nil && block.Type == "module" && len(block.Labels) == 2 {
			label := block.Labels[1]
			lower := strings.ToLower(label)
			if prev, exists := seenModules[lower]; exists {
				if prev == label {
					return fmt.Errorf("module %q is declared more than once", label)
				}
				return fmt.Errorf("module %q conflicts with module %q", label, prev)
			}
			seenModules[lower] = label
			if _, hasForEach := block.Body.Attributes["for_each"]; hasForEach {
				c.forEachModuleNames[label] = struct{}{}
			}
		}
	}
	if err := validateWorkerFallbacks(preparedBlocks); err != nil {
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
	if workerValue, ok := ctx.Variables["worker"]; ok &&
		!workerValue.IsNull() &&
		workerValue.Type().IsObjectType() &&
		workerValue.Type().HasAttribute("") {
		ctx.Variables["worker"] = workerValue.GetAttr("")
	}

	c.injectInheritedSingletonVariables(ctx)
	c.normalizeForEachModuleVariables(ctx)

	modulePath := filepath.ToSlash(c.directory)
	if c.bundlePath != "" {
		modulePath = filepath.ToSlash(c.bundlePath)
	}
	ctx.Variables["path"] = cty.ObjectVal(map[string]cty.Value{
		"module": cty.StringVal(modulePath),
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

func (c *SofaConfig) LookupRegisteredTool(id string) (workerspec.ModuleToolInfo, bool) {
	for _, b := range golden.Blocks[*toolspec.GoToolBlock](c) {
		if b.Id() == id {
			return workerspec.ModuleToolInfo{
				ID:         id,
				InputType:  b.InputConstraint().Type(),
				OutputType: b.OutputConstraint().Type(),
			}, true
		}
	}
	for _, b := range golden.Blocks[*toolspec.ExternalToolBlock](c) {
		if b.Id() == id {
			return workerspec.ModuleToolInfo{
				ID:         id,
				InputType:  b.InputConstraint().Type(),
				OutputType: b.OutputConstraint().Type(),
			}, true
		}
	}
	return workerspec.ModuleToolInfo{}, false
}

func (c *SofaConfig) HasRegisteredMCPTool(id string) bool {
	for _, b := range golden.Blocks[*mcpspec.ServerBlock](c) {
		for _, tool := range b.Tools {
			if b.ToolID(tool) == id {
				return true
			}
		}
	}
	return false
}

func (c *SofaConfig) HasRegisteredMCPResource(id string) bool {
	for _, b := range golden.Blocks[*mcpspec.ServerBlock](c) {
		for _, res := range b.Resources {
			if b.ResourceID(res) == id {
				return true
			}
		}
	}
	return false
}

func (c *SofaConfig) LookupSourceURL(name string) (string, bool) {
	ctx := c.EvalContext()
	for _, b := range golden.Blocks[*sourcespec.SourceBlock](c) {
		if b.Name() == name {
			if u := b.SourceConfig().URL; u != "" {
				return u, true
			}
			if u, ok := c.evalKnownHclStringAttr(b.HclBlock(), "url", ctx, 0); ok && u != "" {
				return u, true
			}
		}
	}
	return "", false
}

func (c *SofaConfig) SingleRootSourceURL() (string, int) {
	ctx := c.EvalContext()
	sources := golden.Blocks[*sourcespec.SourceBlock](c)
	if len(sources) == 0 && c.parentConfig != nil {
		return c.parentConfig.SingleRootSourceURL()
	}
	if len(sources) == 1 {
		if u := sources[0].SourceConfig().URL; u != "" {
			return u, 1
		}
		if u, ok := c.evalKnownHclStringAttr(sources[0].HclBlock(), "url", ctx, 0); ok {
			return u, 1
		}
	}
	return "", len(sources)
}

func (c *SofaConfig) LookupWorkerSourceURLs(address string) ([]string, bool) {
	if v, ok := c.GetVertices()[address]; ok {
		switch b := v.(type) {
		case *workerspec.WorkerBlock:
			return []string{b.WorkerConfig().SourceURL}, true
		case *workerspec.DynamicWorkerBlock:
			return []string{b.DynamicWorkerConfig().Content.SourceURL}, true
		case *mergespec.MergeBlock:
			return []string{b.MergeConfig().SourceURL}, true
		}
	}
	if !strings.Contains(address, "[") {
		prefix := address + "["
		var urls []string
		for _, wb := range golden.Blocks[*workerspec.WorkerBlock](c) {
			if strings.HasPrefix(wb.Address(), prefix) {
				urls = append(urls, wb.WorkerConfig().SourceURL)
			}
		}
		if len(urls) > 0 {
			return urls, true
		}
	}
	return nil, false
}

func (c *SofaConfig) LookupUpstreamSourceURLs(address string) ([]string, bool) {
	return c.LookupWorkerSourceURLs(address)
}

func (c *SofaConfig) LookupVariableSourceURLs(name string) ([]string, bool) {
	urls, ok := c.variableSourceURLs[name]
	if !ok || len(urls) == 0 {
		return nil, false
	}
	return append([]string(nil), urls...), true
}

func (c *SofaConfig) LookupModuleOutputSourceURLs(moduleAddress, outputName string) ([]string, bool) {
	collect := func(mb *modulespec.ModuleBlock) ([]string, bool) {
		mp := mb.PlannedModule()
		if outputName != "" {
			out, ok := mp.Outputs[outputName]
			if !ok {
				return nil, false
			}
			return append([]string(nil), out.SourceURLs...), len(out.SourceURLs) > 0
		}
		var all []string
		for _, out := range mp.Outputs {
			all = append(all, out.SourceURLs...)
		}
		return all, len(all) > 0
	}

	if v, ok := c.GetVertices()[moduleAddress]; ok {
		if mb, isMod := v.(*modulespec.ModuleBlock); isMod {
			return collect(mb)
		}
	}
	if !strings.Contains(moduleAddress, "[") {
		prefix := moduleAddress + "["
		var urls []string
		for _, mb := range golden.Blocks[*modulespec.ModuleBlock](c) {
			if strings.HasPrefix(mb.Address(), prefix) {
				if part, ok := collect(mb); ok {
					urls = append(urls, part...)
				}
			}
		}
		if len(urls) > 0 {
			return urls, true
		}
	}
	return nil, false
}

func (c *SofaConfig) SourceBranches() []mergespec.SourceBranchInfo {
	ctx := c.EvalContext()
	sources := golden.Blocks[*sourcespec.SourceBlock](c)
	out := make([]mergespec.SourceBranchInfo, 0, len(sources))
	seen := make(map[mergespec.SourceBranchInfo]struct{}, len(sources))
	if c.parentConfig != nil {
		for _, info := range c.parentConfig.SourceBranches() {
			if _, dup := seen[info]; !dup {
				seen[info] = struct{}{}
				out = append(out, info)
			}
		}
	}
	for _, s := range sources {
		url, urlOK := c.evalKnownHclStringAttr(s.HclBlock(), "url", ctx, 0)
		branch, branchOK := c.evalKnownHclStringAttr(s.HclBlock(), "branch", ctx, 0)
		if !urlOK || !branchOK {
			continue
		}
		info := mergespec.SourceBranchInfo{
			URL:    url,
			Branch: branch,
		}
		if _, dup := seen[info]; !dup {
			seen[info] = struct{}{}
			out = append(out, info)
		}
	}
	return out
}

func (c *SofaConfig) PreservedKnownBranches() []mergespec.PreservedBranchInfo {
	ctx := c.EvalContext()
	preserves := golden.Blocks[*sourcespec.PreserveBlock](c)
	out := make([]mergespec.PreservedBranchInfo, 0, len(preserves))
	seen := make(map[mergespec.PreservedBranchInfo]struct{}, len(preserves))
	if c.parentConfig != nil {
		for _, info := range c.parentConfig.PreservedKnownBranches() {
			if _, dup := seen[info]; !dup {
				seen[info] = struct{}{}
				out = append(out, info)
			}
		}
	}
	for _, p := range preserves {
		url, urlOK := c.evalKnownHclStringAttr(p.HclBlock(), "url", ctx, 0)
		branch, branchOK := c.evalKnownHclStringAttr(p.HclBlock(), "branch", ctx, 0)
		if !urlOK || !branchOK {
			continue
		}
		info := mergespec.PreservedBranchInfo{
			URL:    url,
			Branch: branch,
		}
		if _, dup := seen[info]; !dup {
			seen[info] = struct{}{}
			out = append(out, info)
		}
	}
	return out
}

func (c *SofaConfig) evalKnownHclStringAttr(
	block *golden.HclBlock,
	attrName string,
	ctx *hcl.EvalContext,
	depth int,
) (string, bool) {
	if block == nil || depth > 2 {
		return "", false
	}
	attr, ok := block.Attributes()[attrName]
	if !ok {
		return "", false
	}
	val, diags := attr.Expr.Value(ctx)
	var unmarked cty.Value
	if !diags.HasErrors() {
		unmarked, _ = val.UnmarkDeep()
	}
	if diags.HasErrors() || unmarked.IsNull() || !unmarked.IsWhollyKnown() {
		if traversal, tDiags := hcl.AbsTraversalForExpr(attr.Expr); !tDiags.HasErrors() && len(traversal) == 3 && traversal.RootName() == "source" {
			if srcName, ok := traversal[1].(hcl.TraverseAttr); ok {
				if fieldAttr, ok := traversal[2].(hcl.TraverseAttr); ok && (fieldAttr.Name == "url" || fieldAttr.Name == "branch") {
					for _, s := range golden.Blocks[*sourcespec.SourceBlock](c) {
						if s.Name() == srcName.Name {
							return c.evalKnownHclStringAttr(s.HclBlock(), fieldAttr.Name, ctx, depth+1)
						}
					}
				}
			}
		}
		return "", false
	}
	if !unmarked.Type().Equals(cty.String) {
		return "", false
	}
	return unmarked.AsString(), true
}

func (c *SofaConfig) ResolveLocalExpr(name string) (hcl.Expression, bool) {
	v, ok := c.GetVertices()["local."+name]
	if !ok {
		return nil, false
	}
	lb, isLocal := v.(*golden.LocalBlock)
	if !isLocal {
		return nil, false
	}
	attr, has := lb.HclBlock().Attributes()["value"]
	if !has {
		return nil, false
	}
	return attr.Expr, true
}

var _ golden.Config = (*SofaConfig)(nil)
var _ workerspec.ModuleRegistry = (*SofaConfig)(nil)
var _ mergespec.ModuleRegistry = (*SofaConfig)(nil)
