package planner

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/lonegunmanb/sofa/internal/module"
	modulespec "github.com/lonegunmanb/sofa/internal/module/spec"
	providerspec "github.com/lonegunmanb/sofa/internal/provider"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

type assignedValues map[string]cty.Value

func (a assignedValues) Variables(*golden.BaseConfig) (map[string]golden.VariableValueRead, error) {
	result := make(map[string]golden.VariableValueRead, len(a))
	for name, value := range a {
		cloned := value
		result[name] = golden.NewVariableValueRead(name, &cloned, nil)
	}
	return result, nil
}

type childVariableDecl struct {
	name             string
	varType          cty.Type
	defaults         *typeexpr.Defaults
	nullable         bool
	sensitive        bool
	hasDefault       bool
	defaultVal       cty.Value
	validationBlocks []*golden.HclBlock
}

var allowedVariableAttrs = map[string]struct{}{
	"type":        {},
	"default":     {},
	"description": {},
	"sensitive":   {},
	"nullable":    {},
}

// CanonicalAddress returns the globally unambiguous canonical block address,
// prefixing child module paths when planning inside nested modules.
func (c *SofaConfig) CanonicalAddress(address string) string {
	if c.addressPrefix == "" {
		return address
	}
	return c.addressPrefix + "." + address
}

// PlannedModules returns deep copies of all planned child modules keyed by
// their local block address (e.g. "module.audit" or "module.region[east]").
func (c *SofaConfig) PlannedModules() map[string]modulespec.ModulePlan {
	blocks := golden.Blocks[*modulespec.ModuleBlock](c)
	out := make(map[string]modulespec.ModulePlan, len(blocks))
	for _, b := range blocks {
		out[b.Address()] = b.PlannedModule()
	}
	return out
}

