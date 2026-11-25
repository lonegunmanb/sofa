# Implementation Plan: Configuration Foundation and Outputs

**Branch**: `001-golden-config-output` | **Date**: 2026-11-25 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/001-golden-config-output/spec.md` and the frozen implementation blueprint in `docs/stage1/001-golden-config-and-output.md`.

## Summary

Build the thinnest sofa configuration planner on top of `github.com/lonegunmanb/golden`. The implementation scans one directory for `*.sofa.hcl`, parses every file with matching syntax/write representations, delegates variables, locals, DAG construction, dependency checks, and plan traversal to `golden`, adds sofa's path context and four deterministic helper functions, and registers a named `output` block with sensitivity-preserving snapshots. This slice establishes the shared packages that slices 002–007 will extend, without container, Git, Dapr, network, or CLI behavior.

## Technical Context

**Language/Version**: Go 1.27.3 (`go.mod` module `github.com/lonegunmanb/sofa`)

**Primary Dependencies**: `github.com/lonegunmanb/golden@v0.0.0-20261006003654-0f1df693e62c`, `github.com/lonegunmanb/hclfuncs@v0.12.0`, `github.com/lonegunmanb/go-defaults@v1.4.0`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty`, `github.com/stretchr/testify`

**Storage**: Local configuration directory and local variable files only; no persistent application storage in this slice

**Testing**: `go test ./... -count=1`, with package-level unit tests; real dependencies are local files and in-memory values only

**Target Platform**: Windows, Linux, and macOS developer/CI environments capable of running the Go toolchain

**Project Type**: Go library/compiler foundation for a future CLI

**Performance Goals**: A normal Stage 1 test-directed configuration (under 100 files) loads and plans in under one second on a developer machine; no operation blocks on network or external daemons

**Constraints**: Offline, deterministic file ordering, no interactive prompts in tests, no reimplementation of `golden` variable or DAG semantics, all HCL block and attribute names use `lower_snake_case`, diagnostics must not expose sensitive values

**Scale/Scope**: One configuration directory level, one output block family, shared variable/local behavior, and four helper functions; later Stage 1 slices add all other block families

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

- **Spec-First Delivery**: PASS — `spec.md`, this plan, research, contracts, and tasks are produced before production code.
- **Strict TDD**: PASS — every story will have failing tests before implementation; tasks enforce Red → Green → Refactor.
- **Pure Core & Real-Dependency Boundaries**: PASS — all code in this slice is deterministic in-process configuration logic; tests use real local filesystem directories rather than mocked parsers.
- **Runtime Enforcement Over Prompt Trust**: PASS — sensitivity is represented by the planning engine's native mark and redaction behavior; no agent or prompt trust is involved.
- **Single-Node & Distributed Parity**: PASS — this slice only defines the shared offline plan semantics and introduces no divergent runtime mode.
- **Research-First & Surgical Sizing**: PASS — the selected scope is exactly blueprint slice 001. `golden` source was inspected for current APIs. Container, Dapr, Git, and agent runtime concerns are excluded.

No constitutional violations or complexity exceptions are required.

## Project Structure

### Documentation (this feature)

```text
specs/001-golden-config-output/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── configuration-loading.md
│   ├── planning-functions.md
│   └── output-block.md
├── checklists/
│   └── requirements.md
└── tasks.md
```

### Source Code (repository root)

```text
internal/
├── spec/
│   ├── cty.go
│   └── cty_test.go
├── config/
│   ├── loader.go
│   ├── loader_test.go
│   ├── functions.go
│   ├── functions_test.go
│   ├── base.go
│   └── base_test.go
├── module/
│   └── spec/
│       ├── output.go
│       └── output_test.go
└── planner/
    ├── config.go
    ├── variable_test.go
    ├── functions_integration_test.go
    └── dag_test.go

go.mod
go.sum
```

Package tests live beside their production packages. `internal/planner` registers every sofa block during package initialization and exposes an idempotent `RegisterBlocks()` entry point. `internal/planner` tests exercise the complete loader → `golden.InitConfig` → `RunPlan` path, while lower-level function tests isolate evaluation behavior.

**Structure Decision**: Use the frozen Stage 1 package layout. `internal/config` remains a low-level primitive and never imports a business block package. `internal/spec` owns shared cty helpers. `internal/module/spec` owns the first block model even though modules are implemented in slice 007. `internal/planner` owns idempotent block registration and the public planning workflow. Any executable or test that loads blocks imports `internal/planner`, causing registration during package initialization before caller code runs. No `cmd/` tree is added in this slice.

## Phase 0 Research Outcome

See [research.md](./research.md). There are no unresolved clarifications.

## Phase 1 Design Outcome

- [data-model.md](./data-model.md) defines loaded files, declarations, variables, outputs, plan snapshots, and sensitivity.
- [contracts/configuration-loading.md](./contracts/configuration-loading.md) defines directory and diagnostic behavior.
- [contracts/planning-functions.md](./contracts/planning-functions.md) defines function names, signatures, and unknown-value behavior.
- [contracts/output-block.md](./contracts/output-block.md) defines the output schema and value contract.
- [quickstart.md](./quickstart.md) defines runnable validation scenarios.

## Post-Design Constitution Check

- **Spec-First Delivery**: PASS — design artifacts define all externally observable behavior before tasks begin.
- **Strict TDD**: PASS — contracts map directly to test cases; no production file is scheduled before its story's test task.
- **Pure Core & Real-Dependency Boundaries**: PASS — only deterministic local configuration and filesystem behavior is introduced.
- **Runtime Enforcement Over Prompt Trust**: PASS — redaction uses marks emitted by the planning engine; no textual heuristics.
- **Single-Node & Distributed Parity**: PASS — no runtime-mode split is introduced.
- **Research-First & Surgical Sizing**: PASS — current upstream code confirms the delegated APIs and the blueprint's block limits.

## Complexity Tracking

No violations.
