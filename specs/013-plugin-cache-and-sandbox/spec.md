# Feature Specification: Harness Plugin Cache Verification & Podman Sandbox / Gatekeeper Runtime (`013-plugin-cache-and-sandbox`)

**Feature Branch**: `013-plugin-cache-and-sandbox`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `013` blueprint (`docs/stage2/013-plugin-cache-and-sandbox.md`), `docs/stage2/README.md`, `internal/harness/lock.go`, `internal/harness/spec/schema.go`, `internal/sandbox/spec/schema.go`, `internal/worker/spec/gatekeeper.go`, `internal/git/adapter.go`.

## User Scenarios & Testing

### User Story 1 - Harness Plugin Local Discovery & Locked Remote Cache (`internal/harness/installer`) (Priority: P1)

Before starting a Work or QC container for a `harness "<name>"` declaration, the host resolves and verifies a self-contained Linux plugin directory (`PreparedPlugin{Name, HostDir, ContainerPath}`) targeted at the container engine's Linux platform (`linux_amd64` or `linux_arm64`):
- **Local discovery (`cfg.Source == ""`)**: Searches candidate roots in strict priority order: (1) `PluginDir` (`--plugin-dir`), (2) `SOFA_PLUGIN_DIR` via injectable `LookupEnv`, (3) `<WorkingDir>/.sofa/plugins`, (4) `<UserHomeDir>/.sofa/plugins` (falling back to `os.UserHomeDir`). Under each `<base>`, checks `<base>/sofa-harness-<name>/<platform.Key()>/sofa-harness-<name>` before `<base>/sofa-harness-<name>/sofa-harness-<name>`, verifies the entrypoint is a regular file (never a symlink or directory), applies `0o755` best-effort, and returns its parent directory as `HostDir` and `"/.sofa/harness/sofa-harness-" + Name` as `ContainerPath`.
- **Remote locked cache (`cfg.Source != ""`)**: Validates `harness.CanSatisfyHarnessLock(cfg, lockFile)`, looks up `entry.Assets[platform.Key()]`, and serializes concurrent installs per cache target `<cacheRoot>/<SourceHost>/<SourceOwner>/<SourceRepo>/<Version>/<platform.Key()>/`. On cache hit (`.verified.sha256` equals locked `asset.SHA256` and `sofa-harness-<name>` is a regular file), returns immediately without network calls. On cache miss or corrupt/missing `.verified.sha256`, downloads `"v" + Version` asset `asset.Filename` via `ReleaseFetcher` (using `harness.ResolveGitHubToken`), verifies actual SHA-256 against locked `asset.SHA256` prior to extraction, extracts the ZIP inside a sibling `.tmp-*` staging directory confined by `os.OpenRoot` (rejecting Zip-Slip, backslashes, drive prefixes, symlinks, irregular files, and Zip-Bomb entry/size limits, preserving executable bits and forcing `0o755` on top-level `sofa-harness-<name>`), writes `.verified.sha256`, and atomically swaps into the cache directory.

**Independent Test**: Tier 1 unit tests (`internal/harness/installer/installer_test.go`) covering 4-tier local priority order, platform-subdir vs single-dir preference, symlink/missing entrypoint errors, remote download + SHA-256 verification, second-call cache hit with zero fetcher invocations, tampered `.verified.sha256` recovery, SHA-256 mismatch rejection, Zip-Slip/symlink/size-limit rejection, missing root entrypoint rejection, and concurrent installs of the same target.

### User Story 2 - Zero-Secret Container Mount Layout & CLI Arg Builder (`internal/sandbox/runtime/spec.go`) (Priority: P1)

`ComputeWorkspaceMounts` and `BuildRunArgs` construct deterministic container CLI arguments implementing the Stage 2 mount matrix and security invariants:
- **Zero secret environment variables**: `BuildRunArgs` does not accept arbitrary secret env maps; it sets only fixed non-sensitive container variables (`HOME=/tmp/sofa-home`, `TMPDIR=/tmp`, `GIT_OPTIONAL_LOCKS=0`).
- **Workspace & `writable_paths` isolation (`ComputeWorkspaceMounts`)**:
  - Rejects `.git`, `.git/**`, absolute paths, backslashes, and `..` traversal in `writablePaths`.
  - QC mode (`ReadOnlyWorkspace == true`): single `<hostWorkspace>:/workspace:ro` mount.
  - Work mode with `writablePaths == nil`: `<hostWorkspace>:/workspace:rw` followed by `<hostWorkspace>/.git:/workspace/.git:ro` (when `.git` exists).
  - Work mode with `writablePaths != nil` and empty (`[]string{}`): single `<hostWorkspace>:/workspace:ro` mount.
  - Work mode with non-empty `writablePaths`: root-level wildcards (`*`, `**`, `.`, `*.md`) or non-existent single-segment root files fall back to `/workspace:rw` + `/.git:ro` (enforced by host `012` `FindUnauthorizedPaths`); existing single-segment literal files mount `<file>:/workspace/<file>:rw`; multi-segment literal-prefix paths ensure `<hostWorkspace>/<topDir>` exists (via `os.OpenRoot` `MkdirAll`, rejecting symlinks) and mount `<topDir>:/workspace/<topDir>:rw` over `<hostWorkspace>:/workspace:ro`, always appending `<hostWorkspace>/.git:/workspace/.git:ro` last when `.git` exists.