// PlanChildModule resolves, validates, and recursively plans a child module.
func (c *SofaConfig) PlanChildModule(req modulespec.ModulePlanRequest) (modulespec.ModulePlan, error) {
	canonicalAddr := c.CanonicalAddress(req.Address)
	staticAddr := staticModuleAddress(canonicalAddr)

	childDir, err := c.resolveChildModuleDirectory(req.Source, canonicalAddr, staticAddr)
	if err != nil {
		return modulespec.ModulePlan{}, err
	}
	if slices.Contains(c.moduleStack, childDir) {
		return modulespec.ModulePlan{}, fmt.Errorf("module source cycle at %s: %s", canonicalAddr, childDir)
	}

	loaded, diags, err := config.LoadDirectoryContext(c.Context(), childDir)
	if err != nil {
		return modulespec.ModulePlan{}, fmt.Errorf("reading module %s directory %q: %w", canonicalAddr, childDir, err)
	}
	if diags.HasErrors() {
		return modulespec.ModulePlan{}, diags
	}

	varDecls, err := inspectChildVariables(loaded.Blocks, canonicalAddr)
	if err != nil {
		return modulespec.ModulePlan{}, err
	}
	for inputName := range req.Inputs {
		if _, declared := varDecls[inputName]; !declared {
			return modulespec.ModulePlan{}, fmt.Errorf("module %s: input %q is not declared as a variable", canonicalAddr, inputName)
		}
	}

	outgoingInherited, err := c.outgoingInheritedSingletons(canonicalAddr)
	if err != nil {
		return modulespec.ModulePlan{}, err
	}

	childBundlePath := ""
	if c.bundlePath != "" {
		childBundlePath = "modules/" + strings.Join(staticModuleLabels(staticAddr), "/")
	}
	disabledVarDir := filepath.Join(childDir, ".sofa-disabled-child-varfiles")

	childConfig := NewSofaConfig(ConfigArgs{
		Context:          c.Context(),
		Directory:        childDir,
		ModulesDirectory: c.modulesDirectory,
		BundlePath:       childBundlePath,
		VarConfigDir:     &disabledVarDir,
	})
	childConfig.addressPrefix = canonicalAddr
	childConfig.moduleStack = append(slices.Clone(c.moduleStack), childDir)
	childConfig.incomingInherited = outgoingInherited
	childConfig.variableSourceURLs = c.resolveInputSourceURLs(req)
	childConfig.parentConfig = c
	childConfig.isChildModule = true

	assigned := make(map[string]cty.Value, len(varDecls))
	sensitiveVars := make(map[string]struct{})
	deferredValidations := make(map[string][]specpkg.Condition)
	stripValidationVars := make(map[string]struct{})

	names := make([]string, 0, len(varDecls))
	for name := range varDecls {
		names = append(names, name)
	}
	sort.Strings(names)

	emptyFns := childConfig.EmptyEvalContext().Functions
	for _, name := range names {
		decl := varDecls[name]
		converted, sensitive, deferred, err := bindChildVariable(decl, req.Inputs, canonicalAddr, emptyFns)
		if err != nil {
			return modulespec.ModulePlan{}, err
		}
		assigned[name] = converted
		if sensitive {
			sensitiveVars[name] = struct{}{}
		}
		if len(deferred) > 0 {
			deferredValidations[name] = deferred
		}
		if !converted.IsWhollyKnown() && len(decl.validationBlocks) > 0 {
			stripValidationVars[name] = struct{}{}
		}
	}

	childConfig.BaseConfig = config.NewBaseConfig(golden.NewBaseConfigArgs{
		Basedir:                  childDir,
		VarConfigDir:             &disabledVarDir,
		Ctx:                      c.Context(),
		DslAbbreviation:          "sofa",
		DslFullName:              "sofa",
		CliFlagAssignedVariables: []golden.CliFlagAssignedVariables{assignedValues(assigned)},
	})

	preparedBlocks := stripVariableValidations(loaded.Blocks, stripValidationVars)
	if err := childConfig.Init(preparedBlocks); err != nil {
		return modulespec.ModulePlan{}, err
	}
	for name := range sensitiveVars {
		childConfig.sensitiveVariables[name] = struct{}{}
	}
	childConfig.deferredValidations = maps.Clone(deferredValidations)

	if err := childConfig.RunPlan(); err != nil {
		return modulespec.ModulePlan{}, err
	}

	outputsList := childConfig.Outputs()
	outputBlocks := map[string]*golden.OutputBlock{}
	for _, ob := range golden.Blocks[*golden.OutputBlock](childConfig) {
		outputBlocks[ob.Name()] = ob
	}
	outputsMap := make(map[string]modulespec.Output, len(outputsList))
	for _, out := range outputsList {
		if ob := outputBlocks[out.Name]; ob != nil {
			if valAttr, ok := ob.HclBlock().Attributes()["value"]; ok {
				out.SourceURLs = childConfig.resolveExpressionSourceURLs(valAttr.Expr, nil)
			}
		}
		outputsMap[out.Name] = out
	}

	exportedInputs := maps.Clone(req.Inputs)
	for name := range sensitiveVars {
		if val, exists := exportedInputs[name]; exists {
			exportedInputs[name] = specpkg.MarkSensitive(val)
		}
	}

	return modulespec.ModulePlan{
		Plan: modulespec.Plan{
			Directory:           childDir,
			BundlePath:          filepath.ToSlash(childBundlePath),
			Outputs:             outputsMap,
			Modules:             childConfig.PlannedModules(),
			DeferredValidations: deferredValidations,
			ChildConfig:         childConfig,
		},
		Address:          req.Address,
		Name:             req.Name,
		CanonicalAddress: canonicalAddr,
		Source:           req.Source,
		Parallelism:      req.Parallelism,
		Timeout:          req.Timeout,
		TimeoutRaw:       req.TimeoutRaw,
		Inputs:           exportedInputs,
		InputExprs:       maps.Clone(req.InputExprs),
	}, nil
}

