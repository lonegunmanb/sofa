package spec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
)

func buildDeferredExpression(
	block *golden.HclBlock,
	imports map[string]ImportArtifact,
	fallbacks []FallbackConfig,
) (string, error) {
	var builder strings.Builder
	builder.WriteString("{\n")
	writeExprAttributes(&builder, block.Attributes(), workerAttributes, nil)

	retryBlocks := nestedBlocksByType(block, "retry")
	builder.WriteString("retry = ")
	if len(retryBlocks) == 0 {
		builder.WriteString("null\n")
	} else {
		writeDeferredObject(&builder, retryBlocks[0], nil, nil)
	}

	builder.WriteString("artifact = {\n")
	for _, art := range nestedBlocksByType(block, "artifact") {
		name := labelOf(art)
		fmt.Fprintf(&builder, "%s = ", strconv.Quote(name))
		writeDeferredObject(&builder, art, map[string]string{
			"name":      strconv.Quote(name),
			"required":  "false",
			"non_empty": "false",
		}, nil)
	}
	builder.WriteString("}\n")

	builder.WriteString("import_artifact = {\n")
	for _, imp := range nestedBlocksByType(block, "import_artifact") {
		name := labelOf(imp)
		effectivePath := ".sofa/imports/" + name
		if resolved, ok := imports[name]; ok && resolved.Path != "" {
			effectivePath = resolved.Path
		}
		fmt.Fprintf(&builder, "%s = ", strconv.Quote(name))
		writeDeferredObject(&builder, imp, nil, map[string]string{
			"path": strconv.Quote(effectivePath),
		})
	}
	builder.WriteString("}\n")

	builder.WriteString("tool_use = {\n")
	for _, tu := range nestedBlocksByType(block, "tool_use") {
		name := labelOf(tu)
		fmt.Fprintf(&builder, "%s = ", strconv.Quote(name))
		writeDeferredObject(&builder, tu, map[string]string{
			"terminate": "false",
			"input":     "{}",
		}, nil)
	}
	builder.WriteString("}\n")

	builder.WriteString("gatekeeper = {\n")
	for _, gk := range nestedBlocksByType(block, "gatekeeper") {
		name := labelOf(gk)
		fmt.Fprintf(&builder, "%s = ", strconv.Quote(name))
		writeDeferredObject(&builder, gk, map[string]string{
			"working_dir":            strconv.Quote("."),
			"timeout":                strconv.Quote("10m"),
			"expect_exit_code":       "0",
			"require_clean_worktree": "true",
		}, nil)
	}
	builder.WriteString("}\n")

	qcBlocks := nestedBlocksByType(block, "qc")
	builder.WriteString("qc = ")
	if len(qcBlocks) == 0 {
		builder.WriteString("null\n")
	} else {
		writeDeferredQC(&builder, qcBlocks[0])
	}

	fallbackTargets := make(map[string]string, len(fallbacks))
	for _, fb := range fallbacks {
		fallbackTargets[fb.Name] = fb.Target
	}
	builder.WriteString("fallback = {\n")
	for _, fb := range nestedBlocksByType(block, "fallback") {
		name := labelOf(fb)
		target := fallbackTargets[name]
		if target == "" {
			if attr, ok := fb.Attributes()["target"]; ok {
				if parsed, err := ParseFallbackTargetExpr(attr.Expr, attr.ExprString()); err == nil {
					target = parsed
				}
			}
		}
		fmt.Fprintf(&builder, "%s = ", strconv.Quote(name))
		writeDeferredObject(&builder, fb, map[string]string{
			"max_reworks": "2",
		}, map[string]string{
			"target": strconv.Quote(target),
		})
	}
	builder.WriteString("}\n")
	builder.WriteString("}\n")

	expr := builder.String()
	_, diags := hclsyntax.ParseExpression([]byte(expr), block.Range().Filename, block.Range().Start)
	if diags.HasErrors() {
		return "", diags
	}
	return expr, nil
}

func writeDeferredQC(builder *strings.Builder, block *golden.HclBlock) {
	builder.WriteString("{\n")
	if _, ok := block.Attributes()["max_qc_rounds"]; !ok {
		fmt.Fprintf(builder, "max_qc_rounds = %d\n", DefaultMaxQCRounds)
	}
	writeExprAttributes(builder, block.Attributes(), nil, nil)
	retryBlocks := nestedBlocksByType(block, "retry")
	builder.WriteString("retry = ")
	if len(retryBlocks) == 0 {
		builder.WriteString("null\n")
	} else {
		writeDeferredObject(builder, retryBlocks[0], nil, nil)
	}
	builder.WriteString("}\n")
}

func writeDeferredObject(
	builder *strings.Builder,
	block *golden.HclBlock,
	defaults map[string]string,
	overrides map[string]string,
) {
	builder.WriteString("{\n")
	for _, name := range sortedAttrKeys(defaults) {
		if _, exists := block.Attributes()[name]; !exists {
			if _, overridden := overrides[name]; !overridden {
				fmt.Fprintf(builder, "%s = %s\n", name, defaults[name])
			}
		}
	}
	writeExprAttributes(builder, block.Attributes(), nil, overrides)
	for _, name := range sortedAttrKeys(overrides) {
		fmt.Fprintf(builder, "%s = %s\n", name, overrides[name])
	}
	builder.WriteString("}\n")
}

func writeExprAttributes(
	builder *strings.Builder,
	attrs map[string]*golden.HclAttribute,
	allowed map[string]struct{},
	overrides map[string]string,
) {
	for _, name := range sortedAttrKeys(attrs) {
		if golden.MetaAttributeNames.Contains(name) {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if _, overridden := overrides[name]; overridden {
			continue
		}
		fmt.Fprintf(builder, "%s = %s\n", name, serializeAttrExpr(attrs[name]))
	}
}

func serializeAttrExpr(attr *golden.HclAttribute) string {
	if lit, ok := attr.Expr.(*hclsyntax.LiteralValueExpr); ok && lit.SrcRange.Empty() && lit.Val.IsWhollyKnown() {
		unmarked, _ := lit.Val.UnmarkDeep()
		return strings.TrimSpace(string(hclwrite.TokensForValue(unmarked).Bytes()))
	}
	return attr.ExprString()
}

func nestedBlocksByType(block *golden.HclBlock, blockType string) []*golden.HclBlock {
	var out []*golden.HclBlock
	for _, nested := range block.NestedBlocks() {
		if nested.Type == blockType {
			out = append(out, nested)
		}
	}
	return out
}

func labelOf(block *golden.HclBlock) string {
	if len(block.Labels) == 0 {
		return ""
	}
	return block.Labels[len(block.Labels)-1]
}

func sortedAttrKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
