---

description: "Migrate sofa output planning to golden native OutputBlock"
---

# Tasks

## Phase 1: Setup

- [X] T001 Upgrade `github.com/lonegunmanb/golden` to commit `059cc8c` or newer in `go.mod`/`go.sum`.
- [X] T002 Update output/cold-start tests to assert native snapshot expression and combined source/output cold-start registration.

## Phase 2: Migration

- [X] T003 Delete custom output block tests and replace `internal/module/spec/output.go` with only the pure Output snapshot DTO.
- [X] T004 Remove output registration and switch `SofaConfig.Outputs()` to `golden.Blocks[*golden.OutputBlock](c)` in `internal/planner/config.go`.
- [X] T005 Update cold-start configuration to include both source and output blocks.

## Phase 3: Validation

- [X] T006 Verify zero sofa definitions/registrations for output.
- [X] T007 Run all output, cold-start, sensitivity, invalid-schema, and unknown-value tests.
- [X] T008 Run `gofmt`, `go vet ./...`, and `go test ./... -count=1`.
- [X] T009 Commit and push the migration with a clean worktree.
