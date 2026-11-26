# 分片 015：宿主机 `go_tool` 交叉编译与容器内 `go_tool` / `external_tool` / MCP 资源执行器

## 1. 目标与边界

实现 `worker` 与 `qc` 会话中所有用户自定义强类型工具及受控 MCP 资源读取的宿主机侧执行底座（对齐 `r42/internal/tool` 并升级为容器内隔离运行）：
1. **`go_tool` 宿主机交叉编译器与缓存（`internal/tool/runtime/compiler.go`）**：
   - 将 `plan.ToolSpec`（`Kind == "go"`）的内联源码包装为标准 `stdin JSON -> Invoke -> stdout ToolResponse JSON` 的 `package main` 程序（复用阶段一 `internal/tool/gotool` 的 AST 分析与注入类型约定）；
   - 按 `(source, dependencies, targetOS, targetArch)` 计算 `SHA-256` 缓存键，在宿主机交叉编译为 **`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`** 静态二进制，供启动容器时只读挂载至 `/.sofa/tools/<tool_id>`。
2. **`go_tool` 与 `external_tool` 统一执行与校验器（`internal/tool/runtime/executor.go`）**：
   - 宿主侧使用 `cty` 校验模型传入的 JSON 参数满足 `InputType`（含 `tool_use.input` 预设字段合并覆盖：注意**预设固定字段对大模型隐藏或由宿主机强制覆盖，大模型绝不能篡改 HCL `tool_use.input` 锁定的字段值！**）；
   - 通过可注入的 `ContainerExecFunc`（在生产中即 `013` 的 `podman exec -i -w /workspace/<working_dir> <cid> ...`）在活跃 `sandbox` 容器内运行工具；
   - 宿主侧解析返回的单个 JSON 值，校验 `Accepted` / `Issues` 协议不变量、`OutputType`、`QuotePaths` 原文存在性校验（核对引用的文件片段真实存在于容器挂载对应的宿主机 `workspace` 或 `imports` 文件中，防模型幻觉造假！）以及 HCL `postcondition`。
3. **MCP 声明资源受控读取工具（`internal/tool/runtime/mcp_resource.go`）**：
   - 当会话声明了非空 `mcp_resource_ids` 时，自动生成宿主工具 `sofa_read_mcp_resource`，校验请求 `(server_name, uri)` 在白名单内后转发给 `runtime.Session.ReadMCPResource`。

**前置依赖**：分片 `013`、`014`。

---

## 2. 涉及包与文件清单

- `internal/tool/runtime/compiler.go`：`Compiler`、`CompiledTool`、包装源码生成与并发安全编译缓存
- `internal/tool/runtime/executor.go`：`Executor`、`ContainerExecFunc`、`InvocationResult`、`QuotePaths` 校验与 `postcondition` 求值
- `internal/tool/runtime/mcp_resource.go`：`MCPResourceToolName = "sofa_read_mcp_resource"`、JSON Schema 与白名单读取处理器
- `internal/tool/runtime/executor_test.go`：Tier 1 单测（本机原生编译执行 + 假 `ContainerExecFunc` 全分支覆盖）
- `internal/tool/runtime/executor_integration_test.go`（`//go:build integration`）：Tier 2 交叉编译 Linux 静态 `go_tool` 挂入真实 Podman `alpine` 容器（无 Go 环境镜像）经 `podman exec -i` 跑通 `go_tool` 与 `external_tool`

---

## 3. 保姆级实现细则

### 3.1 `go_tool` 包装与交叉编译（`compiler.go`）
1. **包装模板（对齐 `r42/internal/tool/gotool/wrapper.go`）**：
   - 使用带下划线前缀别名导入标准库（如 `_sofacontext "context"`、`_sofajson "encoding/json"`），避免与用户内联源码的 `import` 冲突；
   - 注入 `Issue` 与 `ToolResponse[T]` 泛型结构体定义；
   - `main()` 使用 `decoder.DisallowUnknownFields()` 从 `os.Stdin` 读取**严格唯一一个 JSON 对象**解码为 `Input`（多于一个 JSON 值或存在未知字段立即向 `stderr` 报错并以退出码 `1` 退出）；
   - 调用 `Invoke(ctx, input)`，校验返回不变量：
     - `Accepted == true` 时 `len(Issues) == 0`；
     - `Accepted == false` 时 `Output == nil` 且 `len(Issues) >= 1`（每条 `Issue` 的 `Code` 与 `Message` 非空）；
   - 将合法响应编码为 JSON 输出至 `os.Stdout`。
