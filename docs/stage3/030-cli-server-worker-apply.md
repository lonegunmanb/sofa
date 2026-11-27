# 分片 030：全生命周期 CLI 扩展（`sofa server`、`sofa worker`、`sofa apply` 与凭据/运维命令）

## 1. 目标与边界

扩展单一 `sofa` 二进制（`cmd/sofa` / `internal/cli`），交付单机全栈自举与多机分布式部署的完整命令行入口——**本分片一完成，无需等待前端看板即可在终端跑通全套真机工作流**：
1. **三大守护角色命令**：
   - `sofa server --dev`：一键启动本地 `daprhost`（`scheduler` + `daprd` + 加密 `state.sqlite`）+ `blob.LocalStore`（`<state-dir>/blobs`）+ 五大控制面 Actor + 内置本地 `WorkerDaemon`（`--max-concurrency` 默认 **`2`**）+ REST/WebSocket 服务（默认 `127.0.0.1:8080`，`--state-dir` 默认 `~/.sofa/dev-state`），启动完成立即调用 `OperatorActor.RecoverActiveRuns` 实现重启自动断点续跑；
   - `sofa server`（无 `--dev`）：纯控制面服务（连接外部 Dapr Sidecar + 本地或 `021` S3 `BlobStore`，强制校验 `SOFA_API_TOKEN`，不启动本地 `WorkerDaemon`）；
   - `sofa worker`：独立算力守护进程（`--max-concurrency` 默认 **`10`**，支持 `--plugin-dir`、连接外部 Dapr 与共享 `BlobStore`）。
2. **`sofa apply` 双输入与 `Ctrl+C` 即时中断**：
   - `sofa apply [path]`：若 `path` 为 `.sofaplan` 文件则直接上传；若省略或为配置目录，则在客户端本地复用 `planner`（支持 `-var`、`-var-file`、`SOFA_VAR_*`）即时编译生成临时 `.sofaplan` 后调用 `POST /api/v1/runs`（支持 `--namespace`、`--env K=V`、`--secret-env K`）；
   - `-d / --detach`：打印 `run_id` 后立即退出；
   - 默认前台跟踪模式：连接 `/api/v1/ws?run_id=<id>` 实时打印节点状态流转与增量日志；若 Run 进入 `WAITING_HUMAN` 打印待办 `decision_id` 与响应命令提示；**捕获首次 `SIGINT`（`Ctrl+C`）时立即向服务端调用 `POST /api/v1/runs/{id}/pause?mode=immediate` 中断在途容器再以非 0 退出码返回**！
3. **三层受管凭据与全套运维子命令**：
   - `sofa env set/list/delete`、`sofa secret set/list/delete`（支持 `--namespace <ns>`，以及 **`sofa secret set --from-env KEY1 [KEY2...]`** 从当前终端环境变量快捷导入受管库，绝不在命令行历史暴露密钥明文！）；
   - `sofa runs [get <run_id>]`、`sofa logs <run_id> <node_id>`、`sofa pause [--mode planned|immediate] <run_id>`、`sofa resume <run_id>`、`sofa rerun <run_id> <node_id>`、`sofa delete <run_id>`、`sofa interject <run_id> <node_id> -m "..." [--wait]`、`sofa decisions [list | respond <decision_id> ...]`.

**前置依赖**：分片 `021`、`022`、`024`、`029`。

---

## 2. 涉及包与文件清单

- `internal/cli/client.go`：带 `SOFA_SERVER_ADDR`（默认 `http://127.0.0.1:8080`）与 `SOFA_API_TOKEN` 的 REST + WebSocket SDK 客户端
- `internal/cli/server_cmd.go` & `worker_cmd.go`：`sofa server [--dev]` 与 `sofa worker` 组装与信号优雅关停
- `internal/cli/apply_cmd.go` & `ops_cmd.go` & `config_cmd.go`：`apply`、运维生命周期与 `env`/`secret` 命令实现
- `internal/cli/cli_apply_test.go`：Tier 1 双输入编译上传、`Ctrl+C` 触发 `mode=immediate` 暂停、`--from-env` 导入与 `runs/decisions` 单测
- `internal/cli/cli_integration_test.go`（`//go:build integration`）：Tier 2 启动真实 `sofa server --dev` 子进程，用 CLI 执行 `sofa secret set --from-env` → `sofa apply` → `sofa runs` → `sofa delete` 全链路闭环测试

---

## 3. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`cli_apply_test.go`）**：
   - `sofa apply ./dir` 自动在本地编译 `.sofaplan` 上传，缺失受管 Secret 时清晰格式化服务端 `422 missing_keys` 提示用户运行 `sofa secret set`；
   - 前台 `sofa apply` 流式接收期间注入 `ctx` 取消（模拟 `Ctrl+C`），验证客户端必定先发出 `POST /api/v1/runs/{id}/pause`（`mode="immediate"`）；
   - `sofa secret set --from-env GITHUB_TOKEN GEMINI_API_KEY` 准确读取客户端进程环境写入服务端，且 `sofa secret list` 不输出任何明文。
2. **Tier 2 真机 `sofa server --dev` 端到端测试（`cli_integration_test.go`，`//go:build integration`）**：
   - 真机拉起 `sofa server --dev --state-dir <tmp>`，通过 CLI 注入凭据并 `sofa apply` 跑通容器化工作流直至 `SUCCEEDED`，再执行 `sofa delete <run_id>` 验证远端分支与本地 Blob 清空！
