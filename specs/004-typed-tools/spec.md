# Feature Specification: Typed Tools (`go_tool` and `external_tool`)

**Feature Branch**: `004-typed-tools`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 004 from `docs/stage1/004-typed-tools.md`: add strongly typed custom tool declarations (`go_tool` and `external_tool`), reusable condition and response primitives, offline Go AST contract analysis, JSON Schema generation from type constraints, `postcondition` Plan-time type checking, deterministic cross-module tool IDs, and the `tool_name()` HCL function.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Declare and statically verify inline Go tools (Priority: P1)

A workflow author declares a `go_tool "<name>"` block with a description, inline Go source code, optional module dependencies, and optional `postcondition` assertions. During offline planning, the planner verifies syntax, dependency declarations, `Input`/`Output` definitions, `Invoke` signature, and `postcondition` expressions without running `go get` or external compilers.

**Independent Test**: Plan valid single-file and dependency-declaring `go_tool` blocks plus invalid syntax, invalid dependencies, missing/non-struct `Input`, missing `Output`, wrong `Invoke` signature, unsupported field types, and invalid `postcondition` cases.

**Acceptance Scenarios**:

1. **Given** a `go_tool` with a valid `struct` `Input`, supported `Output`, and `func Invoke(context.Context, Input) (ToolResponse[Output], error)`, **When** planning runs, **Then** the tool is accepted and its input/output JSON Schemas are derived.
2. **Given** a `go_tool` with declared `dependencies` in `module@version` format and matching imports, **When** planning runs offline, **Then** planning succeeds without network access or running `go get`.
3. **Given** a `go_tool` missing `Input`, `Output`, or `Invoke`, having a non-struct `Input`, having a wrong `Invoke` signature, using unsupported types (`chan`, `func`, `[]byte`, non-string map keys, embedded/unexported fields, recursive types, custom JSON/text methods), or having invalid `dependencies` (such as `"-modfile=x"` or relative paths), **When** planning runs, **Then** planning fails with a clear diagnostic even when no `postcondition` block is present.
4. **Given** a `go_tool` with a `postcondition` referencing an unknown field such as `input.typo` or returning a non-boolean expression, **When** planning runs, **Then** planning fails during static pre-check.

---

### User Story 2 - Declare and verify external process tools (Priority: P1)

A workflow author declares an `external_tool "<name>"` block with a description, command `program`, optional `working_dir`, object-typed `input_type`, `output_type`, and optional `postcondition` assertions. Planning validates the command list and type constraints and produces JSON Schemas without checking whether the executable exists on the planner host.

**Independent Test**: Plan valid `external_tool` blocks (including optional attributes with defaults and explicit `null` `working_dir`), non-object `input_type`, unsupported `any` types, missing/blank `program`, and invalid `postcondition` cases.

**Acceptance Scenarios**:

1. **Given** an `external_tool` with an `object({...})` `input_type` containing `optional(...)` defaults and a concrete `output_type`, **When** planning runs without the `program` executable installed locally, **Then** planning succeeds and generates JSON Schema with `required` and property definitions.
2. **Given** an `external_tool` with a non-object `input_type` (such as `string`) or dynamic `any` constraint, **When** planning runs, **Then** planning fails.
3. **Given** an `external_tool` with missing, null, empty, or blank-executable `program`, or wrong-type `working_dir`, **When** planning runs, **Then** planning fails; explicit `working_dir = null` is treated as unset.
4. **Given** an `external_tool` with `postcondition` blocks, **When** planning runs, **Then** `input` and `output` placeholders are checked statically and known-false conditions fail planning.

---

### User Story 3 - Generate deterministic tool IDs and resolve `tool_name()` (Priority: P1)

Every `go_tool` and `external_tool` computes a deterministic, canonical-address-scoped ID capped at 64 characters and exposes it through `.id` and `tool_name(tool_ref)`.

**Independent Test**: Plan identical configurations twice, plan configurations with long names, compare IDs across distinct canonical module addresses, and evaluate `tool_name(...)` on valid and invalid inputs.

**Acceptance Scenarios**:

1. **Given** identical `go_tool` or `external_tool` declarations planned twice, **When** `.id` is inspected, **Then** both plans produce byte-identical IDs (`tool_go_tool_<name>_<uuid>` / `tool_external_tool_<name>_<uuid>`) of length `<= 64`.
2. **Given** the same tool name under two distinct canonical module addresses, **When** IDs are computed, **Then** the resulting IDs differ.
3. **Given** a valid typed tool value passed to `tool_name(go_tool.x)` or `tool_name(external_tool.y)`, **When** HCL evaluates the expression, **Then** it returns the tool's deterministic ID; invalid or non-tool arguments fail evaluation.

---

### Edge Cases