func (c *SofaConfig) resolveChildModuleDirectory(source, canonicalAddr, staticAddr string) (string, error) {
	if c.modulesDirectory != "" {
		dir, err := module.Directory(c.modulesDirectory, staticAddr)
		if err != nil {
			return "", fmt.Errorf("resolve initialized module %s: %w", staticAddr, err)
		}
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			if statErr != nil {
				return "", fmt.Errorf("module %s is not initialized; run sofa init: %w", staticAddr, statErr)
			}
			return "", fmt.Errorf("module %s path %q is not a directory; run sofa init", staticAddr, dir)
		}
		return normalizeModuleDir(dir)
	}

	if !module.IsLocalSource(source) {
		return "", fmt.Errorf("module %s uses remote source %q and is not initialized; run sofa init", canonicalAddr, source)
	}
	candidate := source
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(c.directory, candidate)
	}
	normalized, err := normalizeModuleDir(candidate)
	if err != nil {
		return "", fmt.Errorf("reading module %s directory %q: %w", canonicalAddr, source, err)
	}
	return normalized, nil
}

func normalizeModuleDir(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	return abs, nil
}

func inspectChildVariables(blocks []*golden.HclBlock, canonicalAddr string) (map[string]childVariableDecl, error) {
	decls := make(map[string]childVariableDecl)
	for _, block := range blocks {
		if block == nil || block.Type != "variable" {
			continue
		}
		if len(block.Labels) != 2 {
			return nil, fmt.Errorf("module %s contains variable block without a valid name", canonicalAddr)
		}
		name := block.Labels[1]
		if _, dup := decls[name]; dup {
			return nil, fmt.Errorf("module %s declares duplicate variable %q", canonicalAddr, name)
		}
		for attrName := range block.Attributes() {
			if _, allowed := allowedVariableAttrs[attrName]; !allowed {
				return nil, fmt.Errorf("module %s variable %q has unsupported argument %q", canonicalAddr, name, attrName)
			}
		}

		typeAttr, hasType := block.Body.Attributes["type"]
		if !hasType {
			return nil, fmt.Errorf("module %s variable %q MUST declare a concrete type", canonicalAddr, name)
		}
		varType, defaults, diags := typeexpr.TypeConstraintWithDefaults(typeAttr.Expr)
		if diags.HasErrors() {
			return nil, fmt.Errorf("module %s variable %q has invalid type: %s", canonicalAddr, name, diags.Error())
		}
		if err := specpkg.ValidateType(varType); err != nil {
			return nil, fmt.Errorf("module %s variable %q: %w", canonicalAddr, name, err)
		}

		nullable := true
		if nullAttr, hasNullable := block.Body.Attributes["nullable"]; hasNullable {
			val, diags := nullAttr.Expr.Value(nil)
			if diags.HasErrors() {
				return nil, fmt.Errorf("module %s variable %q nullable: %s", canonicalAddr, name, diags.Error())
			}
			unmarked, _ := val.UnmarkDeep()
			if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.Bool) {
				return nil, fmt.Errorf("module %s variable %q nullable MUST be a known boolean", canonicalAddr, name)
			}
			nullable = unmarked.True()
		}

		sensitive := false
		if sensAttr, hasSens := block.Body.Attributes["sensitive"]; hasSens {
			val, diags := sensAttr.Expr.Value(nil)
			if diags.HasErrors() {
				return nil, fmt.Errorf("module %s variable %q sensitive: %s", canonicalAddr, name, diags.Error())
			}
			unmarked, _ := val.UnmarkDeep()
			if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.Bool) {
				return nil, fmt.Errorf("module %s variable %q sensitive flag MUST be a known boolean", canonicalAddr, name)
			}
			sensitive = unmarked.True()
		}

		var hasDefault bool
		var defaultVal cty.Value
		if defAttr, ok := block.Body.Attributes["default"]; ok {
			val, diags := defAttr.Expr.Value(nil)
			if diags.HasErrors() {
				return nil, fmt.Errorf("module %s variable %q default: %s", canonicalAddr, name, diags.Error())
			}
			unmarked, _ := val.UnmarkDeep()
			if unmarked.IsNull() && !nullable {
				return nil, fmt.Errorf("module %s variable %q default cannot be null when nullable = false", canonicalAddr, name)
			}
			if !unmarked.IsNull() && defaults != nil {
				unmarked = defaults.Apply(unmarked)
			}
			converted, err := convert.Convert(unmarked, varType)
			if err != nil {
				return nil, fmt.Errorf("module %s variable %q default incompatible with %s: %w", canonicalAddr, name, varType.FriendlyName(), err)
			}
			hasDefault = true
			defaultVal = converted
		}

		var validations []*golden.HclBlock
		for _, nested := range block.NestedBlocks() {
			if nested.Type != "validation" {
				return nil, fmt.Errorf("module %s variable %q has unsupported block %q", canonicalAddr, name, nested.Type)
			}
			validations = append(validations, nested)
		}

		decls[name] = childVariableDecl{
			name:             name,
			varType:          varType,
			defaults:         defaults,
			nullable:         nullable,
			sensitive:        sensitive,
			hasDefault:       hasDefault,
			defaultVal:       defaultVal,
			validationBlocks: validations,
		}
	}
	return decls, nil
}

