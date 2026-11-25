package spec

import (
	"regexp"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
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

func TestStringMapRejectsUnknownAndNullElements(t *testing.T) {
	_, err := stringMap(cty.ObjectVal(map[string]cty.Value{
		"X-Commit": cty.UnknownVal(cty.String),
	}), "headers")
	require.ErrorContains(t, err, "wholly known")

	_, err = stringMap(cty.ObjectVal(map[string]cty.Value{
		"X-Optional": cty.NullVal(cty.String),
	}), "headers")
	require.ErrorContains(t, err, "null")
}

func TestServerConfigReturnsCanonicalDeepCopy(t *testing.T) {
	block := testServerBlock("search")
	block.planned = mcppkg.Config{
		Name:      "search",
		Transport: mcppkg.TransportHTTP,
		Tools:     []string{"query"},
		Timeout:   30,
		HTTP: &mcppkg.HTTPConfig{
			URL:     "https://mcp.example.com",
			Headers: map[string]string{"X-Test": "value"},
		},
	}
	config := block.ServerConfig()
	require.Equal(t, block.CanonicalAddress(), config.RuntimeName)
	config.Tools[0] = "mutated"
	config.HTTP.Headers["X-Test"] = "mutated"
	require.Equal(t, "query", block.planned.Tools[0])
	require.Equal(t, "value", block.planned.HTTP.Headers["X-Test"])
}
