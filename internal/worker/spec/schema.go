package spec

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

type ToolUse struct {
	Name      string
	ToolID    string
	Terminate bool
	Input     cty.Value
}

type Config struct {
	BasedSource        cty.Value
	Sandbox            cty.Value
	Harness            cty.Value
	ModelProvider      cty.Value
	Model              string
	Profile            string
	ReasoningEffort    string
	SystemPrompt       cty.Value
	Prompt             cty.Value
	WritablePaths      []string
	HumanApproval      string
	CheckpointRounds   int
	ToolIDs            []string
	MCPToolIDs         []string
	MCPResourceIDs     []string
	TerminateToolID    string
	ToolUses           []ToolUse
	Artifacts          map[string]Artifact
	Imports            map[string]ImportArtifact
	Gatekeepers        []GatekeeperConfig
	QC                 *QCConfig
	Fallbacks          []FallbackConfig
	DeferredExpression string
}

type WorkerBlock struct {
	*golden.BaseBlock
	WorkerConfig Config
	artifacts    map[string]Artifact
	importPaths  map[string]cty.Value
}

var workerAttributes = map[string]struct{}{
	"based_source": {}, "sandbox": {}, "harness": {}, "model_provider": {},
	"model": {}, "profile": {}, "reasoning_effort": {}, "system_prompt": {}, "prompt": {},
	"writable_paths": {}, "human_approval": {}, "checkpoint_every_rounds": {},
	"tool_ids": {}, "mcp_tool_ids": {}, "mcp_resource_ids": {}, "terminate_tool_id": {},
	"permission": {}, "max_protocol_attempts": {}, "timeout": {},
	"skill_directories": {}, "skills": {}, "disabled_skills": {},
	"allowed_tools": {}, "disallowed_tools": {},
}

func (w *WorkerBlock) Type() string            { return "" }
func (w *WorkerBlock) BlockType() string       { return "worker" }
func (w *WorkerBlock) AddressLength() int      { return 2 }
func (w *WorkerBlock) CanExecutePrePlan() bool { return false }

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
	if w.WorkerConfig.TerminateToolID != "" {
		return true
	}
	for _, use := range w.WorkerConfig.ToolUses {
		if use.Terminate {
			return true
		}
	}
	return false
}