func bindChildVariable(
	decl childVariableDecl,
	inputs map[string]cty.Value,
	canonicalAddr string,
	fns map[string]function.Function,
) (converted cty.Value, sensitive bool, deferred []specpkg.Condition, err error) {
	sensitive = decl.sensitive

	var raw cty.Value
	providedVal, provided := inputs[decl.name]
	if !provided {
		if !decl.hasDefault {
			return cty.NilVal, false, nil, fmt.Errorf("module %s missing required input variable %q", canonicalAddr, decl.name)
		}
		raw = decl.defaultVal
	} else {
		if specpkg.IsSensitive(providedVal) {
			sensitive = true
		}
		unmarked, _ := providedVal.UnmarkDeep()
		if unmarked.IsNull() {
			if !decl.nullable {
				return cty.NilVal, false, nil, fmt.Errorf("module %s input %q may not be null", canonicalAddr, decl.name)
			}
			if decl.hasDefault {
				raw = decl.defaultVal
			} else {
				raw = cty.NullVal(decl.varType)
			}
		} else {
			raw = unmarked
		}
	}

	normalized := raw
	if hasUnknownDynamicType(normalized) {
		return cty.NilVal, false, nil, fmt.Errorf(
			"module %s input %q has an untyped dynamic unknown value; use jsondecodewithtype(...) to provide a concrete type",
			canonicalAddr,
			decl.name,
		)
	}
	var convErr error
	switch {
	case !normalized.IsKnown() && decl.defaults != nil && normalized.Type().IsObjectType():
		synthesized := synthesizeUnknownObjectForDefaults(normalized.Type())
		applied := decl.defaults.Apply(synthesized)
		converted, convErr = convert.Convert(applied, decl.varType)
		if convErr == nil {
			converted = cty.UnknownVal(decl.varType)
		}
	default:
		if decl.defaults != nil && !normalized.IsNull() && normalized.IsKnown() {
			normalized = decl.defaults.Apply(normalized)
		}
		converted, convErr = convert.Convert(normalized, decl.varType)
	}
	if convErr != nil {
		return cty.NilVal, false, nil, fmt.Errorf(
			"module %s input %q: incompatible type, want %s, got %s: %w",
			canonicalAddr,
			decl.name,
			decl.varType.FriendlyName(),
			normalized.Type().FriendlyName(),
			convErr,
		)
	}

	if !converted.IsWhollyKnown() && len(decl.validationBlocks) > 0 {
		deferred, err = checkDeferredVariableValidations(decl, converted, canonicalAddr, fns)
		if err != nil {
			return cty.NilVal, false, nil, err
		}
	}
	return converted, sensitive, deferred, nil
}

