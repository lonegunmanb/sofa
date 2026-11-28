source "repo" {
  url       = var.repo_url
  branch    = var.base_branch
  token_ref = var.git_token_ref
}

sandbox "go_dev" {
  runtime = "podman"
  image   = var.work_sandbox_image

  dynamic "mount" {
    for_each = var.go_mod_cache_dir != null ? [var.go_mod_cache_dir] : []
    content {
      source    = mount.value
      target    = "/go/pkg/mod"
      read_only = true
    }
  }
}

sandbox "qc_audit" {
  runtime = "podman"
  image   = var.qc_sandbox_image

  dynamic "mount" {
    for_each = var.go_mod_cache_dir != null ? [var.go_mod_cache_dir] : []
    content {
      source    = mount.value
      target    = "/go/pkg/mod"
      read_only = true
    }
  }
}

harness "copilot" {
  env_refs = merge(
    var.copilot_github_token_ref != null ? {
      GITHUB_TOKEN = var.copilot_github_token_ref
    } : {},
    var.copilot_proxy_ref != null ? {
      HTTP_PROXY  = var.copilot_proxy_ref
      HTTPS_PROXY = var.copilot_proxy_ref
      http_proxy  = var.copilot_proxy_ref
      https_proxy = var.copilot_proxy_ref
    } : {},
  )
}

model_provider "work" {
  type        = var.work_provider_type
  endpoint    = var.work_api_url
  wire_api    = var.work_wire_api
  api_key     = var.work_api_key
  api_key_ref = var.work_api_key_ref
}

model_provider "qc" {
  type        = var.qc_provider_type
  endpoint    = var.qc_api_url
  wire_api    = var.qc_wire_api
  api_key     = var.qc_api_key
  api_key_ref = var.qc_api_key_ref
}

worker "implement_slice" {
  based_source     = source.repo.branch
  sandbox          = sandbox.go_dev
  harness          = harness.copilot
  model_provider   = model_provider.work
  model            = var.work_model
  reasoning_effort = var.work_reasoning_effort
  writable_paths   = var.writable_paths
  human_approval   = var.human_approval

  system_prompt = <<-EOT
    Follow AGENTS.md and .specify/memory/constitution.md strictly, and write a concise change summary to ${artifact("summary").path}.
  EOT

  prompt = var.slice_prompt

  artifact "summary" {
    type        = "file"
    path        = "summary.md"
    description = "Implementation summary, modified files, and verification results."
    required    = true
    non_empty   = true
  }

  qc {
    sandbox          = sandbox.qc_audit
    harness          = harness.copilot
    model_provider   = model_provider.qc
    model            = var.qc_model
    reasoning_effort = var.qc_reasoning_effort
    max_qc_rounds    = 3

    gatekeeper "go_quality" {
      command                = ["sh", "-c", "case \"$HTTPS_PROXY$http_proxy\" in *_gateway*) unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy;; esac; unformatted=$(gofmt -l .); if [ -n \"$unformatted\" ]; then echo \"unformatted files:\"; echo \"$unformatted\"; exit 1; fi && go vet ./... && go test ./... -count=1"]
      working_dir            = "."
      timeout                = "10m"
      expect_exit_code       = 0
      require_clean_worktree = true
    }

    criteria = {
      spec_and_contracts = "Inspect ${artifact("summary").path} and git status/diff first; verify every change matches AGENTS.md, the active feature spec under specs/, and frozen contracts line by line."
      tdd_and_tests      = "Verify tests cover every new behavior and edge case without skipped assertions or mocked-away invariants."
      code_quality_scope = "Verify clean, minimal, idiomatic Go code with no out-of-scope edits, clean formatting, and an accurate ${artifact("summary").path} artifact."
    }
  }
}
