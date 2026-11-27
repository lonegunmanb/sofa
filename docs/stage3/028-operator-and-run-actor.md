# 分片 028：`OperatorActor` 与 `RunActor` 非阻塞全图编排、返工级联与延迟清理（`internal/actor/run`）

## 1. 目标与边界

串联 `023`–`027` 全部底座，落地全局索引 `OperatorActor("global")` 与全图调度核心 `RunActor(<run_id>)`：
1. **全局索引与冷启动恢复（`OperatorActor("global")`）**：
   - 维护全量 `RunSummary` 索引（`run_id`、`namespace`、状态、创建/更新时间）；
   - 服务启动时执行 `RecoverActiveRuns`：遍历所有处于 `RUNNING` / `PAUSING` 的 `RunID` 唤醒对应 `RunActor` 执行对账（配合持久化 Reminder 实现断电/崩溃后 100% 自动续跑）。
2. **毫秒级非阻塞全图推进（`RunActor(<run_id>)`）**：
   - 严禁阻塞持有 Actor 锁等待容器运行：每次在收到节点回调或对账 Reminder 时，调用 `027` `applyeval` 找出所有依赖已 `SUCCEEDED` 的就绪节点，遵守 `module.parallelism` 上限异步调用 `NodeActor.StartOrResume` 或 `MergeActor.EnqueueCandidate`，毫秒内保存状态并返回；
   - **`dynamic_worker` 展开键一次固化**：首次求值出 `keys []string` 后立即持久化写入 `RunActor` 状态，后续对账或恢复不再重算（除非其上游祖先发生 `fallback` / `rerun` 导致本 `dynamic_worker` 被级联重置！）；
   - **流式 `merge` 入队**：当 `merge` 依赖 `dynamic_worker` 时，每个子实例一完成即流式调用 `MergeActor.EnqueueCandidate`，无需傻等最慢兄弟跑完！
3. **两类返工与单点重跑的 `epoch` 级联协议**：
   - **显式 `fallback` 祖先返工**：当节点触发 `FallbackTriggered`（`target = ancestorAddr`），校验未超过该 `fallback.max_cycles`，将 `ancestorAddr` 及从它到当前节点受影响子图内的所有节点级联递增 `epoch` 并重置状态，将退回原因与 `FallbackArtifacts` 注入 `ancestorAddr` 重新派发；
   - **`MergeReworkSource` 源头修复**：仅将冲突/失败的那个源头子 `worker` 递增 `epoch`、将其 `BaseSHA` 升级为 `MergeActor` 返回的 `LatestTargetSHA`，附带冲突/测试日志重跑，绝不影响其他已合入兄弟！
   - **人工单节点 `Rerun(nodeAddr)`**：若该节点（或其下游）已有提交合入显式跨 Run 共享 `merge.target_branch`，立即拒绝并返回 `ErrAlreadyMergedSharedBranch`（供 API 转 `409 Conflict`）；否则递增自身及本 Run 所有传递下游节点 `epoch` 从 `based_source` 重跑。
4. **`planned`/`immediate` 暂停与延迟统一清理（`Delete`）**：
   - `Pause("planned")`：不派发新就绪节点，已在途节点继续跑完；`Pause("immediate")`：立即取消队列、递增所有在途节点 `epoch` 并通知 Worker `abort_node` 销毁容器；
   - **延迟清理铁律**：`SUCCEEDED` / `FAILED` / `PAUSED` 时保留全部 `ckpt` 分支、`work` 分支与 `BlobStore` 对象；仅在调用 `DeleteRun` 时：隔离在途执行 → 按 `git.Ledger.PlanCleanup(sources, preserves)` 删除远端中间分支 → 调 `BlobStore.DeletePrefix("runs/<run_id>/")` → 调 `ConfigActor.DeleteRunScope` → 从 `OperatorActor` 移除索引！

**前置依赖**：分片 `021`、`023`、`024`、`025`、`026`、`027`。

---

## 2. 涉及包与文件清单

- `internal/actor/run/types.go`：`RunStatus`、节点表、级联计算与请求/响应结构
- `internal/actor/run/operator_actor.go`：`OperatorActor("global")`
- `internal/actor/run/run_actor.go`：`RunActor` 非阻塞拓扑推进、嵌套 `module` 扁平化子图管理、返工级联与 `Delete` 清理
- `internal/actor/run/run_test.go`：Tier 1 DAG 推进、流式 `dynamic_worker -> merge`、`fallback` `max_cycles`、`MergeReworkSource`、`rerun` `409` 防护与延迟删除单测
- `internal/actor/run/run_integration_test.go`（`//go:build integration`）：Tier 2 真实 `daprhost` + Git + Podman 跑通多节点工作流及模拟重启自动断点续跑

---

## 3. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`run_test.go`）**：
   - 静态 + `dynamic_worker`（3 个子实例，`module.parallelism = 2`）流式合入 `merge` 并求值顶层 `output`；
   - `fallback` 退回祖先节点级联递增受影响链路 `epoch`；超过 `max_cycles` 精确转 `FAILED`；
   - 已合入显式 `target_branch` 的节点执行 `Rerun` 被拒绝返回 `ErrAlreadyMergedSharedBranch`；`SUCCEEDED` 时零分支删除，`DeleteRun` 时保留 `main` 与 `preserve` 分支并删净全部 `sofa/ckpt/*`、`sofa/work/*` 及 `runs/<run_id>/` Blob。
2. **Tier 2 真实端到端测试（`run_integration_test.go`，`//go:build integration`）**：
   - 在真实 `daprhost` 上提交双节点 `.sofaplan`，在节点 2 运行中途关闭并重启宿主，验证 `OperatorActor` + Reminder 自动接管将节点 2 升至 `epoch=2` 跑至全图 `SUCCEEDED`！