func hasUnknownDynamicType(v cty.Value) bool {
	if v == cty.NilVal {
		return false
	}
	unmarked, _ := v.UnmarkDeep()
	if unmarked.IsNull() {
		return false
	}
	if !unmarked.IsKnown() {
		return unmarked.Type().Equals(cty.DynamicPseudoType) || unmarked.Type().HasDynamicTypes()
	}
	vt := unmarked.Type()
	switch {
	case vt.IsListType(), vt.IsTupleType(), vt.IsSetType(), vt.IsMapType(), vt.IsObjectType():
		for it := unmarked.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			if hasUnknownDynamicType(elem) {
				return true
			}
		}
	}
	return false
}

func synthesizeUnknownObjectForDefaults(t cty.Type) cty.Value {
	if !t.IsObjectType() {
		return cty.UnknownVal(t)
	}
	attrTypes := t.AttributeTypes()
	if len(attrTypes) == 0 {
		return cty.EmptyObjectVal
	}
	attrs := make(map[string]cty.Value, len(attrTypes))
	for name, attrType := range attrTypes {
		if attrType.IsObjectType() {
			attrs[name] = synthesizeUnknownObjectForDefaults(attrType)
		} else {
			attrs[name] = cty.UnknownVal(attrType)
		}
	}
	return cty.ObjectVal(attrs)
}

func checkDeferredVariableValidations(
	decl childVariableDecl,
	val cty.Value,
	canonicalAddr string,
	fns map[string]function.Function,
) ([]specpkg.Condition, error) {
	var deferred []specpkg.Condition
	evalCtx := &hcl.EvalContext{
		Functions: fns,
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				decl.name: val,
			}),
		},
	}
	for _, vb := range decl.validationBlocks {
		if len(vb.NestedBlocks()) > 0 {
			return nil, fmt.Errorf("module %s variable %q validation does not support nested blocks", canonicalAddr, decl.name)
		}
		for attrName := range vb.Attributes() {
			if attrName != "condition" && attrName != "error_message" {
				return nil, fmt.Errorf("module %s variable %q validation has unsupported argument %q", canonicalAddr, decl.name, attrName)
			}
		}
		condAttr, hasCond := vb.Attributes()["condition"]
		if !hasCond {
			return nil, fmt.Errorf("module %s variable %q validation requires condition", canonicalAddr, decl.name)
		}
		msgAttr, hasMsg := vb.Attributes()["error_message"]
		if !hasMsg {
			return nil, fmt.Errorf("module %s variable %q validation requires error_message", canonicalAddr, decl.name)
		}

		msgVal, msgDiags := msgAttr.Expr.Value(&hcl.EvalContext{Functions: fns})
		if msgDiags.HasErrors() {
			return nil, msgDiags
		}
		unmarkedMsg, _ := msgVal.UnmarkDeep()
		if unmarkedMsg.IsNull() || !unmarkedMsg.IsWhollyKnown() || !unmarkedMsg.Type().Equals(cty.String) || strings.TrimSpace(unmarkedMsg.AsString()) == "" {
			return nil, fmt.Errorf("module %s variable %q validation error_message must be a known non-empty string", canonicalAddr, decl.name)
		}
		errMsg := strings.TrimSpace(unmarkedMsg.AsString())

		condVal, condDiags := condAttr.Expr.Value(evalCtx)
		if condDiags.HasErrors() {
			return nil, fmt.Errorf("module %s variable %q validation condition: %w", canonicalAddr, decl.name, condDiags)
		}
		unmarkedCond, _ := condVal.UnmarkDeep()
		if !unmarkedCond.Type().Equals(cty.Bool) {
			return nil, fmt.Errorf("module %s variable %q validation condition must return bool, got %s", canonicalAddr, decl.name, unmarkedCond.Type().FriendlyName())
		}
		if unmarkedCond.IsWhollyKnown() {
			if unmarkedCond.IsNull() {
				return nil, fmt.Errorf("module %s variable %q validation condition must not return null", canonicalAddr, decl.name)
			}
			if !unmarkedCond.True() {
				return nil, fmt.Errorf("invalid value for variable %s in %s: %s", decl.name, canonicalAddr, errMsg)
			}
			continue
		}
		deferred = append(deferred, specpkg.Condition{
			Expression:   condAttr.ExprString(),
			ErrorMessage: errMsg,
		})
	}
	return deferred, nil
}

