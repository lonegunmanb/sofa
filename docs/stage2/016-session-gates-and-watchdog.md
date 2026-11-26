# 分片 016：会话协议工具（`sofa_*`）、宿主机 QC 问题账本、四道门禁、配额回滚与看门狗

## 1. 目标与边界

将宿主机 `sofa-worker` 独占的所有会话级安全控制、人工协作门禁与容错自愈能力沉淀为纯确定性、零外部守护进程依赖的核心包 `internal/session`（100% 可在 Tier 1 内存单测中毫秒级验证！）：
1. **宿主机内置协议工具（`sofa_*`）**：
   - Work 会话：`sofa_checkpoint`、`sofa_complete_work`（未配自定义 `terminate` 工具时自动挂载）、`sofa_request_decision`、`sofa_pause_for_human`、`sofa_escalate`（配置了 `agent_escalated` fallback 时挂载）；
   - QC 会话：对齐 `r42/internal/qc` 的宿主机跨轮次问题账本 `sofa_qc_open_issues`、`sofa_qc_resolve_issues`、`sofa_qc_submit_verdict`（宿主机分配稳定 `QC-001` ID，未清零前硬拒绝 `pass`）。
2. **四道宿主机硬门禁（拦截 `PreToolUse`、`PostToolUse` 与 `InvokeHostTool`）**：
   - 门禁 1：每次工具执行前及发新消息前拉取人工日记插话（带 `wait_for_confirmation = true` 时锁定非只读工具，要求回答后调 `sofa_pause_for_human`）；
   - 门禁 2：每满 `checkpoint_every_rounds` 轮强制置位 `checkpointPending`，锁死其他一切工具直至 `sofa_checkpoint` 回调成功；
   - 门禁 3：`roundComplete` 交接锁 + `activeToolCalls` 在途引用计数屏障 + `max_protocol_attempts` 协议自修重试循环；
   - 门禁 4：`tool_call_quota` 配额预扣与内置/自定义工具执行异常时的自动回滚（1:1 对齐 `r42` `OnPostToolUseFailure`）。
3. **统一重试退避（`provider.RetryPolicy`）与卡死看门狗（`StallWatchdog`）**：
   - 在宿主机层统一处理 `TRANSIENT` / `RATE_LIMIT` / `error_message_regex` 指数退避重试及 `SESSION_NOT_FOUND` 新建会话重放回退；
   - 监听流式事件与宿主工具活跃心跳，超时无进展自动触发 `Abort` → 等待在途回调归零 → `Resume` 注入续跑提示词。

**前置依赖**：分片 `014`、`015`。

---

## 2. 涉及包与文件清单

- `internal/session/diary.go`：结构化日记 `DiaryEntry`、人工插话 `HumanInterjection`、人工决策请求 `DecisionRequest` 与回调端口 `ControlHooks`
- `internal/session/qc_ledger.go`：`QCLedger`（`OpenIssues`、`ResolveIssues`、`SubmitVerdict`、`Snapshot`/`Restore`）
- `internal/session/quota.go`：`QuotaTracker`（并发安全 `Reserve` / `Rollback` / `Remaining`）
- `internal/session/gates.go`：`GateController`（四道门禁状态机、`activeToolCalls` 原子计数、轮次统计）
- `internal/session/protocol_tools.go`：全部 `sofa_*` 工具 JSON Schema 定义与宿主机 `ToolHandler` 实现
- `internal/session/watchdog.go`：`StallWatchdog` 与 `RetryExecutor`
- `internal/session/runner.go`：`RunWorkPhase` 与 `RunQCPhase` 多回合对话循环
- `internal/session/session_test.go`：Tier 1 全分支状态机单测

---

## 3. 保姆级实现细则

