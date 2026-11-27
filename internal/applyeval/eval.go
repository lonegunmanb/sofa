package applyeval

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/plan"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// MaterializedMerge holds a resolved merge node configuration ready for MergeActor execution.
type MaterializedMerge struct {
	Name          string
	BasedSource   string
	SourceURL     string
	TargetBranch  *string
	Sources       []string
	SourcesKnown  bool
	Strategy      string
	MaxReworks    int
	HumanApproval string
	Sandbox       cty.Value
	Gatekeepers   []workerspec.GatekeeperConfig
}

// BoundModule captures a validated child module with runtime inputs materialized into its Plan.
type BoundModule struct {
	Plan      *plan.Plan
	Inputs    map[string]cty.Value
	VarObject cty.Value
}

// EvalReadyWorker materializes a planned worker node into a wholly-known workerspec.Config.
func EvalReadyWorker(ctx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (workerspec.Config, error) {
	if node.Kind != "worker" {
		return workerspec.Config{}, fmt.Errorf("applyeval: expected worker node, got %q (%s)", node.Kind, node.Address)
	}
	if ctx == nil {
		return workerspec.Config{}, fmt.Errorf("applyeval: eval context is nil")
	}
	evalOpts := resolveEvalOptions(nil, opts)

	baseCfg, err := decodeWorkerConfigFromPlanVal(node.Config, node.Address)
	if err != nil {
		return workerspec.Config{}, err
	}

	deferredExpr := strings.TrimSpace(baseCfg.DeferredExpression)
	if deferredExpr == "" {
		if err := validateMaterializedWorker(&baseCfg, node.Address); err != nil {
			return workerspec.Config{}, err
		}
		if baseCfg.SourceURL == "" {
			baseCfg.SourceURL = resolveApplySourceURL(ctx, evalOpts, node, "")
		}
		return baseCfg, nil
	}

	evalCtx := withNodeEachContext(ctx, node)
	workerCtx := attachWorkerLocals(evalCtx, node.Address, baseCfg.Artifacts, baseCfg.Imports)
	evaluated, err := evalHCLObjectExpression(deferredExpr, workerCtx, node.Address)
	if err != nil {
		return workerspec.Config{}, err
	}

	mat, err := decodeWorkerConfigFromDeferredObject(evaluated, baseCfg, node.Address)
	if err != nil {
		return workerspec.Config{}, err
	}
	if err := validateMaterializedWorker(&mat, node.Address); err != nil {
		return workerspec.Config{}, err
	}
	if mat.SourceURL == "" {
		mat.SourceURL = resolveApplySourceURL(ctx, evalOpts, node, "")
	}
	return mat, nil
}

// EvalDynamicWorkerKeys evaluates dynamic_worker.for_each and returns sorted expansion keys.
// Empty collections return []string{} with nil error; null collections/elements and invalid types fail.
func EvalDynamicWorkerKeys(ctx *hcl.EvalContext, node plan.NodeSpec) ([]string, error) {
	keys, _, err := EvalDynamicWorkerCollection(ctx, node)
	return keys, err
}

// EvalDynamicWorkerCollection evaluates dynamic_worker.for_each and returns deterministically
// sorted keys alongside each key's element value (preserving sensitivity marks).
func EvalDynamicWorkerCollection(ctx *hcl.EvalContext, node plan.NodeSpec) ([]string, map[string]cty.Value, error) {
	if node.Kind != "dynamic_worker" {
		return nil, nil, fmt.Errorf("applyeval: expected dynamic_worker node, got %q (%s)", node.Kind, node.Address)
	}
	if ctx == nil {
		return nil, nil, fmt.Errorf("applyeval: eval context is nil")
	}

	var forEachVal cty.Value
	forEachExpr := strings.TrimSpace(readStringAttr(node.Config, "for_each_expr"))
	if forEachExpr != "" {
		expr, diags := hclsyntax.ParseExpression([]byte(forEachExpr), node.Address+".for_each", hcl.InitialPos)
		if diags.HasErrors() {
			return nil, nil, fmt.Errorf("%s for_each: %w", node.Address, diags)
		}
		val, evalDiags := expr.Value(ctx)
		if evalDiags.HasErrors() {
			return nil, nil, fmt.Errorf("%s for_each: %w", node.Address, evalDiags)
		}
		forEachVal = val
	} else if hasAttr(node.Config, "for_each") {
		forEachVal = node.Config.GetAttr("for_each")
	} else {
		return nil, nil, fmt.Errorf("%s dynamic_worker missing for_each", node.Address)
	}

	return extractForEachCollection(forEachVal, node.Address)
}

func extractForEachCollection(forEachVal cty.Value, scope string) ([]string, map[string]cty.Value, error) {
	if forEachVal == cty.NilVal {
		return nil, nil, fmt.Errorf("%s for_each must not be null", scope)
	}
	_, outerMarks := forEachVal.Unmark()
	unmarked, _ := forEachVal.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil, fmt.Errorf("%s for_each must not be null", scope)
	}
	if !unmarked.IsWhollyKnown() {
		return nil, nil, fmt.Errorf("%s for_each must be wholly known during apply", scope)
	}

	t := unmarked.Type()
	switch {
	case t.IsMapType(), t.IsObjectType():
		keys := make([]string, 0, unmarked.LengthInt())
		elems := make(map[string]cty.Value, unmarked.LengthInt())
		for k := range unmarked.AsValueMap() {
			keys = append(keys, k)
			if t.IsMapType() {
				elems[k] = forEachVal.Index(cty.StringVal(k))
			} else {
				elems[k] = forEachVal.GetAttr(k)
			}
		}
		slices.Sort(keys)
		return keys, elems, nil

	case t.IsSetType():
		if !t.ElementType().Equals(cty.String) {
			return nil, nil, fmt.Errorf("%s for_each must be a map, object, or set of strings", scope)
		}
		keys := make([]string, 0, unmarked.LengthInt())
		elems := make(map[string]cty.Value, unmarked.LengthInt())
		for it := forEachVal.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			unmarkedElem, _ := elem.UnmarkDeep()
			if unmarkedElem.IsNull() {
				return nil, nil, fmt.Errorf("%s for_each set must not contain null elements", scope)
			}
			k := unmarkedElem.AsString()
			keys = append(keys, k)
			elems[k] = elem.WithMarks(outerMarks)
		}
		slices.Sort(keys)
		return keys, elems, nil

	default:
		return nil, nil, fmt.Errorf("%s for_each must be a map, object, or set of strings, got %s", scope, t.FriendlyName())
	}
}

