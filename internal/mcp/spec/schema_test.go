package spec

import (
	"regexp"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
)

func TestMCPDeterministicIDs(t *testing.T) {
	block := testServerBlock("search")
	toolID := block.ToolID("query")
	resourceID := block.ResourceID("docs://guide")

	require.Equal(t, toolID, block.ToolID("query"))
	require.Equal(t, resourceID, block.ResourceID("docs://guide"))
	require.True(t, regexp.MustCompile(`^mcp_tool_search__query_[0-9a-f-]{36}$`).MatchString(toolID))
	require.True(t, regexp.MustCompile(`^mcp_resource_search__docs_+guide_[0-9a-f-]{36}$`).MatchString(resourceID))

	otherCanonical := toolIDFor("module.child.mcp_server.search", "search", "query")
	require.NotEqual(t, toolID, otherCanonical)
}

func testServerBlock(name string) *ServerBlock {
	hclBlock := &golden.HclBlock{
		Block: &hclsyntax.Block{
			Type:   "mcp_server",
			Labels: []string{"", name},
		},
	}
	return &ServerBlock{BaseBlock: golden.NewBaseBlock(nil, hclBlock)}
}
