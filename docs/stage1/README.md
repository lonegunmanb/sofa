# 阶段一（Stage 1）开发总纲：离线 HCL 配置与 `.sofaplan` 编译器

> **面向后续开发 Agent 的强制阅读说明**：
> 阶段一包含 **7 个严格按序递进的微切片（`001`–`007`）**。在为任一切片执行 `/speckit-specify`、`/speckit-plan`、`/speckit-tasks` 或 `/speckit-implement` 之前，**必须完整阅读本总纲以及对应编号的分片设计文档**，严格按文档给定的包路径、结构体签名、`golden` 接口约定与必测 Red 用例落地，**严禁自行发明替代架构或合并多个分片**。

---

## 1. 阶段一边界（做什么 vs 绝对不做什么）

- **阶段一目标**：纯离线完成 `*.sofa.hcl` 配置加载、全部 HCL 块静态与延迟类型校验、子模块安装与递归规划，最终通过 `sofa init` 和 `sofa plan` 产出可人工审查的 JSON 视图与自包含的 **`.sofaplan` 归档包**。
- **阶段一严禁触碰的范围（Out of Scope）**：
  - 不启动任何容器（无 Podman/Docker 调用）；
  - 不启动或连接 `daprd` / Dapr Actor；
  - 不执行真实的 `git clone` / `git checkout` / `git push`；
  - 不调用 `copilot-sdk/go` 发起任何模型会话；
  - 所有测试均为 **Tier 1 纯本地快速单元测试**（`go test ./... -count=1` 必须在无网络、无守护进程环境下毫秒～秒级全绿）。

---

## 2. 阶段一统一源码目录布局

所有 7 个分片共同演进以下目录结构，**禁止随意改名或另起炉灶创建平级重复包**：

```text
sofa/
├── cmd/
│   └── sofa/
│       └── main.go                 # [007] CLI 入口 (sofa init / sofa plan / sofa schema)
├── internal/
│   ├── spec/                       # [001+] 基础共享原语：Sensitive 标记、Condition、cty 类型校验、BranchRef
│   ├── config/                     # [001+] *.sofa.hcl 扫描加载、golden.BaseConfig 封装、内置 HCL 函数、Root 单例预处理
│   ├── source/                     # [002] source "<name>" 与 preserve "<name>" 块
│   ├── provider/                   # [003] model_provider "<name>" 与 RetryPolicy
│   ├── mcp/                        # [003] mcp_server "<name>" 与确定性工具/资源 ID
│   ├── sandbox/
│   │   └── spec/                   # [003] sandbox "<name>" 块配置模型与校验
│   ├── tool/
│   │   ├── gotool/                 # [004] go_tool 内联 Go 源码 AST 与类型签名分析
│   │   └── spec/                   # [004] go_tool / external_tool 块、Constraint(JSON Schema) 与确定性 ID
│   ├── worker/
│   │   └── spec/                   # [005, 006] worker 与 dynamic_worker 块、嵌套块及延迟表达式求值
│   ├── merge/
│   │   └── spec/                   # [006] 纯确定性 merge "<name>" 块
│   ├── module/
│   │   ├── init.go                 # [007] 本地目录复制与 go-getter 模块安装 (.sofa/modules)
│   │   └── spec/                   # [007] module "<name>" 与 output "<name>" 块（output 在 001 先建立）
│   ├── plan/                       # [007] 不可变 Plan 模型、ctyjson+ctymsgpack 编解码、脱敏展示、.sofaplan ZIP 打包
│   └── planner/                    # [001->007 逐步扩展] SofaConfig（实现 golden.Config），串联所有块的注册与快照导出
```

---

## 3. `github.com/lonegunmanb/golden` 七大防坑铁律（必读！）

以下全部来自 `golden` 源码的硬约束，写错任意一条都会导致隐蔽 Bug 或编译/运行期 Panic：

