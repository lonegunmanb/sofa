package spec

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/lonegunmanb/sofa/internal/tool/gotool"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

var (
	_ golden.PlanBlock        = (*GoToolBlock)(nil)
	_ golden.CustomDecode     = (*GoToolBlock)(nil)
	_ golden.SingleValueBlock = (*GoToolBlock)(nil)
	_ golden.PlanBlock        = (*ExternalToolBlock)(nil)
	_ golden.CustomDecode     = (*ExternalToolBlock)(nil)
	_ golden.SingleValueBlock = (*ExternalToolBlock)(nil)
)

// GoToolConfig is a deep-copied snapshot of a planned GoToolBlock.
type GoToolConfig struct {
	ID               string
	Name             string
	Address          string
	CanonicalAddress string
	Description      string
	Source           string
	Dependencies     []string
	Postconditions   []corespec.Condition
	InputConstraint  Constraint
	OutputConstraint Constraint
	QuotePaths       [][]string
}

// ExternalToolConfig is a deep-copied snapshot of a planned ExternalToolBlock.
type ExternalToolConfig struct {
	ID               string
	Name             string
	Address          string
	CanonicalAddress string
	Description      string
	Program          []string
	WorkingDir       string
	Postconditions   []corespec.Condition
	InputConstraint  Constraint
	OutputConstraint Constraint
}

// GoToolBlock represents an inline Go tool block (`go_tool "<name>"`).
type GoToolBlock struct {
	*golden.BaseBlock
	Description    string
	Source         string
	Dependencies   []string
	Postconditions []corespec.Condition

	descriptionValue  cty.Value
	sourceValue       cty.Value
	dependenciesValue cty.Value
	planned           cty.Value

	inputConstraint  Constraint
	outputConstraint Constraint
	quotePaths       [][]string
}

func (*GoToolBlock) Type() string            { return "" }
func (*GoToolBlock) BlockType() string       { return "go_tool" }
func (*GoToolBlock) AddressLength() int      { return 2 }
func (*GoToolBlock) CanExecutePrePlan() bool { return false }

// CanonicalAddress returns the canonical module-scoped HCL address for this block.
func (b *GoToolBlock) CanonicalAddress() string { return canonicalAddress(b.BaseBlock) }

// Id returns the deterministic tool identifier capped at 64 characters.
func (b *GoToolBlock) Id() string { return typedToolID(b.BaseBlock, b.BlockType()) }

// BaseValues overrides golden's random UUID with the deterministic tool ID.
func (b *GoToolBlock) BaseValues() map[string]cty.Value {
	return typedToolBaseValues(b.BaseBlock, b.Id())
}

// InputConstraint returns the analyzed Go Input struct constraint.
func (b *GoToolBlock) InputConstraint() Constraint { return b.inputConstraint }

// OutputConstraint returns the analyzed Go Output type constraint.
func (b *GoToolBlock) OutputConstraint() Constraint { return b.outputConstraint }

// QuotePaths returns a deep copy of input field paths whose Go type is Quote.
func (b *GoToolBlock) QuotePaths() [][]string { return cloneQuotePaths(b.quotePaths) }

// GoToolConfig returns a deep-copied immutable configuration snapshot.
func (b *GoToolBlock) GoToolConfig() GoToolConfig {
	return GoToolConfig{
		ID:               b.Id(),
		Name:             b.Name(),
		Address:          b.Address(),
		CanonicalAddress: b.CanonicalAddress(),
		Description:      b.Description,
		Source:           b.Source,
		Dependencies:     slices.Clone(b.Dependencies),
		Postconditions:   slices.Clone(b.Postconditions),
		InputConstraint:  b.inputConstraint,
		OutputConstraint: b.outputConstraint,
		QuotePaths:       cloneQuotePaths(b.quotePaths),
	}
}

// Value exposes the block's planned HCL object value, or cty.DynamicVal pre-plan.
func (b *GoToolBlock) Value() cty.Value {
	if b.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return b.planned
}