// ProjectDynamicInstance binds the dynamic_worker iterator for key (preserving elemVal
// sensitivity marks) and materializes ContentTemplate into a wholly-known workerspec.Config.
func ProjectDynamicInstance(
	ctx *hcl.EvalContext,
	node plan.NodeSpec,
	key string,
	elemVal cty.Value,
	opts ...EvalOption,
) (workerspec.Config, error) {
	if node.Kind != "dynamic_worker" {
		return workerspec.Config{}, fmt.Errorf("applyeval: expected dynamic_worker node, got %q (%s)", node.Kind, node.Address)
	}
	if ctx == nil {
		return workerspec.Config{}, fmt.Errorf("applyeval: eval context is nil")
	}
	evalOpts := resolveEvalOptions(nil, opts)

	if elemVal == cty.NilVal {
		keys, elems, err := EvalDynamicWorkerCollection(ctx, node)
		if err != nil {
			return workerspec.Config{}, err
		}
		found := false
		for _, k := range keys {
			if k == key {
				elemVal = elems[k]
				found = true
				break
			}
		}
		if !found {
			return workerspec.Config{}, fmt.Errorf("%s key %q not present in for_each collection", node.Address, key)
		}
	}

	iteratorName := strings.TrimSpace(readStringAttr(node.Config, "iterator"))
	if iteratorName == "" {
		iteratorName = "worker"
	}

	if !hasAttr(node.Config, "content") {
		return workerspec.Config{}, fmt.Errorf("%s missing content configuration", node.Address)
	}
	contentVal := node.Config.GetAttr("content")
	baseCfg, err := decodeWorkerConfigFromPlanVal(contentVal, fmt.Sprintf("%s[%s]", node.Address, key))
	if err != nil {
		return workerspec.Config{}, err
	}

	contentTemplate := strings.TrimSpace(readStringAttr(node.Config, "content_template"))
	if contentTemplate == "" {
		contentTemplate = strings.TrimSpace(baseCfg.DeferredExpression)
	}

	iterCtx, err := bindDynamicIteratorContext(ctx, iteratorName, key, elemVal, node.Address)
	if err != nil {
		return workerspec.Config{}, err
	}

	instanceScope := fmt.Sprintf("%s[%s]", node.Address, key)
	if contentTemplate == "" {
		if err := validateMaterializedWorker(&baseCfg, instanceScope); err != nil {
			return workerspec.Config{}, err
		}
		if baseCfg.SourceURL == "" {
			baseCfg.SourceURL = resolveApplySourceURL(ctx, evalOpts, node, "")
		}
		return baseCfg, nil
	}

	workerCtx := attachWorkerLocals(iterCtx, instanceScope, baseCfg.Artifacts, baseCfg.Imports)
	evaluated, err := evalHCLObjectExpression(contentTemplate, workerCtx, instanceScope)
	if err != nil {
		return workerspec.Config{}, err
	}

	mat, err := decodeWorkerConfigFromDeferredObject(evaluated, baseCfg, instanceScope)
	if err != nil {
		return workerspec.Config{}, err
	}
	if err := validateMaterializedWorker(&mat, instanceScope); err != nil {
		return workerspec.Config{}, err
	}
	if mat.SourceURL == "" {
		mat.SourceURL = resolveApplySourceURL(ctx, evalOpts, node, "")
	}
	return mat, nil
}

func bindDynamicIteratorContext(
	ctx *hcl.EvalContext,
	iteratorName, key string,
	elemVal cty.Value,
	scope string,
) (*hcl.EvalContext, error) {
	child := ctx.NewChild()
	child.Variables = make(map[string]cty.Value, 1)

	if iteratorName == "worker" {
		workerAttrs := make(map[string]cty.Value)
		if ctx.Variables != nil {
			if existing, ok := ctx.Variables["worker"]; ok && existing != cty.NilVal &&
				!existing.IsNull() && existing.IsKnown() && existing.Type().IsObjectType() {
				if existing.Type().HasAttribute("key") || existing.Type().HasAttribute("value") {
					return nil, fmt.Errorf(`%s default iterator "worker" conflicts with static worker named "key" or "value"`, scope)
				}
				maps.Copy(workerAttrs, existing.AsValueMap())
			}
		}
		workerAttrs["key"] = cty.StringVal(key)
		workerAttrs["value"] = elemVal
		child.Variables["worker"] = cty.ObjectVal(workerAttrs)
		return child, nil
	}

	child.Variables[iteratorName] = cty.ObjectVal(map[string]cty.Value{
		"key":   cty.StringVal(key),
		"value": elemVal,
	})
	return child, nil
}

