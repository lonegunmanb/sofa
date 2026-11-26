package spec

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

var (
	fallbackAllowedAttrs = map[string]struct{}{
		"on": {}, "target": {}, "max_reworks": {}, "artifacts": {},
	}
	allowedFallbackTriggers = map[string]struct{}{
		"qc_exhausted":            {},
		"gatekeeper_exhausted":    {},
		"human_returned_upstream": {},
		"agent_escalated":         {},
	}
)

type FallbackConfig struct {
	Name       string
	On         []string
	Target     string
	MaxReworks int
	Artifacts  []string
}

func (f FallbackConfig) Clone() FallbackConfig {
	cloned := f
	cloned.On = slices.Clone(f.On)
	cloned.Artifacts = slices.Clone(f.Artifacts)
	return cloned
}

// ParseFallbackTargetExpr parses a fallback target expression into a worker block
// or worker for_each instance address (e.g. "worker.tasks" or "worker.matrix[linux]").
func ParseFallbackTargetExpr(expr hcl.Expression, exprString string) (string, error) {
	traversal, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() {
		if _, isLit := expr.(*hclsyntax.LiteralValueExpr); isLit && strings.TrimSpace(exprString) != "" {
			if reparsed, parseDiags := hclsyntax.ParseExpression([]byte(exprString), "fallback-target", hcl.InitialPos); !parseDiags.HasErrors() {
				traversal, diags = hcl.AbsTraversalForExpr(reparsed)
			}
		}
	}
	if diags.HasErrors() {
		return "", fail("fallback target must be a worker block traversal")
	}
	if len(traversal) < 2 || traversal.RootName() != "worker" {
		return "", fail("fallback target must reference a worker block")
	}
	workerAttr, ok := traversal[1].(hcl.TraverseAttr)
	if !ok || strings.TrimSpace(workerAttr.Name) == "" {
		return "", fail("fallback target must reference a worker block")
	}
	if len(traversal) == 2 {
		return "worker." + workerAttr.Name, nil
	}
	if len(traversal) == 3 {
		idx, ok := traversal[2].(hcl.TraverseIndex)
		if !ok {
			return "", fail("fallback target must reference a worker block, not an attribute")
		}
		key, _ := idx.Key.UnmarkDeep()
		if key.IsNull() || !key.IsKnown() || (!key.Type().Equals(cty.String) && !key.Type().Equals(cty.Number)) {
			return "", fail("fallback target index must be a known string or number")
		}
		return fmt.Sprintf("worker.%s[%s]", workerAttr.Name, golden.CtyValueToString(key)), nil
	}
	return "", fail("fallback target must reference a worker block, not an attribute")
}

func decodeFallback(block *golden.HclBlock, ctx *hcl.EvalContext, artifacts map[string]Artifact) (FallbackConfig, error) {
	if len(block.Labels) != 1 {
		return FallbackConfig{}, fail("worker fallback block must have exactly one name")
	}
	name := block.Labels[0]
	if err := validateSafeLabel(name, "fallback", false); err != nil {
		return FallbackConfig{}, err
	}
	scope := fmt.Sprintf("fallback %q", name)
	if err := validateNestedBlockSchema(block, scope, fallbackAllowedAttrs, nil); err != nil {
		return FallbackConfig{}, err
	}

	on, err := requiredKnownStringList(block, ctx, "on", scope, true)
	if err != nil {
		return FallbackConfig{}, err
	}
	if len(on) == 0 {
		return FallbackConfig{}, fail("%s on must not be empty", scope)
	}
	for _, trigger := range on {
		if _, ok := allowedFallbackTriggers[trigger]; !ok {
			return FallbackConfig{}, fail("%s has invalid trigger %q", scope, trigger)
		}
	}

	targetAttr, ok := block.Attributes()["target"]
	if !ok {
		return FallbackConfig{}, fail("%s target is required", scope)
	}
	target, err := ParseFallbackTargetExpr(targetAttr.Expr, targetAttr.ExprString())
	if err != nil {
		return FallbackConfig{}, fmt.Errorf("%s: %w", scope, err)
	}

	maxReworks, err := optionalWholeInt(block, ctx, "max_reworks", 2, scope)
	if err != nil {
		return FallbackConfig{}, err
	}
	if maxReworks <= 0 {
		return FallbackConfig{}, fail("%s max_reworks must be positive", scope)
	}

	resolvedArtifacts, err := decodeFallbackArtifacts(block, ctx, artifacts, scope)
	if err != nil {
		return FallbackConfig{}, err
	}

	return FallbackConfig{
		Name:       name,
		On:         on,
		Target:     target,
		MaxReworks: maxReworks,
		Artifacts:  resolvedArtifacts,
	}, nil
}

