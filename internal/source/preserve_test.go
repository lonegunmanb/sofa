package source

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestPreserveBlockAcceptsKnownAndUnknownStringBranches(t *testing.T) {
	t.Run("known", func(t *testing.T) {
		block := decodePreserveBlock(t, `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = "release/v1"
}
`, nil)
		require.NoError(t, block.ExecuteDuringPlan())

		value := block.Value()
		require.Equal(t, "preserve.delivery", value.GetAttr("address").AsString())
		require.Equal(t, "preserve", value.GetAttr("kind").AsString())
		require.Equal(t, "release/v1", value.GetAttr("branch").AsString())
		require.Equal(t, `"release/v1"`, value.GetAttr("branch_expr").AsString())
	})

	t.Run("unknown string", func(t *testing.T) {
		ctx := &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"worker": cty.ObjectVal(map[string]cty.Value{
					"implement": cty.ObjectVal(map[string]cty.Value{
						"work_branch": cty.UnknownVal(cty.String),
					}),
				}),
			},
		}
		block := decodePreserveBlock(t, `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = worker.implement.work_branch
}
`, ctx)
		require.NoError(t, block.ExecuteDuringPlan())

		branch := block.Value().GetAttr("branch")
		require.False(t, branch.IsKnown())
		require.True(t, branch.Type().Equals(cty.String))
		require.Equal(t, "worker.implement.work_branch", block.Value().GetAttr("branch_expr").AsString())
	})
}

func TestPreserveBlockRejectsInvalidBranchesAndURLs(t *testing.T) {
	tests := []struct {
		name string
		hcl  string
		ctx  *hcl.EvalContext
		want string
	}{
		{
			name: "invalid known branch",
			hcl: `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = "bad..branch"
}`,
			want: "branch",
		},
		{
			name: "unknown number",
			hcl: `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = worker.implement.work_branch
}`,
			ctx: &hcl.EvalContext{Variables: map[string]cty.Value{
				"worker": cty.ObjectVal(map[string]cty.Value{
					"implement": cty.ObjectVal(map[string]cty.Value{
						"work_branch": cty.UnknownVal(cty.Number),
					}),
				}),
			}},
			want: "must be a string",
		},
		{
			name: "null branch",
			hcl: `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = null
}`,
			want: "must not be null",
		},
		{
			name: "unknown url",
			hcl: `
preserve "delivery" {
  url    = repository.url
  branch = "main"
}`,
			ctx: &hcl.EvalContext{Variables: map[string]cty.Value{
				"repository": cty.ObjectVal(map[string]cty.Value{
					"url": cty.UnknownVal(cty.String),
				}),
			}},
			want: "url must be a known",
		},
		{
			name: "missing url",
			hcl: `
preserve "delivery" {
  branch = "main"
}`,
			want: "url",
		},
		{
			name: "missing branch",
			hcl: `
preserve "delivery" {
  url = "https://github.com/example/repo.git"
}`,
			want: "branch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, err := parsePreserveBlock(t, tt.hcl)
			require.NoError(t, err)
			err = block.Decode(block.HclBlock(), tt.ctx)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestPreserveBlockRejectsUnknownSchema(t *testing.T) {
	tests := []struct {
		name string
		hcl  string
		want string
	}{
		{
			name: "unknown attribute",
			hcl: `
preserve "delivery" {
  url     = "https://github.com/example/repo.git"
  branch  = "main"
  unknown = true
}`,
			want: "unknown attribute",
		},
		{
			name: "unsupported nested block",
			hcl: `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
  unsupported {}
}`,
			want: "unsupported nested block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, err := parsePreserveBlock(t, tt.hcl)
			require.NoError(t, err)
			err = block.Decode(block.HclBlock(), nil)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func decodePreserveBlock(t *testing.T, source string, ctx *hcl.EvalContext) *PreserveBlock {
	t.Helper()
	block, err := parsePreserveBlock(t, source)
	require.NoError(t, err)
	require.NoError(t, block.Decode(block.HclBlock(), ctx))
	return block
}

func parsePreserveBlock(t *testing.T, source string) (*PreserveBlock, error) {
	t.Helper()
	golden.RegisterBlock(&PreserveBlock{})
	syntaxFile, diags := hclsyntax.ParseConfig([]byte(source), "preserve.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	writeFile, diags := hclwrite.ParseConfig([]byte(source), "preserve.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	syntaxBody := syntaxFile.Body.(*hclsyntax.Body)
	blocks := golden.AsHclBlocks(syntaxBody.Blocks, writeFile.Body().Blocks())
	require.Len(t, blocks, 1)
	return &PreserveBlock{
		BaseBlock: golden.NewBaseBlock(nil, blocks[0]),
	}, nil
}
