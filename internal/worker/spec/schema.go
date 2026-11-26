package spec

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/provider"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

const (
	DefaultMaxProtocolAttempts = 10
	DefaultCheckpointRounds    = 5
	DefaultPermission          = "approve_all"
)

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
	LookupVariableSourceURLs(name string) ([]string, bool)
	LookupModuleOutputSourceURLs(moduleAddress, outputName string) ([]string, bool)
	ResolveLocalExpr(name string) (hcl.Expression, bool)
	ValidateWorkerFallbackTargets(workerAddress string, targets []string) error
}

type ToolUse struct {
	Name      string
	ToolID    string
	Terminate bool
	Input     cty.Value
}

func (u ToolUse) Clone() ToolUse {
	return u
}

type Config struct {
	BasedSource         cty.Value
	SourceURL           string
	Sandbox             cty.Value
	Harness             cty.Value
	ModelProvider       cty.Value
	Model               string
	Profile             string
	ReasoningEffort     string
	SystemPrompt        cty.Value
	Prompt              cty.Value
	WritablePaths       []string
	HumanApproval       string
	CheckpointRounds    int
	Permission          string
	MaxProtocolAttempts int
	Timeout             *time.Duration
	TimeoutRaw          string
	Retry               provider.RetryOverride
	ToolIDs             []string
	MCPToolIDs          []string
	MCPResourceIDs      []string
	TerminateToolID     string
	ToolCallQuota       map[string]int
	AllowedTools        []string
	DisallowedTools     []string
	SkillDirectories    []string
	Skills              []string
	DisabledSkills      []string
	ToolUses            []ToolUse
	Artifacts           map[string]Artifact
	Imports             map[string]ImportArtifact
	Gatekeepers         []GatekeeperConfig
	QC                  *QCConfig
	Fallbacks           []FallbackConfig
	DeferredExpression  string
}

func (c Config) Clone() Config {
	cloned := c
	cloned.WritablePaths = slices.Clone(c.WritablePaths)
	if c.Timeout != nil {
		d := *c.Timeout
		cloned.Timeout = &d
	}
	cloned.Retry = cloneRetryOverride(c.Retry)
	cloned.ToolIDs = slices.Clone(c.ToolIDs)
	cloned.MCPToolIDs = slices.Clone(c.MCPToolIDs)
	cloned.MCPResourceIDs = slices.Clone(c.MCPResourceIDs)
	cloned.ToolCallQuota = maps.Clone(c.ToolCallQuota)
	cloned.AllowedTools = slices.Clone(c.AllowedTools)
	cloned.DisallowedTools = slices.Clone(c.DisallowedTools)
	cloned.SkillDirectories = slices.Clone(c.SkillDirectories)
	cloned.Skills = slices.Clone(c.Skills)
	cloned.DisabledSkills = slices.Clone(c.DisabledSkills)
	if c.ToolUses != nil {
		cloned.ToolUses = make([]ToolUse, len(c.ToolUses))
		for i, u := range c.ToolUses {
			cloned.ToolUses[i] = u.Clone()
		}
	}
	cloned.Artifacts = maps.Clone(c.Artifacts)
	cloned.Imports = maps.Clone(c.Imports)
	if c.Gatekeepers != nil {
		cloned.Gatekeepers = make([]GatekeeperConfig, len(c.Gatekeepers))
		for i, g := range c.Gatekeepers {
			cloned.Gatekeepers[i] = g.Clone()
		}
	}
	if c.QC != nil {
		qc := c.QC.Clone()
		cloned.QC = &qc
	}
	if c.Fallbacks != nil {
		cloned.Fallbacks = make([]FallbackConfig, len(c.Fallbacks))
		for i, fb := range c.Fallbacks {
			cloned.Fallbacks[i] = fb.Clone()
		}
	}
	return cloned
}

type WorkerBlock struct {
	*golden.BaseBlock
	planned        Config
	artifacts      map[string]Artifact
	importPaths    map[string]cty.Value
	expandedBlock  *golden.HclBlock
	ownerBlockType string
}

var (
	workerAttributes = map[string]struct{}{
		"based_source":            {},
		"sandbox":                 {},
		"harness":                 {},
		"model_provider":          {},
		"model":                   {},
		"profile":                 {},
		"reasoning_effort":        {},
		"system_prompt":           {},
		"prompt":                  {},
		"writable_paths":          {},
		"human_approval":          {},
		"checkpoint_every_rounds": {},
		"permission":              {},
		"max_protocol_attempts":   {},
		"timeout":                 {},
		"tool_ids":                {},
		"mcp_tool_ids":            {},
		"mcp_resource_ids":        {},
		"terminate_tool_id":       {},
		"tool_call_quota":         {},
		"allowed_tools":           {},
		"disallowed_tools":        {},
		"skill_directories":       {},
		"skills":                  {},
		"disabled_skills":         {},
	}
	workerNestedBlockTypes = map[string]struct{}{
		"artifact":        {},
		"import_artifact": {},
		"gatekeeper":      {},
		"qc":              {},
		"fallback":        {},
		"tool_use":        {},
		"retry":           {},
	}
	retryAllowedAttrs = map[string]struct{}{
		"lifecycle_retries":    {},
		"model_call_retries":   {},
		"interval_seconds":     {},
		"max_interval_seconds": {},
		"error_message_regex":  {},
	}
	toolUseAllowedAttrs = map[string]struct{}{
		"tool_id":   {},
		"terminate": {},
		"input":     {},
	}
)

func (w *WorkerBlock) Type() string            { return "" }
func (w *WorkerBlock) BlockType() string       { return "worker" }
func (w *WorkerBlock) AddressLength() int      { return 2 }
func (w *WorkerBlock) CanExecutePrePlan() bool { return false }

// WorkerConfig returns a deep-copied snapshot of the planned worker configuration.
func (w *WorkerBlock) WorkerConfig() Config {
	return w.planned.Clone()
}

