# Tasks: `.sofaplan` Permission Preservation and Client-Side External Skill Bundling (`035-archive-permissions-and-external-skills`)

**Input**: Design documents from `/specs/035-archive-permissions-and-external-skills/`
**Prerequisites**: `spec.md`, `plan.md`

## Phase 1: Tests First (Red Phase — MUST fail before implementation)

- [x] T001 [US1] Write failing tests in `internal/plan/plan_test.go` for ZIP header and extracted file/dir permissions (shebang `#!` file without host `+x` → `0755`, plain file → `0644`, directory → `0755`, mode included in `BundleSHA256`, mode tampering rejected on `LoadArchive`).
- [x] T002 [US2] Write failing tests in `internal/plan/plan_test.go` for `skill_directories` classification & external bundling (`${path.module}/skills` existing/missing, `./skills` normalized to `config/skills`, `../external-skills` bundled under `bundle/external_skills/<hash>/` with plan rewrite `external_skills/<hash>`, `SKILL.md` + `#!` script `0755` round-trip, missing absolute host path Fail-Fast, `/workspace/.agents/skills` untouched, `/.sofa/imports/...` untouched, `qc.skill_directories` and `dynamic_worker` handled, symlink rejection, `bundle/external_skills/../x` rejected on load).
- [x] T003 [US2] Write failing tests in `internal/cli/command_test.go`, `internal/cli/cli_apply_test.go`, and `internal/applyeval/eval_test.go` for `sofa plan --out` / `sofa apply <dir>` resolving `../external-skills` relative to the original local source directory and `applyeval` preserving normalized `SkillDirectories`.
- [x] T004 Run `go test ./internal/plan ./internal/applyeval ./internal/cli -count=1` and confirm new tests fail as expected.

## Phase 2: Implementation (Green Phase)

- [x] T005 [US1] Implement permission detection (`0755` for dirs, host `+x`, or `#!` prefix; `0644` otherwise), ZIP header mode writing, normalized mode extraction in `readValidatedArchiveEntries` / `extractBundleEntries` / `writeRootFile` (keeping `os.Root` safety and `plugins.lock.json` `0644`), and mode-aware `computeBundleSHA256` in `internal/plan/archive.go`.
- [x] T006 [US2] Implement `ArchiveSource.RootSourceDir`, `plan.NormalizeSkillDirectories` (and archive collection helper) in `internal/plan/skills.go` / `internal/plan/archive.go`, supporting container paths, bundle-tree paths, external local paths (`../`, `~/`, host absolute paths), symlink rejection, archive size/entry limits, non-mutating `Plan` rewrite, `bundle/external_skills/` validation in `validateZipEntry`, and extraction to `<extractDir>/external_skills/<hash>/`.
- [x] T007 [US2] Wire `ArchiveSource.RootSourceDir` in `internal/cli/command.go` and `internal/cli/apply_cmd.go`, and preserve planned/normalized `SkillDirectories` in `internal/applyeval/eval.go`.
- [x] T008 [US3] Update `docs/hcl-contract.md` and `docs/architecture-contract.md`.

## Phase 3: Verification & Polish

- [x] T009 Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `git status` to verify clean completion without committing.
