# Data Model: `019-plugin-copilot`

## Core Types (`plugins/sofa-harness-copilot/internal/server`)

### 1. `sdkClient` & `sdkSession` (`sdk_iface.go`)
- `sdkClientFactory`: `func(cliPath string, env []string) (sdkClient, error)` — injectable in unit tests; production creates `sdk.NewClient(&sdk.ClientOptions{Connection: sdk.StdioConnection{Path: cliPath, Env: env}})`.
- `sdkClient`:
  - `Start(ctx context.Context) error`
  - `Stop() error`
  - `CreateSession(ctx context.Context, cfg *sdk.SessionConfig) (sdkSession, error)`
  - `ResumeSession(ctx context.Context, sessionID string, cfg *sdk.ResumeSessionConfig) (sdkSession, error)`
- `sdkSession`:
  - `ID() string`
  - `SendAndWait(ctx context.Context, opts sdk.MessageOptions) (*sdk.SessionEvent, error)`
  - `On(handler sdk.SessionEventHandler) func()`
  - `Abort(ctx context.Context) error`
  - `Disconnect() error`
  - `ReadMCPResource(ctx context.Context, serverName, uri string) ([]*harnessv1.MCPResourceContent, error)`

### 2. `managedSession` (`server.go`)
- Holds per-session state indexed by `session_id`:
  - `id string`
  - `client sdkClient`
  - `sdk sdkSession`
  - `resumeCfg *sdk.ResumeSessionConfig`
  - `hostToolNames map[string]struct{}`
  - `turnMu sync.Mutex` — ensures at most one active `RunTurn` per session
  - `activeMu sync.RWMutex` — guards `active *activeTurn` so `AbortTurn` never blocks on `turnMu`

### 3. `activeTurn` (`server.go`)
- Represents a single in-flight `RunTurn` stream:
  - `turnID string`
  - `ctx context.Context` / `cancel context.CancelFunc`
  - `stream harnessv1.AgentHarness_RunTurnServer`
  - `sendMu sync.Mutex` — serializes all `stream.Send(...)` calls across concurrent SDK events, hooks, and host tool handlers
  - `pendingMu sync.Mutex`
  - `pendingHooks map[string]chan *harnessv1.HostTurnFrame`
  - `pendingTools map[string]chan *harnessv1.InvokeHostToolResponse`
  - `toolNames map[string]string` (maps `toolCallID` -> `toolName` for `ToolExecutionProgress` / `ToolShellOutput` / `ToolExecutionComplete`)

### 4. Error Classification (`errors.go`)
- `classifyError(err error) *harnessv1.ErrorDetail`:
  - Extracts `*sdk.SessionErrorData` (via `modelCallError`), `*sdk.RPCError`, or `HTTPStatusCode() int` interfaces, plus message substring fallback.
  - Maps cleanly to `ERROR_CATEGORY_RATE_LIMIT`, `ERROR_CATEGORY_AUTHENTICATION`, `ERROR_CATEGORY_AUTHORIZATION`, `ERROR_CATEGORY_QUOTA`, `ERROR_CATEGORY_CONTEXT_LIMIT`, `ERROR_CATEGORY_SESSION_NOT_FOUND`, `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY`, `ERROR_CATEGORY_TRANSIENT`, and `ERROR_CATEGORY_INTERNAL`.
