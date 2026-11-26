# Feature Specification: Harness Plugin Block and Sandbox Bridge Default

**Feature Branch**: `004b-harness-and-sandbox-bridge`

**Created**: 2026-11-26

**Status**: Approved

**Input**: User description: "Implementation slice 004b from docs/stage1/004b-harness-and-sandbox-bridge.md: default sandbox.network to bridge, add tflint-aligned external harness block orthogonal to model_provider with zero bundled special cases, and extend RootDefaults with ResolveDefaultHarness."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Default sandbox network to bridge (Priority: P1)

A workflow author declares a sandbox without specifying `network` (or explicitly setting `network = null`) so containerized harnesses can reach external model endpoints by default, while explicit `network` settings (`"bridge"`, `"none"`, `"host"`) remain available.

**Why this priority**: Containerized external agent harnesses require outbound network access by default unless an author opts into `"none"` or `"host"`.

**Independent Test**: Plan a sandbox omitting `network`, a sandbox with `network = null`, and a sandbox with `network = "none"`, and verify both `sandbox.<name>.network` and `SandboxConfig().Network`.

**Acceptance Scenarios**:

1. **Given** a sandbox omitting `network`, **When** planning runs, **Then** `sandbox.<name>.network` and `SandboxConfig().Network` default to `"bridge"`.
2. **Given** a sandbox with `network = null`, **When** planning runs, **Then** `sandbox.<name>.network` and `SandboxConfig().Network` default to `"bridge"`.
3. **Given** a sandbox with `network = "none"` or `network = "host"`, **When** planning runs, **Then** the explicit network mode is preserved.

---

### User Story 2 - Configure an external harness plugin (Priority: P1)

A workflow author declares an external agent harness plugin using either GitHub Releases metadata (`source` and `version`) or local plugin directory mode (both omitted or null), with optional signature verification, environment variable name mappings, and structured options, without any built-in special cases or embedded model provider configuration.

**Why this priority**: All agent harnesses execute strictly as external `go-plugin` binaries with no bundled special cases and remain orthogonal to `model_provider` and `sandbox`.

**Independent Test**: Plan both local-mode and GitHub-Releases-mode harnesses (including `"copilot"` and custom names), verify `Value()`, `HarnessConfig()` deep-copy and sensitive mark preservation, and reject invalid sources, versions, signatures, env_refs, options, and `model_provider` attributes.

**Acceptance Scenarios**:

1. **Given** a harness omitting `source` and `version` (or setting both to `null`) for any name including `"copilot"` or `"custom"`, **When** planning runs, **Then** planning succeeds identically with `null` `source` and `version` values and no special-case privilege.
2. **Given** a harness with a valid `<host>/<owner>/<repo>` `source` and an exact SemVer `version` without a `"v"` prefix, **When** planning runs, **Then** planning succeeds, populates `SourceHost`/`SourceOwner`/`SourceRepo`, and publishes `source` and `version` in `Value()`.
3. **Given** only one of `source` or `version`, a `source` with scheme/whitespace/`.git`/`..`/wrong segment count, or a `version` with a leading `"v"` or range operator, **When** planning runs, **Then** planning fails with an actionable validation error.
4. **Given** `signature = "pgp"` without a non-blank `signing_key`, or local mode with non-default `signature` or non-null `signing_key`, or `signing_key` paired with `"none"`/`"attestation"`, **When** planning runs, **Then** planning fails.
5. **Given** `env_refs` containing valid `^[A-Za-z_][A-Za-z0-9_]*$` keys and values, **When** planning runs, **Then** planning succeeds without reading host environment variables; unknown values, null entries, or invalid names fail planning.
6. **Given** `options` as a wholly known concrete object or map (including sensitive nested attributes), **When** planning runs, **Then** `HarnessConfig().Options` preserves the value and its `SensitiveMark`; unknown values, non-object/non-map values, or dynamic/capsule types fail planning.
7. **Given** a `harness` block containing `model_provider`, **When** planning runs, **Then** decoding rejects `model_provider` as an unknown attribute.

---

### User Story 3 - Resolve root harness defaults (Priority: P1)

The planner inventories declared root `harness` names and resolves the default harness for downstream consumers using the same strict 0 / 1 / >=2 semantics as `sandbox`.

**Why this priority**: Downstream `worker` and `qc` blocks rely on deterministic root singleton resolution with clear missing or ambiguity diagnostics.

**Independent Test**: Exercise `InspectRootDefaults` and `ResolveDefaultHarness` for 0, 1, and 2 declared harnesses.

**Acceptance Scenarios**:

1. **Given** zero `harness` blocks in the root module, **When** `ResolveDefaultHarness` is called, **Then** it returns an actionable missing-harness error.
2. **Given** exactly one `harness` block in the root module, **When** `ResolveDefaultHarness` is called, **Then** it returns that harness name without error.
3. **Given** two or more `harness` blocks in the root module, **When** `ResolveDefaultHarness` is called, **Then** it returns an ambiguity error listing all candidate names.

### Edge Cases