2. **交叉编译命令**：
   - 在 `<cacheDir>/<sha256>/` 创建临时 `go.mod`（`module sofa.inline.tool`）；若有 `Dependencies` 先执行 `go get`；
   - 执行 `go build -trimpath -o <outPath> main.go`，环境变量设置 `CGO_ENABLED=0`、`GOOS=targetOS`、`GOARCH=targetArch`、`GO111MODULE=on`、`GOWORK=off`；
   - 编译产出的可执行文件权限设为 `0755`，准备挂载目录时按 `<stagingToolsDir>/<toolSpec.ID>` 硬链接或复制，容器内固定路径为 `"/.sofa/tools/" + toolSpec.ID`。

### 3.2 参数合并、容器执行与后置校验（`executor.go`）
1. **`tool_use` 绑定处理**：
   - 若工具由 `worker` 的 `tool_use "<alias>" { tool_id = ...; input = { ... } }` 挂载：
     - 暴露给大模型的工具名为 `<alias>`，下发的 JSON Schema 剔除已被 `tool_use.input` 预设的顶层属性（若全部属性均已预设则下发空对象 Schema `{ "type": "object", "additionalProperties": false }`）；
     - 收到调用时，宿主机将 `tool_use.input` 的预设 JSON 键值强制覆盖合并进参数对象，再按完整 `InputType` 转换校验。
2. **执行区分**：
   - `go_tool`：调用 `execFn(ctx, "/workspace", []string{"/.sofa/tools/" + spec.ID}, inputJSON)`：
     - 若退出码非 `0`：视为**工具执行异常（`HandlerError`）**（供上层回滚配额并记录错误日志）；
     - 若退出码为 `0` 且 `Accepted == false`：视为**模型可见的业务级拒绝（`HostToolResultType_SUCCESS`，非 `HandlerError`）**，将 `{"accepted": false, "issues": [...]}` 返回给大模型自修！
   - `external_tool`：调用 `execFn(ctx, path.Join("/workspace", spec.WorkingDir), spec.Program, inputJSON)`：
     - 退出码非 `0` 时包装 `stderr` 作为工具失败原因返回；退出码为 `0` 时解析 `stdout` JSON 并按 `OutputType` 转换校验。
3. **`QuotePaths` 防幻觉核验（仅针对 `Accepted == true` 的 `go_tool` 输出）**：
   - 遍历 `spec.QuotePaths` 提取输出对象中的所有 `{path, exact_quote}` 结构体：
     - 若 `path` 以 `"/.sofa/imports/"` 开头则在宿主机 `importsDir` 下只读打开对应文件，否则按相对路径在宿主机 `workspaceDir` 下通过 `os.OpenRoot` 打开文件；
     - 检查 `exact_quote` 非空且作为精确子串存在于目标文件 UTF-8 文本中！若文件不存在或子串不匹配，立即将该次调用降级为业务级拒绝 `{"accepted": false, "issues": [{"code": "quote_verification_failed", ...}]}` 打回给大模型，**绝不放行造假引用**！
4. **`postcondition` 求值**：
   - 构造只含 `input`（转换后的 `cty.Value`）与 `output`（转换后的 `cty.Value`）外加 `config.Functions` 的 `hcl.EvalContext`，逐条求值 `spec.Postconditions`：
     - 若任一条件求值为 `false`，同样返回业务级拒绝 `{"accepted": false, "issues": [{"code": "postcondition_failed", "message": cond.ErrorMessage}]}`（不触发进程崩溃，允许大模型修正重调）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`executor_test.go`）**：
   - 本机目标平台编译 `go_tool`（验证二次 `Compile` 命中缓存不重复调用 `go build`）；
   - `tool_use.input` 预设字段不能被模型入参篡改；
   - 覆盖 `Accepted: true`、`Accepted: false`（模型可见 issues）、`Invoke` 返回 Go `error`（映射为 `HandlerError`）、`QuotePaths` 命中真实文件 vs 伪造不存在引用被拦截、`postcondition` 失败返回 `ErrorMessage`；
   - `sofa_read_mcp_resource`：允许读取白名单内 `(server, uri)`，拒绝读取未在 `mcp_resource_ids` 授权的资源 URI。
2. **Tier 2 真实无 Go 容器跨平台执行测试（`executor_integration_test.go`，`//go:build integration`）**：
   - 选用**不含 Go 编译器**的 `alpine:latest` 启动 Podman 容器（挂载测试 `/workspace` 与交叉编译好的 `/.sofa/tools/`）；
   - 通过真实 `podman exec -i` 成功运行 `go_tool`（读取 `/workspace` 内文件并校验 `QuotePaths` + `postcondition`）与 `external_tool`（调用容器内 `/bin/sh -c` 脚本处理 JSON）。
