# 分片 018：单节点 Work + 冻结式 QC 双容器编排器与确定性 `rebase + gatekeeper` 原语

## 1. 目标与边界

串联 `011`–`017` 所有模块，交付宿主机 `sofa-worker` 执行单个静态/动态展开子 `worker` 的完整闭环状态机，以及供阶段三 `MergeActor` 直接调用的单次候选分支合并验证原语：
1. **单节点执行编排器（`internal/worker/runner/worker_runner.go`）**：
   - 完整驱动：**基线锁定 / 检查点恢复 → 导入 `import_artifact` → Work 容器会话（含自修门禁与兜底硬回滚）→ 停止 Work 容器实现 100% 物理冻结 → 容器 `gatekeeper` 校验 → 独立只读容器 `QC` 会话（支持跨 Harness / 跨模型异构互审）→ 最终检查点 → `human_approval` 版本绑定门禁 → 受管 `work_branch` 发布与不可变 `Artifact` 上传**；
   - 精确区分正常成功、人工等待（`WAITING_HUMAN`，保存完整检查点后释放全部容器！）、可触发显式 `fallback` 的结构化退回原因（`qc_exhausted`、`gatekeeper_exhausted`、`human_returned_upstream`、`agent_escalated`）与硬性报错。
2. **确定性单次合并候选验证原语（`internal/worker/runner/merge_primitive.go`）**：
   - 执行**“严格源头负责制”**单步操作：拉取目标分支最新 `M_k` 与候选分支 `W_i` → `git rebase` → 若冲突立即中止并返回冲突报告 + `M_k`；若无冲突则在 `merge.sandbox` 容器内运行 `merge.gatekeepers` → 失败则丢弃候选树并返回测试日志 + `M_k`；全部通过则推进远端目标分支指针！

**前置依赖**：分片 `013`、`016`、`017`。

---

## 2. 涉及包与文件清单

- `internal/worker/runner/types.go`：`NodeExecutionRequest`、`NodeOutcome`、`OutcomeStatus`（`Succeeded`、`WaitingHuman`、`FallbackTriggered`、`Failed`）、`ApprovalRecord`
- `internal/worker/runner/worker_runner.go`：`WorkerRunner` 与完整状态机流转
- `internal/worker/runner/merge_primitive.go`：`ExecuteMergeCandidate`、`MergeCandidateResult`
- `internal/worker/runner/runner_test.go`：Tier 1 全路径状态机单测（覆盖 `gatekeeper` 打回返工、`QC` 异构互审打回返工、越界路径耗尽硬回滚、`human_approval = "required"` 版本失效拦截、暂停释放容器后恢复）
- `internal/worker/runner/runner_integration_test.go`（`//go:build integration`）：Tier 2 真实本地 Git + 真实 Podman + 回环 Harness 插件跑通 Work → Gatekeeper → QC → 发布 `work_branch` & `Artifact` + `ExecuteMergeCandidate` 端到端测试

---

## 3. 保姆级实现细则

### 3.1 单节点 Work → Gatekeeper → QC 闭环步骤（`worker_runner.go`）

1. **步骤 1：准备宿主机私有工作目录与基线**：
   - 在宿主机创建本次执行的独占临时根 `<tempRoot>/<run_id>/<node_key>-e<epoch>/`，内设 `workspace/`、`imports/`、`artifacts/`、`work-session-store/`、`qc-session-store/`；函数退出时（无论成功、进入 `WAITING_HUMAN` 还是报错）`defer` 自动清理该宿主机临时根！
   - 优先调用 `017` 的 `ckptMgr.RecoverLatestAligned`：若命中完整检查点则检出其 `FinalSHA` 并还原 `artifacts/` 与历史日记；若返回 `ErrNoAlignedCheckpoint`，则调用 `gitAdapter.ResolveBranchSHA` 将 `based_source` 固定为 40 位 `baseSHA` 检出到 `workspace/`；
   - 调用 `011` 的 `artifactSvc.StageImports` 将所有 `import_artifact` 解压至 `imports/`。
