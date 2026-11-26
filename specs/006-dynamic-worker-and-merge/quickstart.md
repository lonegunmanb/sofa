# Quickstart: Planning `dynamic_worker` and `merge`

```hcl
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

go_tool "discover_checks" {
  description = "Return checks JSON"
  source      = "..."
}

worker "discover" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Discover checks."
  terminate_tool_id = go_tool.discover_checks.id
}

dynamic_worker "verify_notifications" {
  parallelism = 4
  serial      = false

  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(object({ rule = string })) })
    ).checks
    iterator = check

    content {
      based_source  = worker.discover.work_branch
      model         = "gpt-5"
      system_prompt = "Verify rule."
      prompt        = "Check ${check.key}: ${check.value.rule}"

      gatekeeper "test" {
        command = ["go", "test", "./..."]
      }
    }
  }
}

merge "integrate" {
  based_source = worker.discover.work_branch
  sources      = dynamic_worker.verify_notifications.work_branches

  gatekeeper "regression" {
    command = ["go", "test", "./...", "-count=1"]
  }
}

worker "post_merge" {
  based_source  = merge.integrate.work_branch
  model         = "gpt-5"
  system_prompt = "Post-merge verification."

  fallback "rework_discover" {
    on     = ["gatekeeper_exhausted"]
    target = worker.discover
  }
}

preserve "final" {
  branch = worker.post_merge.work_branch
}
```
