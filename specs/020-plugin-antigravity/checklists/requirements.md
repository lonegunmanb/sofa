# Specification Quality Checklist: `020-plugin-antigravity`

**Purpose**: Validate completeness and readiness before TDD implementation.
**Created**: 2026-11-27
**Feature**: [spec.md](file:///d:/project/sofa/specs/020-plugin-antigravity/spec.md)

- [x] Pre-task investigation completed (`docs/stage2/020-plugin-antigravity.md`, `docs/harness-plugin-contract.md`, `pkg/harnessplugin/plugin.go`, `internal/harness/runtime/container_runner.go`, `internal/harness/installer/installer.go`, `google-antigravity@0.1.24`).
- [x] Isolation boundary preserved (no changes to root `go.mod`, no `internal/` coupling).
- [x] Windows LF + `0755` and unpadded DER base64 invariants explicitly captured.
- [x] All 7 RPCs, anti-double-interception hook rule, error taxonomy, and `AbortTurn` `turn_id` guard covered.
- [x] TDD Red -> Green -> Refactor tasks ordered with test tasks before implementation tasks.
