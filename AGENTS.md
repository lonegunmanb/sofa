# AGENTS.md — sofa Workspace Rules

This repository implements **sofa (SOftware FActory)** (`github.com/lonegunmanb/sofa`). All human and AI contributors MUST follow `.specify/memory/constitution.md` and the operational rules below.

## 1. Sources of Truth

1. **Engineering Governance**: `.specify/memory/constitution.md` (binding principles and quality gates).
2. **System Vision & Domain Concepts**: `README.md` (design overview; exact HCL fields, API routes, and state machines are frozen feature-by-feature in specs).
3. **Active Feature Contracts**: `specs/<###-feature>/` (`spec.md`, `plan.md`, `data-model.md`, `contracts/`, `tasks.md`).

## 2. Pre-Task Investigation & Anti-Monolith Sizing (NON-NEGOTIABLE)

- **Investigate Before Deciding**: Never propose or freeze a feature slice or task on guesswork. Always inspect the actual source code and behavior of upstream libraries (`golden`, `copilot-sdk/go`, `dapr/go-sdk`), reference implementations (`r42`), and the existing repository *before* scoping a spec or writing a plan, so existing library capabilities are leveraged directly instead of re-invented.
- **Reject Oversized Tasks**: Refuse monolithic specs, plans, or tasks that bundle multiple subsystems, multiple unrelated HCL block families, or broad cross-cutting features into a single unit. If a requested or drafted scope is too big, stop, push back with concrete evidence from your investigation, and split it into minimal, single-purpose, independently testable slices before invoking `/speckit-specify` or writing code.

## 3. Spec-Driven Development (GitHub Spec Kit)

- **No Spec, No Code**: Never write or modify production Go or Vue code without an active feature specification and plan under `specs/<###-feature>/`.
- **Workspace Skills**: Use the installed Spec Kit skills in `.agents/skills/speckit-*`:
  - `speckit-constitution` — view/amend `.specify/memory/constitution.md`
  - `speckit-specify` / `speckit-clarify` — author and de-risk `spec.md`
  - `speckit-plan` — produce `plan.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`
  - `speckit-tasks` / `speckit-analyze` — generate TDD-ordered `tasks.md` and verify cross-artifact alignment
  - `speckit-implement` — execute `tasks.md`
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

- **Backend**: Go (`go.mod` module `github.com/lonegunmanb/sofa`), `hashicorp/hcl/v2`, `github.com/lonegunmanb/golden` (HCL → DAG planning & execution engine), `github.com/github/copilot-sdk/go` (default bundled Copilot CLI agent runtime inside containers, aligned with `github.com/lonegunmanb/r42`), and `dapr/go-sdk`.
  - **Delegate to `golden`**: Never re-implement HCL block loading/decoding, `variable`/`locals`/`for_each`/`depends_on` evaluation, or DAG topological construction and traversal that `golden` already provides (`AsHclBlocks`, `InitConfig`, `BaseBlock`, `PlanBlock`, `ApplyBlock`, `RunPlan`, `RunApply`). Keep each feature spec small and surgical, following `r42`'s incremental design.
  - Keep pure domain packages under `internal/` free of direct `os/exec`, container CLI, or Dapr transport coupling; inject explicit interfaces defined at the consumer boundary.
  - All HCL block and attribute names in workflow configs MUST use `lower_snake_case` (`*.sofa.hcl`).
- **Frontend**: Vue 3 + TypeScript + Vite (under `web/` once the dashboard feature spec is initiated), tested with Vitest + Vue Test Utils under the same Red-Green-Refactor discipline.
  - Untrusted Agent-generated HTML previews MUST be strictly sandboxed (e.g., isolated origin / sandboxed `iframe` without `allow-same-origin` or credential access) so they can never impersonate the dashboard or submit human decisions.
- **Runtime Safety Invariants**:
  - Never rely on LLM prompts to enforce filesystem permissions, branch protection, or QC pass/fail gates.
  - Always register `work_branch` ownership before creating branches from a pinned commit SHA.
  - Enforce execution epoch checks on every checkpoint write and node result publication.
