# 分片 017：Epoch 分支二阶段检查点保存与跨机对齐恢复算法

## 1. 目标与边界

严格按照 [README.md](../../README.md)“检查点与恢复”及 [docs/architecture-contract.md](../architecture-contract.md) 第 5.2 节定义，实现防僵尸覆盖（Epoch Fencing）的二阶段检查点持久化与跨机器恢复对齐引擎 `internal/checkpoint`：
1. **控制面状态端口抽象（`NodeStateStore`）**：
   - 将未来阶段三 `RunActor`（登记检查点分支）与 `NodeActor`（`epoch` 栅栏校验、追加日记、标记检查点完整、读取历史检查点序列）的交互收敛为显式 Go 接口，并提供纯内存/本地 JSON 实现供阶段二单测与集成测试驱动。
2. **按 Epoch 隔离的二阶段检查点保存（`SaveCheckpoint`）**：
   - 每个执行轮次使用独立远端分支 `sofa/ckpt/<run_id>/<node_key>/e<epoch>`（首次推送前必须先通过控制面在受管分支账本登记成功）；
   - 严格落实**“先写日记记初始 SHA → 提交推送产生新 SHA 则补写最终 SHA → 上传 `/.sofa/artifacts` 非 Git 快照至 `BlobStore` → 校验远端分支、日记与 `BlobStore` 三方一致且 `epoch` 未过期后标记完整”**。
3. **跨机器从新到旧搜索最近完整检查点恢复（`RecoverLatestAligned`）**：
   - 在新机器进入 `e<epoch+1>` 时，沿历史检查点从新到旧扫描，跳过未完成记录或日记 `final_sha` 不在对应远端检查点分支提交历史中的损坏候选，定位最近一个三方完全对齐的完整检查点，拉取代码、从 `BlobStore` 还原 `/.sofa/artifacts` 半成品，并组装结构化日记流作为续跑历史记忆。

**前置依赖**：分片 `011`、`012`、`016`。

---

## 2. 涉及包与文件清单

- `internal/checkpoint/model.go`：`CheckpointRecord`、`Status`（`StatusPending`、`StatusComplete`）、`NodeStateStore` 接口、`ErrStaleEpoch`、`ErrNoAlignedCheckpoint`
- `internal/checkpoint/local_store.go`：供阶段二测试与单机独立验证使用的并发安全内存/文件 `LocalNodeStateStore`
- `internal/checkpoint/manager.go`：`Manager`、`SaveCheckpoint`、`RecoverLatestAligned`、`FormatDiaryPromptContext`
- `internal/checkpoint/checkpoint_test.go`：Tier 1 纯核心单测（模拟三阶段任意中断点、僵尸旧 `epoch` 写拦截、远端分支未推上去时的向前搜索回退）
- `internal/checkpoint/checkpoint_integration_test.go`（`//go:build integration`）：Tier 2 基于真实本地 Git bare 仓库 + 本地 `BlobStore` 模拟“机器 A 半途断电 → 机器 B 新目录接管续跑”全闭环测试

---

## 3. 保姆级实现细则

### 3.1 数据结构与 `epoch` 栅栏接口（`model.go`）

