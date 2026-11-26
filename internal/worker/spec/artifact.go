package spec

import (
	"fmt"
	"path"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

var (
	artifactAllowedAttrs = map[string]struct{}{
		"type": {}, "path": {}, "description": {}, "required": {}, "non_empty": {},
	}
	importArtifactAllowedAttrs = map[string]struct{}{
		"desc": {}, "sources": {}, "path": {},
	}
)

var artifactValueType = cty.Object(map[string]cty.Type{
	"id":          cty.String,
	"name":        cty.String,
	"kind":        cty.String,
	"type":        cty.String,
	"path":        cty.String,
	"description": cty.String,
	"required":    cty.Bool,
	"non_empty":   cty.Bool,
})

type Artifact struct {
	Name        string
	Type        string
	Path        string
	Description string
	Required    bool
	NonEmpty    bool
}

type ImportArtifact struct {
	Name    string
	Desc    string
	Path    string
	Sources cty.Value
}

func artifactValue(artifact Artifact) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"id":          cty.UnknownVal(cty.String),
		"name":        cty.StringVal(artifact.Name),
		"kind":        cty.StringVal("artifact"),
		"type":        cty.StringVal(artifact.Type),
		"path":        cty.StringVal(artifact.Path),
		"description": cty.StringVal(artifact.Description),
		"required":    cty.BoolVal(artifact.Required),
		"non_empty":   cty.BoolVal(artifact.NonEmpty),
	})
}

func ValidateSafeLabel(label, scope string, forbidPathTokens bool) error {
	return validateSafeLabel(label, scope, forbidPathTokens)
}

