# 分片 031：Vue 3 安全看板与单二进制 `go:embed` 人工决策工作台（`web/`、`internal/webui`）

## 1. 目标与边界

遵循 `AGENTS.md` 前端规范（Vue 3 + TypeScript + Vite，Vitest + Vue Test Utils 严格 Red → Green → Refactor TDD），构建内嵌于 `sofa server` 同端口的人机协作与可观测工作台：
1. **单二进制 `go:embed` 与零阻塞构建契约（`internal/webui`）**：
   - `internal/webui` 通过 `//go:embed` 托管静态前端资源（SPA History 路由回退 `index.html`，API `/api/*` 与 `/healthz` 优先放行）；
   - 当未执行前端打包或仅运行纯 Go 测试时，内置极简静态状态页（提示构建命令并保证编译恒通过），使日常 `go test ./...` 绝不强耦合 `node_modules`；提供一键构建脚本将 `web/dist` 同步嵌入 `internal/webui/dist/`。
2. **四大核心工作台视图（`web/src/`）**：
   - **Run 列表与实时 DAG 拓扑视图**：展示全部 Run 状态，进入单个 Run 后按节点依赖渲染有向图（静态 `worker`、`dynamic_worker` 展开子卡片、`merge`、嵌套 `module`），通过 `/api/v1/ws` 实时刷新节点状态与当前 `epoch`；
   - **节点深潜抽屉（Node Inspector）**：查看轮次（`epoch`）历史、检查点 SHA 列表、结构化日记时间线、增量日志流（支持实时跟随与历史 JSONL 加载）、产物列表，以及发送人工插话（支持勾选 `wait_for_confirmation`）和单点 `Rerun` / 整图 `Pause` (`planned` / `immediate`) / `Resume` / `Delete`；
   - **人工决策与 `human_approval` 版本审批中心**：集中展示待办决策卡片（单选/自定义回复/追问；针对 `human_approval == "required"` 显式展示锁定的 `commit_sha` 与 `artifact_ids` 指纹，支持一键批准放行或打回指定上游 `fallback`）；
   - **三层受管 `Env` / `Secret` 设置页**：按 `Global` 与 `Namespace` 管理配置与只写密钥（`Secret` 输入框 `type="password"`，列表仅显示键名与更新时间，支持新增/轮换/删除）；若服务端启用 `SOFA_API_TOKEN`，顶部提供登录框将 Token 仅存于 `sessionStorage`。
3. **Agent 生成 HTML 原型的零信任沙箱预览（红线不变量！）**：
   - 预览 HTML 类 `Artifact` 时，组件**必须且只能渲染 `<iframe sandbox="allow-scripts" referrerpolicy="no-referrer" :src="previewUrl">`**——**严禁包含 `allow-same-origin`、`allow-top-navigation` 或 `allow-popups`**！确保不可信 Agent 产物永远运行在浏览器不透明 `null` 源下，无法读取 `sessionStorage` 或冒充人类提交审批。

**前置依赖**：分片 `029`、`030`。

---

## 2. 涉及包与文件清单

- `internal/webui/handler.go` & `handler_test.go`：`embed.FS` SPA 静态路由回退与占位页单测
- `web/package.json`、`vite.config.ts`、`tsconfig.json`：Vue 3 + TS + Vite + Vitest 工程配置
- `web/src/api/`、`web/src/components/`、`web/src/views/`：API/WS 客户端、DAG 视图、节点日志/日记抽屉、安全沙箱预览组件 `ArtifactSandboxPreview.vue`、决策卡片与凭据页
- `web/src/__tests__/*.spec.ts`：Vitest + Vue Test Utils 组件与安全契约单测

---

## 3. 必测 TDD Red → Green 用例清单

1. **前端 Vitest 单测（`npm --prefix web test -- --run`）**：
   - **安全红线断言**：挂载 `ArtifactSandboxPreview.vue`，断言渲染出的 `<iframe>` 精确具备 `sandbox="allow-scripts"`，且**绝不包含 `allow-same-origin`**！
   - **实时状态与日志流**：模拟 WebSocket 推送节点状态迁移与增量日志帧，验证 DAG 节点徽章与日志面板即时更新；
   - **决策与凭据交互**：验证点击批准卡片发送精确绑定 `(commit_sha, artifact_ids)` 的 `POST /api/v1/decisions/{id}/respond`；验证 `Secret` 管理列表不渲染任何明文且 Token 仅读写 `sessionStorage`。
2. **Go 嵌入路由单测（`internal/webui/handler_test.go`）**：
   - 验证 `/runs/123` History 路径回退返回 `200 text/html`，静态资源带正确 MIME 类型，不拦截 `/api/v1/*`。
