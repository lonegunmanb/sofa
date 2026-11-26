package spec

import (
	"fmt"
	"maps"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
)

var (
	dynamicWorkerAttributes = map[string]struct{}{
		"parallelism": {},
		"serial":      {},
	}
	dynamicWorkerNestedAttributes = map[string]struct{}{
		"for_each": {},
		"iterator": {},
	}
	reservedDynamicWorkerIterators = map[string]struct{}{
		"var":             {},
		"local":           {},
		"variable":        {},
		"locals":          {},
		"output":          {},
		"module":          {},
		"source":          {},
		"preserve":        {},
		"sandbox":         {},
		"harness":         {},
		"model_provider":  {},
		"mcp_server":      {},
		"go_tool":         {},
		"external_tool":   {},
		"worker":          {},
		"dynamic_worker":  {},
		"merge":           {},
		"path":            {},
		"import_artifact": {},
		"each":            {},
	}
)

type DynamicConfig struct {
	Name            string
	Parallelism     *int
	Serial          bool
	ForEach         cty.Value
	ForEachExpr     string
	IteratorName    string
	Content         Config
	ContentTemplate string
}

func (c DynamicConfig) Clone() DynamicConfig {
	cloned := c
	if c.Parallelism != nil {
		p := *c.Parallelism
		cloned.Parallelism = &p
	}
	cloned.Content = c.Content.Clone()
	return cloned
}

type DynamicWorkerBlock struct {
	*golden.BaseBlock
	planned       DynamicConfig
	virtualWorker *WorkerBlock
	plannedValue  cty.Value
}

func (d *DynamicWorkerBlock) Type() string            { return "" }
func (d *DynamicWorkerBlock) BlockType() string       { return "dynamic_worker" }
func (d *DynamicWorkerBlock) AddressLength() int      { return 2 }
func (d *DynamicWorkerBlock) CanExecutePrePlan() bool { return false }

func (d *DynamicWorkerBlock) DynamicWorkerConfig() DynamicConfig {
	return d.planned.Clone()
}

func (d *DynamicWorkerBlock) Value() cty.Value {
	if d.plannedValue == cty.NilVal {
		return cty.DynamicVal
	}
	return d.plannedValue
}

func (d *DynamicWorkerBlock) Decode(block *golden.HclBlock, baseCtx *hcl.EvalContext) error {
	d.planned = DynamicConfig{}
	d.virtualWorker = nil
	d.plannedValue = cty.NilVal

	if len(block.Labels) != 2 {
		return fail("dynamic_worker block must have exactly one name")
	}
	if err := validateSafeLabel(d.Name(), "dynamic_worker", false); err != nil {
		return err
	}
	scope := fmt.Sprintf("dynamic_worker %q", d.Name())

	dynBlock, err := validateDynamicWorkerSchema(block, scope)
	if err != nil {
		return err
	}
	contentBlock, err := validateDynamicWorkerNestedBlock(dynBlock, scope)
	if err != nil {
		return err
	}

	parallelism, _, err := optionalWholeIntPtr(block, baseCtx, "parallelism", scope)
	if err != nil {
		return err
	}
	if parallelism != nil && *parallelism <= 0 {
		return fail("%s parallelism must be positive", scope)
	}
	serial, err := optionalBool(block, baseCtx, "serial", false, scope)
	if err != nil {
		return err
	}

	iteratorName, err := extractDynamicWorkerIterator(dynBlock, scope)
	if err != nil {
		return err
	}

	forEachAttr, hasForEach := dynBlock.Attributes()["for_each"]
	if !hasForEach {
		return fail(`%s dynamic "worker" requires for_each`, scope)
	}
	forEachVal, diags := forEachAttr.Expr.Value(baseCtx)
	if diags.HasErrors() {
		return diags
	}
	elemVal, err := deriveDynamicWorkerElementVal(forEachVal, scope)
	if err != nil {
		return err
	}

	childCtx, err := buildDynamicWorkerEvalContext(baseCtx, iteratorName, elemVal, scope)
	if err != nil {
		return err
	}

	virtualHclBlock := buildVirtualWorkerBlock(contentBlock, d.Name())
	var cfg golden.Config
	if d.BaseBlock != nil {
		cfg = d.Config()
	}
	virtualWorker := &WorkerBlock{
		BaseBlock:      golden.NewBaseBlock(cfg, virtualHclBlock),
		ownerBlockType: "dynamic_worker",
	}
	if err := virtualWorker.Decode(virtualHclBlock, childCtx); err != nil {
		return err
	}

	contentTemplate := virtualWorker.planned.DeferredExpression
	if contentTemplate == "" {
		contentTemplate, err = buildDeferredExpression(
			virtualWorker.expandedBlock,
			virtualWorker.planned.Imports,
			virtualWorker.planned.Fallbacks,
		)
		if err != nil {
			return err
		}
	}

	d.virtualWorker = virtualWorker
	d.planned = DynamicConfig{
		Name:            d.Name(),
		Parallelism:     parallelism,
		Serial:          serial,
		ForEach:         forEachVal,
		ForEachExpr:     forEachAttr.ExprString(),
		IteratorName:    iteratorName,
		Content:         virtualWorker.WorkerConfig(),
		ContentTemplate: contentTemplate,
	}
	return nil
}

