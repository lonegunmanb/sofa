# Feature Specification: Workspace Init, Recursive Module Installation, and Harness Plugin Locking

**Feature Branch**: `007-init-and-plugin-lock`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 007 from `docs/stage1/007-init-and-plugin-lock.md`, `docs/hcl-contract.md`, and `docs/architecture-contract.md`.

## User Scenarios & Testing

### User Story 1 - Transactional workspace initialization and snapshot isolation (`internal/project`) (Priority: P1)

An operator runs workspace initialization against a local directory or remote `go-getter` configuration source into a `.sofa` state directory. `project.Init` snapshots the root configuration into `<state_dir>/config` (skipping `.sofa`, `.git`, and `*.sofaplan` and rejecting symlinks or non-regular files), creates `.sofa/state.json` (`format_version = 1`), `.sofa/config/.initialized.json` (`format_version = 2`), `.sofa/modules`, and `.sofa/plugins.lock.json` behind a `.initializing` transaction lock, and redacts credentials, fragments, and non-`ref` query parameters from `state.json` and any error messages. Once initialized, edits to the original source directory do not affect `.sofa/config` or `.sofa/modules` until the next `project.Init`, and `project.Open` / `OpenProject` validates snapshot consistency and refuses interrupted workspaces.

**Independent Test**: Initialize local and remote (`httptest.Server` / injected `FetchFunc`) workspaces, verify `.git`/`.sofa`/`*.sofaplan` exclusion, reject source directories inside `.sofa` and symlinks/non-regular entries, verify snapshot isolation after source edits, verify `SaveOutputs`/`ReadOutputs` lifecycle, verify `Open` rejection on `.initializing` or mismatched state, and verify recovery on subsequent `Init`.

### User Story 2 - Recursive `module` scanning, cycle detection, and atomic upgrade (`internal/module`) (Priority: P1)

