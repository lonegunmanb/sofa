package config

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestRootDefaultsHookRunsBeforeBlockAdaptation(t *testing.T) {
	RegisterRootDefaultsHook(func(module *RootModule) error {
		file := module.Files[0]
		block := file.Syntax.Blocks[0]
		block.Body.Attributes["injected"] = &hclsyntax.Attribute{
			Name: "injected",
			Expr: &hclsyntax.LiteralValueExpr{
				Val: cty.StringVal("syntax-value"),
			},
			SrcRange:    block.TypeRange,
			NameRange:   block.TypeRange,
			EqualsRange: block.TypeRange,
		}
		file.Write.Blocks()[0].Body().SetAttributeValue("injected", cty.StringVal("write-value"))
		return nil
	})
	t.Cleanup(func() {
		RegisterRootDefaultsHook(nil)
	})

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `variable "sample" {}`)
	loaded, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	require.Len(t, loaded.Blocks, 1)
	require.Contains(t, loaded.Blocks[0].Body.Attributes, "injected")
	injected, valueDiags := loaded.Blocks[0].Body.Attributes["injected"].Expr.Value(nil)
	require.False(t, valueDiags.HasErrors(), valueDiags.Error())
	require.Equal(t, "syntax-value", injected.AsString())
	require.Equal(t, `"write-value"`, loaded.Blocks[0].Attributes()["injected"].ExprString())
}
