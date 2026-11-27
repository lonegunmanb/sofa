# Feature Specification: `.sofaplan` Permission Preservation and Client-Side External Skill Bundling (`035-archive-permissions-and-external-skills`)

**Feature Branch**: `035-archive-permissions-and-external-skills`

**Created**: 2026-11-27

**Status**: Approved

**Input**: `internal/plan/archive.go`, `internal/plan/model.go`, `internal/planner/config.go`, `internal/planner/export.go`, `internal/applyeval/eval.go`, `internal/cli/command.go`, `internal/cli/apply_cmd.go`, `docs/hcl-contract.md`, `docs/architecture-contract.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Deterministic Archive Permission Preservation (Priority: P1)

When `plan.SaveArchive` packages a `.sofaplan` ZIP archive and `plan.LoadArchive` extracts it, POSIX permissions are deterministically preserved across Windows and Linux clients without weakening Zip-Slip or `os.Root` protections:
- Directories are stored in the ZIP header with mode `0755` and extracted with `0755` (keeping the extract root creatable with `0755`).
- Regular files are stored with mode `0644`, except files that are executable on the host (any `+x` bit) OR whose content begins with `#!` (shebang), which are stored with mode `0755`.
- On extraction, `LoadArchive` / `extractBundleEntries` / `writeRootFile` normalize stored file modes so only `0644` or `0755` are permitted (any mode with an exec bit normalizes to `0755`, otherwise `0644`) and apply that mode while preserving `os.Root` safety on Windows and Unix. `plugins.lock.json` is written with mode `0644`.
- `computeBundleSHA256` incorporates the normalized entry mode so changing a file's mode between `0644` and `0755` changes `bundle_sha256` and fails integrity verification if tampered with.

**Independent Test**: Unit tests in `internal/plan/plan_test.go` verify:
- A shebang script packed from a non-executable host file receives mode `0755` in the ZIP header and on disk after `LoadArchive` (with `runtime.GOOS` guards only on host OS mode assertions; ZIP header assertions run everywhere).
- Plain regular files receive `0644` and directories receive `0755`.
- Changing a bundle file's mode changes `manifest.BundleSHA256`, and tampering with a ZIP entry's mode fails `LoadArchive` with a `bundle_sha256` mismatch.

---

### User Story 2 - Client-Side `skill_directories` Classification, External Skill Bundling, and Fail-Fast Validation (Priority: P1)