func (w *WorkerBlock) EvalContext() *hcl.EvalContext {
	ctx := w.BaseBlock.EvalContext()
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
		Type: func(args []cty.Value) (cty.Type, error) {
			artifact, ok := w.artifacts[args[0].AsString()]
			if !ok {
				return cty.NilType, fail("artifact %q is not declared in worker %q", args[0].AsString(), w.Name())
			}
			return artifactValue(artifact).Type(), nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return artifactValue(w.artifacts[args[0].AsString()]), nil
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
	w.WorkerConfig = Config{}
	w.artifacts = map[string]Artifact{}
	w.importPaths = map[string]cty.Value{}
	if err := w.validateSchema(block); err != nil {
		return err
	}
	if err := w.decodeNestedDeclarations(block, baseCtx); err != nil {
		return err
	}
	ctx := w.contextWithBase(baseCtx)

	var deferred bool
	if err := w.decodeCore(block, ctx, &deferred); err != nil {
		return err
	}
	if err := w.decodeNestedExecution(block, ctx); err != nil {
		return err
	}
	w.WorkerConfig.Artifacts = maps.Clone(w.artifacts)
	w.WorkerConfig.Imports = maps.Clone(w.WorkerConfig.Imports)
	if deferred {
		w.WorkerConfig.DeferredExpression = deferredExpression(block)
	}
	return nil
}

func (w *WorkerBlock) ExecuteDuringPlan() error {
	return nil
}

func (w *WorkerBlock) contextWithBase(base *hcl.EvalContext) *hcl.EvalContext {
	if base == nil {
		base = &hcl.EvalContext{}
	}
	return w.addWorkerLocals(base)
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
		case "artifact", "import_artifact", "gatekeeper", "qc", "fallback", "tool_use", "retry":
			if nested.Type != "qc" && nested.Type != "retry" && len(nested.Labels) != 1 {
				return fail("worker %s block must have exactly one name", nested.Type)
			}
			if nested.Type == "qc" || nested.Type == "retry" {
				if len(nested.Labels) != 0 {
					return fail("worker %s block does not accept labels", nested.Type)
				}
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

func (w *WorkerBlock) decodeNestedDeclarations(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	paths := map[string]string{}
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "artifact":
			artifact, err := decodeArtifact(nested, ctx)
			if err != nil {
				return err
			}
			if _, exists := w.artifacts[artifact.Name]; exists {
				return fail("worker %q artifact %q is declared more than once", w.Name(), artifact.Name)
			}
			if other, exists := paths[artifact.Path]; exists {
				return fail("artifact %q path conflicts with %q", artifact.Name, other)
			}
			paths[artifact.Path] = artifact.Name
			w.artifacts[artifact.Name] = artifact
		case "import_artifact":
			imported, err := decodeImport(nested, ctx)
			if err != nil {
				return err
			}
			imported.Path = effectiveImportPath(imported)
			if other, exists := paths[imported.Path]; exists {
				return fail("import_artifact %q path conflicts with %q", imported.Name, other)
			}
			paths[imported.Path] = imported.Name
			w.WorkerConfig.Imports = ensureImports(w.WorkerConfig.Imports)
			w.WorkerConfig.Imports[imported.Name] = imported
			w.importPaths[imported.Name] = cty.ObjectVal(map[string]cty.Value{
				"path": cty.StringVal(imported.Path),
				"desc": cty.StringVal(imported.Desc),
			})
		}
	}
	return nil
}

func ensureImports(imports map[string]ImportArtifact) map[string]ImportArtifact {
	if imports == nil {
		return map[string]ImportArtifact{}
	}
	return imports
}

func decodeArtifact(block *golden.HclBlock, ctx *hcl.EvalContext) (Artifact, error) {
	artifact := Artifact{Name: block.Labels[0]}
	var err error
	if artifact.Type, err = requiredString(block, ctx, "type"); err != nil {
		return Artifact{}, err
	}
	if artifact.Type != "file" && artifact.Type != "directory" {
		return Artifact{}, fail("artifact %q type must be file or directory", artifact.Name)
	}
	if artifact.Path, err = requiredString(block, ctx, "path"); err != nil {
		return Artifact{}, err
	}
	if err := validateWorkspaceRelativePath(artifact.Path, "artifact path"); err != nil {
		return Artifact{}, err
	}
	if artifact.Description, err = requiredString(block, ctx, "description"); err != nil {
		return Artifact{}, err
	}
	if attr, ok := block.Attributes()["required"]; ok {
		artifact.Required, err = evaluateBool(attr, ctx)
		if err != nil {
			return Artifact{}, err
		}
	}
	if attr, ok := block.Attributes()["non_empty"]; ok {
		artifact.NonEmpty, err = evaluateBool(attr, ctx)
		if err != nil {
			return Artifact{}, err
		}
	}
	return artifact, nil
}

func decodeImport(block *golden.HclBlock, ctx *hcl.EvalContext) (ImportArtifact, error) {
	imported := ImportArtifact{Name: block.Labels[0]}
	imported.Desc = optionalString(block, ctx, "desc")
	if pathAttr, ok := block.Attributes()["path"]; ok {
		value, diags := pathAttr.Expr.Value(ctx)
		if diags.HasErrors() {
			return ImportArtifact{}, diags
		}
		value, _ = value.UnmarkDeep()
		if !value.IsKnown() || value.IsNull() || !value.Type().Equals(cty.String) {
			return ImportArtifact{}, fail("import_artifact %q path must be a known string", imported.Name)
		}
		imported.Path = value.AsString()
		if err := validateWorkspaceRelativePath(imported.Path, "import_artifact path"); err != nil {
			return ImportArtifact{}, err
		}
	}
	sources, ok := block.Attributes()["sources"]
	if !ok {
		return ImportArtifact{}, fail("import_artifact %q sources is required", imported.Name)
	}
	value, diags := sources.Expr.Value(ctx)
	if diags.HasErrors() {
		return ImportArtifact{}, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if !unmarked.IsNull() && !unmarked.IsWhollyKnown() && !unmarked.Type().IsTupleType() && !unmarked.Type().IsListType() {
		return ImportArtifact{}, fail("import_artifact %q sources must be a list", imported.Name)
	}
	imported.Sources = value
	return imported, nil
}

func (w *WorkerBlock) decodeCore(block *golden.HclBlock, ctx *hcl.EvalContext, deferred *bool) error {
	var err error
	if w.WorkerConfig.BasedSource, err = requiredStringValue(block, ctx, "based_source", true); err != nil {
		return err
	}
	if knownString(w.WorkerConfig.BasedSource) {
		if err := corespec.ValidateBranchName(knownStringValue(w.WorkerConfig.BasedSource)); err != nil {
			return fmt.Errorf("worker %q based_source: %w", w.Name(), err)
		}
	} else {
		*deferred = true
	}
	if w.WorkerConfig.Sandbox, err = requiredObjectValue(block, ctx, "sandbox"); err != nil {
		return err
	}
	if w.WorkerConfig.Harness, err = requiredObjectValue(block, ctx, "harness"); err != nil {
		return err
	}
	w.WorkerConfig.ModelProvider = optionalObjectValue(block, ctx, "model_provider")
	if w.WorkerConfig.Model, err = requiredKnownString(block, ctx, "model"); err != nil {
		return err
	}
	w.WorkerConfig.Profile = optionalString(block, ctx, "profile")
	w.WorkerConfig.ReasoningEffort = optionalString(block, ctx, "reasoning_effort")
	if w.WorkerConfig.SystemPrompt, err = requiredStringValue(block, ctx, "system_prompt", true); err != nil {
		return err
	}
	if !w.WorkerConfig.SystemPrompt.IsWhollyKnown() {
		*deferred = true
	}
	w.WorkerConfig.Prompt = optionalStringValue(block, ctx, "prompt")
	if w.WorkerConfig.Prompt != cty.NilVal && !w.WorkerConfig.Prompt.IsWhollyKnown() {
		*deferred = true
	}
	if w.WorkerConfig.WritablePaths, err = optionalStringList(block, ctx, "writable_paths"); err != nil {
		return err
	}
	for _, candidate := range w.WorkerConfig.WritablePaths {
		if err := validateWorkspaceRelativePath(candidate, "writable path"); err != nil {
			return err
		}
	}
	w.WorkerConfig.HumanApproval = optionalString(block, ctx, "human_approval")
	if w.WorkerConfig.HumanApproval == "" {
		w.WorkerConfig.HumanApproval = "optional"
	}
	if w.WorkerConfig.HumanApproval != "optional" && w.WorkerConfig.HumanApproval != "required" && w.WorkerConfig.HumanApproval != "disabled" {
		return fail("worker %q human_approval is invalid", w.Name())
	}
	w.WorkerConfig.CheckpointRounds = optionalInt(block, ctx, "checkpoint_every_rounds", 5)
	if w.WorkerConfig.CheckpointRounds <= 0 {
		return fail("worker %q checkpoint_every_rounds must be positive", w.Name())
	}
	for _, field := range []string{"tool_ids", "mcp_tool_ids", "mcp_resource_ids"} {
		values, err := optionalStringList(block, ctx, field)
		if err != nil {
			return err
		}
		switch field {
		case "tool_ids":
			w.WorkerConfig.ToolIDs = values
		case "mcp_tool_ids":
			w.WorkerConfig.MCPToolIDs = values
		case "mcp_resource_ids":
			w.WorkerConfig.MCPResourceIDs = values
		}
	}
	w.WorkerConfig.TerminateToolID = optionalString(block, ctx, "terminate_tool_id")
	return nil
}

func (w *WorkerBlock) decodeNestedExecution(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	artifactNames := map[string]struct{}{}
	for name := range w.artifacts {
		artifactNames[name] = struct{}{}
	}
	fallbackNames := map[string]struct{}{}
	triggerTargets := map[string]string{}
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "gatekeeper":
			gatekeeper, err := decodeGatekeeper(nested, ctx)
			if err != nil {
				return err
			}
			w.WorkerConfig.Gatekeepers = append(w.WorkerConfig.Gatekeepers, gatekeeper)
		case "qc":
			if w.WorkerConfig.QC != nil {
				return fail("worker %q supports at most one qc block", w.Name())
			}
			qc, err := decodeQC(nested, ctx)
			if err != nil {
				return err
			}
			w.WorkerConfig.QC = &qc
		case "fallback":
			fallback, err := decodeFallback(nested, ctx, artifactNames)
			if err != nil {
				return err
			}
			if _, exists := fallbackNames[fallback.Name]; exists {
				return fail("fallback %q is declared more than once", fallback.Name)
			}
			fallbackNames[fallback.Name] = struct{}{}
			for _, trigger := range fallback.On {
				if previous, exists := triggerTargets[trigger]; exists && previous != fallback.Name {
					return fail("fallback trigger %q is bound to both %q and %q", trigger, previous, fallback.Name)
				}
				triggerTargets[trigger] = fallback.Name
			}
			w.WorkerConfig.Fallbacks = append(w.WorkerConfig.Fallbacks, fallback)
		case "tool_use":
			toolUse, err := decodeToolUse(nested, ctx)
			if err != nil {
				return err
			}
			w.WorkerConfig.ToolUses = append(w.WorkerConfig.ToolUses, toolUse)
		}
	}
	if len(w.WorkerConfig.ToolUses) > 0 && (len(w.WorkerConfig.ToolIDs) > 0 || w.WorkerConfig.TerminateToolID != "") {
		return fail("worker %q tool_use is mutually exclusive with tool_ids and terminate_tool_id", w.Name())
	}
	terminating := 0
	for _, use := range w.WorkerConfig.ToolUses {
		if use.Terminate {
			terminating++
		}
	}
	if w.WorkerConfig.TerminateToolID != "" {
		terminating++
	}
	if terminating > 1 {
		return fail("worker %q may declare at most one terminating tool", w.Name())
	}
	return nil
}