func (b *GoToolBlock) reset() {
	b.Description = ""
	b.Source = ""
	b.Dependencies = nil
	b.Postconditions = nil
	b.descriptionValue = cty.NilVal
	b.sourceValue = cty.NilVal
	b.dependenciesValue = cty.NilVal
	b.planned = cty.NilVal
	b.inputConstraint = Constraint{}
	b.outputConstraint = Constraint{}
	b.quotePaths = nil
}

// Decode parses, validates, and statically analyzes the go_tool declaration.
func (b *GoToolBlock) Decode(block *golden.HclBlock, evalCtx *hcl.EvalContext) error {
	b.reset()
	if err := validateToolBlockSchema(block, map[string]struct{}{
		"description":  {},
		"source":       {},
		"dependencies": {},
	}); err != nil {
		return err
	}
	description, descriptionVal, err := decodeRequiredString(block, evalCtx, "go tool", "description")
	if err != nil {
		return err
	}
	source, sourceVal, err := decodeRequiredString(block, evalCtx, "go tool", "source")
	if err != nil {
		return err
	}
	dependencies, dependenciesVal, err := decodeStringList(block, evalCtx, "go tool", "dependencies", false)
	if err != nil {
		return err
	}
	for _, dep := range dependencies {
		if strings.TrimSpace(dep) == "" {
			return fmt.Errorf("go tool dependencies must not contain empty entries")
		}
	}

	analysis, err := gotool.AnalyzeWithDependencies(source, dependencies)
	if err != nil {
		return fmt.Errorf("go tool source analysis: %w", err)
	}

	postconditions, err := decodePostconditions(
		block,
		evalCtx,
		cty.UnknownVal(analysis.InputType),
		cty.UnknownVal(analysis.OutputType),
	)
	if err != nil {
		return fmt.Errorf("go tool postcondition: %w", err)
	}

	b.Description = description
	b.Source = source
	b.Dependencies = dependencies
	b.Postconditions = postconditions
	b.descriptionValue = descriptionVal
	b.sourceValue = sourceVal
	b.dependenciesValue = dependenciesVal
	b.inputConstraint = NewConstraint(analysis.InputType)
	b.outputConstraint = NewConstraint(analysis.OutputType)
	b.quotePaths = analysis.QuotePaths
	return nil
}

// ExecuteDuringPlan finalizes plan-time validation and publishes Value().
func (b *GoToolBlock) ExecuteDuringPlan() error {
	if strings.TrimSpace(b.Description) == "" {
		return fmt.Errorf("go tool description is required")
	}
	if strings.TrimSpace(b.Source) == "" {
		return fmt.Errorf("go tool source is required")
	}
	b.planned = cty.ObjectVal(map[string]cty.Value{
		"id":           cty.StringVal(b.Id()),
		"address":      cty.StringVal(b.Address()),
		"kind":         cty.StringVal("go"),
		"description":  b.descriptionValue,
		"source":       b.sourceValue,
		"dependencies": b.dependenciesValue,
	})
	return nil
}

// ExternalToolBlock represents an external JSON-protocol tool block (`external_tool "<name>"`).
type ExternalToolBlock struct {
	*golden.BaseBlock
	Description    string
	Program        []string
	WorkingDir     string
	Postconditions []corespec.Condition

	descriptionValue cty.Value
	programValue     cty.Value
	workingDirValue  cty.Value
	planned          cty.Value

	inputConstraint  Constraint
	outputConstraint Constraint
}

func (*ExternalToolBlock) Type() string            { return "" }
func (*ExternalToolBlock) BlockType() string       { return "external_tool" }
func (*ExternalToolBlock) AddressLength() int      { return 2 }
func (*ExternalToolBlock) CanExecutePrePlan() bool { return false }

// CanonicalAddress returns the canonical module-scoped HCL address for this block.
func (b *ExternalToolBlock) CanonicalAddress() string { return canonicalAddress(b.BaseBlock) }

// Id returns the deterministic tool identifier capped at 64 characters.
func (b *ExternalToolBlock) Id() string { return typedToolID(b.BaseBlock, b.BlockType()) }

