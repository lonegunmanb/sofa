# 分片 003：环境与提供方块（`model_provider`、`mcp_server`、`sandbox`）与 Root 单例默认机制

## 1. 目标与边界

注册三类运行环境基础设施块：
1. `model_provider "<name>"`（1:1 对齐 `r42` 模型协议、认证凭据脱敏与嵌套 `retry`）；
2. `mcp_server "<name>"`（1:1 对齐 `r42` `http`/`stdio` 传输、白名单工具与确定性 `mcp_tool_*` / `mcp_resource_*` ID）；
3. `sandbox "<name>"`（容器运行时 `podman`/`docker`/`nerdctl`/`kirk`、镜像、网络、只读 `mount` 与 `resources` 限制）；
4. 抽离通用的 **Root Module 单例默认解析器（`RootDefaults`）**，供后续 `005`（`worker`）、`006`（`dynamic_worker`）和 `007`（子 `module`）统一复用。

**前置依赖**：分片 `001`（与 `002` 正交，按序基于 `002` 递进）。

---

## 2. 包与文件清单

- `internal/provider/retry.go`：`RetryBlock`、`RetryPolicy`、`RetryOverride` 及合并规则
- `internal/provider/config.go`：`Config`、协议枚举（`openai`、`azure`、`anthropic`）、`wire_api`、`transport` 组合校验
- `internal/provider/schema.go`：`ModelProviderBlock`（实现 `golden.PlanBlock` + `golden.SingleValueBlock`）
- `internal/mcp/config.go`：`Config`、`HTTPConfig`、`StdioConfig` 校验
- `internal/mcp/spec/schema.go`：`ServerBlock`、`ToolID(tool)`、`ResourceID(uri)` 确定性哈希生成
- `internal/sandbox/spec/schema.go`：`SandboxBlock`、`SandboxConfig`、`MountSpec`、`ResourcesSpec`
- `internal/planner/defaults.go`：扩展 `RootDefaults`（管理 `source`、`sandbox`、`model_provider` 的 0/1/≥2 计数与 AST 注入助手）

---

## 3. 各块精确字段与校验规则

### 3.1 `model_provider "<name>"`（对齐 `r42/internal/provider`）

- **字段**：
  - `type`（必填 `string`）：仅允许 `"openai"` | `"azure"` | `"anthropic"`。
  - `endpoint`（必填 `string`）：合法 `http://` 或 `https://` 根地址（禁止路径以 `/chat/completions` 或 `/responses` 结尾，提示填写 API Root）。
  - `wire_api`（可选 `string`）：`"completions"`（默认）| `"responses"`；当 `type == "anthropic"` 时禁止设置！
  - `transport`（可选 `string`）：`"http"`（默认）| `"websockets"`；`websockets` 要求 `wire_api == "responses"`，且 `type == "anthropic"` 时禁止设置！
  - `headers`（可选 `map(string)`）：Plan 期必须全部已知（可包含敏感标记，保留路径级 `SensitiveMark`）。
  - 认证四选一（**至多设置其中一个**，否则报错）：`api_key`（字面量，自动加 `spec.MarkSensitive`）、`api_key_ref`（环境变量名）、`bearer_token`（字面量，加 `spec.MarkSensitive`）、`bearer_token_ref`（环境变量名）。
  - `retry`（至多 1 个嵌套块）：
    - `lifecycle_retries`（默认 `10`，`>= 0`）
    - `model_call_retries`（默认 `5`，`>= 0`）
    - `interval_seconds`（默认 `10`，`> 0`）
    - `max_interval_seconds`（默认 `180`，`>= interval_seconds`）
    - `error_message_regex`（`list(string)`，每项必须通过 Go `regexp.Compile` 校验！）
- **`Value() cty.Value`**：
  `{ address: "model_provider.<name>", kind: "model_provider", name: "<name>", retry: [...] }`。

---

### 3.2 `mcp_server "<name>"`（对齐 `r42/internal/mcp`）

- **字段**：
  - `tools`（可选 `list(string)`）与 `resources`（可选 `list(string)`）：**两者至少有一个非空**（不允许既不暴露任何 tool 也不暴露任何 resource 的空 MCP 服务），且各自内部不得有空串或重复项。
  - `timeout`（可选 `string`）：默认 `"30s"`，Go duration 解析后必须在 `[1s, 5m]` 闭区间内。
  - 传输块（`http` 与 `stdio` **必须且只能出现共 1 个**）：
    - `http`：必填 `url`（`http`/`https`），可选 `headers`（`map(string)`），可选互斥的 `bearer_token` / `bearer_token_ref`。
    - `stdio`：必填非空 `command`，可选 `args`（`list(string)`）、`env`（`map(string)`）、`env_refs`（`map(string)`，值均为合法环境变量名，且键不得与 `env` 重复）、`working_directory`（`string`）。
- **确定性 ID 算法（必须基于含模块前缀的 `CanonicalAddress()` 计算 SHA-256，防止不同子模块同名 `mcp_server` 冲突）**：
  - `ToolID(tool)`：`"mcp_tool_" + sanitize(name) + "__" + sanitize(tool) + "_" + uuidFromSHA256("sofa/mcp-tool-id/v1\x00" + canonicalAddr + "\x00" + tool)`
  - `ResourceID(uri)`：`"mcp_resource_" + sanitize(name) + "__" + sanitize(uri) + "_" + uuidFromSHA256("sofa/mcp-resource-id/v1\x00" + canonicalAddr + "\x00" + uri)`