### 铁律 1：绝对禁止重造或手工拦截 `golden` 已提供的能力（未经用户显式批准严禁违反！）
- **`golden` 原生提供的能力一律直接委托，绝不允许在 `sofa` 中重复实现、包裹冗余校验、或手写代码去禁用/拦截**：
  - 块适配与解码：`golden.AsHclBlocks`、`golden.InitConfig`、`golden.Decode`（普通块直接用 `hcl:"..."` 标签交由 `golden.Decode` 自动完成未知属性与未知嵌套块报错，无需手写属性名白名单）；
  - 内置 `variable`（含 `SOFA_VAR_<name>`、`sofa.sofavars`、`*.auto.sofavars`、CLI `-var`、类型转换、嵌套 `validation`）和 `locals`：**绝不要重复注册或重写求值器**（`sofa` 仅在 `Init` 前检查 `variable` 必填具体 `type` 及记录 `sensitive` 标记）；
  - 元属性与元嵌套块：`for_each`、`depends_on`（`golden.MetaAttributeNames`）以及 `precondition`、`dynamic`（`golden.MetaNestedBlockNames`）均由 `golden` 原生处理——**严禁在任何普通块中手写代码去报错拦截 `for_each`、`depends_on`、`precondition` 或 `dynamic`！** 即便某个块因为字段可能含 `cty.UnknownVal` 而必须实现 `golden.CustomDecode`（如 `preserve`、`worker`、`merge`、`module`），过滤元属性/元嵌套块时也**必须直接复用 `golden.MetaAttributeNames.Contains(...)` 与 `golden.MetaNestedBlockNames.Contains(...)`**，不得手写硬编码列表。
  - **全仓库唯一经用户批准的特例**：仅有分片 `006` 的 `dynamic_worker` 因 `for_each` 遍历上游运行期未知值（`cty.UnknownVal`）且需支持 `iterator` 变量名（对齐 `r42` 的 `dynamic_research`），才在 `CustomDecode` 中专门解析 `dynamic "worker"`。
  - DAG 建图、环检测与拓扑执行：完全交由 `InitConfig` 与 `cfg.RunPlan()` 完成。

### 铁律 2：`go-defaults` 并发初始化竞态防护与测试并行度
- `golden` 依赖的 `github.com/lonegunmanb/go-defaults v1.4.0` 首次调用存在非线程安全懒加载。在全局 `sync.Once` 注册自定义 `golden.RegisterBlock(...)` 前，**第一行必须先调用一次 `defaults.SetDefaults(&struct{}{})`**。
- `golden` 的块注册表与环境变量读取是进程级全局状态：涉及 `SOFA_VAR_*` 环境变量的单测严禁加 `t.Parallel()`（使用 `t.Setenv`）。

### 铁律 3：单标签块（`Type() == ""`）的 `SingleValueBlock` vs `Valuable` 选型规则
- 在 `sofa` 中，所有顶层业务块在 HCL 中都是 **1 个标签**（如 `source "project"`、`worker "specify"`），因此它们的 `Type()` 方法必须返回 `""`，`AddressLength()` 必须返回 `2`（`golden.AsHclBlocks` 会自动为其补齐前置空标签 `Labels = ["", name]`）。
- **按单实例对象暴露的 1 标签块**（`output`、`source`、`preserve`、`sandbox`、`model_provider`、`mcp_server`、`go_tool`、`external_tool`、`dynamic_worker`、`merge`、`module`）：实现 `golden.SingleValueBlock`（`Value() cty.Value`），`golden` 会自动把它们放到 `EvalContext.Variables[blockType][name]`（注意：保持与 `r42` 一致，元语法交由 `golden` 默认处理，**不要手写拦截 `for_each` 的代码**）。
- **需要按多实例映射暴露 `for_each` 的 1 标签块**（`worker "<name>"`）：**不能**实现 `SingleValueBlock`，必须实现 `golden.Valuable`（`Values() map[string]cty.Value`）。此时 `golden` 内部会多包一层空子类型键 `""`（变成 `Variables["worker"].GetAttr("")`）！因此**必须在 `SofaConfig.EvalContext()` 中解包**：
  ```go
  if v, ok := ctx.Variables["worker"]; ok && !v.IsNull() && v.Type().IsObjectType() && v.Type().HasAttribute("") {
      ctx.Variables["worker"] = v.GetAttr("")
  }
  ```
  这样 `worker.specify.work_branch` 和 `worker.check["mail"].work_branch` 才能同时正常工作！

### 铁律 4：敏感值标记必须使用 `golden.SensitiveMark`
- 标记敏感 `cty.Value` 时，统一使用 `value.Mark(golden.SensitiveMark)`（即字符串常量 `"sensitive"`，不要自定义私有类型 `type mark string`），这样 `golden` 自带的 `CtyValueToString` 和诊断脱敏才能自动识别并输出 `<sensitive>`。