// BaseValues overrides golden's random UUID with the deterministic tool ID.
func (b *ExternalToolBlock) BaseValues() map[string]cty.Value {
	return typedToolBaseValues(b.BaseBlock, b.Id())
}

// InputConstraint returns the decoded object constraint for tool inputs.
func (b *ExternalToolBlock) InputConstraint() Constraint { return b.inputConstraint }

// OutputConstraint returns the decoded constraint for tool outputs.
func (b *ExternalToolBlock) OutputConstraint() Constraint { return b.outputConstraint }

// ExternalToolConfig returns a deep-copied immutable configuration snapshot.
func (b *ExternalToolBlock) ExternalToolConfig() ExternalToolConfig {
	return ExternalToolConfig{
		ID:               b.Id(),
		Name:             b.Name(),
		Address:          b.Address(),
		CanonicalAddress: b.CanonicalAddress(),
		Description:      b.Description,
		Program:          slices.Clone(b.Program),
		WorkingDir:       b.WorkingDir,
		Postconditions:   slices.Clone(b.Postconditions),
		InputConstraint:  b.inputConstraint,
		OutputConstraint: b.outputConstraint,
	}
}

// Value exposes the block's planned HCL object value, or cty.DynamicVal pre-plan.
func (b *ExternalToolBlock) Value() cty.Value {
	if b.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return b.planned
}

func (b *ExternalToolBlock) reset() {
	b.Description = ""
	b.Program = nil
	b.WorkingDir = ""
	b.Postconditions = nil
	b.descriptionValue = cty.NilVal
	b.programValue = cty.NilVal
	b.workingDirValue = cty.NilVal
	b.planned = cty.NilVal
	b.inputConstraint = Constraint{}
	b.outputConstraint = Constraint{}
}

// Decode parses and validates the external_tool declaration and its postconditions.
func (b *ExternalToolBlock) Decode(block *golden.HclBlock, evalCtx *hcl.EvalContext) error {
	b.reset()
	if err := validateToolBlockSchema(block, map[string]struct{}{
		"description": {},
		"program":     {},
		"working_dir": {},
		"input_type":  {},
		"output_type": {},
	}); err != nil {
		return err
	}

	description, descriptionVal, err := decodeRequiredString(block, evalCtx, "external tool", "description")
	if err != nil {
		return err
	}
	program, programVal, err := decodeStringList(block, evalCtx, "external tool", "program", true)
	if err != nil {
		return err
	}
	workingDir, workingDirVal, err := decodeOptionalString(block, evalCtx, "external tool", "working_dir")
	if err != nil {
		return err
	}
	inputConstraint, err := decodeConstraint(block, "input_type")
	if err != nil {
		return fmt.Errorf("external tool %w", err)
	}
	if !inputConstraint.Type().IsObjectType() {
		return fmt.Errorf("external tool input_type must be an object type")
	}
	outputConstraint, err := decodeConstraint(block, "output_type")
	if err != nil {
		return fmt.Errorf("external tool %w", err)
	}

	postconditions, err := decodePostconditions(
		block,
		evalCtx,
		cty.UnknownVal(inputConstraint.Type()),
		cty.UnknownVal(outputConstraint.Type()),
	)
	if err != nil {
		return fmt.Errorf("external tool postcondition: %w", err)
	}

	b.Description = description
	b.Program = program
	b.WorkingDir = workingDir
	b.Postconditions = postconditions
	b.descriptionValue = descriptionVal
	b.programValue = programVal
	b.workingDirValue = workingDirVal
	b.inputConstraint = inputConstraint
	b.outputConstraint = outputConstraint
	return nil
}

