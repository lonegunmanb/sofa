# Research: Plugin Cache Verification & Podman Sandbox Runtime (`013`)

## 1. Existing Repository Packages Inspected

- **`internal/harness/lock.go` & `internal/harness/spec/schema.go`**:
  - `CanSatisfyHarnessLock(cfg, lockFile)` enforces source/version equality and signature policy parity; `ResolveGitHubToken(host, lookupEnv)` resolves `GITHUB_TOKEN_<NORMALIZED_HOST>` then `GITHUB_TOKEN`.
  - `httpReleaseFetcher` is currently unexported inside `internal/harness/lock.go`. Exporting `NewDefaultReleaseFetcher() ReleaseFetcher` allows `internal/harness/installer` to reuse the exact HTTP fetcher (with token redaction and loopback HTTP support for local tests) without duplicating code.
- **`internal/sandbox/spec/schema.go`**:
  - `SandboxConfig` defines `Runtime` (`podman`, `docker`, `nerdctl`, `kirk`), `Image`, `Network` (`none`, `bridge`, `host`), `Mounts []MountSpec` (already validated not to collide with `/workspace` or `/.sofa` and to have `ReadOnly == true`), and `*ResourcesSpec` (`CPUs`, `Memory`).
- **`internal/worker/spec/gatekeeper.go`**:
  - `GatekeeperConfig` exposes `Name`, `Command []string`, `WorkingDir string` (default `"."`), `Timeout string` (default `"10m"`), `ExpectExitCode int` (default `0`), and `RequireCleanTree bool` (default `true`).
- **`internal/git/adapter.go` & `internal/git/cli.go`**:
  - `git.Adapter` provides `StatusPaths(ctx, workspaceDir)` (returning `[]ChangedPath` with `Path`, `OldPath`, `Untracked`) and `RevertPaths(ctx, workspaceDir, paths)` (`HEAD` restore via `git checkout HEAD --`, unstaging via `git reset HEAD --`, and `os.OpenRoot`-confined untracked file removal + empty parent pruning).
- **`internal/artifact/service.go`**:
  - Demonstrates `os.OpenRoot`-confined safe ZIP extraction (rejecting `\`, leading `/`, drive letters, `..`, symlinks, special file modes, entry count limits, and single/total uncompressed byte limits) and per-key `sync.Mutex` locking.

## 2. Verified Local Podman WSL2 Behavior

- Probed `podman info --format "{{.Version.OsArch}}"` and `podman info --format "{{.Host.OS}}/{{.Host.Arch}}"`:
  - Both return `linux/amd64` on the current Windows + Podman WSL2 machine.
  - Note: Podman Go template uses `.Host.OS` (uppercase `OS`), whereas Docker uses `.OSType`/`.Architecture`. Checking `{{.Version.OsArch}}` first with `{{.Host.OS}}/{{.Host.Arch}}` and `{{.OSType}}/{{.Architecture}}` fallbacks works across Podman, Docker, and nerdctl.
- Probed nested bind mounts in real `alpine:latest` container on Windows Podman WSL2:
  - `-v <hostWorkspace>:/workspace:ro -v <hostWorkspace>/allowed:/workspace/allowed:rw -v <hostWorkspace>/.git:/workspace/.git:ro` allows writes inside `/workspace/allowed` while the Linux kernel enforces `Read-only file system` on both `/workspace/forbidden.txt` and `/workspace/.git/HEAD`.
- Checked Windows path constraints:
  - Remote test hosts using `httptest.Server` have `SourceHost` like `127.0.0.1:<port>`, containing `:`, which is illegal in Windows directory names; cache directory segment sanitization converts `:` to `_` while preserving standard hostnames like `github.com` unchanged.
  - Windows `os.Rename` requires all handles inside the staging directory to be closed and any existing destination directory to be removed first under the target mutex.