// EvalMergeBase evaluates a planned merge node's based_source, metadata, and sources.
// When based_source is wholly known while sources still contains Apply-time unknowns (such as
// pending sibling dynamic_worker instances), it returns SourcesKnown = false and Sources = nil
// without error so streaming RunActor dispatch can enqueue completed candidates immediately.
func EvalMergeBase(ctx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (MaterializedMerge, error) {
	if node.Kind != "merge" {
		return MaterializedMerge{}, fmt.Errorf("applyeval: expected merge node, got %q (%s)", node.Kind, node.Address)
	}
	if ctx == nil {
		return MaterializedMerge{}, fmt.Errorf("applyeval: eval context is nil")
	}
	evalOpts := resolveEvalOptions(nil, opts)
	evalCtx := withNodeEachContext(ctx, node)

	name := nodeSimpleName(node, readStringAttr(node.Config, "name"))

	basedVal, err := evalAttrOrFallback(
		evalCtx,
		readStringAttr(node.Config, "based_source_expr"),
		attrOrNil(node.Config, "based_source"),
		node.Address+".based_source",
	)
	if err != nil {
		return MaterializedMerge{}, err
	}
	basedSource, err := requireKnownNonBlankStringVal(basedVal, node.Address+" based_source")
	if err != nil {
		return MaterializedMerge{}, err
	}
	if err := corespec.ValidateBranchName(basedSource); err != nil {
		return MaterializedMerge{}, fmt.Errorf("%s based_source: %w", node.Address, err)
	}

	sourcesVal, err := evalAttrOrFallback(
		evalCtx,
		readStringAttr(node.Config, "sources_expr"),
		attrOrNil(node.Config, "sources"),
		node.Address+".sources",
	)
	if err != nil {
		return MaterializedMerge{}, err
	}
	sources, sourcesKnown, err := extractMergeSourcesSlice(sourcesVal, node.Address)
	if err != nil {
		return MaterializedMerge{}, err
	}

	var targetBranch *string
	if tbVal := attrOrNil(node.Config, "target_branch"); tbVal != cty.NilVal {
		unmarkedTB, _ := tbVal.UnmarkDeep()
		if !unmarkedTB.IsNull() && unmarkedTB.IsKnown() && unmarkedTB.Type().Equals(cty.String) {
			s := unmarkedTB.AsString()
			targetBranch = &s
		}
	}

	strategy := readStringAttr(node.Config, "strategy")
	if strategy == "" {
		strategy = "rebase"
	}
	maxReworks := 2
	if n, ok := readOptionalIntAttr(node.Config, "max_reworks"); ok {
		maxReworks = n
	}
	humanApproval := readStringAttr(node.Config, "human_approval")
	if humanApproval == "" {
		humanApproval = "optional"
	}
	sandboxVal := attrOrNil(node.Config, "sandbox")
	if sandboxVal == cty.NilVal {
		sandboxVal = cty.NullVal(cty.EmptyObject)
	}
	gatekeepers, err := decodeGatekeepersFromPlanVal(attrOrNil(node.Config, "gatekeepers"), node.Address)
	if err != nil {
		return MaterializedMerge{}, err
	}

	sourceURL := readStringAttr(node.Config, "source_url")
	if sourceURL == "" {
		sourceURL = resolveApplySourceURL(ctx, evalOpts, node, readStringAttr(node.Config, "sources_expr"))
	}

	return MaterializedMerge{
		Name:          name,
		BasedSource:   basedSource,
		SourceURL:     sourceURL,
		TargetBranch:  targetBranch,
		Sources:       sources,
		SourcesKnown:  sourcesKnown,
		Strategy:      strategy,
		MaxReworks:    maxReworks,
		HumanApproval: humanApproval,
		Sandbox:       sandboxVal,
		Gatekeepers:   gatekeepers,
	}, nil
}

// EvalReadyMerge evaluates a planned merge node and requires both based_source and sources
// to be wholly known during apply.
func EvalReadyMerge(ctx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (MaterializedMerge, error) {
	mat, err := EvalMergeBase(ctx, node, opts...)
	if err != nil {
		return MaterializedMerge{}, err
	}
	if !mat.SourcesKnown {
		return MaterializedMerge{}, fmt.Errorf("%s sources must be wholly known during apply", node.Address)
	}
	return mat, nil
}

func extractMergeSourcesSlice(val cty.Value, scope string) ([]string, bool, error) {
	if val == cty.NilVal {
		return nil, false, fmt.Errorf("%s sources must not be null", scope)
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, false, fmt.Errorf("%s sources must not be null", scope)
	}
	vt := unmarked.Type()
	if !unmarked.IsWhollyKnown() {
		if vt == cty.DynamicPseudoType || vt.IsListType() || vt.IsSetType() || vt.IsTupleType() {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s sources must be a list or set of strings, got %s", scope, vt.FriendlyName())
	}
	if !vt.IsListType() && !vt.IsSetType() && !vt.IsTupleType() {
		return nil, false, fmt.Errorf("%s sources must be a list or set of strings, got %s", scope, vt.FriendlyName())
	}
	out := make([]string, 0, unmarked.LengthInt())
	for it := unmarked.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if elem.IsNull() || !elem.Type().Equals(cty.String) {
			return nil, false, fmt.Errorf("%s sources elements must be non-null strings", scope)
		}
		branch := elem.AsString()
		if err := corespec.ValidateBranchName(branch); err != nil {
			return nil, false, fmt.Errorf("%s sources: %w", scope, err)
		}
		out = append(out, branch)
	}
	return out, true, nil
}

// EvalModuleInputsAndValidate evaluates deferred ModuleSpec.InputExprs against parentCtx,
// binds them into the child Plan's var context (preserving defaults & sensitivity marks),
// executes all deferred variable validations, and returns an updated immutable child Plan.
func EvalModuleInputsAndValidate(
	parentCtx *hcl.EvalContext,
	node plan.NodeSpec,
	opts ...EvalOption,
) (BoundModule, error) {
	if node.Kind != "module" || node.Module == nil || node.Module.Plan == nil {
		return BoundModule{}, fmt.Errorf("applyeval: node %q is not a valid module node", node.Address)
	}
	if parentCtx == nil {
		return BoundModule{}, fmt.Errorf("applyeval: parent eval context is nil")
	}

	mod := node.Module
	childPlan := mod.Plan
	evalOpts := resolveEvalOptions(childPlan, opts)
	evalCtx := withNodeEachContext(parentCtx, node)

	varMap := make(map[string]cty.Value)
	if existingVar, ok := childPlan.Context()["var"]; ok && existingVar != cty.NilVal {
		unmarked, _ := existingVar.UnmarkDeep()
		if !unmarked.IsNull() && unmarked.IsKnown() && (unmarked.Type().IsObjectType() || unmarked.Type().IsMapType()) {
			varMap = existingVar.AsValueMap()
		}
	}

	inputNames := make([]string, 0, len(mod.InputExprs)+len(mod.Inputs))
	seenInput := make(map[string]struct{}, len(mod.InputExprs)+len(mod.Inputs))
	for name := range mod.InputExprs {
		seenInput[name] = struct{}{}
		inputNames = append(inputNames, name)
	}
	for name := range mod.Inputs {
		if _, ok := seenInput[name]; !ok {
			seenInput[name] = struct{}{}
			inputNames = append(inputNames, name)
		}
	}
	sort.Strings(inputNames)

	boundInputs := make(map[string]cty.Value, len(inputNames))
	for _, name := range inputNames {
		planVal, hasPlanVal := mod.Inputs[name]
		childVar, hasChildVar := varMap[name]
		childWasSensitive := (hasChildVar && corespec.IsSensitive(childVar)) ||
			(hasPlanVal && corespec.IsSensitive(planVal))

		var evaluated cty.Value
		if hasPlanVal && planVal != cty.NilVal && planVal.IsWhollyKnown() {
			evaluated = planVal
		} else if exprStr, hasExpr := mod.InputExprs[name]; hasExpr && strings.TrimSpace(exprStr) != "" {
			expr, diags := hclsyntax.ParseExpression([]byte(exprStr), node.Address+"."+name, hcl.InitialPos)
			if diags.HasErrors() {
				return BoundModule{}, fmt.Errorf("module %s input %q: %w", node.Address, name, diags)
			}
			val, evalDiags := expr.Value(evalCtx)
			if evalDiags.HasErrors() {
				return BoundModule{}, fmt.Errorf("module %s input %q: %w", node.Address, name, evalDiags)
			}
			evaluated = val
		} else if hasPlanVal {
			evaluated = planVal
		} else {
			continue
		}

		if hasChildVar && childVar != cty.NilVal {
			converted, err := convertPreservingMarks(evaluated, childVar.Type())
			if err != nil {
				return BoundModule{}, fmt.Errorf("module %s input %q: %w", node.Address, name, err)
			}
			evaluated = converted
		}
		if childWasSensitive {
			evaluated = corespec.MarkSensitive(evaluated)
		}
		boundInputs[name] = evaluated
		varMap[name] = evaluated
	}

	varObject := cty.EmptyObjectVal
	if len(varMap) > 0 {
		varObject = cty.ObjectVal(varMap)
	}

	validations := mergeValidationMaps(mod.DeferredValidations, childPlan.DeferredValidations())
	if len(validations) > 0 {
		fns := parentCtx.Functions
		if len(fns) == 0 {
			fns = config.Functions(evalOpts.baseDir)
		}
		varNames := make([]string, 0, len(validations))
		for varName := range validations {
			varNames = append(varNames, varName)
		}
		sort.Strings(varNames)
		evalVars := map[string]cty.Value{"var": varObject}
		canonicalAddr := nodeCanonicalOrAddress(node)
		for _, varName := range varNames {
			for _, cond := range validations[varName] {
				ok, err := cond.Evaluate(evalVars, fns)
				if err != nil {
					return BoundModule{}, fmt.Errorf("invalid value for variable %s in %s: %w", varName, canonicalAddr, err)
				}
				if !ok {
					return BoundModule{}, fmt.Errorf("deferred validation for variable %s in %s did not resolve to a known boolean", varName, canonicalAddr)
				}
			}
		}
	}

	updatedCtx := childPlan.Context()
	if updatedCtx == nil {
		updatedCtx = make(map[string]cty.Value, 1)
	}
	updatedCtx["var"] = varObject

	updatedPlan, err := plan.New(plan.Args{
		Directory:           childPlan.Directory(),
		BundlePath:          childPlan.BundlePath(),
		Nodes:               childPlan.Nodes(),
		Outputs:             childPlan.Outputs(),
		Context:             updatedCtx,
		LocalExpressions:    childPlan.LocalExpressions(),
		DeferredValidations: childPlan.DeferredValidations(),
		Tools:               childPlan.Tools(),
		PluginLock:          childPlan.PluginLock(),
	})
	if err != nil {
		return BoundModule{}, fmt.Errorf("rebuild bound child plan for %s: %w", node.Address, err)
	}

	return BoundModule{
		Plan:      updatedPlan,
		Inputs:    boundInputs,
		VarObject: varObject,
	}, nil
}

func mergeValidationMaps(a, b map[string][]corespec.Condition) map[string][]corespec.Condition {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string][]corespec.Condition, len(a)+len(b))
	seen := make(map[string]map[corespec.Condition]struct{})
	addFrom := func(src map[string][]corespec.Condition) {
		for k, list := range src {
			if seen[k] == nil {
				seen[k] = make(map[corespec.Condition]struct{})
			}
			for _, cond := range list {
				if _, dup := seen[k][cond]; !dup {
					seen[k][cond] = struct{}{}
					out[k] = append(out[k], cond)
				}
			}
		}
	}
	addFrom(a)
	addFrom(b)
	return out
}