2. **步骤 2：主返工循环（最多允许 `gatekeeper` / `QC` 返工轮数）**：
   - **2a. 运行 Work 容器会话**：
     - 准备 Work 所绑定的 `harness` 插件与交叉编译好的 `go_tool` 目录；
     - 启动 Work 容器（通过 `014` `runtime.Opener`），传入历史日记（首次之后同一节点内返工则传入 `work-session-store` 的 `resume_session_id` + 返工问题列表！）；
     - 若返回 `WaitingHuman`（触发了 `sofa_request_decision` 或 `sofa_pause_for_human`）：立即调用 `ckptMgr.SaveCheckpoint` 保存完整检查点，关闭并销毁容器，返回 `Outcome{Status: WaitingHuman}`！
     - 若返回 `AgentEscalated`：保存检查点（或收集声明的 `fallback.artifacts`），返回 `Outcome{Status: FallbackTriggered, Reason: "agent_escalated"}`；
     - 若因越界路径重试耗尽报错：先调用 `gitAdapter.RevertPaths` **强制回滚所有越界路径**，再返回失败！
     - Work 会话交卷后：**立即 `Close` Work 会话并销毁 Work 容器**——此时宿主机 `workspace/` 与 `artifacts/` 无任何读写容器挂载，进入 **100% 物理冻结态**！
   - **2b. 校验 `writable_paths` 与 `artifact` 产出**：
     - 宿主机再次核对 `FindUnauthorizedPaths == empty`；
     - 调用 `011` 的 `artifact.Validate(artifactsDir, decls)`：若有 `Issue`（如漏写必填 `artifact` 或文件为空），若返工次数未超限则带着 `Issue` 列表重新进入 **2a** 唤醒 Work 容器补齐，超限则报错。
   - **2c. 执行确定性 `gatekeeper` 容器**：
     - 若配置了 `gatekeepers`，调用 `013` 的 `sandboxEngine.RunGatekeepers`；
     - 若失败：若 `gatekeeperAttempts < maxGatekeeperReworks`（默认复用 `max_qc_rounds` 或固定上限 `3`），将失败命令、退出码与 `stdout/stderr` 格式化为返工提示回到 **2a**；超限则检查是否声明了匹配 `"gatekeeper_exhausted"` 的 `fallback`（有则返回 `FallbackTriggered`，无则返回 `Failed`）。
   - **2d. 执行独立只读容器 `QC` 会话**：
     - 若配置了 `qc` 块：解析有效配置（未显式覆盖的 `harness` / `model_provider` / `model` 默认继承所属 `worker`，支持异构覆盖！）；
     - 挂载同一份 `workspace/`（**强制 `:ro`**）、`artifacts/`（**强制 `:ro`**）与 `imports/`（`:ro`）启动独立 `QC` 容器，运行 `016` 的 `RunQCPhase`（传入宿主机持有的跨轮次 `QCLedger`）；
     - 若裁决为 `revise`：若 `qcRound < qc.MaxQCRounds`，带着当前未决 `QC-xxx` 问题详情回到 **2a** 让 Work 返工；若已达 `MaxQCRounds`，检查是否声明了 `"qc_exhausted"` `fallback`（有则返回 `FallbackTriggered`，无则返回 `Failed`）。
