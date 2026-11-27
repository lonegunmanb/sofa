# Implementation Plan: Adopt Official `copilot-sdk/go` `embeddedcli` & `cmd/bundler` (`033-copilot-sdk-bundler`)

**Branch**: `033-copilot-sdk-bundler` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/033-copilot-sdk-bundler/spec.md)

## Summary

Deliver Slice `033` across three coordinated components following strict Red → Green → Refactor TDD:
1. **`sofa-harness-copilot` Server & Entrypoint (`plugins/sofa-harness-copilot/`)**:
   - Update `resolveCLIPath` in `internal/server/server.go` to default to `"", nil` (delegating to `copilot-sdk/go`'s `embeddedcli`) with precedence `options.cli_path` → `<execDir>/bin/copilot` → optional `options.use_path_cli` → `""`.
   - Add `EnsureCopilotHome()` called in `cmd/sofa-harness-copilot/main.go` and `Server.OpenSession`, plus propagate `BaseDirectory` on `sdk.ClientOptions` in `defaultClientFactory`.
   - Sync `plugins/sofa-harness-copilot/go.mod` & `go.sum` with `github.com/klauspost/compress/zstd` and complete transitive checksums.
2. **Official `cmd/bundler` Packaging (`plugins/sofa-harness-copilot/scripts/build-bundle.go`)**:
   - Default mode stages an isolated temp module copy, restores or runs `go run github.com/github/copilot-sdk/go/cmd/bundler --platform linux/<arch> --output <stagedCmdDir>`, caches `zcopilot*` files under `<cache-dir>/copilot-bundler-linux-<arch>/`, and compiles a single-binary Linux `sofa-harness-copilot`.
   - Override mode (`-copilotbin` / `-copilotsrc`) remains intact for offline integration stubs.
3. **Zero-npm Installer & Docs (`scripts/install-local-plugins.go`, `examples/copilot-agy-self-host/README.md`, `examples/examples_test.go`)**:
   - Remove `npm`/`npm.cmd`/`.tgz` extraction from `scripts/install-local-plugins.go`, invoke `build-bundle.go -cache-dir <cacheDir>` in default mode, and conditionally verify `bin/copilot` only for override flags.
   - Add offline cache-seeded single-binary test to `examples/examples_test.go`.

## Technical Context

- **Language / Dependencies**: Go 1.27, `github.com/github/copilot-sdk/go v1.0.19` (`embeddedcli`, `cmd/bundler`), `github.com/klauspost/compress/zstd`.
- **Testing**:
  - Tier 1 (`plugins/sofa-harness-copilot/internal/server/server_test.go`, `examples/examples_test.go`): deterministic, offline, zero network.
  - Tier 2 (`internal/worker/runner/copilot_plugin_integration_test.go`): real Podman container verification.
- **Constraints**:
  - Do not run `git commit` or `git push`.
  - Never generate `zcopilot_*` files inside the git working tree.
  - Preserve Stage 2 `-copilotsrc` / `-copilotbin` compatibility.

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored under `specs/033-copilot-sdk-bundler/` before edits.
- **Principle II (Strict TDD)**: Failing tests in `server_test.go` and `examples_test.go` written and observed failing before implementation edits.
- **Principle III (Pure Core & Real Boundaries)**: `Server` tests use injected `sdkClientFactory` / `execPath` / `lookPath`; Podman integration test exercises real container boundary.
- **Principle VI (Research-First & Surgical Sizing)**: Inspected `copilot-sdk/go@v1.0.19` `embeddedcli.go`, `cmd/bundler/main.go`, and `client.go` before planning.

## Complexity Tracking

No constitution exceptions required.