// EvalPlanOutputs evaluates all planned outputs against ctx, keeping already wholly-known
// Plan outputs and evaluating deferred Expression strings while preserving sensitivity marks.
func EvalPlanOutputs(planned *plan.Plan, ctx *hcl.EvalContext) (map[string]cty.Value, error) {
	if planned == nil {
		return nil, fmt.Errorf("applyeval: plan is nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("applyeval: eval context is nil")
	}

	plannedOutputs := planned.Outputs()
	names := make([]string, 0, len(plannedOutputs))
	for name := range plannedOutputs {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make(map[string]cty.Value, len(names))
	for _, name := range names {
		spec := plannedOutputs[name]
		var val cty.Value
		if spec.Value != cty.NilVal && spec.Value.IsWhollyKnown() {
			val = spec.Value
		} else {
			if strings.TrimSpace(spec.Expression) == "" {
				return nil, fmt.Errorf("output %q is unknown and has no saved expression", name)
			}
			expr, diags := hclsyntax.ParseExpression([]byte(spec.Expression), "output."+name, hcl.InitialPos)
			if diags.HasErrors() {
				return nil, fmt.Errorf("parse output.%s: %w", name, diags)
			}
			evaluated, evalDiags := expr.Value(ctx)
			if evalDiags.HasErrors() {
				return nil, fmt.Errorf("evaluate output.%s: %w", name, evalDiags)
			}
			val = evaluated
		}
		if spec.Sensitive && (spec.Value == cty.NilVal || spec.Value.HasMark(golden.SensitiveMark) || !corespec.IsSensitive(val)) {
			val = corespec.MarkSensitive(val)
		}
		results[name] = val
	}
	return results, nil
}

func withNodeEachContext(ctx *hcl.EvalContext, node plan.NodeSpec) *hcl.EvalContext {
	if ctx == nil || !hasAttr(node.Config, "each") {
		return ctx
	}
	eachVal := node.Config.GetAttr("each")
	unmarked, _ := eachVal.UnmarkDeep()
	if unmarked == cty.NilVal || unmarked.IsNull() || !unmarked.IsKnown() {
		return ctx
	}
	child := ctx.NewChild()
	child.Variables = map[string]cty.Value{
		"each": eachVal,
	}
	return child
}

func attachWorkerLocals(
	ctx *hcl.EvalContext,
	scope string,
	artifacts map[string]workerspec.Artifact,
	imports map[string]workerspec.ImportArtifact,
) *hcl.EvalContext {
	child := ctx.NewChild()
	child.Functions = maps.Clone(ctx.Functions)
	if child.Functions == nil {
		child.Functions = make(map[string]function.Function)
	}
	child.Functions["artifact"] = function.New(&function.Spec{
		Params: []function.Parameter{{Name: "name", Type: cty.String}},
		Type:   function.StaticReturnType(artifactValueType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			arg, _ := args[0].UnmarkDeep()
			if arg.IsNull() {
				return cty.NilVal, function.NewArgError(0, fmt.Errorf("artifact name must not be null"))
			}
			if !arg.IsKnown() {
				return cty.UnknownVal(artifactValueType), nil
			}
			name := strings.TrimSpace(arg.AsString())
			if name == "" {
				return cty.NilVal, function.NewArgError(0, fmt.Errorf("artifact name must not be empty"))
			}
			art, ok := artifacts[name]
			if !ok {
				return cty.NilVal, function.NewArgError(0, fmt.Errorf("artifact %q is not declared in %s", name, scope))
			}
			return cty.ObjectVal(map[string]cty.Value{
				"id":          cty.UnknownVal(cty.String),
				"name":        cty.StringVal(art.Name),
				"kind":        cty.StringVal("artifact"),
				"type":        cty.StringVal(art.Type),
				"path":        cty.StringVal(art.ContainerPath()),
				"rel_path":    cty.StringVal(art.Path),
				"description": cty.StringVal(art.Description),
				"required":    cty.BoolVal(art.Required),
				"non_empty":   cty.BoolVal(art.NonEmpty),
			}), nil
		},
	})

	importVars := make(map[string]cty.Value, len(imports))
	for name, imp := range imports {
		importVars[name] = cty.ObjectVal(map[string]cty.Value{
			"path": cty.StringVal(imp.ContainerPath()),
			"desc": cty.StringVal(imp.Desc),
		})
	}
	if len(importVars) == 0 {
		child.Variables = map[string]cty.Value{"import_artifact": cty.EmptyObjectVal}
	} else {
		child.Variables = map[string]cty.Value{"import_artifact": cty.ObjectVal(importVars)}
	}
	return child
}

func evalHCLObjectExpression(raw string, ctx *hcl.EvalContext, scope string) (cty.Value, error) {
	expr, diags := hclsyntax.ParseExpression([]byte(raw), scope+".deferred", hcl.InitialPos)
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("parse deferred expression for %s: %w", scope, diags)
	}
	val, evalDiags := expr.Value(ctx)
	if evalDiags.HasErrors() {
		return cty.NilVal, fmt.Errorf("evaluate deferred expression for %s: %w", scope, evalDiags)
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || (!unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType()) {
		return cty.NilVal, fmt.Errorf("deferred expression for %s must evaluate to a known object", scope)
	}
	return val, nil
}

func evalAttrOrFallback(ctx *hcl.EvalContext, rawExpr string, fallback cty.Value, scope string) (cty.Value, error) {
	if strings.TrimSpace(rawExpr) != "" {
		expr, diags := hclsyntax.ParseExpression([]byte(rawExpr), scope, hcl.InitialPos)
		if diags.HasErrors() {
			return cty.NilVal, fmt.Errorf("parse %s: %w", scope, diags)
		}
		val, evalDiags := expr.Value(ctx)
		if evalDiags.HasErrors() {
			return cty.NilVal, fmt.Errorf("evaluate %s: %w", scope, evalDiags)
		}
		return val, nil
	}
	if fallback != cty.NilVal {
		return fallback, nil
	}
	return cty.NilVal, fmt.Errorf("%s is missing", scope)
}

func decodeWorkerConfigFromPlanVal(val cty.Value, scope string) (workerspec.Config, error) {
	if !hasAttr(val, "based_source") && !hasAttr(val, "model") {
		return workerspec.Config{}, fmt.Errorf("%s worker config is missing or invalid", scope)
	}
	cfg := workerspec.Config{
		BasedSource:         attrOrNil(val, "based_source"),
		SourceURL:           readStringAttr(val, "source_url"),
		Sandbox:             defaultNullObject(attrOrNil(val, "sandbox")),
		Harness:             defaultNullObject(attrOrNil(val, "harness")),
		ModelProvider:       defaultNullObject(attrOrNil(val, "model_provider")),
		Model:               readStringAttr(val, "model"),
		Profile:             readStringAttr(val, "profile"),
		ReasoningEffort:     readStringAttr(val, "reasoning_effort"),
		SystemPrompt:        attrOrNil(val, "system_prompt"),
		Prompt:              optionalPromptValue(attrOrNil(val, "prompt")),
		WritablePaths:       readStringSliceAttr(val, "writable_paths"),
		HumanApproval:       defaultNonEmpty(readStringAttr(val, "human_approval"), "optional"),
		CheckpointRounds:    workerspec.DefaultCheckpointRounds,
		Permission:          defaultNonEmpty(readStringAttr(val, "permission"), workerspec.DefaultPermission),
		MaxProtocolAttempts: workerspec.DefaultMaxProtocolAttempts,
		TimeoutRaw:          readStringAttr(val, "timeout_raw"),
		Retry:               decodeRetryOverrideFromPlanConfig(attrOrNil(val, "retry")),
		ToolIDs:             readStringSliceAttr(val, "tool_ids"),
		MCPToolIDs:          readStringSliceAttr(val, "mcp_tool_ids"),
		MCPResourceIDs:      readStringSliceAttr(val, "mcp_resource_ids"),
		TerminateToolID:     readStringAttr(val, "terminate_tool_id"),
		ToolCallQuota:       readOptionalIntMap(attrOrNil(val, "tool_call_quota")),
		AllowedTools:        readOptionalStringSlice(attrOrNil(val, "allowed_tools")),
		DisallowedTools:     readOptionalStringSlice(attrOrNil(val, "disallowed_tools")),
		SkillDirectories:    readStringSliceAttr(val, "skill_directories"),
		Skills:              readStringSliceAttr(val, "skills"),
		DisabledSkills:      readStringSliceAttr(val, "disabled_skills"),
		Artifacts:           extractPlannedArtifacts(val),
		DeferredExpression:  readStringAttr(val, "deferred_expression"),
	}
	if n, ok := readOptionalIntAttr(val, "checkpoint_every_rounds"); ok {
		cfg.CheckpointRounds = n
	}
	if n, ok := readOptionalIntAttr(val, "max_protocol_attempts"); ok {
		cfg.MaxProtocolAttempts = n
	}
	if ns, ok := readOptionalInt64Attr(val, "timeout_nanoseconds"); ok && ns > 0 {
		d := time.Duration(ns)
		cfg.Timeout = &d
	}

	var err error
	if cfg.ToolUses, err = decodeToolUsesFromPlanList(attrOrNil(val, "tool_uses")); err != nil {
		return workerspec.Config{}, fmt.Errorf("%s: %w", scope, err)
	}
	if cfg.Imports, err = decodeImportsFromPlanMap(attrOrNil(val, "imports"), false); err != nil {
		return workerspec.Config{}, fmt.Errorf("%s: %w", scope, err)
	}
	if cfg.QC, err = decodeQCFromPlanVal(attrOrNil(val, "qc"), nil, scope); err != nil {
		return workerspec.Config{}, err
	}
	if cfg.Fallbacks, err = decodeFallbacksFromPlanList(attrOrNil(val, "fallbacks")); err != nil {
		return workerspec.Config{}, fmt.Errorf("%s: %w", scope, err)
	}
	return cfg, nil
}

func decodeWorkerConfigFromDeferredObject(val cty.Value, planned workerspec.Config, scope string) (workerspec.Config, error) {
	cfg := planned.Clone()

	cfg.BasedSource = attrOrNil(val, "based_source")
	cfg.Sandbox = defaultNullObject(attrOrNil(val, "sandbox"))
	cfg.Harness = defaultNullObject(attrOrNil(val, "harness"))
	cfg.ModelProvider = defaultNullObject(attrOrNil(val, "model_provider"))
	cfg.SystemPrompt = attrOrNil(val, "system_prompt")
	cfg.Prompt = optionalPromptValue(attrOrNil(val, "prompt"))

	if m, set, err := optionalKnownStringVal(attrOrNil(val, "model"), scope+" model"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.Model = m
	}
	if p, set, err := optionalKnownStringVal(attrOrNil(val, "profile"), scope+" profile"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.Profile = p
	}
	if e, set, err := optionalKnownStringVal(attrOrNil(val, "reasoning_effort"), scope+" reasoning_effort"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.ReasoningEffort = e
	}
	if wp, err := decodeOptionalStringSliceVal(attrOrNil(val, "writable_paths"), scope+" writable_paths"); err != nil {
		return workerspec.Config{}, err
	} else if wp != nil {
		cfg.WritablePaths = wp
	}
	if ha, set, err := optionalKnownStringVal(attrOrNil(val, "human_approval"), scope+" human_approval"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.HumanApproval = ha
	}
	if cr, set, err := optionalWholeIntVal(attrOrNil(val, "checkpoint_every_rounds"), scope+" checkpoint_every_rounds"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.CheckpointRounds = cr
	}
	if perm, set, err := optionalKnownStringVal(attrOrNil(val, "permission"), scope+" permission"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.Permission = perm
	}
	if mpa, set, err := optionalWholeIntVal(attrOrNil(val, "max_protocol_attempts"), scope+" max_protocol_attempts"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.MaxProtocolAttempts = mpa
	}
	if rawTimeout, set, err := optionalKnownStringVal(attrOrNil(val, "timeout"), scope+" timeout"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		d, parseErr := time.ParseDuration(rawTimeout)
		if parseErr != nil || d <= 0 {
			return workerspec.Config{}, fmt.Errorf("%s timeout must be a positive duration", scope)
		}
		cfg.Timeout = &d
		cfg.TimeoutRaw = rawTimeout
	}

	for _, pair := range []struct {
		attr string
		dst  *[]string
	}{
		{"tool_ids", &cfg.ToolIDs},
		{"mcp_tool_ids", &cfg.MCPToolIDs},
		{"mcp_resource_ids", &cfg.MCPResourceIDs},
		{"skill_directories", &cfg.SkillDirectories},
		{"skills", &cfg.Skills},
		{"disabled_skills", &cfg.DisabledSkills},
	} {
		if s, err := decodeOptionalStringSliceVal(attrOrNil(val, pair.attr), scope+" "+pair.attr); err != nil {
			return workerspec.Config{}, err
		} else if s != nil {
			*pair.dst = s
		}
	}
	if term, set, err := optionalKnownStringVal(attrOrNil(val, "terminate_tool_id"), scope+" terminate_tool_id"); err != nil {
		return workerspec.Config{}, err
	} else if set {
		cfg.TerminateToolID = term
	}
	if hasAttr(val, "tool_call_quota") {
		if quota, err := decodeIntMapVal(val.GetAttr("tool_call_quota"), scope+" tool_call_quota"); err != nil {
			return workerspec.Config{}, err
		} else if quota != nil {
			cfg.ToolCallQuota = quota
		}
	}
	if hasAttr(val, "allowed_tools") {
		if s, err := decodeOptionalStringSliceVal(val.GetAttr("allowed_tools"), scope+" allowed_tools"); err != nil {
			return workerspec.Config{}, err
		} else {
			cfg.AllowedTools = s
		}
	}
	if hasAttr(val, "disallowed_tools") {
		if s, err := decodeOptionalStringSliceVal(val.GetAttr("disallowed_tools"), scope+" disallowed_tools"); err != nil {
			return workerspec.Config{}, err
		} else {
			cfg.DisallowedTools = s
		}
	}

	if hasAttr(val, "retry") {
		retryVal := val.GetAttr("retry")
		unmarkedRetry, _ := retryVal.UnmarkDeep()
		if !unmarkedRetry.IsNull() {
			cfg.Retry = decodeRetryOverrideFromPlanConfig(retryVal)
		}
	}
	if hasAttr(val, "artifact") {
		if arts := extractPlannedArtifacts(cty.ObjectVal(map[string]cty.Value{"artifacts": val.GetAttr("artifact")})); len(arts) > 0 {
			cfg.Artifacts = arts
		}
	}
	if hasAttr(val, "import_artifact") {
		imports, err := decodeImportsFromPlanMap(val.GetAttr("import_artifact"), true)
		if err != nil {
			return workerspec.Config{}, fmt.Errorf("%s: %w", scope, err)
		}
		// Preserve planned relative ImportArtifact.Path when available.
		for name, imp := range imports {
			if plannedImp, ok := planned.Imports[name]; ok && plannedImp.Path != "" {
				imp.Path = plannedImp.Path
			}
			imports[name] = imp
		}
		cfg.Imports = imports
	}
	if hasAttr(val, "tool_use") {
		uses, err := decodeToolUsesFromDeferredMap(val.GetAttr("tool_use"), planned.ToolUses)
		if err != nil {
			return workerspec.Config{}, fmt.Errorf("%s: %w", scope, err)
		}
		cfg.ToolUses = uses
	}
	if hasAttr(val, "qc") {
		qcVal := val.GetAttr("qc")
		unmarkedQC, _ := qcVal.UnmarkDeep()
		if unmarkedQC.IsNull() {
			cfg.QC = nil
		} else {
			qc, err := decodeQCFromPlanVal(qcVal, planned.QC, scope)
			if err != nil {
				return workerspec.Config{}, err
			}
			cfg.QC = qc
		}
	}
	if hasAttr(val, "fallback") {
		fbs, err := decodeFallbacksFromDeferredMap(val.GetAttr("fallback"), planned.Fallbacks)
		if err != nil {
			return workerspec.Config{}, fmt.Errorf("%s: %w", scope, err)
		}
		cfg.Fallbacks = fbs
	}
	return cfg, nil
}

func validateMaterializedWorker(cfg *workerspec.Config, scope string) error {
	basedStr, err := requireKnownNonBlankStringVal(cfg.BasedSource, scope+" based_source")
	if err != nil {
		return err
	}
	if err := corespec.ValidateBranchName(basedStr); err != nil {
		return fmt.Errorf("%s based_source: %w", scope, err)
	}
	if err := validateKnownBlockRef(cfg.Sandbox, "sandbox", scope+" sandbox"); err != nil {
		return err
	}
	if err := validateKnownBlockRef(cfg.Harness, "harness", scope+" harness"); err != nil {
		return err
	}
	unmarkedMP, _ := cfg.ModelProvider.UnmarkDeep()
	if cfg.ModelProvider != cty.NilVal && !unmarkedMP.IsNull() {
		if err := validateKnownBlockRef(cfg.ModelProvider, "model_provider", scope+" model_provider"); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("%s model must not be empty", scope)
	}
	if _, err := requireKnownNonBlankStringVal(cfg.SystemPrompt, scope+" system_prompt"); err != nil {
		return err
	}
	if cfg.Prompt != cty.NilVal {
		unmarkedPrompt, _ := cfg.Prompt.UnmarkDeep()
		if !unmarkedPrompt.IsNull() {
			if !unmarkedPrompt.IsWhollyKnown() || !unmarkedPrompt.Type().Equals(cty.String) {
				return fmt.Errorf("%s prompt must be a known string", scope)
			}
		}
	}
	for name, imp := range cfg.Imports {
		if imp.Sources == cty.NilVal {
			return fmt.Errorf("%s import_artifact %q sources must not be null", scope, name)
		}
		unmarkedSrc, _ := imp.Sources.UnmarkDeep()
		if unmarkedSrc.IsNull() || !unmarkedSrc.IsWhollyKnown() || (!unmarkedSrc.Type().IsListType() && !unmarkedSrc.Type().IsTupleType()) {
			return fmt.Errorf("%s import_artifact %q sources must be a wholly known list of artifact objects", scope, name)
		}
		for it := unmarkedSrc.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			if elem.IsNull() || !elem.Type().IsObjectType() || readStringAttr(elem, "kind") != "artifact" {
				return fmt.Errorf("%s import_artifact %q sources must contain artifact objects", scope, name)
			}
		}
	}
	if cfg.QC != nil {
		if cfg.QC.MaxGatekeeperReworks < 0 {
			return fmt.Errorf("%s qc max_gatekeeper_reworks must be non-negative", scope)
		}
		for _, gk := range cfg.QC.Gatekeepers {
			if err := workerspec.ValidateGatekeeper(gk); err != nil {
				return fmt.Errorf("%s qc: %w", scope, err)
			}
		}
		hasCriteria := false
		if cfg.QC.Criteria != cty.NilVal {
			unmarkedCrit, _ := cfg.QC.Criteria.UnmarkDeep()
			if !unmarkedCrit.IsNull() {
				if !unmarkedCrit.IsWhollyKnown() || (!unmarkedCrit.Type().IsMapType() && !unmarkedCrit.Type().IsObjectType()) {
					return fmt.Errorf("%s qc criteria must be a known map(string)", scope)
				}
				if unmarkedCrit.LengthInt() > 0 {
					for it := unmarkedCrit.ElementIterator(); it.Next(); {
						k, v := it.Element()
						if strings.TrimSpace(k.AsString()) == "" || v.IsNull() || !v.Type().Equals(cty.String) || strings.TrimSpace(v.AsString()) == "" {
							return fmt.Errorf("%s qc criteria keys and values must be non-empty strings", scope)
						}
					}
					hasCriteria = true
				}
			}
		}
		if !hasCriteria && len(cfg.QC.Gatekeepers) == 0 {
			return fmt.Errorf("%s qc must declare at least one gatekeeper or non-empty criteria", scope)
		}
	}
	return nil
}

func validateKnownBlockRef(val cty.Value, expectedKind, scope string) error {
	if val == cty.NilVal {
		return fmt.Errorf("%s is required", scope)
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return fmt.Errorf("%s must not be null", scope)
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().IsObjectType() {
		return fmt.Errorf("%s must be a wholly known %s block reference", scope, expectedKind)
	}
	if readStringAttr(unmarked, "kind") != expectedKind || strings.TrimSpace(readStringAttr(unmarked, "address")) == "" {
		return fmt.Errorf("%s must reference a %s block", scope, expectedKind)
	}
	return nil
}

func decodeToolUsesFromPlanList(val cty.Value) ([]workerspec.ToolUse, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.CanIterateElements() {
		return nil, nil
	}
	out := make([]workerspec.ToolUse, 0, unmarked.LengthInt())
	for it := val.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		input := attrOrNil(elem, "input")
		if input == cty.NilVal {
			input = cty.EmptyObjectVal
		}
		out = append(out, workerspec.ToolUse{
			Name:      readStringAttr(elem, "name"),
			ToolID:    readStringAttr(elem, "tool_id"),
			Terminate: readBoolAttr(elem, "terminate", false),
			Input:     input,
		})
	}
	return out, nil
}

func decodeToolUsesFromDeferredMap(val cty.Value, plannedOrder []workerspec.ToolUse) ([]workerspec.ToolUse, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || (!unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType()) {
		return nil, nil
	}
	byName := make(map[string]workerspec.ToolUse, unmarked.LengthInt())
	for name := range unmarked.AsValueMap() {
		elem := getMapOrAttr(val, name)
		input := attrOrNil(elem, "input")
		if input == cty.NilVal {
			input = cty.EmptyObjectVal
		}
		byName[name] = workerspec.ToolUse{
			Name:      name,
			ToolID:    readStringAttr(elem, "tool_id"),
			Terminate: readBoolAttr(elem, "terminate", false),
			Input:     input,
		}
	}
	return orderByPlannedNames(byName, plannedOrder, func(u workerspec.ToolUse) string { return u.Name }), nil
}

func decodeImportsFromPlanMap(val cty.Value, stripContainerPrefix bool) (map[string]workerspec.ImportArtifact, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || (!unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType()) {
		return nil, nil
	}
	if unmarked.LengthInt() == 0 {
		return nil, nil
	}
	out := make(map[string]workerspec.ImportArtifact, unmarked.LengthInt())
	for name := range unmarked.AsValueMap() {
		elem := getMapOrAttr(val, name)
		relPath := readStringAttr(elem, "path")
		if stripContainerPrefix {
			prefix := workerspec.ContainerImportsRoot + "/"
			if strings.HasPrefix(relPath, prefix) {
				relPath = strings.TrimPrefix(relPath, prefix)
			}
			if relPath == "" {
				relPath = name
			}
		} else if relPath == "" {
			relPath = name
		}
		impName := readStringAttr(elem, "name")
		if impName == "" {
			impName = name
		}
		srcs := attrOrNil(elem, "sources")
		if srcs == cty.NilVal {
			srcs = cty.EmptyTupleVal
		}
		out[name] = workerspec.ImportArtifact{
			Name:    impName,
			Desc:    readStringAttr(elem, "desc"),
			Path:    path.Clean(relPath),
			Sources: srcs,
		}
	}
	return out, nil
}

func decodeGatekeepersFromPlanVal(val cty.Value, scope string) ([]workerspec.GatekeeperConfig, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() {
		return nil, nil
	}
	decodeOne := func(name string, elem cty.Value) (workerspec.GatekeeperConfig, error) {
		if n := readStringAttr(elem, "name"); n != "" {
			name = n
		}
		cmd, err := decodeOptionalStringSliceVal(attrOrNil(elem, "command"), scope+" gatekeeper "+name+" command")
		if err != nil {
			return workerspec.GatekeeperConfig{}, err
		}
		expectCode := 0
		if code, ok := readOptionalIntAttr(elem, "expect_exit_code"); ok {
			expectCode = code
		}
		return workerspec.GatekeeperConfig{
			Name:             name,
			Command:          cmd,
			WorkingDir:       defaultNonEmpty(readStringAttr(elem, "working_dir"), "."),
			Timeout:          defaultNonEmpty(readStringAttr(elem, "timeout"), "10m"),
			ExpectExitCode:   expectCode,
			RequireCleanTree: readBoolAttr(elem, "require_clean_worktree", true),
		}, nil
	}
	switch {
	case unmarked.Type().IsListType() || unmarked.Type().IsTupleType():
		if unmarked.LengthInt() == 0 {
			return nil, nil
		}
		out := make([]workerspec.GatekeeperConfig, 0, unmarked.LengthInt())
		for it := val.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			gk, err := decodeOne("", elem)
			if err != nil {
				return nil, err
			}
			out = append(out, gk)
		}
		return out, nil
	case unmarked.Type().IsObjectType() || unmarked.Type().IsMapType():
		names := make([]string, 0, unmarked.LengthInt())
		for name := range unmarked.AsValueMap() {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]workerspec.GatekeeperConfig, 0, len(names))
		for _, name := range names {
			gk, err := decodeOne(name, getMapOrAttr(val, name))
			if err != nil {
				return nil, err
			}
			out = append(out, gk)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s gatekeepers must be a list or object", scope)
	}
}

