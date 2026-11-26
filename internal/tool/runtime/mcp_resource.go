package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
)

// MCPResourceToolName is the fixed host tool name exposed when a session declares mcp_resource_ids.
const MCPResourceToolName = "sofa_read_mcp_resource"

// MCPResourceSpec identifies an authorized (ServerName, URI) pair from mcp_resource_ids.
type MCPResourceSpec struct {
	ID         string
	ServerName string
	URI        string
}

// MCPResourceReadFunc delegates an authorized MCP resource read to Session.ReadMCPResource.
type MCPResourceReadFunc func(ctx context.Context, serverName, uri string) ([]harnessruntime.MCPResourceContent, error)

type mcpResourceKey struct {
	serverName string
	uri        string
}

// MCPResourceTool exposes sofa_read_mcp_resource with strict whitelist enforcement.
type MCPResourceTool struct {
	allowed map[mcpResourceKey]MCPResourceSpec
	reader  MCPResourceReadFunc
	schema  []byte
}

// NewMCPResourceReader builds an MCPResourceTool when allowed is non-empty; returns (nil, false) otherwise.
func NewMCPResourceReader(allowed []MCPResourceSpec, reader MCPResourceReadFunc) (*MCPResourceTool, bool) {
	if len(allowed) == 0 {
		return nil, false
	}
	allowMap := make(map[mcpResourceKey]MCPResourceSpec, len(allowed))
	for _, spec := range allowed {
		key := mcpResourceKey{serverName: spec.ServerName, uri: spec.URI}
		allowMap[key] = spec
	}
	schemaBytes, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server_name": map[string]any{"type": "string"},
			"uri":         map[string]any{"type": "string"},
		},
		"required":             []string{"server_name", "uri"},
		"additionalProperties": false,
	})
	return &MCPResourceTool{
		allowed: allowMap,
		reader:  reader,
		schema:  schemaBytes,
	}, true
}

// HostToolSpec returns the HostToolSpec registered on SessionConfig.HostTools.
func (t *MCPResourceTool) HostToolSpec() harnessruntime.HostToolSpec {
	return harnessruntime.HostToolSpec{
		Name:                 MCPResourceToolName,
		Description:          "Read a declared MCP resource authorized by the session's mcp_resource_ids.",
		ParametersJSONSchema: append([]byte(nil), t.schema...),
	}
}

// Handler returns the ToolHandler enforcing exact (server_name, uri) membership before reading.
func (t *MCPResourceTool) Handler() harnessruntime.ToolHandler {
	return func(ctx context.Context, call harnessruntime.ToolInvocation) (harnessruntime.ToolOutput, error) {
		rawVal, err := parseSingleJSONValue(string(bytes.TrimSpace(call.ArgumentsJSON)))
		if err != nil || len(rawVal) == 0 || rawVal[0] != '{' {
			msg := fmt.Sprintf("invalid %s arguments: expected JSON object", MCPResourceToolName)
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}
		var req struct {
			ServerName string `json:"server_name"`
			URI        string `json:"uri"`
		}
		dec := json.NewDecoder(bytes.NewReader(rawVal))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			msg := fmt.Sprintf("invalid %s arguments: %v", MCPResourceToolName, err)
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}
		if strings.TrimSpace(req.ServerName) == "" || strings.TrimSpace(req.URI) == "" {
			msg := fmt.Sprintf("%s requires non-empty server_name and uri", MCPResourceToolName)
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}

		if _, ok := t.allowed[mcpResourceKey{serverName: req.ServerName, uri: req.URI}]; !ok {
			msg := fmt.Sprintf("unauthorized MCP resource (%s, %s): not declared in session mcp_resource_ids", req.ServerName, req.URI)
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}

		if t.reader == nil {
			msg := "MCP resource reader is not configured"
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}

		contents, err := t.reader(ctx, req.ServerName, req.URI)
		if err != nil {
			msg := fmt.Sprintf("read MCP resource (%s, %s): %v", req.ServerName, req.URI, err)
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}

		encoded, err := json.Marshal(contents)
		if err != nil {
			msg := fmt.Sprintf("marshal MCP resource contents: %v", err)
			return harnessruntime.ToolOutput{
				ResultType:       harnessruntime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}

		return harnessruntime.ToolOutput{
			ResultType:       harnessruntime.HostToolResultSuccess,
			TextResultForLLM: string(encoded),
		}, nil
	}
}
