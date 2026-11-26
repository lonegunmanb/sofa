# 分片 012：Git 受管分支账本与零凭据落盘真实 Git 适配层

## 1. 目标与边界

严格按照纯领域核心与真实基础设施适配分层的铁律，交付 `sofa` 的代码仓库操作与分支卫生底座：
1. **纯领域受管分支账本（`internal/git/ledger.go` — Tier 1 纯内存单测）**：
   - 强制执行**“先登记成功，才允许创建/推送远端分支”**的顺序约束（涵盖正式 `work_branch` 与各轮次检查点分支 `sofa/ckpt/<run_id>/<node_key>/e<epoch>`）；
   - 将 `based_source` 锁定为精确 40 位小写十六进制提交 SHA；
   - 在 Run 结束或删除时，精确计算需删除的受管中间分支列表——**绝对保留所有 `source` 分支及匹配任一 `preserve` 的分支，绝对不触碰未在本 Run 账本登记的任何分支**。
2. **宿主机 `git` CLI 适配层（`internal/git/cli.go` — Tier 1 命令构造单测 + Tier 2 `//go:build integration` 真实仓库测试）**：
   - **不做宿主机共享 bare 缓存**：每次节点直接在独立目录执行 `--single-branch` 从远端拉取构造自包含 `/workspace`；
   - **零凭据落盘**：`/workspace/.git/config` 内严禁写入任何 Token 或密码；`token_ref` 与 `ssh_key_ref` 仅在宿主机单次 `fetch` / `push` / `ls-remote` 进程环境中临时注入；
   - 提供 `writable_paths` 越界改动精确检测与回滚、提交推送、远端分支删除、以及供 `merge` 复用的临时候选树 `git rebase` 检测。

**前置依赖**：分片 `010`。

---

## 2. 涉及包与文件清单

- `internal/git/ledger.go`：`Ledger`、`BranchKind`（`KindWorkBranch`、`KindCheckpointBranch`、`KindMergeBranch`）、`BranchEntry`、`CleanupPlan`、规范分支名生成器
- `internal/git/pathmatch.go`：工作区变更检测与 `writable_paths` glob 匹配器（支持 `**` 通配符）
- `internal/git/ledger_test.go`：账本与 `writable_paths` 匹配纯单测（Tier 1）
- `internal/git/adapter.go`：`Adapter` 显式接口、`AuthConfig`、`RebaseOutcome`
- `internal/git/cli.go`：基于宿主机 `os/exec` 调用 `git` 的 `CLIAdapter` 实现
- `internal/git/cli_integration_test.go`（首行 `//go:build integration`）：针对真实本地 bare + working Git 仓库的完整闭环集成测试（Tier 2）

---

## 3. 保姆级实现细则

### 3.1 分支命名与纯领域账本（`internal/git/ledger.go`）

1. **确定性分支名格式**：
   - `WorkBranchName(runID, canonicalNodeAddr string) string` → `"sofa/work/" + sanitizeRefSegment(runID) + "/" + sanitizeNodeKey(canonicalNodeAddr)`（将 `.`、`[`、`]`、`"` 等统一规范化为安全 Git ref 字符，并对完整 `canonicalNodeAddr` 附带 8 位短 SHA-256 后缀防止 `a.b` 与 `a_b` 碰撞！）；
   - `CheckpointBranchName(runID, canonicalNodeAddr string, epoch uint64) string` → `fmt.Sprintf("sofa/ckpt/%s/%s/e%d", sanitizeRefSegment(runID), sanitizeNodeKey(canonicalNodeAddr), epoch)`。
2. **核心状态转换**：
   - `Register(repoURL, branch, baseSHA string, kind BranchKind, ownerNode string, epoch uint64) error`：
     - 校验 `baseSHA` 必须是合法的 40 位小写 hex SHA；
     - 若 `branch` 等于已声明的受保护 `source` 分支（且非显式允许覆盖共享分支的 `merge.target_branch`），直接拒绝报错！
     - 写入账本状态 `Registered = true`（支持可序列化 `Snapshot() / Restore()` 以便阶段三直接嵌入 `RunActor` State）。
   - `EnsureRegistered(repoURL, branch string) error`：`Adapter` 创建/推送受管分支前必须先经此校验，未登记直接拒绝！
   - `PlanCleanup(sources []SourceRef, preserves []PreserveRef) []BranchTarget`：
     - 遍历本 Run 账本内所有已登记分支，剔除匹配任一 `(source.URL, source.Branch)` 或 `(preserve.URL, preserve.Branch)` 的分支（URL 比较前统一去除末尾 `.git` 与末尾 `/` 规范化），仅返回剩余本 Run 受管中间分支（含全部 `e1`, `e2` 历史检查点分支与未被 `preserve` 选中的 `work_branch`）。

### 3.2 宿主机 `git` CLI 适配层与零凭据注入（`internal/git/cli.go`）

```go
type AuthConfig struct {
    TokenRef  *string // 环境变量名，由 LookupEnv 在宿主机内存读取
    SSHKeyRef *string // 环境变量名（值可为本机私钥文件路径，或以 "-----BEGIN" 开头的私钥原文）
}

type Adapter interface {
    ResolveBranchSHA(ctx context.Context, repoURL, branch string, auth AuthConfig) (string, error)
    PrepareWorkspace(ctx context.Context, repoURL, fetchBranch, pinSHA, destDir string, auth AuthConfig) error
    StatusPaths(ctx context.Context, workspaceDir string) ([]ChangedPath, error)
    RevertPaths(ctx context.Context, workspaceDir string, paths []string) error
    CommitAllAndPush(ctx context.Context, workspaceDir, repoURL, remoteBranch, message string, auth AuthConfig) (newSHA string, committed bool, err error)
    BranchContainsSHA(ctx context.Context, repoURL, remoteBranch, targetSHA string, auth AuthConfig) (headSHA string, contains bool, err error)
    DeleteRemoteBranch(ctx context.Context, repoURL, remoteBranch string, auth AuthConfig) error
    RebaseCandidate(ctx context.Context, repoURL, targetBranch, candidateBranch, workDir string, auth AuthConfig) (RebaseOutcome, error)
}
```

