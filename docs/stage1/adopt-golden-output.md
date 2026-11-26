# 专项收口任务：升级并切换至 `golden` 原生 `output` 块（已解锁 — 优先在 `004` 前执行！）

## 1. 上游状态（已合入解锁 ✅）

- **上游 Issue**：https://github.com/lonegunmanb/golden/issues/18 （状态：**CLOSED**）
- **上游最新提交**：`059cc8c`（包含 `a90ad4f` *Add built-in OutputBlock with value/description/sensitive support*、`775f7fc` 及 `059cc8c`）
- **执行优先级**：遵循 [AGENTS.md](../../AGENTS.md) 铁律（**凡 `golden` 原生提供的能力，严禁在 `sofa` 中保留手造副本**），**必须在推进 `004` 之前立即作为独立微切片完成本迁移任务并提交推送**。

---

## 2. 上游 `golden.OutputBlock` 已提供的精确接口（直接复用！）

在最新 `golden`（`>= 059cc8c`）中，`output` 已在 `registerCommonBlock()` 中自动注册，并在 `readRawHclSyntaxBlock` 中原生处理单标签 `["", name]`，公开接口如下：

```go
// 通过 golden.Blocks[*golden.OutputBlock](c) 获取所有输出块：
block.Name() string            // 输出名称标签
block.Value() cty.Value        // 求值结果（未求值前返回 cty.DynamicVal，sensitive=true 时自动带 golden.SensitiveMark）
block.Description() *string    // 可选已知非 null 字符串描述
block.Sensitive() bool         // 显式 sensitive=true 或 Value() 含 SensitiveMark 时返回 true
block.ValueExprString() string // 原始 value 表达式文本（供 007 Plan 快照序列化直接使用）
```

---

## 3. 变更范围与文件清单

1. **升级依赖**：
   - 执行 `go get github.com/lonegunmanb/golden@059cc8c`（或 `@latest`）与 `go mod tidy`，更新 `go.mod` / `go.sum`。
2. **彻底移除手造 `OutputBlock`**：
   - 删除 `internal/module/spec/output_test.go`；
   - 在 `internal/module/spec/output.go` 中**彻底删除 `OutputBlock` 及其所有 `golden` 接口方法**（若需保留供 `SofaConfig.Outputs()` 返回的纯数据快照结构体 `Output{ Name, Value, Description, Sensitive, Expression }`，可移至 `internal/planner` 或在 `internal/module/spec/output.go` 中仅保留该纯数据结构体，严禁保留任何自定义 `Block` 实现）。
3. **切换 `internal/planner` 至 `*golden.OutputBlock`**：
   - 修改 `internal/planner/config.go`：
     - 从 `RegisterBlocks()` 中**删除 `golden.RegisterBlock(&modulespec.OutputBlock{})`**（`golden` 已内置注册，切勿重复注册！）；
     - 将 `SofaConfig.Outputs()` 改为遍历 `golden.Blocks[*golden.OutputBlock](c)`，直接调用 `block.Name()`、`block.Value()`、`block.Description()`、`block.Sensitive()`（以及可选 `block.ValueExprString()`）构造按名称排序的快照列表。
4. **更新冷启动测试覆盖自定义块**：
   - 由于 `output` 现已内置于 `golden`，在 `internal/planner/cold_start_test.go` 的子进程配置中同时保留 `source "project"` + `output "cold_start"`，使该测试继续验证 `sofa` 自定义单标签块在 `LoadDirectoryContext` 冷启动时的预注册行为。

---

## 4. 验收与回归门禁（TDD Verification & Commit Gate）

1. 确认全仓不存在任何注册 `"output"` 块的 `golden.RegisterBlock` 调用，也不存在手造 `OutputBlock`；
2. 跑通 `internal/planner/` 全部输出与脱敏回归测试（已知值、`UnknownVal` 静态类型保留、`sensitive = true` 显式脱敏、敏感 `variable -> locals -> output` 派生脱敏、缺 `value` / 非法 `description` / 非法 `sensitive` / 未知属性与嵌套块报错、子进程冷启动）；
3. 通过 `gofmt`、`go vet ./...`、`go test ./... -count=1` 全量检查；
4. 按单切片门禁原子 `git commit` 并 `git push` 至 `origin/main`（确保 `git status` 干净）后，再开启分片 `004`。
