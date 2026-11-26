# 阶段二（Stage 2）开发总纲：宿主机可信执行底座、容器沙箱与多语言 Harness 插件

> **面向后续开发 Agent 的强制阅读说明**：
> 阶段二包含 **按序递进的 11 个单职责微切片（`010`–`020`）**，所有关键设计分支均已通过 `/grill-me` 冻结。在为任一切片执行 `/speckit-specify`、`/speckit-plan`、`/speckit-tasks` 或 `/speckit-implement` 之前，**必须完整阅读本总纲及对应编号的分片蓝图**，严格按文档给定的包路径、结构体/接口签名、挂载契约与必测 Red → Green 用例落地，**严禁自行发明替代架构或合并多个分片**。

---

## 1. 阶段二边界与 `/grill-me` 冻结核心决策

### 1.1 做什么 vs 不做什么
- **阶段二交付范围**：在不引入 Dapr Actor 与 Web 看板的前提下，完整跑通宿主机 `sofa-worker` 在真实 Git + Podman 容器 + 多语言 `AgentHarness` 外部插件上的**单节点执行与跨机可恢复闭环**（含确定性 `merge` 候选 `rebase + gatekeeper` 原语）。
- **留给阶段三的范围**：`dapr/go-sdk/actor` 五大 Actor、本地 `daprd` 托管、全图 DAG 调度、`dynamic_worker` 动态派生、跨节点 `fallback` 调度、S3/MinIO `BlobStore` 实现、REST + WebSocket API、`sofa apply` 运维 CLI 与 Vue 3 看板（阶段二通过显式 Go 接口隔离控制面依赖）。

### 1.2 已冻结的 8 条硬性工程契约
1. **禁用 `sandbox.entrypoint`，容器入口由 `sofa` 100% 接管**：
   - 从 HCL `sandbox` 块移除 `entrypoint` 属性（防止用户脚本向 `stdout` 打印内容破坏 `hashicorp/go-plugin` 首行握手）。
   - Agent 会话容器统一使用 `--init --rm --entrypoint /.sofa/harness/sofa-harness-<name>`；`gatekeeper` 容器使用 `--init --rm --entrypoint <command[0]>`。
2. **插件包强制自带完整 Linux 运行时，混合传输兼容 Windows/macOS/Linux**：
   - 宿主机按容器引擎目标架构（`podman info` 返回的 `linux/amd64` 或 `linux/arm64`）准备自包含插件目录，只读挂载至 `/.sofa/harness/`（`copilot` 插件内置 Linux `copilot` CLI；`antigravity` 插件内置可重定位 Linux Python + `grpcio` + `google-antigravity` wheel；未来 TS 插件可用 `bun build --compile` 或内置单文件 `node`）。
   - 沿用 **`hashicorp/go-plugin`**（对齐官方 `examples/grpc/plugin-python`：仅需实现 `harness.proto` + 标准 `grpc.health.v1.Health` 服务名 `"plugin"` + `AutoMTLS: true`，无需实现 `GRPCStdio`/`GRPCBroker`/`GRPCController`）+ **`buf.gen.yaml`** 声明式生成并提交桩代码入库。
   - **宿主↔容器连接**：Linux 宿主机走共享 `0700` 目录 UDS（`/.sofa/plugin/harness.sock`，兼容 `network = "none"`）；Windows / macOS（Podman Machine 虚拟机）走 `-p 127.0.0.1::<port>` 随机本地端口映射 + `AutoMTLS`（Windows/macOS 下跑 Agent 会话若配 `network = "none"` 则明确报错拦截）。
3. **非 Git 产物（`artifact` / `import_artifact`）统一挂载在 `/workspace` 之外，跨机器经 `BlobStore` 中转**：
   - 产出目录挂载在 **`/.sofa/artifacts/`**（Work 容器 `:rw`，QC 容器 `:ro`），`${artifact("<name>").path}` 直接返回 `"/.sofa/artifacts/<rel_path>"`。
   - 导入目录挂载在 **`/.sofa/imports/`**（Work 与 QC 容器均为 `:ro`），`${import_artifact.<name>.path}` 直接返回 `"/.sofa/imports/<rel_path_or_name>"`。
   - 跨机器交接仅传递不可变 `artifact_id + blob_key + sha256`，由 `BlobStore`（阶段二实现流式接口 + 本地 `LocalStore`）上传、按 `sha256` 强校验下载解压，绝不依赖生产机本地路径。
4. **Git 适配层不做宿主机共享 bare 缓存，容器内 `/workspace/.git` 强制只读**：
   - 基于宿主机 `git` CLI，每次节点直接从远端按目标分支 `--single-branch` 全新拉取构造独立 `/workspace`；`token_ref` / `ssh_key_ref` 仅在宿主机单次命令内存中临时注入，**绝不写入 `/workspace/.git/config`**。
   - 容器内 `/workspace/.git` 始终以 **`:ro` 只读覆盖挂载**（允许 `git diff/log/status`，严禁容器内篡改历史或分支，所有提交与推送均由宿主机 `sofa-worker` 执行）。
