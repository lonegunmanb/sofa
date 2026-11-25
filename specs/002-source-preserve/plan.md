# Implementation Plan: Source Repositories and Preserved Branches

**Branch**: `002-source-preserve` | **Date**: 2026-11-25 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/002-source-preserve/spec.md` and the frozen blueprint in `docs/stage1/002-source-and-preserve.md`.

## Summary

Add two offline planning blocks. `source "<name>"` validates a repository URL, branch, and optional credential-reference names. `preserve "<name>"` accepts known or deferred string branches while retaining the original expression. A planner-owned pre-adaptation hook resolves omitted preserve URLs only when the root has exactly one source, injecting a real traversal into both HCL AST representations so golden builds the required dependency edge.

## Technical Context

**Language/Version**: Go 1.27.3

**Primary Dependencies**: `github.com/lonegunmanb/golden@v0.0.0-20261006003654-0f1df693e62c`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty`, `github.com/stretchr/testify`

**Storage**: Local configuration files only; no repository access

**Testing**: `go test ./... -count=1`, including subprocess cold-start and graph-dependency assertions

**Target Platform**: Windows, Linux, and macOS

**Project Type**: Go planning library/compiler foundation

**Performance Goals**: A configuration under 100 files selects and validates sources/preserves in under one second without network or subprocesses

**Constraints**: Offline only; no environment credential reads; no Git invocation; all block registration remains in `internal/planner`; `internal/config` must not import business block packages

**Scale/Scope**: `source`, `preserve`, shared branch-reference validation, and the root preserve-URL singleton rule

## Constitution Check

- **Spec-First Delivery**: PASS — the slice specification, plan, contracts, data model, tasks, and analysis precede code.
- **Strict TDD**: PASS — every story receives failing tests before its implementation.
- **Pure Core & Real-Dependency Boundaries**: PASS — validation is deterministic and local; network and Git behavior are excluded.
- **Runtime Enforcement Over Prompt Trust**: PASS — branch and repository rules are deterministic code checks.
- **Single-Node & Distributed Parity**: PASS — both modes will consume the same immutable plan values.
- **Research-First & Surgical Sizing**: PASS — the slice contains only source and preserve blocks plus their root default; golden interfaces and current repository code were inspected.

## Project Structure

```text
internal/
├── config/
│   ├── loader.go                  # generic pre-adaptation hook and raw module transport
│   └── loader_test.go
├── spec/
│   ├── branch_ref.go
│   └── branch_ref_test.go
├── source/
│   ├── schema.go
│   └── schema_test.go
└── planner/
    ├── defaults.go
    ├── defaults_test.go
    ├── source_test.go
    └── preserve_test.go
```

**Structure Decision**: `internal/spec` owns reusable branch-reference validation. `internal/source` owns source/preserve models and HCL decoding. `internal/planner/defaults.go` owns the source-to-preserve singleton rule and traversals. `internal/config` exposes only a generic raw-module hook so business semantics do not enter the low-level package.

## Design Decisions

### Pre-adaptation hook

`LoadDirectoryContext` parses each file into syntax and write bodies, then invokes an optional generic `RootDefaultsHook` before calling `golden.AsHclBlocks`. Planner package initialization registers the only hook. The hook mutates matching blocks in both ASTs:

- syntax: adds a `ScopeTraversalExpr`;
- write: calls `SetAttributeRaw` with `TokensForTraversal`.

This preserves real dependency discovery and avoids trying to manipulate golden's unexported write-block handle after adaptation.

### Source block

`SourceBlock` implements `PlanBlock` and `SingleValueBlock`. It uses normal HCL decoding into string fields plus a strict pre-decode type check. `SourceConfig.Validate()` owns repository, branch, environment-name, and mutual-exclusion checks. `ExecuteDuringPlan` publishes a stable object whose `commit_sha` is unknown.

### Preserve block

`PreserveBlock` implements `CustomDecode`, `PlanBlock`, and `SingleValueBlock`. Its custom decoder evaluates URL and branch expressions directly, rejects unknown URL shapes, enforces exact string types, validates known branch names, and stores a `BranchRef` for unknown values.

## Post-Design Constitution Check

All gates remain PASS. The generic config hook keeps dependency direction one-way: `planner -> source -> spec`; `config` imports none of them.

## Complexity Tracking

No constitutional violations or exceptions.
