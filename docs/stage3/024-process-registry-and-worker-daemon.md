# 分片 024：`ProcessRegistryActor` 槽位调度、三态存活探测与 Worker 守护宿主

## 1. 目标与边界

落地工作进程注册中心（`ProcessRegistryActor`）与宿主机执行守护模块（`internal/worker/daemon`），解决分布式存活容错、并发槽位防雪崩与僵尸进程自隔离：
1. **三态健康机（`HEALTHY` → `SUSPECT` → `DEAD`）**：
   - 每个 Worker 进程启动生成唯一 `process_id`（Dapr App ID `sofa-worker-<process_id>`；`--dev` 下复用合体进程 App ID），携带 `MaxConcurrency`（独立 `sofa worker` 默认 **`10`**，`sofa server --dev` 默认 **`2`**）与目标容器平台（`linux/amd64` 或 `linux/arm64`）向 `ProcessRegistryActor("global")` 注册并定期心跳；
   - 心跳超时或单次派发调用失败进入 `SUSPECT`（暂停派发新任务，不立即杀任务），由 Actor Reminder 连续主动调用 Worker `ping` 探测；宽限期内恢复则回 `HEALTHY`，连续失败超限则转 `DEAD` 并回调通知相关租约持有方（`NodeActor` / `MergeActor`）递增 `epoch` 换机接管！
2. **并发槽位租约（Lease-Based Slot Allocation）**：
   - `ClaimSlot(ctx, req)` 仅从 `HEALTHY` 且 `ActiveSlots < MaxConcurrency`（并匹配所需容器架构）的 Worker 中挑选**空闲槽位比例最高 / 负载最低**的实例分配 `LeaseID`，全部满载时返回 `Granted: false` 供 `RunActor` / `MergeActor` 非阻塞排队等待；任务结束、取消或进程 `DEAD` 时幂等释放槽位。
3. **Worker Dapr 服务宿主与自清理**：
   - 暴露内部 Dapr Service Invocation 端点：`ping`、`start_node`（异步后台拉起 `018` `WorkerRunner` 立即返回 `202`）、`abort_node`（按 `(run_id, node_addr, epoch)` 立即取消在途上下文销毁容器）、`run_merge_candidate`（异步执行 `018` `ExecuteMergeCandidate`）；
   - **日志双路处理**：实时将节点事件发布到 Dapr PubSub（`sofa-pubsub` / `sofa-events`），同时写入本地缓冲并在每次检查点及节点退出时归档上传 `BlobStore`（`runs/<run_id>/logs/<node_key>/e<epoch>.jsonl`）；
   - **失联自隔离**：心跳收到 `Status == DEAD`（或未知 `process_id`）响应时，立即取消本进程所有在途任务、销毁关联容器，生成全新 `process_id` 重新注册！

**前置依赖**：分片 `018`、`022`。

---

## 2. 涉及包与文件清单

- `internal/actor/registry/types.go`：`ProcessStatus`、槽位租约请求/响应、死信通知接口
- `internal/actor/registry/actor.go`：`ProcessRegistryActor`（ID `"global"`，实现 `actor.ServerContext` 与 `actor.ReminderCallee`）
- `internal/worker/daemon/daemon.go`：Worker Dapr Invocation 处理器、在途任务表、JSONL 日志流式落盘与失联自隔离循环
- `internal/actor/registry/registry_test.go` & `internal/worker/daemon/daemon_test.go`：Tier 1 状态机、最少负载槽位分配与失联自清理单测
- `internal/worker/daemon/daemon_integration_test.go`（`//go:build integration`）：Tier 2 真实 `daprd` + `Reminder` 驱动 `HEALTHY → SUSPECT → DEAD` 探测与跨进程 Service Invocation 测试

---

## 3. 保姆级实现细则

1. **槽位分配幂等性**：`ClaimSlotReq{LeaseKey, OwnerActorType, OwnerActorID, RequiredArch}`——若同一 `LeaseKey`（如 `<run_id>/<node_addr>/e<epoch>`）重复请求且原分配 Worker 仍存活，幂等返回原 `LeaseID` 与 `AppID`，防网络重试双扣槽位！
2. **非阻塞异步派发**：`NodeActor` / `MergeActor` 调 Worker 的 `start_node` / `run_merge_candidate` 必须在毫秒内返回（Worker 登记进本地活跃任务表并启动后台 goroutine 执行），执行完毕后由 Worker 携带 `(epoch, lease_id)` 回调对应 Actor 提交结果并调 `ReleaseSlot`！
3. **可配置时间参数**：心跳间隔（默认 `5s`）、`SUSPECT` 阈值（默认 `15s`）、主动探测周期（默认 `3s` × 连续 `3` 次失败判 `DEAD`）支持在测试中缩短至百毫秒级，使 Tier 2 集成测试在 2 秒内跑完完整故障切换！

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测**：
   - 两个 Worker（`max=10, active=5` vs `max=10, active=1`）竞争时优先分配负载更低者；满载时返回 `Granted: false`；`ReleaseSlot` 重复调用幂等；
   - 心跳超时转 `SUSPECT` 后不再接新单；探测成功恢复 `HEALTHY`；连续探测失败转 `DEAD`、清空其全部活跃槽位并触发持有者失联回调；
   - Worker 心跳发现自身被判 `DEAD` 时立即取消本地运行中任务并轮换新 `process_id`。
2. **Tier 2 真实 Dapr 测试（`//go:build integration`）**：
   - 在真实 `daprhost` 上启动 `ProcessRegistryActor` 与两个模拟 Worker 服务，主动停掉其中一个 Worker 的响应，验证 Reminder 自动完成 `HEALTHY → SUSPECT → DEAD` 流转并触发接管通知。
