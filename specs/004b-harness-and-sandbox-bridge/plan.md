# Implementation Plan: Harness Plugin Block and Sandbox Bridge Default

**Branch**: `004b-harness-and-sandbox-bridge` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/004b-harness-and-sandbox-bridge/spec.md`

## Summary

Implement Stage 1 slice `004b-harness-and-sandbox-bridge`:
1. Change `sandbox` default `network` from `"none"` to `"bridge"` (when omitted or `null`) while keeping `"bridge"`, `"none"`, and `"host"` valid.
2. Add `internal/harness/spec` with `HarnessBlock`, `HarnessConfig`, and `SignatureMode` aligned with `tflint`'s external plugin model: paired `source` + `version` (GitHub Releases) or both omitted/`null` (local plugin directory) with zero bundled `"copilot"` special cases, strict orthogonality with `model_provider`, `signature`/`signing_key`, `env_refs`, and marked `options`.
3. Extend `RootDefaults` in `internal/planner/defaults.go` with `Harnesses []string` and `ResolveDefaultHarness(consumerAddr string) (string, error)` matching `ResolveDefaultSandbox`.
4. Register `&harnessspec.HarnessBlock{}` in `internal/planner/config.go` and add planner and cold-start tests.

## Technical Context

**Language/Version**: Go 1.27.3  
**Primary Dependencies**: `github.com/lonegunmanb/golden`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty/cty`, `github.com/stretchr/testify`  
**Storage**: N/A (pure in-memory planning models)  
**Testing**: Pure Tier 1 unit tests (`go test ./... -count=1`) + cold-start subprocess test  
**Target Platform**: Cross-platform Go library/CLI  
**Project Type**: Library / planner packages under `internal/`  
**Performance Goals**: Instantaneous deterministic offline planning  
**Constraints**: Offline only — zero network calls, container CLI invocations, `go-plugin` launches, or host environment variable lookups; zero re-implementation of `golden` capabilities  
**Scale/Scope**: Single-slice addition (`internal/harness/spec`) + targeted updates to `internal/sandbox/spec` and `internal/planner`

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

- **I. Spec-First Delivery**: PASS — `spec.md`, `checklists/requirements.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`, and `tasks.md` authored before production edits.
- **II. Strict TDD**: PASS — failing tests will be written and observed failing before implementing production changes in `sandbox/spec`, `harness/spec`, and `planner`.
- **III. Pure Core & Real-Dependency Boundaries**: PASS — `internal/harness/spec`, `internal/sandbox/spec`, and `internal/planner` remain pure offline domain packages with zero `os/exec`, network, or `os.Getenv` calls.
- **IV. Runtime Enforcement Over Prompt Trust**: PASS — deterministic schema validation enforces plugin source/version/signature/env_ref invariants at Plan time.
- **V. Single-Node & Distributed Parity**: PASS — `HarnessConfig()` exports an immutable deep-copied snapshot for downstream `005`/`007` consumers.
- **VI. Research-First & Surgical Task Sizing**: PASS — inspected `golden` (`BaseDecode`, `markedStringBody`, `Value`), `internal/provider`, `internal/sandbox/spec`, `internal/planner`, and `tflint` reference prior to design; `golden` handles block decoding, unknown attributes/blocks, `for_each`, `depends_on`, `precondition`, and `dynamic` natively.

## Project Structure

### Documentation (this feature)

```text
specs/004b-harness-and-sandbox-bridge/
├── checklists/
│   └── requirements.md
├── contracts/
│   ├── harness-block.md
│   └── root-defaults.md
├── data-model.md
├── plan.md
├── quickstart.md
├── research.md
├── spec.md
└── tasks.md
```

### Source Code (repository root)

```text
internal/
├── harness/
│   └── spec/
│       ├── schema.go          # HarnessBlock, HarnessConfig, SignatureMode, Validate(), Value()
│       └── schema_test.go     # Unit tests for validation, Value(), HarnessConfig() deep copy
├── sandbox/
│   └── spec/
│       ├── schema.go          # Update default network from "none" to "bridge"
│       └── schema_test.go     # Unit tests covering bridge default and explicit none/host
└── planner/
    ├── config.go              # Register &harnessspec.HarnessBlock{} in RegisterBlocks()
    ├── defaults.go            # Add Harnesses []string and ResolveDefaultHarness
    ├── defaults_test.go       # Add 0/1/2 harness default resolution tests
    ├── sandbox_test.go        # Update bridge default assertions + add null/none tests
    ├── harness_test.go        # Planner integration tests for harness block
    └── cold_start_test.go     # Add cold-start test with harness block
```

**Structure Decision**: Follows existing `internal/<domain>/spec/schema.go` layout (`internal/sandbox/spec`, `internal/mcp/spec`, `internal/tool/spec`) and `internal/planner/` integration tests.

## Post-Design Constitution Check

All six constitutional principles pass with zero exceptions. No new dependencies are added to `go.mod`, `internal/config` remains decoupled from business block packages, and `golden` native capabilities are never duplicated or blocked.
