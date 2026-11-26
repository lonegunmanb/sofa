# 分片 004b：`harness` 插件声明块（对齐 `tflint`）与 `sandbox` 默认 `bridge` 网络调整

## 1. 目标与边界

为支持多语言 Agent Harness（如内置 Go `copilot`、Python `google-antigravity`、未来其他 SDK）通过 `hashicorp/go-plugin`（gRPC）统一接入，并与 `model_provider`、`sandbox` 保持**完全正交**，本分片在进入 `005`（`worker`）前完成三项离线配置层准备：

1. **调整 `sandbox.network` 默认值**：从 `"none"` 调整为 **`"bridge"`**（仍支持显式配置 `"bridge"` | `"none"` | `"host"`），以便容器内运行的 Agent Harness 插件默认可访问外部模型服务；
2. **新增顶层 `harness "<name>"` 块**（声明方式对齐 `github.com/terraform-linters/tflint` 的 `plugin` 块）：支持内置（bundled `"copilot"`）、GitHub Releases（`source` + `version` + `signature` + `signing_key`）与本地手动安装三种模式，**严禁内嵌 `model_provider`** 以保持正交；
3. **扩展 `RootDefaults`**：新增 `Harnesses []string` 与 `ResolveDefaultHarness(consumerAddr string) (*string, error)`（`0` 个回退内置 `"copilot"`，`1` 个作为全局默认，`>=2` 个报歧义错误）。

**前置依赖**：分片 `003`、`004`（与 `004` 无代码耦合，可在 `004` 前或后紧接执行，必须在 `005` 之前完成）。
**本分片不做**：真实网络请求 GitHub Releases API（留给 `007` 的 `sofa init`）或启动 `go-plugin` 进程（留给 Stage 2 运行时）。

---

## 2. 包与文件清单

- 修改 `internal/sandbox/spec/schema.go` 及对应测试 `schema_test.go` / `internal/planner/sandbox_test.go`：将未显式指定 `network` 时的默认值由 `"none"` 改为 `"bridge"`
- 新增 `internal/harness/spec/schema.go`（及 `schema_test.go`）：`HarnessBlock`、`HarnessConfig`、`SignatureMode`、校验逻辑与 `Value()`
- 修改 `internal/planner/defaults.go`（及 `defaults_test.go`）：在 `RootDefaults` 中补充 `Harnesses` 扫描与 `ResolveDefaultHarness`
- 修改 `internal/planner/config.go`（及新增 `internal/planner/harness_test.go`）：在 `RegisterBlocks()` 中注册 `HarnessBlock`

---

## 3. 详细技术规范

### 3.1 `sandbox.network` 默认值调整
- 在 `internal/sandbox/spec/schema.go` 中：当 `b.Network == nil` 时，默认赋值 `"bridge"`（替代原先的 `"none"`）。
- 允许枚举值保持不变：`"bridge"` | `"none"` | `"host"`；显式写 `network = null` 时同样回退默认值 `"bridge"`。

---

### 3.2 `harness "<name>"` 块语法与校验规则（对齐 `tflint`）

```hcl
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity" # 可选（与 version 同设或同省）
  version     = "0.1.0"                                           # 可选（禁止带前缀 "v"，不允许 >= 等范围表达式）
  signature   = "auto"                                            # 可选："auto"(默认) | "attestation" | "pgp" | "none"
  signing_key = null                                              # 可选：signature = "pgp" 时必填

  env_refs = {                                                    # 可选：插件专属宿主机环境变量名映射
    GEMINI_API_KEY = "GEMINI_API_KEY"
  }

  options = {                                                     # 可选：透传给插件 OpenSession 的已知配置对象/映射
    location = "us-central1"
  }
}
```

- **`golden` 接口实现**：`golden.PlanBlock` + `golden.SingleValueBlock`（及必要的 `BaseDecode` / `CustomDecode`；若使用 `CustomDecode`，过滤元属性/元块必须直接调用 `golden.MetaAttributeNames` 与 `golden.MetaNestedBlockNames`！）：
  - `Type() string { return "" }`
  - `BlockType() string { return "harness" }`
  - `AddressLength() int { return 2 }`
  - `CanExecutePrePlan() bool { return false }`
- **与 `model_provider` 的正交铁律**：
  - `harness` 块**不支持也不允许声明 `model_provider` 属性**（写 `model_provider` 按未知属性报错）。`harness` 与 `model_provider` 在 `worker` / `qc` 上独立组合。
