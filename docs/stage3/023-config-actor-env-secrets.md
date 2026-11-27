# 分片 023：三层受管 `Env` / `Secret` Actor 与 `.sofaplan` 第 0 秒预检器（`internal/actor/config`）

## 1. 目标与边界

落地 `/grill-me` 锁定的三层受管配置与密钥中心，使 HCL 中的所有 `token_ref`、`ssh_key_ref`、`api_key_ref`、`bearer_token_ref`、`env_refs` 统一由 `sofa` 托管并在提交瞬间完成完整性预检：
1. **三层继承覆盖模型**：
   - 优先级固定为 **`Run` 单次层（`<run_id>`） > `Namespace` 层（默认 `"default"`） > `Global` 全局层**；
   - **同层键名互斥**：同一层内同一变量名 `K` 不能同时存在于 `Env` 与 `Secret` 中（若已作为 `Secret` 存在，在同一层调 `SetEnv(K)` 必须拒绝报错，需先显式删除或用同类型覆盖，防止误操作把敏感值降级暴露到可读 `Env` 列表！跨层允许高优先级层覆盖低优先级层）；
   - **`Secret` 严格 Write-Only**：外部查询接口仅返回 `SecretMeta{Name, Scope, Namespace, UpdatedAt}`，绝不返回明文。
2. **静态 `.sofaplan` 引用扫描与第 0 秒 Preflight**：
   - 递归扫描根 `Plan` 及所有嵌套子 `module.Plan` 中引用的全部宿主机环境变量名集合；
   - 在 `sofa apply` 创建 Run 时一次性校验三层合并视图是否全部命中，任一缺失立即返回结构化缺失变量清单（拒绝启动残缺 Run）；在派发具体节点给 `sofa-worker` 时，仅提取该节点实际所需的最小键值表下发至 Worker 内存 `LookupEnv`。

**前置依赖**：分片 `022`（`internal/daprhost`）。

---

## 2. 涉及包与文件清单

- `internal/actor/config/types.go`：三层作用域模型、Actor 请求/响应单参数结构体（严格遵循 `dapr/go-sdk` 单入参规范）
- `internal/actor/config/plan_refs.go`：`ExtractPlanEnvRefs(*plan.Plan)` 与 `ExtractNodeEnvRefs` 纯函数扫描器
- `internal/actor/config/actor.go`：`ConfigActor`（Actor Type `"ConfigActor"`，固定 ID `"global"`，基于 `actor.ServerImplBaseCtx`）与客户端 Stub 封装
- `internal/actor/config/config_test.go`：Tier 1 扫描器、三层覆盖优先级、同层 `Env`/`Secret` 互斥拦截单测
- `internal/actor/config/config_integration_test.go`（`//go:build integration`）：Tier 2 在真实本地加密 `daprd` 上验证跨重启持久化与密文落盘

---

## 3. 保姆级实现细则

### 3.1 核心方法（`ConfigActor("global")`）
> 注意：`dapr/go-sdk` Actor 方法签名统一为 `func(context.Context, *Req) (*Resp, error)`。

1. `UpsertEntries(ctx, *UpsertEntriesReq) (*UpsertEntriesResp, error)`：
   - 支持写入 `ScopeGlobal`、`ScopeNamespace`（空字符串规范化为 `"default"`）或 `ScopeRun`（绑定 `RunID`）；
   - 校验键名符合 `^[A-Za-z_][A-Za-z0-9_]*$`；同层若已存在相反敏感度（`Env` vs `Secret`）的同名键，返回明确冲突错误。
2. `DeleteEntry(ctx, *DeleteEntryReq) (*DeleteEntryResp, error)` & `DeleteRunScope(ctx, *DeleteRunScopeReq) (*DeleteRunScopeResp, error)`：
   - `sofa delete <run_id>` 时调用 `DeleteRunScope` 一键销毁该 Run 独占的临时 `Env` / `Secret`。
3. `ListMetadata(ctx, *ListMetadataReq) (*ListMetadataResp, error)`：
   - 返回指定层级的 `EnvEntry`（含 `Value`）与 `SecretMeta`（**不含 `Value`**！）。
4. `PreflightCheck(ctx, *PreflightCheckReq) (*PreflightCheckResp, error)`：
   - 输入 `Namespace`、`RunOverrides` 与 `RequiredKeys []string`，按 `Run > Namespace > Global` 计算有效键集，返回 `MissingKeys []string`（有序去重，绝不回传 Secret 值）。
5. `ResolveForNode(ctx, *ResolveForNodeReq) (*ResolveForNodeResp, error)`（**仅供控制面内部派发 Worker 调用，绝不暴露给外部 HTTP API！**）：
   - 输入 `RunID`、`Namespace`、`Keys []string`，仅返回 `Keys` 对应的合并后明文 `Values map[string]string`。

### 3.2 `.sofaplan` 全量引用提取（`plan_refs.go`）
遍历 `Plan`（含嵌套 `Module.Plan`）中所有：
- `source` 节点的 `token_ref` / `ssh_key_ref`；
- `model_provider` 节点的 `api_key_ref` / `bearer_token_ref`；
- `harness` 节点的 `env_refs` 值；
- `mcp_server` 节点的 `http.bearer_token_ref` 与 `stdio.env_refs` 值；
- `external_tool` 节点的 `env_refs` 值。
返回去重排序后的变量名切片，保证不漏检任何深层子模块引用。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`config_test.go`）**：
   - 验证 `Run > Namespace("proj-a") > Global` 三层覆盖顺序，以及 `Namespace("proj-b")` 读不到 `proj-a` 的专属变量；
   - 验证同层先设 `Secret("FOO")` 再设 `Env("FOO")` 被拒绝报错，`ListMetadata` 永远不包含任何 `Secret` 明文；
   - 构造含 `source`、`model_provider`、`harness`、`mcp_server`、`external_tool` 及嵌套 `module` 的 `Plan`，验证 `ExtractPlanEnvRefs` 100% 精确提取所有 `*_ref`。
2. **Tier 2 真实 Dapr 集成测试（`config_integration_test.go`，`//go:build integration`）**：
   - 使用 `022` `daprhost.Manager` 启动真实 `daprd`，通过 `ImplActorClientStub` 写入三层 `Env`/`Secret`、执行 `PreflightCheck` 与 `ResolveForNode`，断言 SQLite 文件中无明文 `Secret` 且重启后完整保留。
