# sofact

**黑灯工厂**是一个面向软件生产的系统：使用声明式 HCL 定义流程，以 Dapr Actor 分布式执行，支持人工决策，并能从跨机器持久化的检查点恢复。它既可以独立接受人类任务，也可以作为 The Machine 制造软件工具的执行能力。

名称尚未最终确定，`cuo · 搓` 是候选名称。

> **设计状态：** 本 README 描述当前设计概要，不代表所有能力已经实现。具体 HCL 字段、API 路径和状态机细节仍需在实现规范中冻结。

## 设计目标

- 以可审查的 DAG Plan 表达软件生产流程，支持人类审查后提交执行。
- 将流程、节点、Session、检查点和产物作为可追溯的持久状态管理，而不是依赖单次进程或消息。
- 让同一流程中的不同节点可以在不同机器上运行，并在故障后从最后一个完整检查点接管。
- 通过运行时、容器和挂载机制落实文件系统权限，不依赖提示词约束。
- 将质检、确定性门禁、人工决策和 Git 交付纳入执行流程。

## 声明式流程与 Plan

一个目录就是一个 workflow，目录中的 HCL 文件共享配置作用域，不需要用 `workflow` 块包裹全部内容。所有块名都使用小写蛇形命名（lower snake case）。流程用 `variable` 接收需求、仓库和模型等输入，用 `worker` 描述需求分析、架构设计、开发或测试等工序，用可嵌套的 `module` 封装可复用流程。

引用关系和显式依赖共同生成 DAG Plan。配置依赖必须无环；实际执行可通过显式声明的 fallback 携带失败报告和其他材料返回前序工序，形成受控返工循环。

顶层 `source` 块声明代码来源。可以另写零个或多个 `preserve` 块，每个块用 `url` 和 `branch` 指定清理时要保留的分支。例如，保留下文实现工序的工作分支：

```hcl
source "project" {
  url    = var.repository_url
  branch = var.source_branch
}

preserve {
  url    = var.repository_url
  branch = worker.implement.work_branch
}
```

每个 `worker` 都必须用 `based_source` 指定起始分支，可以引用 `source.project.branch`，也可以引用前序工序的 `work_branch`。运行时将起始版本固定为提交 SHA，为每个实例分配独有的 `work_branch`，而不是在起始分支上直接修改。静态和动态生成的 `worker` 都遵循同一顺序：先在全局登记该 Run 所属的工作分支，登记成功后才从固定提交创建分支。Session 产生候选变更，可信运行时校验并推送。

Run 结束时（无论成功或出错）或任务被删除时，运行时根据登记记录清理本次 Run 创建的中间分支。`source` 分支及匹配任一 `preserve` 的分支不清理；未登记为本次 Run 资源的其他分支不在清理范围内。

## 分布式执行

分布式模式以 Dapr Actor 为基础。一个 workflow 的不同 DAG 节点可以调度到不同机器：

| 组件 | 职责 |
| --- | --- |
| 全局固定身份的 Operator Actor | 管理 Run、推进 DAG、调度节点、核对执行状态并恢复 |
| DAG 节点 Actor | 管理工序执行、Work／QC Session、检查点和结果 |
| 合并 Actor | 对指定目标分支串行集成、处理冲突并重新测试 |
| ProcessRegistryActor | 登记工作进程、启动身份、服务地址和存活状态 |
| 工作进程服务 | 在宿主机启动或停止容器，并报告本机执行情况 |

节点完成后会通知 Operator，由 Operator 找出下一批可执行节点。通知不能是唯一的推进依据：Actor 持久状态与 reminder 需要支持恢复和状态核对。Operator 的调度操作应保持短调用，不能被长时间运行的 Agent 工作阻塞。

每个工作进程启动时生成新的 `process_id`，建立可寻址的 Dapr 服务入口后再注册，并持续报告存活状态。一次通信失败只表示进程可疑，不能直接断定宿主机死亡。Operator 记录节点所属进程和执行轮次；故障接管会创建新轮次，旧执行随即失去更新权威检查点和发布结果的资格。重复派发和重复完成通知应幂等处理。

不强制要求 Kubernetes，也支持手动启动 Dapr sidecar。

## 工序、Session 与隔离

