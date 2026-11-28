# 分片 041：宿主自动注入运行时环境与协议工具系统提示词

## 1. 目标与边界

将 `sofa` 宿主自身的运行时环境布局、写权限边界与会话协议工具使用规范从业务 `.sofa.hcl` 手写模板中下沉至 `internal/worker/runner`，由宿主在启动 Work 与 QC 会话时**自动生成并追加到 `SystemPrompt`**：
1. 用户在 `worker.system_prompt` 中只需关注纯领域/业务要求（如架构规范、代码风格），无需手写 `.git` 只读禁令、`writable_paths` 列表、`artifact` 路径或 `sofa_checkpoint` / `sofa_complete_work` 协议说明。
2. 自动兼容 **容器沙箱（`podman`/`docker`/`nerdctl`/`kirk`）** 与 **宿主机模式（`runtime = "host"`）** 两种路径布局。
3. 清理 `examples/copilot-agy-self-host/main.sofa.hcl` 中带 `# TODO(041)` 的临时手写运行时提示词。

**前置依赖**：Stage 3 + `037`（本分片可先于 `038`–`040` 独立落地：`prompt_contract.go` 通过判断 `sandboxRuntime == "host"` 字符串分支提前兼容宿主机路径模式；在 `038` 尚未落地前，`host` 分支通过直接单测 `buildWorkRuntimeContractPrompt` / `buildQCRuntimeContractPrompt` 纯函数验证，不经过 `SandboxConfig.Validate()`）。

---

## 2. 涉及包与文件清单

- `internal/worker/runner/{prompt_contract.go,worker_runner.go,runner_test.go}`
- `examples/copilot-agy-self-host/main.sofa.hcl` & `examples/examples_test.go`

---

## 3. 实现细则与防坑约束

1. **Work 会话自动契约段落（`buildWorkRuntimeContractPrompt`）**：
   - 使用 `appendPromptSection(userSystemPrompt, runtimeContract)` 追加在用户 `system_prompt`（及恢复日记）之后，包含清晰、紧凑的结构化条目：
     - **工作目录与 Git 禁令**：明确当前工作目录路径（容器模式 `/workspace`，`host` 模式宿主机工作区路径），强调 **严禁执行 `git add`、`git commit`、`git push`、`git checkout`、`git reset`、`git clean` 等修改 Git 仓库元数据的命令**（Git 检查点提交与分支推送均由 `sofa` 宿主自动管理）；
     - **可写路径边界（`writable_paths`）**：若配置了非 `nil` 的 `writable_paths`，逐项列出允许修改的相对路径 glob（如未配置则说明允许修改工作区内普通文件，但严禁触碰 `.git/` 与 `.sofa/` 内部控制文件）；
     - **已声明导入与输出产物清单**：
       - 若存在 `imports`，列出每个 `import_artifact` 的名称与可读路径（容器 `"/.sofa/imports/..."` 或宿主 `".sofa/imports/..."`）；
       - 若存在 `artifacts`，按名称排序列出每个 `artifact` 的路径（容器 `"/.sofa/artifacts/..."` 或宿主 `".sofa/artifacts/..."`）、类型（`file`/`directory`）、`required`/`non_empty` 约束与 `description`；
     - **协议工具生命周期**：
       - 在关键里程碑调用 `sofa_checkpoint` 提交结构化进度日记（`completed` / `next_steps`）；
       - 完成全部工作与产物写入并通过自检后，**必须调用终止工具**（默认 `sofa_complete_work`，若配置了自定义终止工具则点明其工具名 `<terminate_tool_name>`）结束本轮，切勿仅输出纯文本而不调用终止工具；
       - 仅在遇到阻塞性的需求歧义或需要人工拍板时调用 `sofa_request_decision`；若配置了 `agent_escalated` 类型的 `fallback`，列出可用升级目标及 `sofa_escalate`。
2. **QC 会话自动契约段落（`buildQCRuntimeContractPrompt`）**：
   - 更新 `qcBaseSession.SystemPrompt`：
     - 明确只读审计环境路径（容器 `/workspace` 与 `/.sofa/artifacts`，`host` 模式对应 `qc-workspace` 与 `.sofa/artifacts`），严禁修改任何工作区或产物文件、严禁执行任何 Git 写操作；
     - 若 Worker 声明了 `artifacts`，自动在 QC 系统提示词中列出所有待核查产物的路径与描述（帮助 QC 第一时间定位如 `summary.md` 等产物与 `git status` / `git diff`）；
     - 明确发现问题调用 `sofa_qc_open_issues`、复核已修复问题调用 `sofa_qc_resolve_issues`、结束本轮**必须调用 `sofa_qc_submit_verdict`**（`pass` 要求未关闭问题数为 0，`revise` 要求至少存在 1 条未关闭问题）。
3. **清理自举示例**：
   - 移除 `examples/copilot-agy-self-host/main.sofa.hcl` 中的 `# TODO(041)` 临时段落，保持示例简洁纯粹。

---

## 4. 必测 TDD Red → Green 用例清单

- `runner_test.go`：
  - 容器模式下 Work `SessionConfig.SystemPrompt` 包含用户 `system_prompt` + `/workspace`、禁 `git commit`、`writable_paths`、`/.sofa/artifacts/...`、`/.sofa/imports/...` 及 `sofa_complete_work`（以及自定义终止工具场景下的自定义工具名）；
  - `runtime == "host"` 分支下，纯函数单测验证 Work 与 QC 系统提示词准确切换为 `.sofa/artifacts/...` / `.sofa/imports/...` 与宿主机工作目录（在 `038` 落地前直接测试 `buildWorkRuntimeContractPrompt` / `buildQCRuntimeContractPrompt`，无需构造 `Runtime: "host"` 触发 `SandboxConfig.Validate()`）；
  - QC `SessionConfig.SystemPrompt` 自动包含 Worker 已声明的 `artifact` 路径清单与 `sofa_qc_submit_verdict` 规则。
- `examples_test.go`：验证精简后的 `examples/copilot-agy-self-host/main.sofa.hcl` 编译与 Plan 往返测试全绿。
