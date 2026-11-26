# 分片 009：不可变 `Plan` 编解码、`.sofaplan` 自包含归档与离线 CLI

## 1. 目标与边界

作为阶段一最终收口分片（原 `007` 拆分后的第三片），在 `001`–`008` 全部就绪的基础上，交付不可变 `Plan` 序列化、脱敏审查视图、自包含 `.sofaplan` 归档包以及完整的离线 `sofa` 命令行工具：
1. **不可变 `Plan` 模型、无损编解码与脱敏展示（`internal/plan`、`internal/planner/export.go`）**：
   - 将根 `SofaConfig` 及递归子模块 `ModulePlan` 导出为不可变 `*plan.Plan`（含全部节点拓扑、配置值、延迟表达式、工具定义、`plugins.lock.json` 快照与输出表）；
   - 使用 `ctyjson.MarshalType` + `ctymsgpack.Marshal` + `SensitivePaths` 实现已知/未知 `cty.Value` 与 `golden.SensitiveMark` 的 100% 无损往返编解码（`Marshal` / `Unmarshal`）；
   - 提供面向终端人工审查的脱敏 JSON 格式化器（`Display`：敏感叶节点统一渲染为 `"<sensitive>"`，未知叶节点统一渲染为 `"<known after apply>"`）。
2. **自包含 `.sofaplan` ZIP 归档写入与安全解包（`internal/plan/archive.go`）**：
   - 将 `manifest.json`、`plan.json`、可选 `plugins.lock.json` 与 `bundle/{config,modules}/**` 静态资源打包为字节级确定性可重现的 `.sofaplan` 归档文件（权限 `0600`），打包前校验所有引用的 bundle 内 `skill_directories` 真实存在；
   - 解包时实施严格防篡改 SHA-256 核验与 Zip-Slip / 符号链接 / 解压炸弹安全防御。
3. **根变量契约导出与 Cobra 离线 CLI（`internal/variableschema`、`internal/cli`、`cmd/sofa`）**：
   - 实现无需赋必填变量即可导出的 `sofa schema [--json]`；
   - 交付 `sofa init`、`sofa schema`、`sofa plan` 三大命令并打通端到端测试。

**前置依赖**：分片 `001`–`008`。

---

## 2. 新增依赖与包文件清单

在 `go.mod` 中引入：
- `github.com/spf13/cobra`（对齐 `r42` CLI）

涉及包与文件：
- `internal/plan/model.go`：`Plan`、`NodeSpec`、`ModuleSpec`、`ToolSpec`、`OutputSpec`、`Origin` 及只读访问器
- `internal/plan/codec.go`：`Marshal` / `Unmarshal`（`ctyjson` + `ctymsgpack` + `SensitivePaths`）
- `internal/plan/display.go`：面向终端审查的脱敏 JSON 渲染器（`<sensitive>` / `<known after apply>`）与含敏感值告警检测（`ContainsSensitiveValues`）
- `internal/plan/archive.go`：`SaveArchive` / `LoadArchive`、`Manifest`、文件权限 `0600`（含 Windows ACL / Unix mode 适配）、`skill_directories` 存在性校验与 Zip-Slip 防御
- `internal/planner/export.go`：将已完成 `RunPlan()` 的 `*SofaConfig` 递归导出为 `*plan.Plan`
- `internal/variableschema/schema.go`：根模块 `variable` 静态契约提取与 JSON/文本格式化（敏感默认值自动脱敏）
- `internal/cli/command.go` & `cmd/sofa/main.go`：`sofa init`、`sofa schema`、`sofa plan` 命令实现

---

## 3. 核心技术设计细则

### 3.1 不可变 `Plan` 导出与编解码契约（对齐 `r42/internal/plan`）
1. **覆盖全量 `sofa` 节点种类**：
   - 按确定性顺序（拓扑层级 + 地址字典序）导出所有块节点：`source`、`preserve`、`sandbox`、`harness`、`model_provider`（确保 `api_key` / `bearer_token` 携带 `golden.SensitiveMark`！）、`mcp_server`（确保 `bearer_token` / 敏感 `headers`/`env` 携带 `golden.SensitiveMark`！）、`go_tool`、`external_tool`、`worker`、`dynamic_worker`、`merge`、`module`（递归内嵌子 `*plan.Plan`）。
   - 同时保存模块级 `LocalExpressions`、已注册强类型工具元数据 `Tools`（含 JSON Schema / `QuotePaths` / `Postconditions`）以及从 `.sofa/plugins.lock.json` 读取的 `PluginLockFile`。
2. **无损 `wireValue`**：
   - 使用 `value.UnmarkDeepWithPaths()` 拆出无标记 `cty.Value` 与 `[]cty.PathValueMarks`；
   - 类型用 `ctyjson.MarshalType`、值用 `ctymsgpack.Marshal`（原生支持嵌套 `cty.UnknownVal`！）、敏感路径编码为 `SensitivePaths`；`Unmarshal` 时恢复路径并验证每条路径可合法作用于解码后的值。