func (w *WorkerBlock) Values() map[string]cty.Value {
	artifacts := make(map[string]cty.Value, len(w.artifacts))
	for name, artifact := range w.artifacts {
		artifacts[name] = artifactValue(artifact)
	}
	artifactMap := cty.EmptyObjectVal
	if len(artifacts) > 0 {
		artifactMap = cty.ObjectVal(artifacts)
	}
	values := map[string]cty.Value{
		"work_branch": cty.UnknownVal(cty.String),
		"commit_sha":  cty.UnknownVal(cty.String),
		"artifact":    artifactMap,
	}
	if w.hasTerminatingTool() {
		values["result"] = cty.UnknownVal(cty.String)
	}
	return values
}

func (w *WorkerBlock) hasTerminatingTool() bool {
	if w.planned.TerminateToolID != "" {
		return true
	}
	for _, use := range w.planned.ToolUses {
		if use.Terminate {
			return true
		}
	}
	return false
}

func (w *WorkerBlock) EvalContext() *hcl.EvalContext {
	var ctx *hcl.EvalContext
	if w.BaseBlock != nil {
		ctx = w.BaseBlock.EvalContext()
	}
	if ctx == nil {
		ctx = &hcl.EvalContext{}
	}
	return w.addWorkerLocals(ctx)
}

func (w *WorkerBlock) addWorkerLocals(ctx *hcl.EvalContext) *hcl.EvalContext {
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
				return cty.NilVal, function.NewArgError(0, fail("artifact name must not be null"))
			}
			if !arg.IsKnown() {
				return cty.UnknownVal(artifactValueType), nil
			}
			name := strings.TrimSpace(arg.AsString())
			if name == "" {
				return cty.NilVal, function.NewArgError(0, fail("artifact name must not be empty"))
			}
			artifact, ok := w.artifacts[name]
			if !ok {
				return cty.NilVal, function.NewArgError(0, fail("artifact %q is not declared in worker %q", name, w.Name()))
			}
			return artifactValue(artifact), nil
		},
	})
	child.Variables = map[string]cty.Value{}
	imports := make(map[string]cty.Value, len(w.importPaths))
	for name, value := range w.importPaths {
		imports[name] = value
	}
	if len(imports) == 0 {
		child.Variables["import_artifact"] = cty.EmptyObjectVal
	} else {
		child.Variables["import_artifact"] = cty.ObjectVal(imports)
	}
	return child
}

