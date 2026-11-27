# 阶段三（Stage 3）开发总纲：Dapr Actor 控制面、三层凭据管理、统一 API、安全看板与自举模板

> **面向后续开发 Agent 的强制阅读说明**：
> 阶段三包含 **按序递进的 12 个单职责微切片（`021`–`032`）**，所有关键设计分支均已通过 `/grill-me` 冻结。在为任一切片执行 `/speckit-specify`、`/speckit-plan`、`/speckit-tasks` 或 `/speckit-implement` 之前，**必须完整阅读本总纲及对应编号的分片蓝图**，严格按文档给定的包路径、结构体/接口签名、安全不变量与必测 Red → Green 用例落地，**严禁自行发明替代架构或合并多个分片**。

---

## 1. `/grill-me` 冻结的 8 条核心架构契约

1. **单一 `sofa` 二进制与三种运行角色（`cmd/sofa`）**：
   - `sofa server --dev`：单机全栈合体模式（自动托管本地 Dapr `scheduler --etcd-embed --placement-enabled` + `daprd` + `state.sqlite` + `blob.LocalStore` + 控制面 Actors + REST/WebSocket API + 内嵌看板 + 本地内置 Worker，状态持久化于 `--state-dir`，宿主机重启后由持久化 Reminder 与 `OperatorActor` 自动换 `epoch` 断点续跑）；
   - `sofa server`：纯控制面服务（连接外部配置的 Dapr 与 `BlobStore`，零本地容器执行）；
   - `sofa worker`：独立宿主机算力节点（注册 Dapr App ID `sofa-worker-<process_id>`）。
2. **Dapr 双端口物理隔离与原生透明落盘加密**：
   - **严禁将 Dapr Actor/PubSub 回调路由暴露在对外公共 API 端口上**：进程内部单独绑定 `127.0.0.1:<dapr-app-port>`（强制随机 `APP_API_TOKEN` 仅对本地 `daprd` 开放）承载 `dapr/go-sdk` 服务回调；公共监听端口（默认 `127.0.0.1:8080`）仅暴露 `/api/v1/*`、`/healthz` 与内嵌看板静态资源。
   - 落盘加密直接采用 Dapr State Store 原生 **`primaryEncryptionKey` / `secondaryEncryptionKey`** 客户端 AES-GCM 透明加密（`--dev` 下自动生成 `0600` 本地密钥文件与 `secretstores.local.file` 绑定，`sofa` 业务层零手写加解密代码）。
3. **三层受管 `Env` / `Secret` 与第 0 秒 Preflight 预检**：
   - 作用域覆盖链：**`Run` 单次层 > `Namespace` 项目命名空间层（默认 `"default"`，免预建实体） > `Global` 全局层**；
   - 同一层内键名互斥（不可同时为 `Env` 与 `Secret`），`Secret` 严格 **Write-Only**（任何读取/列表 API 仅返回键名与更新时间，绝不回传明文）；
   - 运行期严格只从受管库解析 `*_ref` / `env_refs`（不隐式回退读取 Worker `os.Environ()`，CLI 提供 `sofa secret set --from-env` 快捷导入）；`sofa apply` 提交瞬间核对三层合并视图，缺失任意引用变量立即在第 0 秒报错拦截。
4. **轻量 Bearer Token 与防跨站鉴权**：
   - `sofa server --dev` 默认监听 `127.0.0.1` 免输 Token（同时对写接口与 `/api/v1/ws` 实施严格同源 `Origin` 校验，阻断浏览器恶意页面或 `null` 源沙箱 `iframe` 跨站调用）；
   - 正式 `sofa server` 或监听非回环地址时强制要求配置 `SOFA_API_TOKEN`（看板存入 `sessionStorage`）。
5. **`sofa apply` 双输入与 `Ctrl+C` 即时中断语义**：
   - 同时支持 `sofa apply <file.sofaplan>` 与 `sofa apply [dir]`（客户端本地即时编译 `.sofaplan` 后上传，服务端永不编译 HCL）；
   - 默认前台订阅 WebSocket 实时打印进度与日志，**按 `Ctrl+C` 默认立即向服务端发送 `POST /api/v1/runs/{run_id}/pause?mode=immediate` 中断在途容器**；后台提交显式使用 `-d / --detach`。
6. **Worker 槽位调度防雪崩**：
   - `ProcessRegistryActor` 按空闲槽位最少负载分配任务；`--max-concurrency` 在独立 `sofa worker` 默认 **`10`**，在 `sofa server --dev` 默认 **`2`**（全部满载时就绪节点在 `RunActor` 排队，同时遵守 `module.parallelism`）。
7. **延迟统一清理生命周期**：
   - `sofa/ckpt/*` 检查点分支、`sofa/work/*` 工作分支及 `BlobStore` 产物/日志 JSONL 在 `SUCCEEDED` / `FAILED` / `PAUSED` 时一律保留（保障随时查阅、`sofa resume` 与单节点 `rerun`），仅在显式执行 **`DELETE /api/v1/runs/{run_id}`（`sofa delete <run_id>`）** 时统一清理（保留 `source` 与 `preserve` 分支）。
8. **看板单二进制内嵌与自举模板先行**：
   - Vue 3 看板通过 `go:embed` 单二进制同端口内嵌（提供未执行前端构建时的优雅占位 fallback，不阻塞 `go test ./...`），Agent HTML 预览强制 `<iframe sandbox="allow-scripts">`（无 `allow-same-origin`）+ 严格 CSP；
   - `examples/` 首期交付 **`examples/copilot-agy-self-host/`** 自举模板与本地 Linux 双插件一键安装脚本。