### 铁律 5：引用上游 Apply 期未知值（`cty.UnknownVal`）的字段严禁直接解码到 Go `string`
- 在 Plan 期，`worker.specify.work_branch`、`worker.discover.result`、`worker.plan.artifact.design.id` 都是 **Apply 期才确定的未知值（`cty.UnknownVal`）**。
- 如果下游块（如 `preserve.branch`、`worker.based_source`、`worker.prompt`、`merge.sources`）直接用 `gohcl.DecodeBody` 解码到 Go `string` 字段，HCL 会立刻报错 `Unsuitable value type: value must be known`！
- **正确做法**：此类字段要么在结构体上声明为 `cty.Value` 手动校验类型（允许 `!IsWhollyKnown()` 但类型必须是 `cty.String` / 期望对象类型，同时保存 `attr.ExprString()` 原始表达式字符串进 Plan），要么像 `005` 的 `WorkerBlock` 一样实现 `golden.CustomDecode` 构造延迟表达式。

### 铁律 6：Root 单例默认（0/1/≥2）通过 AST 预注入实现 `golden` 自动建边
- `golden.BaseConfig` 内部的 `*Dag` 与 `addEdge` 是未导出私有成员，外部包无法在 `InitConfig` 后手工塞边。
- 因此 Root Module 单例默认（`sandbox`、`model_provider` 以及唯一 `source` 下的 `preserve.url`）统一在 **`LoadDirectory` 之后、`golden.InitConfig` 之前** 对未显式填写该属性的块注入合成遍历属性（如注入 `sandbox = sandbox.<only_name>` 的 `hclsyntax` + `hclwrite` 属性）：
  - `0` 个时：若必填（如 `sandbox`、`preserve.url` 无 `source`）直接返回清晰的阶段错误；
  - `1` 个时：注入引用表达式，`golden` 的 `dagWalker` 会**自动建立隐式依赖边**并完成求值；
  - `≥ 2` 个时：直接返回歧义错误，要求用户显式书写。

### 铁律 7：每个分片的完成门禁（含强制 Commit & Push！）
每个分片在按 Spec Kit（`specify` → `plan` → `tasks` → `implement`）交付时，必须严格按序满足以下全部条件后，**才允许声明本分片完成或进入下一分片**：
1. 先写失败单测（Red），验证预期失败信息后再写最小实现（Green）；
2. 通过 `gofmt` 检查、`go vet ./...` 与 `go test ./... -count=1`；
3. 不破坏前序分片的任何已有测试；
4. **强制提交并推送（Commit & Push）**：将当前分片产生的全部规范与代码文件（`specs/<###-...>/`、`internal/`、`cmd/`、`go.mod`、`go.sum` 等，注意排除未跟踪的编辑器私有目录）执行 `git add` → `git commit` → `git push`，并确认 `git status` 工作区干净后，才能继续开启下一个分片。严禁把多个分片的改动堆积在未提交工作区中！

---

## 4. 阶段一分片索引

| 编号 | 设计文档 | 核心交付物 | 前置依赖 |
| :--- | :--- | :--- | :--- |
| **001** | [001-golden-config-and-output.md](./001-golden-config-and-output.md) | `internal/spec`、`internal/config`、`OutputBlock`、`planner.SofaConfig` 最小闭环 | 无 |
| **002** | [002-source-and-preserve.md](./002-source-and-preserve.md) | `source "<name>"`、`preserve "<name>"`、`BranchRef` 约定、唯一 `source` 自动补全 | 001 |
| **003** | [003-providers-sandbox-and-defaults.md](./003-providers-sandbox-and-defaults.md) | `model_provider`、`mcp_server`、`sandbox` 及 Root 单例（0/1/≥2）自动默认注入 | 001 |
| **004** | [004-typed-tools.md](./004-typed-tools.md) | `go_tool`、`external_tool`、类型约束与 JSON Schema 转换、`postcondition`、确定性 `.id` | 001 |
| **005** | [005-static-worker.md](./005-static-worker.md) | `worker "<name>"`（含 `for_each`、`artifact`、`import_artifact`、`tool_use`、`gatekeeper`、`qc`、`fallback` 及延迟表达式） | 002, 003, 004 |
| **006** | [006-dynamic-worker-and-merge.md](./006-dynamic-worker-and-merge.md) | `dynamic_worker "<name>"`（含 `iterator`）与纯确定性 `merge "<name>"` | 005 |
| **007** | [007-modules-sofaplan-and-cli.md](./007-modules-sofaplan-and-cli.md) | `sofa init`、`module` 递归规划（含 Root 单例继承）、`.sofaplan` ZIP 归档编解码、`sofa plan` / `sofa schema` CLI | 006 |
