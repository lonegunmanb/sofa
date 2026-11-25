# 分片 001：`golden` 底座接入、目录加载、内置函数与 `output` 块

## 1. 目标与边界

建立 `sofa` 在 `github.com/lonegunmanb/golden` 之上的最薄配置规划底座，打通：
1. 扫描配置目录中的 `*.sofa.hcl`（忽略非 `.sofa.hcl` 文件与子目录），按文件名字典序稳定加载并交给 `golden.AsHclBlocks`；
2. 封装 `planner.SofaConfig`（嵌入 `*golden.BaseConfig`，设置 `DslFullName = "sofa"`、`DslAbbreviation = "sofa"`），直接复用 `golden` 原生 `variable`（含 `type` 必填检查、`sensitive` 透传、`SOFA_VAR_<name>`、`sofa.sofavars`、`*.auto.sofavars`、CLI 变量覆盖及嵌套 `validation`）和 `locals`；
3. 提供基础 HCL 函数表（`cwd()`、`one(collection)`、`local_timestamp()`、`jsondecodewithtype(json, type)`）及内置上下文对象 `path.module`（统一转为 `/` 分隔符）；
4. 注册第一个自定义块 `output "<name>"`（支持 `value`、`description`、`sensitive`）。

**本分片不做**：任何其他自定义块（`source`、`sandbox`、`worker` 等留给 `002`–`006`）、模块嵌套、CLI 二进制。

---

## 2. 新增依赖与包文件清单

在 `go.mod` 中引入：
- `github.com/lonegunmanb/golden`
- `github.com/lonegunmanb/hclfuncs`
- `github.com/lonegunmanb/go-defaults`
- `github.com/hashicorp/hcl/v2`
- `github.com/zclconf/go-cty`
- `github.com/stretchr/testify`

涉及包与文件：
- `internal/spec/cty.go`：敏感标记辅助函数与 `ValidateType`
- `internal/config/loader.go`：`LoadDirectoryContext(ctx, dir)` 扫描解析 `*.sofa.hcl`
- `internal/config/functions.go`：`Functions()` 内置函数实现
- `internal/config/base.go`：`NewBaseConfig(args)` 构造 `*golden.BaseConfig`
- `internal/module/spec/output.go`：`OutputBlock` 与 `Output` 快照结构
- `internal/planner/config.go`：`SofaConfig`、块注册 `sync.Once`、变量前置校验（强制 `variable` 声明 `type`）与 `RunPlan()` 封装

---

## 3. 核心数据结构与接口设计

### 3.1 `internal/spec/cty.go`
- `MarkSensitive(v cty.Value) cty.Value`：调用 `v.Mark(golden.SensitiveMark)`（注意：必须使用 `golden.SensitiveMark` 字符串 `"sensitive"`，不要另建私有类型）。
- `IsSensitive(v cty.Value) bool`：递归检查 `v` 或其内部路径是否带有 `golden.SensitiveMark`（利用 `v.HasMarkDeep(golden.SensitiveMark)`）。
- `ValidateType(t cty.Type) error`：拒绝 `cty.NilType`、`cty.DynamicPseudoType`（`any`）和 `CapsuleType`，递归放行基本类型与 `list`/`set`/`map`/`tuple`/`object`。

### 3.2 `internal/config/loader.go`
```go
type SourceFile struct {
    Path   string
    Source []byte
}

type Loaded struct {
    Files  []SourceFile
    Blocks []*golden.HclBlock
}

func LoadDirectoryContext(ctx context.Context, directory string) (Loaded, hcl.Diagnostics, error)
```
- 规则：
  - 将 `directory` 解析为绝对路径（非目录或不存在返回 error）；
  - 读取一层目录项，按文件名升序排列，**仅读取以 `.sofa.hcl` 结尾的普通文件**（目录内若没有任何 `.sofa.hcl` 文件，返回明确错误 `no .sofa.hcl files found in ...`）；
  - 对每个文件同时调用 `hclsyntax.ParseConfig` 与 `hclwrite.ParseConfig`，收集全部 `hcl.Diagnostics`；无语法错误时调用 `golden.AsHclBlocks(syntaxBody.Blocks, writeFile.Body().Blocks())` 追加至 `Loaded.Blocks`。
  - **额外顶层属性拦截**：若用户在文件顶层写了裸属性（如 `foo = "bar"` 而非块），`syntaxBody.Attributes` 非空，必须生成明确的 `hcl.DiagError`（顶层仅允许声明块，不允许顶层裸属性）。

