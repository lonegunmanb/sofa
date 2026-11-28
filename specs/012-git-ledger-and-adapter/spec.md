# Feature Specification: Managed Branch Ledger & Zero-Credential-Persistence Git CLI Adapter (`012-git-ledger-and-adapter`)

**Feature Branch**: `012-git-ledger-and-adapter`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `012` blueprint (`docs/stage2/012-git-ledger-and-adapter.md`), `docs/stage2/README.md`, `internal/source/schema.go`, `internal/merge/spec/schema.go`, `internal/worker/spec/artifact.go`, `internal/blob/permissions_{windows,unix}.go`.

## User Scenarios & Testing

### User Story 1 - Deterministic Branch Naming & Managed Branch Ledger (`internal/git/ledger.go`) (Priority: P1)

Before any worker or checkpoint branch is created or pushed, the runtime registers ownership in a thread-safe managed branch `Ledger` keyed by normalized repository URL and branch name, pinning `base_sha` to an exact 40-character lowercase hex SHA-256/Git commit hash. Deterministic branch names (`WorkBranchName`, `CheckpointBranchName`) prevent collisions between distinct canonical node addresses whose sanitized forms match (such as `worker.a.b` vs `worker.a_b`) via an 8-character short SHA-256 suffix of the raw canonical node address. Protected `source` branches are rejected unless explicitly registered as `KindMergeBranch`, conflicting ownership across nodes is rejected, and `PlanCleanup` returns the deterministic list of registered intermediate branches while preserving all `source` and `preserve` branches (under normalized repository URL comparison) and never touching unregistered branches. `Snapshot()` and `Restore()` allow full JSON serialization for Stage 3 `RunActor` persistence.

**Independent Test**: Tier 1 unit tests (`internal/git/ledger_test.go`) covering `WorkBranchName` / `CheckpointBranchName` collision freedom, `NormalizeRepoURL` variants (`.git`, trailing `/`, scheme/host case, `file://` and local paths), `Register` base SHA validation, protected source rejection vs `KindMergeBranch` allowance, ownership conflict rejection, idempotent re-registration & epoch advancement, `EnsureRegistered` error on unregistered branches, `PlanCleanup` with `sources` and `preserves` exclusions, and `Snapshot()`/`Restore()` JSON round-trip.

### User Story 2 - Workspace Diff Inspection & `writable_paths` Glob Enforcement (`internal/git/pathmatch.go`) (Priority: P1)

Before checkpointing, completing work, or entering a gatekeeper container, the host inspects changed workspace paths (`ChangedPath{Path, OldPath, Untracked}`) and enforces `writable_paths` globs via `FindUnauthorizedPaths`. Both `OldPath` and `Path` of Git renames (`R`) must satisfy `writable_paths`. `writableGlobs == nil` allows all modifications; non-nil empty `[]string{}` forbids all modifications; non-empty globs support POSIX `/` matching with `*`, `?`, and `**` (zero or more directory segments).

**Independent Test**: Tier 1 unit tests (`internal/git/ledger_test.go`) covering `nil` vs `[]string{}` vs `["cmd/**", "internal/**/*.go", "go.mod"]`, zero-segment `**` (`internal/main.go` matching `internal/**/*.go`), renames where either `OldPath` or `Path` is unauthorized, and sorted deduplicated output.

### User Story 3 - Zero-Credential-Persistence Host `git` CLI Adapter (`internal/git/adapter.go`, `internal/git/cli.go`) (Priority: P1)

