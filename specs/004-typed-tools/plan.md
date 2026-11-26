# Implementation Plan: Typed Tools (`go_tool` and `external_tool`)

**Branch**: `main` (staying on `main` per slice workflow) | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Implement Stage 1 Slice 004 (`004-typed-tools`) per `docs/stage1/004-typed-tools.md`: reusable `Condition` and `ToolResponse[T]`/`Issue` primitives in `internal/spec`, pure offline AST analyzer in `internal/tool/gotool`, `Constraint` (JSON Schema + value application), deterministic `sofa/tool-id/v1` tool IDs, `GoToolBlock` and `ExternalToolBlock` in `internal/tool/spec`, `tool_name()` in `internal/config/functions.go`, and block registration + cold-start/integration tests in `internal/planner`.

## Technical Context

- Go 1.27.3, `hashicorp/hcl/v2` (`hclsyntax`, `typeexpr`), `github.com/lonegunmanb/golden`, `github.com/zclconf/go-cty/cty`, `github.com/stretchr/testify`.
- Standard library `go/parser`, `go/ast`, `go/scanner`, `go/token`, `go/build` (`build.FindOnly` for stdlib detection) — strictly zero `go get`, zero `packages.Load`, zero subprocesses or network calls.
- Tier 1 fast local unit tests only (`go test ./... -count=1`).

## Constitution Check

- **I. Spec-First Delivery**: PASS — complete spec, plan, research, data-model, contracts, quickstart, and TDD tasks authored before production code.
- **II. Strict TDD**: PASS — every task group requires failing Red tests before minimal Green implementation.
- **III. Pure Core & Real-Dependency Boundaries**: PASS — pure offline domain packages (`internal/spec`, `internal/tool/gotool`, `internal/tool/spec`) with zero `os/exec` or network coupling.
- **IV. Runtime Enforcement Over Prompt Trust**: PASS — deterministic type constraints, AST validation, and static postcondition checks enforced in Go code.
- **V. Parity & Traceability**: PASS — deterministic `sofa/tool-id/v1` canonical-address-scoped IDs and preserved condition expressions.
- **VI. Research-First & Surgical Sizing**: PASS — inspected `golden`, `sofa` slices `001`–`004a`, and `r42`; delegates meta-attributes (`golden.MetaAttributeNames`), meta-nested-blocks (`golden.MetaNestedBlockNames`), `dynamic` expansion (`hb.ExpandDynamicBlocks`), and DAG execution directly to `golden`.

## Project Structure

```text
internal/
├── spec/
│   ├── condition.go
│   ├── condition_test.go
│   ├── response.go
│   └── response_test.go
├── tool/
│   ├── gotool/
│   │   ├── analyze.go
│   │   └── analyze_test.go
│   └── spec/
│       ├── constraint.go
│       ├── constraint_test.go
│       ├── identity.go
│       ├── identity_test.go
│       ├── schema.go
│       └── schema_test.go
├── config/
│   ├── functions.go          # add tool_name()
│   └── functions_test.go
└── planner/
    ├── config.go             # register GoToolBlock, ExternalToolBlock
    ├── cold_start_test.go    # extend cold-start coverage
    └── tools_test.go
```

## Design Decisions

1. **Pure AST `go_tool` Analyzer**: Parse `"package gotoolwrap\n" + source` with `go/parser.ParseFile` (`parser.AllErrors`) and analyze `ast.File` directly instead of `go/types` + `packages.Load`. This allows `dependencies` (such as `github.com/google/uuid@v1.6.0`) to be validated syntactically and imported in `source` completely offline without `go get` or module cache lookups, while enforcing `Input` as a `struct`, `Output` as any supported mappable type, and `Invoke(context.Context, Input) (ToolResponse[Output], error)`.
2. **Always Analyze `go_tool.source`**: `GoToolBlock.Decode` always validates `dependencies` and calls `gotool.AnalyzeWithDependencies`, even when no `postcondition` blocks exist, catching invalid signatures or types at Plan time and exposing `InputConstraint()` / `OutputConstraint()`.
3. **Strict `external_tool.input_type` Object Rule**: `ExternalToolBlock.Decode` enforces `inputConstraint.Type().IsObjectType()` so tool inputs always form JSON Objects.
4. **Explicit `= null` on Optional Attributes**: `go_tool.dependencies = null` resolves to `[]string{}` and `external_tool.working_dir = null` resolves to `""`; required attributes reject `null`.
5. **Golden Meta Preservation & `dynamic "postcondition"` Expansion**: `GoToolBlock.Decode` and `ExternalToolBlock.Decode` filter attributes/nested blocks via `golden.MetaAttributeNames.Contains` and `golden.MetaNestedBlockNames.Contains`, reject `dynamic` blocks targeting anything other than `postcondition`, and expand `dynamic "postcondition"` locally (binding `postcondition = { key, value }` in a child `EvalContext` for `error_message` while statically checking `condition` against `input`/`output` placeholders) instead of `golden.HclBlock.ExpandDynamicBlocks` which would eagerly evaluate `input`/`output` prematurely.
6. **Deterministic IDs**: Hash `"sofa/tool-id/v1\x00" + blockType + "\x00" + canonicalAddress`, format UUIDv8/v4-masked 36-char string, sanitize and truncate block name so `"tool_" + blockType + "_" + safeName + "_" + uuid` is `<= 64` characters, and override both `Id()` and `BaseValues()["id"]` plus expose `"id"` in `Value()`.

## Post-Design Constitution Check

All principles I–VI pass. `internal/config` remains decoupled from `internal/tool` and `internal/planner`; `tool_name()` inspects the structural `cty.Object` contract (`kind`, `address`, `id`) rather than importing tool block types.