func decodeQCFromPlanVal(val cty.Value, plannedQC *workerspec.QCConfig, scope string) (*workerspec.QCConfig, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || (!unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType()) {
		return nil, nil
	}
	maxRounds := workerspec.DefaultMaxQCRounds
	if n, ok := readOptionalIntAttr(val, "max_qc_rounds"); ok {
		maxRounds = n
	}
	maxGKReworks := workerspec.DefaultMaxGatekeeperReworks
	maxGKReworksSet := false
	if hasAttr(val, "max_gatekeeper_reworks") {
		n, set, err := optionalWholeIntVal(val.GetAttr("max_gatekeeper_reworks"), scope+" qc max_gatekeeper_reworks")
		if err != nil {
			return nil, err
		}
		if set {
			if n < 0 {
				return nil, fmt.Errorf("%s qc max_gatekeeper_reworks must be non-negative", scope)
			}
			maxGKReworks = n
			maxGKReworksSet = readBoolAttr(val, "max_gatekeeper_reworks_set", true)
		}
	}
	var gatekeepers []workerspec.GatekeeperConfig
	var err error
	if hasAttr(val, "gatekeepers") {
		gatekeepers, err = decodeGatekeepersFromPlanVal(val.GetAttr("gatekeepers"), scope+" qc")
		if err != nil {
			return nil, err
		}
	} else if hasAttr(val, "gatekeeper") {
		gatekeepers, err = decodeGatekeepersFromPlanVal(val.GetAttr("gatekeeper"), scope+" qc")
		if err != nil {
			return nil, err
		}
		if plannedQC != nil && len(plannedQC.Gatekeepers) > 0 && len(gatekeepers) > 0 {
			byName := make(map[string]workerspec.GatekeeperConfig, len(gatekeepers))
			for _, gk := range gatekeepers {
				byName[gk.Name] = gk
			}
			gatekeepers = orderByPlannedNames(byName, plannedQC.Gatekeepers, func(g workerspec.GatekeeperConfig) string { return g.Name })
		}
	}
	criteria := attrOrNil(val, "criteria")
	if criteria == cty.NilVal || criteria.IsNull() {
		criteria = cty.MapValEmpty(cty.String)
	}
	mp := defaultNullObject(attrOrNil(val, "model_provider"))
	mpSet := readBoolAttr(val, "model_provider_set", hasAttr(val, "model_provider"))
	return &workerspec.QCConfig{
		Criteria:                criteria,
		Sandbox:                 defaultNullObject(attrOrNil(val, "sandbox")),
		Harness:                 defaultNullObject(attrOrNil(val, "harness")),
		ModelProvider:           mp,
		ModelProviderSet:        mpSet,
		Model:                   readStringAttr(val, "model"),
		Profile:                 readStringAttr(val, "profile"),
		ReasoningEffort:         readStringAttr(val, "reasoning_effort"),
		Permission:              readStringAttr(val, "permission"),
		MaxQCRounds:             maxRounds,
		MaxGatekeeperReworks:    maxGKReworks,
		MaxGatekeeperReworksSet: maxGKReworksSet,
		Gatekeepers:             gatekeepers,
		ToolIDs:                 readStringSliceAttr(val, "tool_ids"),
		ToolCallQuota:           readOptionalIntMap(attrOrNil(val, "tool_call_quota")),
		AllowedTools:            readOptionalStringSlice(attrOrNil(val, "allowed_tools")),
		DisallowedTools:         readOptionalStringSlice(attrOrNil(val, "disallowed_tools")),
		SkillDirectories:        readStringSliceAttr(val, "skill_directories"),
		Skills:                  readStringSliceAttr(val, "skills"),
		DisabledSkills:          readStringSliceAttr(val, "disabled_skills"),
		Retry:                   decodeRetryOverrideFromPlanConfig(attrOrNil(val, "retry")),
	}, nil
}