`CLIAdapter` implements `Adapter` using the host `git` CLI without any shared bare cache:
- `PrepareWorkspace` initializes an isolated repo (`user.name`, `user.email`, `commit.gpgsign = false`, `core.autocrlf = false`), fetches `--no-tags` for the target branch into `refs/remotes/sofa/base` with zero credentials persisted in `.git/config`, verifies `pinSHA` is a valid commit ancestor of the fetched tip, and checks out a local `work` branch at `pinSHA`.
- `buildAuthEnv` injects `GIT_TERMINAL_PROMPT=0` plus either HTTPS `GIT_CONFIG_*` `http.extraHeader` (`Authorization: Basic ...`, redacting both raw token and encoded header in any error) or `GIT_SSH_COMMAND` (writing inline `-----BEGIN` keys to a temp file with `0600` / Windows protected DACL and removing it via `defer` after command completion).
- `StatusPaths` parses `git -c core.quotepath=false status --porcelain=v1 -z -uall` NUL-delimited output cleanly.
- `RevertPaths` reverts unauthorized tracked/staged changes (`git checkout HEAD --` / `git reset HEAD --`) and deletes unauthorized untracked/newly-staged files safely via `os.OpenRoot(workspaceDir)` while pruning newly emptied parent directories and preserving all authorized modifications.
- `CommitAllAndPush`, `BranchContainsSHA`, `DeleteRemoteBranch` (with optional `*Ledger` guard enforcing `EnsureRegistered` on push/delete and idempotent missing-branch behavior), and `RebaseCandidate` (returning `RebaseOutcome` with conflict files and aborting cleanly on conflict, or leaving the clean rebased tree checked out for `gatekeeper`) complete the Git lifecycle.

**Independent Test**: Tier 1 unit tests in `internal/git/ledger_test.go` for `buildAuthEnv` (HTTPS basic header, token + base64 redaction, SSH inline key temp file permissions & cleanup, mutual exclusion) and `StatusPaths` porcelain `-z` parsing; Tier 2 real-Git integration tests (`//go:build integration`) in `internal/git/cli_integration_test.go` against real `git init --bare` and working repositories.

### Edge Cases

- URLs with trailing `/` and/or `.git` suffix (e.g. `https://GitHub.com/Org/Repo.git/` vs `https://github.com/Org/Repo`) MUST match identically in ledger lookups and `PlanCleanup`.
- Repository URLs embedding passwords (`https://user:pass@host/repo`) MUST be rejected so secrets are never leaked into `.git/config` or error messages.
- `git status --porcelain=v1 -z -uall` emits `XY <newPath>\x00<oldPath>\x00` for renames (`R`) and copies (`C`), which must be parsed with `Path = newPath` and `OldPath = oldPath`.
- Staging newly added files (`A ` or rename destination not in `HEAD`) passed to `RevertPaths` must be unstaged (`git reset HEAD --`) and removed via `os.OpenRoot` without failing `git checkout HEAD --`.
- On Windows, temp SSH private key files must close all handles before applying current-user protected DACL (`D:P(A;;FA;;;<sid>)`), and `GIT_SSH_COMMAND` must pass slash-normalized quoted paths.
- Isolated repositories initialized on Windows hosts where global Git config has `core.autocrlf = true` MUST set `core.autocrlf = false` locally so checked-out source files preserve exact LF repository bytes when bind-mounted into Linux containers (preventing `gofmt -l` and `git status` CRLF mismatches on read-only mounts).

## Requirements

- **FR-001**: `internal/git/ledger.go` MUST provide deterministic `WorkBranchName`, `CheckpointBranchName`, `NormalizeRepoURL`, `IsValidCommitSHA`, and thread-safe `Ledger` (`Register`, `EnsureRegistered`, `Entries`, `PlanCleanup`, `Snapshot`, `Restore`).
- **FR-002**: `internal/git/pathmatch.go` MUST define `ChangedPath` and `FindUnauthorizedPaths` with exact `nil` vs `[]string{}` vs `**`/`*`/`?` glob semantics and rename `OldPath`+`Path` enforcement.
- **FR-003**: `internal/git/adapter.go` MUST define `AuthConfig`, `RebaseOutcome`, and `Adapter` matching `docs/stage2/012-git-ledger-and-adapter.md` §3.2.
- **FR-004**: `internal/git/cli.go` MUST implement `CLIAdapter` with injectable `LookupEnv`, optional `*Ledger` guard on `CommitAllAndPush` and `DeleteRemoteBranch`, zero `.git/config` credential persistence, token/base64 redaction in errors, Windows/Unix-safe temp SSH key handling, `os.OpenRoot`-confined `RevertPaths`, and `RebaseCandidate`.

## Success Criteria

- **SC-001**: Tier 1 unit tests in `internal/git/ledger_test.go` pass via `go test ./... -count=1` offline with zero external processes.
- **SC-002**: Tier 2 real-Git integration tests in `internal/git/cli_integration_test.go` pass via `go test -tags=integration ./internal/git/... -count=1`.
- **SC-003**: `gofmt -l .` produces empty output and `go vet ./...` passes with zero errors.
