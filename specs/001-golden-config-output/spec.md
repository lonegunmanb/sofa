# Feature Specification: Configuration Foundation and Outputs

**Feature Branch**: `001-golden-config-output`

**Created**: 2026-11-25

**Status**: Approved

**Input**: User description: "Implementation slice 001 from docs/stage1: load `*.sofa.hcl` directories through the frozen planning foundation, reuse native variables and locals, expose the mandatory offline functions and path context, and support sensitive `output` declarations."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Load and validate one configuration directory (Priority: P1)

An operator points sofa at a directory containing one or more sofa configuration files. The operator receives one deterministic configuration graph, while malformed files, unsupported declarations, and invalid directory inputs produce actionable errors.

**Why this priority**: Every later Stage 1 block depends on a trustworthy, deterministic directory-loading boundary.

**Independent Test**: Create directories containing multiple supported files, ignored files, an empty directory, a top-level bare attribute, and an unsupported block; load each directory and verify the graph or exact failure category.

**Acceptance Scenarios**:

1. **Given** several valid `*.sofa.hcl` files, **When** the directory is loaded, **Then** their declarations are combined in filename order and the order is stable across repeated loads.
2. **Given** files with another extension or nested directories, **When** the directory is loaded, **Then** neither is read.
3. **Given** a directory with no `*.sofa.hcl` file, **When** it is loaded, **Then** loading fails with a message naming the directory and the missing file pattern.
4. **Given** a top-level bare attribute or an unregistered block, **When** the configuration is planned, **Then** planning fails with an actionable diagnostic.

---

### User Story 2 - Use native variables and locals with enforced types (Priority: P1)

A workflow author defines typed inputs and derived locals, supplies values through supported sources, and observes deterministic precedence and redaction for sensitive inputs.

**Why this priority**: Variables and locals are the shared value model for every later workflow block and safety-sensitive reference.

**Independent Test**: Plan configurations that vary declaration type, external value source, validation result, and sensitivity, then inspect resulting outputs and errors.

**Acceptance Scenarios**:

1. **Given** a variable without `type` or declared as `any`, **When** planning begins, **Then** planning fails before block evaluation.
2. **Given** multiple supported value sources, **When** the same variable is supplied more than once, **Then** the established precedence selects the highest-priority known value.
3. **Given** a failed variable validation, **When** planning begins, **Then** planning fails and reports the configured validation message.
4. **Given** a sensitive variable flowing through a local into an output, **When** the output is inspected, **Then** its value remains non-sensitive in raw data only while its sensitivity is represented, and formatted display redacts it.
5. **Given** a supplied value for no declared variable, **When** planning begins, **Then** planning fails with an unknown-variable error.

---

### User Story 3 - Evaluate standard planning functions and module path (Priority: P2)

A workflow author uses the standard collection, JSON typing, current directory, timestamp, and module path helpers while planning offline.

**Why this priority**: These functions are required primitives for deterministic typed expressions and for later worker and dynamic-worker slices.

**Independent Test**: Evaluate expressions independently and within a planned configuration, including a production-like Windows path.

**Acceptance Scenarios**:

1. **Given** an empty, singleton, or multi-element collection, **When** `one` is evaluated, **Then** it returns null, the sole element, or a cardinality error respectively.
2. **Given** a JSON string and an explicit type expression, **When** the JSON is decoded, **Then** the result has exactly that type.
3. **Given** an unknown JSON string and a known type expression, **When** the function is evaluated, **Then** the result is unknown but retains the explicit type.
4. **Given** `path.module` on any supported operating system, **When** the expression is evaluated, **Then** path separators are normalized to `/`.
5. **Given** a call to current-directory or local-timestamp helpers, **When** planning runs, **Then** each returns its documented value without network or daemon access.

---

### User Story 4 - Publish typed output declarations (Priority: P2)

A workflow author declares named outputs, including outputs whose values are intentionally unknown until a later execution phase. Humans can inspect names, descriptions, values, and sensitivity in a stable snapshot.

**Why this priority**: Outputs are the first user-visible result and exercise the custom block integration needed by every later slice.

**Independent Test**: Plan configurations containing known, unknown, described, invalid, and sensitive outputs, then inspect the exported snapshot and diagnostics.

**Acceptance Scenarios**:

