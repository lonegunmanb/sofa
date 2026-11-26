package spec

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

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
	LookupVariableSourceURLs(name string) ([]string, bool)
	LookupModuleOutputSourceURLs(moduleAddress, outputName string) ([]string, bool)
	ResolveLocalExpr(name string) (hcl.Expression, bool)
	SourceBranches() []SourceBranchInfo
	PreservedKnownBranches() []PreservedBranchInfo
}

var (
	mergeAttributes = map[string]struct{}{
		"based_source":   {},
		"target_branch":  {},
		"sources":        {},
		"strategy":       {},
		"max_reworks":    {},
		"human_approval": {},
		"sandbox":        {},
	}
	mergeNestedBlockTypes = map[string]struct{}{
		"gatekeeper": {},
	}
)

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

func (c Config) Clone() Config {
	cloned := c
	if c.TargetBranch != nil {
		branch := *c.TargetBranch
		cloned.TargetBranch = &branch
	}
	if c.Gatekeepers != nil {
		cloned.Gatekeepers = make([]workerspec.GatekeeperConfig, len(c.Gatekeepers))
		for i, g := range c.Gatekeepers {
			cloned.Gatekeepers[i] = g.Clone()
		}
	}
	return cloned
}

type MergeBlock struct {
	*golden.BaseBlock
	planned      Config
	plannedValue cty.Value
}

func (m *MergeBlock) Type() string            { return "" }
func (m *MergeBlock) BlockType() string       { return "merge" }
func (m *MergeBlock) AddressLength() int      { return 2 }
func (m *MergeBlock) CanExecutePrePlan() bool { return false }

func (m *MergeBlock) MergeConfig() Config {
	return m.planned.Clone()
}

func (m *MergeBlock) Value() cty.Value {
	if m.plannedValue == cty.NilVal {
		return cty.DynamicVal
	}
	return m.plannedValue
}

