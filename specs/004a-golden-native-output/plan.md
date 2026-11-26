# Implementation Plan: Adopt Golden Native Output Blocks

**Branch**: `004a-golden-native-output` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Upgrade golden to the inspected native-output revision, delete sofa's custom block implementation and tests, remove output registration, and construct snapshots from `golden.OutputBlock`. Retain only a pure `Output` DTO.

## Technical Context

- Go 1.27.3.
- Minimum golden revision: `v0.0.0-20261125162047-059cc8cb0d1d` (`059cc8c`).
- Existing output integration/cold-start tests become native-block contract tests.
- No new external integration behavior.

## Constitution Check

- Spec-first/TDD: PASS.
- Delegate to golden: PASS; duplicated output capability is removed.
- Pure boundaries: PASS.
- Surgical sizing: PASS; no slice 004 typed-tool work.

## Design

- `internal/module/spec/output.go` retains only the pure `Output` snapshot.
- `SofaConfig.Outputs()` copies native accessor results into deterministic snapshots.
- Golden package initialization registers the native output block.
- Cold-start includes `source` plus native `output` to verify sofa custom registration still occurs before loading.

## Post-Design Constitution Check

All gates pass. The change removes owned implementation and restores strict golden delegation.
