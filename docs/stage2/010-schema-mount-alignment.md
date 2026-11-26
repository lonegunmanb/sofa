# 分片 010：跨阶段契约收口——禁用 `sandbox.entrypoint`、`qc` 独立 `sandbox`/`profile` 覆盖与容器外 `/.sofa/{artifacts,imports}` 路径对齐

## 1. 目标与边界

在编写阶段二运行期代码前，把 `/grill-me` 敲定的三项 HCL 语义修正干净收口，避免阶段一导出的 `Plan` 与阶段二容器挂载契约脱节：
1. **禁用 `sandbox.entrypoint`**：从 `sandbox` 块删除 `entrypoint` 属性支持（用户写 `entrypoint` 时按未知属性直接报错），确保容器主进程入口 100% 由 `sofa` 托管（防止自定义入口向 `stdout` 输出内容破坏 `go-plugin` 首行握手）。
2. **`qc` 支持可选覆盖独立 `sandbox`（及 `profile`）**：允许 `worker` 内的 `qc` 嵌套块显式配置 `sandbox = sandbox.<name>` 与 `profile`（省略或为 `null` 时默认继承所属 `worker` 的 `sandbox` / `profile`），使 `(sandbox, harness, model_provider, model/profile)` 四个维度在 `qc` 上完全正交支持异构审计容器。
3. **`artifact` 与 `import_artifact` 容器外绝对路径对齐**：将 `${artifact("<name>").path}`（含 `worker.<w>.artifact.<name>.path`）统一改为返回 `"/.sofa/artifacts/<rel_path>"`，将 `${import_artifact.<name>.path}` 统一改为返回 `"/.sofa/imports/<rel_path_or_name>"`，使 `/workspace` 保持 100% 纯净的 Git 工作树。

**前置依赖**：阶段一 `009`。
**本分片不做**：任何容器启动、`BlobStore` 读写或磁盘挂载（仅纯离线 Schema / Planner / Export / 测试对齐，100% Tier 1 单测）。

---

## 2. 涉及包与文件清单

- 修改 `internal/sandbox/spec/schema.go` 及 `schema_test.go`
- 修改 `internal/worker/spec/qc.go`、`artifact.go`、`schema.go` 及 `schema_test.go`
- 修改 `internal/planner/export.go` 及 `internal/planner/*_test.go`（适配移除 `entrypoint`、`qc.sandbox`/`qc.profile` 导出与新产物路径）
- 同步修订 `docs/hcl-contract.md` 中 `sandbox`、`qc`、`artifact` 与 `import_artifact` 示例说明

---

## 3. 保姆级实现细则（逐文件照做）

### 3.1 移除 `sandbox.entrypoint`（`internal/sandbox/spec/schema.go` & `internal/planner/export.go`）
1. 在 `internal/sandbox/spec/schema.go` 中：
   - 从 `SandboxConfig` 结构体删除 `Entrypoint []string` 字段；
   - 从 `SandboxBlock` 结构体删除 `Entrypoint cty.Value`（或 `hcl:"entrypoint,optional"`）字段及对应解码逻辑——直接交由 `golden.Decode` 原生拒绝未知属性 `"entrypoint"`（**切勿手写 `if attr == "entrypoint"` 黑名单检查，遵守 `golden` 铁律**）；
   - 从 `SandboxConfig.Validate()` 删除 `c.Entrypoint` 相关校验；
   - 从 `SandboxBlock.Value()` 返回的 `cty.ObjectVal` 中删除 `"entrypoint"` 属性（若之前暴露了该属性）。
2. 在 `internal/planner/export.go` 的 `encodeSandboxConfig` 中：
   - 从导出的 `cty.ObjectVal` 删除 `"entrypoint": stringListVal(cfg.Entrypoint)`。

### 3.2 `qc` 块支持独立覆盖 `sandbox` 与 `profile`（`internal/worker/spec/qc.go` & `internal/planner/export.go`）
1. 在 `internal/worker/spec/qc.go` 中：
   - 在 `qcAllowedAttrs` 新增 `"sandbox": {}` 与 `"profile": {}`；
   - 在 `QCConfig` 结构体新增 `Sandbox cty.Value`（默认 `cty.NullVal(cty.EmptyObject)`，代表继承所属 `worker.Sandbox`）与 `Profile string`；
   - 在 `decodeQC` 中：
     - 解析可选 `sandbox` 属性（逻辑与 `qc.harness` 完全一致：若提供且非 `null`，调用 `validateBlockReference(val, "sandbox", "qc sandbox")` 校验引用了合法 `sandbox.<name>` 并存入 `result.Sandbox`；注意 `golden` 扫描块内遍历表达式时会自动建立 `worker -> sandbox.<qc_sandbox>` 的 DAG 依赖边，同时确保 `dynamic_worker` 虚拟 worker 校验同样生效）；
     - 解析可选 `profile` 属性（非空白已知字符串，与 `qc.model` 一致）。
2. 在 `internal/planner/export.go` 的 `encodeWorkerConfig`（`qcVal` 编码处）中：
   - 新增 `"sandbox": qcSandbox`（`cty.NilVal` 归一化为 `cty.NullVal(cty.EmptyObject)`）与 `"profile": cty.StringVal(cfg.QC.Profile)`。

### 3.3 `artifact` 与 `import_artifact` 容器绝对路径映射（`internal/worker/spec/artifact.go`）
1. 定义两个导出路径前缀常量：
   ```go
   const (
       ContainerArtifactsRoot = "/.sofa/artifacts"
       ContainerImportsRoot   = "/.sofa/imports"
   )
   ```
