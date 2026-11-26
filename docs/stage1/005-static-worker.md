# 分片 005：静态 `worker` 块与嵌套契约校验

## 1. 目标与边界

实现 `sofa` 最核心的生产工序块 `worker "<name>"` 的完整 Plan 期语法、类型校验、Root 单例注入、`for_each` 展开兼容与未知上游表达式延迟求值。

**前置依赖**：分片 `002`（`source`）、`003`（`model_provider`/`mcp_server`/`sandbox`/`RootDefaults`）、`004`（`go_tool`/`external_tool`）、`004b`（`harness` 与 `RootDefaults.Harnesses`）。

---

## 2. 包与文件清单

- `internal/worker/spec/artifact.go`：`ArtifactBlock`、`ImportArtifactBlock`、`artifact("<name>")` 块内函数与 `import_artifact.<name>.path` 上下文注入
- `internal/worker/spec/gatekeeper.go`：`GatekeeperConfig`、`GatekeeperBlock` 校验
- `internal/worker/spec/qc.go`：`QCBlock` 与 `QCConfig` 校验（默认继承所属 `worker` 的 `harness` / `model_provider` / `model` / `sandbox`，且允许单独覆盖 `harness` 或 `model_provider` 实现异构互审）
- `internal/worker/spec/fallback.go`：`FallbackBlock`、触发枚举校验与**正向祖先合法性检查**
- `internal/worker/spec/schema.go`：`WorkerBlock`、`ToolUseBlock`、`Config`
- `internal/worker/spec/deferred.go`：未知上游值（`cty.UnknownVal`）检测与规范化延迟 HCL 表达式生成
- 修改 `internal/planner/config.go` & `defaults.go`：注册 `WorkerBlock`、在 `EvalContext()` 中解包 `worker` 的 `""` 空子类型键、在 `InitConfig` 前对省略 `sandbox`/`harness`/`model_provider` 的 `worker` 执行正交 Root 单例 AST 注入

---

## 3. 关键 `golden` 对接细节（必读防坑！）

### 3.1 `Valuable` 与 `for_each` 解包
- `WorkerBlock` 需要原生支持静态 `for_each`（例如 `for_each = toset(["linux", "windows"])`），因此：
  - `Type() string { return "" }`，`BlockType() string { return "worker" }`，`AddressLength() int { return 2 }`；
  - **实现 `golden.Valuable`（`Values() map[string]cty.Value`），不要实现 `SingleValueBlock`！**
  - 在 `SofaConfig.EvalContext()` 中必须解包空子类型层（见 `README.md` 铁律 3）：
    ```go
    if v, ok := ctx.Variables["worker"]; ok && !v.IsNull() && v.Type().IsObjectType() && v.Type().HasAttribute("") {
        ctx.Variables["worker"] = v.GetAttr("")
    }
    ```

### 3.2 块内专属局部函数与变量（覆写 `WorkerBlock.EvalContext()`）
基于 `b.BaseBlock.EvalContext()` 克隆出子上下文，额外注入两项：
1. **函数 `artifact("<art_name>")`**：
   - 仅允许引用**当前 `worker` 块内声明**的 `artifact "<art_name>"`（引用不存在的名字直接报错！跨块消费必须写 `worker.<up>.artifact.<name>` 或 `import_artifact`）；
   - 返回对象 `{ id: UnknownVal(String), name: StringVal(name), kind: StringVal("artifact"), type: ..., path: ..., description: ..., required: ..., non_empty: ... }`。
2. **变量 `import_artifact`**：
   - 遍历当前块内的 `import_artifact "<imp_name>"` 嵌套块，构造 `cty.ObjectVal`：每个 `<imp_name>` 映射为 `{ path: StringVal(effectivePath), desc: ... }`；
   - 若用户未显式填写 `import_artifact.path`，默认设为 `".sofa/imports/" + imp_name`；若显式填写，必须是工作区内干净的相对路径（禁止绝对路径、禁止 `..` 逃逸、禁止与同一 `worker` 内其他导入路径或产出 `artifact.path` 重叠）。
   - 这样 `prompt = "读取 ${import_artifact.design.path}"` 在 Plan 期即可直接求值为已知字符串！