func (d *DynamicWorkerBlock) ExecuteDuringPlan() error {
	if d.virtualWorker != nil && d.BaseBlock != nil && d.Config() != nil {
		if reg, ok := d.Config().(ModuleRegistry); ok {
			if err := d.virtualWorker.validateModuleToolsAndMCP(reg); err != nil {
				return err
			}
			sourceURL, err := d.virtualWorker.resolveBasedSourceURL(reg)
			if err != nil {
				return err
			}
			d.planned.Content.SourceURL = sourceURL
			d.virtualWorker.planned.SourceURL = sourceURL

			if len(d.virtualWorker.planned.Fallbacks) > 0 {
				targets := make([]string, len(d.virtualWorker.planned.Fallbacks))
				for i, fb := range d.virtualWorker.planned.Fallbacks {
					targets[i] = fb.Target
				}
				if err := reg.ValidateWorkerFallbackTargets(d.Address(), targets); err != nil {
					return err
				}
			}
		}
	}

	plannedContentArtifactsType := cty.EmptyObject
	if len(d.planned.Content.Artifacts) > 0 {
		artAttrs := make(map[string]cty.Type, len(d.planned.Content.Artifacts))
		for artName := range d.planned.Content.Artifacts {
			artAttrs[artName] = artifactValueType
		}
		plannedContentArtifactsType = cty.Object(artAttrs)
	}
	itemAttrs := map[string]cty.Type{
		"work_branch": cty.String,
		"commit_sha":  cty.String,
		"artifact":    plannedContentArtifactsType,
	}
	if d.virtualWorker != nil && d.virtualWorker.hasTerminatingTool() {
		itemAttrs["result"] = cty.String
	}
	workerItemType := cty.Object(itemAttrs)

	d.plannedValue = cty.ObjectVal(map[string]cty.Value{
		"address":       cty.StringVal(d.Address()),
		"kind":          cty.StringVal("dynamic_worker"),
		"name":          cty.StringVal(d.Name()),
		"workers":       cty.UnknownVal(cty.Map(workerItemType)),
		"work_branches": cty.UnknownVal(cty.List(cty.String)),
	})
	return nil
}

func validateDynamicWorkerSchema(block *golden.HclBlock, scope string) (*golden.HclBlock, error) {
	for name := range block.Attributes() {
		if _, ok := dynamicWorkerAttributes[name]; ok || golden.MetaAttributeNames.Contains(name) {
			continue
		}
		return nil, fail("unsupported argument %q in dynamic_worker block", name)
	}
	var dynBlocks []*golden.HclBlock
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "dynamic":
			if len(nested.Labels) != 1 || nested.Labels[0] != "worker" {
				return nil, fail(`%s only supports dynamic "worker" blocks`, scope)
			}
			dynBlocks = append(dynBlocks, nested)
		case "precondition":
			continue
		default:
			return nil, fail("unsupported block type %q in dynamic_worker block", nested.Type)
		}
	}
	if len(dynBlocks) == 0 {
		return nil, fail(`%s must declare a dynamic "worker" block`, scope)
	}
	if len(dynBlocks) > 1 {
		return nil, fail(`%s supports at most one dynamic "worker" block`, scope)
	}
	return dynBlocks[0], nil
}

func validateDynamicWorkerNestedBlock(dynBlock *golden.HclBlock, scope string) (*golden.HclBlock, error) {
	for name := range dynBlock.Attributes() {
		if _, ok := dynamicWorkerNestedAttributes[name]; ok {
			continue
		}
		return nil, fail(`unsupported argument %q in dynamic "worker" block`, name)
	}
	var contentBlocks []*golden.HclBlock
	for _, nested := range dynBlock.NestedBlocks() {
		if nested.Type != "content" {
			return nil, fail(`unsupported block type %q in dynamic "worker" block`, nested.Type)
		}
		if len(nested.Labels) != 0 {
			return nil, fail(`%s dynamic "worker" content block must not have labels`, scope)
		}
		contentBlocks = append(contentBlocks, nested)
	}
	if len(contentBlocks) == 0 {
		return nil, fail(`%s dynamic "worker" must declare a content block`, scope)
	}
	if len(contentBlocks) > 1 {
		return nil, fail(`%s dynamic "worker" supports at most one content block`, scope)
	}
	return contentBlocks[0], nil
}

func extractDynamicWorkerIterator(dynBlock *golden.HclBlock, scope string) (string, error) {
	iterAttr, ok := dynBlock.Attributes()["iterator"]
	if !ok {
		return "worker", nil
	}
	traversal, diags := hcl.AbsTraversalForExpr(iterAttr.Expr)
	if diags.HasErrors() || len(traversal) != 1 {
		return "", fail("%s iterator must be a single unquoted identifier", scope)
	}
	candidate := strings.TrimSpace(traversal.RootName())
	if !hclsyntax.ValidIdentifier(candidate) {
		return "", fail("%s iterator %q is not a valid identifier", scope, candidate)
	}
	if _, reserved := reservedDynamicWorkerIterators[candidate]; reserved {
		return "", fail("%s iterator %q conflicts with a reserved identifier", scope, candidate)
	}
	return candidate, nil
}