- **字段校验细则（`HarnessConfig.Validate()`）**：
  1. **`source` 与 `version` 成对约束（对齐 `tflint` 三模式）**：
     - 两者均省略（或均为 `null`）：合法！若 `b.Name() == "copilot"` 则代表使用内置（bundled）Copilot Harness（若本地插件目录存在手动安装的 `sofa-harness-copilot` 则运行期本地优先）；若为其他名称则代表本地手动安装模式（运行期查找 `<plugin_dir>/sofa-harness-<name>`）。
     - 仅设置其中一个而省略另一个：直接 Plan 报错（`source and version must be set together or omitted together`）。
     - 两者均设置时：
       - `source` 必须符合 `<host>/<owner>/<repo>` 三段非空格式（如 `github.com/org/sofa-harness-foo` 或私有 GHES `ghe.example.com/org/repo`；禁止协议头 `https://`、禁止空白字符、禁止 `..` 或路径后缀 `.git`）；解析保存 `SourceHost`、`SourceOwner`、`SourceRepo`。
       - `version` 必须为精确语义化版本号（如 `"0.1.0"`、`"1.2.3-rc.1"`），**严禁以 `"v"` 开头**（提示去掉前导 `"v"`，GitHub Release Tag 会自动拼为 `v{version}`），严禁包含版本范围操作符（`>=`、`~>`、`*` 等）。
  2. **`signature` 与 `signing_key`（对齐 `tflint`）**：
     - `signature`（可选 `string`，省略或 `null` 时默认 `"auto"`）：仅允许 `"auto"` | `"attestation"` | `"pgp"` | `"none"`。
     - 当 `signature == "pgp"` 时，`signing_key` 必须为非空字符串；当 `source` 省略（手动/内置模式）时，若显式设置了非默认 `signature` 或 `signing_key` 则报错（无远端下载无需验签）。
  3. **`env_refs`（可选 `map(string)`）**：
     - Plan 期必须完全已知（`IsWhollyKnown()`），拒绝 `null` 元素；键和值都必须是合法的环境变量名（匹配 `^[A-Za-z_][A-Za-z0-9_]*$`）；Plan 期**绝不读取宿主机环境变量值**。
  4. **`options`（可选任意已知结构化 `cty.Value`）**：
     - 若提供且非 `null`，必须在 Plan 期完全已知（`IsWhollyKnown()`），且类型为 `object` 或 `map`（通过 `spec.ValidateType` 或递归检查不含 `CapsuleType`），保留其原始敏感标记（`SensitiveMark`）。
- **导出方法与 `Value() cty.Value`**：
  - 导出 `HarnessConfig() HarnessConfig`（返回深拷贝，供 `005` / `007` 快照读取）；
  - 未规划前 `Value()` 返回 `cty.DynamicVal`；规划后返回：
    ```go
    cty.ObjectVal(map[string]cty.Value{
        "address": cty.StringVal(b.Address()), // "harness.<name>"
        "kind":    cty.StringVal("harness"),
        "name":    cty.StringVal(b.Name()),
        "bundled": cty.BoolVal(cfg.IsBundled()), // source=="" && name=="copilot"
        "source":  optionalStringVal(cfg.Source),
        "version": optionalStringVal(cfg.Version),
    })
    ```

---

### 3.3 `RootDefaults` 扩展（`internal/planner/defaults.go`）

在 `RootDefaults` 结构体中新增 `Harnesses []string`：
- `InspectRootDefaults(blocks)` 同步收集顶层 `block.Type == "harness"` 的名称列表；
- 新增 `ResolveDefaultHarness(consumerAddr string) (*string, error)`：
  - `0` 个 `harness`：返回 `(nil, nil)`（代表使用内置默认 `"copilot"` Harness）；
  - `1` 个 `harness`：返回 `(&d.Harnesses[0], nil)`；
  - `>= 2` 个 `harness`：返回歧义错误（列出所有候选 `harness` 名称，提示显式设置 `harness = harness.<name>`）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **`sandbox` 默认网络变更**：省略 `network` 及显式 `network = null` 时，`sandbox.<name>.network` 与 `SandboxConfig().Network` 均为 `"bridge"`；显式 `network = "none"` 仍正常生效。
2. **`harness` 三种声明模式与负向校验**：
   - 无 `source`/`version` 的 `harness "copilot" {}`（`bundled == true`）与 `harness "custom" {}`（`bundled == false`，手动模式）均通过 Plan；
   - 带合法 `source = "github.com/org/sofa-harness-agy"` + `version = "0.1.0"` 通过 Plan；
   - 只写 `source` 不写 `version`（或反之）报错；`version = "v0.1.0"`（带 `v` 前缀）或 `version = ">= 0.1.0"` 报错；`source = "https://github.com/..."` 报错；
   - `signature = "pgp"` 未配 `signing_key` 报错；非法 `env_refs` 变量名报错；`options` 含未知值报错、含敏感值时保留 `SensitiveMark`；
   - 在 `harness` 内误写 `model_provider` 时按未知属性报错。
3. **`RootDefaults.ResolveDefaultHarness` 矩阵**：完整覆盖 `0`（返回 `nil`）、`1`（返回唯一名称）、`2`（返回含候选名的歧义错误）三种情况。