### 3.3 Root 单例默认注入（`sandbox` / `harness` / `model_provider` 三者正交）
- 在 `InitConfig` 前扫描所有 `worker` 块（三类基础设施块完全正交，互不绑定）：
  - **显式置空免注入原则**：只要用户在该 `worker` 块源码中**已经写出了该属性名**（包括显式写 `harness = null` 或 `model_provider = null`），即视为用户已显式表态，**绝不执行 AST 默认注入**（例如 Root 定义了唯一 OpenAI `model_provider`，但某 `worker` 显式写 `harness = harness.antigravity` + `model_provider = null` 以使用 `antigravity` 自身的 `GEMINI_API_KEY`，不会被强塞 OpenAI 提供方；而 `sandbox = null` 则会在 `WorkerBlock` 自身校验阶段因缺少必填沙箱而报错）。
  - 若**未声明** `sandbox` 属性 → 调用 `RootDefaults.ResolveDefaultSandbox(addr)`：0 个或 ≥2 个直接返回错误；唯一 1 个则向该 `HclBlock` 注入 `sandbox = sandbox.<only>` 属性遍历。
  - 若**未声明** `harness` 属性 → 调用 `RootDefaults.ResolveDefaultHarness(addr)`：≥2 个直接返回歧义错误；唯一 1 个则注入 `harness = harness.<only>` 属性遍历；0 个保持不注入（回退内置默认 `"copilot"` Harness）。
  - 若**未声明** `model_provider` 属性 → 调用 `RootDefaults.ResolveDefaultModelProvider(addr)`：≥2 个直接返回歧义错误；唯一 1 个则注入 `model_provider = model_provider.<only>` 属性遍历；0 个保持不注入（回退所选 `harness` 的原生默认提供方）。

### 3.4 未知上游值（Deferred Expression）处理
- 当 `based_source = worker.specify.work_branch` 或 `prompt = "... ${worker.discover.result} ..."` 时，由于上游 `work_branch` / `result` 在 Plan 期是 `cty.UnknownVal(cty.String)`，直接用 `gohcl.DecodeBody` 往 Go `string` 字段解码会报错。
- **标准流程（参考 `r42/internal/research/spec/static_expression.go`）**：
  1. 先校验所有属性名与嵌套块类型均在白名单内，必填字段（`based_source`、`model`、`system_prompt`）必须存在；
  2. 对所有属性在 `b.EvalContext()` 下调用 `attr.Expr.Value(ctx)` 做**静态类型检查**：
     - 标量字符串字段（`based_source`、`model`、`profile`、`reasoning_effort`、`system_prompt`、`prompt`、`terminate_tool_id`、`permission`、`timeout`、`human_approval`）：去掉 mark 后若非 null，类型**必须是 `cty.String`**（允许 `!IsWhollyKnown()` 的 `based_source`、`prompt`、`system_prompt`，但 **`model`、`timeout`、`human_approval`、`permission`、`checkpoint_every_rounds`、`sandbox`、`harness`、`model_provider` 必须在 Plan 期完全已知**！）；
  3. 若全部字段已知，直接解码生成 `planned Config`；若包含允许延迟的未知字段（如 `based_source`、`prompt`、`import_artifact.sources`、`tool_use.input`），将块完整序列化为规范化 HCL 对象表达式字符串保存到 `DeferredExpression`，供 Apply 期 `RunActor` 求值！

---

## 4. 各字段与嵌套块校验细则

1. **核心属性**：
   - `based_source`（必填）：类型必须为 `string`（已知非空合法分支名，或来自上游 `source`/`worker`/`merge` 的未知 `string`）；同时推导并记录其所属 `source` 仓库地址（若遍历根为 `source.<s_name>` 则取其 URL；若为上游 `worker.<w_name>` 则继承上游推导出的仓库 URL；若来自 `var` 且 Root 仅有 1 个 `source` 则绑定该唯一 `source`）。
   - `sandbox`（必填，或由 Root 唯一 `sandbox` 自动注入）：必须引用合法的 `sandbox.<name>` 对象（显式写 `sandbox = null` 报错）。
   - `harness`（可选，或由 Root 唯一 `harness` 自动注入）：引用合法的 `harness.<name>` 对象；省略或显式写 `harness = null` 时回退内置 `"copilot"` Harness。
   - `model_provider`（可选，或由 Root 唯一 `model_provider` 自动注入）：引用合法的 `model_provider.<name>` 对象；省略或显式写 `model_provider = null` 时不绑定外部 `model_provider`（交由所选 `harness` 使用原生默认认证/端点）。
   - `writable_paths`（可选 `list(string)`）：每项必须为非空相对 glob 路径（禁止 `..` 逃逸）。
   - `human_approval`（可选 `string`，默认 `"optional"`）：仅允许 `"optional"` | `"required"` | `"disabled"`。
   - `checkpoint_every_rounds`（可选 `int`，默认 `5`）：必须 `> 0`。
   - `tool_ids` / `mcp_tool_ids` / `mcp_resource_ids` / `terminate_tool_id` / `tool_use`：校验引用的 ID 格式合法且已在当前模块注册；`tool_use` 与 `tool_ids`/`terminate_tool_id` 互斥，至多 1 个 `tool_use` 可设 `terminate = true`。
