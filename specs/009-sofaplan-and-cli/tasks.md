---
description: "Immutable Plan codec, .sofaplan archive, ExportPlan, variableschema, and offline CLI TDD tasks"
---

# Tasks

- [X] T001 Add `github.com/spf13/cobra` to `go.mod` / `go.sum`.
- [X] T002 [Red] Write failing unit tests in `internal/plan/plan_test.go` covering:
  - Immutable `Plan` constructor invariants (unique nodes, valid kinds, non-nil child `Module.Plan`, dependency existence, cycle rejection, `ToolSpec` validation, defensive deep-cloning getters);
  - Lossless `Marshal` -> `Unmarshal` round-trip of known values, primitive & nested `cty.UnknownVal`, `SensitivePaths` (object attrs, map keys, list indices, whole values), `ModuleSpec` inputs/timeouts/deferred validations, `Tools`, and `PluginLock`;
  - `Display` / `DisplayValues` redacting sensitive leaves & marked containers to `"<sensitive>"` and unknown values to `"<known after apply>"`, plus `ContainsSensitiveValues`;
  - `SaveArchive` byte-for-byte determinism, `0600` / Windows ACL permissions, cross-directory `LoadArchive` restoring `Plan`, `PluginLock`, `bundle/config/skills/...`, and `bundle/modules/...`;
  - `SaveArchive` failing on missing `bundle/` `skill_directories` (for `worker`, `worker.qc`, and `dynamic_worker`);
  - `LoadArchive` rejecting tampered `plan.json`, tampered `bundle/` files, missing/unlisted `plugins.lock.json`, Zip-Slip `../../evil`, backslash paths, symlink entries, and oversized streams exceeding `MaxSingleFileBytes` / `MaxTotalBytes`.
- [X] T003 [Green] Implement `internal/plan/{model.go,codec.go,display.go,archive.go,permissions_windows.go,permissions_unix.go}` passing all `internal/plan` tests.
- [X] T004 [Red] Write failing unit tests in `internal/planner/export_test.go` covering:
  - Exporting a multi-module workflow with all 12 block kinds (`source`, `preserve`, `sandbox`, `harness`, `model_provider`, `mcp_server`, `go_tool`, `external_tool`, `worker`, `dynamic_worker`, `merge`, `module` + sensitive `output`);
  - Topological + lexicographical node ordering, transitive dependencies routed through `locals`, `Tools` metadata (`InputSchema`, `InputType`, `OutputType`, `QuotePaths`, `Postconditions`), and `Origin`;
  - Full sensitivity preservation across `model_provider`, `mcp_server`, `harness`, `worker`, `dynamic_worker`, `output`, and `var` context, verified through `Marshal` -> `Unmarshal` and zero plaintext leakage in `Display`;
  - `RequirePluginLock` rejecting unlocked or mismatched remote harnesses in root and child modules via `harness.CanSatisfyHarnessLock`.
- [X] T005 [Green] Expose `harness.CanSatisfyHarnessLock`, preserve `signing_key` marks in `internal/harness/spec/schema.go` and `http.headers`/`stdio.env` marks in `internal/mcp/spec/schema.go`, and implement `internal/planner/export.go` passing all `internal/planner` tests.
- [X] T006 [Red] Write failing unit tests in `internal/variableschema/schema_test.go` covering required variables without defaults, all `cty` / `optional(...)` types, `nullable = false` default rejection, `sensitive = true` redaction in JSON and `FormatText`, and invalid/duplicate variable rejection.
- [X] T007 [Green] Implement `internal/variableschema/schema.go` passing all `internal/variableschema` tests.
- [X] T008 [Red] Write failing unit & end-to-end tests in `internal/cli/command_test.go` and `cmd/sofa/main_test.go` covering `NormalizeArgs`, `sofa init`, `sofa schema` (text & `--json`), `sofa plan` (interleaved `-var` / `-var-file` precedence, redacted stdout `Display`, stderr sensitive archive warning, `LoadArchive` round-trip in a fresh directory), uninitialized state errors (`ExitFailure`), bad flags/`-var` syntax (`ExitUsage`), and interrupt (`130`) + `hcl.Diagnostics` formatting.
- [X] T009 [Green] Implement `internal/cli/command.go` and `cmd/sofa/main.go` passing all `internal/cli` and `cmd/sofa` tests.
- [X] T010 [Refactor & Verify] Review diffs, run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, and mark all tasks `[X]`.