- Sensitive variables bound to `harness` `source` or `version` retain `SensitiveMark` on `Value().GetAttr("source")` / `Value().GetAttr("version")` without panicking during decoding.
- Non-string literals (such as numbers or booleans) passed to `source`, `version`, `signature`, or `signing_key` are rejected instead of being implicitly coerced to strings.
- Unknown values passed to `source`, `version`, `signature`, `signing_key`, `env_refs`, or `options` fail cleanly during planning without panicking.
- `env_refs = null` is treated as an empty map and `options = null` is treated as unset (`cty.NilVal`).
- `HarnessBlock.Value()` returns `cty.DynamicVal` before `ExecuteDuringPlan()` completes.
- `HarnessBlock.HarnessConfig()` returns a deep copy so mutating `EnvRefs` in a caller snapshot does not affect block state.
- `HarnessBlock` delegates unknown attribute/block rejection and `for_each` / `depends_on` / `precondition` / `dynamic` meta-constructs natively to `golden`.
- Cold-start loading of a `.sofa.hcl` file containing `harness` before `NewSofaConfig` succeeds in a fresh process.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `sandbox` MUST default `network` to `"bridge"` when omitted or explicitly `null`, while continuing to accept `"bridge"`, `"none"`, and `"host"`.
- **FR-002**: `harness "<name>"` MUST be registered as a top-level `golden.PlanBlock` and `golden.SingleValueBlock` (`Type() == ""`, `BlockType() == "harness"`, `AddressLength() == 2`, `CanExecutePrePlan() == false`).
- **FR-003**: `harness` MUST rely on `golden` native decoding for unknown attributes/blocks and native meta-attributes/meta-blocks (`for_each`, `depends_on`, `precondition`, `dynamic`).
- **FR-004**: `harness` MUST reject `model_provider` as an unknown attribute to preserve orthogonality.
- **FR-005**: `harness` `source` and `version` MUST either both be set or both be omitted/`null`, with zero special-case behavior for any harness name including `"copilot"`.
- **FR-006**: When set, `source` MUST follow `<host>/<owner>/<repo>` (three non-empty segments, valid host with optional `:port` in `1..65535`, no scheme `://`, no whitespace or control characters, no `.` or `..` segments, no `.git` suffix) and store `SourceHost`, `SourceOwner`, and `SourceRepo`.
- **FR-006a**: `harness` label `<name>` MUST match `^[A-Za-z0-9][A-Za-z0-9_-]*$` to prevent empty names, leading-dash flags, or path-traversal in plugin binary/asset filenames.
- **FR-007**: When set, `version` MUST be an exact SemVer string without a leading `"v"`/`"V"` (instructing the user to omit the leading `"v"`) and without range operators (`>=`, `<=`, `~>`, `^`, `*`, `||`, whitespace).
- **FR-008**: `signature` MUST default to `"auto"` when omitted/`null` and only accept `"auto"`, `"attestation"`, `"pgp"`, or `"none"`.
- **FR-009**: `signing_key` MUST be a non-blank string when `signature == "pgp"`, MUST be rejected when `signature` is `"attestation"` or `"none"`, and local mode (omitted/`null` `source`) MUST reject explicit non-default `signature` or non-null `signing_key`.
- **FR-010**: `env_refs` MUST accept optional `map(string)` (`null` treated as empty), require wholly known non-null elements, validate all keys and values against `^[A-Za-z_][A-Za-z0-9_]*$`, and NEVER read host environment variables during planning.
- **FR-011**: `options` MUST accept optional wholly known `object` or `map` values (`null` treated as unset/`cty.NilVal`), allow known `null` attributes/elements (including untyped HCL `null`) and empty tuples/lists/maps, reject capsule types anywhere in the value/type tree, and preserve `cty.Value` sensitivity marks.
- **FR-012**: `HarnessBlock.HarnessConfig()` MUST return an isolated deep copy (`EnvRefs` cloned, `Options` preserving marks) and `Value()` MUST return `cty.DynamicVal` pre-plan and `{address, kind: "harness", name, source, version}` post-plan preserving marks on `source`/`version`.
- **FR-013**: `RootDefaults` MUST collect top-level `harness` names in `InspectRootDefaults` and expose `ResolveDefaultHarness(consumerAddr)` failing on 0 or >=2 candidates and returning the single name on 1 candidate.
- **FR-014**: `RegisterBlocks()` MUST register `&harnessspec.HarnessBlock{}` so cold-start loading and planning recognize `harness` blocks.
- **FR-015**: All behavior MUST remain offline and deterministic with zero network calls or plugin process launches.

### Key Entities

- **HarnessConfig**: Snapshot configuration containing `Name`, `Source`, `SourceHost`, `SourceOwner`, `SourceRepo`, `Version`, `Signature` (`SignatureMode`), `SigningKey`, `EnvRefs`, and `Options`.
- **SignatureMode**: Enumerated verification mode (`auto`, `attestation`, `pgp`, `none`).
- **HarnessBlock**: `golden` block adapter decoding, validating, and publishing planned harness values and snapshots.
- **RootDefaults**: Root module singleton inventory including `Sources`, `Sandboxes`, `Harnesses`, and `ModelProviders`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of acceptance scenarios and negative validation cases in `docs/stage1/004b-harness-and-sandbox-bridge.md` have failing-first tests and pass.
- **SC-002**: Zero built-in `"copilot"` special cases exist in `internal/harness` or `internal/planner`.
- **SC-003**: `gofmt -l .` produces no output, `go vet ./...` exits 0, and `go test ./... -count=1` passes 100% including cold-start and existing 001–004a tests.
- **SC-004**: Zero network requests, container commands, plugin executions, or host environment reads occur during planning.

## Assumptions

- Network download/verification of GitHub Releases belongs to slice `007` (`sofa init`), and `go-plugin` execution belongs to Stage 2.
- `sandbox` default network change from `"none"` to `"bridge"` requires updating existing tests in `internal/planner/sandbox_test.go` (and adding explicit `"bridge"`/`null`/`"none"` tests) while keeping `"none"` and `"host"` valid.