func (w *WorkerBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error {
	w.planned = Config{}
	w.artifacts = map[string]Artifact{}
	w.importPaths = map[string]cty.Value{}
	w.expandedBlock = nil

	if err := validateSafeLabel(w.Name(), "worker", false); err != nil {
		return err
	}
	if err := w.validateSchema(block); err != nil {
		return err
	}

	expanded := block
	if hasDynamicBlocks(block) {
		if err := w.decodeNestedDeclarations(block, baseCtx); err != nil {
			return err
		}
		var err error
		expanded, err = block.ExpandDynamicBlocks(w.contextWithBase(baseCtx))
		if err != nil {
			return err
		}
		if err := w.validateSchema(expanded); err != nil {
			return err
		}
		w.artifacts = map[string]Artifact{}
		w.importPaths = map[string]cty.Value{}
		w.planned.Imports = nil
	}

	if err := w.decodeNestedDeclarations(expanded, baseCtx); err != nil {
		return err
	}
	ctx := w.contextWithBase(baseCtx)

	var deferred bool
	for _, imp := range w.planned.Imports {
		if !imp.Sources.IsWhollyKnown() {
			deferred = true
			break
		}
	}
	if err := w.decodeCore(expanded, ctx, &deferred); err != nil {
		return err
	}
	if err := w.decodeNestedExecution(expanded, ctx, &deferred); err != nil {
		return err
	}
	if err := w.validatePolicies(); err != nil {
		return err
	}

	w.planned.Artifacts = maps.Clone(w.artifacts)
	w.planned.Imports = maps.Clone(w.planned.Imports)
	w.expandedBlock = expanded
	if deferred {
		expr, err := buildDeferredExpression(expanded, w.planned.Imports, w.planned.Fallbacks)
		if err != nil {
			return err
		}
		w.planned.DeferredExpression = expr
	}
	return nil
}

func (w *WorkerBlock) ExecuteDuringPlan() error {
	if w.BaseBlock == nil || w.Config() == nil {
		return nil
	}
	reg, ok := w.Config().(ModuleRegistry)
	if !ok {
		return nil
	}
	if err := w.validateModuleToolsAndMCP(reg); err != nil {
		return err
	}
	sourceURL, err := w.resolveBasedSourceURL(reg)
	if err != nil {
		return err
	}
	w.planned.SourceURL = sourceURL

	if len(w.planned.Fallbacks) > 0 {
		targets := make([]string, len(w.planned.Fallbacks))
		for i, fb := range w.planned.Fallbacks {
			targets[i] = fb.Target
		}
		if err := reg.ValidateWorkerFallbackTargets(w.Address(), targets); err != nil {
			return err
		}
	}
	return nil
}

func (w *WorkerBlock) validateModuleToolsAndMCP(reg ModuleRegistry) error {
	for _, id := range w.planned.ToolIDs {
		if _, exists := reg.LookupRegisteredTool(id); !exists {
			return fail("worker %q references unregistered tool_id %q", w.Name(), id)
		}
	}
	if w.planned.TerminateToolID != "" {
		if _, exists := reg.LookupRegisteredTool(w.planned.TerminateToolID); !exists {
			return fail("worker %q references unregistered terminate_tool_id %q", w.Name(), w.planned.TerminateToolID)
		}
	}
	for _, use := range w.planned.ToolUses {
		info, exists := reg.LookupRegisteredTool(use.ToolID)
		if !exists {
			return fail("worker %q tool_use %q references unregistered tool_id %q", w.Name(), use.Name, use.ToolID)
		}
		if err := validateToolUseInputAgainstConstraint(use, info.InputType); err != nil {
			return err
		}
	}
	if w.planned.QC != nil {
		for _, id := range w.planned.QC.ToolIDs {
			if _, exists := reg.LookupRegisteredTool(id); !exists {
				return fail("worker %q qc references unregistered tool_id %q", w.Name(), id)
			}
		}
	}
	for _, id := range w.planned.MCPToolIDs {
		if !reg.HasRegisteredMCPTool(id) {
			return fail("worker %q references unregistered mcp_tool_id %q", w.Name(), id)
		}
	}
	for _, id := range w.planned.MCPResourceIDs {
		if !reg.HasRegisteredMCPResource(id) {
			return fail("worker %q references unregistered mcp_resource_id %q", w.Name(), id)
		}
	}
	return nil
}

func validateToolUseInputAgainstConstraint(use ToolUse, inputType cty.Type) error {
	if inputType == cty.NilType || !inputType.IsObjectType() {
		return nil
	}
	if use.Input == cty.NilVal {
		return nil
	}
	unmarked, _ := use.Input.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() {
		return nil
	}
	if !unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType() {
		return nil
	}
	for fieldName, fieldVal := range unmarked.AsValueMap() {
		if !inputType.HasAttribute(fieldName) {
			return fail("tool_use %q input field %q is not declared in tool %q", use.Name, fieldName, use.ToolID)
		}
		if !fieldVal.IsWhollyKnown() {
			continue
		}
		expectedType := inputType.AttributeType(fieldName)
		if fieldVal.IsNull() {
			if !inputType.AttributeOptional(fieldName) {
				return fail("tool_use %q input field %q must not be null", use.Name, fieldName)
			}
			continue
		}
		if err := validateStrictValueType(fieldVal, expectedType, fmt.Sprintf("tool_use %q input.%s", use.Name, fieldName)); err != nil {
			return err
		}
	}
	return nil
}

func validateStrictValueType(val cty.Value, expected cty.Type, path string) error {
	if val.IsNull() {
		return nil
	}
	switch {
	case expected.Equals(cty.String):
		if !val.Type().Equals(cty.String) {
			return fail("%s must be a string", path)
		}
	case expected.Equals(cty.Number):
		if !val.Type().Equals(cty.Number) {
			return fail("%s must be a number", path)
		}
	case expected.Equals(cty.Bool):
		if !val.Type().Equals(cty.Bool) {
			return fail("%s must be a bool", path)
		}
	case expected.IsListType() || expected.IsSetType() || expected.IsTupleType():
		if !val.Type().IsListType() && !val.Type().IsSetType() && !val.Type().IsTupleType() {
			return fail("%s must be a collection", path)
		}
		if !expected.IsTupleType() {
			for it := val.ElementIterator(); it.Next(); {
				_, elem := it.Element()
				if err := validateStrictValueType(elem, expected.ElementType(), path+"[]"); err != nil {
					return err
				}
			}
		}
	case expected.IsMapType():
		if !val.Type().IsMapType() && !val.Type().IsObjectType() {
			return fail("%s must be a map", path)
		}
		for k, elem := range val.AsValueMap() {
			if err := validateStrictValueType(elem, expected.ElementType(), path+"."+k); err != nil {
				return err
			}
		}
	case expected.IsObjectType():
		if !val.Type().IsObjectType() && !val.Type().IsMapType() {
			return fail("%s must be an object", path)
		}
		for k := range val.AsValueMap() {
			if !expected.HasAttribute(k) {
				return fail("%s has undeclared attribute %q", path, k)
			}
		}
		for k, attrType := range expected.AttributeTypes() {
			elem, exists := val.AsValueMap()[k]
			if !exists {
				continue
			}
			if elem.IsNull() {
				if !expected.AttributeOptional(k) {
					return fail("%s.%s must not be null", path, k)
				}
				continue
			}
			if err := validateStrictValueType(elem, attrType, path+"."+k); err != nil {
				return err
			}
		}
	default:
		if _, err := convert.Convert(val, expected); err != nil {
			return fail("%s does not match expected type: %v", path, err)
		}
	}
	return nil
}

func (w *WorkerBlock) resolveBasedSourceURL(reg ModuleRegistry) (string, error) {
	attr, ok := w.HclBlock().Attributes()["based_source"]
	if !ok {
		return "", nil
	}
	urls := map[string]struct{}{}
	visitedLocals := map[string]struct{}{}

	var inspectExpr func(hcl.Expression) error
	inspectExpr = func(expr hcl.Expression) error {
		if expr == nil {
			return nil
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
							if err := inspectExpr(localExpr); err != nil {
								return err
							}
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
						if workerURLs, exists := reg.LookupWorkerSourceURLs(addr); exists {
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
						if upstreamURLs, exists := reg.LookupWorkerSourceURLs(addr); exists {
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
				if addr, outputName, ok := ParseModuleOutputTraversal(traversal); ok {
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
		return nil
	}

	if err := inspectExpr(attr.Expr); err != nil {
		return "", err
	}
	if len(urls) > 1 {
		return "", fail("worker %q based_source references multiple distinct source repositories", w.Name())
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
		return "", fail("worker %q based_source does not identify a source repository and %d source blocks exist; reference source.<name>.branch or an upstream worker", w.Name(), sourceCount)
	}
}

// ParseModuleOutputTraversal extracts the module block address (including any
// known for_each index) and optional output name from a `module.*` traversal.
func ParseModuleOutputTraversal(traversal hcl.Traversal) (moduleAddr, outputName string, ok bool) {
	if len(traversal) < 2 || traversal.RootName() != "module" {
		return "", "", false
	}
	mAttr, isAttr := traversal[1].(hcl.TraverseAttr)
	if !isAttr || mAttr.Name == "" {
		return "", "", false
	}
	moduleAddr = "module." + mAttr.Name
	outputIdx := 2
	if len(traversal) >= 3 {
		if idx, isIdx := traversal[2].(hcl.TraverseIndex); isIdx {
			k, _ := idx.Key.UnmarkDeep()
			if k.IsKnown() && !k.IsNull() && (k.Type().Equals(cty.String) || k.Type().Equals(cty.Number)) {
				moduleAddr = fmt.Sprintf("%s[%s]", moduleAddr, golden.CtyValueToString(k))
			}
			outputIdx = 3
		}
	}
	if len(traversal) > outputIdx {
		if outAttr, isOut := traversal[outputIdx].(hcl.TraverseAttr); isOut {
			outputName = outAttr.Name
		}
	}
	return moduleAddr, outputName, true
}

func (w *WorkerBlock) contextWithBase(base *hcl.EvalContext) *hcl.EvalContext {
	if base == nil {
		base = &hcl.EvalContext{}
	}
	return w.addWorkerLocals(base)
}

func HasDynamicBlocks(block *golden.HclBlock) bool {
	return hasDynamicBlocks(block)
}

func hasDynamicBlocks(block *golden.HclBlock) bool {
	if block == nil {
		return false
	}
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "dynamic" || hasDynamicBlocks(nested) {
			return true
		}
	}
	return false
}

func (w *WorkerBlock) validateSchema(block *golden.HclBlock) error {
	for name := range block.Attributes() {
		if _, ok := workerAttributes[name]; ok || golden.MetaAttributeNames.Contains(name) {
			continue
		}
		return fail("unsupported argument %q in worker block", name)
	}
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "artifact", "import_artifact", "gatekeeper", "fallback", "tool_use":
			if len(nested.Labels) != 1 {
				return fail("worker %s block must have exactly one name", nested.Type)
			}
		case "qc", "retry":
			if len(nested.Labels) != 0 {
				return fail("worker %s block does not accept labels", nested.Type)
			}
		case "dynamic":
			if len(nested.Labels) != 1 {
				return fail("worker dynamic block must declare a single target block label")
			}
			if _, ok := workerNestedBlockTypes[nested.Labels[0]]; !ok {
				return fail("unsupported dynamic block type %q in worker block", nested.Labels[0])
			}
		default:
			if golden.MetaNestedBlockNames.Contains(nested.Type) {
				continue
			}
			return fail("unsupported block type %q in worker block", nested.Type)
		}
	}
	return nil
}

func validateNestedBlockSchema(
	block *golden.HclBlock,
	scope string,
	allowedAttrs map[string]struct{},
	allowedNested map[string]struct{},
) error {
	for name := range block.Attributes() {
		if _, ok := allowedAttrs[name]; ok {
			continue
		}
		return fail("unsupported argument %q in %s block", name, scope)
	}
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "dynamic" {
			if allowedNested == nil || len(nested.Labels) != 1 {
				return fail("unsupported block type %q in %s block", nested.Type, scope)
			}
			if _, ok := allowedNested[nested.Labels[0]]; !ok {
				return fail("unsupported dynamic block type %q in %s block", nested.Labels[0], scope)
			}
			continue
		}
		if allowedNested != nil {
			if _, ok := allowedNested[nested.Type]; ok {
				continue
			}
		}
		return fail("unsupported block type %q in %s block", nested.Type, scope)
	}
	return nil
}

func (w *WorkerBlock) decodeNestedDeclarations(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	artifactPaths := map[string]string{}
	importRelPaths := map[string]string{}
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "artifact":
			art, err := decodeArtifact(nested, ctx)
			if err != nil {
				return err
			}
			if _, exists := w.artifacts[art.Name]; exists {
				return fail("worker %q artifact %q is declared more than once", w.Name(), art.Name)
			}
			if other, exists := findConflictingPath(artifactPaths, art.Path); exists {
				return fail("artifact %q path conflicts with %q", art.Name, other)
			}
			artifactPaths[art.Path] = art.Name
			w.artifacts[art.Name] = art
		case "import_artifact":
			ownerType := w.ownerBlockType
			if ownerType == "" {
				ownerType = "worker"
			}
			imported, err := decodeImport(nested, ctx, ownerType, w.Name())
			if err != nil {
				return err
			}
			if _, exists := w.planned.Imports[imported.Name]; exists {
				return fail("worker %q import_artifact %q is declared more than once", w.Name(), imported.Name)
			}
			imported.Path = effectiveImportRelPath(imported)
			if other, exists := findConflictingPath(importRelPaths, imported.Path); exists {
				return fail("import_artifact %q path conflicts with %q", imported.Name, other)
			}
			importRelPaths[imported.Path] = imported.Name
			if w.planned.Imports == nil {
				w.planned.Imports = map[string]ImportArtifact{}
			}
			w.planned.Imports[imported.Name] = imported
			w.importPaths[imported.Name] = cty.ObjectVal(map[string]cty.Value{
				"path": cty.StringVal(imported.ContainerPath()),
				"desc": cty.StringVal(imported.Desc),
			})
		}
	}
	return nil
}

