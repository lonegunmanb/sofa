---
description: "Stage 2 Slice 013 TDD tasks: Harness Plugin Cache Installer and Podman Sandbox / Gatekeeper Runtime"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/harness/installer/installer_test.go`:
  - `Platform.Key()` and `Platform.Validate()` (accept `linux/amd64`, `linux/arm64`; reject non-linux or empty/unknown arch);
  - 4-level local discovery priority (`--plugin-dir` > `SOFA_PLUGIN_DIR` > `./.sofa/plugins` > `~/.sofa/plugins`), platform-subdir preference over flat directory, regular-file check rejecting symlinks/directories, and error listing searched paths when absent;
  - Remote locked cache: lockfile verification (`CanSatisfyHarnessLock`, missing platform asset), first download with `ResolveGitHubToken` + SHA-256 check + executable bit preservation + `0755` entrypoint, second call cache hit with zero fetcher calls, missing/corrupt `.verified.sha256` triggering re-fetch, SHA-256 mismatch rejection prior to extract, Zip-Slip (`../../evil`, `\`, `/abs`, `C:\`), symlink rejection, entry/byte limit rejection, missing root entrypoint rejection, and concurrent installs on the same target.
- [X] T002 [Green] Export `NewDefaultReleaseFetcher()` in `internal/harness/lock.go` and implement `internal/harness/installer/installer.go` so `go test ./internal/harness/... -count=1` passes.
- [X] T003 [Red] Write failing Tier 1 unit tests in `internal/sandbox/runtime/podman_test.go`:
  - `ComputeWorkspaceMounts`: QC `ReadOnlyWorkspace == true`, Work `nil` (`/workspace:rw` + `.git:ro` last), Work `[]string{}` (`/workspace:ro`), Work `["docs/spec.md", "internal/**"]` (auto-creating missing `docs` and `internal` dirs + `:ro` root + `:rw` submounts + `.git:ro` last), existing vs absent single-segment root file (`go.mod`), root wildcard fallback (`*.md`, `*`, `.`), and `.git`/`..`/symlink rejections;
  - `BuildRunArgs`: zero secret `-e` vars (`HOME=/tmp/sofa-home`, `TMPDIR=/tmp`, `GIT_OPTIONAL_LOCKS=0` only), `--rm`, `--init`, `-w`, `--network`, `--cpus`, `--memory`, `--name`, `-d`, `-p 127.0.0.1::19500`, `--entrypoint`, user `:ro` mounts, system `/.sofa/*` mounts, `.git:ro` last, and non-Linux `ForAgentSession` + `network = "none"` rejection;
  - `PodmanEngine`: `DetectPlatform` parsing (Podman `OsArch`, `Host.OS/Arch`, Docker `x86_64`/`aarch64`), `Exec` exit-code vs error behavior (fake script/binary or mock runner), and `RunGatekeepers` pre/post `RequireCleanTree` snapshot detection and restoration of both new garbage files and mutated/deleted pre-existing Worker edits.
- [X] T004 [Green] Implement `internal/sandbox/runtime/spec.go` and `internal/sandbox/runtime/podman.go` so `go test ./internal/sandbox/... -count=1` passes.
- [X] T005 [Red] Write failing Tier 2 real-Podman integration tests in `internal/sandbox/runtime/podman_integration_test.go` (`//go:build integration`):
  - `DetectPlatform` against real Podman;
  - `writable_paths = ["allowed/**"]` in real `alpine:latest` container: `/workspace/allowed/ok.txt` succeeds on host, `/workspace/forbidden.txt` and `/workspace/.git/HEAD` fail with `Read-only file system`;
  - Detached `alpine:latest` (`sleep 30`) + `Exec` passing JSON via `stdin`, verifying `Stdout`, `Stderr`, non-zero `ExitCode`, and container cleanup;
  - `RunGatekeepers` against real `alpine:latest` covering: (1) pass with matching exit code and clean tree, (2) exit code mismatch failure, (3) timeout cancellation, (4) `require_clean_worktree = true` on a workspace with an existing uncommitted Worker edit (`allowed/worker_edit.txt`) where gatekeeper creates `garbage.txt` AND mutates `allowed/worker_edit.txt` -> catches both `DirtyFiles`, deletes `garbage.txt`, and restores `allowed/worker_edit.txt` to exact pre-gatekeeper bytes.
- [X] T006 [Green & Verify] Pass `go test -tags=integration ./internal/sandbox/runtime/... -count=1`, `gofmt -l .` (empty), `go vet ./...`, and `go test ./... -count=1`, and mark all tasks `[X]`.
