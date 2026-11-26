# Feature Specification: Static Workers and Nested Execution Contracts

**Feature Branch**: `005-static-worker`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 005 from `docs/stage1/005-static-worker.md`.

## User Scenarios & Testing

### User Story 1 - Compose a static worker DAG (Priority: P1)

A workflow author chains workers through branches, prompts, imports, artifacts, and explicit dependencies. Planning supports static `for_each`, deferred upstream values, and correct downstream values.

**Independent Test**: Plan a four-worker chain and a static for_each worker; inspect dependencies and indexed values.

### User Story 2 - Apply orthogonal root defaults (Priority: P1)

Each worker binds missing sandbox, harness, and model provider independently from unique root defaults. Explicit source attributes, including null, prevent injection.

**Independent Test**: Exercise 0/1/2 defaults, explicit null provider, explicit alternative harness, and dependency edges.

### User Story 3 - Validate execution controls (Priority: P1)

Workers validate artifacts/imports, gatekeepers, QC overrides, fallback triggers/targets, tool use, and conditional `.result` exposure.

**Independent Test**: Plan valid/invalid gatekeeper, QC, fallback, artifact path, tool termination, and result-reference cases.

### Edge Cases

- Unknown `based_source`, prompt, system prompt, import sources, and tool inputs remain deferred.
- Known based-source values are valid branch names.
- Import paths cannot escape or alias artifacts/imports.
- Fallback targets must be positive ancestors and cannot target self, downstream, or unrelated workers.
- `result` is absent unless a terminating tool is configured.

## Requirements

- **FR-001**: Worker MUST implement `golden.Valuable`, not `SingleValueBlock`.
- **FR-002**: Worker values MUST expose branch, commit, artifacts, and conditional result.
- **FR-003**: Static for_each instances MUST be addressable by key.
- **FR-004**: Worker local context MUST expose `artifact()` and `import_artifact`.
- **FR-005**: Artifact and import names/paths MUST be unique and safe.
- **FR-006**: Imports without paths MUST default to `.sofa/imports/<name>`.
- **FR-007**: Root sandbox, harness, and provider defaults MUST be independent.
- **FR-008**: Explicitly present properties MUST never be overwritten, including null.
- **FR-009**: Required/known scalar/type rules MUST follow the blueprint.
- **FR-010**: Gatekeeper command/path/timeout/exit/worktree rules MUST validate.
- **FR-011**: QC MUST validate criteria and support heterogeneous overrides.
- **FR-012**: Fallback triggers, unique bindings, target traversal, and ancestor legality MUST validate.
- **FR-013**: Tool use wiring MUST validate exclusivity and termination cardinality.
- **FR-014**: Deferred expressions MUST be retained when allowed values are unknown.
- **FR-015**: `.result` MUST be exposed only with a terminating tool.
- **FR-016**: All behavior remains offline with no container/session execution.

## Success Criteria

- **SC-001**: All blueprint acceptance scenarios have automated tests.
- **SC-002**: No invalid configuration panic; all return diagnostics.
- **SC-003**: Full fast suite and prior slices remain green.
- **SC-004**: No external process, network, container, or model runtime invocation.

## Assumptions

- Runtime execution is deferred to a later stage.
- IDs from typed tools and MCP are syntactically validated during Plan.
- Fallback ancestry is computed from positive worker references excluding fallback edges.
