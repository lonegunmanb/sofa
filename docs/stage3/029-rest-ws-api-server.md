# 分片 029：双端口隔离、统一 REST JSON + WebSocket 管理服务与 CSP 预览网关（`internal/server`）

## 1. 目标与边界

将控制面 Actors、Dapr PubSub 实时事件流与静态前端资源封装为安全、幂等的统一 HTTP/WebSocket 网关（供 `030` CLI、`031` Vue 3 看板及外部系统共用）：
1. **双端口物理隔离（防绕过鉴权直调 Actor）**：
   - **内部 Dapr 回调监听（`127.0.0.1:<daprAppPort>`）**：仅挂载 `dapr/go-sdk/service/http`（启用 `APP_API_TOKEN` 校验），供本地 `daprd` 调用 Actor 与投递 `sofa-events` PubSub 消息；
   - **对外公共监听（默认 `127.0.0.1:8080`）**：仅挂载 `/healthz`、`/api/v1/*` 与静态 UI Handler，绝不挂载 `/actors/*` 或 `/dapr/*`！
2. **轻量 Bearer Token + 严格同源防跨站鉴权**：
   - 当配置了 `SOFA_API_TOKEN`（正式 `sofa server` 或监听非 `127.0.0.1`/`::1` 地址时**启动强制校验非空**）：所有 `/api/v1/*` 请求校验 `Authorization: Bearer <token>`（WebSocket 支持 `Sec-WebSocket-Protocol` 或同源帧握手，以及受限只读预览一次性短期签名票据防 URL 泄漏长效 Token）；
   - 即便在 `--dev` 免 Token 监听 `127.0.0.1` 时：所有写请求（`POST`/`PUT`/`DELETE`）与 `/api/v1/ws` 强制核对 `Origin` 头（若存在，主机名必须精确匹配当前服务端 `Host`，**显式拒绝 `Origin: null` 与任意外部域名**！），彻底阻断浏览器恶意网页或沙箱 `iframe` 发起本地 CSRF 攻击。
3. **Agent 生成 HTML 产物的零信任 CSP 预览端点**：
   - `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content` 下发强制安全响应头：
     `Content-Security-Policy: sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'` + `X-Content-Type-Options: nosniff`，从浏览器内核层断绝网络外连与凭据窃取！

**前置依赖**：分片 `023`、`028`。

---

## 2. 涉及包与文件清单

- `internal/server/auth.go`：监听地址安全检查、Bearer Token 中间件、`Origin` 防跨站中间件与短期预览票据
- `internal/server/ws_hub.go`：消费 Dapr PubSub `sofa-events` 并按全局 / `run_id` 扇出推送的 WebSocket Hub
- `internal/server/handlers.go` & `server.go`：全量 `/api/v1/*` 路由处理、`.sofaplan` 解包 + `023` 第 0 秒 Preflight 拦截、Sensitive 输出脱敏与优雅关停
- `internal/server/server_test.go`：Tier 1 全端点契约、鉴权/`Origin: null` 拦截、Sensitive 输出掩码、CSP 响应头与 WS 扇出单测
- `internal/server/server_integration_test.go`（`//go:build integration`）：Tier 2 真实 `daprhost` 下通过 HTTP 上传 `.sofaplan`、订阅 WebSocket 收事件流、查询状态与 `DELETE` 清理

---

## 3. REST + WebSocket 完整端点表

| 方法与路径 | 核心语义 |
| :--- | :--- |
| `POST /api/v1/runs` | `multipart` 或二进制上传 `.sofaplan`（可选 `namespace`、Run 层 `envs`/`secrets`），先调 `023` `PreflightCheck`（缺失返回 `422` + `missing_keys`），通过则存入 `BlobStore` 并启动 `RunActor`，返回 `201 {run_id}` |
| `GET /api/v1/runs` / `GET /api/v1/runs/{run_id}` | 列表与详情（`outputs` 中 `sensitive = true` 字段默认掩码为 `"<sensitive>"`，仅显式 `?reveal_sensitive=true` 且通过鉴权时返回原值） |
| `POST /api/v1/runs/{run_id}/pause` | `{"mode": "planned" \| "immediate"}` |
| `POST /api/v1/runs/{run_id}/resume` | 解除手动暂停或让 `FAILED` 节点从最近完整检查点续跑 |
| `DELETE /api/v1/runs/{run_id}` | 停止并执行全量延迟清理（中间 Git 分支 + `BlobStore` + Run 层凭据） |
| `GET /api/v1/runs/{run_id}/nodes/{node_id}` | 节点轮次历史、检查点、结构化日记、产物元数据；`.../logs` 流式读取 `BlobStore` JSONL 日志 |
| `POST /api/v1/runs/{run_id}/nodes/{node_id}/rerun` | 单节点重跑（已合入共享分支返回 `409 Conflict`） |
| `POST /api/v1/runs/{run_id}/nodes/{node_id}/diary-messages` | 写入人工插话（`{"message": "...", "wait_for_confirmation": bool}`） |
| `GET /api/v1/decisions` / `POST /api/v1/decisions/{id}/respond` | 待办人工决策列表与提交回答/批准/退回上游 |
| `GET/PUT/DELETE /api/v1/envs` & `/api/v1/secrets` | 三层（`global` / `namespace`）受管配置与只写密钥 CRUD（`GET /api/v1/secrets` 严禁含 `value` 字段！） |
| `GET /api/v1/runs/{run_id}/artifacts/{artifact_id}/content` | 流式读取产物内容并附加严格 `CSP` 隔离头 |
| `GET /api/v1/ws?run_id=<optional>` | 实时推送状态迁移、分级日志、日记与新决策通知 |

---

## 4. 必测 TDD Red → Green 用例清单

1. **安全边界单测（`server_test.go`）**：
   - 非 `127.0.0.1` 监听且未设 `SOFA_API_TOKEN` 时 `NewServer` 直接报错拒绝启动；
   - `--dev` 免密模式下携带 `Origin: null` 或 `Origin: https://evil.example` 调用 `POST /api/v1/secrets` / `POST /api/v1/decisions/{id}/respond` 返回 `403 Forbidden`；
   - 公共端口请求 `/actors/ConfigActor/global/method/ResolveForNode` 返回 `404`；
   - `GET /api/v1/secrets` 响应 JSON 绝不含明文；`GET .../artifacts/{id}/content` 必含 `sandbox allow-scripts` 与 `connect-src 'none'` CSP 头。
2. **缺失凭据第 0 秒拦截**：上传引用未配置 `GEMINI_API_KEY` 的 `.sofaplan`，`POST /api/v1/runs` 立即返回 `422` 并精确列出 `["GEMINI_API_KEY"]`。
