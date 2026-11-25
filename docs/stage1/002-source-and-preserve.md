# 分片 002：Git 代码源与保留块（`source`、`preserve`）

## 1. 目标与边界

在分片 `001` 的 `SofaConfig` 基础上，新增 Git 仓库基线声明块 `source "<name>"` 与分支清理保护块 `preserve "<name>"`，并落地 **Root 单例 `source` 下 `preserve.url` 的省略自动绑定**。

**前置依赖**：分片 `001`。
**本分片不做**：真实网络/本地 `git ls-remote` 操作（Plan 期纯离线校验，锁定远端 SHA 是运行期行为）。

---

## 2. 包与文件清单

- `internal/spec/branch_ref.go`：分支引用结构与校验辅助（供 `source`、`worker`、`merge` 共用）
- `internal/source/schema.go`：`SourceBlock`、`PreserveBlock`、`SourceConfig`、`PreserveConfig`
- `internal/planner/defaults.go`（首版）：预处理 Root 单例 `source` 对省略 `url` 的 `preserve` 块的属性补全
- 修改 `internal/planner/config.go`：注册 `SourceBlock` 与 `PreserveBlock`

---

## 3. 核心语法与数据模型

### 3.1 `source "<name>"` 块

```hcl
source "project" {
  url         = var.repository_url
  branch      = var.source_branch
  token_ref   = "GITHUB_TOKEN" # 可选
  ssh_key_ref = null           # 可选，与 token_ref 互斥
}
```

- **`golden` 接口实现**：`golden.PlanBlock` + `golden.SingleValueBlock`
  - `Type() string { return "" }`
  - `BlockType() string { return "source" }`
  - `AddressLength() int { return 2 }`
  - `CanExecutePrePlan() bool { return false }`
- **字段与校验约束（`SourceConfig.Validate()`）**：
  - `url`（必填，Plan 期已知非空 `string`）：必须为合法的 Git 仓库地址（支持 `https://`、`http://`、`ssh://`、`file://`、本地路径或 SCP 风格 `git@host:owner/repo.git`；严禁包含空白控制字符；若为 HTTP(S) URL 则禁止内嵌明文 `user:password@` 凭据——提示改用 `token_ref`！）。
  - `branch`（必填，Plan 期已知非空 `string`）：合法 Git 分支名（禁止为空、禁止以 `-` 或 `/` 开头结尾、禁止包含 `..`、`~`、`^`、`:`、空白或 `@{`）。
  - `token_ref`（可选，Plan 期已知非空 `string`）：环境变量名（符合 `^[A-Za-z_][A-Za-z0-9_]*$`）；Plan 期**只校验变量名格式，绝不读取宿主机环境变量**（因为离线本机 Plan 环境未必配了服务端才有的密钥）。
  - `ssh_key_ref`（可选，Plan 期已知非空 `string`）：环境变量名，格式要求同上。
  - **互斥约束**：`token_ref` 与 `ssh_key_ref` 至多设置一个，同时非空时 Plan 报错。
  - 未知属性与未知嵌套块校验直接委托给 `golden.Decode`（`gohcl`），**不要手写拦截 `for_each`、`depends_on`、`precondition`、`dynamic` 等 `golden` 元语法**。
- **`Value() cty.Value` 对外暴露结构**：
  ```go
  cty.ObjectVal(map[string]cty.Value{
      "address":    cty.StringVal(b.Address()),       // "source.project"
      "kind":       cty.StringVal("source"),
      "name":       cty.StringVal(b.Name()),          // "project"
      "url":        cty.StringVal(b.URL),
      "branch":     cty.StringVal(b.Branch),
      "commit_sha": cty.UnknownVal(cty.String),       // Apply 启动锁定基线时才确定！
  })
  ```

---

### 3.2 `preserve "<name>"` 块（⚠️ 重点防坑：`branch` 支持未知值！）

```hcl
preserve "final_implementation" {
  url    = source.project.url           # Root Module 仅有 1 个 source 时可省略！
  branch = worker.implement.work_branch # 注意：Plan 期这里是 cty.UnknownVal(cty.String)！
}
```