func findConflictingPath(existing map[string]string, candidate string) (string, bool) {
	for existingPath, owner := range existing {
		if existingPath == candidate ||
			strings.HasPrefix(candidate, existingPath+"/") ||
			strings.HasPrefix(existingPath, candidate+"/") {
			return owner, true
		}
	}
	return "", false
}

func (w *WorkerBlock) decodeCore(block *golden.HclBlock, ctx *hcl.EvalContext, deferred *bool) error {
	scope := fmt.Sprintf("worker %q", w.Name())
	var err error

	if w.planned.BasedSource, err = requiredStringAllowUnknown(block, ctx, "based_source", scope); err != nil {
		return err
	}
	if knownString(w.planned.BasedSource) {
		if err := corespec.ValidateBranchName(knownStringValue(w.planned.BasedSource)); err != nil {
			return fmt.Errorf("%s based_source: %w", scope, err)
		}
	} else {
		*deferred = true
	}

	if w.planned.Sandbox, err = requiredBlockReference(block, ctx, "sandbox", "sandbox", scope); err != nil {
		return err
	}
	if w.planned.Harness, err = requiredBlockReference(block, ctx, "harness", "harness", scope); err != nil {
		return err
	}
	if w.planned.ModelProvider, err = optionalBlockReference(block, ctx, "model_provider", "model_provider", scope); err != nil {
		return err
	}

	if w.planned.Model, err = requiredKnownNonBlankString(block, ctx, "model", scope); err != nil {
		return err
	}

	if profile, set, err := optionalKnownString(block, ctx, "profile", scope); err != nil {
		return err
	} else if set {
		if strings.TrimSpace(profile) == "" {
			return fail("%s profile must not be empty", scope)
		}
		w.planned.Profile = profile
	}

	if effort, set, err := optionalKnownString(block, ctx, "reasoning_effort", scope); err != nil {
		return err
	} else if set {
		if strings.TrimSpace(effort) == "" {
			return fail("%s reasoning_effort must not be empty", scope)
		}
		w.planned.ReasoningEffort = effort
	}

	if w.planned.SystemPrompt, err = requiredStringAllowUnknown(block, ctx, "system_prompt", scope); err != nil {
		return err
	}
	if knownString(w.planned.SystemPrompt) {
		if strings.TrimSpace(knownStringValue(w.planned.SystemPrompt)) == "" {
			return fail("%s system_prompt must not be empty", scope)
		}
	} else {
		*deferred = true
	}

	if w.planned.Prompt, err = optionalStringAllowUnknown(block, ctx, "prompt", scope); err != nil {
		return err
	}
	if w.planned.Prompt != cty.NilVal && !w.planned.Prompt.IsWhollyKnown() {
		*deferred = true
	}

	if w.planned.WritablePaths, err = optionalKnownStringList(block, ctx, "writable_paths", scope, true); err != nil {
		return err
	}
	for _, candidate := range w.planned.WritablePaths {
		if err := validateWorkspaceRelativePath(candidate, "writable_paths entry", true); err != nil {
			return err
		}
	}

	if approval, set, err := optionalKnownString(block, ctx, "human_approval", scope); err != nil {
		return err
	} else if set {
		if approval != "optional" && approval != "required" && approval != "disabled" {
			return fail("%s human_approval must be optional, required, or disabled", scope)
		}
		w.planned.HumanApproval = approval
	} else {
		w.planned.HumanApproval = "optional"
	}

	if w.planned.CheckpointRounds, err = optionalWholeInt(block, ctx, "checkpoint_every_rounds", DefaultCheckpointRounds, scope); err != nil {
		return err
	}
	if w.planned.CheckpointRounds <= 0 {
		return fail("%s checkpoint_every_rounds must be positive", scope)
	}

	if perm, set, err := optionalKnownString(block, ctx, "permission", scope); err != nil {
		return err
	} else if set {
		if perm != DefaultPermission {
			return fail("%s permission must be %s", scope, DefaultPermission)
		}
		w.planned.Permission = perm
	} else {
		w.planned.Permission = DefaultPermission
	}

	if w.planned.MaxProtocolAttempts, err = optionalWholeInt(block, ctx, "max_protocol_attempts", DefaultMaxProtocolAttempts, scope); err != nil {
		return err
	}
	if w.planned.MaxProtocolAttempts <= 0 {
		return fail("%s max_protocol_attempts must be positive", scope)
	}

	if timeoutRaw, set, err := optionalKnownString(block, ctx, "timeout", scope); err != nil {
		return err
	} else if set {
		if strings.TrimSpace(timeoutRaw) == "" {
			return fail("%s timeout must not be empty", scope)
		}
		parsed, err := time.ParseDuration(timeoutRaw)
		if err != nil || parsed <= 0 {
			return fail("%s timeout must be a positive duration", scope)
		}
		w.planned.Timeout = &parsed
		w.planned.TimeoutRaw = timeoutRaw
	}

	if w.planned.ToolIDs, err = optionalKnownStringList(block, ctx, "tool_ids", scope, true); err != nil {
		return err
	}
	if w.planned.MCPToolIDs, err = optionalKnownStringList(block, ctx, "mcp_tool_ids", scope, true); err != nil {
		return err
	}
	if w.planned.MCPResourceIDs, err = optionalKnownStringList(block, ctx, "mcp_resource_ids", scope, true); err != nil {
		return err
	}
	if terminateID, set, err := optionalKnownString(block, ctx, "terminate_tool_id", scope); err != nil {
		return err
	} else if set {
		if strings.TrimSpace(terminateID) == "" {
			return fail("%s terminate_tool_id must not be empty", scope)
		}
		w.planned.TerminateToolID = terminateID
	}

	if w.planned.ToolCallQuota, err = optionalIntMap(block, ctx, "tool_call_quota", scope); err != nil {
		return err
	}
	if w.planned.AllowedTools, err = optionalKnownStringList(block, ctx, "allowed_tools", scope, true); err != nil {
		return err
	}
	if w.planned.DisallowedTools, err = optionalKnownStringList(block, ctx, "disallowed_tools", scope, true); err != nil {
		return err
	}
	if w.planned.SkillDirectories, err = optionalKnownStringList(block, ctx, "skill_directories", scope, true); err != nil {
		return err
	}
	if w.planned.Skills, err = optionalKnownStringList(block, ctx, "skills", scope, true); err != nil {
		return err
	}
	if w.planned.DisabledSkills, err = optionalKnownStringList(block, ctx, "disabled_skills", scope, true); err != nil {
		return err
	}

	return nil
}