5. **`writable_paths` 挂载隔离 + 宿主机 Diff 自修门禁 + 兜底硬回滚**：
   - 配了 `writable_paths` 时，Work 容器 `/workspace` 根挂载 `:ro`，仅将涉及的顶层目录/已存在根文件子挂载为 `:rw`（仍保持 `.git:ro`）。
   - 在 `sofa_checkpoint`、完成工具及进 `gatekeeper` 前，宿主机用 `git status/diff` 精确校验通配符：若存在越界修改/新增文件，**拒绝提交并反馈越界清单让 Agent 自行 `mv` 或清理**（计入 `max_protocol_attempts`）；若重试耗尽仍未清干净，宿主机强制回滚越界路径并判节点失败。
6. **`go_tool` 宿主机交叉编译 + `podman exec -i` 容器内统一执行**：
   - 宿主机将 `go_tool` 编译为 `CGO_ENABLED=0 GOOS=linux GOARCH=<arch>` 静态二进制（按源码哈希缓存），只读挂载至 `/.sofa/tools/<tool_id>`；调用时经宿主机门禁校验后，与 `external_tool` 共用 `podman exec -i -w /workspace <cid>` 在活跃容器内隔离运行。
7. **Work 显式交卷工具 + 宿主机跨轮次 QC 问题账本**：
   - 未配自定义 `terminate` 工具的 Work 会话自动挂载 `sofa_complete_work`；QC 会话挂载 `sofa_qc_open_issues` / `sofa_qc_resolve_issues` / `sofa_qc_submit_verdict`，宿主机分配稳定 `QC-001` ID，返工后新一轮 QC 未将宿主机账本清零前硬性拒绝 `pass`。
8. **两层测试铁律**：
   - **Tier 1（`go test ./...`）**：纯内存/本地临时文件快速单测，零外部守护进程、零网络；
   - **Tier 2（`go test -tags=integration ./...`）**：针对真实本地 Git 仓库、真实 Podman 容器与 `AutoMTLS` 插件进程的端到端测试（云端真实大模型 API 冒烟测试额外由 `SOFA_LIVE_HARNESS_TEST=1` 开关门控）。

---

## 2. 容器内标准挂载总表

| 容器内路径 | Work 容器权限 | Gatekeeper 容器权限 | QC 容器权限 | 用途说明 |
| :--- | :---: | :---: | :---: | :--- |
| `/workspace` | 未设 `writable_paths` 为 `rw`；设了则根 `ro` + 白名单目录 `rw` | `rw`（跑完核验退出码与 `require_clean_worktree`） | `ro`（100% 物理冻结） | 纯净 Git 代码工作树 |
| `/workspace/.git` | `ro` | `ro` | `ro` | 允许 `git status/diff/log`，禁止容器篡改分支与提交 |
| `/.sofa/harness/` | `ro` | 不挂载 | `ro` | 自包含外部 `AgentHarness` 插件目录（入口 `sofa-harness-<name>`） |
| `/.sofa/plugin/` | Linux `rw` (`0700`) / Win 不挂 | 不挂载 | Linux `rw` (`0700`) / Win 不挂 | Linux 下存放 UDS `harness.sock` |
| `/.sofa/tools/` | `ro` | 不挂载 | `ro` | 宿主机交叉编译好的静态 `go_tool` 二进制 |
| `/.sofa/bundle/` | `ro` | `ro` | `ro` | `.sofaplan` 解压的静态资源快照（如 `config/skills`） |
| `/.sofa/imports/` | `ro` | `ro` | `ro` | 从 `BlobStore` 校验还原的上游 `import_artifact` 内容 |
| `/.sofa/artifacts/` | `rw` | `ro` | `ro` | 当前 `worker` 声明的非 Git `artifact` 产出目录 |
| `/.sofa/session-store/` | `rw` | 不挂载 | `rw` | 底层 SDK 会话事件存储（支持跨返工轮次 `ResumeSession`） |

---

## 3. 阶段二统一源码目录布局

