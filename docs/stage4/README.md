# 阶段四（Stage 4）开发总纲：单机 `runtime = "host"` 原生宿主执行模式与运行时提示词自动注入

> **面向开发与 QC Agent 的强制阅读说明**：
> 阶段四包含 **4 个按序递进的单职责微切片（`038`–`041`）**，所有设计分支已通过 `/grill-me` 冻结。在为任一切片执行 `/speckit-specify`、`/speckit-plan`、`/speckit-tasks` 或编写代码之前，**必须完整阅读本总纲及对应编号的分片蓝图**，严格按文档给定的包路径、接口签名、安全不变量与 Red → Green TDD 用例落地，**严禁自行发明替代架构或合并多个分片**。

---

## 1. `/grill-me` 冻结的 6 条核心设计契约

1. **显式 `sandbox { runtime = "host" }` 语法与互斥校验**：
   - `sandbox` 块与 `runtime` 字段保持显式声明（Root Module `0` 个 `sandbox` 依然报错，杜绝漏写配置时在远端机器上静默裸跑宿主机）；
   - `runtime = "host"` 下**禁止**配置 `image`、`mount`、`resources`；`network` 省略时默认 `"host"`，显式配置时仅允许 `"host"`（配置 `"bridge"` / `"none"` 在 Plan 期立即报错）；容器运行时（`podman` / `docker` / `nerdctl` / `kirk`）下 `image` 仍为必填、默认 `network = "bridge"`。
2. **严格限定单机开发模式（`sofa server --dev` 强门禁）**：
   - `runtime = "host"` **仅允许在 `sofa server --dev` 单机全栈模式下运行**；
   - 分布式 `sofa server`（非 `--dev`）在 `POST /api/v1/runs`（`sofa apply`）第 0 秒 Preflight 扫描 `.sofaplan`，一旦发现任何节点或 `sandbox` 使用 `runtime == "host"` 立即返回 `400/422` 拒绝；独立 `sofa worker`（`!DevMode`）在 `start_node` / `run_merge_candidate` 处做纵深防御二次拦截；
   - 为 `sofa server` 补齐 `--temp-dir` flag（`--dev` 下默认 `<state-dir>/worker-tmp`），与 `sofa worker --temp-dir` 保持一致。
3. **隔离工作区 + 内嵌 `.sofa/{artifacts,imports}` 与 `.git/info/exclude`**：
   - `source.url` 原生支持本地 Git 仓库路径或远端 URL；每个节点仍在 `<temp-dir>` 下按 `run/node` 检出独立工作区并推回 `sofa/ckpt/*` 与 `sofa/work/*` 分支；
   - 绑定 `runtime == "host"` 的节点将产物与导入目录置于 `<workspace>/.sofa/artifacts` 与 `<workspace>/.sofa/imports`，自动把 `/.sofa/` 追加写入 `<workspace>/.git/info/exclude`（不修改代码仓 `.gitignore`、不污染 `git status`）；
   - 在 `runtime == "host"` 下，`artifact("<name>").path` 与 `import_artifact("<name>").path` 返回工作区相对路径 `".sofa/artifacts/..."` 与 `".sofa/imports/..."`（容器模式保持 `"/.sofa/artifacts/..."` 与 `"/.sofa/imports/..."` 不变）。
4. **宿主机工具链环境变量继承策略**：
   - `runtime = "host"` 下启动 Harness 插件、`gatekeeper`、`go_tool`、`external_tool` 时，直接继承宿主机当前进程环境变量（仅剔除 `APP_API_TOKEN`、`SOFA_API_TOKEN`、`SOFA_HARNESS_PLUGIN`、`PLUGIN_*`、`GIT_DIR`、`GIT_WORK_TREE`、`GIT_INDEX_FILE` 等控制面/Git 内部句柄变量），并在其上叠加 `ConfigActor` 解析的 `env_refs` / `api_key_ref`。
5. **QC 阶段 `git clone --depth=1 --no-local` 浅克隆物理隔离**：
   - 在 `runtime = "host"` 下无容器 `:ro` 挂载，每次进入 QC 阶段（含 `qc.gatekeeper` 与只读 LLM `qc`）前，使用 `git clone --depth=1 --no-local` 从当前节点 `workspace` 快速浅克隆出 `<execRoot>/qc-workspace-r<n>`，同步当前未提交的跟踪/未跟踪工作树改动及 `.sofa/{artifacts,imports}`（同样写入 `.git/info/exclude`），并在 QC 会话前后做工作树与产物哈希快照校验，既最小化克隆耗时与磁盘占用，又物理隔离保护 Worker 主工作区。
6. **宿主自动注入运行时与协议系统提示词（`041`）**：
   - 工作目录位置、`.git` 禁止手动提交/推送、`writable_paths` 可写白名单、已声明 `artifact` / `import_artifact` 路径清单以及 `sofa_checkpoint` / `sofa_complete_work` / 自定义终止工具 / QC 裁决工具调用契约属于 `sofa` 宿主自身协议不变量，由 `WorkerRunner` 自动追加至 Work 与 QC 会话的 `SystemPrompt`，无需业务 `.sofa.hcl` 重复手写。

---

## 2. 阶段四微切片索引（`038`–`041`）

| 编号 | 设计文档 | 核心交付物 | 前置依赖 |
| :--- | :--- | :--- | :--- |
| **038** | [038-host-sandbox-schema-and-dev-guard.md](./038-host-sandbox-schema-and-dev-guard.md) | `sandbox` `runtime = "host"` HCL 校验、`host` 下 `artifact().path` / `import_artifact().path` 相对路径解析、`sofa server --temp-dir` 及分布式拒绝 `host` 双层门禁 | Stage 3 + `037` |
| **039** | [039-host-plugin-installer-and-engine.md](./039-host-plugin-installer-and-engine.md) | `installer` 跨平台（`windows`/`darwin`/`linux`）入口解析、`NewHostSubprocessOpener` 宿主环境启动器、`HostEngine`（`gatekeeper`/`go_tool`/`external_tool`）与本地宿主插件安装脚本 `-host` | `038` |
| **040** | [040-host-worker-qc-shallow-clone-and-e2e.md](./040-host-worker-qc-shallow-clone-and-e2e.md) | `WorkerRunner` 宿主机执行闭环（`.git/info/exclude`、宿主 Skills 过滤挂载、宿主 `QuotePaths`）、QC `depth=1` 浅克隆隔离工作区、无 Podman 启动 `--dev` 与端到端集成测试 | `038`, `039` |
| **041** | [041-auto-runtime-system-prompt.md](./041-auto-runtime-system-prompt.md) | `WorkerRunner` 自动构建并注入容器/`host` 双模式运行时约束与协议工具说明至 Work/QC `SystemPrompt`，清理 `examples/copilot-agy-self-host/main.sofa.hcl` 临时手写提示词 | `040` |