func (w *WorkerBlock) decodeNestedExecution(block *golden.HclBlock, ctx *hcl.EvalContext, deferred *bool) error {
	gatekeeperNames := map[string]struct{}{}
	fallbackNames := map[string]struct{}{}
	triggerTargets := map[string]string{}
	toolUseNames := map[string]struct{}{}
	toolUseToolIDs := map[string]struct{}{}
	retryCount := 0

	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "retry":
			retryCount++
			if retryCount > 1 {
				return fail("worker %q supports at most one retry block", w.Name())
			}
			override, err := decodeRetry(nested, ctx, fmt.Sprintf("worker %q retry", w.Name()))
			if err != nil {
				return err
			}
			w.planned.Retry = override
		case "gatekeeper":
			gatekeeper, err := DecodeGatekeeper(nested, ctx)
			if err != nil {
				return err
			}
			if _, exists := gatekeeperNames[gatekeeper.Name]; exists {
				return fail("worker %q gatekeeper %q is declared more than once", w.Name(), gatekeeper.Name)
			}
			gatekeeperNames[gatekeeper.Name] = struct{}{}
			w.planned.Gatekeepers = append(w.planned.Gatekeepers, gatekeeper)
		case "qc":
			if w.planned.QC != nil {
				return fail("worker %q supports at most one qc block", w.Name())
			}
			qc, err := decodeQC(nested, ctx)
			if err != nil {
				return err
			}
			w.planned.QC = &qc
		case "fallback":
			fb, err := decodeFallback(nested, ctx, w.artifacts)
			if err != nil {
				return err
			}
			if _, exists := fallbackNames[fb.Name]; exists {
				return fail("fallback %q is declared more than once", fb.Name)
			}
			fallbackNames[fb.Name] = struct{}{}
			for _, trigger := range fb.On {
				if previous, exists := triggerTargets[trigger]; exists && previous != fb.Name {
					return fail("fallback trigger %q is bound to both %q and %q", trigger, previous, fb.Name)
				}
				triggerTargets[trigger] = fb.Name
			}
			w.planned.Fallbacks = append(w.planned.Fallbacks, fb)
		case "tool_use":
			use, err := decodeToolUse(nested, ctx)
			if err != nil {
				return err
			}
			if _, exists := toolUseNames[use.Name]; exists {
				return fail("tool_use %q is declared more than once", use.Name)
			}
			toolUseNames[use.Name] = struct{}{}
			if _, exists := toolUseToolIDs[use.ToolID]; exists {
				return fail("typed tool %q is used more than once in tool_use", use.ToolID)
			}
			toolUseToolIDs[use.ToolID] = struct{}{}
			if use.Input != cty.NilVal && !use.Input.IsWhollyKnown() {
				*deferred = true
			}
			w.planned.ToolUses = append(w.planned.ToolUses, use)
		}
	}
	return nil
}