func validateSafeLabel(label, scope string, forbidPathTokens bool) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("%s name must not be empty", scope)
	}
	if forbidPathTokens {
		if label == "." || label == ".." || strings.ContainsAny(label, `/\`) {
			return fmt.Errorf("%s name %q must not contain path separators or '.'/'..'", scope, label)
		}
	}
	return nil
}

func validateWorkspaceRelativePath(value, label string, allowDot bool) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", label)
	}
	if strings.Contains(value, `\`) {
		return fmt.Errorf("%s must be a clean workspace-relative path", label)
	}
	if len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' {
		return fmt.Errorf("%s must be a clean workspace-relative path", label)
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return fmt.Errorf("%s must be a clean workspace-relative path", label)
	}
	cleaned := path.Clean(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%s must be a clean workspace-relative path", label)
	}
	if !allowDot && cleaned == "." {
		return fmt.Errorf("%s must not be workspace root '.'", label)
	}
	return nil
}

func effectiveImportPath(imported ImportArtifact) string {
	if imported.Path != "" {
		return path.Clean(imported.Path)
	}
	return path.Clean(".sofa/imports/" + imported.Name)
}

func decodeArtifact(block *golden.HclBlock, ctx *hcl.EvalContext) (Artifact, error) {
	if len(block.Labels) != 1 {
		return Artifact{}, fail("worker artifact block must have exactly one name")
	}
	name := block.Labels[0]
	if err := validateSafeLabel(name, "artifact", true); err != nil {
		return Artifact{}, err
	}
	if err := validateNestedBlockSchema(block, fmt.Sprintf("artifact %q", name), artifactAllowedAttrs, nil); err != nil {
		return Artifact{}, err
	}

	art := Artifact{Name: name}
	var err error
	if art.Type, err = requiredKnownNonBlankString(block, ctx, "type", fmt.Sprintf("artifact %q", name)); err != nil {
		return Artifact{}, err
	}
	if art.Type != "file" && art.Type != "directory" {
		return Artifact{}, fail("artifact %q type must be file or directory", name)
	}
	rawPath, err := requiredKnownNonBlankString(block, ctx, "path", fmt.Sprintf("artifact %q", name))
	if err != nil {
		return Artifact{}, err
	}
	if err := validateWorkspaceRelativePath(rawPath, "artifact path", false); err != nil {
		return Artifact{}, err
	}
	art.Path = path.Clean(rawPath)

	if art.Description, err = requiredKnownNonBlankString(block, ctx, "description", fmt.Sprintf("artifact %q", name)); err != nil {
		return Artifact{}, err
	}
	if art.Required, err = optionalBool(block, ctx, "required", false, fmt.Sprintf("artifact %q", name)); err != nil {
		return Artifact{}, err
	}
	if art.NonEmpty, err = optionalBool(block, ctx, "non_empty", false, fmt.Sprintf("artifact %q", name)); err != nil {
		return Artifact{}, err
	}
	return art, nil
}

func decodeImport(block *golden.HclBlock, ctx *hcl.EvalContext, ownerBlockType, workerName string) (ImportArtifact, error) {
	if len(block.Labels) != 1 {
		return ImportArtifact{}, fail("worker import_artifact block must have exactly one name")
	}
	name := block.Labels[0]
	if err := validateSafeLabel(name, "import_artifact", true); err != nil {
		return ImportArtifact{}, err
	}
	scope := fmt.Sprintf("import_artifact %q", name)
	if err := validateNestedBlockSchema(block, scope, importArtifactAllowedAttrs, nil); err != nil {
		return ImportArtifact{}, err
	}

	imported := ImportArtifact{Name: name}
	desc, descSet, err := optionalKnownString(block, ctx, "desc", scope)
	if err != nil {
		return ImportArtifact{}, err
	}
	if descSet {
		if strings.TrimSpace(desc) == "" {
			return ImportArtifact{}, fail("%s desc must not be empty", scope)
		}
		imported.Desc = desc
	}

	rawPath, pathSet, err := optionalKnownString(block, ctx, "path", scope)
	if err != nil {
		return ImportArtifact{}, err
	}
	if pathSet {
		if err := validateWorkspaceRelativePath(rawPath, "import_artifact path", false); err != nil {
			return ImportArtifact{}, err
		}
		imported.Path = path.Clean(rawPath)
	}

	sourcesAttr, ok := block.Attributes()["sources"]
	if !ok {
		return ImportArtifact{}, fail("%s sources is required", scope)
	}
	if err := rejectSelfArtifactReferenceInImport(sourcesAttr.Expr, ownerBlockType, workerName, scope); err != nil {
		return ImportArtifact{}, err
	}
	value, diags := sourcesAttr.Expr.Value(ctx)
	if diags.HasErrors() {
		return ImportArtifact{}, diags
	}
	if err := validateImportSources(value, scope); err != nil {
		return ImportArtifact{}, err
	}
	imported.Sources = value
	return imported, nil
}

func rejectSelfArtifactReferenceInImport(expr hcl.Expression, ownerBlockType, workerName, scope string) error {
	if ownerBlockType == "" {
		ownerBlockType = "worker"
	}
	for _, traversal := range expr.Variables() {
		if len(traversal) >= 2 && traversal.RootName() == ownerBlockType {
			if attr, ok := traversal[1].(hcl.TraverseAttr); ok && attr.Name == workerName {
				return fail("%s sources must not reference current %s %q", scope, ownerBlockType, workerName)
			}
		}
	}
	if synExpr, ok := expr.(hclsyntax.Expression); ok {
		var walkErr error
		_ = hclsyntax.Walk(synExpr, walkerFunc(func(node hclsyntax.Node) hcl.Diagnostics {
			if call, isCall := node.(*hclsyntax.FunctionCallExpr); isCall && call.Name == "artifact" {
				walkErr = fail("%s sources must not call local artifact()", scope)
			}
			return nil
		}))
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

func validateImportSources(sources cty.Value, scope string) error {
	if sources == cty.NilVal {
		return fail("%s sources must be a list of artifact objects", scope)
	}
	unmarked, _ := sources.UnmarkDeep()
	if unmarked.IsNull() {
		return fail("%s sources must not be null", scope)
	}
	if !unmarked.Type().IsListType() && !unmarked.Type().IsTupleType() {
		return fail("%s sources must be a list of artifact objects", scope)
	}
	if !unmarked.IsKnown() {
		return nil
	}
	for _, element := range unmarked.AsValueSlice() {
		if element.IsNull() || !element.Type().IsObjectType() || !element.Type().HasAttribute("kind") {
			return fail("%s sources must contain artifact objects", scope)
		}
		kind := element.GetAttr("kind")
		if !kind.Type().Equals(cty.String) {
			return fail("%s sources must contain artifact objects", scope)
		}
		if kind.IsKnown() {
			if kind.IsNull() || kind.AsString() != "artifact" {
				return fail("%s sources must contain artifact objects", scope)
			}
		}
	}
	return nil
}

type walkerFunc func(hclsyntax.Node) hcl.Diagnostics

func (f walkerFunc) Enter(node hclsyntax.Node) hcl.Diagnostics { return f(node) }
func (walkerFunc) Exit(hclsyntax.Node) hcl.Diagnostics         { return nil }
