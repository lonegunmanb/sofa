# Specification Quality Checklist: Managed Branch Ledger & Git CLI Adapter

- [x] Deterministic branch naming (`WorkBranchName`, `CheckpointBranchName`) with 8-char SHA-256 collision-prevention suffix is specified
- [x] `Ledger` registration invariants, `baseSHA` validation, source protection vs `KindMergeBranch`, `PlanCleanup`, and `Snapshot`/`Restore` are documented
- [x] `FindUnauthorizedPaths` (`nil` vs `[]string{}` vs `**`/`*`/`?` globs and rename `OldPath`+`Path`) is specified
- [x] Zero-credential-persistence `CLIAdapter`, error secret redaction, Windows/Unix SSH temp key permissions, `os.OpenRoot` `RevertPaths`, and `RebaseCandidate` are documented
- [x] Strict Tier 1 (`go test ./...`) and Tier 2 (`//go:build integration`) separation is preserved
