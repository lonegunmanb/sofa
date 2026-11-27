# 分片 025：`NodeActor` 权威状态机、插话/决策卡片与远端 `NodeControlPlane` 桥接（`internal/actor/node`）

## 1. 目标与边界

将阶段二 `017`/`018` 抽象出的单节点控制面接口（`runner.NodeControlPlane`）升级为真实的分布式 Dapr `NodeActor`，承载静态 `worker` 及 `dynamic_worker` 展开子任务的全部持久化生命周期：
1. **Actor 寻址安全规范**：
   - Actor Type 固定为 `"NodeActor"`；为避免 `/` 字符干扰 Dapr HTTP Actor 路由，Actor ID 统一编码为 URL 安全的 `EncodeNodeActorID(runID, canonicalNodeAddr)`（如 `<sanitized_run_id>__<node_key_with_sha256_prefix>`，双向无损保存原始 `RunID` 与 `CanonicalNodeAddr`）。
2. **单调递增 `epoch` 权威栅栏**：
   - 每当发生进程 `DEAD` 故障接管、`immediate` 强制暂停后恢复、上游 `fallback` / `MergeReworkSource` 退回返工或人工单点 `rerun`，`NodeActor` 单调递增 `CurrentEpoch`；
   - 任何携带旧 `epoch` 的检查点写入、QC 账本快照、`work_branch` 登记或最终节点结果回报，一律被 `NodeActor` 硬性拒绝并返回 `checkpoint.ErrStaleEpoch`！
3. **人机协作全闭环状态**：
   - 维护结构化日记流、人工插话 FIFO 队列（支持 `wait_for_confirmation`）、待处理人工决策卡片（`sofa_request_decision`、`sofa_pause_for_human`、`human_approval == "required"`），并严格执行 **`(commit_sha, artifact_ids)` 版本精确绑定校验**；
   - 提供实现了 `runner.NodeControlPlane` 接口的客户端适配器 `RemoteNodeControlPlane`（供 `sofa-worker` 内的 `WorkerRunner` 零改动直接调用！）。

**前置依赖**：分片 `018`、`023`、`024`。

---

## 2. 涉及包与文件清单

- `internal/actor/node/types.go`：节点状态枚举（`PENDING`、`QUEUED_SLOT`、`RUNNING`、`WAITING_HUMAN`、`SUCCEEDED`、`FALLBACK_TRIGGERED`、`FAILED`、`PAUSED`）、决策卡片模型与 Actor 单参数请求/响应结构
- `internal/actor/node/actor.go`：`NodeActor`（基于 `actor.ServerImplBaseCtx` + `ReminderCallee` 槽位重试唤醒）
- `internal/actor/node/control_plane_client.go`：实现 `runner.NodeControlPlane` 的 Dapr Actor Stub 桥接器（将远端 `ErrStaleEpoch` 错误码精准还原为 `checkpoint.ErrStaleEpoch`）
- `internal/actor/node/node_test.go`：Tier 1 全状态流转、`epoch` 栅栏、决策卡片与版本绑定单测
- `internal/actor/node/node_integration_test.go`（`//go:build integration`）：Tier 2 真实 `daprd` 下 `WorkerRunner` 通过 `RemoteNodeControlPlane` 驱动 `NodeActor` 跑通检查点、暂停与跨 `epoch` 恢复

---

## 3. 保姆级实现细则

1. **派发与槽位排队闭环（`Dispatch`）**：
   - `RunActor` 通知 `NodeActor.StartOrResume(req)`（传入已求值的输入、基准 `BaseSHA` / `BasedSource` 等）；
   - `NodeActor` 先调 `023` `ConfigActor.ResolveForNode` 获取该节点最小凭据集，再调 `024` `ProcessRegistryActor.ClaimSlot`：
     - 若暂无空闲槽位：置状态为 `QUEUED_SLOT`，注册短周期 Reminder（如 `1s`）重试抢槽，立即返回不阻塞 `RunActor`！
     - 抢到槽位后：持久化记录当前 `(Epoch, LeaseID, WorkerAppID)`，置为 `RUNNING`，调目标 Worker 的 `start_node`；
   - 若收到 `ProcessRegistryActor` 通知当前 `WorkerAppID` 已 `DEAD`（且节点仍处于 `RUNNING`）：自动 `Epoch++`，回到抢槽派发流程从最近完整检查点换机续跑！
2. **分支登记与 Run 账本联动**：
   - `RegisterCheckpointBranch` 与 `RegisterWorkBranch` 先在 `NodeActor` 校验 `epoch == CurrentEpoch`，随后同步调用所属 `RunActor.RegisterManagedBranch` 写入 Run 级总账本，确保先记账后推远端分支！
3. **提交人工决策不误解冻手动暂停**：
   - `SubmitDecision(ctx, req)` 先将人类回答/批准持久化写入 `NodeActor` 状态（若是选项回答或追问，自动拼成高优日记插话供下一轮 `RecoverLatestAligned` / 会话读取；若是 `human_approval` 批准或退回上游，写入 `ApprovalRecord`），然后通知 `RunActor`；若当前 Run 处于手动 `PAUSED` 态，仅更新卡片为已回答，不擅自拉起容器，等 `RunActor.Resume` 时再调度！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`node_test.go`）**：
   - 旧 `epoch=1` 的僵尸 Worker 在节点已升至 `epoch=2` 后调用 `BeginCheckpoint` / `RegisterWorkBranch` / `ReportOutcome`，100% 返回 `ErrStaleEpoch` 且不污染 `epoch=2` 状态；
   - `ClaimSlot` 满载时进入 `QUEUED_SLOT` 并注册 Reminder，槽位释放后自动成功派发；
   - `human_approval == "required"`：提交批准后若因插话导致新跑出 `CommitSHA'`，旧批准自动失效重新进入 `WAITING_HUMAN`。
2. **Tier 2 真实 Dapr 联调（`node_integration_test.go`，`//go:build integration`）**：
   - 通过真实 `daprd` + `RemoteNodeControlPlane` 跑通 `WorkerRunner` -> `WAITING_HUMAN`（释放容器）-> 调用 `NodeActor.SubmitDecision` -> `epoch+1` 断点恢复 -> `SUCCEEDED` 全链路。