// ExecuteDuringPlan finalizes plan-time validation without probing host executables.
func (b *ExternalToolBlock) ExecuteDuringPlan() error {
	if strings.TrimSpace(b.Description) == "" {
		return fmt.Errorf("external tool description is required")
	}
	if len(b.Program) == 0 || strings.TrimSpace(b.Program[0]) == "" {
		return fmt.Errorf("external tool program must contain an executable")
	}
	b.planned = cty.ObjectVal(map[string]cty.Value{
		"id":          cty.StringVal(b.Id()),
		"address":     cty.StringVal(b.Address()),
		"kind":        cty.StringVal("external"),
		"description": b.descriptionValue,
		"program":     b.programValue,
		"working_dir": b.workingDirValue,
	})
	return nil
}

func validateToolBlockSchema(block *golden.HclBlock, allowedAttributes map[string]struct{}) error {
	for name := range block.Attributes() {
		if _, ok := allowedAttributes[name]; ok || golden.MetaAttributeNames.Contains(name) {
			continue
		}
		return fmt.Errorf("unsupported argument; An argument named %q is not expected here", name)
	}
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "postcondition" {
			continue
		}
		if nested.Type == "dynamic" {
			if len(nested.Labels) != 1 {
				return fmt.Errorf("dynamic block must declare a single target block label")
			}
			if nested.Labels[0] != "postcondition" {
				return fmt.Errorf("unsupported block type; Blocks of type %q are not expected here", nested.Labels[0])
			}
			continue
		}
		if golden.MetaNestedBlockNames.Contains(nested.Type) {
			continue
		}
		return fmt.Errorf("unsupported block type; Blocks of type %q are not expected here", nested.Type)
	}
	return nil
}

func decodeRequiredString(
	block *golden.HclBlock,
	evalCtx *hcl.EvalContext,
	toolLabel string,
	name string,
) (string, cty.Value, error) {
	attribute, ok := block.Attributes()[name]
	if !ok {
		return "", cty.NilVal, fmt.Errorf("%s %s is required", toolLabel, name)
	}
	value, diagnostics := attribute.Expr.Value(evalCtx)
	if diagnostics.HasErrors() {
		return "", cty.NilVal, diagnostics
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", cty.NilVal, fmt.Errorf("%s %s must be a known string", toolLabel, name)
	}
	return unmarked.AsString(), value, nil
}

func decodeOptionalString(
	block *golden.HclBlock,
	evalCtx *hcl.EvalContext,
	toolLabel string,
	name string,
) (string, cty.Value, error) {
	attribute, ok := block.Attributes()[name]
	if !ok {
		return "", cty.StringVal(""), nil
	}
	value, diagnostics := attribute.Expr.Value(evalCtx)
	if diagnostics.HasErrors() {
		return "", cty.NilVal, diagnostics
	}
	_, outerMarks := value.Unmark()
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return "", cty.StringVal("").WithMarks(outerMarks), nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", cty.NilVal, fmt.Errorf("%s %s must be a known string or null", toolLabel, name)
	}
	return unmarked.AsString(), value, nil
}

func decodeStringList(
	block *golden.HclBlock,
	evalCtx *hcl.EvalContext,
	toolLabel string,
	name string,
	required bool,
) ([]string, cty.Value, error) {
	attribute, ok := block.Attributes()[name]
	if !ok {
		if required {
			return nil, cty.NilVal, fmt.Errorf("%s %s is required", toolLabel, name)
		}
		return []string{}, cty.ListValEmpty(cty.String), nil
	}
	value, diagnostics := attribute.Expr.Value(evalCtx)
	if diagnostics.HasErrors() {
		return nil, cty.NilVal, diagnostics
	}
	_, outerMarks := value.Unmark()
	unmarkedDeep, _ := value.UnmarkDeep()
	if unmarkedDeep.IsNull() {
		if required {
			return nil, cty.NilVal, fmt.Errorf("%s %s must be a list of strings", toolLabel, name)
		}
		return []string{}, cty.ListValEmpty(cty.String).WithMarks(outerMarks), nil
	}
	if !unmarkedDeep.IsWhollyKnown() {
		return nil, cty.NilVal, fmt.Errorf("%s %s must be wholly known", toolLabel, name)
	}
	unmarkedList, err := convert.Convert(unmarkedDeep, cty.List(cty.String))
	if err != nil {
		return nil, cty.NilVal, fmt.Errorf("%s %s must be a list of strings: %w", toolLabel, name, err)
	}
	markedList, err := convert.Convert(value, cty.List(cty.String))
	if err != nil {
		return nil, cty.NilVal, fmt.Errorf("%s %s must be a list of strings: %w", toolLabel, name, err)
	}

	result := make([]string, 0, unmarkedList.LengthInt())
	for iterator := unmarkedList.ElementIterator(); iterator.Next(); {
		_, element := iterator.Element()
		if element.IsNull() {
			return nil, cty.NilVal, fmt.Errorf("%s %s must not contain null elements", toolLabel, name)
		}
		result = append(result, element.AsString())
	}
	return result, markedList, nil
}