func decodeFallbacksFromPlanList(val cty.Value) ([]workerspec.FallbackConfig, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.CanIterateElements() || unmarked.LengthInt() == 0 {
		return nil, nil
	}
	out := make([]workerspec.FallbackConfig, 0, unmarked.LengthInt())
	for it := val.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		maxReworks := 2
		if n, ok := readOptionalIntAttr(elem, "max_reworks"); ok {
			maxReworks = n
		}
		out = append(out, workerspec.FallbackConfig{
			Name:       readStringAttr(elem, "name"),
			On:         readStringSliceAttr(elem, "on"),
			Target:     readStringAttr(elem, "target"),
			MaxReworks: maxReworks,
			Artifacts:  readStringSliceAttr(elem, "artifacts"),
		})
	}
	return out, nil
}

func decodeFallbacksFromDeferredMap(val cty.Value, plannedOrder []workerspec.FallbackConfig) ([]workerspec.FallbackConfig, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || (!unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType()) {
		return nil, nil
	}
	byName := make(map[string]workerspec.FallbackConfig, unmarked.LengthInt())
	for name := range unmarked.AsValueMap() {
		elem := getMapOrAttr(val, name)
		maxReworks := 2
		if n, ok := readOptionalIntAttr(elem, "max_reworks"); ok {
			maxReworks = n
		}
		var artNames []string
		if artsVal := attrOrNil(elem, "artifacts"); artsVal != cty.NilVal {
			unmarkedArts, _ := artsVal.UnmarkDeep()
			if !unmarkedArts.IsNull() && unmarkedArts.IsKnown() && unmarkedArts.CanIterateElements() {
				for it := unmarkedArts.ElementIterator(); it.Next(); {
					_, artElem := it.Element()
					if artElem.Type().Equals(cty.String) {
						artNames = append(artNames, artElem.AsString())
					} else if artName := readStringAttr(artElem, "name"); artName != "" {
						artNames = append(artNames, artName)
					}
				}
			}
		}
		byName[name] = workerspec.FallbackConfig{
			Name:       name,
			On:         readStringSliceAttr(elem, "on"),
			Target:     readStringAttr(elem, "target"),
			MaxReworks: maxReworks,
			Artifacts:  artNames,
		}
	}
	return orderByPlannedNames(byName, plannedOrder, func(f workerspec.FallbackConfig) string { return f.Name }), nil
}

