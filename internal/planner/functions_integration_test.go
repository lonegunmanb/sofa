package planner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestPlanningContextFunctionsAndModulePath(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
locals {
  module_path = path.module
  decoded = jsondecodewithtype(
    "{\"checks\":{\"a\":\"b\"}}",
    object({checks = map(string)})
  )
  one_value = one(["only"])
  current_dir = cwd()
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	locals := cfg.EvalContext().Variables["local"]
	modulePath := locals.GetAttr("module_path").AsString()
	require.False(t, strings.Contains(modulePath, `\`))
	require.Equal(t, filepath.ToSlash(dir), modulePath)

	decoded := locals.GetAttr("decoded")
	require.True(t, decoded.Type().Equals(cty.Object(map[string]cty.Type{
		"checks": cty.Map(cty.String),
	})))
	require.Equal(t, "only", locals.GetAttr("one_value").AsString())
	require.NotEmpty(t, locals.GetAttr("current_dir").AsString())
}

func TestTypedJSONInLocalHasExactType(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "payload" {
  type    = string
  default = "{\"checks\":{\"a\":\"b\"}}"
}

locals {
  typed = jsondecodewithtype(
    var.payload,
    object({checks = map(string)})
  )
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	typed := cfg.EvalContext().Variables["local"].GetAttr("typed")
	require.True(t, typed.Type().Equals(cty.Object(map[string]cty.Type{
		"checks": cty.Map(cty.String),
	})))
}

func TestUnknownJSONExpressionInHCLHasExactType(t *testing.T) {
	cfg := newTestPlanner(t, t.TempDir(), nil)
	ctx := cfg.EvalContext()
	ctx.Variables["var"] = cty.ObjectVal(map[string]cty.Value{
		"payload": cty.UnknownVal(cty.String),
	})

	expr, diags := hclsyntax.ParseExpression(
		[]byte(`jsondecodewithtype(var.payload, object({checks = map(string)}))`),
		"unknown.sofa.hcl",
		hcl.InitialPos,
	)
	require.False(t, diags.HasErrors(), diags.Error())

	typed, valueDiags := expr.Value(ctx)
	require.False(t, valueDiags.HasErrors(), valueDiags.Error())
	require.False(t, typed.IsKnown())
	require.True(t, typed.Type().Equals(cty.Object(map[string]cty.Type{
		"checks": cty.Map(cty.String),
	})))
}

func TestEvalContextUsesAbsoluteModulePath(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "relative-module")
	require.NoError(t, os.Mkdir(dir, 0o755))

	cfg := NewSofaConfig(ConfigArgs{
		Context:   context.Background(),
		Directory: dir,
	})
	modulePath := cfg.EvalContext().Variables["path"].GetAttr("module").AsString()
	require.True(t, filepath.IsAbs(filepath.FromSlash(modulePath)))
	require.False(t, strings.Contains(modulePath, `\`))
}