func stripVariableValidations(blocks []*golden.HclBlock, vars map[string]struct{}) []*golden.HclBlock {
	if len(vars) == 0 {
		return blocks
	}
	out := make([]*golden.HclBlock, len(blocks))
	for i, block := range blocks {
		if block != nil && block.Type == "variable" && len(block.Labels) == 2 {
			if _, strip := vars[block.Labels[1]]; strip {
				syntaxBlock := golden.CloneHclSyntaxBlock(block.Block)
				var keptSyntax []*hclsyntax.Block
				for _, nested := range syntaxBlock.Body.Blocks {
					if nested.Type != "validation" {
						keptSyntax = append(keptSyntax, nested)
					}
				}
				syntaxBlock.Body.Blocks = keptSyntax

				writeBlock := hclwrite.NewBlock(block.Block.Type, append([]string(nil), block.Block.Labels[1:]...))
				for name, attr := range block.Attributes() {
					writeBlock.Body().SetAttributeRaw(name, attr.ExprTokens())
				}
				for _, nested := range block.NestedBlocks() {
					if nested.Type != "validation" {
						writeBlock.Body().AppendBlock(cloneWriteBlock(nested))
					}
				}
				out[i] = golden.NewHclBlock(syntaxBlock, writeBlock, block.ForEach)
				continue
			}
		}
		out[i] = block
	}
	return out
}

func (c *SofaConfig) moduleDisplayName() string {
	if c.addressPrefix == "" {
		return "root module"
	}
	return c.addressPrefix
}

func (c *SofaConfig) outgoingInheritedSingletons(childCanonicalAddr string) (InheritedSingletons, error) {
	origin := c.moduleDisplayName()
	sandbox, err := resolveOutgoingSingleton[*sandboxspec.SandboxBlock](c, c.incomingInherited.Sandbox, "sandbox", origin, childCanonicalAddr)
	if err != nil {
		return InheritedSingletons{}, err
	}
	harness, err := resolveOutgoingSingleton[*harnessspec.HarnessBlock](c, c.incomingInherited.Harness, "harness", origin, childCanonicalAddr)
	if err != nil {
		return InheritedSingletons{}, err
	}
	provider, err := resolveOutgoingSingleton[*providerspec.ModelProviderBlock](c, c.incomingInherited.ModelProvider, "model_provider", origin, childCanonicalAddr)
	if err != nil {
		return InheritedSingletons{}, err
	}
	return InheritedSingletons{
		Sandbox:       sandbox,
		Harness:       harness,
		ModelProvider: provider,
	}, nil
}

func resolveOutgoingSingleton[T golden.SingleValueBlock](
	c *SofaConfig,
	incoming InheritedSingleton,
	kind string,
	origin string,
	childCanonicalAddr string,
) (InheritedSingleton, error) {
	blocks := golden.Blocks[T](c)
	switch len(blocks) {
	case 0:
		return incoming, nil
	case 1:
		b := blocks[0]
		val := b.Value()
		if !val.IsKnown() {
			return InheritedSingleton{}, fmt.Errorf("%s %s was not planned before module %s", kind, c.CanonicalAddress(b.Address()), childCanonicalAddr)
		}
		return InheritedSingleton{
			Status: SingletonUnique,
			Name:   b.Name(),
			Value:  canonicalizeSingletonValue(val, c.CanonicalAddress(b.Address())),
			Origin: origin,
		}, nil
	default:
		names := make([]string, len(blocks))
		for i, b := range blocks {
			names[i] = b.Name()
		}
		sort.Strings(names)
		return InheritedSingleton{
			Status: SingletonAmbiguous,
			Names:  names,
			Origin: origin,
		}, nil
	}
}

