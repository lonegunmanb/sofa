# 分片 004：强类型工具块（`go_tool`、`external_tool`）

## 1. 目标与边界

1:1 对齐 `r42` 的强类型自定义工具契约，实现：
1. `go_tool "<name>"`：内联 Go 代码片段静态语法与类型检查（验证 `type Input`、`type Output`、`func Invoke(context.Context, Input) (ToolResponse[Output], error)` 签名，将其映射为 `cty.Type` 与 JSON Schema，不在 Plan 期执行外部编译或联网 `go get`）；
2. `external_tool "<name>"`：外部进程 JSON 协议工具（通过 `hashicorp/hcl/v2/ext/typeexpr` 解析 `input_type` 与 `output_type` 类型约束及 `optional(...)` 默认值，生成 JSON Schema）；
3. 支持 `postcondition` 嵌套块（在 Plan 期用未知类型的 `input` / `output` 占位符做静态表达式类型预检，确保 `condition` 只引用 `input` 和 `output` 且返回 `bool`）；
4. 生成跨模块唯一的确定性工具 `.id`（长度 `<= 64`，符合大模型工具名字符集限制），并注册内置 HCL 函数 `tool_name(tool_ref)`。

**前置依赖**：分片 `001`（按序基于 `003` 递进）。

---

## 2. 包与文件清单

- `internal/spec/condition.go`：`Condition{ Expression, ErrorMessage }` 与静态/运行期 `Evaluate`
- `internal/spec/response.go`：`Issue` 与泛型 `ToolResponse[T]` 定义
- `internal/tool/gotool/analyze.go`：使用标准库 `go/parser` + `go/ast` 分析内联 Go 片段的 `Input` / `Output` 类型结构与 `Invoke` 函数签名，映射为 `cty.Type`
- `internal/tool/spec/constraint.go`：`Constraint`（包装 `cty.Type` + `*typeexpr.Defaults`）及 `JSONSchema()` 转换
- `internal/tool/spec/identity.go`：确定性工具 ID 算法（`tool_go_tool_<name>_<uuid>` / `tool_external_tool_<name>_<uuid>`，总长封顶 64 字符）
- `internal/tool/spec/schema.go`：`GoToolBlock` 与 `ExternalToolBlock`
- 修改 `internal/config/functions.go`：补充 `tool_name(address_obj)` 函数
- 修改 `internal/planner/config.go`：注册 `GoToolBlock` 与 `ExternalToolBlock`

---

## 3. 详细技术规范

### 3.1 确定性工具 ID（`internal/tool/spec/identity.go`）
- 输入：`canonicalAddress`（根模块为 `go_tool.uuid`，子模块为 `module.sub.go_tool.uuid`）和 `blockType`（`"go_tool"` 或 `"external_tool"`）。
- 算法：
  - `digest = sha256("sofa/tool-id/v1\x00" + blockType + "\x00" + canonicalAddress)`
  - 按 UUIDv8/v4 掩码格式化前 16 字节为 36 字符 UUID；
  - 将 `block.Name()` 中非 `[a-zA-Z0-9_-]` 字符替换为 `_` 并截断，使最终 `"tool_" + blockType + "_" + safeName + "_" + uuid` 总长度 **`<= 64`**！
- **重要覆写**：`GoToolBlock` 与 `ExternalToolBlock` 必须同时覆写 `Id() string` 和 `BaseValues() map[string]cty.Value`（令 `BaseValues()["id"] = cty.StringVal(b.Id())`），否则 `golden.BaseBlock` 默认生成的随机 UUID 会覆盖确定性 `.id`！

