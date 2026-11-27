# Implementation Plan: Container `/.sofa/bundle` Extract-Root Mounting, Path Relocation, Lenient Skill Filtering, and Harness Parity (`037-container-skill-relocation-and-lenient-filtering`)

**Branch**: `037-container-skill-relocation-and-lenient-filtering` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/037-container-skill-relocation-and-lenient-filtering/spec.md)

## Summary

Pass the full `<extractRoot>` from `WorkerDaemon` to `WorkerRunner` and `ExecuteMergeCandidate` as `BundleDir`; introduce shared bundle command argument relocation in `internal/sandbox/runtime` for `PodmanEngine.RunGatekeepers` and `toolruntime.Executor`; add `SkillsDir` (`/.sofa/skills:ro`) to `SessionMountLayout`; implement `prepareSessionSkills` in `internal/worker/runner/skills.go` for deterministic `skill_directories` relocation, `disabled_skills` priority filtering + symlink-free mode-preserving staging, and lenient `skills` filtering across Work and QC sessions; and align `sofa-harness-copilot` and `sofa-harness-antigravity` with matching `disabled_skills` and lenient eager skill loading semantics.

## Technical Context

- **Languages / Dependencies**: Go 1.27 (`internal/...`, `plugins/sofa-harness-copilot`), Python 3.12+ (`plugins/sofa-harness-antigravity`, `google-antigravity`, `pytest`).
- **Testing**:
  - Go Tier 1: `internal/worker/daemon`, `internal/sandbox/runtime`, `internal/tool/runtime`, `internal/worker/runner`, `plugins/sofa-harness-copilot/internal/server`.
  - Python unit tests: `plugins/sofa-harness-antigravity/tests/test_servicer.py`.
  - Go Tier 2 (`//go:build integration`): `internal/worker/runner/runner_integration_test.go` against real Podman and Git.
- **Constraints**:
  - Do NOT modify `path.module` evaluation at Plan or Apply time.
  - Do NOT re-implement `golden` capabilities.
  - Never combine `inline_skills` and `skills_paths` in `google.antigravity.LocalAgentConfig`.
  - Do NOT `git commit` or `git push`.

## Constitution Check

- **Principle I (Spec-First)**: `spec.md`, `plan.md`, `tasks.md` authored before any production edits.
- **Principle II (Strict TDD)**: Failing Red-phase tests written across Go and Python suites before implementing production code.
- **Principle III (Pure Core & Real Boundaries)**: Shared relocation helper in `sandboxruntime`, isolated host-side skill staging in `internal/worker/runner/skills.go`, and real Podman Tier 2 integration verification.
- **Principle VI (Research-First & Surgical Sizing)**: Verified `daemon.go`, `spec.go`, `podman.go`, `executor.go`, `worker_runner.go`, `server.go`, `backend.py`, and `google.antigravity.LocalAgentConfig` source before designing.

## Complexity Tracking

No constitution exceptions required.
