# Quickstart: Auto-Injected Runtime & Protocol System Prompt (`041`)

## Verify the slice

```bash
cd /workspace

# Tier 1 — targeted
go test ./internal/worker/runner/... ./examples/... -count=1

# Full quality gate
gofmt -l .
go vet ./...
go test ./... -count=1
```

## Expected behavior

A `worker` that declares only domain guidance:

```hcl
worker "implement_slice" {
  system_prompt = "Follow AGENTS.md strictly."
  writable_paths = ["internal/**", "specs/**"]
  artifact "summary" {
    type = "file"
    path = "summary.md"
    description = "Change summary."
    required = true
    non_empty = true
  }
}
```

receives a Work `SystemPrompt` that starts with `Follow AGENTS.md strictly.` and is followed by an auto-generated section containing, for container mode:

- `Working directory: /workspace`
- the `git add` / `git commit` / `git push` prohibition
- `internal/**` and `specs/**`
- `/.sofa/artifacts/summary.md`
- `sofa_checkpoint` and `sofa_complete_work`

For `sandbox { runtime = "host" }` the same section instead contains the host workspace path, `.sofa/artifacts/summary.md`, and `.sofa/imports/...` paths.

QC sessions receive a read-only contract listing `/.sofa/artifacts/summary.md` and the `sofa_qc_open_issues` / `sofa_qc_resolve_issues` / `sofa_qc_submit_verdict` lifecycle.

## Files touched

- `internal/worker/runner/prompt_contract.go` (new)
- `internal/worker/runner/prompt_contract_test.go` (new, `package runner`)
- `internal/worker/runner/worker_runner.go` (wire Work + QC prompts)
- `internal/worker/runner/runner_test.go` (end-to-end prompt assertions)
- `examples/copilot-agy-self-host/main.sofa.hcl` (drop `TODO(041)` block)

## Manual sanity check

```bash
grep -Rn "TODO(041)" examples/ || echo "clean: no TODO(041) markers"
```