func decodeConstraint(block *golden.HclBlock, name string) (Constraint, error) {
	attribute, ok := block.Attributes()[name]
	if !ok {
		return Constraint{}, fmt.Errorf("%s is required", name)
	}
	typeValue, defaults, diagnostics := typeexpr.TypeConstraintWithDefaults(attribute.Expr)
	if diagnostics.HasErrors() {
		return Constraint{}, fmt.Errorf("%s: %w", name, diagnostics)
	}
	if err := corespec.ValidateType(typeValue); err != nil {
		return Constraint{}, fmt.Errorf("%s: %w", name, err)
	}
	return NewConstraintWithDefaults(typeValue, defaults), nil
}

func decodePostconditions(
	block *golden.HclBlock,
	evalCtx *hcl.EvalContext,
	input cty.Value,
	output cty.Value,
) ([]corespec.Condition, error) {
	var evalFuncs map[string]function.Function
	if evalCtx != nil {
		evalFuncs = evalCtx.Functions
	}
	result := make([]corespec.Condition, 0)
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "postcondition":
			cond, err := decodeSinglePostcondition(nested, evalCtx, input, output, evalFuncs)
			if err != nil {
				return nil, err
			}
			result = append(result, cond)
		case "dynamic":
			if len(nested.Labels) != 1 || nested.Labels[0] != "postcondition" {
				continue
			}
			expanded, err := decodeDynamicPostconditions(nested, evalCtx, input, output, evalFuncs)
			if err != nil {
				return nil, err
			}
			result = append(result, expanded...)
		}
	}
	return result, nil
}