During `SaveArchive` (invoked by both `sofa plan --out` and `sofa apply <dir>`), every known `skill_directories` entry on `worker`, `dynamic_worker` (when known at plan time), and `qc` (on both `worker.qc` and `dynamic_worker.content.qc`) is classified, validated Fail-Fast, bundled if external, and normalized in the exported `plan.json` via a dedicated helper (`plan.NormalizeSkillDirectories` / archive collection step):
1. **Container paths** (`/workspace`, `/workspace/...`, `/.sofa/imports/...`): left unchanged, not bundled, and not validated on the client.
2. **Bundle paths** (`config/...`, `modules/<name>/...`, or relative paths such as `./skills`, `skills`, `../shared-skills` that resolve against the module's client source directory to inside the module's or root/active module's bundle tree): validated to exist in the packed bundle (Fail-Fast error naming node address and path if missing) and normalized to `config/...` or `modules/<name>/...`.
3. **External local paths** (relative paths escaping the bundle tree such as `../agent-skills`, `~/...` expanded against user home, or host absolute paths such as `D:/x`, `C:\x`, `/home/u/x` not under `/workspace` or `/.sofa/imports/`): validated Fail-Fast to exist as a directory on the client at plan/archive time (error naming node address and raw path if missing, non-directory, or symlink), copied cleanly (rejecting symlinks and respecting archive size/entry limits) into `bundle/external_skills/<hash>/` where `<hash>` is the first 16 hex characters of `sha256` of the cleaned absolute client path, with the same permission rules applied.
4. **Rewritten Plan Config & Safe Extraction**: the exported node config in `plan.json` rewrites external entries to `external_skills/<hash>` (not `/.sofa/bundle/...`, which belongs to a later slice). `validateZipEntry` / `normalizeArchiveEntryName` permits `bundle/external_skills/` while continuing to reject traversal (`bundle/external_skills/../x`), backslashes, absolute paths, drive letters, and symlinks, and `LoadArchive` extracts external skills to `<extractDir>/external_skills/<hash>/` preserving permissions.
5. **CLI Source Dir & Apply-Time Preservation**: `sofa plan --out` and `sofa apply <dir>` pass the original local root source directory via `ArchiveSource.RootSourceDir` so relative external paths (`../external-skills`) resolve against the original config source rather than `.sofa/config`. `applyeval` preserves already-normalized `SkillDirectories` on `worker`, `dynamic_worker`, and `qc` when evaluating deferred expressions at Apply time.

**Independent Test**: Unit tests in `internal/plan/plan_test.go`, `internal/applyeval/eval_test.go`, and `internal/cli/command_test.go` verify all three categories, Fail-Fast errors, normalization, `bundle/external_skills/<hash>/` packing/extraction, shebang script `0755` permissions inside external skills, symlink/traversal rejection, and end-to-end `sofa plan --out` / `sofa apply <dir>` behavior.

---

### User Story 3 - Contract Documentation Updates (Priority: P1)

`docs/hcl-contract.md` documents the three `skill_directories` path categories and Fail-Fast client validation; `docs/architecture-contract.md` documents the `.sofaplan` bundle layout (`bundle/config`, `bundle/modules`, `bundle/external_skills/<hash>`) and deterministic permission preservation.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/plan/archive.go` MUST store deterministic POSIX modes in `.sofaplan` ZIP headers (`0755` for directories and files with host `+x` bit or `#!` prefix; `0644` for other regular files, `manifest.json`, `plan.json`, and `plugins.lock.json`).
- **FR-002**: `internal/plan/archive.go` MUST normalize extracted file modes to `0644` or `0755` (exec bit → `0755`, else `0644`), extract directories with `0755` (keeping extract root creatable with `0755`), write `plugins.lock.json` with `0644`, and preserve `os.Root` safety across Windows and Unix.
- **FR-003**: `computeBundleSHA256` MUST include normalized entry mode in the digest so mode changes alter `bundle_sha256`.
- **FR-004**: `plan.NormalizeSkillDirectories` (invoked by `SaveArchive`) MUST classify every known `skill_directories` entry on `worker`, `dynamic_worker`, and `qc` into container paths, bundle paths, or external local paths, fail fast on missing/invalid directories or symlinks (naming node address and raw path), copy external directories into `bundle/external_skills/<hash>/` respecting archive size/entry limits, and rewrite exported node config entries to `config/...`, `modules/<name>/...`, or `external_skills/<hash>` without mutating the caller's input `*Plan`.
- **FR-005**: `validateZipEntry` and `extractBundleEntries` MUST support `bundle/external_skills/` extraction to `<extractDir>/external_skills/<hash>/` while rejecting unsafe segments such as `bundle/external_skills/../x`.
- **FR-006**: `internal/cli/command.go` and `internal/cli/apply_cmd.go` MUST populate `ArchiveSource.RootSourceDir` when planning/applying local configurations, and `internal/applyeval/eval.go` MUST preserve normalized `SkillDirectories` when evaluating deferred worker/qc expressions.
- **FR-007**: `docs/hcl-contract.md` and `docs/architecture-contract.md` MUST reflect the updated `skill_directories` classification and `.sofaplan` bundle layout.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` returns empty.
- **SC-002**: `go vet ./...` passes cleanly.
- **SC-003**: `go test ./... -count=1` passes 100%.
- **SC-004**: `git status` shows only Slice `035` changes (uncommitted for parent review).
