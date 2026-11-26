# 阶段三（Stage 3）概要规划：分布式对象存储、Dapr Actor 控制面、统一 API 与安全看板

> **当前状态**：**概要规划（Outline）**——待阶段二（[docs/stage2/README.md](../stage2/README.md)，`010`–`020`）全部收口后，再按同样颗粒度拆写逐片保姆级实施蓝图。
> 架构基线遵循 [architecture-contract.md](../architecture-contract.md) 与 [constitution.md](../../.specify/memory/constitution.md)。

---

## 1. 阶段三目标与 `/grill-me` 对齐交付边界

在阶段一（离线 `.sofaplan` 编译，`001`–`009`）与阶段二（宿主机单节点容器/双语言 Harness/Git/二阶段检查点执行底座，`010`–`020`）全部就绪的基础上，阶段三完成**分布式共享存储、Dapr Actor 编排面、对外统一管理协议与人机协作看板**：

1. **分布式 `BlobStore` 扩展（S3 / MinIO / OSS 兼容实现）**：
   - 在阶段二 `internal/blob.Store` 流式接口之上落地基于 `aws-sdk-go-v2/service/s3` 的 `S3Store`（支持自定义 Endpoint、Path-Style、分片上传、SHA-256 完整性校验与 `DeletePrefix` 批量清理），使多机 `sofa-worker` 无缝共享 `.sofaplan`、跨机 `Artifact` 与检查点快照。
2. **纯正 `dapr/go-sdk/actor` 分布式控制面（单机/分布式 100% 同构）**：
   - 落地五大 Actor（`OperatorActor`、`RunActor`、`NodeActor`、`MergeActor`、`ProcessRegistryActor`）及宿主机 `sofa-worker-<process_id>` Dapr Service Invocation 宿主；将阶段二的 `NodeStateStore` 与分支账本直接对接到真实 Actor；
   - 单机模式自动生成指向本地状态目录的 Dapr 组件配置并启停托管本地 `daprd`；分布式模式直接连接共享 Dapr State Store / PubSub / Placement（不强制依赖 Kubernetes）。
3. **非阻塞全图 DAG 推进、动态展开、返工循环与串行合并**：
   - `RunActor` 通过毫秒级事件短调用 + Reminder 对账推进拓扑：注入已完成上游值求值延迟表达式、首次持久化固化 `dynamic_worker` 展开键并派生子 `NodeActor`、执行子模块延迟 `variable` `validation`、驱动嵌套 `module` 子图、处理显式 `fallback` 祖先返工（级联递增受影响节点 `epoch`）以及 `planned` / `immediate` 暂停；
   - `MergeActor` 按 `sha256(repo_url + target_branch)` 维护持久化合并队列，调用阶段二 `018` 的 `ExecuteMergeCandidate` 串行集成，冲突或 `gatekeeper` 失败时按**“严格源头负责制”**将原 `worker` 下一轮基线升级为最新集成分支 `M_k` 并附带失败报告退回重跑。
4. **统一 REST JSON + WebSocket 管理 API、全生命周期 CLI 与 Vue 3 安全看板**：
   - 暴露 `/api/v1/runs`、`/nodes`、`/decisions`、`/artifacts` 与基于 Dapr PubSub 的 `/api/v1/ws` 实时流；
   - 扩展 `cmd/sofa` 交付 `sofa apply <.sofaplan>`、`sofa runs`、`sofa pause/resume/rerun/delete`、`sofa decisions`；
   - 构建 `web/`（Vue 3 + TypeScript + Vite，Vitest TDD）：提供 DAG 状态视图、节点轮次/日记/日志/检查点追踪、人工插话、决策卡片审批，以及强制不带 `allow-same-origin` 的 `<iframe sandbox="allow-scripts">` + CSP 隔离材料预览。
5. **开箱即用 `examples/` 模式库与 `copilot + antigravity` 自举模板**：
   - 新增顶层 `examples/` 目录收录核心工程编排模式（含 **`copilot` 作为编码 Worker + 确定性 `gatekeeper` + 独立只读容器 `antigravity` 作为异构 QC 审查员** 的自举切片开发模板、上游设计 `fallback` 返工模板、`dynamic_worker` 扇出 + `merge` 串行合入模板），并通过自动化单测对全部示例执行 `sofa plan` 编译校验防止示例腐烂。

