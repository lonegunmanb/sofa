# 分片 006：`dynamic_worker` 与纯确定性 `merge` 块

## 1. 目标与边界

完成另外两类核心 DAG 编排块的 Plan 期契约校验与类型推导：
1. `dynamic_worker "<name>"`：支持 `dynamic "worker" { for_each = ...; iterator = ...; content { ... } }` 语法，在 `for_each` 为未知上游映射（如 `jsondecodewithtype(worker.discover.result, ...).checks`）时完成静态模板与类型检查；
2. `merge "<name>"`：遵循已锁定的**“严格源头负责制”**，作为无内置写代码 Agent 的纯确定性串行集成块完成校验。

**前置依赖**：分片 `005`（直接复用 `WorkerBlock` 对 `content { ... }` 模板与 `GatekeeperBlock` 的全部校验器！）。

---

## 2. 包与文件清单

- `internal/worker/spec/dynamic.go`：`DynamicWorkerBlock`、`DynamicWorkerTemplate`
- `internal/merge/spec/schema.go`：`MergeBlock`、`MergeConfig`
- 修改 `internal/planner/config.go`：注册 `DynamicWorkerBlock` 与 `MergeBlock`，并为省略 `sandbox`/`harness`/`model_provider` 的 `dynamic_worker`（`content`）和带 `gatekeeper` 的 `merge`（`sandbox`）接入正交 Root 单例默认检查

---

## 3. `dynamic_worker "<name>"` 详细规范与防坑指南

```hcl
dynamic_worker "verify_notifications" {
  parallelism = 4
  serial      = false

  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(string) })
    ).checks
    iterator = check # 可选，省略时默认迭代变量名为 "worker"

    content {
      based_source  = worker.discover.work_branch
      model         = var.model
      system_prompt = "验证单项邮件条件。"
      prompt        = "验证 ${check.key}：${check.value}"

      gatekeeper "test" {
        command = ["go", "test", "./..."]
      }
    }
  }
}
```

### 3.1 ⚠️ 两大必知实现防坑（写错必挂！）

1. **不要调用 `golden` 自带的 `HclBlock.ExpandDynamicBlocks`！**
   - `golden/hcl_block.go` 里的 `MetaNestedBlockNames` 包含 `"dynamic"`，默认 `Decode` 会自动调用 `ExpandDynamicBlocks`，而 `golden` 原生的 `ExpandDynamicBlocks` 既不支持 `cty.UnknownVal` 的 `for_each`，也**完全忽略了 `iterator = check` 属性**！
   - **正确做法**：`DynamicWorkerBlock` **必须实现 `golden.CustomDecode`**，直接从原始 `block.HclBlock().NestedBlocks()` 中读取 `nested.Type == "dynamic"` 的块自行处理！
2. **`iterator = check` 是裸标识符，严禁调用 `attr.Expr.Value(ctx)`！**
   - 在 HCL 中，`iterator = check` 的右侧 `check` 不是一个已定义的变量，而是一个待声明的迭代变量名！如果你调用 `attr.Expr.Value(ctx)`，HCL 会立刻报错 `There is no variable named "check"`！
   - **正确做法**：
     - 第一步：用 `hcl.AbsTraversalForExpr(iteratorAttr.Expr)` 提取遍历路径，要求 `len(traversal) == 1` 且取得 `traversal.RootName()` 作为 `iteratorName`（校验其为合法标识符，且不能与保留根名 `var`、`local`、`source`、`preserve`、`sandbox`、`harness`、`model_provider`、`mcp_server`、`go_tool`、`external_tool`、`worker`、`dynamic_worker`、`merge`、`module`、`path`、`import_artifact` 冲突！）；
     - 第二步（⚠️ 同样关键！）：`golden.InitConfig` 里的 `dagWalker` 只对已注册块根名建边，不会因为 `check` 报错，但校验 `content` 内部表达式时，你必须先构造包含 `iteratorName` 的子 `hcl.EvalContext`！

### 3.2 `for_each` 与 `content` 的静态类型检查流程

1. **顶层属性校验**：仅允许可选 `parallelism`（正整数）与可选 `serial`（已知 `bool`，默认 `false`）；必须包含**恰好 1 个** `dynamic "worker"` 块（标签必须严格为 `["worker"]`）。
2. **`for_each` 类型推导**：
   - 在外层 `EvalContext` 求值 `forEachAttr.Expr.Value(ctx)`（去掉 mark）：
   - 拒绝 `IsNull()`；
   - 校验 `forEachVal.Type()`（即便 `forEachVal` 本身是 `UnknownVal`！因为 `jsondecodewithtype` 已经赋予了精确 `cty.Type`）：
     - 若为 `MapType(elemType)` → `keyType = cty.String`，`valType = elemType`；
     - 若为 `SetType(cty.String)` → `keyType = cty.String`，`valType = cty.String`；
     - 若为 `ObjectType(attrTypes)` → 若属性类型一致则 `valType` 取该类型，否则取 `cty.DynamicPseudoType`（若 `forEachVal.IsKnown()` 且为空对象 `{}`，合法且代表 0 个实例！）；
     - 其他类型（如 `string`、`number`、`list` 非 set）→ 直接在 Plan 期报错：`dynamic_worker for_each must be a map, object, or set of strings`！