func decodeFallbackArtifacts(
	block *golden.HclBlock,
	ctx *hcl.EvalContext,
	declared map[string]Artifact,
	scope string,
) ([]string, error) {
	attr, ok := block.Attributes()["artifacts"]
	if !ok {
		return nil, nil
	}
	traversals := attr.Expr.Variables()
	if _, isLit := attr.Expr.(*hclsyntax.LiteralValueExpr); isLit && strings.TrimSpace(attr.ExprString()) != "" {
		if reparsed, parseDiags := hclsyntax.ParseExpression([]byte(attr.ExprString()), "fallback-artifacts", hcl.InitialPos); !parseDiags.HasErrors() {
			traversals = reparsed.Variables()
		}
	}
	for _, traversal := range traversals {
		if traversal.RootName() == "worker" {
			return nil, fail("%s artifacts must reference local artifact() declarations, not %q", scope, traversal.RootName())
		}
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsKnown() {
		return nil, fail("%s artifacts must be known during plan", scope)
	}
	if !unmarked.Type().IsListType() && !unmarked.Type().IsTupleType() {
		return nil, fail("%s artifacts must be a list of artifact(\"...\") objects", scope)
	}
	elements := unmarked.AsValueSlice()
	result := make([]string, 0, len(elements))
	seen := make(map[string]struct{}, len(elements))
	for _, elem := range elements {
		if elem.IsNull() || !elem.IsKnown() || !elem.Type().IsObjectType() {
			return nil, fail("%s artifacts elements must be artifact(\"...\") objects", scope)
		}
		if !elem.Type().HasAttribute("kind") || !elem.Type().HasAttribute("name") {
			return nil, fail("%s artifacts elements must be artifact(\"...\") objects", scope)
		}
		kindVal := elem.GetAttr("kind")
		if kindVal.IsNull() || !kindVal.IsKnown() || !kindVal.Type().Equals(cty.String) || kindVal.AsString() != "artifact" {
			return nil, fail("%s artifacts elements must have kind = \"artifact\"", scope)
		}
		nameVal := elem.GetAttr("name")
		if nameVal.IsNull() || !nameVal.IsKnown() || !nameVal.Type().Equals(cty.String) {
			return nil, fail("%s artifacts elements must have a known artifact name", scope)
		}
		artName := nameVal.AsString()
		art, exists := declared[artName]
		if !exists {
			return nil, fail("%s references unknown artifact %q", scope, artName)
		}
		if !matchesDeclaredArtifact(elem, art) {
			return nil, fail("%s artifacts must reference local artifact(%q) declared in this worker", scope, artName)
		}
		if _, dup := seen[artName]; dup {
			return nil, fail("%s artifacts contains duplicate reference %q", scope, artName)
		}
		seen[artName] = struct{}{}
		result = append(result, artName)
	}
	return result, nil
}

func matchesDeclaredArtifact(elem cty.Value, art Artifact) bool {
	if elem.Type().HasAttribute("path") {
		p := elem.GetAttr("path")
		if !p.IsKnown() || p.IsNull() || !p.Type().Equals(cty.String) || p.AsString() != art.ContainerPath() {
			return false
		}
	}
	if elem.Type().HasAttribute("rel_path") {
		rp := elem.GetAttr("rel_path")
		if !rp.IsKnown() || rp.IsNull() || !rp.Type().Equals(cty.String) || rp.AsString() != art.Path {
			return false
		}
	}
	if elem.Type().HasAttribute("type") {
		t := elem.GetAttr("type")
		if !t.IsKnown() || t.IsNull() || !t.Type().Equals(cty.String) || t.AsString() != art.Type {
			return false
		}
	}
	if elem.Type().HasAttribute("description") {
		d := elem.GetAttr("description")
		if !d.IsKnown() || d.IsNull() || !d.Type().Equals(cty.String) || d.AsString() != art.Description {
			return false
		}
	}
	return true
}