func decodeGatekeeper(block *golden.HclBlock, ctx *hcl.EvalContext) (GatekeeperConfig, error) {
	gatekeeper := GatekeeperBlock{Name: block.Labels[0]}
	var err error
	if gatekeeper.Command, err = requiredStringList(block, ctx, "command"); err != nil {
		return GatekeeperConfig{}, err
	}
	gatekeeper.WorkingDir = optionalString(block, ctx, "working_dir")
	gatekeeper.Timeout = optionalString(block, ctx, "timeout")
	gatekeeper.ExpectExitCode = optionalInt(block, ctx, "expect_exit_code", 0)
	if attr, ok := block.Attributes()["require_clean_worktree"]; ok {
		value, err := evaluateBool(attr, ctx)
		if err != nil {
			return GatekeeperConfig{}, err
		}
		gatekeeper.RequireCleanTree = &value
	}
	config := gatekeeper.Config()
	if err := validateGatekeeper(config); err != nil {
		return GatekeeperConfig{}, err
	}
	return config, nil
}

func decodeQC(block *golden.HclBlock, ctx *hcl.EvalContext) (QCConfig, error) {
	criteriaAttr, ok := block.Attributes()["criteria"]
	if !ok {
		return QCConfig{}, fail("qc criteria is required")
	}
	criteria, diags := criteriaAttr.Expr.Value(ctx)
	if diags.HasErrors() {
		return QCConfig{}, diags
	}
	criteria, _ = criteria.UnmarkDeep()
	if !criteria.IsWhollyKnown() || criteria.IsNull() || !criteria.Type().IsMapType() && !criteria.Type().IsObjectType() {
		return QCConfig{}, fail("qc criteria must be a known non-empty map(string)")
	}
	if criteria.LengthInt() == 0 {
		return QCConfig{}, fail("qc criteria must be a known non-empty map(string)")
	}
	for iterator := criteria.ElementIterator(); iterator.Next(); {
		key, value := iterator.Element()
		if value.IsNull() || !value.Type().Equals(cty.String) || strings.TrimSpace(value.AsString()) == "" ||
			strings.TrimSpace(key.AsString()) == "" {
			return QCConfig{}, fail("qc criteria keys and values must be non-empty strings")
		}
	}
	result := QCConfig{Criteria: criteria, MaxQCRounds: optionalInt(block, ctx, "max_qc_rounds", 3)}
	if result.MaxQCRounds <= 0 {
		return QCConfig{}, fail("qc max_qc_rounds must be positive")
	}
	result.Model = optionalString(block, ctx, "model")
	result.ReasoningEffort = optionalString(block, ctx, "reasoning_effort")
	result.ToolIDs, _ = optionalStringList(block, ctx, "tool_ids")
	result.SkillDirectories, _ = optionalStringList(block, ctx, "skill_directories")
	result.Skills, _ = optionalStringList(block, ctx, "skills")
	result.DisabledSkills, _ = optionalStringList(block, ctx, "disabled_skills")
	if attr, ok := block.Attributes()["harness"]; ok {
		result.Harness = evalAttr(attr, ctx)
	}
	if attr, ok := block.Attributes()["model_provider"]; ok {
		result.ModelProvider = evalAttr(attr, ctx)
	}
	return result, nil
}