func decodeDynamicPostconditions(
	dynamicBlock *golden.HclBlock,
	evalCtx *hcl.EvalContext,
	input cty.Value,
	output cty.Value,
	evalFuncs map[string]function.Function,
) ([]corespec.Condition, error) {
	for name := range dynamicBlock.Attributes() {
		if name != "for_each" && name != "iterator" {
			return nil, fmt.Errorf("unsupported dynamic postcondition argument %q", name)
		}
	}
	forEachAttr, ok := dynamicBlock.Attributes()["for_each"]
	if !ok {
		return nil, fmt.Errorf("dynamic postcondition must declare for_each")
	}
	forEachVal, diagnostics := forEachAttr.Expr.Value(evalCtx)
	if diagnostics.HasErrors() {
		return nil, diagnostics
	}
	forEachUnmarked, _ := forEachVal.UnmarkDeep()
	if forEachUnmarked.IsNull() || !forEachUnmarked.IsKnown() {
		return nil, fmt.Errorf("dynamic postcondition for_each must be a known collection")
	}
	if !forEachUnmarked.CanIterateElements() {
		return nil, fmt.Errorf("incorrect type for for_each, must be a collection")
	}

	iteratorName := "postcondition"
	if iteratorAttr, hasIterator := dynamicBlock.Attributes()["iterator"]; hasIterator {
		traversal, diags := hcl.AbsTraversalForExpr(iteratorAttr.Expr)
		if diags.HasErrors() || len(traversal) != 1 {
			return nil, fmt.Errorf("dynamic postcondition iterator must be a single identifier")
		}
		candidate := traversal.RootName()
		if !hclsyntax.ValidIdentifier(candidate) {
			return nil, fmt.Errorf("dynamic postcondition iterator must be a single identifier")
		}
		if isReservedIteratorName(candidate) {
			return nil, fmt.Errorf("dynamic postcondition iterator %q uses a reserved name", candidate)
		}
		iteratorName = candidate
	}

	nestedBlocks := dynamicBlock.NestedBlocks()
	if len(nestedBlocks) == 0 {
		return nil, fmt.Errorf("dynamic postcondition must contain a content block")
	}
	for _, inner := range nestedBlocks {
		if inner.Type != "content" {
			return nil, fmt.Errorf("dynamic postcondition may only contain content blocks; found %q", inner.Type)
		}
		if len(inner.Labels) != 0 {
			return nil, fmt.Errorf("dynamic postcondition content block must not have labels")
		}
	}

	expanded := make([]corespec.Condition, 0)
	for iterator := forEachUnmarked.ElementIterator(); iterator.Next(); {
		key, value := iterator.Element()
		var childCtx *hcl.EvalContext
		if evalCtx != nil {
			childCtx = evalCtx.NewChild()
		} else {
			childCtx = &hcl.EvalContext{}
		}
		childCtx.Variables = map[string]cty.Value{
			iteratorName: cty.ObjectVal(map[string]cty.Value{
				"key":   key,
				"value": value,
			}),
		}
		for _, contentBlock := range nestedBlocks {
			cond, err := decodeSinglePostcondition(contentBlock, childCtx, input, output, evalFuncs)
			if err != nil {
				return nil, err
			}
			expanded = append(expanded, cond)
		}
	}
	return expanded, nil
}

func decodeSinglePostcondition(
	postBlock *golden.HclBlock,
	evalCtx *hcl.EvalContext,
	input cty.Value,
	output cty.Value,
	evalFuncs map[string]function.Function,
) (corespec.Condition, error) {
	for name := range postBlock.Attributes() {
		if name != "condition" && name != "error_message" {
			return corespec.Condition{}, fmt.Errorf("unsupported postcondition argument %q", name)
		}
	}
	if len(postBlock.NestedBlocks()) != 0 {
		return corespec.Condition{}, fmt.Errorf("postcondition must not contain nested blocks")
	}
	conditionAttr, ok := postBlock.Attributes()["condition"]
	if !ok {
		return corespec.Condition{}, fmt.Errorf("condition is required")
	}
	errorAttr, ok := postBlock.Attributes()["error_message"]
	if !ok {
		return corespec.Condition{}, fmt.Errorf("error_message is required")
	}
	errorValue, diagnostics := errorAttr.Expr.Value(evalCtx)
	if diagnostics.HasErrors() {
		return corespec.Condition{}, diagnostics
	}
	unmarkedErr, _ := errorValue.UnmarkDeep()
	if !unmarkedErr.IsWhollyKnown() || unmarkedErr.IsNull() || !unmarkedErr.Type().Equals(cty.String) {
		return corespec.Condition{}, fmt.Errorf("error_message must be a known string")
	}
	condition := corespec.Condition{
		Expression:   conditionAttr.ExprString(),
		ErrorMessage: unmarkedErr.AsString(),
	}
	if _, err := condition.Evaluate(
		map[string]cty.Value{"input": input, "output": output},
		evalFuncs,
	); err != nil {
		return corespec.Condition{}, err
	}
	return condition, nil
}

func isReservedIteratorName(name string) bool {
	switch name {
	case "input", "output", "var", "local", "path",
		"variable", "locals", "module", "source", "preserve",
		"model_provider", "mcp_server", "sandbox", "harness",
		"go_tool", "external_tool", "worker", "dynamic_worker", "merge":
		return true
	default:
		return false
	}
}

func cloneQuotePaths(paths [][]string) [][]string {
	if len(paths) == 0 {
		return nil
	}
	cloned := make([][]string, len(paths))
	for i, path := range paths {
		cloned[i] = slices.Clone(path)
	}
	return cloned
}