func (w *WorkerBlock) validatePolicies() error {
	if len(w.planned.ToolUses) > 0 && (len(w.planned.ToolIDs) > 0 || w.planned.TerminateToolID != "") {
		return fail("worker %q tool_use is mutually exclusive with tool_ids and terminate_tool_id", w.Name())
	}

	terminating := 0
	terminatingID := w.planned.TerminateToolID
	if terminatingID != "" {
		terminating++
	}
	for _, use := range w.planned.ToolUses {
		if use.Terminate {
			terminating++
			terminatingID = use.ToolID
		}
	}
	if terminating > 1 {
		return fail("worker %q may declare at most one terminating tool", w.Name())
	}

	sessionTools := slices.Clone(w.planned.ToolIDs)
	if w.planned.TerminateToolID != "" {
		sessionTools = append(sessionTools, w.planned.TerminateToolID)
	}
	for _, use := range w.planned.ToolUses {
		sessionTools = append(sessionTools, use.ToolID)
	}
	if err := validateToolCallQuota(w.planned.ToolCallQuota, sessionTools, fmt.Sprintf("worker %q", w.Name())); err != nil {
		return err
	}
	if terminatingID != "" {
		if err := validateMandatoryTool(w.planned.AllowedTools, w.planned.DisallowedTools, terminatingID); err != nil {
			return fmt.Errorf("worker %q: %w", w.Name(), err)
		}
	}

	workerRetry, err := provider.MergeRetry(provider.DefaultRetryPolicy(), w.planned.Retry)
	if err != nil {
		return fmt.Errorf("worker %q retry: %w", w.Name(), err)
	}
	if w.planned.QC != nil {
		if _, err := provider.MergeRetry(workerRetry, w.planned.QC.Retry); err != nil {
			return fmt.Errorf("worker %q qc retry: %w", w.Name(), err)
		}
	}
	return nil
}

func validateToolCallQuota(quota map[string]int, sessionToolIDs []string, scope string) error {
	for toolName, limit := range quota {
		if strings.TrimSpace(toolName) == "" {
			return fail("%s tool_call_quota must not contain an empty tool name", scope)
		}
		if limit < 0 {
			return fail("%s tool_call_quota for %q must be non-negative", scope, toolName)
		}
		if strings.HasPrefix(toolName, "mcp_tool_") {
			return fail("%s tool_call_quota does not support mcp tool id %q", scope, toolName)
		}
		if strings.HasPrefix(toolName, "tool_") && !slices.Contains(sessionToolIDs, toolName) {
			return fail("%s tool_call_quota references tool id %q that is not configured for this session", scope, toolName)
		}
	}
	return nil
}

func validateMandatoryTool(allowed, disallowed []string, toolID string) error {
	if allowed != nil && !slices.ContainsFunc(allowed, func(filter string) bool {
		return matchesCustomTool(filter, toolID)
	}) {
		return fail("mandatory tool %s must be included in allowed_tools", toolID)
	}
	if slices.ContainsFunc(disallowed, func(filter string) bool {
		return matchesCustomTool(filter, toolID)
	}) {
		return fail("mandatory tool %s must not be in disallowed_tools", toolID)
	}
	return nil
}

func matchesCustomTool(filter, name string) bool {
	return filter == name || filter == "custom:"+name || filter == "custom:*"
}

func decodeRetry(block *golden.HclBlock, ctx *hcl.EvalContext, scope string) (provider.RetryOverride, error) {
	if len(block.Labels) != 0 {
		return provider.RetryOverride{}, fail("%s block does not accept labels", scope)
	}
	if err := validateNestedBlockSchema(block, scope, retryAllowedAttrs, nil); err != nil {
		return provider.RetryOverride{}, err
	}

	var override provider.RetryOverride
	if val, set, err := optionalWholeIntPtr(block, ctx, "lifecycle_retries", scope); err != nil {
		return provider.RetryOverride{}, err
	} else if set {
		override.LifecycleRetries = val
	}
	if val, set, err := optionalWholeIntPtr(block, ctx, "model_call_retries", scope); err != nil {
		return provider.RetryOverride{}, err
	} else if set {
		override.ModelCallRetries = val
	}
	if val, set, err := optionalSecondsPtr(block, ctx, "interval_seconds", scope); err != nil {
		return provider.RetryOverride{}, err
	} else if set {
		override.Interval = val
	}
	if val, set, err := optionalSecondsPtr(block, ctx, "max_interval_seconds", scope); err != nil {
		return provider.RetryOverride{}, err
	} else if set {
		override.MaxInterval = val
	}
	regexes, err := optionalKnownStringList(block, ctx, "error_message_regex", scope, true)
	if err != nil {
		return provider.RetryOverride{}, err
	}
	override.ErrorMessageRegex = regexes

	if _, err := provider.MergeRetry(provider.DefaultRetryPolicy(), override); err != nil {
		return provider.RetryOverride{}, fmt.Errorf("%s: %w", scope, err)
	}
	return override, nil
}