### 3.1 结构化日记与外部回调接口（`diary.go`）
为与阶段二检查点（`017`）及阶段三 `NodeActor` 解耦，定义窄接口：
```go
type DiaryInput struct {
    Completed      []string `json:"completed"`
    Hypotheses     []string `json:"hypotheses,omitempty"`
    Decisions      []string `json:"decisions,omitempty"`
    TestEvidence   []string `json:"test_evidence,omitempty"`
    OpenQuestions  []string `json:"open_questions,omitempty"`
    NextSteps      []string `json:"next_steps"`
}

type HumanInterjection struct {
    ID                  string `json:"id"`
    Message             string `json:"message"`
    WaitForConfirmation bool   `json:"wait_for_confirmation"`
}

type ControlHooks interface {
    DrainInterjections(ctx context.Context) ([]HumanInterjection, error)
    OnCheckpointRequested(ctx context.Context, diary DiaryInput) error // 在 017 中执行完整二阶段检查点落盘！失败则返回 error 保持 checkpointPending 不解除
    ValidateWorkspaceForHandoff(ctx context.Context) ([]string, error) // 检查 writable_paths 越界路径，非空则拒绝交卷让 Agent 自修！
}
```

### 3.2 四道门禁精确状态转换（`gates.go`）
1. **轮次定义（对齐 `README.md`）**：
   - *“一次模型响应发起的整批工具调用完成后，计为一轮。”*（即单次 `RunTurn` 中若发生了至少 1 次获准执行的非协议业务工具调用，在该 `RunTurn` 返回时 `roundsSinceCheckpoint++`；若 `roundsSinceCheckpoint >= checkpointEveryRounds`，立即置 `checkpointPending = true`！同时单次 `RunTurn` 内若连续工具调用次数超过单回合安全阈值如 `checkpointEveryRounds * 4`，也可提前置位 `checkpointPending` 防止单回合无限调 `bash` 绕过轮次统计！）。
2. **人工插话检查（每次 `PreToolUse` / `InvokeHostTool` 开头及每次 `RunTurn` 发送前）**：
   - 调用 `hooks.DrainInterjections(ctx)`：
     - 若有新插话：在 `PreToolUse` 中通过 `PreToolUseOutput.AdditionalContext` 注入（若其中任一条 `WaitForConfirmation == true`，置位 `mustPauseForHuman = true`，除只读查看工具与 `sofa_pause_for_human` / `sofa_checkpoint` 外拒绝一切修改型工具，提示先回答人类问题再调用 `sofa_pause_for_human`！）；
     - 在 `RunTurn` 发送原定提示词前若收到新插话：先单独发起一轮携带人工插话的 `RunTurn`；若带 `WaitForConfirmation == true`，原定消息挂起不发，待 Agent 调用 `sofa_pause_for_human` 后直接返回 `OutcomeWaitingHuman`！
3. **`checkpointPending` 锁**：
   - 当 `checkpointPending == true` 时，除 `sofa_checkpoint` 外拒绝所有内置工具与宿主工具（返回清晰 `deny_reason`：`"checkpoint required: call sofa_checkpoint with a structured diary entry before continuing"`）；
   - `sofa_checkpoint` 执行 `hooks.OnCheckpointRequested(ctx, diary)` 成功后，清零 `roundsSinceCheckpoint = 0` 并清除 `checkpointPending = false`。
4. **`roundComplete` 交接屏障与越界路径门禁**：
   - 当模型调用终止工具（自定义 `terminate` 工具或 `sofa_complete_work`）、`sofa_request_decision`、`sofa_pause_for_human`、`sofa_escalate` 或 `sofa_qc_submit_verdict` 时：
     - 若存在其他并发执行中的工具（`activeToolCalls > 1`），拒绝该交接调用要求等待其他工具结束后单独提交；
     - 若是 Work 终止工具或 `sofa_checkpoint`：先调 `hooks.ValidateWorkspaceForHandoff(ctx)`！若返回非空越界路径列表，**返回模型可见的业务拒绝（`Accepted: false`）列出越界路径与允许的 `writable_paths` 让模型立即 `mv` 或删除**，不置位 `roundComplete`！
     - 校验全部通过后置位 `roundComplete = true`，本回合后续任何工具调用一律硬拒绝（强制模型结束当前回复）。
   - 若单次 `RunTurn` 结束但 `roundComplete == false`：
     - 若此时 `checkpointPending == true`：下一轮自动发提示要求先调 `sofa_checkpoint`；
     - 否则视为模型未调交卷/暂停工具就结束回复（或在正常多轮思考间歇？注意：仅当模型既未调任何工具就空手停下，或耗尽回合未交卷时，`protocolAttempts++`，超过 `max_protocol_attempts` 返回协议耗尽错误；若本轮正常调了业务工具推进工作，继续下一轮提示推进直至显式调用交卷/暂停工具！）。

