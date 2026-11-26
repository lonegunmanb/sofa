// Package runtime defines the pure Go host domain abstractions for AgentHarness sessions
// and implements the containerized hashicorp/go-plugin AutoMTLS gRPC bridge.
package runtime

import (
	"context"
	"fmt"
	"time"
)

// ToolHandler executes a host-managed tool invocation (sofa_*, go_tool, external_tool, sofa_read_mcp_resource).
type ToolHandler func(ctx context.Context, call ToolInvocation) (ToolOutput, error)

// PreToolUseHook gates and optionally rewrites or augments non-host tool calls before execution.
type PreToolUseHook func(ctx context.Context, input PreToolUseInput) (PreToolUseOutput, error)

// PostToolUseHook observes non-host tool completion or failure for quota rollback and progress tracking.
type PostToolUseHook func(ctx context.Context, input PostToolUseInput) error

// Session represents an open AgentHarness conversation session.
type Session interface {
	ID() string
	RunTurn(ctx context.Context, prompt string, onEvent func(Event)) (TurnSummary, error)
	Abort(ctx context.Context) error
	Resume(ctx context.Context) error
	ReadMCPResource(ctx context.Context, serverName, uri string) ([]MCPResourceContent, error)
	Close(ctx context.Context) error
}

// Opener introspects harness capabilities and opens or resumes sessions.
type Opener interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Open(ctx context.Context, cfg SessionConfig) (Session, error)
}

// Capabilities describes the feature matrix reported by a harness plugin.
type Capabilities struct {
	HarnessName            string
	HarnessVersion         string
	ProtocolVersion        uint32
	SupportedProviderTypes []string
	SupportsSessionResume  bool
	SupportsMCPServers     bool
	SupportsMCPResources   bool
	SupportsSkills         bool
}

// SessionConfig configures an OpenSession call with materialized credentials and host hooks.
type SessionConfig struct {
	SessionID             string
	ResumeSessionID       string
	Model                 string
	Profile               string
	ReasoningEffort       string
	SystemPrompt          string
	WorkingDirectory      string
	SessionStoreDirectory string
	PermissionMode        string
	Provider              *ProviderConfig
	HarnessEnv            map[string]string
	HarnessOptionsJSON    []byte
	HostTools             []HostToolSpec
	AvailableTools        ToolFilter
	ExcludedTools         ToolFilter
	SkillDirectories      []string
	Skills                []string
	DisabledSkills        []string
	MCPServers            []MCPServerConfig
	PreToolUseHook        PreToolUseHook
	PostToolUseHook       PostToolUseHook
	ToolHandlers          map[string]ToolHandler
}

// ProviderConfig carries orthogonal model provider settings and materialized auth secrets.
type ProviderConfig struct {
	Type        string
	BaseURL     string
	WireAPI     string
	Transport   string
	Headers     map[string]string
	ModelID     string
	WireModel   string
	APIKey      string
	BearerToken string
}

// HostToolSpec declares a host-managed tool schema passed to the plugin.
type HostToolSpec struct {
	Name                 string
	Description          string
	ParametersJSONSchema []byte
}

// ToolFilter distinguishes an unconfigured filter (Configured=false) from an explicit empty list.
type ToolFilter struct {
	Configured bool
	Names      []string
}

// MCPServerConfig carries a credential-materialized MCP server definition.
type MCPServerConfig struct {
	Name    string
	Tools   []string
	Timeout time.Duration
	HTTP    *MCPHTTPConfig
	Stdio   *MCPStdioConfig
}

// MCPHTTPConfig configures an HTTP MCP server transport.
type MCPHTTPConfig struct {
	URL     string
	Headers map[string]string
}

// MCPStdioConfig configures a stdio MCP server transport.
type MCPStdioConfig struct {
	Command          string
	Args             []string
	Env              map[string]string
	WorkingDirectory string
}

// ToolInvocation carries a host tool call request from the plugin.
type ToolInvocation struct {
	ToolCallID    string
	ToolName      string
	ArgumentsJSON []byte
}

// HostToolResultType classifies host tool execution outcomes returned to the plugin.
type HostToolResultType int

const (
	HostToolResultUnspecified HostToolResultType = iota
	HostToolResultSuccess
	HostToolResultFailure
)

// ToolOutput carries the host tool handler response back to the plugin.
type ToolOutput struct {
	ResultType       HostToolResultType
	TextResultForLLM string
	HandlerError     string
}

// PreToolUseInput carries a non-host tool pre-execution hook request.
type PreToolUseInput struct {
	HookCallID    string
	ToolCallID    string
	ToolName      string
	ArgumentsJSON []byte
}

// PreToolUseOutput carries the host gate decision for a non-host tool call.
type PreToolUseOutput struct {
	Allow                 bool
	DenyReason            string
	ModifiedArgumentsJSON []byte
	AdditionalContext     string
}

// PostToolUseInput carries a non-host tool completion/failure notification.
type PostToolUseInput struct {
	HookCallID   string
	ToolCallID   string
	ToolName     string
	Succeeded    bool
	ErrorMessage string
}

// EventKind identifies streaming session event types emitted during RunTurn.
type EventKind int

const (
	EventUnspecified EventKind = iota
	EventAssistantMessageDelta
	EventAssistantMessage
	EventAssistantReasoningDelta
	EventAssistantReasoning
	EventAssistantUsage
	EventAssistantToolCallDelta
	EventToolExecutionStart
	EventToolExecutionProgress
	EventToolShellOutput
	EventToolExecutionComplete
	EventSubagentStarted
	EventSubagentCompleted
	EventSubagentFailed
	EventSessionWarning
	EventSessionIdle
)

// TokenUsage reports model token accounting metrics.
type TokenUsage struct {
	Model            string
	APICallID        string
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Event represents a single streaming event from a RunTurn execution.
type Event struct {
	EventID           string
	TimestampUnixNano int64
	Kind              EventKind
	MessageID         string
	ToolCallID        string
	ToolName          string
	SubagentName      string
	Text              string
	ArgumentsJSON     []byte
	ResultJSON        []byte
	ErrorMessage      string
	Usage             *TokenUsage
	RawSDKEventJSON   []byte
}

// TurnSummary captures the final response metadata of a completed RunTurn.
type TurnSummary struct {
	TurnID             string
	AssistantMessageID string
	FinalContent       string
}

// MCPResourceContent represents one content item returned by ReadMCPResource.
type MCPResourceContent struct {
	URI      string
	MIMEType string
	Text     string
	Blob     []byte
	Meta     map[string]string
}

// ErrorCategory classifies plugin and model errors for RetryPolicy decisions.
type ErrorCategory int

const (
	ErrorCategoryUnspecified ErrorCategory = iota
	ErrorCategoryTransient
	ErrorCategoryRateLimit
	ErrorCategoryAuthentication
	ErrorCategoryAuthorization
	ErrorCategoryQuota
	ErrorCategoryContextLimit
	ErrorCategorySessionNotFound
	ErrorCategoryUnsupportedCapability
	ErrorCategoryInternal
)

// HarnessError represents a structured business or model error returned by a harness plugin.
type HarnessError struct {
	Category       ErrorCategory
	HTTPStatusCode int32
	Message        string
}

// Error implements the standard error interface.
func (e *HarnessError) Error() string {
	if e == nil {
		return ""
	}
	if e.HTTPStatusCode > 0 {
		return fmt.Sprintf("harness error (category=%d status=%d): %s", e.Category, e.HTTPStatusCode, e.Message)
	}
	return fmt.Sprintf("harness error (category=%d): %s", e.Category, e.Message)
}
