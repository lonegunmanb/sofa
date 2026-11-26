# Feature Specification: Adopt Golden Native Output Blocks

**Feature Branch**: `004a-golden-native-output`

**Created**: 2026-11-26

**Status**: Approved

**Input**: User request to execute `docs/stage1/adopt-golden-output.md` now that `golden#18` is resolved.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Use native output planning (Priority: P1)

A workflow author uses the same `output` syntax and receives the same plan values, descriptions, sensitivity, validation, and unknown-value behavior while sofa delegates implementation entirely to golden.

**Independent Test**: Run all existing output integration tests against the upgraded native block and inspect planner snapshots.

**Acceptance Scenarios**:

1. **Given** a known output, **When** planning runs, **Then** the planner snapshot contains the name, value, description, and sensitivity.
2. **Given** sensitive input propagated through locals, **When** output is formatted, **Then** it is redacted.
3. **Given** an unknown output value, **When** planning runs, **Then** its exact static type is preserved.
4. **Given** missing/invalid output attributes or nested content, **When** planning runs, **Then** golden returns actionable diagnostics.
5. **Given** a fresh process loads output and another custom block before planner construction, **When** planning runs, **Then** both are recognized.

### Edge Cases

- No custom output block registration may remain.
- No block type implementing output may remain under sofa.
- Native output expression text is available for later plan serialization.
- Existing 001–003 behavior remains green.

## Requirements *(mandatory)*

- **FR-001**: The golden dependency MUST include commit `059cc8c` or newer native output support.
- **FR-002**: Sofa MUST delete its custom output block implementation and block unit tests.
- **FR-003**: Sofa MUST NOT register an `output` block with golden.
- **FR-004**: Planner output snapshots MUST be built from `golden.OutputBlock` accessors.
- **FR-005**: Snapshot ordering MUST remain deterministic by output name.
- **FR-006**: Output expression text MUST be available in the snapshot DTO for slice 007.
- **FR-007**: All existing output behavior and cold-start behavior MUST remain covered.
- **FR-008**: No Stage 1 slice 004 implementation may be included in this migration.

### Key Entities

- **Output snapshot**: Pure data containing name, value, optional description, sensitivity, and original expression.
- **Native output block**: Golden-provided block that owns decoding, evaluation, sensitivity, preconditions, and dynamic handling.

## Success Criteria *(mandatory)*

- **SC-001**: Repository search finds zero custom `OutputBlock` definitions and zero output registration calls.
- **SC-002**: All output and cold-start tests pass against the native block.
- **SC-003**: `gofmt`, `go vet ./...`, and `go test ./... -count=1` pass.
- **SC-004**: No behavior from slices 001–003 regresses.

## Assumptions

- Golden `059cc8c` is the minimum compatible revision.
- The pure snapshot DTO may remain in sofa for serialization.
- Stage 1 numeric slice 004 remains reserved for typed tools.