func canonicalizeSingletonValue(val cty.Value, canonicalAddr string) cty.Value {
	unmarked, marks := val.Unmark()
	if !unmarked.IsKnown() || unmarked.IsNull() || !unmarked.Type().IsObjectType() {
		return val
	}
	attrs := unmarked.AsValueMap()
	attrs["address"] = cty.StringVal(canonicalAddr)
	return cty.ObjectVal(attrs).WithMarks(marks)
}

func (c *SofaConfig) injectInheritedSingletonVariables(ctx *hcl.EvalContext) {
	inject := func(dim string, inh InheritedSingleton) {
		if inh.Status != SingletonUnique || inh.Name == "" || inh.Value == cty.NilVal {
			return
		}
		existing := map[string]cty.Value{}
		if v, ok := ctx.Variables[dim]; ok && !v.IsNull() && v.IsKnown() && v.Type().IsObjectType() {
			existing = v.AsValueMap()
		}
		existing[inh.Name] = inh.Value
		ctx.Variables[dim] = cty.ObjectVal(existing)
	}
	inject("sandbox", c.injectedSingletons.Sandbox)
	inject("harness", c.injectedSingletons.Harness)
	inject("model_provider", c.injectedSingletons.ModelProvider)
}

func (c *SofaConfig) normalizeForEachModuleVariables(ctx *hcl.EvalContext) {
	if len(c.forEachModuleNames) == 0 {
		return
	}
	modMap := map[string]cty.Value{}
	if existing, ok := ctx.Variables["module"]; ok && !existing.IsNull() && existing.IsKnown() && existing.Type().IsObjectType() {
		modMap = existing.AsValueMap()
	}

	expanded := map[string]map[string]cty.Value{}
	unexpanded := map[string]bool{}
	for _, mb := range golden.Blocks[*modulespec.ModuleBlock](c) {
		name := mb.Name()
		if _, isForEach := c.forEachModuleNames[name]; !isForEach {
			continue
		}
		if mb.HclBlock().ForEach == nil {
			unexpanded[name] = true
			continue
		}
		key, ok := forEachKeyFromAddress(mb.Address(), name)
		if !ok {
			continue
		}
		if expanded[name] == nil {
			expanded[name] = map[string]cty.Value{}
		}
		expanded[name][key] = mb.Value()
	}

	for name := range c.forEachModuleNames {
		if insts := expanded[name]; len(insts) > 0 {
			modMap[name] = cty.ObjectVal(insts)
		} else if !unexpanded[name] {
			modMap[name] = cty.EmptyObjectVal
		}
	}
	if len(modMap) == 0 {
		ctx.Variables["module"] = cty.EmptyObjectVal
		return
	}
	ctx.Variables["module"] = cty.ObjectVal(modMap)
}

func forEachKeyFromAddress(address, name string) (string, bool) {
	prefix := "module." + name + "["
	if !strings.HasPrefix(address, prefix) || !strings.HasSuffix(address, "]") {
		return "", false
	}
	return address[len(prefix) : len(address)-1], true
}

func staticModuleAddress(canonicalAddress string) string {
	parts := strings.Split(canonicalAddress, ".")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if idx := strings.IndexByte(part, '['); idx >= 0 {
			part = part[:idx]
		}
		out = append(out, part)
	}
	return strings.Join(out, ".")
}

func staticModuleLabels(staticAddress string) []string {
	parts := strings.Split(staticAddress, ".")
	labels := make([]string, 0, len(parts)/2)
	for i := 1; i < len(parts); i += 2 {
		labels = append(labels, parts[i])
	}
	return labels
}

