# Implementation Plan: `.sofaplan` Permission Preservation and Client-Side External Skill Bundling (`035-archive-permissions-and-external-skills`)

**Branch**: `035-archive-permissions-and-external-skills` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/035-archive-permissions-and-external-skills/spec.md)

## Summary

Add deterministic POSIX permission preservation (`0755` for directories, host-executable files, and `#!` shebang files; `0644` for regular files) to `.sofaplan` archive pack/extract and `computeBundleSHA256`, implement client-side `skill_directories` classification, Fail-Fast validation, external skill bundling under `bundle/external_skills/<hash>/`, and plan config normalization in `internal/plan` (invoked by `SaveArchive`), wire `ArchiveSource.RootSourceDir` in CLI and preserve normalized `SkillDirectories` in `applyeval`, and update contract docs.

## Technical Context

- **Language / Dependencies**: Go 1.27 (`archive/zip`, `crypto/sha256`, `os.Root`, `io/fs`), `github.com/zclconf/go-cty`.
- **Testing**: Tier 1 unit and CLI tests (`go test ./... -count=1`) across `internal/plan`, `internal/applyeval`, `internal/cli`.
- **Constraints**:
  - Do not `git commit` or `git push`.
  - Never re-implement `golden` capabilities.
  - Preserve Windows `os.Root` extraction safety.
  - Do not relocate `skill_directories` to `/.sofa/bundle/...` (reserved for Slice 037).

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored under `specs/035-archive-permissions-and-external-skills/` before production edits.
- **Principle II (Strict TDD)**: Failing tests written and verified failing before implementing production changes.
- **Principle III (Pure Core & Real Boundaries)**: Archive packing/loading and `applyeval` tested deterministically using `t.TempDir()` and real ZIP inspection without mocks.
- **Principle VI (Research-First & Surgical Sizing)**: Inspected `internal/plan/archive.go`, `internal/plan/model.go`, `internal/planner/config.go`, `internal/planner/export.go`, `internal/applyeval/eval.go`, `internal/cli/{command.go,apply_cmd.go}`, and `internal/project/project.go` prior to planning.

## Complexity Tracking

No constitution exceptions required.