### 3.3 宿主机跨轮次 QC 问题账本（`qc_ledger.go`）
1. `sofa_qc_open_issues`：入参 `{ issues: [{ criterion, code, message, path, evidence, repair_hint }] }`（`criterion` 必须存在于 `qc.criteria` 键集合中！禁止模型自行传 `id`），宿主机递增分配 `"QC-001"`, `"QC-002"` 存入活跃表与历史表，返回带生成 `id` 的列表。
2. `sofa_qc_resolve_issues`：入参 `{ ids: ["QC-001", ...], resolution_evidence: "..." }`，仅允许关闭当前活跃表中的合法 ID（未知或重复 ID 报错）。
3. `sofa_qc_submit_verdict`：入参 `{ decision: "pass" | "revise", summary: "..." }`：
   - 当 `decision == "pass"` 时：若活跃未决问题数 `> 0`，**硬性拒绝**并附带剩余未决 `QC-xxx` 列表要求先逐一验证核销；
   - 当 `decision == "revise"` 时：若活跃未决问题数 `== 0`，**硬性拒绝**要求先通过 `sofa_qc_open_issues` 登记至少 1 条具体问题再打回！

### 3.4 `RetryPolicy` 与 `StallWatchdog`（`watchdog.go`）
1. **`RetryExecutor`**：
   - `OpenSession` 失败按 `Retry.LifecycleRetries` 重试；若 `resume_session_id != ""` 且返回 `ERROR_CATEGORY_SESSION_NOT_FOUND`，自动清空 `resume_session_id` 回退新建会话并注入历史结构化日记摘要！
   - `RunTurn` 失败时：仅当 `ErrorCategory` 为 `TRANSIENT` / `RATE_LIMIT` 或消息匹配 `Retry.ErrorMessageRegex`（且非 `AUTHENTICATION`/`AUTHORIZATION`/`QUOTA`/`CONTEXT_LIMIT`）时，按 `Retry.ModelCallRetries` 做指数退避 + 抖动重试。
2. **`StallWatchdog`（对齐 `r42` 双屏障）**：
   - 任何 `SessionEvent` 到达或宿主机工具处于活跃执行中（`activeToolCalls > 0` 且工具自身受其命令超时控制，或工具产生心跳）时刷新 `lastActivity`；
   - 超过 `stallTimeout` 无活动时：调用 `session.Abort(ctx)` → 等待当前 `RunTurn` 流退出且 `activeToolCalls == 0` → 调用 `session.Resume(ctx)` → 发送续跑提示词恢复执行（计入生命周期重试上限）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **四道门禁全矩阵单测**：
   - 普通插话注入上下文不锁工具；带 `wait_for_confirmation = true` 的插话锁死 `bash`/`edit`，仅放行回答与 `sofa_pause_for_human`；
   - 满 `checkpoint_every_rounds` 轮后调 `bash` 被拒，调 `sofa_checkpoint`（且 `OnCheckpointRequested` 返回 `nil`）后解锁；若 `OnCheckpointRequested` 报错则保持锁定；
   - 交卷时 `ValidateWorkspaceForHandoff` 发现越界文件返回模型可见拒绝，模型第二轮修复清空越界文件后交卷成功；交卷成功后同一回合紧跟的第二个工具调用被 `roundComplete` 拒绝；
   - `tool_call_quota = { bash = 2 }`：第 1 次成功（剩 1），第 2 次 `PostToolUse(succeeded=false)` 自动回滚（仍剩 1），第 3 次成功（剩 0），第 4 次在 `PreToolUse` 被配额门禁拒绝！
2. **QC 跨轮次账本防遗忘单测**：
   - 第 1 轮开出 `QC-001`、`QC-002` 并提交 `revise`；第 2 轮只核销 `QC-001` 就尝试调 `pass` 被宿主机硬拒绝；核销 `QC-002` 后再调 `pass` 成功通过；空账本调 `revise` 被拒。
3. **重试与看门狗自愈单测**：
   - 模拟 `RATE_LIMIT` 退避重试成功、`AUTHENTICATION` 立即永久失败不重试、`SESSION_NOT_FOUND` 自动降级新建会话；
   - 模拟 `RunTurn` 挂死超过 `stallTimeout`（使用短测试阈值如 `50ms`），验证自动触发 `Abort` → 等待在途工具归零 → `Resume` → 续跑完成。