### 3.2 `go_tool "<name>"` 静态分析规则（`internal/tool/gotool/analyze.go`）
```hcl
go_tool "submit_checks" {
  description  = "校验并提交受影响的邮件类型映射"
  dependencies = ["github.com/google/uuid@v1.6.0"]
  source       = <<-GO
    import "context"

    type Input struct {
      Checks map[string]string `json:"checks"`
    }
    type Output string

    func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO

  postcondition {
    condition     = length(input.checks) >= 0
    error_message = "checks must be valid"
  }
}
```
- **Plan 期离线分析要求**（严禁在 Plan 期跑 `go get`！否则无网环境无法 `sofa plan`）：
  1. 在 `source` 前拼上虚拟 `package gotoolwrap` 后用 `go/parser.ParseFile` 解析语法；语法错误直接返回清晰行号诊断。
  2. 校验 `dependencies` 中的每一项均为非空合法的 `module@version` 字符串格式（禁止空白、禁止本地相对路径替换或命令行注入前缀 `-`）。
  3. 在 AST 中查找顶层类型声明 `Input` 与 `Output`：
     - `Input` 必须是 `struct`（模型工具调用参数顶层必须是 JSON Object！结构体字段若有 `json:"foo"` 标签则以标签名为准，`json:"-"` 忽略，无标签则要求导出字段名匹配）；支持嵌套 `struct`、基本类型（`string`、`bool`、整数/浮点）、切片 `[]T`、字典 `map[string]T` 以及指针 `*T`（指针或 `omitempty` 视为可选属性）。
     - `Output` 允许是上述任意可映射为合法 `cty.Type` 的类型（当该工具在 `005` 中用作 `terminate_tool_id` 时，额外要求 `Output` 可转换为 `string` 或结构体 JSON）。
  4. 查找顶层函数 `Invoke`：必须恰好接收 `(context.Context, Input)` 两个参数，返回 `(ToolResponse[Output], error)` 两个返回值。

### 3.3 `external_tool "<name>"` 校验规则
- 必填属性：`description`（非空已知 `string`）、`program`（非空已知 `list(string)`，且 `program[0]` 非空白串）、`input_type`（类型表达式，顶层必须是 `object({...})`！因为传给 stdin 的是一份 JSON 参数对象）、`output_type`（任意通过 `spec.ValidateType` 的类型表达式）。
- 可选属性：`working_dir`（已知 `string`）。
- Plan 期**不检查** `program[0]` 可执行文件在本机是否存在（因为离线做 Plan 的电脑未必装了容器镜像里才有的 Python/工具链！）。

### 3.4 `postcondition` 嵌套块（Plan 期静态预检）
- 仅允许 `condition` 与 `error_message`（已知非空 `string`）两个属性。
- 在 `Decode` 阶段构造 `variables = {"input": cty.UnknownVal(inputType), "output": cty.UnknownVal(outputType)}`，调用 `Condition.Evaluate(variables, ctx.Functions)`：
  - 若引用了 `input` / `output` 以外的变量根（如 `var.x`）→ 立即报错；
  - 若访问了 `Input` / `Output` 上不存在的字段名（如 `input.non_existent`）→ `hcl` 求值器会在未知对象类型上直接捕获拼写错误并在 Plan 期拦截！
  - 若返回值类型不是 `cty.Bool` → 立即报错。

---

## 4. 必测 TDD Red → Green 用例清单

1. **确定性 ID 与 `tool_name()`**：同一配置两次 Plan 产出的 `go_tool.x.id` 与 `external_tool.y.id` 完全一致，超长块名截断后长度 `<= 64`；`tool_name(go_tool.x)` 返回该确定性 ID。
2. **`go_tool` AST 校验**：缺少 `Input` / `Output` / `Invoke`、签名不匹配、`Input` 不是 `struct`、使用了不支持的 `chan`/`func` 字段类型、`dependencies` 含非法项如 `"-modfile=x"` 均在 Plan 期报错；合法嵌套 `struct` + 指针可选字段生成正确的 JSON Schema。
3. **`external_tool` 类型约束**：`input_type = string`（非 `object`）报错；`input_type = object({ limit = optional(number, 10), tags = list(string) })` 正确生成含 `required` 与属性类型的 JSON Schema，且无需本机安装 `program` 即可通过 Plan。
4. **`postcondition` 静态类型检查**：`postcondition` 中写错字段名 `input.typo == ""` 或返回非布尔值 `input.query` 在 Plan 期直接失败。