- Both `go_tool` and `external_tool` preserve `golden` meta-attributes (`for_each`, `depends_on`) and meta-nested-blocks (`precondition`, `dynamic`).
- `Condition.Evaluate` restricts variables strictly to provided keys, rejects non-boolean results, defers unknown boolean results, and surfaces `error_message` on known false results.
- `ToolResponse[T].Validate` enforces mutual exclusion between `accepted = true` with `issues` and `accepted = false` with `output` or empty `issues`.
- `go_tool` `dependencies = null` is accepted as an empty list, while `[null]` or `[""]` is rejected.
- `go_tool` syntax errors include line/column information from Go parsing; `package` clauses and `func main` are rejected.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/spec.Condition` MUST parse and evaluate HCL assertion expressions against explicit variable roots, reject undeclared variable roots and non-bool results, defer wholly-unknown bool results (`known=false, err=nil`), and return `ErrorMessage` when known false.
- **FR-002**: `internal/spec.Issue` and `internal/spec.ToolResponse[T]` MUST validate acceptance/rejection invariants and non-empty issue `Code` and `Message`.
- **FR-003**: `go_tool` AST analysis MUST operate 100% offline using standard library `go/parser` and `go/ast` without invoking `go get`, `packages.Load`, or external compilers, even when `dependencies` is non-empty.
- **FR-004**: `go_tool` `dependencies` MUST validate every entry as a non-empty `module@version` string free of whitespace, leading `-`, or local path replacements.
- **FR-005**: `go_tool` source MUST NOT declare a `package` clause or `func main`, MUST only import Go standard library packages or declared `dependencies`, MUST declare defined `Input` (`struct`) and `Output` types, and MUST declare `func Invoke(context.Context, Input) (ToolResponse[Output], error)`.
- **FR-006**: `go_tool` AST type mapping MUST reject recursive types, imported named types, `[]byte`, non-string map keys, unexported/embedded struct fields, duplicate JSON field names, unsupported JSON tag options, and custom JSON/text marshaling methods, while treating pointer `*T` and `omitempty` fields as optional.
- **FR-007**: `GoToolBlock.Decode` MUST analyze `source` and `dependencies` during Plan even when no `postcondition` block is declared.
- **FR-008**: `external_tool` MUST require non-empty known `description`, non-empty known `program` (`list(string)` with non-blank `program[0]`), an `object({...})` `input_type`, and a concrete `output_type` validated via `spec.ValidateType`, with optional `working_dir` accepting `null` as unset and never checking local executable presence.
- **FR-009**: Both `go_tool` and `external_tool` MUST preserve `golden.MetaAttributeNames` and `golden.MetaNestedBlockNames` and support `postcondition` blocks statically evaluated against `UnknownVal` `input`/`output` placeholders.
- **FR-010**: `Constraint` MUST apply `typeexpr.Defaults`, enforce wholly-known non-null required shapes, and convert concrete `cty.Type` values to JSON Schema (`string`, `number`, `boolean`, `array`, `object` with `required` and `additionalProperties`).
- **FR-011**: Deterministic tool IDs MUST hash `"sofa/tool-id/v1\x00" + blockType + "\x00" + canonicalAddress`, format a 36-char UUID, sanitize/truncate the block name so total length is `<= 64`, and be exposed via overridden `Id()`, `BaseValues()["id"]`, and `Value()`.
- **FR-012**: `internal/config.Functions` MUST register `tool_name(tool_ref)` returning the deterministic tool ID for valid typed tool references, and `internal/planner` MUST register `GoToolBlock` and `ExternalToolBlock`.

### Key Entities

- **Condition**: Saved HCL assertion `{Expression, ErrorMessage}` evaluated statically at Plan time and deterministically at runtime.
- **ToolResponse[T] / Issue**: Generic structured response contract returned by Go tools.
- **Constraint**: Wrapper around `cty.Type` and `*typeexpr.Defaults` supporting runtime value validation and JSON Schema generation.
- **GoToolBlock / ExternalToolBlock**: Registered `golden.PlanBlock` / `golden.CustomDecode` / `golden.SingleValueBlock` declarations for typed tools.

## Success Criteria *(mandatory)*

- **SC-001**: 100% of acceptance scenarios and mandatory TDD cases in `docs/stage1/004-typed-tools.md` have Red-first tests and pass.
- **SC-002**: `gofmt -l .` produces zero output, `go vet ./...` exits 0, and `go test ./... -count=1` passes completely offline in seconds with zero regressions to slices 001–004a.
- **SC-003**: Repeated plans produce identical `<= 64`-char tool IDs, and distinct canonical module addresses produce distinct IDs.

## Assumptions

- `go/build.Default.Import(path, "", build.FindOnly)` is used strictly to verify local Go standard library `Goroot` membership without network calls.
- Runtime execution of `go_tool` and `external_tool` (compiling plugins, spawning processes, and evaluating `postcondition` with real inputs/outputs) is deferred to Stage 2/3; Stage 1 only plans, validates, and derives schemas/IDs.
