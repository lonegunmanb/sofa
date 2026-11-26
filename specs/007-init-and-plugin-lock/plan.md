# Implementation Plan: Workspace Init, Recursive Module Installation, and Harness Plugin Locking

**Branch**: `007-init-and-plugin-lock` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Implement the workspace initialization, recursive module installer, and `tflint`-style harness plugin lock infrastructure across `internal/module/init.go`, `internal/harness/lock.go`, and `internal/project/{project,state}.go`, backed by `github.com/hashicorp/go-getter/v2` and `github.com/ProtonMail/go-crypto`.

## Technical Context

- **Language/Version**: Go 1.27.3
- **New Dependencies**: `github.com/hashicorp/go-getter/v2`, `github.com/ProtonMail/go-crypto`
- **Existing Packages Consumed**: `internal/config` (`LoadDirectoryContext`), `internal/harness/spec` (`HarnessConfig`, `SignatureMode`)
- **Testing**: 100% offline Tier 1 unit tests (`httptest.Server`, in-memory OpenPGP keypairs, injected `FetchFunc`, `ReleaseFetcher`, `AttestationVerifier`, `LookupEnv`)

## Constitution Check

- **Principle I (Spec-First)**: Full Spec Kit artifacts authored under `specs/007-init-and-plugin-lock/` prior to dependencies or code.
- **Principle II (Strict TDD)**: Failing Red tests written for `internal/module`, `internal/harness`, and `internal/project` before implementing production logic.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: Network/env boundaries (`FetchFunc`, `ReleaseFetcher`, `AttestationVerifier`, `LookupEnv`) sit behind explicit Go interfaces injected through `InitOptions` and `LockOptions`.
- **Principle IV (Runtime Enforcement)**: Snapshot and local module copying use `os.OpenRoot` to enforce directory boundaries, reject symlinks and special files, and lock plugin SHA-256 hashes verified by OpenPGP or attestation prior to runtime execution.
- **Principle VI (Research-First & Golden Reuse)**: HCL block parsing reuses `config.LoadDirectoryContext` (`golden.AsHclBlocks`) and harness schema validation reuses `harnessspec.HarnessConfig.Validate()` without duplicating schema rules.

## Design Decisions

1. **Shared Safe Snapshot Copy (`internal/module/init.go`)**:
   - Provide `CopyCleanDirectory(ctx, source, destination)` using `os.OpenRoot` for both source and destination, skipping `.sofa`, `.git` directories and `*.sofaplan` regular files, rejecting symlinks/devices/sockets/FIFOs via `Lstat` + `Open` `IsRegular()` checks, and closing every `*os.File` and `*os.Root` inside scoped helpers before returning so Windows renames/cleanups never hit `ERROR_SHARING_VIOLATION`.
2. **Module Recursive Installer (`internal/module/init.go`)**:
   - Inspect `module` blocks from `config.LoadDirectoryContext`, supporting `golden.AsHclBlocks` label format (`["", label]` or `[label]`), checking single-segment label validity and case-insensitive uniqueness per directory.
   - Track recursion stack (`"local:" + cleanAbsPath` / `"remote:" + source`) and report `"module source cycle at <address>"`.
   - Reuse cached directories when `!Upgrade` while still scanning and recursing into child modules; on `Upgrade`, install to `<parent>/.modules-tmp-*` and swap atomically with rollback.
3. **Harness Plugin Locking (`internal/harness/lock.go`)**:
   - Scan `<state_dir>/config` and `<state_dir>/modules/**` directories with `*.sofa.hcl`; extract static `source`, `version`, `signature`, `signing_key` (rejecting variable traversals during `init`, while ignoring `options`/`env_refs` variable references until `plan`).
   - Validate each declaration via `harnessspec.HarnessConfig.Validate()` and merge/reject cross-module conflicts (`attestation` > `auto`, `pgp` > `auto`, any > `none`; `attestation` vs `pgp` rejected).
   - Provide default `net/http` `ReleaseFetcher` and `AttestationVerifier` implementations plus `ResolveGitHubToken`, `checksums.txt` strict parser, 4-mode verification (`none`, `pgp` via `ProtonMail/go-crypto/openpgp`, `attestation`, `auto`), `!Upgrade` cache reuse, and `0600` atomic `plugins.lock.json`.
4. **Transactional Project Lifecycle (`internal/project/{project,state}.go`)**:
   - Coordinate `.initializing` transaction file, `module.Init`, configuration snapshot activation (`CopyCleanDirectory` + `.initialized.json` v2), `harness.LockPlugins`, `state.json` v1 (`0600` atomic write + credential/query/fragment redaction), and `SaveOutputs`/`ReadOutputs`.
   - Keep `.initializing` in place if `module.Init` or `harness.LockPlugins` fails so `Open`/`OpenProject` refuses half-initialized workspaces and subsequent `Init` triggers full recovery (`effectiveUpgrade = true`).

## Files

```text
go.mod, go.sum
internal/module/{init.go,init_test.go}
internal/harness/{lock.go,lock_test.go}
internal/project/{project.go,state.go,project_test.go}
specs/007-init-and-plugin-lock/{spec,plan,research,data-model,quickstart,tasks}.md
specs/007-init-and-plugin-lock/contracts/init-and-plugin-lock.md
specs/007-init-and-plugin-lock/checklists/requirements.md
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
