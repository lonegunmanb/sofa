# Data Model

## Provider

| Entity.field | Rules |
|---|---|
| `ModelProvider.type` | openai, azure, anthropic |
| `ModelProvider.endpoint` | known HTTP(S) API root |
| `ModelProvider.wire_api` | completions default, responses allowed |
| `ModelProvider.transport` | http default, websockets requires responses |
| `ModelProvider.headers` | wholly known map(string), marks retained |
| `ModelProvider.auth` | exactly zero or one of API key/token literal or reference |
| `RetryPolicy` | defaults 10/5/10s/180s; nonnegative counts; positive interval; regex compiled |

## MCP server

| Entity.field | Rules |
|---|---|
| `MCPServer.tools/resources` | at least one; no empty or duplicates |
| `MCPServer.timeout` | 30s default, 1s..5m |
| `MCPServer.transport` | exactly one HTTP or stdio |
| `MCPServer.tool_ids/resource_ids` | deterministic canonical-address-scoped IDs |

## Sandbox

| Entity.field | Rules |
|---|---|
| `Sandbox.runtime` | podman, docker, nerdctl, kirk |
| `Sandbox.image` | known, non-empty, no whitespace |
| `Sandbox.network` | none default, bridge or host |
| `MountSpec.target` | absolute, clean, unique, outside `/workspace` and `/.sofa` |
| `MountSpec.read_only` | must be true |
| `ResourcesSpec` | at most one; positive CPU; bounded memory format |

## RootDefaults

| Entity.field | Rules |
|---|---|
| `RootDefaults.Sources` | ordered source names |
| `RootDefaults.Sandboxes` | ordered sandbox names |
| `RootDefaults.ModelProviders` | ordered provider names |
