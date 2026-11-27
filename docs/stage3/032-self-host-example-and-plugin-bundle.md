# 分片 032：`copilot + antigravity` 自举模板与本地 Linux 双插件一键安装脚本（`examples/`、`scripts/`）

## 1. 目标与边界

打通本机真机自举（Dogfooding）的“最后一公里”，让开发者在克隆仓库后只需运行一条脚本装好两个同仓孵化的 Linux 插件，即可直接通过 `sofa server --dev` + `sofa apply ./examples/copilot-agy-self-host` 驱动 **Copilot 编码 + 容器门禁 + 独立只读容器 Antigravity 逐行审查** 开发新切片：
1. **本地 Linux 双插件一键构建安装器（`scripts/install-local-plugins.go`）**：
   - 自动探测本地 Podman 虚拟机目标架构（`linux/amd64` 或 `linux/arm64`，亦可通过 `-arch` 指定）；
   - 调用 `plugins/sofa-harness-copilot/scripts/build-bundle.go` 编译 `sofa-harness-copilot` 并获取对应架构 Linux `@github/copilot` CLI 二进制（支持 `-copilot-linux-bin` 本地缓存路径或使用容器内 `npm pack @github/copilot-linux-<x64|arm64>` 提取独立二进制缓存至 `.sofa/cache/`）；
   - 调用 `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py` 构建带自包含可重定位 Linux Python 3.12 + `google-antigravity` 的 `sofa-harness-antigravity` 包；
   - 原子安装至目标插件目录（默认 `~/.sofa/plugins` 或 `-plugin-dir ./.sofa/plugins`）下的 `sofa-harness-copilot/linux_<arch>/` 与 `sofa-harness-antigravity/linux_<arch>/`，100% 匹配 `013` 本地插件发现规则！
2. **核心自举工程模板（`examples/copilot-agy-self-host/`）**：
   - 收录生产级 `main.sofa.hcl`、`variables.sofa.hcl`、`outputs.sofa.hcl` 与简明自举操作手册 `README.md`；
   - 完整示范：
     - `source "repo"`（参数化 `var.repo_url`、`var.base_branch`、`token_ref = "GITHUB_TOKEN"`）；
     - `sandbox "go_dev"` 与 `sandbox "qc_audit"`（默认 `golang:1.27`，零自定义 `entrypoint`）；
     - `harness "copilot"`（`env_refs = { GITHUB_TOKEN = "COPILOT_GITHUB_TOKEN" }`）与 `harness "antigravity"`（`env_refs = { GEMINI_API_KEY = "GEMINI_API_KEY" }`）；
     - `worker "implement_slice"`：使用 `harness.copilot` 在 `var.writable_paths` 约束下实现目标切片并产出变更说明 `artifact "summary"`，挂载确定性 `gatekeeper "go_quality"`（`gofmt -l .`、`go vet ./...`、`go test ./... -count=1` 且 `require_clean_tree = true`），并配置异构独立只读 `qc`（`sandbox = sandbox.qc_audit`、`harness = harness.antigravity`、`model_provider = null`、可调 `var.qc_model` 与审查准则）及可配 `var.human_approval`（默认 `"required"`）。
3. **防腐烂自动化测试（`examples/examples_test.go`）**：
   - 在 `go test ./examples/...` 中直接调用真实 `planner` 对 `examples/copilot-agy-self-host` 执行完整编译与 `.sofaplan` 编码/解码往返校验，确保任何未来引擎改动若破坏示例语法会在 Tier 1 单测第一时间红灯报警！

**前置依赖**：分片 `019`、`020`、`030`。

---

## 2. 涉及包与文件清单

- `scripts/install-local-plugins.go`：跨平台本地 Linux 双插件构建与安装工具
- `examples/copilot-agy-self-host/variables.sofa.hcl`、`main.sofa.hcl`、`outputs.sofa.hcl`、`README.md`
- `examples/examples_test.go`：Tier 1 示例编译与 `.sofaplan` 往返校验单测

---

## 3. 必测 TDD Red → Green 用例清单

1. **Tier 1 示例防腐烂单测（`examples/examples_test.go`）**：
   - 传入示例必填变量（`repo_url`、`slice_prompt` 等），断言 `examples/copilot-agy-self-host` 通过真实 `sofa plan` 编译成功、生成的 `Plan` 中 `worker.implement_slice` 同时包含 `copilot` Work 配置、`go_quality` `gatekeeper` 与覆盖了 `antigravity` + `model_provider = null` 的独立 `qc` 配置，且 `ExtractPlanEnvRefs` 精确提取 `["COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "GITHUB_TOKEN"]`。
2. **脚本干跑/打包验证**：
   - 验证 `install-local-plugins.go` 输出的目录结构可被 `internal/harness/installer.NewInstaller().Prepare(...)` 无报错直接命中发现。
