# Feature Specification: Providers, MCP Servers, Sandboxes, and Root Defaults

**Feature Branch**: `003-providers-sandbox-defaults`

**Created**: 2026-11-25

**Status**: Approved

**Input**: User description: "Implementation slice 003 from docs/stage1: add offline model provider, MCP server, and sandbox declarations plus reusable root singleton selection for sandbox and model provider."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Configure a model provider safely (Priority: P1)

A workflow author declares a model protocol endpoint and optional retry policy. Planning validates protocol combinations, credential exclusivity, retry regexes, and hides literal secrets.

**Independent Test**: Plan valid and invalid provider combinations, missing credentials, malformed regexes, and literal credentials.

**Acceptance Scenarios**:

1. **Given** an OpenAI or Azure endpoint with valid wire/transport settings, **When** planning runs, **Then** the provider is accepted.
2. **Given** Anthropic with wire API or transport, **When** planning runs, **Then** planning fails.
3. **Given** websockets with completions, **When** planning runs, **Then** planning fails.
4. **Given** more than one authentication field, **When** planning runs, **Then** planning fails.
5. **Given** a literal API key or bearer token, **When** its provider value is formatted, **Then** it is redacted.
6. **Given** malformed retry regexes or conflicting retry bounds, **When** planning runs, **Then** planning fails.

---

### User Story 2 - Configure an MCP server and stable IDs (Priority: P1)

A workflow author declares an HTTP or stdio MCP server and selected tools/resources. Planning validates transport, timeout, and selection, then publishes deterministic IDs.

**Independent Test**: Plan both transports, invalid combinations/selection/timeouts, duplicate entries, and canonical-address ID collisions.

**Acceptance Scenarios**:

1. **Given** exactly one valid HTTP or stdio transport, **When** planning runs, **Then** the server is accepted.
2. **Given** zero or two transports, **When** planning runs, **Then** planning fails.
3. **Given** no tools and no resources, **When** planning runs, **Then** planning fails.
4. **Given** duplicate or empty tool/resource names, **When** planning runs, **Then** planning fails.
5. **Given** a timeout outside one second through five minutes, **When** planning runs, **Then** planning fails.
6. **Given** valid tool/resource selections, **When** the server value is inspected, **Then** each selection has a stable deterministic ID scoped by canonical address.

---

### User Story 3 - Configure an isolated sandbox (Priority: P1)

A workflow author declares a supported container runtime, image, network, read-only mounts, and resource limits. Unsafe host mounts and invalid layouts fail planning.

**Independent Test**: Plan every runtime/network value, duplicate/reserved/invalid mount targets, mutable mounts, malformed resources, and multiple resource blocks.

**Acceptance Scenarios**:

1. **Given** a supported runtime and valid image, **When** planning runs, **Then** the sandbox is accepted with a default network of none.
2. **Given** duplicate, relative, root, workspace, or `.sofa` mount targets, **When** planning runs, **Then** planning fails.
3. **Given** a mutable extra host mount, **When** planning runs, **Then** planning fails.
4. **Given** invalid CPU or memory values or multiple resources blocks, **When** planning runs, **Then** planning fails.

---

### User Story 4 - Resolve root infrastructure defaults (Priority: P1)

The planner selects one root sandbox and at most one provider for consumers, while reporting zero or multiple candidates clearly.

**Independent Test**: Exercise 0/1/2 sandbox and provider declarations and inspect selected names/errors.

**Acceptance Scenarios**:

1. **Given** no sandbox, **When** a default is requested, **Then** planning returns an actionable error.
2. **Given** one sandbox, **When** a default is requested, **Then** its name is returned.
3. **Given** multiple sandboxes, **When** a default is requested, **Then** the error lists all candidates.
4. **Given** no provider, **When** a default is requested, **Then** nil indicates the bundled provider default.
5. **Given** multiple providers, **When** a default is requested, **Then** the error lists all candidates.

### Edge Cases

- Provider endpoint paths ending in `/chat/completions` or `/responses` are rejected as not API roots.
- Provider header maps must be wholly known and may contain sensitive path marks.
- MCP HTTP headers, arguments, environment maps, and references are validated deterministically.
- MCP IDs differ for equal names under different canonical module addresses.
- Sandbox entrypoints cannot be explicitly empty.
- Mount target cleanup prevents path-alias duplicate attacks.
- Root defaults never perform network, container, or credential lookup operations.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Provider type MUST be `openai`, `azure`, or `anthropic`.
- **FR-002**: Provider endpoint MUST be a valid HTTP(S) API root.
- **FR-003**: Provider wire API and transport defaults/combinations MUST be validated.
- **FR-004**: Anthropic MUST reject wire API and transport.
- **FR-005**: Provider authentication fields MUST be mutually exclusive and non-empty.
- **FR-006**: Literal provider secrets MUST carry sensitivity marks.
- **FR-007**: Provider headers MUST be a wholly known string map and retain sensitivity marks.
- **FR-008**: Provider retry values and regexes MUST be validated with the documented defaults.
- **FR-009**: MCP MUST require exactly one HTTP or stdio transport.
- **FR-010**: MCP timeout MUST default to 30 seconds and stay within 1 second through 5 minutes.
- **FR-011**: MCP tools/resources MUST contain at least one selection and reject empty or duplicate values.
- **FR-012**: MCP HTTP and stdio sub-schemas MUST be validated.
- **FR-013**: MCP tool/resource IDs MUST be deterministic and canonical-address-scoped.
- **FR-014**: Sandbox runtime, image, entrypoint, and network MUST be validated.
- **FR-015**: Sandbox mount targets MUST be absolute, unique, non-root, and outside reserved roots.
- **FR-016**: Sandbox extra host mounts MUST be read-only.
- **FR-017**: Sandbox resources MUST enforce at most one block and valid CPU/memory strings.
- **FR-018**: Root defaults MUST inspect source, sandbox, and provider declaration names.
- **FR-019**: Sandbox default resolution MUST fail for zero or multiple candidates.
- **FR-020**: Provider default resolution MUST return nil for zero and fail for multiple candidates.
- **FR-021**: All behavior MUST remain offline and deterministic.

### Key Entities

- **Model provider**: Protocol type, endpoint, transport settings, headers, authentication reference/literal, and retry policy.
- **MCP server**: Selected tools/resources, timeout, HTTP/stdio transport, and deterministic IDs.
- **Sandbox**: Runtime, image, network, read-only mounts, and resource limits.
- **Root defaults**: Ordered source, sandbox, and model-provider names used by downstream consumers.

## Success Criteria *(mandatory)*

- **SC-001**: 100% of acceptance scenarios have failing-first tests and pass.
- **SC-002**: Every invalid configuration returns a category-specific error without panic.
- **SC-003**: MCP IDs are byte-identical across repeated plans and differ across canonical addresses.
- **SC-004**: Full fast tests pass with all 001/002 regressions and no external daemon.
- **SC-005**: Provider literal secrets never appear in formatted sensitive output.

## Assumptions

- r42's provider and MCP protocol semantics are the baseline where the Stage 1 blueprint does not override them.
- Credential references are validated syntactically but not resolved during offline planning.
- Container runtime execution and MCP connectivity are deferred.
