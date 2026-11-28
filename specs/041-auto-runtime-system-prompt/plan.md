# Implementation Plan: Host Auto-Injected Runtime Environment & Protocol System Prompt (`041-auto-runtime-system-prompt`)

**Branch**: `041-auto-runtime-system-prompt` | **Date**: 2026-11-28 | **Spec**: [spec.md](file:///workspace/specs/041-auto-runtime-system-prompt/spec.md)

## Summary

Add `internal/worker/runner/prompt_contract.go` with two pure renderers, `buildWorkRuntimeContractPrompt` and `buildQCRuntimeContractPrompt`, that emit the `sofa` host runtime/protocol invariants (working directory, Git-metadata prohibition, `writable_paths` boundary, import/artifact paths, checkpoint/terminate/decision/escalate lifecycle, QC open/resolve/submit lifecycle) for both container (`/workspace`, `/.sofa/...`) and `runtime = "host"` (host workspace, `.sofa/...`) layouts. Wire the Work renderer into the per-attempt `effectiveSysPrompt` (after the recovery diary) and the QC renderer into `qcBaseSession.SystemPrompt`. Delete the `# TODO(041)` hand-written paragraph from `examples/copilot-agy-self-host/main.sofa.hcl`.

## Technical Context

- **Languages / Dependencies**: Go 1.27; `internal/worker/runner`, `internal/worker/spec`, `internal/sandbox/runtime`, `internal/tool/runtime`, `internal/session`.
- **Testing**:
  - Go Tier 1: `internal/worker/runner` (`runner_test.go` integration injection + `prompt_contract_test.go` internal pure-function tests), `examples/examples_test.go`.
- **Constraints**:
  - No HCL schema changes; no new persisted state.
  - Must not call `SandboxConfig.Validate()` from the pure renderers so the `host` branch is testable before Slice `038` lands.
  - Preserve existing user `system_prompt` and recovery-diary ordering (`user + diary + contract`).
  - Do not re-implement `golden` capabilities.

## Constitution Check

- **Principle I (Spec-First)**: `spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`, and `tasks.md` authored before production edits.
- **Principle II (Strict TDD)**: Red-phase tests written first in `prompt_contract_test.go` and `runner_test.go`, confirmed failing, then implementation.
- **Principle III (Pure Core & Real Boundaries)**: Contract rendering is a deterministic pure function; the runner only wires the resulting string, so no container/Git boundary is touched.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: This slice intentionally only *documents* runtime invariants in the prompt; the actual enforcement (read-only mounts, epoch fencing, Git ownership) stays in trusted runtime mechanisms and is unchanged.
- **Principle VI (Research-First & Surgical Sizing)**: Verified `worker_runner.go` Work/QC session construction, `protocol_tools.go` tool names, `artifact.go` path helpers, `fallback.go` triggers, and `collectAgentEscalatedFallbacks` before designing. This slice touches only prompt rendering plus one example file.

## Complexity Tracking

No constitution exceptions required.

## Design Notes

- `promptPathLayout(sandboxRuntime string)` returns artifacts/imports roots: container (`/.sofa/{artifacts,imports}`) vs host (`.sofa/{artifacts,imports}`) chosen by `strings.TrimSpace(runtime) == "host"`.
- The host working directory is supplied by the caller (`workspaceDir` for Work, `qc-workspace-r<n>` for QC) so the renderer stays pure; container always uses `sandboxruntime.DefaultContainerWorkingDir`.
- Terminate tool name is derived from `workBindings`: `EffectiveName()` of the binding with `Terminate == true`, else `session.ToolSofaCompleteWork`.
- Artifact declarations are sorted by name; imports are sorted by name for deterministic prompts.
- Pure-function tests live in `prompt_contract_test.go` under `package runner` because `runner_test.go` is `package runner_test` and cannot access unexported identifiers; end-to-end injection assertions stay in `runner_test.go`.