func orderByPlannedNames[T any](byName map[string]T, planned []T, keyOf func(T) string) []T {
	if len(byName) == 0 {
		return nil
	}
	out := make([]T, 0, len(byName))
	used := make(map[string]bool, len(byName))
	for _, item := range planned {
		k := keyOf(item)
		if updated, ok := byName[k]; ok {
			out = append(out, updated)
			used[k] = true
		}
	}
	var extra []string
	for k := range byName {
		if !used[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		out = append(out, byName[k])
	}
	return out
}

func resolveApplySourceURL(ctx *hcl.EvalContext, opts evalOptions, node plan.NodeSpec, extraExpr string) string {
	plansToSearch := make([]*plan.Plan, 0, 1+len(opts.parentPlans))
	if opts.plan != nil {
		plansToSearch = append(plansToSearch, opts.plan)
	}
	for i := len(opts.parentPlans) - 1; i >= 0; i-- {
		plansToSearch = append(plansToSearch, opts.parentPlans[i])
	}

	// Inspect expressions if available.
	var exprs []string
	if bsExpr := readStringAttr(node.Config, "based_source_expr"); bsExpr != "" {
		exprs = append(exprs, bsExpr)
	}
	if extraExpr != "" {
		exprs = append(exprs, extraExpr)
	}
	if opts.plan != nil && len(exprs) > 0 {
		if u := resolveSourceURLFromExprs(opts.plan, exprs); u != "" {
			return u
		}
	}

	// Check upstream dependency nodes in current plan.
	if opts.plan != nil {
		byAddr := make(map[string]plan.NodeSpec, len(opts.plan.Nodes()))
		for _, n := range opts.plan.Nodes() {
			byAddr[n.Address] = n
		}
		for _, dep := range node.Dependencies {
			if depNode, ok := byAddr[dep]; ok {
				switch depNode.Kind {
				case "source":
					if u := readStringAttr(depNode.Config, "url"); u != "" {
						return u
					}
				case "worker", "merge":
					if u := readStringAttr(depNode.Config, "source_url"); u != "" {
						return u
					}
				case "dynamic_worker":
					if hasAttr(depNode.Config, "content") {
						if u := readStringAttr(depNode.Config.GetAttr("content"), "source_url"); u != "" {
							return u
						}
					}
				}
			}
		}
	}

	// Fall back to single source in plan or ancestor parent plans.
	for _, p := range plansToSearch {
		var sourceURLs []string
		for _, n := range p.Nodes() {
			if n.Kind == "source" {
				if u := readStringAttr(n.Config, "url"); u != "" {
					sourceURLs = append(sourceURLs, u)
				}
			}
		}
		if len(sourceURLs) == 1 {
			return sourceURLs[0]
		}
		if len(sourceURLs) > 1 {
			break
		}
	}

	// Final fallback: single source in ctx.Variables["source"].
	if ctx != nil && ctx.Variables != nil {
		if srcObj, ok := ctx.Variables["source"]; ok && srcObj != cty.NilVal {
			unmarked, _ := srcObj.UnmarkDeep()
			if unmarked.IsKnown() && !unmarked.IsNull() && unmarked.Type().IsObjectType() && unmarked.LengthInt() == 1 {
				for _, entry := range unmarked.AsValueMap() {
					if u := readStringAttr(entry, "url"); u != "" {
						return u
					}
				}
			}
		}
	}
	return ""
}

func resolveSourceURLFromExprs(planned *plan.Plan, exprs []string) string {
	byAddr := make(map[string]plan.NodeSpec, len(planned.Nodes()))
	for _, n := range planned.Nodes() {
		byAddr[n.Address] = n
	}
	localExprs := planned.LocalExpressions()
	visitedLocals := make(map[string]bool)
	urls := make(map[string]struct{})

	var inspect func(hcl.Expression)
	inspect = func(expr hcl.Expression) {
		if expr == nil {
			return
		}
		for _, tr := range expr.Variables() {
			root := tr.RootName()
			switch root {
			case "local":
				if len(tr) >= 2 {
					if attr, ok := tr[1].(hcl.TraverseAttr); ok && !visitedLocals[attr.Name] {
						visitedLocals[attr.Name] = true
						if raw, ok := localExprs[attr.Name]; ok {
							if parsed, diags := hclsyntax.ParseExpression([]byte(raw), "local."+attr.Name, hcl.InitialPos); !diags.HasErrors() {
								inspect(parsed)
							}
						}
					}
				}
			case "source":
				if len(tr) >= 2 {
					if attr, ok := tr[1].(hcl.TraverseAttr); ok {
						if n, ok := byAddr["source."+attr.Name]; ok {
							if u := readStringAttr(n.Config, "url"); u != "" {
								urls[u] = struct{}{}
							}
						}
					}
				}
			case "worker", "dynamic_worker", "merge":
				if len(tr) >= 2 {
					if attr, ok := tr[1].(hcl.TraverseAttr); ok {
						addr := root + "." + attr.Name
						if n, ok := byAddr[addr]; ok {
							if u := readStringAttr(n.Config, "source_url"); u != "" {
								urls[u] = struct{}{}
							} else if n.Kind == "dynamic_worker" && hasAttr(n.Config, "content") {
								if u := readStringAttr(n.Config.GetAttr("content"), "source_url"); u != "" {
									urls[u] = struct{}{}
								}
							}
						}
					}
				}
			}
		}
	}

	for _, raw := range exprs {
		if parsed, diags := hclsyntax.ParseExpression([]byte(raw), "source-url", hcl.InitialPos); !diags.HasErrors() {
			inspect(parsed)
		}
	}
	if len(urls) == 1 {
		for u := range urls {
			return u
		}
	}
	return ""
}

func attrOrNil(obj cty.Value, attr string) cty.Value {
	if !hasAttr(obj, attr) {
		return cty.NilVal
	}
	return obj.GetAttr(attr)
}

func getMapOrAttr(obj cty.Value, key string) cty.Value {
	unmarked, _ := obj.UnmarkDeep()
	if unmarked.Type().IsObjectType() {
		return obj.GetAttr(key)
	}
	return obj.Index(cty.StringVal(key))
}

func defaultNullObject(v cty.Value) cty.Value {
	if v == cty.NilVal {
		return cty.NullVal(cty.EmptyObject)
	}
	return v
}

func optionalPromptValue(v cty.Value) cty.Value {
	if v == cty.NilVal {
		return cty.NilVal
	}
	unmarked, _ := v.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NilVal
	}
	return v
}

