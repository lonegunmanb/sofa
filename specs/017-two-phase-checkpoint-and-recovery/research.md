# Research: Epoch-Scoped Two-Phase Checkpoint & Cross-Machine Aligned Recovery (`017-two-phase-checkpoint-and-recovery`)

## Decision 1: Adding `HeadSHA` to `git.Adapter` Instead of `os/exec` in `internal/checkpoint`

- **Decision**: Add `HeadSHA(ctx context.Context, workspaceDir string) (string, error)` to `git.Adapter` (`internal/git/adapter.go`) and implement it via `git rev-parse HEAD` + `IsValidCommitSHA` validation on `*git.CLIAdapter` (`internal/git/cli.go`).
- **Rationale**: `AGENTS.md` Rule 5 and Constitution Principle III require domain packages under `internal/` (other than boundary adapters) to stay free of `os/exec`. Searching the codebase confirmed `CLIAdapter` is currently the only `git.Adapter` implementation, so extending `git.Adapter` breaks zero existing packages or mocks.

## Decision 2: Same-Epoch Branch Registration Idempotency Across Multiple Checkpoints

- **Decision**: Track registered epoch branches in both `LocalNodeStateStore` (`registeredBranches` keyed by normalized repo URL + branch) and `Manager` (`registeredEpochs` map), skipping redundant `RegisterCheckpointBranch` calls when an epoch's checkpoint branch is already registered.
- **Rationale**: `git.Ledger.Register` rejects re-registration at the same `epoch` if `baseSHA` differs (`ledger.go:319-322`). Within a single `epoch`, Checkpoint 1 registers `sofa/ckpt/.../e1` with `initialSHA = sha0`, commits `sha1`, and then Checkpoint 2 starts with `initialSHA = sha1` on the same `e1` branch. Tracking epoch branch registration avoids triggering a false `base_sha` mismatch on Checkpoint 2+, while `LocalNodeStateStore.RegisterCheckpointBranch` remains idempotent when invoked with the exact same `(epoch, repoURL, branch, baseSHA)` (e.g., after `RecoverLatestAligned` registers `e<newEpoch>` and a fresh `Manager` later runs `SaveCheckpoint` before any checkpoint is written on `newEpoch`).

## Decision 3: Deterministic `blob.ValidateKey`-Safe Checkpoint Artifact Keys

- **Decision**: Construct checkpoint staging archive keys as `runs/<safeRunKey>/checkpoints/<safeNodeKey>/seq-<06d>-e<epoch>-artifacts.zip`, sanitizing `runID` and `nodeAddr` (plus an 8-hex SHA-256 hash suffix for `nodeAddr`) to `[A-Za-z0-9._-]`.
- **Rationale**: `blob.ValidateKey` (`internal/blob/store.go`) allows only `[A-Za-z0-9._-]` per slash-separated segment, whereas `canonicalNodeAddr` for `dynamic_worker` nodes contains brackets, quotes, or dots/slashes. Sanitizing with a deterministic SHA-256 suffix guarantees valid keys and collision freedom.

## Decision 4: Atomic JSON Persistence & Monotonic Epoch Preservation in `LocalNodeStateStore`

- **Decision**: Persist `LocalNodeStateStore` state (`epoch`, `records`, `registeredBranches`) via temp-file creation in the target directory + `os.Rename`, and load existing state on startup when `StatePath` already exists without allowing `InitialEpoch` to downgrade a higher persisted `epoch`.
- **Rationale**: Ensures simulated cross-machine recovery (`Machine A` directory deleted, `Machine B` advances `epoch = 2` against shared state file) survives process restart and prevents stale processes from resetting authoritative `epoch`.
