# Implementation Plan: Harness Plugin Cache Verification & Podman Sandbox / Gatekeeper Runtime (`013-plugin-cache-and-sandbox`)

**Branch**: `013-plugin-cache-and-sandbox` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `013`:
1. Export `NewDefaultReleaseFetcher()` from `internal/harness/lock.go` to avoid duplicating HTTP release download logic.
2. Implement `internal/harness/installer` (`Platform`, `PreparedPlugin`, `ResolveOptions`, `Installer`) for 4-level local plugin discovery and SHA-256-locked remote ZIP caching with `os.OpenRoot` Zip-Slip/Zip-Bomb/symlink protection and atomic activation.
3. Implement `internal/sandbox/runtime` (`spec.go` & `podman.go`) for zero-secret container mount & CLI arg generation (`ComputeWorkspaceMounts`, `BuildRunArgs`), `PodmanEngine` (`DetectPlatform`, `Exec`, `RunGatekeepers`), and exact pre/post-gatekeeper workspace dirty detection and restoration.
4. Verify with Tier 1 unit tests (`installer_test.go`, `podman_test.go`) and Tier 2 real Podman (`alpine:latest`) + real Git integration tests (`podman_integration_test.go`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Dependencies Reused**:
  - `internal/harness` (`CanSatisfyHarnessLock`, `ValidateLockFile`, `ResolveGitHubToken`, `ReleaseFetcher`, `NewDefaultReleaseFetcher`)
  - `internal/harness/spec` (`HarnessConfig`)
  - `internal/sandbox/spec` (`SandboxConfig`, `MountSpec`, `ResourcesSpec`)
  - `internal/worker/spec` (`GatekeeperConfig`)
  - `internal/git` (`Adapter`, `CLIAdapter`, `ChangedPath`)
  - Go stdlib (`archive/zip`, `crypto/sha256`, `encoding/hex`, `os.OpenRoot`, `os/exec`, `path`, `path/filepath`, `sync`)
- **Testing**: Tier 1 offline unit tests (`go test ./... -count=1`) + Tier 2 real Podman/Git integration tests (`go test -tags=integration ./internal/sandbox/runtime/... -count=1`).

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts created under `specs/013-plugin-cache-and-sandbox/` before writing Go production code.
- **Principle II (Strict TDD)**: Failing Tier 1 tests (`installer_test.go`, `podman_test.go`) and Tier 2 tests (`podman_integration_test.go`) are written and confirmed Red before production implementation.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `ComputeWorkspaceMounts` and `BuildRunArgs` are pure deterministic functions; container runtime interactions sit behind `runtime.Engine` and are tested against real local Podman (`alpine:latest`) under `//go:build integration`.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Enforces container filesystem read/write boundaries via Linux kernel bind mounts (`/workspace:ro`, `writable_paths` sub-mounts, `/workspace/.git:ro` last), zero secret environment variables inside containers, SHA-256 verification before ZIP extraction, and pre/post gatekeeper tree snapshot/restoration.
- **Principle VI (Research-First & Surgical Task Sizing)**: Verified `podman info --format` fields (`{{.Version.OsArch}}` and `{{.Host.OS}}/{{.Host.Arch}}` on Podman WSL2) and Podman WSL2 nested `:ro`/`:rw`/`:ro` bind mount behavior before designing; scoped strictly to Slice `013` without bleeding into `014` (`go-plugin` RPC bridge) or `015` (`go_tool` compiler/executor).

## Files

```text
internal/harness/lock.go
internal/harness/installer/installer.go
internal/harness/installer/installer_test.go
internal/sandbox/runtime/spec.go
internal/sandbox/runtime/podman.go
internal/sandbox/runtime/podman_test.go
internal/sandbox/runtime/podman_integration_test.go
specs/013-plugin-cache-and-sandbox/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