func defaultNonEmpty(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

func requireKnownNonBlankStringVal(val cty.Value, scope string) (string, error) {
	if val == cty.NilVal {
		return "", fmt.Errorf("%s is required", scope)
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return "", fmt.Errorf("%s must not be null", scope)
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", fmt.Errorf("%s must be a wholly known string", scope)
	}
	s := unmarked.AsString()
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s must not be empty", scope)
	}
	return s, nil
}

func optionalKnownStringVal(val cty.Value, scope string) (string, bool, error) {
	if val == cty.NilVal {
		return "", false, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return "", false, nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", false, fmt.Errorf("%s must be a known string", scope)
	}
	return unmarked.AsString(), true, nil
}

func optionalWholeIntVal(val cty.Value, scope string) (int, bool, error) {
	if val == cty.NilVal {
		return 0, false, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return 0, false, nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.Number) {
		return 0, false, fmt.Errorf("%s must be an integer", scope)
	}
	bf := unmarked.AsBigFloat()
	if bf.IsInf() || !bf.IsInt() {
		return 0, false, fmt.Errorf("%s must be a finite whole integer", scope)
	}
	i64, acc := bf.Int64()
	if acc != big.Exact || i64 < math.MinInt || i64 > math.MaxInt {
		return 0, false, fmt.Errorf("%s integer out of range", scope)
	}
	return int(i64), true, nil
}

func decodeOptionalStringSliceVal(val cty.Value, scope string) ([]string, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() || (!unmarked.Type().IsListType() && !unmarked.Type().IsTupleType() && !unmarked.Type().IsSetType()) {
		return nil, fmt.Errorf("%s must be a list of strings", scope)
	}
	out := make([]string, 0, unmarked.LengthInt())
	for it := unmarked.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if elem.IsNull() || !elem.Type().Equals(cty.String) {
			return nil, fmt.Errorf("%s must contain non-null strings", scope)
		}
		out = append(out, elem.AsString())
	}
	return out, nil
}

func decodeIntMapVal(val cty.Value, scope string) (map[string]int, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() || (!unmarked.Type().IsMapType() && !unmarked.Type().IsObjectType()) {
		return nil, fmt.Errorf("%s must be a map of integers", scope)
	}
	out := make(map[string]int, unmarked.LengthInt())
	for it := unmarked.ElementIterator(); it.Next(); {
		k, elem := it.Element()
		n, set, err := optionalWholeIntVal(elem, fmt.Sprintf("%s[%q]", scope, k.AsString()))
		if err != nil {
			return nil, err
		}
		if !set {
			return nil, fmt.Errorf("%s[%q] must not be null", scope, k.AsString())
		}
		out[k.AsString()] = n
	}
	return out, nil
}

func readOptionalIntMap(val cty.Value) map[string]int {
	m, err := decodeIntMapVal(val, "tool_call_quota")
	if err != nil {
		return nil
	}
	return m
}

func readOptionalStringSlice(val cty.Value) []string {
	s, err := decodeOptionalStringSliceVal(val, "list")
	if err != nil {
		return nil
	}
	return s
}
