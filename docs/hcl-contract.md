# sofa HCL 语言块契约（Frozen Draft）

本文档记录经设计与访谈确认的 **sofa** HCL 语言块契约，作为后续 `specs/<###-feature>/` 细化规范的统一基线。配置文件后缀为 `*.sofa.hcl`，变量文件为 `sofa.sofavars` / `*.auto.sofavars`，环境变量前缀为 `SOFA_VAR_`，所有块名与字段名严格使用 `lower_snake_case`。

---

## 1. 全局解析与默认推导规则

1. **直接复用 `github.com/lonegunmanb/golden`**：
   - 目录内所有 `*.sofa.hcl` 共享配置作用域，无顶层 `workflow {}` 包裹块。
   - 块注册、解码、`variable` / `locals` / `for_each` / `depends_on` / `precondition` 求值以及无环 DAG 构建全部委托给 `golden`（`AsHclBlocks`、`InitConfig`、`BaseBlock`、`PlanBlock`）。
2. **Root Module 单例自动默认（0 / 1 / ≥2 规则）**：
   - 不设显式 `defaults {}` 块；对 `sandbox` 与 `model_provider` 按 Root Module 中的声明数量推导：
     - **0 个**：`sandbox` 在 Plan 期报错（所有 Session 必须在容器沙箱内运行）；`model_provider` 回退使用 `copilot-sdk/go` 默认提供方。
     - **恰好 1 个**：自动作为全局默认绑定给未显式声明该字段的 `worker`、`dynamic_worker`（`content`）及未覆盖的子 `module`，并在 `InitConfig` 阶段自动补齐隐式 DAG 依赖边。
     - **≥ 2 个**：存在歧义，未显式声明的消费块在 Plan 期报错。
   - `worker.based_source` **始终必填**（不参与默认推导，防止漏写静默断开前序工序链路）；`preserve.url` 在 Root 仅有 1 个 `source` 时可省略并自动关联。
3. **结构化输出与非 Git 产物交接（对齐 `r42`）**：
   - 机械校验与结构化终止输出通过 `go_tool` / `external_tool` + `terminate_tool_id`（或 `tool_use { terminate = true }`）完成，结果暴露在 `worker.<name>.result`（HCL 内消费结构时配合 `jsondecode` 或保类型的 `jsondecodewithtype`）。
   - 跨工序、跨机器供下游读取的非 Git 文件或目录，必须由流程作者通过 `artifact "<name>"` 显式声明，下游通过 `import_artifact "<name>"` 导入授权后读取。

---

## 2. 顶层块清单

### 2.1 与 `r42` 1:1 对齐复用的块

- `variable "<name>"`：必填 `type`，可选 `description`、`default`、`sensitive`、`validation`。
- `locals`：局部派生值（`local.<name>`）。
- `output "<name>"`：`value`、可选 `description`、`sensitive`。
- `module "<name>"`：`source`（本地相对目录或 `go-getter` 地址）、可选 `parallelism`、`timeout` 及子模块输入变量。
- `model_provider "<name>"`：`type`（`openai` | `azure` | `anthropic`）、`endpoint`、可选 `wire_api`、`transport`、`headers`、`api_key` / `api_key_ref` / `bearer_token` / `bearer_token_ref`（至多其一）、可选嵌套 `retry`。
- `mcp_server "<name>"`：`tools`、可选 `resources`、`timeout`，以及必选其一的 `http` 或 `stdio` 嵌套块；对外暴露 `.tool_ids` 与 `.resource_ids`。
- `go_tool "<name>"`：`description`、`source`、可选 `dependencies`、`postcondition`；对外暴露确定性 `.id`。
- `external_tool "<name>"`：`description`、`program`、`input_type`、`output_type`、可选 `working_dir`、`postcondition`；对外暴露确定性 `.id`。

---

### 2.2 `sofa` 专属顶层块

#### 1) `source "<name>"` 与 `preserve "<name>"`

```hcl
source "project" {
  url         = var.repository_url
  branch      = var.source_branch
  token_ref   = "GITHUB_TOKEN" # 可选：Apply 期由宿主机 sofa-worker 读取，绝不注入容器
  ssh_key_ref = null           # 可选：与 token_ref 互斥
}

preserve "final_implementation" {
  url    = source.project.url # Root 仅有 1 个 source 时可省略
  branch = worker.implement.work_branch
}
```

#### 2) `sandbox "<name>"`

```hcl
sandbox "go_env" {
  runtime    = "podman" # "podman" | "docker" | "nerdctl" | "kirk"
  image      = "ghcr.io/lonegunmanb/sofa-go:1.27"
  entrypoint = null
  network    = "none"

  mount {
    source    = "/var/cache/sofa/gomod"
    target    = "/go/pkg/mod"
    read_only = true
  }

  resources {
    cpus   = "4"
    memory = "8g"
  }
}
```

