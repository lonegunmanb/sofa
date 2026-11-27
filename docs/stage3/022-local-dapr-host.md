# 分片 022：本地 Dapr 守护托管与 `primaryEncryptionKey` 透明加密基座（`internal/daprhost`）

## 1. 目标与边界

为 `sofa server --dev` 单机全栈模式以及后续所有 Actor 切片（`023`–`029`）的真实 Tier 2 集成测试，提供零外部容器依赖的本地 Dapr 运行时托管器：
1. **自动发现本地 Dapr 二进制**：按显式配置路径 > `~/.dapr/bin/{scheduler,daprd}`（Windows 自动兼容 `.exe`）> `PATH` 查找 `scheduler` 与 `daprd`。
2. **原生透明落盘加密配置生成**：
   - 在 `<stateDir>/keys/master-key.json` 首次原子生成随机 32 字节（64 位 hex）主密钥并设置严格文件权限（Unix `0600` + Windows 当前用户独占 DACL）；已存在时直接复用，确保跨重启可解密历史状态！
   - 在 `<stateDir>/components/` 自动生成三个组件 YAML：
     - `local-secrets.yaml`（`secretstores.local.file`，指向 `master-key.json`）；
     - `statestore.yaml`（`state.sqlite`，`actorStateStore: "true"`，`connectionString: file:<stateDir>/actor-state.db`，并通过 `primaryEncryptionKey` `secretKeyRef` 绑定上述主密钥，由 `daprd` 原生执行客户端 AES-GCM 加密！）；
     - `pubsub.yaml`（`pubsub.in-memory`，组件名固定 `"sofa-pubsub"`，Topic 固定 `"sofa-events"`）。
3. **双进程托管与端口隔离**：
   - 启动单实例 `scheduler --mode standalone --etcd-embed --etcd-data-dir <stateDir>/scheduler --placement-enabled`（同时提供持久化 Actor Reminder 与 Actor Placement！）；
   - 启动 `daprd` 连接本地应用的内部回调端口 `127.0.0.1:<appPort>`，双向注入一次性随机 `APP_API_TOKEN` 与 `DAPR_API_TOKEN` 防本机未授权进程直调 `daprd` 或伪造回调。

**前置依赖**：无（独立基础包，并引入 `github.com/dapr/go-sdk` 至主仓 `go.mod`）。

---

## 2. 涉及包与文件清单

- `internal/daprhost/components.go`：密钥文件原子创建/权限收紧与 Dapr 组件 YAML 渲染
- `internal/daprhost/manager.go`：`Manager`（`Start`、`Client`、`Stop`）、动态空闲端口分配、健康就绪等待与进程树优雅回收
- `internal/daprhost/components_test.go`：Tier 1 组件 YAML 渲染、主密钥幂等复用与权限校验单测
- `internal/daprhost/manager_integration_test.go`（`//go:build integration`）：Tier 2 拉起真实 `scheduler` + `daprd`，验证加密 Actor State 跨重启持久化与 Reminder 准时触发

---

## 3. 保姆级实现细则

### 3.1 核心 API（`internal/daprhost/manager.go`）
```go
package daprhost

type Options struct {
    StateDir      string // 必填：持久化根目录
    AppID         string // 如 "sofa-dev" 或 "sofa-worker-<id>"
    AppPort       int    // 本地 dapr/go-sdk 内部回调监听端口 (127.0.0.1:<AppPort>)
    AppAPIToken   string // 为空时自动生成 32 字节随机 token
    SchedulerBin  string // 可选覆盖
    DaprdBin      string // 可选覆盖
}

type Manager struct { ... }

func NewManager(opts Options) (*Manager, error)
func (m *Manager) AppAPIToken() string
func (m *Manager) DaprAPIToken() string
func (m *Manager) GRPCAddress() string // "127.0.0.1:<daprGRPCPort>"
func (m *Manager) Start(ctx context.Context) (daprclient.Client, error)
func (m *Manager) Stop(ctx context.Context) error
```

### 3.2 关键防坑细则
1. **启动顺序**：应用进程必须**先启动内部 `127.0.0.1:<AppPort>` HTTP 服务**（注册好 Actor Factory 以响应 `GET /dapr/config`），再调用 `Manager.Start` 拉起 `scheduler`（等待其健康端口就绪）→ 拉起 `daprd`（等待 `/v1.0/healthz/outbound` 返回 `204/200` 并校验 Actor Runtime Placement 就绪）。
2. **端口冲突规避**：`scheduler` 的 gRPC 端口、嵌入式 etcd 客户端/peer 端口、healthz/metrics 端口，以及 `daprd` 的 HTTP/gRPC/internal-gRPC/metrics 端口，默认全部在 `127.0.0.1` 动态挑选空闲端口，保证单机并跑多个集成测试互不抢端口！
3. **验证原生加密生效**：在 Tier 2 集成测试中，向 Actor 写入一段唯一明文标记串（如 `"SUPER_SECRET_CANARY_VALUE_9918"`），落盘后直接读取 `<stateDir>/actor-state.db` 原始字节断言**不包含该明文子串**，且重启 `Manager` 后通过 Actor 仍能完整读回原值！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`components_test.go`）**：
   - 首次调用生成 64 位 hex 主密钥且二次调用原样保留不覆盖；
   - 生成的 `statestore.yaml` 含 `actorStateStore: "true"`、`primaryEncryptionKey` `secretKeyRef`，路径在 Windows 斜杠下合法转义。
2. **Tier 2 真实 `scheduler` + `daprd` 联调（`manager_integration_test.go`，`//go:build integration`）**：
   - 注册测试 `ServerImplBaseCtx` Actor + `ReminderCallee`：写入状态 → 校验 SQLite 文件中无明文泄漏 → 验证 Reminder 在 200ms 内触发 → `Stop` 后在同一 `StateDir` 重新 `Start` 验证状态与跨重启 Reminder 100% 恢复！