```go
type Status string

const (
    StatusPending  Status = "PENDING"
    StatusComplete Status = "COMPLETE"
)

type CheckpointRecord struct {
    Seq               uint64            `json:"seq"`                 // 单调递增序号 1, 2, ...
    Epoch             uint64            `json:"epoch"`               // 产生该检查点的执行轮次
    Branch            string            `json:"branch"`              // 如 "sofa/ckpt/run1/worker_impl_a1b2c3d4/e1"
    InitialSHA        string            `json:"initial_sha"`         // 提交前工作区 HEAD SHA
    FinalSHA          string            `json:"final_sha"`           // 推送远端后的最终提交 SHA（无代码改动时等于 InitialSHA）
    ArtifactsSnapshot *blob.ObjectMeta  `json:"artifacts_snapshot,omitempty"` // /.sofa/artifacts 非空时的 BlobStore 快照
    Diary             session.DiaryInput `json:"diary"`
    Status            Status            `json:"status"`
}

type NodeStateStore interface {
    // RegisterCheckpointBranch 在首次创建/推送 e<epoch> 分支前向 Run 账本登记（若 epoch 已过期返回 ErrStaleEpoch）
    RegisterCheckpointBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error
    // BeginCheckpoint 校验当前权威 epoch == reqEpoch，分配下一个 Seq 并持久化 StatusPending 记录与初始日记（含 InitialSHA）
    BeginCheckpoint(ctx context.Context, reqEpoch uint64, branch, initialSHA string, diary session.DiaryInput) (uint64, error)
    // AppendFinalSHA 当 git commit 产生了不同于 initialSHA 的新提交时，校验 reqEpoch 后将 finalSHA 追加写入该条日记与检查点记录
    AppendFinalSHA(ctx context.Context, reqEpoch, seq uint64, finalSHA string) error
    // CompleteCheckpoint 校验当前权威 epoch == reqEpoch，记录 ArtifactsSnapshot 并将 Status 置为 StatusComplete
    CompleteCheckpoint(ctx context.Context, reqEpoch, seq uint64, finalSHA string, artifactsSnap *blob.ObjectMeta) error
    // ListCheckpoints 按 Seq 从大到小（从新到旧）返回该节点全部历史检查点与完整已提交日记流
    ListCheckpoints(ctx context.Context) ([]CheckpointRecord, error)
}
```

- **铁律**：`NodeStateStore` 的每一次写方法（`RegisterCheckpointBranch`、`BeginCheckpoint`、`AppendFinalSHA`、`CompleteCheckpoint`）都必须核对 `reqEpoch` 等于当前权威 `epoch`；一旦节点已因超时失联或人工立即暂停升级到了 `epoch + 1`，旧机器的任何残余写入必须立即收到 `ErrStaleEpoch` 并中止！

### 3.2 二阶段检查点保存精确步骤（`SaveCheckpoint`）
给定 `(ctx, epoch, workspaceDir, artifactsDir, writablePaths, diary)`：
1. **前置越界检查**：调用 `gitAdapter.StatusPaths` + `git.FindUnauthorizedPaths` 确认当前 `workspaceDir` 无 `writable_paths` 之外的脏文件（若有则直接报错，绝不把越界文件打进检查点！）。
2. **登记当前轮次检查点分支（幂等）**：读取当前 `workspaceDir` 的 `HEAD` 为 `initialSHA`；计算 `ckptBranch = git.CheckpointBranchName(runID, nodeAddr, epoch)`，若本轮尚未登记则调用 `stateStore.RegisterCheckpointBranch(ctx, epoch, repoURL, ckptBranch, initialSHA)`。
3. **阶段一（预写 Pending 日记）**：调用 `seq, err := stateStore.BeginCheckpoint(ctx, epoch, ckptBranch, initialSHA, diary)`。
4. **阶段二（Git 提交推送 + 补写最终 SHA + 上传非 Git 产物）**：
   - 调用 `finalSHA, committed, err := gitAdapter.CommitAllAndPush(ctx, workspaceDir, repoURL, ckptBranch, msg, auth)`（注意：即使 `committed == false` 即本轮无代码变更，若远端 `ckptBranch` 尚未指向 `initialSHA`，仍需推送确保远端分支存在且指向 `initialSHA`！）；
   - 若 `committed && finalSHA != initialSHA`：调用 `stateStore.AppendFinalSHA(ctx, epoch, seq, finalSHA)`（把提交后的真实 SHA 补记入日记，严禁把提交前的 `initialSHA` 当成最终版本！）；
   - 调用 `011` 的 `artifactSvc.SnapshotStagingDir(ctx, ckptBlobKey, artifactsDir)` 将 `/.sofa/artifacts/` 下的非 Git 文件（若有）打包上传至 `BlobStore`，获得 `artifactsSnap`。
5. **阶段三（三方对齐核验与标记 Complete）**：
   - 调用 `gitAdapter.BranchContainsSHA(ctx, repoURL, ckptBranch, finalSHA, auth)` 确认远端分支真实包含 `finalSHA`；
   - 若 `artifactsSnap != nil`，调用 `blobStore.Stat(ctx, artifactsSnap.Key)` 确认对象可读且 `SHA256` 一致；
   - 调用 `stateStore.CompleteCheckpoint(ctx, epoch, seq, finalSHA, artifactsSnap)` 原子置为 `StatusComplete`！

