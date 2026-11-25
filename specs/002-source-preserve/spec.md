# Feature Specification: Source Repositories and Preserved Branches

**Feature Branch**: `002-source-preserve`

**Created**: 2026-11-25

**Status**: Approved

**Input**: User description: "Implementation slice 002 from docs/stage1: add offline `source` and `preserve` planning blocks, validate repository and branch references without network access, and infer a single root source's URL when `preserve.url` is omitted."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Declare a valid Git source (Priority: P1)

A workflow author declares where the run starts, including a repository address, source branch, and optional credential-reference names. Invalid repository or credential declarations stop planning with actionable errors before any network or Git operation occurs.

**Why this priority**: Every later worker and merge operation needs one trustworthy repository and branch baseline.

**Independent Test**: Plan configurations containing supported repository formats, invalid URLs, embedded credentials, invalid branch names, invalid credential references, and both credential-reference fields.

**Acceptance Scenarios**:

1. **Given** an HTTPS, SSH, SCP-style, file, or local-path repository address with a valid branch, **When** planning runs, **Then** the source is accepted offline.
2. **Given** an HTTP(S) address containing an embedded password, **When** planning runs, **Then** planning fails and directs the author to a credential reference.
3. **Given** a branch with whitespace, `..`, a leading slash, or other prohibited syntax, **When** planning runs, **Then** planning fails with a branch-name error.
4. **Given** both token and SSH-key references, **When** planning runs, **Then** planning fails because they are mutually exclusive.
5. **Given** a valid credential-reference name whose host environment variable is absent, **When** planning runs, **Then** planning succeeds without reading the environment.
6. **Given** a valid source consumed downstream, **When** its exposed value is inspected, **Then** the branch, URL, identity, and an unknown commit revision are available.

---

### User Story 2 - Preserve a known or deferred branch (Priority: P1)

A workflow author declares a branch that must survive run cleanup. The branch may be a literal value or a reference determined only during execution; planning validates the static type and preserves the original expression.

**Why this priority**: Cleanup must not delete protected delivery branches, including branches produced by later workflow nodes.

**Independent Test**: Plan preserve declarations with known valid and invalid branches, unknown string references, and unknown non-string references.

**Acceptance Scenarios**:

1. **Given** a known valid branch, **When** planning runs, **Then** the preserve declaration is accepted.
2. **Given** a known invalid branch, **When** planning runs, **Then** planning fails immediately.
3. **Given** an unknown string-producing reference, **When** planning runs, **Then** planning succeeds, records the original expression, and creates an upstream dependency.
4. **Given** an unknown non-string reference, **When** planning runs, **Then** planning fails with a string-type error.
5. **Given** a null or missing branch, **When** planning runs, **Then** planning fails.

---

### User Story 3 - Infer the single root source URL (Priority: P1)

A workflow author omits `preserve.url` when the root has exactly one source. Planning binds the preserve declaration to that source and records the dependency; zero or multiple sources require a clear error instead of ambiguous inference.

**Why this priority**: Omission is safe only when it is provably unambiguous, and the inferred dependency must participate in the planning graph.

**Independent Test**: Plan preserve declarations with zero, one, and two root source declarations, plus the explicit-URL variant with two sources.

**Acceptance Scenarios**:

1. **Given** no root source and an omitted preserve URL, **When** planning runs, **Then** planning fails and names the preserve declaration.
2. **Given** exactly one root source and an omitted preserve URL, **When** planning runs, **Then** the source URL is inferred and the preserve node depends on that source.
3. **Given** multiple root sources and an omitted preserve URL, **When** planning runs, **Then** planning fails as ambiguous and lists the source names.
4. **Given** multiple root sources and an explicit URL, **When** planning runs, **Then** planning succeeds and uses only the authored dependency.

### Edge Cases

- Repository addresses containing whitespace or control characters are rejected.
- Credential-reference names are validated syntactically but their values are never read during offline planning.
- An unknown branch remains deferred without being converted to an empty string.
- A preserve URL must be known during planning even when its branch is unknown.
- Unsupported attributes, unsupported nested blocks, `for_each`, and multiple labels are rejected.
- Explicit preserve URLs remain untouched by automatic source inference.
- Source and preserve values remain deterministic across repeated plans.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A named `source` declaration MUST require a repository URL and branch.
- **FR-002**: Supported repository addresses MUST include HTTP(S), SSH, file, local-path, and SCP-style forms.
- **FR-003**: Repository addresses MUST reject whitespace and control characters.
- **FR-004**: HTTP(S) repository addresses MUST reject embedded password credentials and indicate that a credential reference should be used.
- **FR-005**: Source branches MUST satisfy the documented safe Git branch-name restrictions.
- **FR-006**: Token and SSH-key references MUST each be optional, syntactically valid environment-variable names.
- **FR-007**: Token and SSH-key references MUST NOT be accepted together.
- **FR-008**: Planning MUST NOT read environment variables named by credential references.
- **FR-009**: A source's exposed value MUST identify its address, kind, name, URL, and branch.
- **FR-010**: A source's exposed commit revision MUST remain unknown until execution locks the baseline.
- **FR-011**: A named `preserve` declaration MUST support a known or unknown string branch and retain its source expression.
- **FR-012**: A known preserve branch MUST pass the same branch-name validation as a source branch.
- **FR-013**: An unknown preserve branch MUST pass only when its static type is string.
- **FR-014**: Unknown non-string, null, or missing preserve branches MUST fail planning.
- **FR-015**: Preserve URLs MUST be known non-empty valid repository addresses.
- **FR-016**: Omitted preserve URLs MUST fail when the root has no source.
- **FR-017**: Omitted preserve URLs MUST infer the sole root source URL and create a dependency on that source.
- **FR-018**: Omitted preserve URLs MUST fail as ambiguous when the root has multiple sources.
- **FR-019**: Explicit preserve URLs MUST bypass automatic inference.
- **FR-020**: Source and preserve planning MUST remain fully offline and perform no repository command or network call.
- **FR-021**: Source and preserve declarations MUST reject unknown attributes, unsupported nested blocks, and unsupported collection expansion.

### Key Entities

- **Source**: A named repository baseline with URL, branch, optional credential-reference names, and an execution-time commit revision.
- **Source value**: The read-only object exposed to downstream expressions.
- **Branch reference**: A value plus its original expression, allowing known values to be validated and deferred values to remain typed.
- **Preserve**: A named branch-cleanup protection with repository URL and protected branch.
- **Root source set**: The set of source declarations used to resolve omitted preserve URLs.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of acceptance scenarios have automated failing-first tests and pass after implementation.
- **SC-002**: Every invalid URL, branch, credential, or inference case returns a category-specific error; none panic.
- **SC-003**: Cold-start planning with source and preserve declarations performs zero network and zero Git subprocess operations.
- **SC-004**: Repeating the same local plan produces identical source/preserve values and dependency ordering.
- **SC-005**: The complete fast suite passes together with all 001 regressions.

## Assumptions

- The frozen Stage 1 `002` blueprint is approved scope and behavior authority.
- Local paths need not exist during planning because this slice performs no repository access.
- HTTP(S) usernames without passwords may remain in an address; embedded passwords are prohibited.
- Execution-time commit resolution and credential lookup are intentionally deferred to later slices.
- Root inference applies only when exactly one source block is declared.
