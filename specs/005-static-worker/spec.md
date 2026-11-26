# Feature Specification: Static Workers and Nested Execution Contracts

**Feature Branch**: `005-static-worker`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 005 from `docs/stage1/005-static-worker.md` and `docs/hcl-contract.md`.

## User Scenarios & Testing

### User Story 1 - Compose a static worker DAG (Priority: P1)

A workflow author chains static `worker "<name>"` blocks through `based_source`, `prompt`, `system_prompt`, `import_artifact`, `artifact`, `tool_use`, and explicit `depends_on`. Planning supports static `for_each`, deferred Apply-time values serialized into a valid HCL expression, `based_source` repository URL resolution, and conditional `.result` exposure.

**Independent Test**: Plan the four-stage `specify -> plan -> tasks -> implement` chain with `source`, `preserve`, `go_tool`, `mcp_server`, `import_artifact.<name>.path` interpolation, `artifact("<name>")`, and a static `for_each` worker; inspect DAG edges, resolved `SourceURL`, `DeferredExpression`, and indexed values.

### User Story 2 - Apply orthogonal root defaults (Priority: P1)

Each worker binds missing `sandbox`, `harness`, and `model_provider` independently from unique root defaults (`0` / `1` / `>=2` rules). Explicit source attributes, including `model_provider = null`, prevent AST injection.

**Independent Test**: Exercise 0/1/2 defaults for `sandbox`, `harness`, and `model_provider`, explicit `model_provider = null`, explicit alternative `harness`, and resulting DAG edges.

### User Story 3 - Validate execution controls, schemas, and cross-block contracts (Priority: P1)

Workers enforce strict `cty` types, knownness, whole-integer bounds, nested block schemas, `retry` policies, `tool_call_quota`, `allowed_tools`/`disallowed_tools` mandatory tool safety, `gatekeeper` checks, `qc` overrides (including explicit `model_provider = null` and heterogeneous `harness`), `fallback` triggers and positive-ancestry targets (including `locals`, `depends_on`, and `for_each` instances), and module-level `go_tool`/`external_tool`/`mcp_server` registration.

**Independent Test**: Plan valid and invalid scalar types, evaluation errors, `artifact`/`import_artifact`, `gatekeeper`, `retry`, `qc`, `fallback`, `tool_use`, `tool_call_quota`, `dynamic "gatekeeper"`, and module tool/MCP registration cases.

### Edge Cases

