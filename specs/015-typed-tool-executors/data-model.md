# Data Model: Typed Tool Executors & Cross-Compiler Cache (`015-typed-tool-executors`)

## `Compiler` & `CompiledTool` (`internal/tool/runtime/compiler.go`)

| Entity.field | Rules |
|---|---|
| `CompilerOptions.CacheDir` | Host directory for cached binaries (defaults to temp cache dir when empty) |
| `CompilerOptions.TargetOS` | Target OS (`"linux"` for containers; defaults to `runtime.GOOS`) |
| `CompilerOptions.TargetArch` | Target architecture (`"amd64"` / `"arm64"`; defaults to `runtime.GOARCH`) |
| `CompilerOptions.GoVersion` | Toolchain version included in cache key and `go.mod` |
| `CompiledTool.ToolID` | `plan.ToolSpec.ID` |
| `CompiledTool.CacheKey` | SHA-256 hex over `(targetOS, targetArch, goVersion, wrappedSource, sortedDependencies)` |
| `CompiledTool.BinaryPath` | Cached executable path on host |
| `CompiledTool.ContainerPath` | `"/.sofa/tools/" + ToolID` |

## `Executor`, `ToolBinding` & `InvocationResult` (`internal/tool/runtime/executor.go`)

| Entity.field | Rules |
|---|---|
| `ToolBinding.Spec` | Planned `plan.ToolSpec` (`Kind == "go"` or `"external"`) |
| `ToolBinding.ExposedName` | Optional alias (`tool_use.<alias>`); defaults to `Spec.ID` |
| `ToolBinding.PresetInput` | Optional `cty.Value` object from `tool_use.input` |
| `ToolBinding.PresetInputJSON` | Optional JSON object alternative for preset input fields |
| `ToolBinding.Terminate` | Marks binding as a terminating tool for session callers (`016`) |
| `ToolBinding.Env` / `EnvRefs` | Static env and host env-var references for `external_tool` |
| `ToolBinding.Timeout` | Optional execution timeout for container `Exec` |
| `InvocationResult.ToolOutput` | `harnessruntime.ToolOutput` (`ResultType`, `TextResultForLLM`, `HandlerError`) |
| `InvocationResult.Accepted` | `true` iff tool execution, `OutputType`, `QuotePaths`, and `Postconditions` all succeed |
| `InvocationResult.ValidatedInput` | Converted `cty.Value` after preset merge and `InputType` constraint |
| `InvocationResult.ValidatedOutput` | Converted `cty.Value` matching `OutputType` when `Accepted == true` |
| `InvocationResult.OutputJSON` | Canonical JSON of `ValidatedOutput` when `Accepted == true` |
| `InvocationResult.Issues` | Structured `[]Issue` when `Accepted == false` |

## `MCPResourceSpec` & `MCPResourceTool` (`internal/tool/runtime/mcp_resource.go`)

| Entity.field | Rules |
|---|---|
| `MCPResourceSpec.ID` | Deterministic `mcp_resource_*` ID |
| `MCPResourceSpec.ServerName` | Authorized MCP runtime server name |
| `MCPResourceSpec.URI` | Exact authorized MCP resource URI |
| `MCPResourceToolName` | Constant `"sofa_read_mcp_resource"` |
