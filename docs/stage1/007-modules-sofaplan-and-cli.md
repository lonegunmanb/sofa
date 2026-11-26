# 分片 007：`sofa init`、`module` 递归规划、`.sofaplan` 自包含包与离线 CLI

## 1. 目标与边界

完成阶段一最终收口，交付完整的离线命令行工作流：
1. **`sofa init [source] [--upgrade]`**：
   - 将根配置目录（本地路径或 `go-getter` 远端仓库子目录）及递归引用的所有 `module` 安装到 `<cwd>/.sofa/config` 与 `<cwd>/.sofa/modules/...` 快照目录；
   - **对齐 `tflint` 的 Harness 插件校验与跨平台哈希锁定**：扫描根配置与所有已安装子模块中声明了 `source` + `version` 的 `harness` 块，拉取对应 GitHub Release（`v{version}`）的 `checksums.txt` 并按 `signature` 模式完成校验（`auto` 优先验 GitHub OIDC Artifact Attestations 回退 PGP、`pgp` 验 `checksums.txt.sig` + `signing_key`、`none` 跳过签名仅验哈希；自动支持 `GITHUB_TOKEN` 与 `GITHUB_TOKEN_<normalized_host>`），将全平台 `sofa-harness-<name>_<os>_<arch>.zip` 的 SHA-256 固化到 `<cwd>/.sofa/plugins.lock.json`；
2. **`module "<name>"` 块递归规划**：静态规划子模块，支持将 `source` 分支、`work_branch`、`artifact` 引用作为模块输入/输出跨模块传递，并实现 **Root 单例 `sandbox` / `harness` / `model_provider` 向子模块的自动正交继承**；
3. **不可变 `Plan` 编解码与自包含 `.sofaplan` ZIP 归档包**：使用 `ctyjson` + `ctymsgpack` 无损保存已知/未知值与敏感路径标记，并将冻结的 `plan.json`、`plugins.lock.json` 与 `bundle/`（根快照 + 已安装子模块静态文件，如 `skills/`、脚本）打包进同一个确定性 `.sofaplan` 文件；
4. **CLI 命令集（`cmd/sofa`）**：`sofa init`、`sofa schema [--json]`、`sofa plan [-var ...] [-var-file ...] [--out xxx.sofaplan]`。

**前置依赖**：分片 `001`–`006`。

---

## 2. 新增依赖与包文件清单

在 `go.mod` 中按需引入（均为 `r42` 已验证依赖）：
- `github.com/hashicorp/go-getter/v2`
- `github.com/spf13/cobra`

涉及包与文件：
- `internal/project/project.go`：管理 `<cwd>/.sofa/`（`config/`、`modules/`、`plugins.lock.json`、`state.json` 源哈希元数据）
- `internal/module/init.go`：扫描 `module` 块的字面量 `source`，防环路安装至 `.sofa/modules/<labels...>`（`DisableSymlinks: true`，远端 URL 报错脱敏）
- `internal/harness/lock.go`：`sofa init` 阶段扫描根模块与已安装子模块中带 `source` + `version` 的 `harness` 块，通过可注入 `ReleaseFetcher` 拉取并校验 GitHub Release `checksums.txt`（支持 `auto` / `attestation` / `pgp` / `none` 及 `GITHUB_TOKEN` / `GITHUB_TOKEN_<normalized_host>`），生成确定性的 `.sofa/plugins.lock.json`
- `internal/module/spec/module.go`：`ModuleBlock`（实现 `CustomDecode` + `PlanBlock` + `SingleValueBlock`，过滤元属性/元块统一使用 `golden.MetaAttributeNames` / `golden.MetaNestedBlockNames`）与 `ModulePlanner` 接口（子模块输出直接使用 `golden` 原生 `*golden.OutputBlock`）
- `internal/plan/model.go`：`Plan`、`NodeSpec`、`SourceSpec`、`PreserveSpec`、`SandboxSpec`、`HarnessSpec`、`PluginLock`、`ProviderSpec`、`MCPServerSpec`、`ToolSpec`、`OutputSpec`、`Origin`
- `internal/plan/codec.go`：`Marshal` / `Unmarshal`（采用 `r42/internal/plan/codec.go` 同款 `ctyjson.MarshalType` + `ctymsgpack.Marshal` + `SensitivePaths` 编码，完美支持嵌套 `UnknownVal`！）
- `internal/plan/display.go`：面向人类终端审查的脱敏 JSON 格式化器（所有携带 `golden.SensitiveMark` 的叶节点替换为 `"<sensitive>"`，未知值展示为 `"<known after apply>"`）
- `internal/plan/archive.go`：`.sofaplan` 自包含 ZIP 归档写入器与读取器（`SaveArchive` / `LoadArchive`）
- `internal/variableschema/schema.go`：`sofa schema --json` 根变量契约导出（无需提供必填变量值即可输出根模块 `variable` 列表、类型、描述、默认值脱敏状态）
- `internal/cli/command.go` & `cmd/sofa/main.go`：Cobra CLI 组装