`module.Init` scans `*.sofa.hcl` via `config.LoadDirectoryContext` for `module "<name>" { source = "..." }` blocks, validates single-segment safe labels (rejecting `.`, `..`, `/`, `\`, control chars, duplicate labels, and case-insensitive label collisions), enforces static literal non-empty `source` strings without variable references, installs local modules safely using `os.OpenRoot` (skipping `.sofa`, `.git`, `*.sofaplan` and rejecting non-regular files) and remote modules via `FetchFunc` (`go-getter/v2` with `DisableSymlinks: true` and redacted errors), detects source cycles (`a -> b -> a`, `a -> a`) via recursion stack keys (`local:<abs>` / `remote:<source>`), reuses cached modules on `Upgrade == false` while still recursing into nested children, and atomically swaps `<state_dir>/modules` via `<state_dir>/.modules-tmp-*` staging on `Upgrade == true`.

**Independent Test**: Install nested local modules (`a -> b`), resolve relative nested local sources from original source directories, reject cycles (`a -> b -> a`, `a -> a`), reject computed/empty/missing `source` and unsafe/duplicate/case-colliding labels, test `Fetch` credential redaction and `httptest.Server` zip download, and verify `Upgrade = false` reuse vs `Upgrade = true` atomic swap and rollback on failure.

### User Story 3 - `tflint`-style Harness plugin verification and cross-platform SHA-256 locking (`internal/harness`) (Priority: P1)

`harness.LockPlugins` scans `<state_dir>/config` and all `<state_dir>/modules/**` directories containing `*.sofa.hcl` for `harness "<name>"` blocks, enforces static literal `source`, `version`, `signature`, and `signing_key` (while permitting `var.*` / `local.*` inside `options` or `env_refs` for later `plan`-time evaluation), validates via `harnessspec.HarnessConfig.Validate()`, detects cross-module local/remote, version/source, and signature conflicts (preventing child `"none"` from downgrading `"attestation"`/`"pgp"`/`"auto"`), resolves `GITHUB_TOKEN_<NORMALIZED_HOST>` before `GITHUB_TOKEN` in memory only, fetches `v{version}` `checksums.txt`, extracts `sofa-harness-<name>_<os>_<arch>.zip` assets requiring `linux_amd64` or `linux_arm64`, verifies signatures across `"none"`, `"pgp"`, `"attestation"`, and `"auto"` (attestation with PGP fallback on `ErrAttestationNotFound` when `signing_key` is set), reuses matching entries from existing `plugins.lock.json` when `!Upgrade`, and atomically writes deterministic `<state_dir>/plugins.lock.json` (`0600`).

**Independent Test**: Verify local-mode `harness` triggers zero network calls, test all four signature modes (`none`, `pgp` with real generated OpenPGP keypairs and tampered-file rejection, `attestation`, and `auto` attestation/fallback/failure) with both injected fakes and default `net/http` implementations against `httptest.Server`, test `ResolveGitHubToken` host normalization and fallback, test cross-module conflict merging/rejection, and test `!Upgrade` lockfile cache reuse.

### Edge Cases

- `golden.AsHclBlocks` represents a single-label HCL block (`module "x"` or `harness "y"`) as `Labels = ["", "x"]`; both `len == 1` and `len == 2 && labels[0] == ""` must be accepted while multi-label blocks are rejected.
- Windows file handle sharing (`ERROR_SHARING_VIOLATION`): all `os.Root` and `*os.File` handles opened during snapshot or module copy MUST be closed before `os.Rename` or `os.RemoveAll`.
- A `harness` block may reference `var.*` or `local.*` in `options` or `env_refs` (evaluated at `plan` time); `LockPlugins` MUST NOT fail on those during `init`, while strictly rejecting variable references in `source`, `version`, `signature`, and `signing_key`.
- Harness names containing underscores or hyphens (e.g. `my_custom-harness`) must still parse `sofa-harness-<name>_<os>_<arch>.zip` accurately by stripping exact `"sofa-harness-" + name + "_"` prefix and `".zip"` suffix.
- Cached modules (`Upgrade == false`) that already exist on disk MUST still have their `*.sofa.hcl` scanned for missing child modules or cycles.
- Failure in `module.Init` or `harness.LockPlugins` inside `project.Init` MUST leave `.initializing` on disk so `project.Open` refuses half-initialized workspaces and the next `project.Init` triggers full recovery.

## Requirements

- **FR-001**: `internal/module` MUST export `FetchFunc`, `InitOptions`, `Init`, `Directory`, `IsLocalSource`, and `Fetch` (using `go-getter/v2` with `DisableSymlinks: true` and URL secret redaction).
- **FR-002**: `module.Init` MUST validate single-segment safe module labels, reject duplicate and case-insensitive colliding labels in the same directory, enforce static literal non-empty `source` strings, detect source cycles, copy local modules via `os.OpenRoot` (skipping `.sofa`, `.git`, `*.sofaplan`, rejecting non-regular files), reuse cached directories when `!Upgrade` while still recursing into children, and atomically swap `<state_dir>/modules` via staging when `Upgrade` is true.
- **FR-003**: `internal/harness` MUST export `LockFileName`, `LockFormatVersion`, `ErrAttestationNotFound`, `PluginAsset`, `PluginLockEntry`, `PluginLockFile`, `ReleaseFetcher`, `AttestationVerifier`, `LockOptions`, `LockPlugins`, `ReadLockFile`, and `ResolveGitHubToken`.
- **FR-004**: `harness.LockPlugins` MUST scan `<state_dir>/config` and `<state_dir>/modules/**`, validate static `harness` metadata via `harnessspec.HarnessConfig`, detect/merge cross-module harness declarations, resolve tokens without persisting them, parse `checksums.txt` requiring `linux_amd64` or `linux_arm64`, verify `"none"` / `"pgp"` / `"attestation"` / `"auto"` signatures, reuse matching lock entries when `!Upgrade`, and atomically write sorted `<state_dir>/plugins.lock.json` (`0600`).
- **FR-005**: `internal/harness` MUST provide default `net/http` implementations of `ReleaseFetcher` and `AttestationVerifier` supporting `github.com`, GHES hosts, and local HTTP test servers, sending `Bearer` tokens when non-empty and redacting credentials from errors.
- **FR-006**: `internal/project` MUST implement `Init`, `Open`, `OpenProject`, `Project`, `State`, `SaveOutputs`, and `ReadOutputs` with `.initializing` transaction lock, `.initialized.json` (`format_version = 2`), `state.json` (`format_version = 1`, `0600` atomic write, source URL redaction), snapshot copy skipping `.sofa`/`.git`/`*.sofaplan` and rejecting non-regular files, and integrated `module.Init` + `harness.LockPlugins`.

## Success Criteria

- **SC-001**: All unit tests in `internal/module/init_test.go`, `internal/harness/lock_test.go`, and `internal/project/project_test.go` pass offline with zero external network, Docker/Podman, or Dapr calls.
- **SC-002**: `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` pass with zero warnings or errors.
