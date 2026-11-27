# 分片 027：Apply 期延迟表达式求值、`dynamic_worker` 展开与子 `module` 校验纯核心（`internal/applyeval`）

## 1. 目标与边界

遵循架构契约与反巨石原则，将 `RunActor` 所需的 **HCL / `cty` / `golden` 函数表求值逻辑** 独立封装为无网络、无 Dapr 依赖的纯计算包 `internal/applyeval`，使复杂的延迟求值与动态扇出 100% 具备毫秒级确定性单测能力：
1. **复用 `golden` 标准函数表，严禁重造表达式求值轮子**：
   - 将已完成上游节点（`source`、`worker`、`dynamic_worker`、`merge`、`import_artifact`、`module`）的真实运行期公开输出对象注入 `hcl.EvalContext`（保留 `golden.SensitiveMark` 敏感路径标记！）；
   - 按拓扑顺序求值延迟 `locals`（`Plan.LocalExpressions()`）与顶层/子模块 `outputs`。
2. **单节点启动参数具体化（Materialize Node Config）**：
   - 对就绪的 `worker` / `merge` 节点，将 Plan 期含 `cty.UnknownVal` 的字段（如依赖上游 `worker.x.branch` 的 `based_source`、引用 `${worker.x.result.summary}` 的 `prompt`、`import_artifact` 来源列表）求值为完全已知（Wholly Known）的具体执行结构。
3. **`dynamic_worker` 展开键计算与子任务投影**：
   - 求值 `dynamic_worker` 的 `for_each` 集合（`map` 或 `set(string)`），生成稳定排序的展开键列表 `[]string`，并为每个 `key` 注入 `each.key` / `each.value`（或自定义 `iterator` 名称）投影出独立子 `worker` 配置；全部子任务完成后聚合构造 `dynamic_worker.<name>[key]` 映射对象供下游 `merge` 或 `worker` 消费。
4. **嵌套 `module` 延迟输入求值与 `variable.validation` 执行**：
   - 当子 `module` 前置依赖全部完成时，求值其 `InputExprs`，随后逐条执行 `ModuleSpec.DeferredValidations`（Plan 期因含 `Unknown` 被推迟的 `variable` `validation` 块），不满足条件立即返回带原始 `ErrorMessage` 的校验失败！

**前置依赖**：阶段一（`internal/plan`、`internal/spec`）与阶段二领域类型。

---

## 2. 涉及包与文件清单

- `internal/applyeval/context.go`：构造含上游运行期值、`var`、`local`、`path` 与 `golden`/内置函数的 `*hcl.EvalContext`
- `internal/applyeval/eval.go`：`EvalReadyWorker`、`EvalDynamicWorkerKeys`、`ProjectDynamicInstance`、`EvalModuleInputsAndValidate`、`EvalPlanOutputs`
- `internal/applyeval/eval_test.go`：Tier 1 全场景纯内存求值单测（含敏感标记传递、`dynamic_worker` 空集合/多元素展开、延迟 `validation` 拦截）

---

## 3. 保姆级实现细则

1. **运行期节点公开属性对齐 `docs/hcl-contract.md`**：
   - `worker.<name>`：`{ branch = string, commit_sha = string, result = dynamic/object, artifacts = map(object) }`；
   - `dynamic_worker.<name>`：以展开 `key` 为键的 `map/object`，每个元素结构与单个 `worker` 一致；
   - `merge.<name>`：`{ branch = string, commit_sha = string }`；
   - `module.<name>`：子模块求值完毕后的公开 `outputs` 对象（保留 `sensitive` 标记）。
2. **`dynamic_worker` 确定性与边界处理**：
   - 若求值出的 `for_each` 为空集合（`0` 个元素），返回空切片 `[]string{}`，该 `dynamic_worker` 直接标记为 `SUCCEEDED`（公开值为空对象 `{}`），下游节点正常放行！
   - 若求值结果包含 `null` 元素或非字符串键，返回明确类型错误。
3. **源码零再解析安全求值**：
   - 使用 `hclsyntax.ParseExpression` 求值 `Plan` 中冻结保存的表达式文本（配合 `Plan.Context` 中的已知 `var` 值），绝不在 Apply 期重新读取宿主机上的原始 `.sofa.hcl` 文件！

---

## 4. 必测 TDD Red → Green 用例清单

1. **上游值串联求值**：由真实 `planner` 编译出 `worker.design -> dynamic_worker.coding -> merge.int -> output.final_branch` 的 `.sofaplan`，逐步喂入模拟完成结果，验证下游 `based_source`、`prompt`、`each.key`/`each.value`、`merge` 候选分支列表与最终 `output`（含 `sensitive = true` 掩码标记）全部精确算出；
2. **`dynamic_worker` 空集**：上游返回空列表时展开 0 个实例且不报错；
3. **子 `module` 延迟 `validation`**：喂入违反子模块 `variable` `validation` 的上游 `result` 字段，验证精确拦截并返回声明的 `error_message`。
