# AGENTS.md — sofa Workspace Rules

This repository implements **sofa (SOftware FActory)** (`github.com/lonegunmanb/sofa`). All human and AI contributors MUST follow `.specify/memory/constitution.md` and the operational rules below.

## 1. Sources of Truth

1. **Engineering Governance**: `.specify/memory/constitution.md` (binding principles and quality gates).
2. **System Vision & Domain Concepts**: `README.md` (design overview).
3. **Frozen Skeleton Contracts**: `docs/hcl-contract.md` (HCL blocks, orthogonal `sandbox`/`harness`/`model_provider` singleton defaults, `gatekeeper`, `merge`), `docs/architecture-contract.md` (Dapr Actor topology, `daprd` standalone/distributed parity, `hashicorp/go-plugin` gRPC Harness boundary & Session tool gates, `tflint`-style plugin installation, Epoch checkpoint branches, REST + WebSocket API), and `docs/harness-plugin-contract.md` (`proto/sofa/harness/v1/harness.proto` 7-RPC schema, `RunTurn` bidirectional stream, `r42`-parity hooks/quota/watchdog/retry split, and 1:1 `copilot` / `antigravity` SDK mapping).
4. **Pre-Designed Slice Blueprints**:
   - **Stage 1 (Completed)**: `docs/stage1/README.md`, `docs/stage1/001-*.md` through `009-*.md`, and `004b-harness-and-sandbox-bridge.md`.
   - **Stage 2 (Completed)**: `docs/stage2/README.md` and `docs/stage2/010-*.md` through `020-*.md`.
   - **Stage 3 & Post-Stage-3 Refinements (Completed)**: `docs/stage3/README.md`, `docs/stage3/021-*.md` through `032-*.md`, and `specs/033-*` through `specs/037-*`.
   - **Stage 4 (Detailed Blueprints)**: `docs/stage4/README.md` and `docs/stage4/038-*.md` through `041-*.md` (MUST be read and followed verbatim before running `/speckit-specify` or `/speckit-plan` on any Stage 4 slice).
5. **Active Feature Contracts**: `specs/<###-feature>/` (`spec.md`, `plan.md`, `data-model.md`, `contracts/`, `tasks.md`).

## 2. Pre-Task Investigation & Anti-Monolith Sizing (NON-NEGOTIABLE)

- **Investigate Before Deciding**: Never propose or freeze a feature slice or task on guesswork. Always inspect the actual source code and behavior of upstream libraries (`golden`, `hashicorp/go-plugin`, `dapr/go-sdk`), reference implementations (`r42`), and the existing repository *before* scoping a spec or writing a plan, so existing library capabilities are leveraged directly instead of re-invented.
- **Reject Oversized Tasks**: Refuse monolithic specs, plans, or tasks that bundle multiple subsystems, multiple unrelated HCL block families, or broad cross-cutting features into a single unit. If a requested or drafted scope is too big, stop, push back with concrete evidence from your investigation, and split it into minimal, single-purpose, independently testable slices before invoking `/speckit-specify` or writing code.

## 3. Spec-Driven Development (GitHub Spec Kit)

- **No Spec, No Code**: Never write or modify production Go or Vue code without an active feature specification and plan under `specs/<###-feature>/`.
- **Workspace Skills**: Use the installed Spec Kit skills in `.agents/skills/speckit-*`:
  - `speckit-constitution` — view/amend `.specify/memory/constitution.md`
  - `speckit-specify` / `speckit-clarify` — author and de-risk `spec.md`
  - `speckit-plan` — produce `plan.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`
  - `speckit-tasks` / `speckit-analyze` — generate TDD-ordered `tasks.md` and verify cross-artifact alignment
  - `speckit-implement` — execute `tasks.md`
- **Per-Slice Commit & Push Gate (NON-NEGOTIABLE)**: Every time a feature slice is completed and passes `gofmt`, `go vet ./...`, and `go test ./... -count=1` (plus any applicable Tier 2 / frontend tests), the agent MUST stage all slice artifacts (`specs/<###-feature>/`, source/test files, `go.mod`/`go.sum`), create an atomic `git commit`, and `git push` to the remote repository (`git status` MUST be clean) **before** starting the next slice. Never stack multiple uncommitted slices in the working tree.
- **Tooling Paths (Windows)**:
  - Specify CLI: `C:\Users\hezijie\.local\bin\specify.exe` (or `specify` if on `PATH`)
  - Helper scripts: `.specify/scripts/powershell/*.ps1`

