# Contract: `qc`-Nested `gatekeeper` (`034`)

## 1. HCL Schema Contract

### Removed from `worker` and `dynamic_worker.dynamic.content`
- `gatekeeper "<name>" { ... }` and `dynamic "gatekeeper" { ... }` are invalid directly inside `worker` or `dynamic_worker` `content`.
- Attempting to declare them at worker level returns a Plan error instructing the user to move them into `qc { gatekeeper "..." {} }`.

### Added to `worker.<name>.qc` (and `dynamic_worker.<name>.dynamic.content.qc`)
- `max_gatekeeper_reworks`: optional whole integer `>= 0` (default `3`).
- `gatekeeper "<name>"`: repeatable nested block (and `dynamic "gatekeeper"`), using identical attribute schema and defaults as `merge.gatekeeper`:
  - `command` (required `list(string)`, non-empty; inside `qc`, elements may evaluate at Apply time)
  - `working_dir` (optional workspace-relative string, default `"."`)
  - `timeout` (optional positive duration string, default `"10m"`)
  - `expect_exit_code` (optional whole integer, default `0`)
  - `require_clean_worktree` (optional bool, default `true`)
- `criteria`: now optional (omitted or `null` means no LLM QC session). When set non-null, must be a non-empty map of non-empty strings.
- Constraint: `qc` MUST declare at least one `gatekeeper` OR non-empty `criteria`.

### Unchanged: `merge.<name>`
- `merge` keeps `gatekeeper "<name>"` directly inside `merge` with `merge.sandbox`; no `qc` block exists on `merge`.

## 2. Runtime Execution Order

For each Work -> Freeze -> Artifact Validate iteration:
1. If `qc != nil && len(qc.Gatekeepers) > 0`:
   - Run `SandboxEngine.RunGatekeepers` with `Sandbox: qcSandbox` (`qc.sandbox` falling back to `worker.sandbox`).
   - On failure: if `gatekeeperReworks < maxGKReworks`, increment `gatekeeperReworks`, set `reworkPrompt = formatGatekeeperFailurePrompt(gkFail)`, and `continue` to Work rework (without launching QC LLM). Otherwise trigger `gatekeeper_exhausted` fallback if declared, or return `StatusFailed`.
2. If `qc != nil && len(qcCriteria) > 0`:
   - Increment `qcRound`, launch read-only QC LLM session in `qcSandbox`.
   - On `revise`: if `qcRound < qc.MaxQCRounds`, set `reworkPrompt` and `continue` (next iteration reruns gatekeepers before next LLM QC round). Otherwise trigger `qc_exhausted` fallback or fail.
3. Otherwise break and publish.
