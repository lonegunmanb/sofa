# Data Model: AgentHarness Protocol & Go-Plugin Bridge (`014`)

## `pkg/harnessplugin` (Zero `internal/` Imports)

- `Handshake`: `plugin.HandshakeConfig{ProtocolVersion: 1, MagicCookieKey: "SOFA_HARNESS_PLUGIN", MagicCookieValue: "sofa-harness-v1"}`
- `PluginName = "agent_harness"`
- `GRPCPlugin`: implements `plugin.GRPCPlugin` (`Impl harnessv1.AgentHarnessServer`)
- `ScrubPluginEnv()`: unsets `SOFA_HARNESS_PLUGIN` and all `PLUGIN_*` env vars
- `Serve(impl harnessv1.AgentHarnessServer)`: serves `AgentHarness` over `AutoMTLS` gRPC across local subprocess, Linux UDS, and container `0.0.0.0:<port>` TCP modes

## `internal/sandbox/runtime` Extension

- `SessionMountLayout.HandshakeEnv []string`: strictly validated list of `KEY=VALUE` pairs where `KEY == "SOFA_HARNESS_PLUGIN"` or `strings.HasPrefix(KEY, "PLUGIN_")`. Any other key fails `BuildRunArgs`.

## `internal/harness/runtime` Domain Model (`types.go`, Zero `harnessv1` Imports)

### Interfaces
- `Opener`:
  - `Capabilities(ctx context.Context) (Capabilities, error)`
  - `Open(ctx context.Context, cfg SessionConfig) (Session, error)`
- `Session`:
  - `ID() string`
  - `RunTurn(ctx context.Context, prompt string, onEvent func(Event)) (TurnSummary, error)`
  - `Abort(ctx context.Context) error`
  - `Resume(ctx context.Context) error`
  - `ReadMCPResource(ctx context.Context, serverName, uri string) ([]MCPResourceContent, error)`
  - `Close(ctx context.Context) error`

### Value & Callback Types
- `Capabilities`: `HarnessName`, `HarnessVersion`, `ProtocolVersion`, `SupportedProviderTypes`, `SupportsSessionResume`, `SupportsMCPServers`, `SupportsMCPResources`, `SupportsSkills`
- `SessionConfig`: `SessionID`, `ResumeSessionID`, `Model`, `Profile`, `ReasoningEffort`, `SystemPrompt`, `WorkingDirectory`, `SessionStoreDirectory`, `PermissionMode`, `Provider *ProviderConfig`, `HarnessEnv map[string]string`, `HarnessOptionsJSON []byte`, `HostTools []HostToolSpec`, `AvailableTools ToolFilter`, `ExcludedTools ToolFilter`, `SkillDirectories []string`, `Skills []string`, `DisabledSkills []string`, `MCPServers []MCPServerConfig`, `PreToolUseHook`, `PostToolUseHook`, `ToolHandlers map[string]ToolHandler`
- `ProviderConfig`: `Type`, `BaseURL`, `WireAPI`, `Transport`, `Headers map[string]string`, `ModelID`, `WireModel`, `APIKey`, `BearerToken`
- `ToolFilter`: `{Configured bool, Names []string}`
- `MCPServerConfig`: `Name`, `Tools`, `Timeout time.Duration`, `HTTP *MCPHTTPConfig`, `Stdio *MCPStdioConfig`
- `HostToolSpec`, `ToolInvocation`, `ToolOutput` (`ResultType`, `TextResultForLLM`, `HandlerError`), `ToolHandler`
- `PreToolUseInput`, `PreToolUseOutput`, `PreToolUseHook`
- `PostToolUseInput`, `PostToolUseHook`
- `EventKind`, `TokenUsage`, `Event`, `TurnSummary`, `MCPResourceContent`
- `ErrorCategory`, `*HarnessError` (`Category`, `HTTPStatusCode`, `Message`)

## Container Runner & gRPC Client (`container_runner.go`, `client.go`)

- `ContainerRunner`: implements `runner.Runner` and `runner.AddrTranslator`
  - `NewContainerRunnerFunc(layout sandboxruntime.SessionMountLayout, runtimeBin string) func(hclog.Logger, *exec.Cmd, string) (runner.Runner, error)`
- Materialization helpers:
  - `MaterializeProviderConfig`, `MaterializeMCPServerConfig`, `MaterializeHarnessEnv`, `MarshalHarnessOptions`
- `GRPCClientOpener` & `GRPCSession`: implements `Opener` and `Session` over `plugin.Client` / `harnessv1.AgentHarnessClient`