---

## 2. 阶段三拟议微切片概要（`021`–`028`）

| 切片编号 | 切片主题 | 核心职责概要 | 前置依赖 |
| :--- | :--- | :--- | :--- |
| **021** | **S3 / MinIO 兼容对象存储实现（`internal/blob/s3`）** | 基于 `aws-sdk-go-v2` 实现 `blob.Store`（流式分片上传、SHA-256 核验、重试与前缀清理），配合本地 MinIO / S3 兼容容器完成 Tier 2 集成测试 | Stage 2 (`011`) |
| **022** | **`ProcessRegistryActor` 三态存活探测与 `sofa-worker` 守护进程** | 工作进程启动生成 `process_id` 注册 Dapr App ID、定期心跳、`HEALTHY → SUSPECT → DEAD` Reminder 主动探测、失联换机隔离与残留容器自清理 | Stage 2 (`018`–`020`) |
| **023** | **`NodeActor` 权威状态机、日记插话队列与 `human_approval` 版本绑定** | 实现生产级 `NodeStateStore`：单调递增 `epoch` 栅栏、结构化日记流、人工插话（含 `wait_for_confirmation`）入队消费、决策卡片状态流转与 `(commit_sha, artifact_ids)` 精确绑定校验 | 022 |
| **024** | **`MergeActor` 持久队列、串行集成与源头打回协议** | 按目标分支全局串行持久入队（忙时不丢弃不阻塞）、调度宿主机执行 `ExecuteMergeCandidate`、冲突/失败升级基线打回源头 `worker`、合入成功后禁止回滚 | 022, 023 |
| **025** | **`RunActor`、`OperatorActor` 与单机 `daprd` 同构引导** | 全量 Run 索引与冷启动恢复；毫秒级非阻塞拓扑推进、延迟表达式求值、`dynamic_worker` 展开固化、嵌套 `module`、`fallback` 级联 `epoch` 重跑、`planned`/`immediate` 暂停、Run 结束/删除时按账本清理中间 Git 分支与 `BlobStore` 临时对象；单机 `daprd` 启停托管 | 021, 023, 024 |
| **026** | **统一 REST + WebSocket API 服务与 `sofa apply` / 运维 CLI** | `/api/v1/*` 幂等 REST 端点、Dapr PubSub 驱动的 `/api/v1/ws` 实时流，以及 `sofa apply`、`sofa runs`、`sofa pause/resume/rerun/delete`、`sofa decisions` CLI 命令 | 025 |
| **027** | **Vue 3 安全看板与人工决策工作台（`web/`）** | Vite + Vue 3 + TypeScript 前端工程（Vitest + Vue Test Utils TDD），实现 DAG 可视化、节点日记/检查点/日志追踪、人工插话、决策卡片审批与无 `allow-same-origin` 沙箱 `iframe` 材料预览 | 026 |
| **028** | **`examples/` 经典编排模式库与 `copilot + antigravity` 自举模板** | 提供自举开发模板（`copilot` 编码 Worker + `gofmt`/`go vet`/`go test` `gatekeeper` + 独立只读容器 `antigravity` QC 互审）、`fallback` 返工模板、`dynamic_worker` + `merge` 模板，并附带 `sofa plan` 自动化校验测试 | 026 |

---

## 3. 阶段三源码目录扩展预览

```text
sofa/
├── cmd/
│   ├── sofa/                               # [026] 扩展 apply / runs / pause / resume / rerun / delete / decisions
│   └── sofa-worker/                        # [022] 宿主机工作进程守护入口（Dapr App + Actor Host）
├── examples/                               # [028] 经典模式库（含 copilot Worker + antigravity 只读 QC 自举模板）
├── internal/
│   ├── blob/s3/                            # [021] S3 / MinIO / OSS 兼容 BlobStore 实现
│   ├── actor/                              # [022-025] OperatorActor / RunActor / NodeActor / MergeActor / ProcessRegistryActor
│   ├── daprhost/                           # [025] 单机模式本地 daprd 组件配置生成与生命周期托管
│   └── server/                             # [026] REST JSON + WebSocket 统一管理 API 与 CSP 预览头
└── web/                                    # [027] Vue 3 + TypeScript + Vite 看板与沙箱化决策工作台
```