func decodeFallback(block *golden.HclBlock, ctx *hcl.EvalContext, artifacts map[string]struct{}) (FallbackConfig, error) {
	fallback := FallbackConfig{Name: block.Labels[0], MaxReworks: 2}
	var err error
	if fallback.On, err = requiredStringList(block, ctx, "on"); err != nil {
		return FallbackConfig{}, err
	}
	allowed := map[string]struct{}{"qc_exhausted": {}, "gatekeeper_exhausted": {}, "human_returned_upstream": {}, "agent_escalated": {}}
	for _, trigger := range fallback.On {
		if _, ok := allowed[trigger]; !ok {
			return FallbackConfig{}, fail("fallback %q has invalid trigger %q", fallback.Name, trigger)
		}
	}
	targetAttr, ok := block.Attributes()["target"]
	if !ok {
		return FallbackConfig{}, fail("fallback %q target is required", fallback.Name)
	}
	traversal, diags := hcl.AbsTraversalForExpr(targetAttr.Expr)
	if diags.HasErrors() {
		return FallbackConfig{}, fail("fallback %q target must be a block traversal", fallback.Name)
	}
	fallback.Target = traversalAddress(traversal)
	fallback.MaxReworks = optionalInt(block, ctx, "max_reworks", 2)
	if fallback.MaxReworks <= 0 {
		return FallbackConfig{}, fail("fallback max_reworks must be positive")
	}
	if fallback.Artifacts, err = optionalStringList(block, ctx, "artifacts"); err != nil {
		return FallbackConfig{}, err
	}
	for _, name := range fallback.Artifacts {
		if _, ok := artifacts[name]; !ok {
			return FallbackConfig{}, fail("fallback %q references unknown artifact %q", fallback.Name, name)
		}
	}
	return fallback, nil
}