## 4. Mandatory TDD & Testing Tiers

- **Tests Are NEVER Optional**: Override any generic template note stating tests are optional. Every story in `tasks.md` MUST list failing test tasks **before** implementation tasks.
- **Red → Green → Refactor**:
  1. Write the test first from `spec.md` / `contracts/`.
  2. Run the test and confirm it **fails** for the expected reason.
  3. Implement the minimal code to pass.
  4. Refactor while keeping tests green.
- **Two-Tier Test Strategy**:
  - **Tier 1 — Pure Core Unit Tests (`go test ./...`)**: Fast, deterministic, zero external processes/network/containers. Covers HCL decoding, DAG topological resolution & `dynamic_worker` expansion, fallback cycles, Actor state machines, epoch fencing, diary message injection, and checkpoint SHA alignment logic.
  - **Tier 2 — Real-Dependency Integration Tests (`//go:build integration`)**: Exercises thin adapter boundaries against real local Git bare/working repositories, Podman (or configured container runtime), filesystem mounts, and Dapr sidecar/Actor runtime. Do not mock CLI stdout/stderr for Git or container runtimes when testing adapters.

## 5. Tech Stack & Codebase Conventions

- **Backend**: Go (`go.mod` module `github.com/lonegunmanb/sofa`), `hashicorp/hcl/v2`, `github.com/lonegunmanb/golden` (HCL → DAG planning & execution engine), `github.com/hashicorp/go-plugin` (gRPC multi-language Agent Harness plugin boundary inside containers with zero built-in harness special cases—all harnesses such as `sofa-harness-copilot` and Python `sofa-harness-antigravity` run strictly as external plugins installed via local plugin directories or `tflint`-style GitHub Release `source` + `version` + checksums/attestations, and are incubated in isolated sub-modules under `plugins/sofa-harness-<name>/` until `harness.proto` stabilizes for standalone repo split), and `dapr/go-sdk`.
  - **NEVER Re-Implement `golden` Capabilities (NON-NEGOTIABLE — Requires Explicit User Approval)**: Any capability already provided by `golden` MUST be used directly and MUST NEVER be re-implemented, duplicated, or manually intercepted/disabled without explicit prior approval from the user. This includes:
    - HCL block registration, `gohcl` struct decoding, and unknown attribute/block rejection (`golden.RegisterBlock`, `golden.Decode`);
    - Native `variable` (precedence, `SOFA_VAR_*`, `.sofavars`, type conversion, `validation`) and `locals` evaluation;
    - Native meta-attributes (`for_each`, `depends_on` via `golden.MetaAttributeNames`) and meta-nested-blocks (`precondition`, `dynamic` via `golden.MetaNestedBlockNames`) — NEVER write custom checks to forbid or re-implement `for_each`, `depends_on`, `precondition`, or `dynamic` on blocks (when implementing `CustomDecode` for `cty.UnknownVal` fields, always delegate meta filtering to `golden.MetaAttributeNames` / `golden.MetaNestedBlockNames`; the ONLY approved exception is `006` `dynamic_worker` whose `dynamic "worker"` iterates over Apply-time `cty.UnknownVal` outputs with `iterator`, matching `r42`);
    - DAG vertex/edge construction, cycle detection, and topological plan/apply traversal (`AsHclBlocks`, `InitConfig`, `BaseBlock`, `PlanBlock`, `ApplyBlock`, `RunPlan`, `RunApply`).
  - Keep pure domain packages under `internal/` free of direct `os/exec`, container CLI, or Dapr transport coupling; inject explicit interfaces defined at the consumer boundary.
  - All HCL block and attribute names in workflow configs MUST use `lower_snake_case` (`*.sofa.hcl`).
- **Frontend**: Vue 3 + TypeScript + Vite (under `web/` once the dashboard feature spec is initiated), tested with Vitest + Vue Test Utils under the same Red-Green-Refactor discipline.
  - Untrusted Agent-generated HTML previews MUST be strictly sandboxed (e.g., isolated origin / sandboxed `iframe` without `allow-same-origin` or credential access) so they can never impersonate the dashboard or submit human decisions.
- **Runtime Safety Invariants**:
  - Never rely on LLM prompts to enforce filesystem permissions, branch protection, or QC pass/fail gates.
  - Always register `work_branch` ownership before creating branches from a pinned commit SHA.
  - Enforce execution epoch checks on every checkpoint write and node result publication.
