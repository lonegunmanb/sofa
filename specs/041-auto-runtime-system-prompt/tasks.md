# Tasks: Host Auto-Injected Runtime Environment & Protocol System Prompt (`041-auto-runtime-system-prompt`)

**Input**: Design documents from `/specs/041-auto-runtime-system-prompt/`
**Prerequisites**: `spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/prompt-contract.md`

## Phase 1: Tests First (Red Phase — MUST fail before implementation)

- [x] T001 [US1][US2] Add `internal/worker/runner/prompt_contract_test.go` (`package runner`) with failing table tests for `buildWorkRuntimeContractPrompt` covering container mode (`/workspace`, `git commit` prohibition, `writable_paths`, `/.sofa/artifacts/...`, `/.sofa/imports/...`, `sofa_checkpoint`, default `sofa_complete_work`), custom terminate tool name, `agent_escalated` escalation listing, `nil` vs empty `writable_paths`, and `runtime = "host"` switching to `.sofa/artifacts/...` / `.sofa/imports/...` / host workspace path.
- [x] T002 [US2] Add failing tests in `prompt_contract_test.go` for `buildQCRuntimeContractPrompt` covering container mode (`/workspace`, `/.sofa/artifacts/...`, read-only wording, `sofa_qc_open_issues`, `sofa_qc_resolve_issues`, `sofa_qc_submit_verdict`, `pass`/`revise` rules) and `runtime = "host"` switching to `.sofa/artifacts/...` / host `qc-workspace` path.
- [x] T003 [US1][US2] Add failing end-to-end assertions in `internal/worker/runner/runner_test.go` that the Work `SessionConfig.SystemPrompt` contains the user prompt + `git commit` prohibition + `writable_paths` entry + `/.sofa/artifacts/report.json` + `sofa_complete_work`, and the QC `SessionConfig.SystemPrompt` contains `/workspace`, `/.sofa/artifacts/report.json`, and `sofa_qc_submit_verdict`.
- [x] T004 [US1] Add a failing end-to-end assertion in `runner_test.go` (or extend an existing custom-terminate scenario) that a custom `terminate_tool_id`/`tool_use` alias appears in the Work `SystemPrompt`.
- [x] T005 Run `go test ./internal/worker/runner/... ./examples/... -count=1` and confirm the new tests fail for the expected reasons (undefined helpers / missing prompt text).

## Phase 2: Implementation (Green Phase)

- [x] T006 [US1][US2] Create `internal/worker/runner/prompt_contract.go` with `workRuntimeContractInput`, `qcRuntimeContractInput`, `promptPathLayout`, `buildWorkRuntimeContractPrompt`, `buildQCRuntimeContractPrompt`, and the small `terminateToolName` / `agentEscalatedFallbackNames` helpers.
- [x] T007 [US1] Wire `buildWorkRuntimeContractPrompt` into the Work attempt loop in `internal/worker/runner/worker_runner.go` via `appendPromptSection` after the diary append, passing `workSandbox.Runtime`, the effective working directory, `req.Worker.WritablePaths`, `req.Worker.Artifacts`, `req.Worker.Imports`, the terminate binding name, and `req.Worker.Fallbacks`.
- [x] T008 [US2] Wire `buildQCRuntimeContractPrompt` into `runQCRound`'s `qcBaseSession.SystemPrompt` using `qcSandbox.Runtime`, the QC working directory, and `req.Worker.Artifacts`.
- [x] T009 [US3] Remove the `# TODO(041)` hand-written runtime/protocol paragraph from `examples/copilot-agy-self-host/main.sofa.hcl`.

## Phase 3: Verification

- [x] T010 Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`; confirm `examples/examples_test.go` stays green and `grep -Rn "TODO(041)" examples/` is empty.
- [x] T011 Refactor while green (deduplicate rendering helpers, confirm deterministic ordering) and re-run T010.

## Phase 4: QC Round 1 Revision (Contract Compliance)

- [x] T012 [US1] Render every artifact with explicit `required=<bool>, non_empty=<bool>` fields in the frozen contract shape (`- <name> (<type>, required=<bool>, non_empty=<bool>): <path> — <description>`) via the shared `renderArtifactLine` helper, preserving name-sorted ordering (addresses QC-001).
- [x] T013 [US1] Add focused `prompt_contract_test.go` coverage for `WritablePaths: []string{}` (explicit empty behaves like unset) and all four `required`/`non_empty` boolean combinations asserting the exact contract line format (addresses QC-002).
## Phase 5: QC Round 2 Revision (Contract Shape & Edge Coverage)

- [x] T014 [US1] Render the explicit `.git/`/`.sofa/` control-file prohibition inside the configured `writable_paths` branch (previously only in the unset branch), addressing QC-003.
- [x] T015 [US1] Switch imported-artifact entries to the frozen `- <name>: <path>` shape (with optional ` — <description>` suffix), replacing backticked `->` form, addressing QC-004.
- [x] T016 [US1] Add focused `prompt_contract_test.go` coverage for control-file bans in both writable-path branches, exact container/host import shape, no-escalation omission, sorted/deduplicated `agent_escalated` names, and user→diary→contract ordering via `composeWorkSystemPrompt`, addressing QC-005.

## Phase 6: QC Round 3 Revision (Summary Accuracy)

- [x] T017 [Docs] Correct `/.sofa/artifacts/summary.md` to state all 16 tasks (T001–T016) are complete (previously said 11) and fix the malformed inline backticks on the sample escalation line, addressing QC-006. No production code change; `gofmt`/`go vet ./...`/`go test ./... -count=1` re-verified green.