3. **步骤 3：最终检查点、`human_approval` 版本绑定与正式发布**：
   - 调用 `ckptMgr.SaveCheckpoint` 将通过全部门禁的最终代码与产物落成完整检查点，得到最终 `finalCommitSHA`；
   - 调用 `artifactSvc.Publish` 上传所有有效声明产物至 `BlobStore`，获得按名称排序的 `publishedArtifacts`（提取有序 `artifactIDs`）；
   - **核验 `human_approval`**：
     - 若 `human_approval == "required"`：查询控制面当前已记录的人工批准 `ApprovalRecord{CommitSHA, ArtifactIDs}`——**只有当 `Approved == true` 且 `CommitSHA == finalCommitSHA` 且 `ArtifactIDs` 完全一致时才放行**！若尚未批准或代码/产物在旧批准后发生过任何变动（哈希变化），立即返回 `Outcome{Status: WaitingHuman, PendingApprovalVersion: ...}` 等待人类对当前精确版本拍板！
   - **权威 `epoch` 栅栏与发布 `work_branch`**：
     - 先向控制面校验当前 `epoch` 未过期并在分支账本登记正式 `workBranch = git.WorkBranchName(runID, canonicalNodeAddr)`（基线 `baseSHA`）；
     - 将 `finalCommitSHA` 推送至远端 `refs/heads/<workBranch>`；
     - 返回 `NodeOutcome{Status: Succeeded, WorkBranch: workBranch, CommitSHA: finalCommitSHA, ResultJSON: termResult, Artifacts: publishedArtifacts}`！

### 3.2 确定性合并原语（`merge_primitive.go`）
- `ExecuteMergeCandidate(ctx, req MergeCandidateRequest) (MergeCandidateResult, error)`：
  1. 若 `merge.target_branch == nil`，目标分支使用本 Run 受管分支 `git.WorkBranchName(runID, mergeNodeAddr)`（首次从 `based_source` SHA 登记创建）；否则使用显式 `target_branch`；
  2. 调用 `012` 的 `gitAdapter.RebaseCandidate`：若 `Conflicted == true`，直接返回 `MergeCandidateResult{Status: MergeReworkSource, LatestTargetSHA: out.TargetSHA, ConflictFiles: out.ConflictFiles, FailureReport: out.Details}`（绝不跑 Agent 改代码，直接让上层把原 `worker` 基线升级到 `LatestTargetSHA` 重跑！）；
  3. 若无冲突且 `len(req.Gatekeepers) > 0`，对 rebase 后的候选目录调用 `013` 的 `RunGatekeepers`：若失败，丢弃临时目录并返回 `MergeCandidateResult{Status: MergeReworkSource, LatestTargetSHA: out.TargetSHA, GatekeeperFailure: gkFail}`；
  4. 全部通过后，使用 `git push --force-with-lease=<targetBranch>:<out.TargetSHA>`（防并发竞态覆盖！）将目标分支快进更新至 `out.RebasedSHA`，返回 `MergeSucceeded`！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 状态机单测（`runner_test.go`）**：
   - 第 1 轮 `gatekeeper` 失败 → 自动回 Work 修复 → 第 1 轮 `QC` 开出 `QC-001` 打回 → 自动回 Work 修复 → 第 2 轮 `QC` 核销 `QC-001` 并 `pass` → 成功发布 `work_branch` 与 `Artifact`；
   - 验证 `QC` 启动时的 `SessionMountLayout.ReadOnlyWorkspace == true` 且 `Work` 容器在进入 `gatekeeper`/`QC` 前已被 `Close`；
   - `human_approval = "required"`：无批准时进入 `WaitingHuman`；附带旧 `CommitSHA` 的过期批准被拒绝；附带精确匹配 `(finalCommitSHA, artifactIDs)` 的批准成功发布；
   - `qc_exhausted` 时：声明了对应 `fallback` 返回 `FallbackTriggered`，未声明则返回 `Failed`。
2. **Tier 2 真实 Git + Podman 全闭环测试（`runner_integration_test.go`，`//go:build integration`）**：
   - 在真实本地 Git 仓库 + Podman 容器中驱动脚本化测试 Harness 插件完成代码修改、生成 `/.sofa/artifacts/report.json`、通过容器内真实 shell `gatekeeper` 与只读 `QC` 容器审查，验证远端仓库成功生成 `work_branch` 且 `BlobStore` 中可读取完整产物；
   - 验证 `ExecuteMergeCandidate` 顺序合入两个不冲突分支成功，合入第三个冲突（或破坏单测使 `gatekeeper` 失败）的分支时目标分支保持前两次合入结果不回滚，并准确返回最新 `LatestTargetSHA` 与失败报告。