func (c *SofaConfig) resolveInputSourceURLs(req modulespec.ModulePlanRequest) map[string][]string {
	if len(req.InputAttrs) == 0 {
		return map[string][]string{}
	}
	var forEachExpr hcl.Expression
	if v, ok := c.GetVertices()[req.Address]; ok {
		if mb, isMod := v.(*modulespec.ModuleBlock); isMod {
			if feAttr, hasFE := mb.HclBlock().Attributes()["for_each"]; hasFE {
				forEachExpr = feAttr.Expr
			}
		}
	}
	out := make(map[string][]string, len(req.InputAttrs))
	for name, attr := range req.InputAttrs {
		if attr == nil {
			continue
		}
		if urls := c.resolveExpressionSourceURLs(attr.Expr, forEachExpr); len(urls) > 0 {
			out[name] = urls
		}
	}
	return out
}

func (c *SofaConfig) resolveExpressionSourceURLs(expr hcl.Expression, eachForEachExpr hcl.Expression) []string {
	if expr == nil {
		return nil
	}
	urls := map[string]struct{}{}
	visitedLocals := map[string]struct{}{}

	var inspect func(hcl.Expression)
	inspect = func(e hcl.Expression) {
		if e == nil {
			return
		}
		for _, traversal := range e.Variables() {
			switch traversal.RootName() {
			case "local":
				if len(traversal) >= 2 {
					if localAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						if _, seen := visitedLocals[localAttr.Name]; seen {
							continue
						}
						visitedLocals[localAttr.Name] = struct{}{}
						if localExpr, exists := c.ResolveLocalExpr(localAttr.Name); exists {
							inspect(localExpr)
						}
					}
				}
			case "source":
				if len(traversal) >= 2 {
					if srcAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						if u, exists := c.LookupSourceURL(srcAttr.Name); exists && u != "" {
							urls[u] = struct{}{}
						}
					}
				}
			case "worker":
				if len(traversal) >= 2 {
					if wAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						addr := "worker." + wAttr.Name
						if len(traversal) >= 3 {
							if idx, ok := traversal[2].(hcl.TraverseIndex); ok {
								k, _ := idx.Key.UnmarkDeep()
								if k.IsKnown() && !k.IsNull() && (k.Type().Equals(cty.String) || k.Type().Equals(cty.Number)) {
									addr = fmt.Sprintf("%s[%s]", addr, golden.CtyValueToString(k))
								}
							}
						}
						if workerURLs, exists := c.LookupWorkerSourceURLs(addr); exists {
							for _, u := range workerURLs {
								if u != "" {
									urls[u] = struct{}{}
								}
							}
						}
					}
				}
			case "dynamic_worker", "merge":
				if len(traversal) >= 2 {
					if uAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						addr := traversal.RootName() + "." + uAttr.Name
						if upstreamURLs, exists := c.LookupWorkerSourceURLs(addr); exists {
							for _, u := range upstreamURLs {
								if u != "" {
									urls[u] = struct{}{}
								}
							}
						}
					}
				}
			case "var":
				if len(traversal) >= 2 {
					if vAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						if varURLs, exists := c.LookupVariableSourceURLs(vAttr.Name); exists {
							for _, u := range varURLs {
								if u != "" {
									urls[u] = struct{}{}
								}
							}
						}
					}
				}
			case "module":
				if addr, outputName, ok := workerspec.ParseModuleOutputTraversal(traversal); ok {
					if modURLs, exists := c.LookupModuleOutputSourceURLs(addr, outputName); exists {
						for _, u := range modURLs {
							if u != "" {
								urls[u] = struct{}{}
							}
						}
					}
				}
			case "each":
				if eachForEachExpr != nil {
					inspect(eachForEachExpr)
				}
			}
		}
	}

	inspect(expr)
	if len(urls) == 0 {
		return nil
	}
	out := make([]string, 0, len(urls))
	for u := range urls {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

var _ modulespec.ModulePlanner = (*SofaConfig)(nil)
