# 分片 007：工作区 `init`、模块递归安装与 Harness 插件跨平台锁定

## 1. 目标与边界

作为原 `007` 拆分后的第一片，专注完成纯配置安装与插件哈希锁定的基础设施层（不含 `ModuleBlock` DAG 递归规划、不含 `.sofaplan` 编解码与 Cobra CLI）：
1. **状态目录与事务化快照（`internal/project`）**：
   - 将根配置源（本地目录或 `go-getter` 远端地址）快照复制到 `<state_dir>/config`，维护 `.initialized.json`、`state.json` 与 `.initializing` 中断恢复事务；
   - 对远端源 URL 自动脱敏（剥离凭据、Fragment 与非 `ref` 查询参数），确保 `state.json` 绝不落盘明文密钥。
2. **`module` 源码扫描与防环递归安装（`internal/module/init.go`）**：
   - 扫描根配置及各级子模块的字面量 `module "<name>" { source = "..." }` 声明，按规范化地址 `<state_dir>/modules/<labels...>` 递归安装本地或远端模块（`DisableSymlinks: true`，支持注入 `FetchFunc` 离线单测），精确拦截重名冲突（含大小写不敏感路径冲突）与 `a -> b -> a` 模块源环路。
3. **对齐 `tflint` 的 Harness 插件验签与跨平台哈希锁定（`internal/harness/lock.go`）**：
   - 扫描 `<state_dir>/config` 与所有已安装 `<state_dir>/modules/**` 中声明了 `source` + `version` 的 `harness` 块，拉取对应 GitHub Release（`v{version}`）的 `checksums.txt` 并按 `signature` 模式完成校验，生成确定性的 `<state_dir>/plugins.lock.json`。

**前置依赖**：分片 `001`–`006`。

---

## 2. 新增依赖与包文件清单

在 `go.mod` 中按需引入：
- `github.com/hashicorp/go-getter/v2`（对齐 `r42` 模块安装）
- `github.com/ProtonMail/go-crypto`（用于 `signature = "pgp"` 或 `"auto"` 回退时的 OpenPGP `checksums.txt.sig` 验签）

涉及包与文件：
- `internal/project/project.go`：`Init`、`Open`、原子临时目录替换、`.initializing` 中断检测、跳过 `.sofa` / `.git` / `*.sofaplan` 及拒绝非常规文件（符号链接/设备文件）
- `internal/project/state.go`：`State`、`SaveOutputs`、`ReadOutputs`、远端源地址凭据脱敏与 `0600` 原子写盘
- `internal/module/init.go`：`Init`、`Directory`、`IsLocalSource`、`Fetch`（`DisableSymlinks: true`）与递归模块源环路检测
- `internal/harness/lock.go`：`LockPlugins`、`PluginLockFile`、`PluginLockEntry`、`ReleaseFetcher` 与 `AttestationVerifier` 注入接口、`GITHUB_TOKEN` / `GITHUB_TOKEN_<normalized_host>` 解析

---

## 3. 核心技术设计细则

### 3.1 `<state_dir>` 目录结构与事务安全（对齐 `r42/internal/project`）

```text
.sofa/
├── .initializing          # 初始化进行中事务锁文件（异常中断后 Open 报错提示重新运行 sofa init，下次 Init 自动触发全量刷新）
├── state.json             # 项目状态元数据（format_version = 1, source, source_key, directory, initialized_at, outputs）
├── plugins.lock.json      # 远端 GitHub Release harness 插件全平台 zip SHA-256 锁定表（无远端 harness 时可省略或写入空表）
├── config/                # 根模块快照目录（排除 .sofa / .git / *.sofaplan）
│   └── .initialized.json  # 快照一致性标记（format_version = 2, source_key, initialized_at）
└── modules/               # 递归安装的子模块快照树（如 modules/audit/nested/...）
```

- **快照隔离保证**：`project.Init` 完成后，修改原始源码目录中的 `*.sofa.hcl` 在下次重新执行 `Init` 前**绝不影响** `.sofa/config` 与 `.sofa/modules` 中的快照内容。