func deriveDynamicWorkerElementVal(forEachVal cty.Value, scope string) (cty.Value, error) {
	unmarked, _ := forEachVal.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NilVal, fail("%s for_each must not be null", scope)
	}
	t := unmarked.Type()
	switch {
	case t.IsMapType():
		elemType := t.ElementType()
		if elemType == cty.DynamicPseudoType {
			if unmarked.IsKnown() && unmarked.LengthInt() == 0 {
				return cty.UnknownVal(cty.String), nil
			}
			return cty.NilVal, fail("%s for_each map element type must be known", scope)
		}
		if err := corespec.ValidateType(elemType); err != nil {
			return cty.NilVal, fail("%s for_each: %w", scope, err)
		}
		return cty.UnknownVal(elemType), nil
	case t.IsSetType():
		if !t.ElementType().Equals(cty.String) {
			return cty.NilVal, fail("%s for_each must be a map, object, or set of strings", scope)
		}
		return cty.UnknownVal(cty.String), nil
	case t.IsObjectType():
		attrTypes := t.AttributeTypes()
		if len(attrTypes) == 0 {
			if unmarked.IsKnown() {
				return cty.UnknownVal(cty.String), nil
			}
			return cty.DynamicVal, nil
		}
		var commonType cty.Type
		homogeneous := true
		for _, attrType := range attrTypes {
			if attrType != cty.DynamicPseudoType && !attrType.HasDynamicTypes() {
				if err := corespec.ValidateType(attrType); err != nil {
					return cty.NilVal, fail("%s for_each: %w", scope, err)
				}
			} else if hasCapsuleType(attrType) {
				return cty.NilVal, fail("%s for_each must be a map, object, or set of strings", scope)
			}
			if commonType == cty.NilType {
				commonType = attrType
				continue
			}
			if !attrType.Equals(commonType) {
				homogeneous = false
			}
		}
		if homogeneous && commonType != cty.NilType {
			return cty.UnknownVal(commonType), nil
		}
		return cty.DynamicVal, nil
	default:
		return cty.NilVal, fail("%s for_each must be a map, object, or set of strings", scope)
	}
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

func buildDynamicWorkerEvalContext(
	baseCtx *hcl.EvalContext,
	iteratorName string,
	elemVal cty.Value,
	scope string,
) (*hcl.EvalContext, error) {
	if baseCtx == nil {
		baseCtx = &hcl.EvalContext{}
	}
	child := baseCtx.NewChild()
	child.Variables = map[string]cty.Value{}
	if iteratorName == "worker" {
		workerAttrs := map[string]cty.Value{}
		if baseCtx.Variables != nil {
			if existing, ok := baseCtx.Variables["worker"]; ok && !existing.IsNull() && existing.IsKnown() && existing.Type().IsObjectType() {
				if existing.Type().HasAttribute("key") || existing.Type().HasAttribute("value") {
					return nil, fail(`%s default iterator "worker" conflicts with static worker named "key" or "value"`, scope)
				}
				maps.Copy(workerAttrs, existing.AsValueMap())
			}
		}
		workerAttrs["key"] = cty.UnknownVal(cty.String)
		workerAttrs["value"] = elemVal
		child.Variables["worker"] = cty.ObjectVal(workerAttrs)
		return child, nil
	}

	child.Variables[iteratorName] = cty.ObjectVal(map[string]cty.Value{
		"key":   cty.UnknownVal(cty.String),
		"value": elemVal,
	})
	return child, nil
}

func buildVirtualWorkerBlock(content *golden.HclBlock, name string) *golden.HclBlock {
	syntaxBlock := golden.CloneHclSyntaxBlock(content.Block)
	syntaxBlock.Type = "worker"
	syntaxBlock.Labels = []string{"", name}
	writeBlock := cloneWriteBlockAs(content, "worker", []string{name})
	return golden.NewHclBlock(syntaxBlock, writeBlock, nil)
}

func cloneWriteBlockAs(block *golden.HclBlock, blockType string, labels []string) *hclwrite.Block {
	if blockType == "" {
		blockType = block.Block.Type
	}
	if labels == nil {
		labels = append([]string(nil), block.Block.Labels...)
		if len(labels) > 0 && labels[0] == "" {
			labels = labels[1:]
		}
	}
	writeBlock := hclwrite.NewBlock(blockType, labels)
	for name, attribute := range block.Attributes() {
		writeBlock.Body().SetAttributeRaw(name, attribute.ExprTokens())
	}
	for _, nested := range block.NestedBlocks() {
		writeBlock.Body().AppendBlock(cloneWriteBlockAs(nested, "", nil))
	}
	return writeBlock
}

var _ golden.CustomDecode = (*DynamicWorkerBlock)(nil)
var _ golden.PlanBlock = (*DynamicWorkerBlock)(nil)
var _ golden.SingleValueBlock = (*DynamicWorkerBlock)(nil)
