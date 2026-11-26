package spec_test

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func indexByName[T golden.Block](blocks []T) map[string]T {
	byName := make(map[string]T, len(blocks))
	for _, block := range blocks {
		byName[block.Name()] = block
	}
	return byName
}

func TestTypedToolIDsDeterministicCanonicalAndCapped(t *testing.T) {
	source := `
go_tool "finish" {
  description = "Finish"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

go_tool "this_is_a_very_long_tool_name_exceeding_sixty_four_characters_easily_when_combined_with_uuid" {
  description = "Long"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

external_tool "this_is_another_very_long_external_tool_name_exceeding_sixty_four_characters_easily" {
  description = "Long external"
  program     = ["run"]
  input_type  = object({ query = string })
  output_type = string
}
`
	first := parseToolConfigWithPrefix(t, "module.alpha", source)
	require.NoError(t, first.RunPlan())
	second := parseToolConfigWithPrefix(t, "module.alpha", source)
	require.NoError(t, second.RunPlan())
	sibling := parseToolConfigWithPrefix(t, "module.beta", source)
	require.NoError(t, sibling.RunPlan())

	firstGo := indexByName(golden.Blocks[*toolspec.GoToolBlock](first))
	secondGo := indexByName(golden.Blocks[*toolspec.GoToolBlock](second))
	siblingGo := indexByName(golden.Blocks[*toolspec.GoToolBlock](sibling))
	firstExt := indexByName(golden.Blocks[*toolspec.ExternalToolBlock](first))
	secondExt := indexByName(golden.Blocks[*toolspec.ExternalToolBlock](second))
	siblingExt := indexByName(golden.Blocks[*toolspec.ExternalToolBlock](sibling))

	require.Len(t, firstGo, 2)
	require.Len(t, firstExt, 1)

	for name, block := range firstGo {
		assert.Equal(t, block.Id(), secondGo[name].Id())
		assert.NotEqual(t, block.Id(), siblingGo[name].Id())
		assert.LessOrEqual(t, len(block.Id()), 64)
		assert.Regexp(t, `^tool_go_tool_[a-zA-Z0-9_-]+_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, block.Id())
		assert.Equal(t, block.Id(), block.BaseValues()["id"].AsString())
		assert.Equal(t, block.Id(), block.Value().GetAttr("id").AsString())
	}

	for name, block := range firstExt {
		assert.Equal(t, block.Id(), secondExt[name].Id())
		assert.NotEqual(t, block.Id(), siblingExt[name].Id())
		assert.LessOrEqual(t, len(block.Id()), 64)
		assert.Regexp(t, `^tool_external_tool_[a-zA-Z0-9_-]+_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, block.Id())
		assert.Equal(t, block.Id(), block.BaseValues()["id"].AsString())
		assert.Equal(t, block.Id(), block.Value().GetAttr("id").AsString())
	}
}

func TestTypedToolIDSameNameDifferentKindDiffer(t *testing.T) {
	cfg := parseToolConfigWithPrefix(t, "module.root", `
go_tool "lookup" {
  description = "Go lookup"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

external_tool "lookup" {
  description = "External lookup"
  program     = ["lookup"]
  input_type  = object({ q = string })
  output_type = string
}
`)
	require.NoError(t, cfg.RunPlan())

	goBlock := golden.Blocks[*toolspec.GoToolBlock](cfg)[0]
	extBlock := golden.Blocks[*toolspec.ExternalToolBlock](cfg)[0]
	assert.NotEqual(t, goBlock.Id(), extBlock.Id())
	assert.True(t, strings.HasPrefix(goBlock.Id(), "tool_go_tool_lookup_"))
	assert.True(t, strings.HasPrefix(extBlock.Id(), "tool_external_tool_lookup_"))

	// Verify tool_name() returns the exact deterministic ID in HCL expressions.
	evalCtx := cfg.EvalContext()
	expr, diags := hclsyntax.ParseExpression([]byte("tool_name(go_tool.lookup)"), "test.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	val, diags := expr.Value(evalCtx)
	require.False(t, diags.HasErrors(), diags.Error())
	assert.Equal(t, goBlock.Id(), val.AsString())

	_ = config.Functions
}