1. **独立自包含 `/workspace` 初始化（`PrepareWorkspace`）**：
   - 在空目录 `destDir` 执行：
     - `git init`
     - 配置本仓库提交者身份 `git config user.name "sofa-worker"` & `git config user.email "sofa-worker@sofa.local"`（避免宿主机未配全局 `user.name` 导致 `git commit` 失败！）
     - **不配带凭据的 `remote origin`**：直接调用 `git fetch --no-tags <cleanRepoURL> +refs/heads/<fetchBranch>:refs/remotes/sofa/base`（若按历史检查点分支恢复同理），核验 `pinSHA` 存在后执行 `git checkout --detach <pinSHA>`（或切本地 `work` 分支指向 `pinSHA`）。
2. **内存级零落盘认证（`buildAuthEnv`）**：
   - **当配置 `TokenRef` 时**：通过 `LookupEnv(*TokenRef)` 读取 Token（为空则报错）；使用 Git 原生环境变量配置传递认证头（绝不拼进命令行参数防 `ps` 暴露，绝不写进 `.git/config`）：
     - `GIT_TERMINAL_PROMPT=0`
     - `GIT_CONFIG_COUNT=1`、`GIT_CONFIG_KEY_0=http.extraHeader`、`GIT_CONFIG_VALUE_0="Authorization: Basic " + base64("x-access-token:" + token)`（兼容 GitHub / GitLab / Gitea HTTPS 仓库）；错误信息返回前必须把 `token` 字符串替换为 `"<redacted>"`！
   - **当配置 `SSHKeyRef` 时**：通过 `LookupEnv(*SSHKeyRef)` 读取值：若以 `"-----BEGIN"` 开头，在宿主机私有临时目录写 `0600` 临时密钥文件并在本次命令 `defer os.Remove` 立即销毁；设置 `GIT_SSH_COMMAND=ssh -i "<keyPath>" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new`。
3. **`writable_paths` 精确校验与回滚（`pathmatch.go` + `RevertPaths`）**：
   - 调用 `git -c core.quotepath=false status --porcelain=v1 -uall` 获取所有已修改、已删除、重命名及未跟踪普通文件路径（统一转为 `/` 分隔）；
   - `FindUnauthorizedPaths(changed []ChangedPath, writableGlobs []string) []string`：当 `writableGlobs == nil`（未配置 `writable_paths`）时允许全部路径；当显式配置为空切片 `[]string{}` 时**不允许任何修改**；否则逐个匹配 `**` / `*` / `?` glob，返回不匹配的越界路径有序列表；
   - `RevertPaths`：对越界的已跟踪文件执行 `git checkout HEAD -- <path>`，对越界的未跟踪文件在 `os.OpenRoot(workspaceDir)` 内删除文件及清理变空的父目录，**100% 保留白名单内的合法改动**。
4. **`RebaseCandidate`（供 `018` 与阶段三 `MergeActor` 复用）**：
   - 在临时目录拉取 `targetBranch`（记录当前最新 `TargetSHA`）与 `candidateBranch`，检出候选分支执行 `git rebase FETCH_HEAD`：
     - 若冲突：收集冲突文件列表与冲突摘要，执行 `git rebase --abort`，返回 `RebaseOutcome{Conflicted: true, TargetSHA: targetSHA, ConflictFiles: ..., Details: ...}`；
     - 若无冲突：返回 `RebaseOutcome{Conflicted: false, TargetSHA: targetSHA, RebasedSHA: headSHA}` 留在 `workDir` 供紧接着运行容器 `gatekeeper` 测试！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 纯单测（`ledger_test.go`）**：
   - 未登记分支调用 `EnsureRegistered` 报错；尝试把 `source` 分支注册为临时工作分支报错；
   - 混合登记 `e1`、`e2` 检查点分支与多个 `work_branch`，`PlanCleanup` 精确剔除 `source` 分支与 `preserve` 匹配分支（含 `.git` 后缀 URL 归一化匹配），且绝不返回未登记分支；
   - `FindUnauthorizedPaths` 覆盖 `nil`（全放行）、`[]`（全拦截）、`["cmd/**", "internal/**/*.go", "go.mod"]` 精确区分合法与越界路径。
2. **Tier 2 真实 Git 集成测试（`cli_integration_test.go`，`//go:build integration`）**：
   - 使用 `git init --bare` 创建真实本地远端仓库并灌入初始提交；
   - 验证 `ResolveBranchSHA` → `PrepareWorkspace`（检查 `<workspace>/.git/config` 不含任何敏感凭据）→ 修改允许路径与越界路径 → `StatusPaths` + `FindUnauthorizedPaths` 抓出越界文件 → `RevertPaths` 仅回滚越界文件并保留合法改动 → `CommitAllAndPush` 推送新分支 → `BranchContainsSHA` 验证历史祖先关系 → `DeleteRemoteBranch` 删除远端分支；
   - 构造两条并行修改同一行与修改不同文件的分支，验证 `RebaseCandidate` 在无冲突时产出线性 `RebasedSHA`，冲突时准确返回 `Conflicted: true`、冲突文件名与最新 `TargetSHA`。