3. **`content { ... }` 模板静态预检**：
   - 构造模拟单实例的子 `EvalContext`：注入 `ctx.Variables[iteratorName] = cty.ObjectVal({"key": cty.UnknownVal(cty.String), "value": cty.UnknownVal(valType)})`；
   - 将 `content` 块包装为虚拟 `WorkerBlock`（应用与 `005` 完全相同的 Root 单例 `sandbox`/`harness`/`model_provider` 正交注入、局部 `artifact(...)` 函数、`import_artifact` 变量、嵌套 `gatekeeper`/`qc`/`fallback` 校验），确保 `content` 内部的任何拼写错误（例如当 `valType = object({ cond = string })` 时误写 `${check.value.typo}`）在 **Plan 期 100% 捕获**！
   - 保存 `ForEachExpr`、`IteratorName` 与 `ContentTemplate`（规范化延迟表达式）。

### 3.3 `DynamicWorkerBlock.Value()` 导出结构
```go
workerItemType := cty.Object(map[string]cty.Type{
    "work_branch": cty.String,
    "commit_sha":  cty.String,
    "artifact":    plannedContentArtifactsType,
    // 若 content 配置了 terminate 工具，则包含 "result": cty.String
})
// 对外暴露：
cty.ObjectVal(map[string]cty.Value{
    "address":       cty.StringVal(b.Address()),
    "kind":          cty.StringVal("dynamic_worker"),
    "workers":       cty.UnknownVal(cty.Map(workerItemType)),
    "work_branches": cty.UnknownVal(cty.List(cty.String)),
})
```

---

## 4. `merge "<name>"` 详细规范（纯确定性 + 严格源头负责制）

```hcl
merge "integrate" {
  based_source   = worker.discover.work_branch
  target_branch  = null # 可选：已知非空合法分支名；省略或 null 时创建本 Run 独占的受管集成 work_branch
  sources        = dynamic_worker.verify_notifications.work_branches
  strategy       = "rebase"
  max_reworks    = 2
  human_approval = "optional"
  sandbox        = sandbox.go_env # 当配置了至少 1 个 gatekeeper 且省略 sandbox 时，走 Root 单例 sandbox 推导！

  gatekeeper "regression" {
    command = ["go", "test", "./...", "-count=1"]
  }
}
```

- **`golden` 接口实现**：`golden.CustomDecode` + `golden.PlanBlock` + `golden.SingleValueBlock`（`Type() == ""`，`BlockType() == "merge"`，`AddressLength() == 2`）。
- **字段与校验约束**：
  - **严禁出现 `conflict_resolution` 或任何模型/提示词属性**（已在架构契约中锁定 `MergeActor` 不跑改代码的 LLM，冲突或重测不过一律升级基线退回源头 `worker`）。
  - `based_source`（必填）：非 null 的 `cty.String`（已知合法分支名或未知上游 `work_branch`）。
  - `target_branch`（可选）：若非 null，必须是 Plan 期**已知**的合法 Git 分支名（跨 Run 共享集成分支场景）；且 `target_branch` 不得直接填写未配 `preserve` 的保护源分支除非用户显式声明。
  - `sources`（必填）：求值类型必须可转换为 `list(string)`（支持静态列表 `[worker.a.work_branch, worker.b.work_branch]`、支持 `dynamic_worker.<x>.work_branches` 未知列表、支持 `concat(...)`；若在 Plan 期已知且非动态来源，空列表报错；动态来源在运行期允许为空列表，代表无需合并直接沿用 `based_source`）。
  - `strategy`（可选 `string`，默认 `"rebase"`）：当前仅允许 `"rebase"`。
  - `max_reworks`（可选 `int`，默认 `2`，必须 `>= 0`）：单个来源分支合并/重测失败允许退回原 `worker` 重修的最大次数。
  - `human_approval`（可选 `string`，默认 `"optional"`）：`"optional"` | `"required"` | `"disabled"`。
  - `gatekeeper "<name>"`（0..N 个）：复用 `005` 的 `GatekeeperBlock` 校验器；**注意**：若配置了至少 1 个 `gatekeeper`（需在容器内跑测试命令），则 `merge` 必须拥有有效 `sandbox`（显式指定或通过 Root 单例 `sandbox` 自动绑定；若未配任何 `gatekeeper`，`sandbox` 可为空）。
- **`Value() cty.Value` 导出结构**：
  `{ address: "merge.<name>", kind: "merge", work_branch: UnknownVal(String), commit_sha: UnknownVal(String) }`。

---

## 5. 必测 TDD Red → Green 用例清单

1. **`dynamic_worker` 正常与异常校验**：
   - 使用 `iterator = check` 与省略 `iterator`（默认 `worker.key` / `worker.value`）两种写法均通过 Plan；
   - `for_each = {}` 空映射在 Plan 期合法通过；`for_each = 123` 或 `for_each = ["a"]`（普通 list 而非 set/map）报错；
   - `for_each` 使用 `jsondecodewithtype(worker.discover.result, object({ checks = map(object({ rule = string })) })).checks` 时，`content` 内写 `${check.value.rule}` 通过，写错 `${check.value.wrong_field}` 在 **Plan 期报错**！
   - `iterator = var`（冲突保留字）报错。
2. **`merge` 与下游衔接**：
   - `sources = dynamic_worker.verify_notifications.work_branches` 静态类型匹配并自动建立 `dynamic_worker -> merge` 的 DAG 边；
   - `preserve "out" { branch = merge.integrate.work_branch }` 正常串联；
   - 配了 `gatekeeper` 但 Root 有 0 个或 2 个 `sandbox` 且 `merge` 未显式传 `sandbox` 时报错；Root 有 1 个 `sandbox` 时自动绑定成功；
   - 在 `merge` 里误写 `model` 或 `conflict_resolution` 块时返回清晰的 `unsupported` 错误。
