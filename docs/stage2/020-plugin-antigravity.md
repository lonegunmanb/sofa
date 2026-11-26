# 分片 020：同仓隔离孵化外部插件 `sofa-harness-antigravity`（Python + `google-antigravity`）

## 1. 目标与边界

验证并落地 `sofa` 的跨语言外部插件架构：在完全独立的 Python 工程 `plugins/sofa-harness-antigravity/` 中，按照 `hashicorp/go-plugin` 官方 `examples/grpc/plugin-python` 模式实现 Google Antigravity Agent Harness 插件：
1. **标准 `go-plugin` 非 Go 语言握手（零 HashiCorp 私有 `.proto`）**：
   - 使用标准 `grpc.aio` + `grpcio-health-checking`（注册 `"plugin"` → `SERVING`）+ `cryptography`（解析 `PLUGIN_CLIENT_CERT` PEM 客户端证书，签发单次 ECDSA P-256 服务端证书并以裸 DER Base64 输出握手行 `1|1|...|grpc|<der_b64>`），握手准备完毕立即清空进程 `os.environ` 中的 `PLUGIN_*` 与 `SOFA_HARNESS_PLUGIN` 变量。
2. **`google-antigravity` Python SDK 完整映射**：
   - 将 `OpenSessionRequest` 映射为 `LocalAgentConfig` 与 `CapabilitiesConfig`（`permission_mode == "read_only"` 时禁用写工具，`"approve_all"` 时开启写能力），并将 `harness_env`（如 `GEMINI_API_KEY`）仅注入 SDK 会话客户端配置而不泄漏给命令执行环境；
   - 在异步 `RunTurn` 双向流上并发消费 `response`（文本增量）、`response.thoughts`（思考增量）与 `response.tool_calls`，将 `host_tools` 包装为 `asyncio.Future` 回调 `InvokeHostToolRequest`，将内置工具前置/后置包装为 `PreToolUseRequest` / `PostToolUseRequest`。
3. **自包含可重定位 Linux Python 打包与三层测试**：
   - 提供容器化打包脚本，将可重定位 Linux Python 运行时 + `grpcio` + `grpcio-health-checking` + `cryptography` + `google-antigravity`（含其平台编译 wheel 二进制）与顶层 POSIX 启动脚本 `sofa-harness-antigravity` 封装为自包含目录，使裸 `golang:1.27` / `debian` 容器无需预装 Python 即可通过 `--entrypoint /.sofa/harness/sofa-harness-antigravity` 直接启动！

**前置依赖**：分片 `014`、`018`。

---

## 2. 目录与文件清单

```text
plugins/sofa-harness-antigravity/
├── pyproject.toml                          # 独立 Python 包定义与 pytest 配置
├── bin/
│   └── sofa-harness-antigravity            # 顶层 POSIX /bin/sh 入口脚本 (0755)
├── src/
│   └── sofa_harness_antigravity/
│       ├── __init__.py
│       ├── __main__.py                     # CLI 主入口: AutoMTLS 握手 + grpc.aio Server
│       ├── handshake.py                    # 严格实现 014 第 3.2 节的 5 步 AutoMTLS + DER 握手与 env 擦除
│       ├── backend.py                      # AgentBackend 抽象协议 + GoogleAntigravityBackend 真实适配器
│       ├── servicer.py                     # AgentHarnessServicer 7 个异步 RPC 完整实现
│       └── proto/                          # 由根 buf.gen.yaml 生成并提交的 harness_pb2.py / harness_pb2_grpc.py
├── tests/
│   ├── test_handshake.py                   # 握手串格式、mTLS 证书校验与 PLUGIN_* 环境变量擦除单测
│   └── test_servicer.py                    # 注入 FakeAgentBackend 的 7 个 RPC 与双向流 pytest 单测
└── scripts/
    └── build_linux_bundle.py               # 生成带独立 Linux Python 环境的自包含目录
```
以及主仓跨语言容器集成测试：`internal/worker/runner/antigravity_plugin_integration_test.go`（`//go:build integration`）。

---

## 3. 保姆级实现细则

### 3.1 `handshake.py`（逐行对齐 `go-plugin@v1.6.0`）
1. 核对 `os.environ.get("SOFA_HARNESS_PLUGIN") == "sofa-harness-v1"`，不符则向 `sys.stderr` 打印提示并以退出码 `1` 退出。
2. 读取 `client_cert_pem = os.environ["PLUGIN_CLIENT_CERT"].encode("utf-8")` 及可选 `unix_dir = os.environ.get("PLUGIN_UNIX_SOCKET_DIR")`、`min_port = int(os.environ.get("PLUGIN_MIN_PORT", "19500"))`；随即遍历 `list(os.environ.keys())`，凡 `k.startswith("PLUGIN_")` 或 `k == "SOFA_HARNESS_PLUGIN"` 一律 `os.environ.pop(k, None)`！
3. 使用 `cryptography.x509` 生成 `ec.SECP256R1()` 密钥对与有效期 24 小时的自签名证书（`SubjectAlternativeName([DNSName("localhost"), IPAddress(ip_address("127.0.0.1"))])`）：
   - 传给 `grpc.ssl_server_credentials([(server_key_pem, server_cert_pem)], root_certificates=client_cert_pem, require_client_auth=True)`；
   - 取 `server_cert_der = cert.public_bytes(serialization.Encoding.DER)`，计算 `cert_b64 = base64.b64encode(server_cert_der).decode("ascii")`（**注意：必须是无换行的单行纯 Base64 裸 DER！**）。