```text
sofa/
├── buf.yaml & buf.gen.yaml                         # [014] Go + Python (+ 未来 TS) 声明式桩生成配置
├── proto/
│   └── sofa/harness/v1/harness.proto               # [014] 唯一跨语言协议真源
├── pkg/
│   └── harnessplugin/                              # [014] 公开 Go 插件 SDK 与 v1/*.pb.go（零 internal/ 依赖）
├── plugins/                                        # [019, 020] 官方外部插件同仓孵化区（主仓严禁 import！）
│   ├── sofa-harness-copilot/                       # [019] 独立 go.mod + copilot-sdk/go
│   └── sofa-harness-antigravity/                   # [020] 独立 pyproject.toml + grpcio + google-antigravity
└── internal/
    ├── blob/                                       # [011] 流式 BlobStore 接口与本地原子 LocalStore
    ├── artifact/                                   # [011] Artifact 校验、确定性归档上传与 import_artifact 还原
    ├── git/                                        # [012] 分支账本（纯核心）+ 宿主机 git CLI 零凭据落盘适配层
    ├── harness/
    │   ├── installer/                              # [013] 本地插件发现与远端锁定包下载/SHA-256 核验缓存
    │   └── runtime/                                # [014] Opener/Session 领域接口与 go-plugin 容器宿主桥接
    ├── sandbox/
    │   └── runtime/                                # [013] Podman 容器参数构造、writable_paths 挂载、exec 与 gatekeeper
    ├── tool/
    │   └── runtime/                                # [015] go_tool Linux 交叉编译缓存与容器内统一工具执行器
    ├── session/                                    # [016] sofa_* 协议工具、QC 账本、四道门禁、配额回滚、Retry 与 Watchdog
    ├── checkpoint/                                 # [017] Epoch 二阶段检查点保存与跨机对齐恢复算法
    └── worker/
        └── runner/                                 # [018] Work + 冻结式 QC 单节点编排器与确定性 rebase+gatekeeper 原语
```

---

## 4. 阶段二微切片索引（`010`–`020`）

| 编号 | 设计文档 | 核心交付物（单篇 ~250–500 行核心代码） | 前置依赖 |
| :--- | :--- | :--- | :--- |
| **010** | [010-schema-mount-alignment.md](./010-schema-mount-alignment.md) | 移除 `sandbox.entrypoint`、支持 `qc` 独立覆盖 `sandbox`/`profile`、对齐 `/.sofa/{artifacts,imports}/...` 路径 | Stage 1 (`009`) |
| **011** | [011-blobstore-and-artifacts.md](./011-blobstore-and-artifacts.md) | `internal/blob` 流式 `Store` + `LocalStore`，`internal/artifact` 产出校验、上传缓存、检查点非 Git 快照与 `import_artifact` 解压 | 010 |
| **012** | [012-git-ledger-and-adapter.md](./012-git-ledger-and-adapter.md) | `internal/git` 受管分支登记/清理纯领域账本 + 宿主机 `git` CLI 适配层（`--single-branch` 独立工作区、内存凭据、`writable_paths` Diff 与回滚、`rebase` 检测） | 010 |
| **013** | [013-plugin-cache-and-sandbox.md](./013-plugin-cache-and-sandbox.md) | `internal/harness/installer` 插件发现与锁校验缓存 + `internal/sandbox/runtime` Podman 挂载隔离、`podman exec -i` 与 `gatekeeper` 容器执行器 | 010, 011, 012 |
| **014** | [014-harness-proto-and-goplugin-bridge.md](./014-harness-proto-and-goplugin-bridge.md) | `harness.proto`、`buf` 生成、`pkg/harnessplugin`、`internal/harness/runtime` 容器化 `go-plugin` `AutoMTLS` 桥接（Linux UDS / Win TCP）与最小回环测试插件 | 013 |
| **015** | [015-typed-tool-executors.md](./015-typed-tool-executors.md) | `internal/tool/runtime`：宿主机交叉编译静态 Linux `go_tool`、容器内 `podman exec -i` 执行 `go_tool` / `external_tool`、`QuotePaths` / `postcondition` 与 MCP 资源读取 | 013, 014 |
| **016** | [016-session-gates-and-watchdog.md](./016-session-gates-and-watchdog.md) | `internal/session`：`sofa_*` 协议工具、宿主机跨轮次 `QC-001` 问题账本、四道门禁、配额回滚、`RetryPolicy` 与 `StallWatchdog` | 014, 015 |
| **017** | [017-two-phase-checkpoint-and-recovery.md](./017-two-phase-checkpoint-and-recovery.md) | `internal/checkpoint`：`NodeStateStore` 抽象、Epoch 分支二阶段检查点保存（Git SHA + 日记 + `BlobStore` 非 Git 产物）与从新到旧对齐恢复算法 | 011, 012, 016 |
| **018** | [018-worker-qc-and-merge-runner.md](./018-worker-qc-and-merge-runner.md) | `internal/worker/runner`：Work → 物理冻结 → `gatekeeper` → 只读 `QC` → `human_approval` 单节点状态机，以及确定性 `ExecuteMergeCandidate` 原语 | 013, 016, 017 |
| **019** | [019-plugin-copilot.md](./019-plugin-copilot.md) | `plugins/sofa-harness-copilot`：独立 Go 模块实现 `copilot-sdk/go` 桥接、自包含 Linux 打包与真实 Podman 联调 | 014, 018 |
| **020** | [020-plugin-antigravity.md](./020-plugin-antigravity.md) | `plugins/sofa-harness-antigravity`：独立 Python 工程实现官方 `go-plugin` `Health` + `AutoMTLS` + `google-antigravity` 桥接、自包含打包与 Podman 联调 | 014, 018 |
