package spec

import (
	"fmt"
	"path"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

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

func validateWorkspaceRelativePath(value, label string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", label)
	}
	cleaned := path.Clean(value)
	if path.IsAbs(value) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%s must be a clean workspace-relative path", label)
	}
	return nil
}

func effectiveImportPath(imported ImportArtifact) string {
	if imported.Path != "" {
		return path.Clean(imported.Path)
	}
	return path.Clean(".sofa/imports/" + imported.Name)
}