2. **HCL 输入校验保持不变**：
   - 用户在 HCL `artifact "<name>" { path = "reports/out.json" }` 和 `import_artifact "<name>" { path = "design.md" }` 中填写的 `path` **仍然是干净的相对子路径**（继续使用 `validateWorkspaceRelativePath` 拒绝绝对路径、`\`、驱动器盘符、`.`、`..` 逃逸）。
3. **结构体字段明确区分“相对子路径”与“容器内绝对路径”**：
   - `Artifact` 结构体：
     - `Path string`：保存清理后的相对子路径（如 `"reports/out.json"`，供宿主机在临时 `artifactsDir` 下定位文件）；
     - 新增方法 `func (a Artifact) ContainerPath() string { return path.Join(ContainerArtifactsRoot, a.Path) }`（返回 `"/.sofa/artifacts/reports/out.json"`）。
   - `artifactValue(artifact Artifact) cty.Value`：
     - 将 `"path"` 字段改为 `cty.StringVal(artifact.ContainerPath())`，新增 `"rel_path": cty.StringVal(artifact.Path)`！这样：
       - `prompt = "写入 ${artifact("design").path}"` 在 Plan 期直接展开为 `"写入 /.sofa/artifacts/plan.md"`；
       - 下游 `import_artifact` 引用的 `sources = [worker.plan.artifact.design]` 同时携带 `rel_path` 与容器路径元数据。
4. **`import_artifact` 路径求值**：
   - 修改 `effectiveImportRelPath(imported ImportArtifact) string`：若 `imported.Path != ""` 返回 `path.Clean(imported.Path)`（注意：若用户旧写法写了前导 `".sofa/imports/foo"`，它本身仍是合法相对子路径，但推荐直接写 `"design.md"`）；若 `imported.Path == ""` 返回 `imported.Name`；
   - 新增方法 `func (i ImportArtifact) ContainerPath() string { return path.Join(ContainerImportsRoot, effectiveImportRelPath(i)) }`（例如 `"/.sofa/imports/design"` 或 `"/.sofa/imports/design.md"`）；
   - 在 `WorkerBlock` 注入的 `import_artifact.<name>` 对象中，`.path` 统一设为 `cty.StringVal(imp.ContainerPath())`！
5. **冲突校验调整（`internal/worker/spec/schema.go`）**：
   - 保留同一 `worker` 内多个 `artifact` 之间的 `Path` 重叠/父子目录冲突检查；
   - 保留同一 `worker` 内多个 `import_artifact` 之间的 `effectiveImportRelPath` 重叠/父子目录冲突检查；
   - **移除** `artifact.Path` 与 `import_artifact.Path` 之间的跨集合路径冲突报错（因为一个在 `/.sofa/artifacts/`，另一个在 `/.sofa/imports/`，物理挂载树已完全隔离，即使都叫 `"spec.md"` 也绝不冲突！）。
6. **`internal/planner/export.go` 同步导出**：
   - 在 `encodeWorkerConfig` 编码 `artifacts[name]` 时，包含 `"path": cty.StringVal(art.Path)` 与 `"container_path": cty.StringVal(art.ContainerPath())`；
   - 编码 `imports[name]` 时，包含 `"path": cty.StringVal(effectiveImportRelPath(imp))` 与 `"container_path": cty.StringVal(imp.ContainerPath())`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **拒绝 `sandbox.entrypoint`**：
   - 编写 `sandbox "go" { runtime = "podman"; image = "golang:1.27"; entrypoint = ["/bin/sh"] }`，验证 `golden` 解码阶段直接报错拒绝未知属性 `entrypoint`；
   - 验证合法 `sandbox` 块完成 `ExportPlan` -> `Marshal` -> `Unmarshal` -> `Display` 全链路无 `"entrypoint"` 字段残留。
2. **容器外绝对路径插值验证**：
   - `artifact "design" { type = "file"; path = "docs/plan.md"; description = "Plan" }`：验证块内 `${artifact("design").path}` 与外部 `worker.<w>.artifact.design.path` 均求值为 `"/.sofa/artifacts/docs/plan.md"`，且 `rel_path` 为 `"docs/plan.md"`；
   - `import_artifact "design" { sources = [...] }`（省略 `path`）：验证 `${import_artifact.design.path}` 求值为 `"/.sofa/imports/design"`；显式写 `path = "inputs/plan.md"` 时求值为 `"/.sofa/imports/inputs/plan.md"`；
   - 在 HCL 中直接给 `artifact.path` 写绝对路径 `"/etc/passwd"` 或 `"../escape"` 时，依然在 Plan 期报错拦截。
3. **同名 `artifact` 与 `import_artifact` 互不冲突**：
   - 同一 `worker` 同时声明 `import_artifact "doc" { path = "spec.md"; sources = [...] }` 与 `artifact "out" { type = "file"; path = "spec.md"; description = "Out" }` 时合法通过（一个在 `/.sofa/imports/spec.md`，一个在 `/.sofa/artifacts/spec.md`）；但两个 `artifact` 同指 `"spec.md"`（或 `"dir"` 与 `"dir/a.txt"`）依然报错。
4. **`qc` 独立覆盖 `sandbox` 与 `profile`**：
   - `qc` 省略 `sandbox`（或写 `sandbox = null`）时导出为 `null`（运行期继承 `worker.sandbox`）；
   - `qc { sandbox = sandbox.sec_audit; profile = "opus-audit"; criteria = { ... } }` 通过 Plan、自动建立对 `sandbox.sec_audit` 的 DAG 依赖边，且 `ExportPlan` 完整保留 `qc.sandbox` 与 `qc.profile`；误传非 `sandbox` 块引用（如 `sandbox = harness.copilot`）在 Plan 期报错。
