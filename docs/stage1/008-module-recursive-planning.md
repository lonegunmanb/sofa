# 分片 008：`module "<name>"` 递归规划、未知输入传递与 Root 单例继承

## 1. 目标与边界

作为原 `007` 拆分后的第二片，在 `007` 完成模块快照目录安装的基础上，实现 **`module "<name>"` 块的静态递归规划、跨模块类型化 `cty.UnknownVal` 传递、正交 Root 单例向下继承以及跨模块确定性地址/路径规范化**（不含 `.sofaplan` 归档打包与 CLI）：
1. **`ModuleBlock`（`internal/module/spec`）**：
   - 实现 `golden.CustomDecode`、`golden.PlanBlock` 与 `golden.SingleValueBlock`（元属性与元嵌套块严格复用 `golden.MetaAttributeNames` / `golden.MetaNestedBlockNames`）；
   - 允许将上游未知产出（`worker.specify.work_branch`、`worker.discover.result`、`worker.plan.artifact.design`、`dynamic_worker.x.work_branches`、`merge.x.work_branch`）作为输入传给子模块 `variable`，只要静态 `cty.Type` 与子模块 `variable` 声明兼容；
2. **`SofaConfig` 子模块递归规划与正交继承（`internal/planner`）**：
   - 递归规划已安装在 `<modules_dir>/<labels...>`（或测试态本地相对目录）的子模块，生成嵌套 `ModulePlan` 并暴露子模块 `output` 对象；
   - 实现 `sandbox`、`harness`、`model_provider` 三个维度的独立向下正交继承；
   - 提供跨模块唯一 `CanonicalAddress`（消除不同模块同名 `go_tool` / `external_tool` / `mcp_server` 的 ID 碰撞）与相对于 Bundle 根的稳定 `path.module`（根模块为 `config`，子模块为 `modules/<a>/<b>`）。

**前置依赖**：分片 `001`–`007`。

---

## 2. 包与文件清单

- `internal/module/spec/plan.go`：`Plan`、`ModulePlan`（含 `Directory`、`BundlePath`、`Outputs`、`Modules`、`Parallelism`、`Timeout` 与底层子规划引用）及深拷贝方法
- `internal/module/spec/module.go`：`ModuleBlock`、`ModulePlanner` 消费者接口、`InputExprs map[string]string`
- 修改 `internal/planner/config.go`、`defaults.go`、新增 `internal/planner/module.go`：
  - 在 `RegisterBlocks()` 中注册 `&modulespec.ModuleBlock{}`；
  - 实现 `SofaConfig.PlanChildModule(...)`、`CanonicalAddress(addr string) string`、Bundle 相对 `path.module` 计算与子模块 `variable` 输入绑定/未知值类型校验；
  - 支持跨模块 `SourceURL` 传递（当上游 `source` 或 `worker` 分支通过 `variable` 传入子模块、再从子模块 `output` 传回下游 `merge` / `preserve` 时保持链路合法）。

---

## 3. 核心技术设计细则

### 3.1 `ModuleBlock.Decode` 与未知输入放行规则
1. **固有元属性（必须在 Plan 期完全已知）**：
   - `source`（必填）：无变量引用的非空白字面量字符串（与 `007` `module.Init` 保持一致）；
   - `parallelism`（可选）：已知正整数（`> 0`），`null` 视为未设置；
   - `timeout`（可选）：已知正 Go duration 字符串（`time.ParseDuration` 且 `> 0`），`null` 视为未设置。
   - 过滤 `golden.MetaAttributeNames`（`for_each`、`depends_on`）与 `golden.MetaNestedBlockNames`（`precondition`、`dynamic`）；拒绝除 `precondition` / `dynamic "precondition"` 以外的任何嵌套块。
2. **业务输入属性（支持类型化 `cty.UnknownVal` 透传！）**：
   - 除 `source`、`parallelism`、`timeout` 与 `golden.MetaAttributeNames` 之外的所有属性均为传给子模块的输入变量（记录求值后的 `Inputs map[string]cty.Value` 与原始表达式 `InputExprs map[string]string`）。
   - 对子模块声明的 `variable "<v>"` 逐一核对：
     - 传入了子模块未声明的变量名 → 报错；
     - 子模块必填变量（无 `default`）未传入 → 报错；
     - `nullable = false`（或默认 `nullable = true`）且传入 `null` → 按 `nullable` 规则处理/拦截；
     - 应用 `typeexpr.Defaults`（若值已知）并用 `convert.Convert(val, varType)` 校验并转换类型（注意：`go-cty` 的 `convert.Convert` 原生支持对类型兼容的 `cty.UnknownVal(cty.String)` / 含未知字段的 `cty.ObjectVal` 做精确类型转换与 `SensitiveMark` 保留！若子模块 `variable` 声明了 `validation` 块，当输入 `IsWhollyKnown()` 时正常由 `golden` 执行校验，当输入含 `UnknownVal` 时保留静态类型检查并将 `validation` 留待运行期求值）；
     - 子模块若某 `variable` 标记了 `sensitive = true`，子模块内引用 `var.<v>` 依然自动携带 `golden.SensitiveMark`。