2. **`gatekeeper "<name>"`（0..N 个，名称唯一）**：
   - `command`（必填非空已知 `list(string)`，首元素非空）；
   - `working_dir`（可选相对路径，默认 `"."`，禁止 `..` 逃逸）；
   - `timeout`（可选正 Go duration 字符串，默认 `"10m"`）；
   - `expect_exit_code`（可选 `int`，默认 `0`）；
   - `require_clean_worktree`（可选 `bool`，默认 `true`）。
3. **`qc`（0..1 个）**：
   - `criteria`（必填非空 `map(string)`，键值均非空白）；
   - 可选覆盖 `harness`、`model_provider`、`model`、`reasoning_effort`、`max_qc_rounds`（默认 `3`，`> 0`）、`tool_ids`、`skill_directories`、`skills`、`disabled_skills`、`retry`（未指定时默认继承所属 `worker` 的 `harness`、`model_provider`、`model`、`sandbox`；同样支持在 `qc` 内显式设置不同的 `harness` 或 `model_provider = null` 实现跨 Harness / 跨模型异构互审）。
4. **`fallback "<name>"`（0..N 个，名称唯一）—— ⚠️ 重点校验规则**：
   - `on`（必填非空 `list(string)`）：元素只能取自 `["qc_exhausted", "gatekeeper_exhausted", "human_returned_upstream", "agent_escalated"]`，且同一 `worker` 的多个 `fallback` 块之间同一个触发枚举不得重复绑定到不同目标！
   - `target`（必填遍历引用，如 `target = worker.tasks`）：
     - 在语法上必须直接写块遍历（禁止写纯字符串 `"worker.tasks"`）；
     - **正向祖先校验**：在 `ExecuteDuringPlan()` 中，检查**剔除 `fallback` 块内部引用后**本 `worker` 的所有正向直接/传递上游集合（通过 `based_source`、`import_artifact`、`prompt` 引用、`depends_on` 建立的祖先），`target` **必须已经在该正向祖先集合中**！
       - 若 `target` 指向本节点自身 → 报错；
       - 若 `target` 指向下游节点 → `golden` 环检测自动报错；
       - 若 `target` 指向一条毫无关系的旁路 `worker` → 被本条“正向祖先校验”精准拦截报错！
   - `max_reworks`（可选 `int`，默认 `2`，必须 `> 0`）；
   - `artifacts`（可选引用本块声明的 `artifact("...")` 列表）。

### 4.1 `WorkerBlock.Values()` 导出表
- `work_branch`：`cty.UnknownVal(cty.String)`
- `commit_sha`：`cty.UnknownVal(cty.String)`
- `artifact`：由声明的 `artifact "<name>"` 组成的 `map(object)`（其中 `.path`、`.type`、`.description` 已知，`.id` 为 `cty.UnknownVal(cty.String)`）
- `result`：**仅当配置了 `terminate_tool_id` 或存在 `tool_use { terminate = true }` 时才暴露** `cty.UnknownVal(cty.String)`！未配置终止工具的 `worker` 不暴露 `.result` 属性，这样下游误引用 `worker.specify.result` 时能在 Plan 期立刻报 `This object does not have an attribute named "result"`（与 `r42` 完全一致）！

---

## 5. 必测 TDD Red → Green 用例清单

1. **串联链路与延迟求值**：跑通 README 四工序（`specify -> plan -> tasks -> implement`），验证 `based_source = worker.specify.work_branch`、`import_artifact`、`artifact("design").path` 全部通过 Plan 且 DAG 依赖边完全正确。
2. **静态 `for_each` 解包**：`worker "os_ matrix" { for_each = toset(["linux", "win"]) ... }` 展开后，下游可通过 `worker.os_matrix["linux"].work_branch` 正常引用。
3. **Root 单例绑定与正交显式置空**：
   - Root 有 1 个 `sandbox` / `harness` / `model_provider` 时省略对应属性自动绑定并建边；0 个或 2 个 `sandbox` 时省略 `sandbox` 报错；2 个 `harness` 或 2 个 `model_provider` 时省略对应属性报歧义错误；0 个 `harness` 回退内置 `"copilot"`；
   - 当 Root 存在 1 个 `model_provider "openai"` 时，某 `worker` 显式声明 `harness = harness.antigravity` 与 `model_provider = null`（或在 `qc` 中异构覆盖 `harness`），不会被注入 `model_provider.openai`。
4. **`.result` 属性门禁**：未配终止工具的 `worker` 被下游引用 `.result` 时 Plan 报错；配了 `terminate_tool_id` 时通过。
5. **`gatekeeper` 与 `fallback` 边界**：`gatekeeper` 空 `command` 或 `working_dir = "../outside"` 报错；`fallback.target` 指向自身、下游或无正向依赖关系的旁路 `worker` 时报错；指向真实前序 `worker.tasks` 时通过。