一个 `worker` 可以描述小任务，也可以承载一个 spec 的完整开发过程。其配置参考 [r42 的静态研究工序（`static_research`）](https://github.com/lonegunmanb/r42#example)：可选择模型提供方（`model_provider`）和模型（`model`），设置重试策略（`retry`）、思考强度（`reasoning_effort`）、可用工具（`tools`）及 MCP 提供方（`mcp_providers`）。

同一 `worker` 可配置 Work Session 和独立 QC Session。两者由同一节点 Actor 管理，在同一机器上的独立容器中运行，并挂载同一个项目工作卷。QC 检查期间冻结待审候选，避免审查过程中代码继续变化。

节点只有通过配置的质检与确定性门禁才能发布成功结果。报告中写“通过”不能替代真实命令退出码、测试记录和代码版本校验。

通过 `sandbox` 配置镜像、运行时、挂载和入口；容器运行时可适配 Docker、Podman、Kirk 和 nerdctl，当前示例使用 Podman。工厂程序及 Agent 运行时挂入容器，由 entrypoint 读取分配的任务。项目工作区按 Session 配置可读写范围；内置文件工具和 shell 命令受相同限制。

除明确共享的项目卷和只读运行时材料外，其他文件系统必须隔离、各自私有，不要求预先枚举第三方工具会使用的临时目录、缓存目录或用户目录。不同节点使用独立的项目工作副本，通过 Git 和 Artifacts 交接。

## 检查点与恢复

每个 Work／QC Session 都有独立日记。一次模型响应发起的整批工具调用完成后，计为一轮。每满 N 轮、进入人工等待或正常结束工序时，运行时都要求保存完整检查点；保存成功后才能继续。

日记是 Session 主动编写的中期记忆，记录自上一篇以来的完成事项、发现与验证过的假设、决策依据、测试证据、未解决问题和下一步计划。它不同于完整的消息流，并属于稳定的逻辑 Session，而不是临时容器。

人类可以随时在日记中向当前 `worker` 插话。Session 在**每次工具调用前**检查日记，将属于该 `worker` 的新插话作为 `user message` 加入上下文。系统向 Session 发送 `user message` 前也要检查：有新插话时，先投递人工插话，再处理原定消息。

提问时，人类可以勾选“回答后等待确认”。收到带此标记的提问后，Agent 必须先回答，再调用指定的暂停工具，使 Session 进入人工等待。原定消息不能触发继续执行；只有人类给出下一步指示后，Session 才能继续。普通插话既不要求暂停，也不等于批准。

检查点由运行时维护，流程作者无需在 HCL 中声明。

保存检查点时，先写日记，记录当前 Git 提交的 SHA；再提交变更并将检查点分支推送到远端。若提交产生了新 SHA，须在日记中追加该条目对应的最终 SHA，不能把提交前的 SHA 当作已推送的版本。确认远端分支与日记记录一致、日记和恢复元数据已写入共享持久存储、所需非 Git 文件也已上传后，才能将检查点标记为完整。

恢复时，先核对最新日记记录的 SHA 与远端检查点分支的最新 SHA。若二者不符，舍弃不匹配的恢复候选，沿检查点序列向前查找，直到找到日记与远端分支历史中的提交对齐的最近完整检查点。找不到就不能据此恢复；仅保存在本机或尚未完整的检查点也不能使用。

新机器从选定的检查点取得代码、输入材料和日记流，以日记作为历史记忆继续执行。原始消息流作为按需查询的证据保留，无须全量重放。未保存的进度可能需要重做，因此外部操作还需具备幂等性，或在恢复时进行核对。

## 合并与 Artifacts

显式 `merge` 工序负责汇集并行开发结果。合并请求通过持久队列接收，不能依赖长时间等待的 HTTP 请求排队；已接收的请求不得因 Actor 正忙而静默丢弃。合并 Actor 针对目标分支串行执行 rebase、冲突处理和重新测试。

尚未成功合入共享集成分支时，可以附带证据退回开发；合入成功后采用向前修复。推送自己的检查点分支不等于已合入共享分支。

`worker` 可以用 HCL Object 直接输出结构化数据。输出一个路径并不会传输文件：要让下游读取设计文档等中间产物，要么把文件提交到 Git 仓库的约定路径，让下游按提交 SHA 和仓库相对路径读取；要么在 `artifact` 块中声明文件或目录，让下游通过 `import_artifact` 导入后按本地路径读取。

全局 Artifact 注册表按 Run 管理文件和目录。上传后取得 Artifact ID；`artifact` 声明产物，`import_artifact` 声明导入材料及依赖。提示词通过插值明确告知 Agent 应读取的 Artifact 及内容。跨机器传递 Artifact ID，不依赖生产者的本地路径；已发布的产物版本不可变，消费者绑定确定版本。

流程完成或删除时清理临时 Artifacts。需长期保留的交付包、报告和决策依据必须保存到明确的永久位置。

## 人工决策

任何 Work／QC Session 都可以请求人工介入，HCL 也可以声明必须经过的人类决策点。决策形式包括选项选择、自定义答案、自由问答、追问和要求补充探索。Agent 应先提交上下文、方案、证据及推荐理由，再由人类明确决定后继续。

等待人工是正常执行状态。问题和材料绑定具体版本及检查点，等待期间可以释放容器。普通讨论不构成批准；材料发生变化后，不能沿用旧版本的批准。

## API、看板与任务生命周期

单机与分布式模式提供一致的管理接口，看板、CLI 和外部系统共用 API。HTML 看板用于查看流程列表与 DAG 状态、节点详情、日志、输出、检查点和重跑操作，并集中展示人工决策卡片，包括上下文、材料预览、讨论记录和最终决定。

人类可以在决策卡片中要求补充实验；Agent 完成后更新材料并再次等待。回答先可靠保存，再由 Operator 推进。Run 被手动暂停时，提交回答不会自动解除暂停。Agent 生成的 HTML 原型和其他材料必须隔离展示，不能获得看板身份或代替人类提交决定。

`apply` 始终表示提交任务；提交后通过 CLI、API 或看板管理：

- **单机模式：** 可将状态保存在指定文件夹，同时保持相同的流程与恢复语义。
- **分布式模式：** 基于 Dapr Actor，并尽量使用 Dapr 的状态与消息能力。
- **计划暂停：** 当前工序继续，但不启动新的 DAG 节点。
- **立即暂停：** 停止当前执行并舍弃未保存进度。
- **手动重跑：** 可针对运行中或报错节点；必须隔离旧执行，并处理受影响的下游结果。
- **删除任务：** 停止并隔离残留执行，清理任务所属资源，保留 `source` 分支和 `preserve` 指定的分支。

## 示例：从规格到代码

下面把 Anil Bapat 的[规格驱动开发工作流](https://www.linkedin.com/pulse/spec-driven-development-smarter-way-build-ai-anil-bapat-ern7c/)映射为四个 `worker`。以“用户可以关闭邮件通知”为例，先确认用户可观察的行为，再决定如何实现。

以下是 HCL 风格的设计示例，字段尚未定稿。规格和任务清单随 Git 提交交接；设计文档不进仓库，作为 Artifact 交接。前三道工序各自等待人类审查当前文档，下游只读取获批版本。`based_source` 指定新工作分支的基线，`import_artifact` 则按上游产物版本导入文件；示例中的 `import_artifact.design.path` 是文件在**消费方**的路径。

```hcl
worker "specify" {
  based_source = source.project.branch
  prompt       = "根据需求 ${var.requirement}，说明谁可以关闭通知、关闭后哪些邮件不再发送。将规格保存到仓库的 docs/spec.md。"
  output       = { spec_path = "docs/spec.md" }
}

worker "plan" {
  based_source = worker.specify.work_branch
  prompt       = "读取仓库中的 ${worker.specify.output.spec_path}，设计偏好存储、发送前的检查及测试方案，将设计保存到 ${artifact("design").path}。"

  artifact "design" {
    type = "file"
    path = "plan.md"
  }
}

worker "tasks" {
  based_source = worker.specify.work_branch
  import_artifact "design" {
    sources = [worker.plan.artifact.design]
  }
  prompt = "读取导入的设计文档 ${import_artifact.design.path}，按依赖拆成小任务，为每项列出验收条件，将清单保存到仓库的 docs/tasks.md。"
  output = { tasks_path = "docs/tasks.md" }
}

worker "implement" {
  based_source = worker.tasks.work_branch
  prompt       = "读取仓库中的 ${worker.tasks.output.tasks_path}，按依赖顺序逐项实现。每项交给 QC 验证，测试通过且人类批准该提交后才开始下一项。"
}
```

例如，“保存通知偏好”和“发送前检查偏好”可以分开审查。后一项不仅要证明关闭后不发送，还要验证开启时仍会发送。QC 失败就带着测试记录返工；通过后，人类审查该候选的小范围变更，再以获批提交为下一项的基线。

### 按上游结果生成工序

如果验收项的数量只有在上游完成后才知道，可以声明 `dynamic_worker`。例如，上游 `worker.discover` 检查代码，将受影响的邮件类型及其验收条件作为映射 `output.checks` 发布。下面的 `dynamic "worker"` 用 `for_each` 遍历该映射：每个条目生成一个独立的 `worker`，空映射则不生成任何节点或分支。

```hcl
dynamic_worker "verify_notifications" {
  dynamic "worker" {
    for_each = worker.discover.output.checks
    iterator = check
    content {
      based_source = worker.discover.work_branch
      prompt       = "验证 ${check.key} 类型的邮件是否满足以下条件：${check.value}。运行对应测试并记录结果。"
    }
  }
}
```

## 可观测与追溯

数据按 Run、节点、Session 和执行轮次组织，并按日志级别记录输入、模型响应、工具调用、命令执行、测试输出和状态变化。日记、人工决策和发布记录也是正式数据。

最终应能追溯每份软件基于什么需求、由哪些节点完成、经过哪些验证、人类作出了哪些决定，以及执行从哪个检查点恢复。
