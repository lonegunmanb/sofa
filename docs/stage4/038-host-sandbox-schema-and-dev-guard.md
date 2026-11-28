# 分片 038：`sandbox { runtime = "host" }` HCL 契约、相对产物路径与单机 `--dev` 门禁

## 1. 目标与边界

1. 在 `internal/sandbox/spec` 中新增 `runtime = "host"` 支持，严格实施与容器专属字段（`image`、`mount`、`resources`、`network = "bridge"|"none"`）的互斥校验，同时保持 `0` 个 `sandbox` 在 Plan 期报错不变。
2. 当 `worker` / `dynamic_worker` 绑定的 `sandbox`（含显式绑定与 Root Module 单例继承）为 `runtime == "host"` 时，使其内部 `artifact("<name>").path` 返回 `".sofa/artifacts/<rel_path>"`、`import_artifact("<name>").path` 返回 `".sofa/imports/<rel_path>"`（容器运行时继续返回 `"/.sofa/artifacts/..."` 与 `"/.sofa/imports/..."`）。
3. 为 `sofa server` 增加 `--temp-dir` CLI 参数（`--dev` 模式下默认 `<state-dir>/worker-tmp`）。
4. 实施 **仅限单机 `--dev` 使用** 的双层运行期门禁：
   - 控制面 `internal/server`：当 `!s.cfg.DevMode` 时，`POST /api/v1/runs` 在第 0 秒解析上传的 `.sofaplan`（含递归子模块 `module`），只要存在 `runtime == "host"` 的 `sandbox` 立即返回 HTTP `422`（或 `400`）清晰错误；
   - 执行面 `internal/worker/daemon`：当 `!d.opts.DevMode` 时，`start_node` 与 `run_merge_candidate` 若检测到有效 `sandbox.Runtime == "host"`（含 `qc.sandbox`），立即拒绝并返回清晰错误。
5. 同步更新 `docs/hcl-contract.md` 中 `sandbox` 块的契约说明。

**前置依赖**：`037`（已完成）。

---

## 2. 涉及包与文件清单

- `internal/sandbox/spec/schema.go` & `internal/sandbox/spec/schema_test.go`（或对应测试文件）
- `internal/worker/spec/{artifact.go,schema.go,dynamic.go,worker_test.go,dynamic_test.go}`
- `internal/planner/{config.go,export.go,export_test.go}` & `internal/applyeval/{eval.go,eval_test.go}`
- `internal/server/{handlers.go,server_test.go}`
- `internal/worker/daemon/{daemon.go,daemon_test.go}`
- `internal/cli/{server_cmd.go,cli_test.go}`
- `docs/hcl-contract.md`

---

## 3. 实现细则与防坑约束

1. **`SandboxBlock` 解码与 `SandboxConfig.Validate()`（`internal/sandbox/spec/schema.go`）**：
   - 将 `SandboxBlock.Image` 改为可选指针 `*string \`hcl:"image,optional"\``；
   - `runtime == "host"` 时：
     - `Image != nil`（即使空字符串）报错：`sandbox with runtime "host" must not set image`；
     - `len(MountBlocks) > 0` / `len(Mounts) > 0` 报错：`sandbox with runtime "host" must not declare mount blocks`；
     - `len(ResourceBlocks) > 0` / `Resources != nil` 报错：`sandbox with runtime "host" must not declare resources`；
     - `Network` 未设置时默认 `"host"`；若显式设置且 `!= "host"` 报错：`sandbox with runtime "host" only supports network = "host"`；
     - `Value()` 导出的 `cty.ObjectVal` 中 `"image"` 在 `host` 模式下设为 `cty.StringVal("")`（保持对象属性类型集稳定不变，不破坏 `golden` 引用类型推导！）。
   - `runtime` 为 `"podman" | "docker" | "nerdctl" | "kirk"` 时：保持原有全部规则（`image` 必填非空且无空白字符，默认 `network = "bridge"`）。
2. **`artifact().path` 与 `import_artifact().path` 路径感知（`internal/worker/spec` + `internal/applyeval`）**：
   - 新增常量 `HostArtifactsRoot = ".sofa/artifacts"`、`HostImportsRoot = ".sofa/imports"`；
   - 在 Plan 期求值 `worker` / `dynamic_worker` 的 `artifact(...)` 与 `import_artifact(...)` 函数返回值时，读取当前块绑定的 `sandbox` 值（`sandbox` 已通过显式属性或 `InitConfig` 单例注入建好 DAG 前置依赖边，因此在 `ExecuteDuringPlan` 时 `sandbox.Value().GetAttr("runtime")` 已知！）：
     - 若 `runtime == "host"`，`"path"` 字段分别为 `path.Join(HostArtifactsRoot, art.Path)` 与 `path.Join(HostImportsRoot, effectiveImportRelPath(imp))`；
     - 否则保持 `ContainerArtifactsRoot`（`"/.sofa/artifacts/..."`）与 `ContainerImportsRoot`（`"/.sofa/imports/..."`）。
   - 在 `internal/applyeval` 延迟重求值（如 `dynamic_worker` 展开实例或含上游未知引用的 `prompt` / `system_prompt`）时保持完全一致的判断逻辑！
3. **`--dev` 强门禁与 `--temp-dir`**：
   - `internal/server/handlers.go`（`handleCreateRun`）：当 `!s.cfg.DevMode` 时，递归遍历 `loaded.Plan`（及所有 `n.Module.Plan`）中的 `"sandbox"` 节点，若 `runtime == "host"`，立即响应 `422 Unprocessable Entity`（错误消息明确包含 `sandbox runtime "host" is only allowed in single-node "sofa server --dev" mode`）；
   - `internal/worker/daemon/daemon.go`：当 `!d.opts.DevMode` 时，`runNodeTask` 检查 `req.Sandboxes`（或当前 `worker`/`qc` 选中的 sandbox）含 `Runtime == "host"`、`runMergeTask` 检查 `req.Sandbox.Runtime == "host"` 时立即失败回报；
   - `internal/cli/server_cmd.go`：绑定 `--temp-dir` flag 至 `devServerConfig.TempDir`，非空时优先使用该路径作为内置 `WorkerDaemon.Options.TempDir`，为空时回退 `<absStateDir>/worker-tmp`。

---

## 4. 必测 TDD Red → Green 用例清单

- **HCL Schema 单测**：`runtime = "host"` 省略 `image`/`network` 通过且 `network == "host"`；显式 `network = "host"` 通过；配置 `image`、`mount`、`resources`、`network = "bridge"` 或 `network = "none"` 均精确报错；容器 `runtime = "podman"` 省略 `image` 仍报错。
- **Artifact/Import 路径单测（Plan 期 + ApplyEval 期）**：Root 单例 `sandbox { runtime = "host" }` 及显式 `sandbox = sandbox.host_dev` 下，`worker` 与 `dynamic_worker` 中 `${artifact("summary").path}` 求值为 `".sofa/artifacts/summary.md"`、`${import_artifact("in").path}` 求值为 `".sofa/imports/in"`；`podman` 下仍为 `"/.sofa/artifacts/summary.md"`。
- **门禁与 CLI 单测**：非 `--dev` 的 `server.Server` 提交含 `runtime = "host"`（含嵌套子 `module` 内）的 `.sofaplan` 返回 `422`；`--dev` 下允许通过；`WorkerDaemon` 在 `DevMode: false` 下拒绝 `host` 节点/合并任务；`sofa server --help` 暴露 `--temp-dir` 并在 `--dev` 下生效。
