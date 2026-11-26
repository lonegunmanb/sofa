# Research: Managed Branch Ledger & Git CLI Adapter (`012`)

## 1. Existing `sofa` Patterns Inspected

- **`internal/source/schema.go`**: Validates repository URLs (`http`, `https`, `ssh`, `file`, SCP-style `user@host:path`, and local paths) and rejects passwords embedded in HTTP(S) URLs.
- **`internal/spec/branch_ref.go`**: Defines `ValidateBranchName` rules (no leading/trailing `/` or `.`, no `..`, `@{`, `.lock`, whitespace, or control chars). Deterministic `WorkBranchName` and `CheckpointBranchName` must always satisfy these rules.
- **`internal/worker/spec/artifact.go`**: Defines `writable_paths` validation allowing `"."` and clean relative POSIX paths.
- **`internal/blob/permissions_windows.go` & `permissions_unix.go`**: Demonstrates closing file handles before applying `SetNamedSecurityInfo` (`D:P(A;;FA;;;<sid>)` + `PROTECTED_DACL_SECURITY_INFORMATION`) on Windows and `0600` on Unix. OpenSSH on Windows requires strict current-user-only ACLs on private key files passed via `-i`.
- **`docs/stage2/017-two-phase-checkpoint-and-recovery.md` & `018-worker-qc-and-merge-runner.md`**: Confirm `CommitAllAndPush` is expected to ensure the remote branch exists at `headSHA` even when `committed == false` (no new commit created), and `RebaseCandidate` leaves `workDir` checked out at `RebasedSHA` on success so `gatekeeper` can execute against it.

## 2. Verified Host `git` CLI Behavior (`git version 2.56.0.windows.4`)

- Probed `git -c core.quotepath=false status --porcelain=v1 -z -uall` after `git mv old.txt new.txt`:
  - Output bytes: `R  new.txt\x00old.txt\x00`.
  - Confirmed that for renames (`R`) and copies (`C`), the first NUL-terminated path after `XY ` is the destination `Path` (`new.txt`), and the immediately following NUL-terminated token is `OldPath` (`old.txt`).
- `git checkout HEAD -- <path>` fails if `<path>` does not exist in `HEAD` (e.g., staged newly added file or destination of `git mv`). Inspecting `HEAD:<path>` via `git cat-file -e` separates `HEAD`-backed paths (restored via `git checkout HEAD --`) from newly staged/untracked paths (unstaged via `git reset HEAD --` if tracked in index, then removed safely via `os.OpenRoot`).