4. 注册 `HealthServicer` 并调用 `health.set("plugin", HealthCheckResponse.SERVING)`：
   - 若 `unix_dir` 非空：绑定 `f"unix://{unix_dir}/harness.sock"`，握手串输出 `f"1|1|unix|{unix_dir}/harness.sock|grpc|{cert_b64}\n"`；
   - 否则：绑定 `f"0.0.0.0:{min_port}"`，握手串输出 `f"1|1|tcp|0.0.0.0:{min_port}|grpc|{cert_b64}\n"`；
   - 调用 `sys.stdout.write(line); sys.stdout.flush()`，随后的所有业务日志统一只写 `sys.stderr`！

### 3.2 `servicer.py` 与 `backend.py` 核心逻辑
1. **`GetCapabilities`**：
   - 返回 `harness_name="antigravity"`、`protocol_version=1`、`supported_provider_types=[]`（默认仅使用自身原生 Gemini/Vertex 认证；若 `OpenSession` 传入了不支持的非空外部 `provider`，立即返回 `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY`！）、`supports_session_resume=True`、`supports_mcp_servers=True`、`supports_skills=True`。
2. **`OpenSession` 与 `host_tools` 动态工具注册**：
   - 根据 `permission_mode` 构造 `CapabilitiesConfig`（`"read_only"` 下关闭文件编辑与命令执行能力；`"approve_all"` 下开启）；
   - 将每个 `HostToolSpec`（`name`, `description`, `parameters_json_schema`）动态包装为异步 Python 工具函数挂载到 `LocalAgentConfig`：调用时向当前 `RunTurn` 的输出异步队列推入 `PluginTurnFrame(invoke_host_tool=...)`，并在 `pending_tool_futures[tool_call_id]` 上 `await` 宿主机发回的 `InvokeHostToolResponse`；
   - 对非 `host_tools` 的内置工具执行包装钩子：执行前向输出队列发 `PreToolUseRequest` 并 `await` `PreToolUseDecision`（`allow == False` 则直接将 `deny_reason` 返回给模型而不执行底层工具），执行完成后发 `PostToolUseRequest(succeeded=...)`。
3. **顶层入口脚本 `bin/sofa-harness-antigravity`**：
   ```sh
   #!/bin/sh
   set -eu
   SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
   if [ -x "$SELF_DIR/python/bin/python3" ]; then
     exec "$SELF_DIR/python/bin/python3" -m sofa_harness_antigravity "$@"
   fi
   exec python3 -m sofa_harness_antigravity "$@"
   ```
   - 正式打包目录自带 `$SELF_DIR/python/bin/python3`（在无 Python 的裸 `golang:1.27` 容器中也 100% 直接运行）；开发/容器联调若使用含 Python 的测试镜像或挂载 venv 时也能无缝运行！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Python 独立离线单测（`pytest plugins/sofa-harness-antigravity`）**：
   - `test_handshake.py`：验证 `PLUGIN_*` 环境变量擦除、生成的 DER 证书可被客户端验证、未持客户端私钥的连接握手失败；
   - `test_servicer.py`：注入 `FakeAgentBackend` 完整覆盖 `GetCapabilities`、`OpenSession`（含传入 `openai` `ProviderConfig` 时精确返回 `UNSUPPORTED_CAPABILITY`）、`RunTurn`（并发推送思考流 + 文本流 + 内置工具 `PreToolUse` 拒绝/放行 + 宿主工具 `InvokeHostTool` 往返）、`AbortTurn` 取消挂起任务及 429/401/403 状态码分类。
2. **Tier 2 Go 宿主机 ↔ Podman 容器内 Python 插件真实联调（`antigravity_plugin_integration_test.go`，`//go:build integration`）**：
   - 构建 `sofa-harness-antigravity` 插件包放入临时 `<plugin_dir>/sofa-harness-antigravity/`（通过测试环境变量启用内置确定性脚本桩 `FakeAgentBackend`），在 HCL 声明 `harness "antigravity" {}` + `model_provider = null`；
   - 由 Go 宿主机 `018` `WorkerRunner` 在真实 Podman 容器内以 `--init --entrypoint /.sofa/harness/sofa-harness-antigravity` 拉起 Python 插件，完成跨语言 `hashicorp/go-plugin` `AutoMTLS` 握手、代码修改、`sofa_checkpoint`、容器 `gatekeeper` 与只读 `QC` 全流程闭环！