### 3.2 `.sofaplan` 自包含归档规范（`internal/plan/archive.go`）
```text
example.sofaplan (ZIP archive, mode 0600)
├── manifest.json          # {"format_version": 1, "sofa_dsl": "sofa", "plan_sha256": "...", "plugins_lock_sha256": "...", "bundle_sha256": "..."}
├── plan.json              # Marshal(planned) 字节流
├── plugins.lock.json      # 可选：存在远端 harness 锁定时写入，哈希记入 plugins_lock_sha256
└── bundle/
    ├── config/            # 来自 .sofa/config 的快照文件（排除 .initialized.json / .git / .sofa / *.sofaplan）
    └── modules/           # 来自 .sofa/modules 的快照文件
```
- **确定性可重现构建**：所有 ZIP 条目按字典序排序写入，修改时间固定为 `time.Unix(0, 0).UTC()`，`bundle_sha256` 由所有 `bundle/**` 条目的相对路径与内容哈希确定性聚合计算。
- **`skill_directories` 打包校验**：若根模块或子模块内的 `worker` / `qc` / `dynamic_worker` 配置了位于 `bundle/` 相对路径下的 `skill_directories`（如 `${path.module}/skills` -> `config/skills`），`SaveArchive` 必须核验对应目录在 `bundle/` 中真实存在，缺失则立即报错！
- **安全解包防护（`LoadArchive`）**：
  - 拒绝空归档、缺失 `manifest.json` 或 `plan.json`、未知 `format_version`；
  - 拒绝任何包含绝对路径、`\`、`..` 路径段、非预期顶层前缀、符号链接模式位（`os.ModeSymlink`）或超出单文件/总解压字节上限（可配置默认上限）的 ZIP 条目；
  - 严格比对解压出的 `plan.json`、`plugins.lock.json` 与 `bundle/**` 实际 SHA-256 与 `manifest.json` 一致，任何字节篡改立即拒绝报错！

### 3.3 CLI 命令契约（`cmd/sofa` & `internal/cli`）
1. `sofa init [source] [--state-dir .sofa] [--upgrade]`
   - 默认 `source` 为 `"."`；串联调用 `007` 的 `project.Init`（含 `module.Init`）与 `harness.LockPlugins`，输出简洁初始化摘要。
2. `sofa schema [--state-dir .sofa] [--json]`
   - 打开已初始化的 `.sofa/config`，调用 `variableschema.Inspect` 输出根模块 `variable` 定义列表（支持 JSON 与可读文本两种输出，无需提供必填变量值，`sensitive = true` 的 `default` 严格置为 `null` 并标记 `default_redacted: true`）。
3. `sofa plan [-var 'k=v'] [-var-file path] [--state-dir .sofa] [--out path.sofaplan]`
   - 同时支持单横杠 `-var` / `-var-file` 与双横杠 `--var` / `--var-file`（可重复指定，优先级遵循 `001`：`.sofavars` < `*.auto.sofavars` < `SOFA_VAR_*` < CLI `-var` / `-var-file` 按出现顺序后者覆盖前者）；
   - 打开已初始化的 `.sofa` 状态目录，对 `.sofa/config` + `.sofa/modules` 执行完整 `SofaConfig` 规划与 `*plan.Plan` 导出（核对远端 `harness` 已存在于 `.sofa/plugins.lock.json`）；
   - 向 `stdout` 输出脱敏后的 `Display` JSON；
   - 当指定 `--out` 时，写出 `0600` 权限的自包含 `.sofaplan` 归档包，且若 Plan 内包含敏感值，向 `stderr` 输出未加密敏感值保管警告（对齐 `r42`）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Plan 编解码无损往返与全量脱敏展示**：
   - 构造包含 `source`、`preserve`、`sandbox`、远端 `harness`（敏感 `options`）、`model_provider`（敏感 `api_key`）、`mcp_server`（敏感 `bearer_token`）、`go_tool`、`external_tool`、静态 `worker`、`dynamic_worker`、`merge`、嵌套 `module` 与敏感 `output` 的完整规划，验证 `Marshal -> Unmarshal` 往返后所有已知值、`UnknownVal` 类型、延迟表达式与 `SensitiveMark` 100% 一致；
   - 验证 `Display` JSON 中绝不出现任何明文敏感字符串（全部替换为 `"<sensitive>"`），所有未知值显示为 `"<known after apply>"`。
2. **`.sofaplan` 确定性打包、跨目录解包、缺失 `skill_directories` 拦截与防篡改 / Zip-Slip 防御**：
   - 相同输入连续两次 `SaveArchive` 产出的 `.sofaplan` 字节 SHA-256 完全一致；
   - 移动到全新临时目录执行 `LoadArchive`，验证 `plan.json`、`plugins.lock.json` 与 `bundle/config/skills/...`、`bundle/modules/...` 完整还原；
   - 配置不存在的 `${path.module}/missing_skills` 时 `SaveArchive` 报错；
   - 构造篡改 `plan.json` 字节但未改 `manifest.json`、篡改 `bundle/` 文件、含 `../../evil` 条目、含符号链接条目、超大小上限的恶意 ZIP，`LoadArchive` 均精确拦截报错。
3. **`variableschema` 与 Cobra CLI 全链路端到端单测**：
   - 必填变量未赋值时 `sofa schema --json` 依然成功返回完整 schema，且敏感变量默认值严格脱敏；
   - 通过内存 `bytes.Buffer` 驱动 `sofa init` -> `sofa schema --json` -> `sofa plan -var ... -var-file ... --out workflow.sofaplan` 全链路退出码为 `0`，未先运行 `sofa init` 直接 `sofa plan` 返回清晰引导错误。