1. **Given** a valid output, **When** planning completes, **Then** the snapshot exposes its name and value.
2. **Given** an output marked sensitive, **When** the snapshot is formatted for display, **Then** its value is redacted.
3. **Given** an output value that is unknown at planning time, **When** planning completes, **Then** the output remains declared and retains its static type.
4. **Given** an unknown attribute or invalid description or sensitivity type, **When** planning runs, **Then** planning fails with a diagnostic.
5. **Given** mutually dependent locals or an invalid dependency edge, **When** planning runs, **Then** a cycle or dependency error is returned without stack overflow.

### Edge Cases

- A path that does not exist or names a regular file instead of a directory is rejected.
- Mixed-case or near-match suffixes such as `.HCL`, `.hcl`, and `.sofa.hcl.bak` are not treated as configuration files.
- Duplicate blocks or duplicate parse diagnostics are reported without silently dropping source information.
- A variable marked `sensitive` remains sensitive through locals, lists, objects, and outputs.
- An output with a null value remains a valid declared output when its declaration is otherwise valid.
- Function evaluation never prompts interactively and never requires network access.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The configuration loader MUST scan exactly one directory level and read only regular files ending in `.sofa.hcl`, ordered by filename.
- **FR-002**: The loader MUST combine all parsed declarations into one configuration scope and preserve stable declaration ordering.
- **FR-003**: The loader MUST reject missing, non-directory, and empty inputs with errors that identify the offending path.
- **FR-004**: The planner MUST reject top-level bare attributes, unsupported block types, malformed labels, and syntax errors with actionable diagnostics.
- **FR-005**: The planner MUST reuse the planning engine's native variable, local, dependency, and cycle behavior rather than implementing a parallel evaluator.
- **FR-006**: Every variable MUST declare a concrete type; missing types and `any` MUST be rejected.
- **FR-007**: Variable evaluation MUST support defaults, environment values, default and automatic variable files, and explicit command-line assignments with the established precedence.
- **FR-008**: Variable validation MUST fail planning when its condition evaluates false and MUST include the authored error message.
- **FR-009**: Values assigned to undeclared variables MUST fail planning.
- **FR-010**: Sensitivity MUST propagate from a sensitive input through derived expressions into outputs and remain detectable during inspection.
- **FR-011**: Planning context MUST expose `path.module` using forward slashes.
- **FR-012**: Planning functions MUST provide `cwd()`, `one()`, `local_timestamp()`, and `jsondecodewithtype()` with the specified behavior.
- **FR-013**: `jsondecodewithtype()` MUST preserve the explicit result type when its JSON input is unknown.
- **FR-014**: A named output MUST accept a value, optional description, and optional sensitivity flag.
- **FR-015**: Output values MAY be unknown during planning but MUST retain their static types.
- **FR-016**: Sensitive output values MUST be redacted by the planning engine's display formatting.
- **FR-017**: Planning MUST complete deterministically and offline using local files and values only.

### Key Entities

- **Configuration directory**: One directory whose top-level sofa configuration files form a single workflow scope.
- **Configuration declaration**: A named or structural declaration from a configuration file, including source location.
- **Variable**: A named, typed planning input with optional default, description, validation, and sensitivity.
- **Local value**: A named derived expression evaluated in dependency order.
- **Output**: A named planning result with a value, optional description, and optional sensitivity.
- **Plan snapshot**: The stable collection of successfully evaluated declarations and their diagnostics.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of the mandatory acceptance scenarios have an automated failing-first test and pass after implementation.
- **SC-002**: Repeating a load and plan for the same local inputs produces identical ordering and values in 100 consecutive runs.
- **SC-003**: Every invalid input category in this specification returns a non-empty, category-specific error; none panic.
- **SC-004**: The complete fast test suite passes in an offline environment with no container runtime, Git remote, Dapr sidecar, or network call.
- **SC-005**: A configuration with sensitive input-to-output propagation never displays the underlying value in formatted plan diagnostics or reports.

## Assumptions

- The frozen Stage 1 blueprint defines the intended contract and is approved source material for this slice.
- File contents and external variable inputs are trusted only after parsing and validation; this slice provides deterministic diagnostics, not process isolation.
- Configuration files in nested directories belong to no module in this slice and are intentionally ignored.
- Current directory and local timestamp helpers return process-local values at evaluation time and are not part of idempotent snapshot equality assertions.
- Native planning-engine semantics remain authoritative for variable precedence and dependency graph behavior; this feature verifies those semantics instead of replacing them.
- User-facing output snapshots may use a stable JSON-compatible representation, but archive packaging and command-line presentation belong to later slices.