### 3.2 Root 单例 `sandbox` / `harness` / `model_provider` 向子模块正交继承
当父模块调用 `PlanChildModule` 规划子模块时，对 `sandbox`、`harness`、`model_provider` 三类单例分别**独立**决议：
1. 扫描子模块自身的 `loaded.Blocks`：
   - **子模块自身声明了 `>= 1` 个该类块**：子模块内部完全按自身的 `InspectRootDefaults` 规则走（1 个自动默认，`>= 2` 个要求子模块内显式指定），不受父模块同名维度干扰；
   - **子模块自身声明了 `0` 个该类块**：
     - 若父模块在该维度有唯一有效默认对象（例如父模块有唯一 `sandbox.go_env`、唯一 `harness.copilot`、或唯一 `model_provider.openai`），则向子模块提供该继承单例对象（使子模块内省略 `sandbox` / `harness` / `model_provider` 的 `worker`、`dynamic_worker`、带 `gatekeeper` 的 `merge` 无缝绑定父模块单例；子模块内显式写 `model_provider = null` 依然豁免继承！）；
     - 若子模块内某消费块需要默认 `sandbox`（或 `harness`），而子模块自身为 `0` 个且父模块为 `0` 个或 `>= 2` 个（无唯一可继承单例），则返回清晰的跨模块诊断错误（指明模块地址、消费块地址及缺失/歧义原因）；同理，若子模块为 `0` 个 `model_provider` 而父模块有 `>= 2` 个歧义 `model_provider`，且子模块消费块未显式写 `model_provider = null`，同样返回清晰的跨模块歧义错误！

### 3.3 `CanonicalAddress` 与 Bundle 相对 `path.module`
- **跨模块唯一地址**：当 `SofaConfig` 处于子模块前缀 `modulePrefix`（如 `"module.audit"` 或 `"module.audit.module.sub"`）下运行时，`CanonicalAddress("go_tool.check")` 返回 `"module.audit.go_tool.check"`。由此 `GoToolBlock.Id()`、`ExternalToolBlock.Id()` 与 `ServerBlock.ToolID()` / `ResourceID()` 在多模块下天然全局唯一！
- **可重定位 `path.module`**：当传入 `BundlePath`（初始化工程下根模块为 `"config"`，子模块为 `"modules/audit"` / `"modules/audit/sub"`）时，`EvalContext().Variables["path"]` 中的 `path.module` 使用规范化正斜杠相对路径（未传 `BundlePath` 的纯目录单测保持原有目录路径兼容），从而 `${path.module}/skills` 在跨机器 `.sofaplan` 解包后始终稳定指向 `bundle/config/skills` 或 `bundle/modules/audit/skills`！

---

## 4. 必测 TDD Red → Green 用例清单

1. **跨模块未知值与产物接力**：
   - 根模块 `source "project"` -> `worker "specify"` -> 将 `worker.specify.work_branch`、`worker.specify.artifact.spec` 与已知参数传给 `module "audit"` -> 子模块内 `worker` -> `dynamic_worker` -> `merge` -> 子模块 `output "verified_branch"` -> 根模块 `preserve "final" { branch = module.audit.verified_branch }` 全链路 `RunPlan()` 通过，且根模块 DAG 边 `worker.specify -> module.audit -> preserve.final` 完整无环！
   - 传入类型不匹配的未知值（如把 `UnknownVal(List(String))` 传给 `type = number` 的子模块 `variable`）、漏传子模块必填变量、传入未声明输入名、或 `module.source` 引用变量时均精确报错。
2. **正交 Root 单例跨模块继承矩阵**：
   - 子模块不声明 `sandbox` / `harness` / `model_provider`，根模块各有 1 个时，子模块内 `worker`、`dynamic_worker`、带 `gatekeeper` 的 `merge` 全部自动继承成功；子模块内 `worker` 写 `model_provider = null` 时成功豁免继承；
   - 子模块自己声明了 `harness "alt" {}` 时优先使用子模块自己的 `harness.alt`，同时仍可正交继承根模块的唯一 `sandbox.go_env`；
   - 根模块有 2 个 `sandbox`（或 2 个 `harness`、2 个 `model_provider`）且子模块声明 0 个时，精确报跨模块歧义错误。
3. **跨模块工具/MCP 确定性 ID 防碰撞、多级嵌套模块与冷启动测试**：
   - 两个子模块 `module "a"` 与 `module "b"` 内均声明同名 `go_tool "check"` 与 `mcp_server "docs"`，验证生成的确定性 `.id` / `.tool_ids` 因 `CanonicalAddress` 不同而互不冲突，且 `tool_name(go_tool.check)` 在子模块内正常工作；
   - `BundlePath` 模式下根与子模块 `${path.module}/skills` 分别求值为 `"config/skills"` 与 `"modules/audit/skills"`；
   - 在 `internal/planner/cold_start_test.go` 新增 `TestColdStartLoadModuleBeforeNewSofaConfig` 子进程测试。