---

## 3. 核心技术设计细则

### 3.1 `module "<name>"` 输入未知值放行与 Root 单例向下继承

1. **`ModuleBlock.Decode` 的未知输入放行（重要升级！）**：
   - 在 `r42` 中，模块输入仅放行了 `kind == "artifact"` 的未知值；
   - 在 `sofa` 中，上游工序产出的 `worker.specify.work_branch`（`UnknownVal(String)`）、`worker.discover.result`（`UnknownVal(String)`）以及 `worker.plan.artifact.design` **都必须允许作为输入传给子 `module` 的 `variable`**！
   - 规则：只要传入值符合子模块对应 `variable` 声明的静态 `cty.Type`（即便 `!value.IsWhollyKnown()`），`SofaConfig` 在为子模块构造变量表时保留其类型化的 `cty.UnknownVal`，并在 Plan 快照中记录绑定表达式，使子模块内的 `worker` 能无缝接力上游 `work_branch`！
   - 反之，`module` 自身的固有元属性 `source`（必须是无变量引用的字面量字符串）、`parallelism`（可选正整数）、`timeout`（可选正 duration 字符串）必须在 Plan 期完全已知。

2. **Root 单例 `sandbox` / `harness` / `model_provider` 向子模块正交继承规则**：
   - 当 `PlanChildModule` 规划子模块目录时，对 `sandbox`、`harness`、`model_provider` 三类单例基础设施块分别独立评估：
     - 先扫描子模块自身的 `loaded.Blocks`：
       - 若子模块自身声明了 `>= 1` 个该类块（`sandbox` / `harness` / `model_provider`），子模块内部按自己的 `InspectRootDefaults` 规则走；
       - 若子模块自身声明了 **`0` 个**该类块，则**自动继承 Root Module 解析出的唯一默认 `sandbox`、`harness` 或 `model_provider` 描述符**！
       - 若子模块自身声明了 `0` 个 `sandbox`，而 Root Module 声明了 `0` 个或 `>= 2` 个 `sandbox`（存在歧义），且子模块的 `worker` 未获得显式 `sandbox` 引用，则返回清晰的跨模块错误：`module "<addr>" worker "<w>" has no sandbox: declare a sandbox in the module or leave a single default sandbox in the root module`（同理，若 Root Module 存在 `>= 2` 个 `harness` 或 `model_provider` 且子模块 `worker` 未显式指定或置 `null`，返回清晰的跨模块歧义错误）。

3. **路径重定位（适配跨机器 `.sofaplan` 分发！）**：
   - 在 `sofa init` 后，根模块位于 `<state_dir>/config`，子模块位于 `<state_dir>/modules/<a>/<b>`。
   - 为了让打包后的 `.sofaplan` 解压到任意远端宿主机目录（如 `/var/lib/sofa/bundles/<hash>/`）都能工作，Plan 期 `path.module` 在求值时使用相对于 Bundle 根目录的稳定占位符（或规范化相对路径：根模块为 `config`，`module.sub` 为 `modules/sub`，通过 `${path.module}/skills` 拼接出 `config/skills` 或 `modules/sub/skills`），并在 `SaveArchive` 时校验引用的 `skill_directories` 在 Bundle 内真实存在！

---

### 3.2 `.sofaplan` 自包含 ZIP 归档格式（`internal/plan/archive.go`）

使用 Go 标准库 `archive/zip`（统一压缩时间戳 `time.Unix(0, 0)`、文件路径按字典序写入，保证相同输入生成的 `.sofaplan` 字节哈希稳定可重现）：