- **`Value() cty.Value`**：
  `{ address, kind: "mcp_server", name, tool_ids: map(string), resource_ids: map(string) }`。

---

### 3.3 `sandbox "<name>"`（`sofa` 新增块）

> **演进说明（分片 `004b` 更新）**：分片 `003` 初始落地时 `network` 默认值为 `"none"`；在分片 [004b-harness-and-sandbox-bridge.md](./004b-harness-and-sandbox-bridge.md) 中，`sandbox.network` 的默认值已统一调整为 **`"bridge"`**（仍可显式配置为 `"bridge"` | `"none"` | `"host"`），同时 `RootDefaults` 扩展增加了 `Harnesses`。

```hcl
sandbox "go_env" {
  runtime    = "podman"
  image      = "ghcr.io/lonegunmanb/sofa-go:1.27"
  entrypoint = ["/sofa/bin/entrypoint"] # 可选 list(string)
  network    = "none"                   # 可选: "none"(默认) | "bridge" | "host"

  mount {
    source    = "/var/cache/sofa/gomod"
    target    = "/go/pkg/mod"
    read_only = true # 默认 true；sandbox 模板级额外挂载仅允许 read_only = true！
  }

  resources {
    cpus   = "4"
    memory = "8g"
  }
}
```

- **`golden` 接口实现**：`golden.PlanBlock` + `golden.SingleValueBlock`（`Type() == ""`，`BlockType() == "sandbox"`，`AddressLength() == 2`）。
- **字段校验约束**：
  - `runtime`（必填 `string`）：枚举 `"podman"` | `"docker"` | `"nerdctl"` | `"kirk"`。
  - `image`（必填非空 `string`）：禁止包含空白字符。
  - `entrypoint`（可选 `list(string)`）：若显式提供则不能为空列表，且首元素不得为空白串。
  - `network`（可选 `string`）：默认 `"none"`，枚举 `"none"` | `"bridge"` | `"host"`。
  - `mount`（0..N 个嵌套块）：
    - `source`（必填非空 `string`）
    - `target`（必填绝对容器内路径，必须以 `/` 开头，清理后不得为 `/` 根目录或与系统保留挂载点 `/workspace`、`/.sofa` 冲突，且多个 `mount` 的 `target` 不得重复！）
    - `read_only`（可选 `bool`，默认 `true`；**安全硬约束**：根据 README *“除明确共享的项目卷和只读运行时材料外，其他文件系统必须隔离、各自私有”*，`sandbox` 块中的宿主机额外挂载若设置 `read_only = false` 直接 Plan 报错！项目可读写范围由 `worker.writable_paths` 控制）。
  - `resources`（0..1 个嵌套块）：
    - `cpus`（可选正浮点数字符串，如 `"0.5"`、`"4"`）
    - `memory`（可选容器内存格式，匹配 `^[1-9][0-9]*(b|k|m|g|kb|mb|gb|kib|mib|gib)?$`，大小写不敏感）。
- **`Value() cty.Value`**：
  `{ address: "sandbox.<name>", kind: "sandbox", name: "<name>", runtime: ..., image: ..., network: ... }`。

---

### 3.4 `RootDefaults` 单例解析器（在 `internal/planner/defaults.go` 中实现并单测）

提供纯函数：
```go
type SingletonStatus uint8
const (
    SingletonNone SingletonStatus = iota
    SingletonUnique
    SingletonAmbiguous
)

type RootDefaults struct {
    Sources        []string // e.g. ["project"]
    Sandboxes      []string // e.g. ["go_env"]
    ModelProviders []string // e.g. ["primary"]
}

func InspectRootDefaults(blocks []*golden.HclBlock) RootDefaults
```
并提供辅助方法 `ResolveDefaultSandbox(consumerAddr string) (refName string, err error)` 与 `ResolveDefaultModelProvider(consumerAddr string) (refName *string, err error)`：
- **`sandbox`**：`0` 个报错，`1` 个返回唯一名称，`>=2` 个报歧义错误（列出所有候选名）；
- **`model_provider`**：`0` 个返回 `(nil, nil)`（代表使用 Copilot SDK 默认提供方），`1` 个返回唯一名称，`>=2` 个报歧义错误（列出所有候选名）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **`model_provider`**：`anthropic` 配 `wire_api` 报错；`websockets` 配 `completions` 报错；同时设 `api_key` 和 `api_key_ref` 报错；非法正则 `error_message_regex = ["["]` 报错；字面量 `api_key` 携带 `golden.SensitiveMark`。
2. **`mcp_server`**：同时写 `http` 和 `stdio` 报错；`tools` 与 `resources` 同时为空报错；`timeout = "10m"` 超上限报错；`mcp_server.idx.tool_ids["search"]` 返回稳定格式 ID，且不同 `CanonicalAddress` 前缀产出不同 UUID。
3. **`sandbox`**：非法 `runtime` 报错；`mount.target` 非绝对路径、重复路径、挂载到保留路径 `/workspace` 或 `/.sofa` 报错；`mount { read_only = false }` 报错；多个 `resources` 块报错。
4. **`RootDefaults` 单元测试**：完整覆盖 `0 / 1 / 2` 个 `sandbox` 与 `model_provider` 的全部返回分支与错误信息可读性。