func decodeToolUse(block *golden.HclBlock, ctx *hcl.EvalContext) (ToolUse, error) {
	if len(block.Labels) != 1 {
		return ToolUse{}, fail("worker tool_use block must have exactly one name")
	}
	name := block.Labels[0]
	if err := validateSafeLabel(name, "tool_use", false); err != nil {
		return ToolUse{}, err
	}
	scope := fmt.Sprintf("tool_use %q", name)
	if err := validateNestedBlockSchema(block, scope, toolUseAllowedAttrs, nil); err != nil {
		return ToolUse{}, err
	}

	toolUse := ToolUse{Name: name, Input: cty.EmptyObjectVal}
	var err error
	if toolUse.ToolID, err = requiredKnownNonBlankString(block, ctx, "tool_id", scope); err != nil {
		return ToolUse{}, err
	}
	if toolUse.Terminate, err = optionalBool(block, ctx, "terminate", false, scope); err != nil {
		return ToolUse{}, err
	}
	if attr, ok := block.Attributes()["input"]; ok {
		value, diags := attr.Expr.Value(ctx)
		if diags.HasErrors() {
			return ToolUse{}, diags
		}
		unmarked, _ := value.UnmarkDeep()
		if !unmarked.IsNull() {
			if !unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType() {
				return ToolUse{}, fail("%s input must be an object", scope)
			}
			toolUse.Input = value
		}
	}
	return toolUse, nil
}

func requiredKnownNonBlankString(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (string, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return "", fail("%s %s is required", scope, name)
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return "", diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return "", fail("%s %s must not be null", scope, name)
	}
	if !unmarked.IsWhollyKnown() {
		return "", fail("%s %s must be known during plan", scope, name)
	}
	if !unmarked.Type().Equals(cty.String) {
		return "", fail("%s %s must be a string", scope, name)
	}
	str := unmarked.AsString()
	if strings.TrimSpace(str) == "" {
		return "", fail("%s %s must not be empty", scope, name)
	}
	return str, nil
}

func OptionalKnownString(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (string, bool, error) {
	return optionalKnownString(block, ctx, name, scope)
}

func optionalKnownString(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (string, bool, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return "", false, nil
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return "", false, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return "", false, nil
	}
	if !unmarked.IsWhollyKnown() {
		return "", false, fail("%s %s must be known during plan", scope, name)
	}
	if !unmarked.Type().Equals(cty.String) {
		return "", false, fail("%s %s must be a string", scope, name)
	}
	return unmarked.AsString(), true, nil
}

func RequiredStringAllowUnknown(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (cty.Value, error) {
	return requiredStringAllowUnknown(block, ctx, name, scope)
}

func requiredStringAllowUnknown(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (cty.Value, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NilVal, fail("%s %s is required", scope, name)
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NilVal, fail("%s %s must not be null", scope, name)
	}
	if !unmarked.Type().Equals(cty.String) {
		return cty.NilVal, fail("%s %s must be a string", scope, name)
	}
	return value, nil
}

func optionalStringAllowUnknown(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (cty.Value, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NilVal, nil
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NilVal, nil
	}
	if !unmarked.Type().Equals(cty.String) {
		return cty.NilVal, fail("%s %s must be a string", scope, name)
	}
	return value, nil
}

func requiredBlockReference(block *golden.HclBlock, ctx *hcl.EvalContext, name, expectedKind, scope string) (cty.Value, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NilVal, fail("%s %s is required", scope, name)
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	if err := validateBlockReference(value, expectedKind, scope+" "+name); err != nil {
		return cty.NilVal, err
	}
	return value, nil
}

func optionalBlockReference(block *golden.HclBlock, ctx *hcl.EvalContext, name, expectedKind, scope string) (cty.Value, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NullVal(cty.EmptyObject), nil
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NullVal(cty.EmptyObject), nil
	}
	if err := validateBlockReference(value, expectedKind, scope+" "+name); err != nil {
		return cty.NilVal, err
	}
	return value, nil
}

func ValidateBlockReference(value cty.Value, expectedKind, scope string) error {
	return validateBlockReference(value, expectedKind, scope)
}

func validateBlockReference(value cty.Value, expectedKind, scope string) error {
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return fail("%s must not be null", scope)
	}
	if !unmarked.IsWhollyKnown() {
		return fail("%s must be wholly known during plan", scope)
	}
	if !unmarked.Type().IsObjectType() {
		return fail("%s must reference a %s object", scope, expectedKind)
	}
	if !unmarked.Type().HasAttribute("address") || !unmarked.Type().HasAttribute("kind") {
		return fail("%s must reference a %s block", scope, expectedKind)
	}
	addr := unmarked.GetAttr("address")
	if addr.IsNull() || !addr.IsKnown() || !addr.Type().Equals(cty.String) || strings.TrimSpace(addr.AsString()) == "" {
		return fail("%s reference address must be a non-empty string", scope)
	}
	kind := unmarked.GetAttr("kind")
	if kind.IsNull() || !kind.IsKnown() || !kind.Type().Equals(cty.String) {
		return fail("%s reference kind must be %q", scope, expectedKind)
	}
	if kind.AsString() != expectedKind {
		return fail("%s must reference a %s block (got %q)", scope, expectedKind, kind.AsString())
	}
	return nil
}

func requiredKnownOrderedStringList(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string, rejectBlank bool) ([]string, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return nil, fail("%s %s is required", scope, name)
	}
	return evalKnownStringList(attr, ctx, scope+" "+name, false, false, rejectBlank)
}

