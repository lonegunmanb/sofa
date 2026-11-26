# Specification Quality Checklist: `018-worker-qc-and-merge-runner`

**Purpose**: Validate completeness and alignment with `docs/stage2/018-worker-qc-and-merge-runner.md` before implementation.

## Content Quality

- [x] Zero `os/exec` imports planned inside `internal/worker/runner`
- [x] All `018-worker-qc-and-merge-runner.md` state transitions, mount invariants, and error paths covered
- [x] Red-Green-Refactor ordering enforced in `tasks.md`
- [x] Tier 1 unit tests and Tier 2 real Git + Podman integration tests explicitly defined
