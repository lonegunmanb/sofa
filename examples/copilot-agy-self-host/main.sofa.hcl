source "repo" {
  url       = var.repo_url
  branch    = var.base_branch
  token_ref = "GITHUB_TOKEN"
}

sandbox "go_dev" {
  runtime = "podman"
  image   = var.sandbox_image
}

sandbox "qc_audit" {
  runtime = "podman"
  image   = var.sandbox_image
}

harness "copilot" {
  env_refs = {
    GITHUB_TOKEN = "COPILOT_GITHUB_TOKEN"
  }
}

harness "antigravity" {
  env_refs = {
    GEMINI_API_KEY = "GEMINI_API_KEY"
  }
}

worker "implement_slice" {
  based_source   = source.repo.branch
  sandbox        = sandbox.go_dev
  harness        = harness.copilot
  model          = var.work_model
  writable_paths = var.writable_paths
  human_approval = var.human_approval

  system_prompt = <<-EOT
    You are implementing a sofa engineering slice inside a sandboxed container.
    Follow AGENTS.md and .specify/memory/constitution.md strictly:
    - Author or update Spec Kit artifacts before modifying code, and follow Red -> Green -> Refactor TDD.
    - Reuse existing golden and internal package capabilities without duplicating logic.
    - Ensure gofmt -l . is empty, go vet ./... passes, and go test ./... -count=1 passes before completing work.
    - Write a concise change summary to ${artifact("summary").path}.
  EOT

  prompt = var.slice_prompt

  artifact "summary" {
    type        = "file"
    path        = "summary.md"
    description = "Implementation summary, modified files, and verification results."
    required    = true
    non_empty   = true
  }

  gatekeeper "go_quality" {
    command                = ["sh", "-c", "unformatted=$(gofmt -l .); if [ -n \"$unformatted\" ]; then echo \"unformatted files:\"; echo \"$unformatted\"; exit 1; fi && go vet ./... && go test ./... -count=1"]
    working_dir            = "."
    timeout                = "10m"
    expect_exit_code       = 0
    require_clean_worktree = true
  }

  qc {
    sandbox        = sandbox.qc_audit
    harness        = harness.antigravity
    model_provider = null
    model          = var.qc_model
    max_qc_rounds  = 3

    criteria = {
      spec_and_contracts = "Verify every implementation change matches AGENTS.md, the feature spec, and frozen contracts line by line."
      tdd_and_tests      = "Verify tests cover every new behavior and edge case without skipped assertions or mocked-away invariants."
      code_quality_scope = "Verify clean, minimal, idiomatic Go code with no duplicated logic, clean formatting, and accurate summary.md artifact."
    }
  }
}
