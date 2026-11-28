# Research: Auto-Injected Runtime & Protocol System Prompt (`041-auto-runtime-system-prompt`)

## Decision 1: Where the contract is rendered

- **Decision**: A new file `internal/worker/runner/prompt_contract.go` holding pure functions `buildWorkRuntimeContractPrompt(in workRuntimeContractInput) string` and `buildQCRuntimeContractPrompt(in qcRuntimeContractInput) string`.
- **Rationale**: `docs/stage4/041` explicitly names these functions and requires the `host` branch be unit-testable without going through `SandboxConfig.Validate()` (Slice `038` not yet landed).
- **Alternatives rejected**: Extending `buildQCPrompt` (that builds the per-round *user* prompt, not the system prompt); embedding the text inline in `worker_runner.go` (untestable pure logic).

## Decision 2: Path layout selection

- **Decision**: `promptPathLayout` switches on `strings.TrimSpace(sandboxRuntime) == "host"`.
  - Container: workdir `/workspace`, roots `/.sofa/artifacts` and `/.sofa/imports`.
  - Host: workdir supplied by caller, roots `.sofa/artifacts` and `.sofa/imports`.
- **Rationale**: Matches the frozen Stage 4 contract §3 (host artifacts are workspace-relative `.sofa/...`, containers keep `/.sofa/...`). The caller owns the concrete host path so the function stays deterministic.

## Decision 3: Terminate tool name resolution

- **Decision**: Scan the resolved `[]toolruntime.ToolBinding` for `Terminate == true` and use `EffectiveName()`; default to `session.ToolSofaCompleteWork`.
- **Rationale**: `resolveToolBindings` already appends the `terminate_tool_id` binding and honours `tool_use` aliases; reusing bindings avoids duplicating name-resolution logic (Principle VI).

## Decision 4: Escalation target collection

- **Decision**: Local helper mirroring `session.collectAgentEscalatedFallbacks` (names of fallbacks whose `on` contains `agent_escalated`), sorted for determinism, rendered only when non-empty.
- **Rationale**: The session helper is unexported and lives in another package; a three-line local helper is cheaper than exporting and crossing package intent. `sofa_escalate` is only mounted when such fallbacks exist, so the contract must stay silent otherwise.

## Decision 5: Test placement

- **Decision**: Pure-function tests in `internal/worker/runner/prompt_contract_test.go` with `package runner`; end-to-end `SessionConfig.SystemPrompt` assertions in `runner_test.go` (`package runner_test`).
- **Rationale**: Go forbids external test packages from referencing unexported identifiers, so testing the lowercase renderers directly requires an internal test package. The blueprint's "runner_test.go" intent (pure-function coverage of the host branch) is satisfied while keeping the integration assertions in the existing suite.

## Decision 6: Example cleanup

- **Decision**: Remove the `# TODO(041)` paragraph from `examples/copilot-agy-self-host/main.sofa.hcl`, keeping `Follow AGENTS.md ... write ... ${artifact("summary").path}`.
- **Rationale**: The example must no longer duplicate host protocol constraints now that the runner injects them; `examples_test.go` validates compilation and Plan round-trip.

## Unknowns resolved

- Whether `runtime == "host"` passes `SandboxConfig.Validate()` today: it does not (Slice `038`); the pure renderer therefore accepts the runtime string directly.
- Whether QC `SystemPrompt` is used on resume: it is rebuilt each round, so recomputing is safe and idempotent.