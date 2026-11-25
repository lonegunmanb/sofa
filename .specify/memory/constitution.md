# sofa (SOftware FActory) Constitution

## Core Principles

### I. Spec-First Delivery (NON-NEGOTIABLE)

Every subsystem, workflow capability, API contract, and UI surface MUST be specified and approved through the Spec Kit workflow (`spec.md` → `plan.md` → `tasks.md`) before production code is written. `README.md` captures architectural intent, while `specs/<###-feature>/` defines frozen, testable behavior for each slice. Design ambiguities MUST be surfaced and resolved during specification or research rather than improvised during implementation.

### II. Strict Test-Driven Development (NON-NEGOTIABLE)

All Go and Vue production code MUST follow the Red-Green-Refactor cycle. Notwithstanding generic template defaults, test tasks are MANDATORY in every `tasks.md`:

- **Red**: Write automated tests derived directly from `spec.md` acceptance scenarios and `contracts/` definitions first, and verify that they fail for the expected reason.
- **Green**: Write the minimal production code required to make the failing tests pass.
- **Refactor**: Clean up duplication and sharpen abstractions while keeping all tests green.

Writing production business logic before a failing test exists is prohibited.

### III. Pure Core & Real-Dependency Boundaries

Core domain logic—including HCL schema decoding, DAG planning, dynamic worker expansion, fallback routing, Actor state machines, execution epoch fencing, diary interleaving, and checkpoint SHA reconciliation—MUST be modeled as deterministic, side-effect-isolated packages driven by fast in-memory unit tests. External infrastructure boundaries (Git repositories, container runtimes such as Podman/Docker/nerdctl/Kirk, Dapr Actors/sidecars, object/artifact storage, and filesystem mounts) MUST sit behind explicit Go interfaces and be verified by reproducible integration tests running against real dependencies rather than fragile mocks of CLI flags or daemon internals.

### IV. Runtime Enforcement Over Prompt Trust

Safety, isolation, and quality invariants MUST be enforced deterministically by trusted runtime mechanisms rather than LLM prompt instructions or self-reported Agent text:

- Filesystem read/write boundaries and private scratch environments MUST be enforced via container sandboxes and volume mounts.
- Quality control (QC) and deterministic gates MUST rely on verified command exit codes, frozen candidate trees, test artifacts, and exact Git commit SHAs—never an Agent claiming "passed" in prose.
- Stale or superseded worker executions MUST be fenced off via monotonic execution epochs so zombie processes cannot overwrite checkpoints or publish results.
- Untrusted Agent-generated HTML prototypes and artifacts MUST be rendered in isolated contexts that cannot access dashboard credentials or submit human decisions.

### V. Single-Node & Distributed Parity with End-to-End Traceability

Standalone local-directory mode and distributed Dapr Actor mode MUST share identical domain semantics, management APIs, recovery rules, and idempotency guarantees. Every Run, DAG node, Work/QC Session, execution epoch, structured diary entry, human intervention/approval, Git work branch registration, checkpoint, and immutable Artifact version MUST be persisted as auditable state so any delivered software change can be traced back to its requirement, verification evidence, recovery lineage, and human decisions.

## Technology Stack & Architectural Constraints

- **Backend, DSL & Agent Runtime**: Go (`module github.com/lonegunmanb/sofa`), using:
  - `hashicorp/hcl/v2` and `github.com/lonegunmanb/golden` as the HCL-to-DAG planning and execution engine (with `lower_snake_case` block/attribute conventions aligned with `github.com/lonegunmanb/r42`). Contributors MUST delegate HCL block registration/decoding, `variable`/`locals`/`for_each`/`depends_on` evaluation, and DAG construction/traversal directly to `golden` (`AsHclBlocks`, `InitConfig`, `BaseBlock`, `PlanBlock`, `ApplyBlock`) rather than re-implementing custom HCL loaders or DAG engines.
  - `github.com/github/copilot-sdk/go` (using its bundled Copilot CLI by default) as the in-container Session runtime for Work and QC agents, preserving `r42`-compatible `model_provider`, `mcp_server`, typed tools (`go_tool` / `external_tool`), skills, and retry semantics.
  - `dapr/go-sdk` for distributed Actor orchestration (`OperatorActor`, `NodeActor`, `MergeActor`, `ProcessRegistryActor`) without requiring Kubernetes.
- **Frontend (Dashboard & Human Decision UI)**: Vue 3 + TypeScript + Vite (with supporting UI/state libraries justified in the relevant feature `plan.md`), consuming the same HTTP/JSON management API used by the CLI and external callers such as The Machine.
- **Git & Branch Hygiene**: Workers MUST never mutate `source` branches directly; every worker execution MUST register its `work_branch` before creation from a pinned commit SHA, and Run completion or deletion MUST clean up registered intermediate branches while preserving `source` and `preserve`-matched branches.
- **Test Execution Tiers**:
  - Fast unit & contract tests MUST execute via `go test ./...` with zero external daemons required.
  - Real-dependency integration tests (Git remotes, Podman containers, Dapr sidecar) MUST be isolated behind explicit Go build tags or dedicated integration test targets so both fast inner-loop TDD and full boundary verification remain reliable.

## Development Workflow & Quality Gates

1. **Specify (`/speckit-specify` & `/speckit-clarify`)**: Define user/operator stories, deterministic invariants, failure/recovery cases, and measurable acceptance criteria in `specs/<###-feature>/spec.md`.
2. **Plan (`/speckit-plan`)**: Resolve technical unknowns in `research.md`, define state machines and schemas in `data-model.md` and `contracts/`, and pass the **Constitution Check** gate in `plan.md`.
3. **Tasks (`/speckit-tasks` & `/speckit-analyze`)**: Produce dependency-ordered tasks in `tasks.md` where failing test tasks precede implementation tasks for every story.
4. **Implement (`/speckit-implement`)**: Execute tasks strictly via Red-Green-Refactor, keeping commits atomic and ensuring `go vet ./...`, `go test ./...`, and applicable integration/frontend suites pass before marking a story complete.

## Governance

This Constitution supersedes ad-hoc conventions and generic template defaults across the repository. Every specification, implementation plan, task list, and code review MUST verify compliance with Principles I–V; any architectural exception MUST be explicitly justified in the `Complexity Tracking` table of the feature's `plan.md` and approved by the human maintainer.

Amendments to this Constitution require explicit user approval, an updated Sync Impact Report during review, alignment updates to `AGENTS.md` if workflow rules change, and a semantic version bump:

- **MAJOR**: Removal or backward-incompatible redefinition of a Core Principle or governance gate.
- **MINOR**: Addition of a new principle/section or material expansion of binding constraints.
- **PATCH**: Wording clarifications, formatting fixes, or non-semantic refinements.

Runtime guidance for AI coding agents is maintained in `AGENTS.md` at the repository root and MUST stay consistent with this Constitution.

**Version**: 1.0.0 | **Ratified**: 2026-11-25 | **Last Amended**: 2026-11-25