### 3.3 `internal/config/functions.go`
在 `golden` 默认挂载的 `hclfuncs.Functions(basedir)` 之上，通过 `BaseConfig.OverrideFunctions` 注入：
1. `cwd()`：返回当前进程工作目录（`filepath.ToSlash`）。
2. `one(collection)`：集合长度为 0 返回同元素类型的 `cty.NullVal`，长度为 1 返回唯一元素，长度 > 1 或为 `null` 或非集合类型报错（对齐 Terraform / `r42`）。
3. `local_timestamp()`：返回 `RFC3339Nano` 当前时间字符串。
4. `jsondecodewithtype(json_str, type_expr)`：使用 `hashicorp/hcl/v2/ext/typeexpr.TypeConstraintType` 作为第 2 参数类型！**关键细节**：当第 1 参数 `json_str` 在 Plan 期是 `cty.UnknownVal(cty.String)`（例如未来的 `worker.discover.result`）且第 2 参数 `type_expr` 已知时，`cty` 函数框架在 `AllowUnknown: false`（默认）下会自动返回 `cty.UnknownVal(resolvedReturnType)`！这正是 `006` 中 `dynamic_worker` 在 Plan 期获得静态类型的核心原理，务必保持第 1 参数 `AllowUnknown: false`、第 2 参数解析出精确 `cty.Type`。

### 3.4 `internal/module/spec/output.go`（`OutputBlock`）
> **⚠️ 上游演进跟进说明**：已提交 [golden#18](https://github.com/lonegunmanb/golden/issues/18) 推进 `golden` 原生内置 `OutputBlock`。当前分片 `001` 暂用本节过渡实现；一旦 `golden#18` 解决，须立即按 [adopt-golden-output.md](./adopt-golden-output.md) 升级 `golden` 并删除此处手造的 `OutputBlock`。

```go
type OutputBlock struct {
    *golden.BaseBlock
    Expression  cty.Value `hcl:"value"`
    Description *string   `hcl:"description,optional"`
    Sensitive   bool      `hcl:"sensitive,optional"`
    planned     cty.Value
}
```
- 实现 `golden.PlanBlock` 与 `golden.SingleValueBlock`：
  - `Type() string { return "" }`
  - `BlockType() string { return "output" }`
  - `AddressLength() int { return 2 }`
  - `CanExecutePrePlan() bool { return false }`
- 校验规则：
  - 禁止包含除 `precondition` / `dynamic` 之外的未知嵌套块；禁止未知属性；
  - `description` 若存在必须是已知非空类型 `string`；`sensitive` 若存在必须是已知 `bool`；
  - `Expression` 允许是 `cty.UnknownVal`（因为后续 `output` 常引用 `worker.implement.work_branch`）；若 `Sensitive == true`，对 `planned` 调用 `spec.MarkSensitive`。

### 3.5 `internal/planner/config.go`（`SofaConfig`）
- 嵌入 `*golden.BaseConfig`。
- **变量前置扫描（对齐 `r42` 规则）**：在调用 `golden.InitConfig` 之前扫描 `loaded.Blocks` 中的 `variable` 块：
  1. 每个 `variable` 块**必须显式声明 `type` 属性**，且通过 `typeexpr.TypeConstraintWithDefaults` + `spec.ValidateType`（禁止无类型或 `any`）；
  2. 解析可选布尔属性 `sensitive = true`，记录到 `SofaConfig.sensitiveVariables` 集合中；在 `SofaConfig.EvalContext()` 中将对应的 `ctx.Variables["var"].AsValueMap()[name]` 打上 `spec.MarkSensitive`，使敏感变量传递到 `locals` 和 `output` 时自动携带脱敏标记！
  3. 校验外部传入的命名变量不存在未声明的孤儿变量名。
- `EvalContext()` 注入 `path = cty.ObjectVal({"module": cty.StringVal(filepath.ToSlash(c.directory))})`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **目录加载边界**：按字典序合并多个 `*.sofa.hcl`；忽略 `.txt`/`.hcl`（非 `.sofa.hcl`）文件；空目录报错；文件顶层写裸属性 `x = 1` 报错；未注册块类型报错。
2. **`variable` 全优先级与类型硬约束**：
   - 未声明 `type` 或 `type = any` 在 Plan 期报错；
   - 优先级递增验证：`default` < `SOFA_VAR_<name>` < `sofa.sofavars` < `sofa.sofavars.json` < `a.auto.sofavars` < `z.auto.sofavars` < CLI `-var` / `-var-file`；
   - `validation { condition = ...; error_message = ... }` 不满足时报错；
   - `sensitive = true` 的变量经 `locals` 流转到 `output` 后，`spec.IsSensitive(out.Value)` 为 `true`。
3. **内置函数与 `path.module`**：
   - `path.module` 在 Windows 下也返回 `/` 分隔路径；
   - `one([])` 返回 null、`one(["x"])` 返回 `"x"`、`one(["x", "y"])` 报错；
   - `jsondecodewithtype("{\"checks\":{\"a\":\"b\"}}", object({checks = map(string)}))` 返回精确对象类型；当第 1 参数为 `cty.UnknownVal(cty.String)` 时返回未知但类型精确为 `object({checks = map(string)})` 的值！
4. **DAG 依赖与环检测（验证 `golden` 原生工作）**：
   - `locals` 之间循环引用（`local.a -> local.b -> local.a`）或 `depends_on` 环路在 `InitConfig` / `RunPlan` 阶段报错。
