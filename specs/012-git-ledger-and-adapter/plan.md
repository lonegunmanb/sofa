# Implementation Plan: Managed Branch Ledger & Zero-Credential-Persistence Git CLI Adapter (`012-git-ledger-and-adapter`)

**Branch**: `012-git-ledger-and-adapter` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `012`: implement the pure-domain thread-safe managed branch `Ledger` and deterministic branch naming (`internal/git/ledger.go`), the `writable_paths` glob matcher (`internal/git/pathmatch.go`), the explicit `Adapter` interface (`internal/git/adapter.go`), and the host `git` CLI adapter `CLIAdapter` (`internal/git/cli.go`, `permissions_windows.go`, `permissions_unix.go`) with Tier 1 unit tests (`ledger_test.go`) and Tier 2 real-Git integration tests (`cli_integration_test.go`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Existing Dependencies Consumed**: `golang.org/x/sys/windows` (already in `go.mod`, used for Windows protected DACL on temp SSH keys matching `internal/blob`), Go stdlib (`os.OpenRoot`, `os/exec`, `crypto/sha256`, `encoding/base64`, `encoding/json`, `net/url`, `path`, `path/filepath`, `sync`)
- **Testing**: Tier 1 offline unit tests (`go test ./... -count=1`) + Tier 2 real `git` CLI integration tests (`go test -tags=integration ./internal/git/... -count=1`)

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts created under `specs/012-git-ledger-and-adapter/` prior to writing Go production code.
- **Principle II (Strict TDD)**: Failing Tier 1 tests in `internal/git/ledger_test.go` and Tier 2 tests in `internal/git/cli_integration_test.go` are authored and verified Red before implementing production code.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `ledger.go` and `pathmatch.go` are 100% pure Go with zero `os/exec` calls; external Git operations sit behind `git.Adapter` and are tested against real `git init --bare` and working repositories under `//go:build integration`.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Branch registration precedes push/delete (`Ledger.EnsureRegistered`), `base_sha` is strictly validated as 40-char lowercase hex, `.git/config` persists zero credentials, and `RevertPaths` confines deletions via `os.OpenRoot`.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `internal/source/schema.go`, `internal/merge/spec/schema.go`, `internal/worker/spec/artifact.go`, `internal/blob/permissions_{windows,unix}.go`, `docs/stage2/012-git-ledger-and-adapter.md`, `017`, and `018`, plus verified host `git status --porcelain=v1 -z` byte output on `git 2.56.0.windows.4` before designing.

## Design Decisions

1. **`internal/git/ledger.go`**:
   - Deterministic `WorkBranchName` and `CheckpointBranchName` using `sanitizeRefSegment` and `sanitizeNodeKey` (with 8-char SHA-256 hex of raw `canonicalNodeAddr`).
   - `NormalizeRepoURL` normalizes HTTP/HTTPS/SSH schemes and hosts to lowercase, cleans SCP-style hostnames, cleans `file://` and local paths with slash normalization, and strips trailing `/` and `.git` consistently.
   - Thread-safe `Ledger` (`sync.RWMutex`) tracks protected `SourceRef` entries and `BranchEntry` records keyed by `(NormalizeRepoURL(repoURL), branch)`. Supports `Register`, `EnsureRegistered`, `Entries`, `PlanCleanup`, `Snapshot`, and `Restore`.
2. **`internal/git/pathmatch.go`**:
   - `ChangedPath{Path, OldPath, Untracked}` and `FindUnauthorizedPaths(changed, writableGlobs)` implementing dynamic-programming segment glob matching for `*`, `?`, and `**` (0+ directory segments), enforcing both `OldPath` and `Path` for renames.
3. **`internal/git/adapter.go` & `internal/git/cli.go`**:
   - `Adapter` interface, `AuthConfig`, `RebaseOutcome`, and `CLIAdapter` with injectable `LookupEnv` and optional `*Ledger` enforcement.
   - `buildAuthEnv` clears ambient `GIT_*` config/auth overrides, sets `GIT_TERMINAL_PROMPT=0`, injects HTTPS basic auth via `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0` (redacting token & base64 secret from any returned error), and writes inline SSH keys to temp files with `0600` / Windows protected DACL removed via `defer`.
   - `RevertPaths` handles tracked modifications/deletions (`git checkout HEAD --`), staged additions not in `HEAD` (`git reset HEAD --` + `os.OpenRoot` removal), and untracked files (`os.OpenRoot` removal + empty parent dir pruning).

## Files

```text
internal/git/ledger.go
internal/git/pathmatch.go
internal/git/adapter.go
internal/git/cli.go
internal/git/permissions_windows.go
internal/git/permissions_unix.go
internal/git/ledger_test.go
internal/git/cli_integration_test.go
specs/012-git-ledger-and-adapter/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