- HCL evaluation errors in any expression (e.g., `artifact("undeclared")`, unknown functions, missing object attributes) MUST fail planning with diagnostics rather than silently turning into `cty.DynamicVal`.
- Scalar string fields allowed to be unknown during Plan (`based_source`, `system_prompt`, `prompt`) MUST still have unmarked type `cty.String`; `based_source` and `system_prompt` MUST NOT be `null`, and known `system_prompt` MUST be non-blank.
- Scalar string, integer, bool, reference, and quota fields required to be known during Plan MUST reject unknowns, implicit number/bool-to-string coercions, fractional numbers (`1.5`), `NaN`/`Inf`, and out-of-range integers.
- Import paths default to `.sofa/imports/<name>` when omitted or `null`, cannot escape the workspace (`..`, `/abs`, `\`, drive prefixes) or target `.`, and cannot collide with another import or declared artifact path (after `path.Clean`).
- `fallback.artifacts` accepts a list of local `artifact("<name>")` objects (not raw strings and not upstream `worker.<other>.artifact.<name>`).
- `fallback.target` must be a direct `worker.<name>` or `worker.<name>[<key>]` traversal referencing an existing positive ancestor (via non-`fallback` edges including `locals` and `depends_on`), never self, downstream, or unrelated workers.
- `.result` is exposed on `worker.<name>` if and only if `terminate_tool_id` or a `tool_use { terminate = true }` is configured.

## Requirements

- **FR-001**: `WorkerBlock` MUST implement `golden.CustomDecode`, `golden.PlanBlock`, and `golden.Valuable` (NOT `SingleValueBlock`), with `Type() == ""`, `BlockType() == "worker"`, `AddressLength() == 2`, and `SofaConfig.EvalContext()` unwrapping `worker[""]`.
- **FR-002**: `WorkerBlock.Values()` MUST expose `work_branch` (`UnknownVal(String)`), `commit_sha` (`UnknownVal(String)`), `artifact` (`ObjectVal` of declared artifacts), and conditional `result` (`UnknownVal(String)` only when a terminating tool is configured).
- **FR-003**: Static `for_each` instances MUST expand via `golden` and remain addressable by key (`worker.<name>[<key>]`).
- **FR-004**: `WorkerBlock.EvalContext()` MUST expose local `artifact("<name>")` (validating non-null/non-blank declared names) and `import_artifact` (mapping each declared import to `{ path, desc }`).
- **FR-005**: Worker, artifact, import, gatekeeper, fallback, and tool_use labels MUST be non-empty safe labels; artifact/import labels MUST reject `.`, `..`, `/`, `\`; artifact, import, gatekeeper, fallback, and tool_use names MUST be unique per worker.
- **FR-006**: `import_artifact` MUST reject duplicate names, default `path` to `.sofa/imports/<name>`, require non-null list/tuple `sources` of `kind == "artifact"` objects, and prohibit referencing the current worker's own `artifact`.
- **FR-007**: Root `sandbox`, `harness`, and `model_provider` defaults MUST be injected orthogonally in `SofaConfig.Init` when the attribute is absent in source HCL.
- **FR-008**: Explicitly present attributes (including `model_provider = null`) MUST NEVER be overwritten by default injection.
- **FR-009**: All `docs/hcl-contract.md` `worker` attributes (`based_source`, `sandbox`, `harness`, `model_provider`, `model`, `profile`, `reasoning_effort`, `system_prompt`, `prompt`, `writable_paths`, `human_approval`, `checkpoint_every_rounds`, `permission`, `max_protocol_attempts`, `timeout`, `tool_ids`, `mcp_tool_ids`, `mcp_resource_ids`, `terminate_tool_id`, `tool_call_quota`, `allowed_tools`, `disallowed_tools`, `skill_directories`, `skills`, `disabled_skills`) and nested `retry` MUST be decoded, validated, and exposed via deep-copied `WorkerBlock.WorkerConfig() Config`.
- **FR-010**: `sandbox`, `harness`, and `model_provider` references on `worker` and `qc` MUST validate wholly known objects with matching `kind` (`"sandbox"`, `"harness"`, `"model_provider"`).
- **FR-011**: Every nested block (`artifact`, `import_artifact`, `gatekeeper`, `qc`, `fallback`, `tool_use`, `retry`) MUST enforce strict attribute, label, and nested-block whitelists, and `WorkerBlock.Decode` MUST support `golden` native `dynamic` nested block expansion.
- **FR-012**: `qc` MUST validate `criteria`, optional `harness`, `model_provider` (with `ModelProviderSet` distinguishing omitted vs explicit `null`), `model`, `reasoning_effort`, `permission`, `max_qc_rounds` (default 3, `> 0`), `tool_ids`, `tool_call_quota`, `allowed_tools`, `disallowed_tools`, `skill_directories`, `skills`, `disabled_skills`, and nested `retry`.
- **FR-013**: `fallback` MUST validate `on` triggers (unique trigger-to-fallback binding), `target` (`worker.<name>` or `worker.<name>[<key>]` traversal), `max_reworks` (default 2, `> 0`), `artifacts` (local `artifact("...")` objects), and positive ancestry across `locals`, `depends_on`, and `for_each` instances.
- **FR-014**: `ExecuteDuringPlan()` MUST verify that all `tool_ids`, `terminate_tool_id`, `tool_use.tool_id`, `qc.tool_ids`, `mcp_tool_ids`, and `mcp_resource_ids` are registered in the current module, validate known `tool_use.input` fields against `InputConstraint`, and resolve `Config.SourceURL` from `based_source` (including `locals`, `source`, upstream `worker`, or single root `source`).
- **FR-015**: Whenever any allowed Apply-time field (`based_source`, `system_prompt`, `prompt`, `import_artifact.sources`, `tool_use.input`) is not wholly known, `WorkerBlock` MUST serialize the complete block into a parseable HCL expression stored in `DeferredExpression`.
- **FR-016**: All behavior remains offline with no container, network, Git, or model runtime invocation.

## Success Criteria

- **SC-001**: All 11 remediation defects and Stage 1 blueprint acceptance scenarios have passing automated unit, cold-start, and planner tests.
- **SC-002**: Invalid configurations never panic or swallow diagnostics; every invalid case returns a clear error.
- **SC-003**: `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` pass with zero errors.
- **SC-004**: `internal/worker/spec` remains decoupled from `internal/planner`, `internal/tool/spec`, `internal/mcp/spec`, and `internal/source` via consumer interfaces.

## Assumptions

- Runtime execution is deferred to Stage 2/3.
- Cross-module `module "<name>"` worker planning is delivered in Slice 007.
