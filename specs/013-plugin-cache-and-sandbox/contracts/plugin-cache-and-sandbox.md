# Contract: Plugin Cache Verification & Podman Sandbox Runtime (`013`)

## 1. `internal/harness/lock.go`

- `NewDefaultReleaseFetcher() ReleaseFetcher`: returns the default HTTP release asset fetcher reused by `LockPlugins` and `installer.Installer`.

## 2. `internal/harness/installer`

- **Local discovery (`cfg.Source == ""`)**:
  - Searches candidate roots in strict priority:
    1. `opts.PluginDir`
    2. `SOFA_PLUGIN_DIR` via `opts.LookupEnv` (default `os.LookupEnv`)
    3. `filepath.Join(opts.WorkingDir, ".sofa", "plugins")` (when `WorkingDir != ""`)
    4. `filepath.Join(homeDir, ".sofa", "plugins")` (`opts.UserHomeDir` or `os.UserHomeDir()`)
  - Within each `<base>`, checks `<base>/sofa-harness-<name>/<platform.Key()>/sofa-harness-<name>` then `<base>/sofa-harness-<name>/sofa-harness-<name>`.
  - Rejects symlinks and directories; requires a regular file; applies `os.Chmod(..., 0o755)` best-effort; returns absolute parent `HostDir` and `ContainerPath = "/.sofa/harness/sofa-harness-" + name`.
- **Remote locked cache (`cfg.Source != ""`)**:
  - Validates `harness.CanSatisfyHarnessLock(cfg, opts.LockFile)` and presence of `entry.Assets[platform.Key()]`.
  - Serializes concurrent installs per target `<cacheRoot>/<host>/<owner>/<repo>/<version>/<platform.Key()>/`.
  - Cache hit when `.verified.sha256` equals `asset.SHA256` and `sofa-harness-<name>` is a regular file.
  - On miss or invalid marker: downloads `"v" + cfg.Version` asset `asset.Filename` using `harness.ResolveGitHubToken(cfg.SourceHost, opts.LookupEnv)`, verifies SHA-256 before extraction, extracts ZIP via `os.OpenRoot` inside a sibling `.tmp-*` directory (enforcing entry/size limits and rejecting Zip-Slip, backslashes, drive prefixes, symlinks, and special files), enforces `0o755` on regular root entrypoint `sofa-harness-<name>`, writes `.verified.sha256`, removes any stale target directory, and atomically swaps via `os.Rename`.

## 3. `internal/sandbox/runtime`

- **Zero secret env var invariant**: `BuildRunArgs` emits only `HOME=/tmp/sofa-home`, `TMPDIR=/tmp`, `GIT_OPTIONAL_LOCKS=0` and exposes no arbitrary env map.
- **`ComputeWorkspaceMounts`**:
  - Rejects `.git`, `.git/**`, absolute paths, backslashes, and `..` segments in `writablePaths`.
  - `readOnlyWorkspace == true`: `[{HostWorkspaceDir, "/workspace", true}]`.
  - `writablePaths == nil`: `[{HostWorkspaceDir, "/workspace", false}, {HostWorkspaceDir/.git, "/workspace/.git", true}]` (when `.git` exists).
  - `len(writablePaths) == 0` (non-nil empty): `[{HostWorkspaceDir, "/workspace", true}]`.
  - Non-empty `writablePaths`: root wildcards (`*`, `**`, `.`, `*.ext`) or absent single-segment root files fall back to `/workspace:rw` + `/.git:ro`; existing single-segment literal files mount `<file>:/workspace/<file>:rw`; multi-segment literal-prefix paths auto-create `<topDir>` via `os.OpenRoot` `MkdirAll` (rejecting symlinks) and mount `<topDir>:/workspace/<topDir>:rw` over `/workspace:ro`, always appending `/.git:ro` last when `.git` exists.
- **Cross-platform Agent network guard**: `ForAgentSession && HostOS != "linux" && Sandbox.Network == "none"` returns an error.
- **`PodmanEngine`**:
  - `DetectPlatform`: resolves `linux/amd64` or `linux/arm64` from container runtime `info`.
  - `Exec`: runs `<runtimeBin> exec -i -w <workDir> [-e K=V ...] <containerName> <command...>`, returning non-zero exit codes in `ExecResult.ExitCode` with `err == nil` and returning `error` only on spawn failure or context cancellation/timeout.
  - `RunGatekeepers`: executes gatekeepers sequentially with per-gatekeeper timeout and `/.sofa/artifacts:ro`. When `RequireCleanTree == true`, snapshots pre-gatekeeper `git status` and pre-existing dirty/untracked file contents before execution, detects any additions, deletions, or mutations (including edits/deletions of pre-existing uncommitted Worker files) afterward, restores the exact pre-gatekeeper workspace state on violation/failure/timeout, and immediately returns `*GatekeeperFailure`.