#### 3) `worker "<name>"`

支持 `for_each`、`depends_on`、`precondition`。对外只读暴露 `.work_branch`、`.commit_sha`、`.result`（配置终止工具时）、`.artifact.<name>`。

```hcl
worker "implement" {
  based_source   = worker.tasks.work_branch
  sandbox        = sandbox.go_env           # Root 单例时可省略
  writable_paths = ["cmd/**", "internal/**", "go.mod", "go.sum"]

  model_provider          = model_provider.openrouter # Root 单例时可省略
  model                   = var.model
  profile                 = null
  reasoning_effort        = "high"
  system_prompt           = "严格遵循 TDD Red-Green-Refactor 实现任务。"
  prompt                  = "读取 docs/tasks.md 与 ${import_artifact.design.path} 完成实现。"
  timeout                 = "2h"
  permission              = "approve_all"
  max_protocol_attempts   = 10
  checkpoint_every_rounds = 5
  human_approval          = "required" # "optional"(默认) | "required" | "disabled"

  tool_ids          = [go_tool.verify_task_done.id]
  mcp_tool_ids      = []
  mcp_resource_ids  = []
  terminate_tool_id = go_tool.verify_task_done.id # 或使用嵌套 tool_use 块
  tool_call_quota   = { bash = 100 }
  allowed_tools     = null
  disallowed_tools  = null
  skill_directories = ["${path.module}/skills"]
  skills            = ["golang-tdd"]
  disabled_skills   = []

  retry {
    lifecycle_retries    = 10
    model_call_retries   = 5
    interval_seconds     = 10
    max_interval_seconds = 180
  }

  import_artifact "design" {
    desc    = "上游获批的技术设计方案"
    sources = [worker.plan.artifact.design]
    path    = ".sofa/imports/design.md" # 可选：消费方工作区内只读相对路径
  }

  artifact "test_report" {
    type        = "file"
    path        = "reports/test-summary.json"
    description = "测试摘要"
    required    = true
    non_empty   = true
  }

  # 确定性守门员：在冻结后的容器内部执行命令，校验真实退出码与干净工作树
  gatekeeper "unit_test" {
    command                = ["go", "test", "./...", "-count=1"]
    working_dir            = "."
    timeout                = "10m"
    expect_exit_code       = 0
    require_clean_worktree = true
  }

  # 独立容器 QC Session（默认继承所属 worker 的 model_provider / model / sandbox）
  qc {
    model            = "claude-opus-4-6"
    reasoning_effort = "high"
    max_qc_rounds    = 3

    criteria = {
      both_directions = "验证关闭通知不发送且开启通知仍正常发送。"
    }
  }

  # 显式跨节点受控返工（target 必须为正向 DAG 中的祖先节点）
  fallback "rework_tasks" {
    on          = ["qc_exhausted", "gatekeeper_exhausted", "human_returned_upstream"]
    target      = worker.tasks
    max_reworks = 2
    artifacts   = [artifact("test_report")]
  }
}
```

#### 4) `dynamic_worker "<name>"`

运行时按上游确定后的映射展开独立节点与分支（空映射不生成节点或分支）；对外暴露 `.workers` 与 `.work_branches`：

```hcl
dynamic_worker "verify_notifications" {
  parallelism = 4
  serial      = false

  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(string) })
    ).checks
    iterator = check

    content {
      based_source  = worker.discover.work_branch
      model         = var.model
      system_prompt = "完成单项邮件通知验证。"
      prompt        = "验证 ${check.key} 满足：${check.value}。"

      gatekeeper "test" {
        command = ["go", "test", "./..."]
      }
    }
  }
}
```

#### 5) `merge "<name>"`

纯确定性串行集成工序（不内置改业务代码的冲突修复 Agent，采用**严格源头负责制**）：若候选分支 rebase 冲突或未通过 `gatekeeper` 重测，丢弃临时树，将原上游 `worker` 新一轮基线升级至当前集成分支最新 SHA 并附带报错证据精准退回原 `worker` 修复；合入成功后禁止回滚，仅允许向前修复。

```hcl
merge "integrate" {
  based_source   = worker.discover.work_branch
  target_branch  = null # 可选：省略时创建本 Run 独占的受管 work_branch；指定时合入对应目标分支
  sources        = dynamic_worker.verify_notifications.work_branches
  strategy       = "rebase"
  max_reworks    = 2
  human_approval = "optional"

  gatekeeper "regression" {
    command = ["go", "test", "./...", "-count=1"]
  }
}
```