- **System mounts & cross-platform network guard**: Appends `--rm`, `--init`, `-w <workingDir>`, `--network`, optional `--cpus`/`--memory`, optional `--name`, `-d`, `-p 127.0.0.1::19500`, `--entrypoint`, user `cfg.Mounts` (`:ro`), `/.sofa/{bundle,imports,artifacts,tools,harness,session-store,plugin}` mounts, `cfg.Image`, and `Args`. Rejects `ForAgentSession == true` with `HostOS != "linux"` and `Network == "none"`.

**Independent Test**: Tier 1 unit tests (`internal/sandbox/runtime/podman_test.go`) covering zero secret `-e` flags, `.git:ro` trailing position, all `ComputeWorkspaceMounts` modes (`RO`, `nil`, `[]`, `docs/spec.md` auto-created dir, existing vs missing `go.mod`, root wildcard fallback, `.git` rejection), and Windows/macOS `network = "none"` rejection.

### User Story 3 - `PodmanEngine`: `DetectPlatform`, `Exec`, and `RunGatekeepers` (`internal/sandbox/runtime/podman.go`) (Priority: P1)

`PodmanEngine` implements `Engine` against `podman` / `docker` / `nerdctl`:
- `DetectPlatform(ctx)` queries the container engine (`podman info --format {{.Version.OsArch}}` with `{{.Host.OS}}/{{.Host.Arch}}` and `{{.OSType}}/{{.Architecture}}` fallbacks) and normalizes to `Platform{OS: "linux", Arch: "amd64"|"arm64"}`.
- `Exec(ctx, containerName, workDir, command, env, stdin)` executes `<runtimeBin> exec -i -w <workDir> [-e K=V ...] <containerName> <command...>`, capturing `Stdout`, `Stderr`, and `ExitCode` (returning non-zero exit codes in `ExecResult.ExitCode` with `err == nil`, and returning `error` only on context cancellation/timeout or spawn failure).
- `RunGatekeepers(ctx, spec, gatekeepers)` runs each gatekeeper sequentially in a dedicated short-lived `--rm --init -w <resolvedWorkDir> --entrypoint <command[0]>` container with `/workspace:rw`, `/workspace/.git:ro`, optional `/.sofa/{bundle,imports,artifacts}:ro`, and per-gatekeeper `Timeout`. When `RequireCleanTree == true`, snapshots pre-gatekeeper workspace status and exact dirty/untracked file bytes/SHA-256 before execution, compares post-execution state to detect any gatekeeper-introduced additions, deletions, or mutations (including mutations to pre-existing uncommitted Worker edits), restores the exact pre-gatekeeper workspace state on any violation (including on failure/timeout), and immediately returns `&GatekeeperFailure{...}` on exit code mismatch, timeout, or dirty files.

**Independent Test**: Tier 1 unit tests (`podman_test.go`) for `Exec`, `DetectPlatform` parsing, and pre/post gatekeeper snapshot/restore logic using a fake CLI/Git adapter; Tier 2 real Podman integration tests (`podman_integration_test.go`, `//go:build integration`) against `alpine:latest` verifying real Linux kernel `Read-only file system` enforcement, `Exec` stdin/stdout/exit codes, and all 4 `RunGatekeepers` scenarios.

### Edge Cases

- Windows host paths for remote plugins running against loopback `127.0.0.1:<port>` contain `:` in `SourceHost`, which is invalid in Windows directory names and must be sanitized safely in the cache path.
- On Windows, `os.Rename(stagingDir, cacheDir)` fails if `cacheDir` already exists or if any file/root handle inside `stagingDir` remains open; all handles must be closed and stale `cacheDir` removed under the per-target mutex prior to rename.
- `RequireCleanTree == true` MUST preserve pre-existing uncommitted Worker changes while detecting and restoring both newly created files and gatekeeper mutations/deletions of pre-existing dirty files (even if a gatekeeper reverts a modified file back to `HEAD`).
- Gatekeeper timeouts on Windows/WSL2 Podman must force-remove the named container (`rm -f -t 1`) so timed-out containers do not linger.

## Requirements

- **FR-001**: `internal/harness/lock.go` MUST export `NewDefaultReleaseFetcher() ReleaseFetcher` and reuse it inside `LockPlugins`.
- **FR-002**: `internal/harness/installer` MUST provide `Platform`, `PreparedPlugin`, `ResolveOptions`, and `Installer.Prepare` implementing 4-tier local plugin discovery and locked remote cache download, SHA-256 verification, `os.OpenRoot`-confined ZIP extraction, and atomic activation.
- **FR-003**: `internal/sandbox/runtime/spec.go` MUST provide `Platform` (aliased to `installer.Platform`), `BindMount`, `ComputeWorkspaceMounts`, `SessionMountLayout`, `BuildRunArgs`, `ExecResult`, `GatekeeperRunSpec`, `GatekeeperFailure`, and `Engine` with the zero-secret-env invariant and cross-platform `network = "none"` guard.
- **FR-004**: `internal/sandbox/runtime/podman.go` MUST implement `PodmanEngine` (`DetectPlatform`, `Exec`, `RunGatekeepers`, plus `StartDetached`/`RemoveContainer`/`Port` helpers) with pre/post `RequireCleanTree` snapshot verification and restoration via `git.Adapter` + `os.OpenRoot`.

## Success Criteria

- **SC-001**: `gofmt -l .` produces empty output.
- **SC-002**: `go vet ./...` passes with zero diagnostics.
- **SC-003**: `go test ./... -count=1` passes offline with zero external daemon dependencies.
- **SC-004**: `go test -tags=integration ./internal/sandbox/runtime/... -count=1` passes against real local Podman (`alpine:latest`) and real `git` CLI.