```text
example.sofaplan (ZIP archive, file mode 0600)
├── manifest.json          # {"format_version": 1, "sofa_dsl": "sofa", "plan_sha256": "...", "plugins_lock_sha256": "...", "bundle_sha256": "..."}
├── plan.json              # wirePlan (由 internal/plan/codec.go Marshal 生成，完整保留 cty 类型、UnknownVal 与 SensitivePaths)
├── plugins.lock.json      # 可选（当存在远端 GitHub Release harness 时包含全平台 zip SHA-256 锁定表）
└── bundle/
    ├── config/            # 根模块快照文件（*.sofa.hcl, skills/**, 随附脚本等，排除 .git / .sofa / *.sofaplan）
    └── modules/           # 所有已安装子模块快照文件
```

- **安全防护（Zip Slip 防御）**：`LoadArchive` / 解包函数必须严格校验 ZIP 条目路径（拒绝绝对路径、`..` 路径穿越、符号链接条目、单文件与总解压大小上限，校验 `plan_sha256`、`plugins_lock_sha256` 与 `bundle_sha256` 一致性）。

---

### 3.3 CLI 命令契约（`cmd/sofa`）

1. `sofa init [source] [--state-dir .sofa] [--upgrade]`
   - 默认 `source` 为 `.`；复制根配置到 `.sofa/config` 并递归安装所有 `module` 到 `.sofa/modules`；对声明了远端 `source` + `version` 的 `harness` 块完成 Release `checksums.txt` 验签并写出 `.sofa/plugins.lock.json`（若未变更且未传 `--upgrade`，已锁定的匹配版本可直接复用）；拒绝未加密凭据残留到 `state.json` 或 `plugins.lock.json`。
2. `sofa schema [--state-dir .sofa] [--json]`
   - 不需要为必填 `variable` 赋值，直接读取 `.sofa/config` 输出根输入变量定义文档（支持 `The Machine` 或看板动态了解一个模板需要哪些入参）。
3. `sofa plan [-var 'k=v'] [-var-file path] [--state-dir .sofa] [--out path.sofaplan]`
   - 同时支持单横杠 `-var` / `-var-file` 与双横杠 `--var` / `--var-file`（可重复指定，后者覆盖前者，优先级与 `001` 一致）；
   - 向 `stdout` 输出脱敏后的人类可审查 JSON Plan（敏感值显示为 `"<sensitive>"`，未知值显示为 `"<known after apply>"`）；
   - 当指定 `--out` 时，以 `0600` 权限写出自包含 `.sofaplan` 归档包，并向 `stderr` 输出未加密敏感值安全提示（对齐 `r42`）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **`sofa init`、模块环路检测与 `plugins.lock.json` 生成**：本地相对模块 `./modules/a -> ./modules/b` 递归安装成功；`a -> b -> a` 模块源环路报错；修改原目录文件在重新 `sofa init` 前不影响已快照的 `.sofa/config`；通过注入 `FetchFunc` / `ReleaseFetcher` 测试远端模块安装、远端 `harness` 的 `checksums.txt` 跨平台哈希锁定与错误脱敏（纯离线单测，无需外网）。
2. **跨模块 DAG 与 Root 单例正交继承**：
   - 上游 `worker.specify.work_branch` 传给 `module "audit"`，子模块 `output` 再传给下游 `preserve` / `merge`，生成的全局节点依赖无环且完整；
   - 子模块不声明 `sandbox` / `harness` / `model_provider` 时，自动继承 Root 唯一的对应单例；Root 有 2 个 `sandbox`（或 2 个 `harness`）时子模块报错。
3. **Plan 编解码与脱敏展示**：
   - 含 `UnknownVal`、`SensitiveMark`、嵌套 `module`、`go_tool`、`dynamic_worker`、`fallback` 的复杂 Plan 经过 `Marshal -> Unmarshal` 往返无损；
   - `Display` JSON 中所有敏感字段（含 `model_provider.api_key`、`sensitive = true` 的变量派生值）严格脱敏为 `"<sensitive>"`，绝不泄漏明文。
4. **`.sofaplan` 归档打包、篡改校验与 Zip Slip 防御**：
   - 打包 README 完整“关闭邮件通知”+ 动态校验示例生成 `.sofaplan`，移动到全新临时目录调用 `LoadArchive` 解包，验证 `plan.json` 与 `bundle/config/skills/...` 完整可读；
   - 构造篡改了 `plan.json` 字节但未改 `manifest.json` 哈希的归档，以及含 `../../evil` 路径的恶意 ZIP，`LoadArchive` 均精确拒绝报错。
5. **CLI 端到端单测**：通过 `cobra` 内存 `bytes.Buffer` 驱动 `sofa init` -> `sofa schema --json` -> `sofa plan -var ... --out test.sofaplan` 全链路退出码为 0。