- **`golden` 接口实现**：`golden.CustomDecode` + `golden.PlanBlock` + `golden.SingleValueBlock`
  - `Type() string { return "" }`
  - `BlockType() string { return "preserve" }`
  - `AddressLength() int { return 2 }`
  - `CanExecutePrePlan() bool { return false }`
- **为什么必须用 `CustomDecode` / `cty.Value` 解码 `branch`？**
  - 在真实配置中，`branch` 几乎总是引用 `worker.<x>.work_branch` 或 `merge.<x>.work_branch`，它们在 Plan 期都是 **`cty.UnknownVal(cty.String)`**！
  - 如果把结构体写成 `Branch string \`hcl:"branch"\``，`gohcl.DecodeBody` 会直接报 `value must be known`！
  - **正确实现**：
    1. `url`：必须求值为**已知**非空 `cty.String`（因为仓库 `url` 在 Plan 期总是已知的）。
    2. `branch`：求值得到 `cty.Value`（先 `UnmarkDeep`）：
       - 拒绝 `IsNull()`；
       - 类型必须满足 `Type().Equals(cty.String)`（否则报错 `preserve branch must be a string`）；
       - 若 `IsWhollyKnown()`，立即执行与 `source.branch` 相同的合法 Git 分支名校验；
       - 若 `!IsWhollyKnown()`（即 `cty.UnknownVal(cty.String)`），允许通过，并记录原始表达式 `BranchExpr = attr.ExprString()` 供后续 Plan 快照序列化！
- **Root 单例 `source` 自动补全 `preserve.url` 规则**：
  - 在 `planner` 对 Root Module 的 `loaded.Blocks` 调用 `golden.InitConfig` 之前：
    - 统计 Root Module 中 `block.Type == "source"` 的块列表；
    - 检查所有未显式声明 `url` 属性的 `preserve` 块：
      - 若 `len(sources) == 0`：直接返回错误 `preserve "<name>" omits url, but no source block is declared in root module`；
      - 若 `len(sources) > 1`：直接返回错误 `preserve "<name>" omits url, but multiple source blocks are declared in root module (...); set url explicitly`；
      - 若 `len(sources) == 1`：在克隆后的 `preserve` `*golden.HclBlock` 上注入属性 `url = source.<only_source_name>.url`（同时构造 `hclsyntax.ScopeTraversalExpr` 与 `hclwrite.TokensForTraversal`），使 `golden` 自动建立 `source.<only> -> preserve.<name>` 的 DAG 依赖边并求值！

---

## 4. 必测 TDD Red → Green 用例清单

1. **`source` 合法与非法校验**：
   - 支持 HTTPS URL、`git@github.com:org/repo.git`、本地路径；
   - URL 含内嵌密码 `https://user:secret@github.com/...` 被拒绝；
   - 非法 Git 分支名（空串、含空格、`a..b`、`/main`）被拒绝；
   - `token_ref` 与 `ssh_key_ref` 同时设置报错；非法环境变量名（如 `"123-BAD"`）报错；宿主机未设置 `GITHUB_TOKEN` 时 Plan **依然成功**；
   - `source.project.commit_sha` 在下游表达式中可读且为 `UnknownVal(cty.String)`。
2. **`preserve` 已知与未知分支引用**：
   - `branch = "release/v1"` 已知合法分支通过；`branch = "bad..branch"` 已知非法分支报错；
   - 当 `branch` 引用一个返回 `cty.UnknownVal(cty.String)` 的测试桩块（或 `output`）时，Plan 成功且 `golden` 自动建立从该上游块到 `preserve.<name>` 的依赖边；当引用一个未知 `cty.Number` 时，Plan 期报类型错误！
3. **`preserve` 省略 `url` 的 0 / 1 / ≥2 `source` 矩阵**：
   - 0 个 `source` + 省略 `url` → Plan 报错；
   - 1 个 `source "project"` + 省略 `url` → 自动解析得到 `source.project.url` 且 DAG 中 `preserve` 依赖 `source.project`；
   - 2 个 `source` + 省略 `url` → Plan 报歧义错误；2 个 `source` + 显式写 `url = source.b.url` → Plan 成功。