func requiredKnownStringList(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string, rejectBlank bool) ([]string, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return nil, fail("%s %s is required", scope, name)
	}
	return evalKnownStringList(attr, ctx, scope+" "+name, false, true, rejectBlank)
}

func optionalKnownStringList(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string, rejectBlank bool) ([]string, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return nil, nil
	}
	return evalKnownStringList(attr, ctx, scope+" "+name, true, true, rejectBlank)
}

func evalKnownStringList(
	attr *golden.HclAttribute,
	ctx *hcl.EvalContext,
	field string,
	allowNull bool,
	allowSet bool,
	rejectBlank bool,
) ([]string, error) {
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		if allowNull {
			return nil, nil
		}
		return nil, fail("%s must not be null", field)
	}
	if !unmarked.IsWhollyKnown() {
		return nil, fail("%s must be wholly known during plan", field)
	}
	vt := unmarked.Type()
	validCollection := vt.IsListType() || vt.IsTupleType() || (allowSet && vt.IsSetType())
	if !validCollection {
		return nil, fail("%s must be a list of strings", field)
	}
	result := make([]string, 0, unmarked.LengthInt())
	for iterator := unmarked.ElementIterator(); iterator.Next(); {
		_, item := iterator.Element()
		if item.IsNull() || !item.Type().Equals(cty.String) {
			return nil, fail("%s elements must be non-null strings", field)
		}
		str := item.AsString()
		if rejectBlank && strings.TrimSpace(str) == "" {
			return nil, fail("%s must not contain empty strings", field)
		}
		result = append(result, str)
	}
	return result, nil
}

func optionalIntMap(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (map[string]int, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return nil, nil
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, fail("%s %s must be wholly known during plan", scope, name)
	}
	if !unmarked.Type().IsMapType() && !unmarked.Type().IsObjectType() {
		return nil, fail("%s %s must be a map of integers", scope, name)
	}
	result := make(map[string]int, unmarked.LengthInt())
	for iterator := unmarked.ElementIterator(); iterator.Next(); {
		k, v := iterator.Element()
		n, err := ctyToWholeInt(v, fmt.Sprintf("%s %s[%q]", scope, name, k.AsString()))
		if err != nil {
			return nil, err
		}
		result[k.AsString()] = n
	}
	return result, nil
}

func OptionalWholeInt(block *golden.HclBlock, ctx *hcl.EvalContext, name string, defaultValue int, scope string) (int, error) {
	return optionalWholeInt(block, ctx, name, defaultValue, scope)
}

func optionalWholeInt(block *golden.HclBlock, ctx *hcl.EvalContext, name string, defaultValue int, scope string) (int, error) {
	ptr, set, err := optionalWholeIntPtr(block, ctx, name, scope)
	if err != nil {
		return 0, err
	}
	if !set {
		return defaultValue, nil
	}
	return *ptr, nil
}

func optionalWholeIntPtr(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (*int, bool, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return nil, false, nil
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, false, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, false, nil
	}
	n, err := ctyToWholeInt(unmarked, scope+" "+name)
	if err != nil {
		return nil, false, err
	}
	return &n, true, nil
}

func optionalSecondsPtr(block *golden.HclBlock, ctx *hcl.EvalContext, name, scope string) (*time.Duration, bool, error) {
	val, set, err := optionalWholeIntPtr(block, ctx, name, scope)
	if err != nil || !set {
		return nil, set, err
	}
	if *val < 0 || int64(*val) > int64(math.MaxInt64/int64(time.Second)) {
		return nil, false, fail("%s %s is out of range", scope, name)
	}
	d := time.Duration(*val) * time.Second
	return &d, true, nil
}

func ctyToWholeInt(unmarked cty.Value, field string) (int, error) {
	if unmarked.IsNull() {
		return 0, fail("%s must not be null", field)
	}
	if !unmarked.IsKnown() {
		return 0, fail("%s must be known during plan", field)
	}
	if !unmarked.Type().Equals(cty.Number) {
		return 0, fail("%s must be an integer", field)
	}
	bf := unmarked.AsBigFloat()
	if bf.IsInf() || !bf.IsInt() {
		return 0, fail("%s must be a finite whole integer", field)
	}
	i64, acc := bf.Int64()
	if acc != big.Exact || i64 < math.MinInt || i64 > math.MaxInt {
		return 0, fail("%s integer value is out of range", field)
	}
	return int(i64), nil
}

func optionalBool(block *golden.HclBlock, ctx *hcl.EvalContext, name string, defaultValue bool, scope string) (bool, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return defaultValue, nil
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return false, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return defaultValue, nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.Bool) {
		return false, fail("%s %s must be a known bool", scope, name)
	}
	return unmarked.True(), nil
}

func knownString(value cty.Value) bool {
	if value == cty.NilVal {
		return false
	}
	unmarked, _ := value.UnmarkDeep()
	return !unmarked.IsNull() && unmarked.IsWhollyKnown() && unmarked.Type().Equals(cty.String)
}

func knownStringValue(value cty.Value) string {
	unmarked, _ := value.UnmarkDeep()
	return unmarked.AsString()
}

func cloneRetryOverride(retry provider.RetryOverride) provider.RetryOverride {
	result := retry
	if retry.LifecycleRetries != nil {
		v := *retry.LifecycleRetries
		result.LifecycleRetries = &v
	}
	if retry.ModelCallRetries != nil {
		v := *retry.ModelCallRetries
		result.ModelCallRetries = &v
	}
	if retry.Interval != nil {
		v := *retry.Interval
		result.Interval = &v
	}
	if retry.MaxInterval != nil {
		v := *retry.MaxInterval
		result.MaxInterval = &v
	}
	result.ErrorMessageRegex = slices.Clone(retry.ErrorMessageRegex)
	return result
}

var _ golden.CustomDecode = (*WorkerBlock)(nil)
var _ golden.PlanBlock = (*WorkerBlock)(nil)
var _ golden.Valuable = (*WorkerBlock)(nil)