func decodeToolUse(block *golden.HclBlock, ctx *hcl.EvalContext) (ToolUse, error) {
	toolUse := ToolUse{Name: block.Labels[0]}
	var err error
	if toolUse.ToolID, err = requiredKnownString(block, ctx, "tool_id"); err != nil {
		return ToolUse{}, err
	}
	if attr, ok := block.Attributes()["terminate"]; ok {
		toolUse.Terminate, err = evaluateBool(attr, ctx)
		if err != nil {
			return ToolUse{}, err
		}
	}
	if attr, ok := block.Attributes()["input"]; ok {
		toolUse.Input = evalAttr(attr, ctx)
	} else {
		toolUse.Input = cty.EmptyObjectVal
	}
	return toolUse, nil
}

func requiredString(block *golden.HclBlock, ctx *hcl.EvalContext, name string) (string, error) {
	value, err := requiredStringValue(block, ctx, name, false)
	if err != nil {
		return "", err
	}
	return knownStringValue(value), nil
}

func requiredKnownString(block *golden.HclBlock, ctx *hcl.EvalContext, name string) (string, error) {
	value, err := requiredStringValue(block, ctx, name, false)
	if err != nil {
		return "", err
	}
	if !value.IsWhollyKnown() {
		return "", fail("%s must be known", name)
	}
	return knownStringValue(value), nil
}

func requiredStringValue(block *golden.HclBlock, ctx *hcl.EvalContext, name string, allowUnknown bool) (cty.Value, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NilVal, fail("%s is required", name)
	}
	value := evalAttr(attr, ctx)
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.Type().Equals(cty.String) {
		return cty.NilVal, fail("%s must be a string", name)
	}
	if !allowUnknown && !unmarked.IsWhollyKnown() {
		return cty.NilVal, fail("%s must be known", name)
	}
	return value, nil
}

func optionalStringValue(block *golden.HclBlock, ctx *hcl.EvalContext, name string) cty.Value {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NilVal
	}
	return evalAttr(attr, ctx)
}