### 3.2 `module` 安装契约（`internal/module/init.go`）
1. `module.source` 在 `Init` 阶段**必须是无变量引用的非空白字面量字符串**（`len(attr.Expr.Variables()) == 0` 且求值为非空 `cty.String`）。
2. 模块标签必须是合法单段本地路径名（禁止 `.`、`..`、`/`、`\`），同一目录内禁止同名或仅大小写不同的 `module` 标签冲突。
3. 维护调用栈集合（本地路径用 `"local:" + cleanAbsPath`，远端源用 `"remote:" + source`），发现环路立即返回 `module source cycle at module.<a>.module.<b>`。
4. 当 `Upgrade == false` 且目标模块目录已存在时直接复用；当 `Upgrade == true`、根 `source_key` 变化或上次 `.initializing` 中断时，在临时目录全量拉取后原子替换 `modules/`。

### 3.3 Harness 插件验签与 `plugins.lock.json`（对齐 `tflint`）
1. **扫描与去重冲突检查**：
   - 扫描 `<state_dir>/config` 及 `<state_dir>/modules/**` 内所有 `harness "<name>"` 块：
     - 本地模式（`source` 与 `version` 均省略或为 `null`）跳过远端锁定；
     - 远端模式（声明了已知字面量或无未知依赖的 `source` + `version`；若 `harness` 在 `init` 期使用了 `var.*` 等未知变量导致 `source`/`version` 无法静态求值，则返回清晰错误要求 `harness` 的 `source`/`version`/`signature`/`signing_key` 在 `init` 期静态可求值，或求值静态默认值）；
     - 若根模块与子模块声明了相同 `(name, source, version)` 且验签策略兼容则合并去重；若同一插件 `<name>` 声明了冲突的不同 `(source, version)`，返回明确的版本冲突错误。
2. **认证环境变量解析（绝不落盘）**：
   - 按主机名优先级查找环境变量：`GITHUB_TOKEN_<NORMALIZED_HOST>`（将 `<host>` 中非 `[A-Za-z0-9]` 字符如 `.`、`-`、`:` 统一替换为 `_` 并转大写，例如 `ghe.example.com:8443` -> `GITHUB_TOKEN_GHE_EXAMPLE_COM_8443`），未设置且 `<host>` 为 `github.com`（或通用回退）时读取 `GITHUB_TOKEN`。
   - Token 仅通过内存传递给 `ReleaseFetcher`，**严禁**写入 `plugins.lock.json`、`state.json` 或错误信息。
3. **`checksums.txt` 解析与四模式验签**：
   - 解析 Release `v{version}` 下的 `checksums.txt`（标准 `<64-hex-sha256>  <filename>` 格式），提取所有形如 `sofa-harness-<name>_<os>_<arch>.zip` 的平台资产哈希（至少包含 1 个有效资产，且必须包含 `linux_amd64` 或 `linux_arm64` 容器目标平台之一，否则报错）；
   - 按 `signature` 模式执行校验：
     - `"none"`：仅锁定 `checksums.txt` 中的 SHA-256，不验签；
     - `"pgp"`：拉取 `checksums.txt.sig` 并使用 `signing_key`（ASCII-armored OpenPGP 公钥）验证 `checksums.txt` 原文签名，失败报错；
     - `"attestation"`：调用可注入的 `AttestationVerifier` 校验 GitHub OIDC Artifact Attestation（归属 `<owner>/<repo>` 与 `checksums.txt` 摘要），失败报错；
     - `"auto"`（默认）：优先调用 `AttestationVerifier`；若目标 Release 未提供 Attestation 且配置了 `signing_key` 则回退 PGP 验签；若两者均不可用或验签失败则报错。
4. **缓存复用与确定性落盘**：
   - 若 `Upgrade == false` 且现有 `plugins.lock.json` 中已存在相同 `(name, source, version)` 的完整锁定记录（且验签级别不低于当前声明），直接复用已有锁定条目，无需重复网络请求；
   - 按插件 `name` 与平台 `<os>_<arch>` 字典序稳定排序，以 `0600` 权限原子写入 `<state_dir>/plugins.lock.json`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **`project.Init` 与 `project.Open` 生命周期**：
   - 本地目录初始化成功生成 `.sofa/config/.initialized.json`、`.sofa/state.json`、`.sofa/modules`，且自动跳过源目录内的 `.git`、`.sofa` 与 `*.sofaplan`；
   - 源目录位于 `.sofa` 内部时拒绝初始化；存在非常规文件（如 symlink）时拒绝复制；
   - 修改源目录文件后，未重新 `Init` 前不影响 `.sofa/config` 快照；残留 `.initializing` 锁文件时 `Open` 报错，再次 `Init` 自动走全量恢复并清理锁文件；
   - 远端源 URL 含用户名密码或敏感查询参数时，`state.json` 与拉取报错信息均严格脱敏。
2. **递归 `module.Init` 与防环**：
   - 本地多级模块 `./modules/a -> ./modules/b` 递归安装成功；
   - `a -> b -> a` 模块源环路精确报错；非字面量 `module.source = var.mod`、空 `source`、非法标签 `../evil`、大小写冲突模块名均报错；
   - 通过注入 `FetchFunc` 测试远端模块拉取、`Upgrade = false` 缓存复用与 `Upgrade = true` 覆盖刷新。
3. **`harness/lock.go` 验签与锁定**：
   - 纯本地 `harness "copilot" {}` 不触发网络拉取；
   - 通过注入内存 `ReleaseFetcher` 与 `AttestationVerifier` 完整测试 `auto`（Attestation 成功 / 回退真实 OpenPGP 签名成功 / 无签名失败）、`attestation`、`pgp`（真实临时生成的 OpenPGP 密钥对签名与篡改 `checksums.txt` 拒签）、`none` 四种模式；
   - 验证 `GITHUB_TOKEN_<NORMALIZED_HOST>` 与 `GITHUB_TOKEN` 解析优先级、`Upgrade = false` 复用现有 `plugins.lock.json`、以及多模块同名插件版本冲突报错。
