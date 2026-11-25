package planner

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestMCPServerPublishesStableIDs(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
mcp_server "search" {
  tools     = ["query"]
  resources = ["docs://guide"]
  timeout   = "30s"

  http {
    url = "https://mcp.example.com/mcp"
  }
}

output "tool" {
  value = mcp_server.search.tool_ids["query"]
}

output "resource" {
  value = mcp_server.search.resource_ids["docs://guide"]
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	server := cfg.EvalContext().Variables["mcp_server"].GetAttr("search")
	toolID := server.GetAttr("tool_ids").Index(cty.StringVal("query")).AsString()
	resourceID := server.GetAttr("resource_ids").Index(cty.StringVal("docs://guide")).AsString()
	require.Contains(t, toolID, "mcp_tool_search__query_")
	require.Contains(t, resourceID, "mcp_resource_search__docs")
	require.Equal(t, resourceID, cfg.Outputs()[0].Value.AsString())
	require.Equal(t, toolID, cfg.Outputs()[1].Value.AsString())
}

func TestMCPServerRejectsMultipleTransports(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
mcp_server "search" {
  tools = ["query"]
  http {
    url = "https://mcp.example.com/mcp"
  }
  stdio {
    command = "server"
  }
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.ErrorContains(t, cfg.RunPlan(), "exactly one")
}
