# Feature Specification: Host Auto-Injected Runtime Environment & Protocol System Prompt (`041-auto-runtime-system-prompt`)

**Feature Branch**: `041-auto-runtime-system-prompt`

**Created**: 2026-11-28

**Status**: Approved

**Input**: `docs/stage4/041-auto-runtime-system-prompt.md`, `internal/worker/runner/{worker_runner.go,types.go,runner_test.go}`, `internal/worker/spec/{schema.go,artifact.go,fallback.go}`, `internal/sandbox/runtime/spec.go`, `internal/session/protocol_tools.go`, `examples/copilot-agy-self-host/main.sofa.hcl`, `examples/examples_test.go`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Auto-Inject Work Runtime & Protocol Contract into `SystemPrompt` (Priority: P1)

Today `examples/copilot-agy-self-host/main.sofa.hcl` carries a `# TODO(041)` block that hand-writes runtime constraints (working directory, `.git` prohibition, `writable_paths`, artifact paths, protocol tool usage) into `worker.system_prompt`. Those constraints are `sofa` host protocol invariants, not business requirements, so every workflow author must duplicate them and can drift from the actual runner behavior.

`WorkerRunner` MUST build the Work runtime/protocol contract itself and append it to the user-supplied `system_prompt` (and after any recovery diary) via `appendPromptSection` when launching each Work session. The contract MUST contain:

- **Working directory & Git prohibition**: the effective working directory (`/workspace` for container mode, the host workspace path for `runtime = "host"`), and an explicit prohibition on `git add`, `git commit`, `git push`, `git checkout`, `git reset`, and `git clean` (the host owns checkpoint commits and branch pushes), plus a ban on mutating `.git/` and `.sofa/` control files.
- **Writable path boundary**: when `writable_paths` is configured, enumerate every allowed relative glob; otherwise state that ordinary workspace files may be edited but `.git/` and `.sofa/` control files are off-limits.
- **Imported artifacts**: each declared `import_artifact` name with its readable path (`/.sofa/imports/...` in containers, `.sofa/imports/...` in host mode).
- **Declared output artifacts**: each `artifact` name sorted by name, with its path (`/.sofa/artifacts/...` or `.sofa/artifacts/...`), type (`file`/`directory`), `required`/`non_empty` constraints, and `description`.
- **Protocol tool lifecycle**: call `sofa_checkpoint` with a structured diary (`completed`, `next_steps`) at meaningful milestones; MUST end the turn with the terminate tool (`sofa_complete_work` by default, or the effective custom terminate tool name when `terminate_tool_id`/`tool_use` is configured); call `sofa_request_decision` only for blocking ambiguity; when `agent_escalated` fallbacks exist, list the escalate targets and mention `sofa_escalate`.

**Independent Test**: `runner_test.go` asserts the Work `SessionConfig.SystemPrompt` begins with the user `system_prompt` and additionally contains `/workspace`, the `git commit` prohibition, the configured `writable_paths` entry, `/.sofa/artifacts/...`, `/.sofa/imports/...`, and `sofa_complete_work`; a custom terminate tool scenario asserts the custom tool name appears. A `runtime == "host"` pure-function test asserts `.sofa/artifacts/...`, `.sofa/imports/...`, and the host workspace path replace the container layout.

---

### User Story 2 - Auto-Inject QC Read-Only Runtime & Verdict Contract into `SystemPrompt` (Priority: P1)

`runQCRound` currently hardcodes a single-sentence QC `SystemPrompt` and never mentions the Worker's declared artifacts nor the QC tool lifecycle. `buildQCRuntimeContractPrompt` MUST produce a read-only audit contract that:

- states the read-only audit environment paths (`/workspace` + `/.sofa/artifacts` in container mode; the `qc-workspace` directory + `.sofa/artifacts` in host mode) and forbids modifying any workspace or artifact file and any Git write command;
- when the Worker declares `artifacts`, lists every artifact path and description so QC can locate outputs (e.g. `summary.md`) together with `git status` / `git diff`;
- states that problems are reported via `sofa_qc_open_issues`, previously opened issues are closed via `sofa_qc_resolve_issues`, and the round MUST end with `sofa_qc_submit_verdict` (`pass` requires zero open issues, `revise` requires at least one open issue).

