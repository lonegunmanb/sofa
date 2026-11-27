# 分片 026：`MergeActor` 目标分支持久队列与源头负责制串行集成（`internal/actor/merge`）

## 1. 目标与边界

实现按 `(repo_url, target_branch)` 全局串行排队的 `MergeActor`，确保多个并发子 `worker`（或跨多个并发 Run 指向同一 `target_branch`）流式产出的候选分支**绝不互相覆盖、忙时绝不阻塞或丢单**，严格执行“源头负责制”：
1. **确定性目标分支寻址**：
   - Actor Type 为 `"MergeActor"`，Actor ID 为 `MergeActorID(repoURL, effectiveTargetBranch)`（`sha256(git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch)` 的 64 位 hex，完全 URL 安全）；
   - 当 HCL `merge.target_branch == null` 时，`effectiveTargetBranch = git.WorkBranchName(runID, mergeNodeCanonicalAddr)`；显式指定时使用该 `target_branch`。
2. **持久化 FIFO 合并队列 + 单在途任务锁**：
   - `EnqueueCandidate(ctx, req)` 将候选请求（按 `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)` 幂等去重！）立即追加进 Actor 持久队列并注册/唤醒调度 Reminder，毫秒级返回释放调用方；
   - 同一 `MergeActor` 同一时刻至多只有 **1 个**候选处于 `IN_FLIGHT`：向 `ProcessRegistryActor.ClaimSlot` 申请 Worker 槽位，调用 Worker 的 `run_merge_candidate`（底层执行 `018` `ExecuteMergeCandidate`）。
3. **严格源头负责与已合入禁止回滚**：
   - 若返回 `MergeStatusSucceeded`：记录该候选已在目标分支合入（最新 `MergedCommitSHA`），释放槽位，回调 `RunActor.OnMergeCandidateResult` 推进状态，并立即拉起队列下一单（**已合入提交严禁回滚**）；
   - 若返回 `MergeStatusReworkSource`（冲突或 `merge.gatekeeper` 失败）：保留目标分支当前指针不变，将 `LatestTargetSHA` 与冲突/测试失败报告回调给 `RunActor`，由 `RunActor` 将对应源头 `worker` 递增 `epoch` 并将其基线升级至 `LatestTargetSHA` 修复重跑！队列中其他无关联候选继续正常消费，不被卡死。

**前置依赖**：分片 `018`、`023`、`024`、`025`。

---

## 2. 涉及包与文件清单

- `internal/actor/merge/types.go`：队列项 `CandidateTicket`、结果回调消息与取消请求结构
- `internal/actor/merge/actor.go`：`MergeActor`（`EnqueueCandidate`、`CancelByRun`、`ReportCandidateResult`、`OnWorkerDead`、`ReminderCall`）
- `internal/actor/merge/merge_test.go`：Tier 1 幂等入队、串行单飞、冲突打回不阻塞后序候选、Worker `DEAD` 自动重派单测
- `internal/actor/merge/merge_integration_test.go`（`//go:build integration`）：Tier 2 真实 `daprd` + 本地裸 Git 仓库 + Podman 跑通 3 个并发候选入队（2 成功 + 1 冲突打回带回最新 `LatestTargetSHA`）

---

## 3. 保姆级实现细则

1. **取消与过期剔除（`CancelByRun`）**：
   - 当所属 Run 执行 `immediate` 暂停、`delete` 或祖先重跑时，`RunActor` 调用 `MergeActor.CancelByRun` 移除队列中尚未开始的对应条目；若某条目正处于 `IN_FLIGHT`，通知对应 Worker 取消并在结果返回时按 `RequestID` 丢弃。
2. **跨 Run 共享目标分支保护标记**：
   - `CandidateTicket` 记录 `SharedTargetBranch bool`（即 HCL 显式配置了 `merge.target_branch != null`）；一旦合入成功，在回报给 `RunActor` 的结果中置位 `MergedIntoSharedBranch: true`，供 `028` 单节点 `rerun` 精确实施 `409 Conflict` 防护！
3. **Worker 失联自动重试**：
   - 若执行当前 `IN_FLIGHT` 候选的 Worker 被 `ProcessRegistryActor` 判为 `DEAD`，由于 `018` `ExecuteMergeCandidate` 基于 `--force-with-lease` 天然幂等安全，`MergeActor` 直接将该队首条目重置回 `PENDING` 重新抢槽执行！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`merge_test.go`）**：
   - 连续并发入队 `W1, W2, W3`：验证同一时刻仅有 `W1` 派发给 Worker；`W1` 冲突打回后，`W2` 立即自动接续派发且不被阻塞；
   - 重复入队相同 `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)` 不会产生重复队列项；
   - `IN_FLIGHT` 期间 Worker `DEAD` 自动触发重抢槽重跑。
2. **Tier 2 真实 Dapr + Git + Podman 联调（`merge_integration_test.go`，`//go:build integration`）**：
   - 在真实 `daprhost` + `WorkerDaemon` + 本地 Git 裸仓上并发提交 3 条分支，验证目标分支形成干净线性历史且冲突分支准确收到前两条合入后的最新 `LatestTargetSHA`。
