---
description: "Stage 2 Slice 012 TDD tasks: Managed Branch Ledger, writable_paths Glob Matcher, and Zero-Credential Host Git CLI Adapter"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/git/ledger_test.go`:
  - `WorkBranchName` and `CheckpointBranchName` collision-freedom (`a.b` vs `a_b`, bracketed `for_each` addresses) and branch-name validity;
  - `NormalizeRepoURL` across HTTPS (`.git`, trailing `/`, mixed-case host/scheme), SSH/SCP, `file://`, and local paths;
  - `Ledger.Register` 40-char lowercase hex `baseSHA` check, protected `source` branch rejection vs `KindMergeBranch` allowance, cross-node ownership conflict rejection, idempotent re-registration & epoch advancement, `EnsureRegistered` error on unregistered branches, `PlanCleanup` excluding `sources` & `preserves` (with normalized URLs) and never returning unregistered branches, and JSON `Snapshot()`/`Restore()`;
  - `FindUnauthorizedPaths` covering `nil` (allow all), `[]string{}` (forbid all), `["cmd/**", "internal/**/*.go", "go.mod"]` (including 0-segment `**` match), and Git rename `OldPath` + `Path` enforcement;
  - `CLIAdapter` Tier 1 `buildAuthEnv` and `parseStatusPorcelainZ` tests: `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_*` HTTPS basic header + token & base64 redaction in errors, inline SSH key temp file `0600`/Windows DACL + immediate removal, mutual exclusion of `TokenRef` and `SSHKeyRef`, and `-z` rename parsing (`R  new\x00old\x00`).
- [X] T002 [Green] Implement `internal/git/ledger.go`, `internal/git/pathmatch.go`, `internal/git/adapter.go`, `internal/git/permissions_windows.go`, `internal/git/permissions_unix.go`, and the auth/status parts of `internal/git/cli.go`, and verify `go test ./internal/git/... -count=1` passes.
- [X] T003 [Red] Write failing Tier 2 real-Git integration tests in `internal/git/cli_integration_test.go` (`//go:build integration`):
  - Create real `git init --bare` remote with initial commit;
  - Test `ResolveBranchSHA` -> `PrepareWorkspace` (assert `<workspace>/.git/config` contains zero credentials/remote secrets) -> modify allowed & unauthorized files, stage a new unauthorized file, create an untracked nested unauthorized file, and rename across unauthorized/authorized boundaries -> `StatusPaths` + `FindUnauthorizedPaths` -> `RevertPaths` (confirming only unauthorized changes are reverted/removed with empty parent pruning while authorized modifications remain intact);
  - Test `CommitAllAndPush` (both with changes `committed == true` and clean tree branch push `committed == false`, plus optional `*Ledger` unregistered guard) -> `BranchContainsSHA` (ancestor, equal, non-ancestor, missing branch) -> `DeleteRemoteBranch` (including idempotent repeat delete);
  - Test `RebaseCandidate` on both non-conflicting parallel branches (linear `RebasedSHA` checked out in `workDir`) and conflicting branches (`Conflicted: true`, `ConflictFiles`, `TargetSHA`, clean abort).
- [X] T004 [Green] Complete `internal/git/cli.go` implementation for all `Adapter` methods and verify `go test -tags=integration ./internal/git/... -count=1` passes.
- [X] T005 [Refactor & Verify] Review code for clarity/duplication, run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -tags=integration ./internal/git/... -count=1`, and mark all tasks `[X]`.