### 3.3 跨机器恢复对齐搜索（`RecoverLatestAligned`）
当新机器以新轮次 `newEpoch` 尝试恢复该节点时：
1. 调用 `records, err := stateStore.ListCheckpoints(ctx)`（按 `Seq` 从新到旧排序）；若列表为空，返回 `ErrNoAlignedCheckpoint`（上层据此从 `based_source` 全新开始）。
2. 从新到旧遍历每条 `rec`：
   - 若 `rec.Status != StatusComplete` 或 `rec.FinalSHA == ""`：**丢弃该未完成候选，继续往前找**；
   - 调用 `_, contains, err := gitAdapter.BranchContainsSHA(ctx, repoURL, rec.Branch, rec.FinalSHA, auth)`：若远端不存在该分支或分支历史中不含 `rec.FinalSHA`（例如旧机器写完状态前远端分支异常丢失或未对齐），**丢弃该候选，继续往前找**；
   - 若 `rec.ArtifactsSnapshot != nil`：调用 `blobStore.Stat` 核对 `SHA256` 一致，缺失或不匹配则**丢弃继续往前找**；
   - 找到第一个完全对齐的 `rec` 后：
     1) 先在账本登记新轮次分支 `newBranch = git.CheckpointBranchName(runID, nodeAddr, newEpoch)`（基线设为 `rec.FinalSHA`）；
     2) 调用 `gitAdapter.PrepareWorkspace(ctx, repoURL, rec.Branch, rec.FinalSHA, destWorkspaceDir, auth)` 将新机器 `/workspace` 精确检出到 `rec.FinalSHA`；
     3) 若 `rec.ArtifactsSnapshot != nil`，调用 `artifactSvc.RestoreStagingDir(ctx, rec.ArtifactsSnapshot, destArtifactsDir)` 还原 `/.sofa/artifacts`；
     4) 收集所有 `Seq <= rec.Seq` 且 `Status == StatusComplete` 的历史日记（过滤掉已被舍弃的尾随残缺记录！），按时间正序格式化为结构化历史记忆提示词返回！
3. 若遍历完所有记录均无合法对齐项，返回 `ErrNoAlignedCheckpoint`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 故障注入与 Epoch 栅栏单测（`checkpoint_test.go`）**：
   - 验证无代码变更轮次（`finalSHA == initialSHA`）与有代码变更轮次（`finalSHA != initialSHA`，验证 `AppendFinalSHA` 被调用且记录更新）均正确完成；
   - **故障注入矩阵**：
     - 情况 A：`Seq=1` 完整，`Seq=2` 在 `CommitAllAndPush` 后、`CompleteCheckpoint` 前崩溃（`StatusPending`）→ `RecoverLatestAligned` 自动跳过 `Seq=2`，精准恢复到 `Seq=1` 的代码与日记；
     - 情况 B：`Seq=2` 虽标了 `Complete` 但模拟远端分支不含其 `FinalSHA`（未对齐）→ 自动回退恢复到 `Seq=1`；
     - 情况 C：`Seq=2` 的 `ArtifactsSnapshot` 在 `BlobStore` 缺失 → 自动回退恢复到 `Seq=1`；
   - **僵尸进程隔离**：在 `CommitAllAndPush` 期间将 `LocalNodeStateStore` 的权威 `epoch` 从 `1` 升为 `2`，验证旧进程随后的 `AppendFinalSHA` / `CompleteCheckpoint` 立即被 `ErrStaleEpoch` 拦截，绝不污染新轮次状态！
2. **Tier 2 跨目录真实 Git + `BlobStore` 恢复集成测试（`checkpoint_integration_test.go`，`//go:build integration`）**：
   - 机器 A 目录完成第 1 次检查点（改代码 + 写 `/.sofa/artifacts/draft.md`）→ 第 2 次改代码写到一半模拟崩溃未完成检查点 → 彻底删除机器 A 工作目录 → 在全新机器 B 目录以 `epoch=2` 调用 `RecoverLatestAligned` → 验证机器 B 的 `/workspace` 代码、`/.sofa/artifacts/draft.md` 内容与恢复出的历史日记 100% 回到第 1 次完整检查点状态，并能继续提交推送到 `e2` 分支！