func optionalString(block *golden.HclBlock, ctx *hcl.EvalContext, name string) string {
	value := optionalStringValue(block, ctx, name)
	if value == cty.NilVal {
		return ""
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return ""
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return ""
	}
	return unmarked.AsString()
}

func requiredObjectValue(block *golden.HclBlock, ctx *hcl.EvalContext, name string) (cty.Value, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NilVal, fail("%s is required", name)
	}
	value := evalAttr(attr, ctx)
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.Type().IsObjectType() {
		return cty.NilVal, fail("%s must reference an object", name)
	}
	return value, nil
}

func optionalObjectValue(block *golden.HclBlock, ctx *hcl.EvalContext, name string) cty.Value {
	attr, ok := block.Attributes()[name]
	if !ok {
		return cty.NullVal(cty.EmptyObject)
	}
	return evalAttr(attr, ctx)
}

func requiredStringList(block *golden.HclBlock, ctx *hcl.EvalContext, name string) ([]string, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return nil, fail("%s is required", name)
	}
	return stringListValue(attr, ctx, name, false)
}

func optionalStringList(block *golden.HclBlock, ctx *hcl.EvalContext, name string) ([]string, error) {
	attr, ok := block.Attributes()[name]
	if !ok {
		return []string{}, nil
	}
	return stringListValue(attr, ctx, name, true)
}

func stringListValue(attr *golden.HclAttribute, ctx *hcl.EvalContext, name string, allowNull bool) ([]string, error) {
	value := evalAttr(attr, ctx)
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() && allowNull {
		return []string{}, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, fail("%s must be wholly known", name)
	}
	converted, err := convert.Convert(unmarked, cty.List(cty.String))
	if err != nil {
		return nil, fail("%s must be a list(string)", name)
	}
	result := make([]string, 0, converted.LengthInt())
	for iterator := converted.ElementIterator(); iterator.Next(); {
		_, item := iterator.Element()
		if item.IsNull() {
			return nil, fail("%s must not contain null elements", name)
		}
		result = append(result, item.AsString())
	}
	return result, nil
}

func optionalInt(block *golden.HclBlock, ctx *hcl.EvalContext, name string, defaultValue int) int {
	attr, ok := block.Attributes()[name]
	if !ok {
		return defaultValue
	}
	value := evalAttr(attr, ctx)
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.Type().Equals(cty.Number) {
		return defaultValue
	}
	number, _ := unmarked.AsBigFloat().Int64()
	return int(number)
}

func evaluateBool(attr *golden.HclAttribute, ctx *hcl.EvalContext) (bool, error) {
	value := evalAttr(attr, ctx)
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.Bool) {
		return false, fail("expected a known bool")
	}
	return unmarked.True(), nil
}

func evalAttr(attr *golden.HclAttribute, ctx *hcl.EvalContext) cty.Value {
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return cty.DynamicVal
	}
	return value
}

func knownString(value cty.Value) bool {
	unmarked, _ := value.UnmarkDeep()
	return !unmarked.IsNull() && unmarked.IsWhollyKnown() && unmarked.Type().Equals(cty.String)
}

func knownStringValue(value cty.Value) string {
	unmarked, _ := value.UnmarkDeep()
	return unmarked.AsString()
}

func traversalAddress(traversal hcl.Traversal) string {
	var result strings.Builder
	result.WriteString(traversal.RootName())
	for _, step := range traversal[1:] {
		if attr, ok := step.(hcl.TraverseAttr); ok {
			result.WriteString(".")
			result.WriteString(attr.Name)
		}
	}
	return result.String()
}

func deferredExpression(block *golden.HclBlock) string {
	var builder strings.Builder
	builder.WriteString("{\n")
	keys := make([]string, 0, len(block.Attributes()))
	for name := range block.Attributes() {
		if !golden.MetaAttributeNames.Contains(name) {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	for _, name := range keys {
		fmt.Fprintf(&builder, "%s = %s\n", name, block.Attributes()[name].ExprString())
	}
	builder.WriteString("}\n")
	return builder.String()
}

var _ golden.CustomDecode = (*WorkerBlock)(nil)
var _ golden.PlanBlock = (*WorkerBlock)(nil)
var _ golden.Valuable = (*WorkerBlock)(nil)
