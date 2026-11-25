# 专项收口任务：升级并切换至 `golden` 原生 `output` 块（依赖 `golden#18`）

## 1. 前置解锁条件（Blocked Until Upstream Resolved）

- **上游依赖 Issue**：https://github.com/lonegunmanb/golden/issues/18 （*Support built-in output block in golden*）
- **触发时机**：当 `lonegunmanb/golden#18` 关闭且主干合入内置 `OutputBlock` 后，立即作为一个独立的最小微切片执行本任务（遵循 [AGENTS.md](../../AGENTS.md) 铁律：**凡 `golden` 原生提供的能力，严禁在 `sofa` 中保留手造副本**）。
- **前置检查**：开始前先通过 `gh issue view 18 -R lonegunmanb/golden` 及检查 `golden` 最新源码确认 `OutputBlock` 已在 `golden` 的 `registerCommonBlock()` 中内置注册。

---

## 2. 变更范围与文件清单

1. **升级依赖**：
   - `go.mod` / `go.sum`：`go get github.com/lonegunmanb/golden@latest`（锁定包含内置 `OutputBlock` 的提交）并执行 `go mod tidy`。
2. **删除手造实现**：
   - 删除 `internal/module/spec/output.go`（或仅保留供 `007` Plan 序列化使用的纯数据快照 DTO，**彻底移除自定义 `OutputBlock` 及其 `BaseDecode` / `ExecuteDuringPlan` / `Value` 实现**）；
   - 删除对应的手造块单测 `internal/module/spec/output_test.go`（若该包在 `007` 之前无其他文件，同步清理空包目录）。
3. **切换 `planner`（及 `module` / `plan`，若 `007` 已完成）至 `golden` 原生 `OutputBlock`**：
   - 修改 `internal/planner/config.go`：
     - 从 `RegisterBlocks()` 中删除 `golden.RegisterBlock(&modulespec.OutputBlock{})`（与 `variable`、`locals` 一样交由 `golden` 自动内置注册，禁止重复注册）；
     - 将 `SofaConfig.Outputs()` 改为通过 `golden.Blocks[*golden.OutputBlock](c)` 读取 `golden` 原生输出块并导出稳定快照；
   - 若分片 `007` 已合入，同步将子 `module` 输出收集与 `internal/plan` 快照导出对接到 `*golden.OutputBlock`。

---

## 3. 验收与回归门禁（TDD Verification）

1. 先更新/保留 `internal/planner/` 下的集成回归测试（包括已知值、`UnknownVal` 静态类型保留、`sensitive = true` 显式脱敏、敏感 `variable -> locals -> output` 派生脱敏、缺 `value` / 非法 `description` / 非法 `sensitive` / 未知属性与嵌套块报错、`cold_start_test.go` 冷启动加载）；
2. 确认无任何自定义 `RegisterBlock` 注册 `"output"`；
3. 通过 `gofmt`、`go vet ./...`、`go test ./... -count=1` 全量检查；
4. 按单切片门禁原子 `git commit` 并 `git push` 至 `origin/main`。