**Independent Test**: `runner_test.go` asserts the QC `SessionConfig.SystemPrompt` contains the declared artifact path (`/.sofa/artifacts/report.json`), the read-only wording, and `sofa_qc_submit_verdict`. A `runtime == "host"` pure-function test asserts `.sofa/artifacts/...` and the host QC workspace path.

---

### User Story 3 - Remove Bootstrap Example Hand-Written Prompt (Priority: P2)

Remove the `# TODO(041)` runtime/protocol paragraph from `examples/copilot-agy-self-host/main.sofa.hcl`, leaving only the domain requirement (`Follow AGENTS.md ... write the summary to ${artifact("summary").path}`). The existing example compile + Plan round-trip test MUST stay green.

**Independent Test**: `examples/examples_test.go` `TestCopilotAgySelfHostExampleCompilesAndRoundTripsPlan` passes after the edit.

---

### Edge Cases

- `writable_paths` is `nil` vs. an explicitly empty slice: both mean "no declared restrictions"; the contract MUST NOT print an empty list header.
- A Worker with no `import_artifact` and no `artifact` declarations: the contract omits those sections rather than printing empty headings.
- Artifact ordering MUST be deterministic (sorted by name) so prompts are reproducible.
- Artifact `required=false` / `non_empty=false` SHOULD still be stated explicitly so the agent knows the constraint set.
- No `agent_escalated` fallback: the contract MUST NOT mention `sofa_escalate`.
- Custom terminate tool from `tool_use` alias: the contract MUST name the effective exposed tool name.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/worker/runner/prompt_contract.go` MUST expose pure functions `buildWorkRuntimeContractPrompt` and `buildQCRuntimeContractPrompt` that deterministically render the runtime/protocol contract for container and `runtime == "host"` modes without invoking `SandboxConfig.Validate()`.
- **FR-002**: `WorkerRunner` MUST append the Work contract to the user `system_prompt` (after the recovery diary) via `appendPromptSection` for every Work attempt, both container and host modes.
- **FR-003**: The Work contract MUST include working directory, Git metadata prohibition, `writable_paths` boundary, import paths, sorted artifact declarations (path/type/required/non_empty/description), `sofa_checkpoint`, the effective terminate tool, `sofa_request_decision`, and `sofa_escalate` when `agent_escalated` fallbacks exist.
- **FR-004**: `runQCRound` MUST set the QC `SystemPrompt` to the existing audit sentence plus the QC contract rendered by `buildQCRuntimeContractPrompt`, listing declared artifacts and `sofa_qc_open_issues` / `sofa_qc_resolve_issues` / `sofa_qc_submit_verdict` rules.
- **FR-005**: Path layout MUST switch between `/.sofa/{artifacts,imports}` (container) and `.sofa/{artifacts,imports}` (host) based on the sandbox runtime string; host mode MUST use the host workspace path as the working directory.
- **FR-006**: The terminate tool name MUST be the effective name of the terminating tool binding (`ExposedName` when aliased, otherwise `Spec.ID`) or `sofa_complete_work` when no custom terminate tool is configured.
- **FR-007**: `examples/copilot-agy-self-host/main.sofa.hcl` MUST drop the `# TODO(041)` hand-written runtime/protocol paragraph and keep only domain guidance plus the summary artifact reference.

### Key Entities

- **WorkRuntimeContractInput**: `SandboxRuntime`, `WorkingDir`, `WritablePaths`, `Artifacts`, `Imports`, `TerminateTool`, `Fallbacks`.
- **QCRuntimeContractInput**: `SandboxRuntime`, `WorkingDir`, `Artifacts`.
- Contract rendering is pure text assembly; no new persisted state and no HCL schema changes.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` is empty.
- **SC-002**: `go vet ./...` passes.
- **SC-003**: `go test ./... -count=1` passes, including the new Red-first contract tests and `examples/examples_test.go`.
- **SC-004**: `examples/copilot-agy-self-host/main.sofa.hcl` contains no `TODO(041)` marker while the example compile + Plan round-trip test remains green.