func (m *MergeBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error {
	m.planned = Config{}
	m.plannedValue = cty.NilVal

	if len(block.Labels) != 2 {
		return fmt.Errorf("merge block must have exactly one name")
	}
	if err := workerspec.ValidateSafeLabel(m.Name(), "merge", false); err != nil {
		return err
	}
	if err := validateMergeSchema(block); err != nil {
		return err
	}

	expanded := block
	if workerspec.HasDynamicBlocks(block) {
		var err error
		expanded, err = block.ExpandDynamicBlocks(baseCtx)
		if err != nil {
			return err
		}
		if err := validateMergeSchema(expanded); err != nil {
			return err
		}
	}

	scope := fmt.Sprintf("merge %q", m.Name())
	cfg := Config{
		Name:          m.Name(),
		Strategy:      "rebase",
		MaxReworks:    2,
		HumanApproval: "optional",
		Sandbox:       cty.NullVal(cty.EmptyObject),
	}

	basedSource, err := workerspec.RequiredStringAllowUnknown(expanded, baseCtx, "based_source", scope)
	if err != nil {
		return err
	}
	unmarkedBased, _ := basedSource.UnmarkDeep()
	if unmarkedBased.IsWhollyKnown() {
		if err := corespec.ValidateBranchName(unmarkedBased.AsString()); err != nil {
			return fmt.Errorf("%s based_source: %w", scope, err)
		}
	}
	cfg.BasedSource = basedSource
	cfg.BasedSourceExpr = expanded.Attributes()["based_source"].ExprString()

	if target, set, err := workerspec.OptionalKnownString(expanded, baseCtx, "target_branch", scope); err != nil {
		return err
	} else if set {
		if err := corespec.ValidateBranchName(target); err != nil {
			return fmt.Errorf("%s target_branch: %w", scope, err)
		}
		cfg.TargetBranch = &target
	}

	sourcesVal, sourcesExpr, err := decodeMergeSources(expanded, baseCtx, scope)
	if err != nil {
		return err
	}
	cfg.Sources = sourcesVal
	cfg.SourcesExpr = sourcesExpr

	if strategy, set, err := workerspec.OptionalKnownString(expanded, baseCtx, "strategy", scope); err != nil {
		return err
	} else if set {
		if strategy != "rebase" {
			return fmt.Errorf("%s strategy must be \"rebase\"", scope)
		}
		cfg.Strategy = strategy
	}

	maxReworks, err := workerspec.OptionalWholeInt(expanded, baseCtx, "max_reworks", 2, scope)
	if err != nil {
		return err
	}
	if maxReworks < 0 {
		return fmt.Errorf("%s max_reworks must be non-negative", scope)
	}
	cfg.MaxReworks = maxReworks

	if approval, set, err := workerspec.OptionalKnownString(expanded, baseCtx, "human_approval", scope); err != nil {
		return err
	} else if set {
		switch approval {
		case "optional", "required", "disabled":
			cfg.HumanApproval = approval
		default:
			return fmt.Errorf("%s human_approval must be optional, required, or disabled", scope)
		}
	}

	gatekeeperNames := map[string]struct{}{}
	for _, nested := range expanded.NestedBlocks() {
		if nested.Type != "gatekeeper" {
			continue
		}
		gk, err := workerspec.DecodeGatekeeper(nested, baseCtx)
		if err != nil {
			return err
		}
		if _, dup := gatekeeperNames[gk.Name]; dup {
			return fmt.Errorf("%s gatekeeper %q is declared more than once", scope, gk.Name)
		}
		gatekeeperNames[gk.Name] = struct{}{}
		cfg.Gatekeepers = append(cfg.Gatekeepers, gk)
	}

	if sandboxAttr, hasSandbox := expanded.Attributes()["sandbox"]; hasSandbox {
		sandboxVal, diags := sandboxAttr.Expr.Value(baseCtx)
		if diags.HasErrors() {
			return diags
		}
		unmarkedSandbox, _ := sandboxVal.UnmarkDeep()
		if !unmarkedSandbox.IsNull() {
			if err := workerspec.ValidateBlockReference(sandboxVal, "sandbox", scope+" sandbox"); err != nil {
				return err
			}
			cfg.Sandbox = sandboxVal
		}
	}
	if len(cfg.Gatekeepers) > 0 && cfg.Sandbox.IsNull() {
		return fmt.Errorf("%s with gatekeeper requires a sandbox", scope)
	}

	m.planned = cfg
	return nil
}

func (m *MergeBlock) ExecuteDuringPlan() error {
	if m.BaseBlock != nil && m.Config() != nil {
		if reg, ok := m.Config().(ModuleRegistry); ok {
			sourceURL, err := m.resolveSourceURL(reg)
			if err != nil {
				return err
			}
			m.planned.SourceURL = sourceURL

			if m.planned.TargetBranch != nil {
				if err := m.validateTargetBranchNotUnpreservedSource(reg); err != nil {
					return err
				}
			}
		}
	}

	m.plannedValue = cty.ObjectVal(map[string]cty.Value{
		"address":     cty.StringVal(m.Address()),
		"kind":        cty.StringVal("merge"),
		"name":        cty.StringVal(m.Name()),
		"work_branch": cty.UnknownVal(cty.String),
		"commit_sha":  cty.UnknownVal(cty.String),
	})
	return nil
}

func (m *MergeBlock) resolveSourceURL(reg ModuleRegistry) (string, error) {
	urls := map[string]struct{}{}
	visitedLocals := map[string]struct{}{}

	var inspectExpr func(hcl.Expression)
	inspectExpr = func(expr hcl.Expression) {
		if expr == nil {
			return
		}
		for _, traversal := range expr.Variables() {
			switch traversal.RootName() {
			case "local":
				if len(traversal) >= 2 {
					if localAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						if _, seen := visitedLocals[localAttr.Name]; seen {
							continue
						}
						visitedLocals[localAttr.Name] = struct{}{}
						if localExpr, exists := reg.ResolveLocalExpr(localAttr.Name); exists {
							inspectExpr(localExpr)
						}
					}
				}
			case "source":
				if len(traversal) >= 2 {
					if srcAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
						if u, exists := reg.LookupSourceURL(srcAttr.Name); exists && u != "" {
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
						if upstreamURLs, exists := reg.LookupUpstreamSourceURLs(addr); exists {
							for _, u := range upstreamURLs {
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
						if upstreamURLs, exists := reg.LookupUpstreamSourceURLs(addr); exists {
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
						if varURLs, exists := reg.LookupVariableSourceURLs(vAttr.Name); exists {
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
					if modURLs, exists := reg.LookupModuleOutputSourceURLs(addr, outputName); exists {
						for _, u := range modURLs {
							if u != "" {
								urls[u] = struct{}{}
							}
						}
					}
				}
			}
		}
	}

	if basedAttr, ok := m.HclBlock().Attributes()["based_source"]; ok {
		inspectExpr(basedAttr.Expr)
	}
	if sourcesAttr, ok := m.HclBlock().Attributes()["sources"]; ok {
		inspectExpr(sourcesAttr.Expr)
	}

	if len(urls) > 1 {
		return "", fmt.Errorf("merge %q references multiple distinct source repositories", m.Name())
	}
	if len(urls) == 1 {
		for u := range urls {
			return u, nil
		}
	}

	singleURL, sourceCount := reg.SingleRootSourceURL()
	switch sourceCount {
	case 0:
		return "", nil
	case 1:
		return singleURL, nil
	default:
		return "", fmt.Errorf("merge %q does not identify a source repository and %d source blocks exist; reference source.<name>.branch or an upstream block", m.Name(), sourceCount)
	}
}

func (m *MergeBlock) validateTargetBranchNotUnpreservedSource(reg ModuleRegistry) error {
	target := *m.planned.TargetBranch
	preserved := reg.PreservedKnownBranches()
	for _, src := range reg.SourceBranches() {
		if m.planned.SourceURL != "" && src.URL != "" && src.URL != m.planned.SourceURL {
			continue
		}
		if src.Branch != target {
			continue
		}
		protected := false
		for _, p := range preserved {
			if p.Branch == src.Branch && (p.URL == src.URL || p.URL == "" || src.URL == "") {
				protected = true
				break
			}
		}
		if !protected {
			return fmt.Errorf("merge %q target_branch %q targets a source branch without a matching preserve block", m.Name(), target)
		}
	}
	return nil
}

func validateMergeSchema(block *golden.HclBlock) error {
	for name := range block.Attributes() {
		if _, ok := mergeAttributes[name]; ok || golden.MetaAttributeNames.Contains(name) {
			continue
		}
		return fmt.Errorf("unsupported argument %q in merge block", name)
	}
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "gatekeeper":
			if len(nested.Labels) != 1 {
				return fmt.Errorf("merge gatekeeper block must have exactly one name")
			}
		case "dynamic":
			if len(nested.Labels) != 1 {
				return fmt.Errorf("merge dynamic block must declare a single target block label")
			}
			if _, ok := mergeNestedBlockTypes[nested.Labels[0]]; !ok {
				return fmt.Errorf("unsupported dynamic block type %q in merge block", nested.Labels[0])
			}
		default:
			if golden.MetaNestedBlockNames.Contains(nested.Type) {
				continue
			}
			return fmt.Errorf("unsupported block type %q in merge block", nested.Type)
		}
	}
	return nil
}

func decodeMergeSources(block *golden.HclBlock, ctx *hcl.EvalContext, scope string) (cty.Value, string, error) {
	attr, ok := block.Attributes()["sources"]
	if !ok {
		return cty.NilVal, "", fmt.Errorf("%s sources is required", scope)
	}
	val, err := validateMergeSourceExpr(attr.Expr, ctx, scope, false)
	if err != nil {
		return cty.NilVal, "", err
	}
	return val, attr.ExprString(), nil
}

func validateMergeSourceExpr(
	expr hcl.Expression,
	ctx *hcl.EvalContext,
	scope string,
	allowEmpty bool,
) (cty.Value, error) {
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NilVal, fmt.Errorf("%s sources must not be null", scope)
	}
	vt := unmarked.Type()
	switch {
	case vt == cty.DynamicPseudoType && !unmarked.IsKnown():
		call, ok := unwrapParensExpr(expr).(*hclsyntax.FunctionCallExpr)
		if !ok || call.Name != "concat" || len(call.Args) == 0 {
			return cty.NilVal, fmt.Errorf("%s sources must be a list or set of strings", scope)
		}
		for _, arg := range call.Args {
			if _, err := validateMergeSourceExpr(arg, ctx, scope, true); err != nil {
				return cty.NilVal, err
			}
		}
		return val, nil
	case vt.IsListType(), vt.IsSetType():
		if vt.ElementType() == cty.DynamicPseudoType && allowEmpty && unmarked.IsKnown() && unmarked.LengthInt() == 0 {
			return val, nil
		}
		if !vt.ElementType().Equals(cty.String) {
			return cty.NilVal, fmt.Errorf("%s sources must be a list or set of strings", scope)
		}
	case vt.IsTupleType():
		for _, elemType := range vt.TupleElementTypes() {
			if !elemType.Equals(cty.String) {
				return cty.NilVal, fmt.Errorf("%s sources must be a list or set of strings", scope)
			}
		}
	default:
		return cty.NilVal, fmt.Errorf("%s sources must be a list or set of strings", scope)
	}

	if unmarked.IsKnown() {
		if !allowEmpty && unmarked.LengthInt() == 0 {
			return cty.NilVal, fmt.Errorf("%s sources must not be an empty static list", scope)
		}
		for it := unmarked.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			if elem.IsNull() || !elem.Type().Equals(cty.String) {
				return cty.NilVal, fmt.Errorf("%s sources elements must be non-null strings", scope)
			}
			if elem.IsWhollyKnown() {
				if err := corespec.ValidateBranchName(elem.AsString()); err != nil {
					return cty.NilVal, fmt.Errorf("%s sources: %w", scope, err)
				}
			}
		}
	}
	return val, nil
}

func unwrapParensExpr(expr hcl.Expression) hcl.Expression {
	for {
		paren, ok := expr.(*hclsyntax.ParenthesesExpr)
		if !ok {
			return expr
		}
		expr = paren.Expression
	}
}

var _ golden.CustomDecode = (*MergeBlock)(nil)
var _ golden.PlanBlock = (*MergeBlock)(nil)
var _ golden.SingleValueBlock = (*MergeBlock)(nil)