---

## 2. 阶段三源码目录布局

```text
sofa/
├── cmd/sofa/                                       # [030] 统一 CLI 入口 (init/plan/apply/server/worker/runs/decisions/env/secret)
├── examples/copilot-agy-self-host/                 # [032] Copilot Worker + Gatekeeper + 只读 Antigravity QC 自举模板
├── scripts/install-local-plugins.go                # [032] 一键交叉构建安装本地 Linux 双插件至 .sofa/plugins
├── internal/
│   ├── blob/s3/                                    # [021] S3 / MinIO / OSS 兼容流式 BlobStore
│   ├── daprhost/                                   # [022] 本地 scheduler + daprd 透明加密配置生成与进程托管
│   ├── applyeval/                                  # [027] Apply 期延迟表达式求值、dynamic_worker 展开与 module 校验纯核心
│   ├── actor/
│   │   ├── config/                                 # [023] ConfigActor 三层 Env/Secret 管理与 .sofaplan 预检解析
│   │   ├── registry/                               # [024] ProcessRegistryActor 三态探测与并发槽位调度
│   │   ├── node/                                   # [025] NodeActor 权威状态机、插话/决策与 NodeControlPlane 桥接
│   │   ├── merge/                                  # [026] MergeActor 持久合并队列与源头打回协议
│   │   └── run/                                    # [028] OperatorActor + RunActor 全图推进、返工级联与延迟清理
│   ├── worker/daemon/                              # [024] sofa-worker-<process_id> Dapr 服务宿主与 JSONL 日志归档
│   ├── server/                                     # [029] 双端口隔离、REST + WebSocket API、鉴权与 CSP 预览网关
│   └── webui/                                      # [031] Vue 3 构建产物 go:embed 包装与开发期空置 fallback
└── web/                                            # [031] Vue 3 + TypeScript + Vite 安全看板与人工决策工作台
```

---

## 3. 阶段三微切片索引（`021`–`032`）

| 编号 | 设计文档 | 核心交付物（单篇 ~250–450 行核心代码） | 前置依赖 |
| :--- | :--- | :--- | :--- |
| **021** | [021-s3-blobstore.md](./021-s3-blobstore.md) | `internal/blob/s3`：基于 `aws-sdk-go-v2` 的流式分片上传、SHA-256 核验与批量前缀删除 | Stage 2 (`011`) |
| **022** | [022-local-dapr-host.md](./022-local-dapr-host.md) | `internal/daprhost`：`scheduler` + `daprd` 托管、`state.sqlite` `primaryEncryptionKey` 透明加密组件生成与集成测试基座 | Stage 2 |
| **023** | [023-config-actor-env-secrets.md](./023-config-actor-env-secrets.md) | `internal/actor/config`：`ConfigActor` 三层（`Run > Namespace > Global`）`Env`/`Secret` 管理与第 0 秒 Preflight 解析 | 022 |
| **024** | [024-process-registry-and-worker-daemon.md](./024-process-registry-and-worker-daemon.md) | `internal/actor/registry` + `internal/worker/daemon`：三态存活探测、槽位调度（`10`/`2`）、Worker 服务宿主与残留容器自清理 | 022 |
| **025** | [025-node-actor-and-bridge.md](./025-node-actor-and-bridge.md) | `internal/actor/node`：`NodeActor` 权威状态机、日记流、插话队列、决策卡片、版本绑定批准与 `NodeControlPlane` 远端桥接 | 023, 024 |
| **026** | [026-merge-actor-queue.md](./026-merge-actor-queue.md) | `internal/actor/merge`：目标分支全局串行持久队列、Worker 槽位调度执行 `ExecuteMergeCandidate` 与源头打回结果回报 | 024, 025 |
| **027** | [027-apply-deferred-evaluator.md](./027-apply-deferred-evaluator.md) | `internal/applyeval`：上游公开值注入、`hcl.EvalContext` 延迟求值、`dynamic_worker` 展开与子 `module` 输入校验纯核心 | Stage 1 & 2 |
| **028** | [028-operator-and-run-actor.md](./028-operator-and-run-actor.md) | `internal/actor/run`：`OperatorActor` + `RunActor` 非阻塞全图推进、嵌套 `module`、`fallback` 级联 `epoch`、暂停/恢复/`rerun` 与延迟删除 | 021, 023–027 |
| **029** | [029-rest-ws-api-server.md](./029-rest-ws-api-server.md) | `internal/server`：双端口隔离、Bearer/`Origin` 鉴权、`/api/v1/*` 全量幂等端点、CSP 隔离预览与 `/api/v1/ws` 实时流 | 028 |
| **030** | [030-cli-server-worker-apply.md](./030-cli-server-worker-apply.md) | `internal/cli` + `cmd/sofa`：`sofa server [--dev]`、`sofa worker`、`sofa apply`（双输入 + `Ctrl+C` 即时中断）及全套运维/凭据 CLI | 029 |
| **031** | [031-vue3-dashboard.md](./031-vue3-dashboard.md) | `web/` + `internal/webui`：Vue 3 + TS + Vitest 安全看板、DAG 拓扑/日志/凭据管理、决策审批与无 `allow-same-origin` 沙箱 `iframe` + `go:embed` | 029, 030 |
| **032** | [032-self-host-example-and-plugin-bundle.md](./032-self-host-example-and-plugin-bundle.md) | `examples/copilot-agy-self-host/` + `scripts/install-local-plugins.go`：双栈异构自举模板、本地 Linux 插件一键安装与 `sofa plan` 自动化单测 | 030 |
