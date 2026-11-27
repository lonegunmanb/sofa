# Feature Specification: Adopt Official `copilot-sdk/go` `embeddedcli` & `cmd/bundler` (`033-copilot-sdk-bundler`)

**Feature Branch**: `033-copilot-sdk-bundler`

**Created**: 2026-11-27

**Status**: Approved

**Input**: `copilot-sdk/go` `v1.0.19` (`internal/embeddedcli/embeddedcli.go`, `cmd/bundler/main.go`, `client.go`), `plugins/sofa-harness-copilot/internal/server/{server,sdk_iface,server_test}.go`, `plugins/sofa-harness-copilot/cmd/sofa-harness-copilot/main.go`, `plugins/sofa-harness-copilot/scripts/build-bundle.go`, `plugins/sofa-harness-copilot/go.mod`, `scripts/install-local-plugins.go`, `internal/sandbox/runtime/spec.go`, `internal/worker/runner/copilot_plugin_integration_test.go`, `examples/examples_test.go`, and `examples/copilot-agy-self-host/README.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Default `embeddedcli` Resolution & Container-Safe `COPILOT_HOME` in `sofa-harness-copilot` (Priority: P1)

When `sofa-harness-copilot` runs inside a container (including `glibc` and `musl`/Alpine images) with no external `bin/copilot` sidecar and no `options.cli_path`, `OpenSession` delegates runtime startup directly to `copilot-sdk/go`'s built-in `embeddedcli` (`Path: ""`) and ensures `COPILOT_HOME` points to a pre-created writable directory (`/tmp/sofa-copilot` by default when unset) so `embeddedcli` reliably extracts `copilot-runtime` + `runtime.node` and Copilot stores runtime state without hitting a missing or read-only directory.

- **CLI Path Precedence**:
  1. Explicit `options.cli_path` (non-empty trimmed string in `HarnessOptionsJson`);
  2. `<execDir>/bin/copilot` regular file (preserved for `-copilotbin` / `-copilotsrc` test stubs);
  3. `PATH` lookup via `LookPath("copilot")` ONLY when `options.use_path_cli == true` (returning an explicit error if `use_path_cli == true` and `"copilot"` is not found on `PATH`);
  4. Default `"", nil` so `copilot-sdk/go` uses its embedded runtime (`embeddedcli`).
- **Writable `COPILOT_HOME` & `BaseDirectory`**:
  - `EnsureCopilotHome()` sets `COPILOT_HOME` to `filepath.Join(os.TempDir(), "sofa-copilot")` when empty and ensures the directory exists (`0o700`), invoked both in `cmd/sofa-harness-copilot/main.go` and before `s.clientFactory` in `Server.OpenSession`.
  - Child runtime environment sets `COPILOT_HOME` to `req.GetSessionStoreDirectory()` when provided (unless `HarnessEnv["COPILOT_HOME"]` overrides it), or inherits the writable `COPILOT_HOME`, and `defaultClientFactory` passes `BaseDirectory` on `sdk.ClientOptions`.

**Independent Test**: Unit tests in `plugins/sofa-harness-copilot/internal/server/server_test.go` verify `OpenSession` invokes `clientFactory` with `cliPath == ""` by default, honors `options.cli_path` > `<execDir>/bin/copilot` > `use_path_cli` precedence without touching `PATH` by default, and guarantees a writable `COPILOT_HOME` on disk.

---

### User Story 2 - Self-Contained Linux Bundle via Official `cmd/bundler` (`plugins/sofa-harness-copilot/scripts/build-bundle.go`) (Priority: P1)

Building a Linux `sofa-harness-copilot` bundle with no `-copilotbin` or `-copilotsrc` overrides produces a single self-contained Linux executable (`<out>/sofa-harness-copilot/sofa-harness-copilot`) embedding both `glibc` and `musl` Copilot runtimes via `github.com/github/copilot-sdk/go/cmd/bundler` inside an isolated temporary staging copy, without writing `zcopilot_*` files into the git working tree.

- **Cache Support (`-cache-dir`)**: When `-cache-dir` is provided in default mode, checks `<cache-dir>/copilot-bundler-linux-<arch>/zcopilot_linux_<arch>.go` (matching `-cli-version` if specified). On cache hit, copies cached `zcopilot*` files into the staged `cmd/sofa-harness-copilot/` directory with zero network access; on cache miss, runs `go run github.com/github/copilot-sdk/go/cmd/bundler --platform linux/<arch> --output <stagedCmdDir> [--cli-version <ver>]` and saves generated `zcopilot*` files into `<cache-dir>/copilot-bundler-linux-<arch>/`.
- **Override Mode Preserved**: `-copilotbin` and `-copilotsrc` remain mutually exclusive override flags that populate `<out>/sofa-harness-copilot/bin/copilot`, keeping Stage 2 integration tests (`copilot_plugin_integration_test.go`) and offline stub tests 100% intact.

**Independent Test**: `build-bundle.go` invoked with `-cache-dir` pre-seeded with `zcopilot_linux_amd64.go` produces a single-binary `sofa-harness-copilot` bundle offline with zero `bin/copilot` directory and zero modifications to the working tree; Stage 2 `TestWorkerRunnerWithExternalCopilotPluginPodmanIntegration` passes unchanged using `-copilotsrc`.

---

### User Story 3 - Zero-Node/npm `scripts/install-local-plugins.go` (Priority: P1)

`scripts/install-local-plugins.go` requires zero Node.js or `npm` installation (`fetchNpmTarball`, `extractCopilotFromTgz`, and `npm`/`npm.cmd` fallback removed). In default mode (no `-copilot-linux-bin` or `-copilot-src`), it delegates directly to `build-bundle.go -out <stageParent> -arch <arch> -cache-dir <cacheDir>` and installs the single-binary `sofa-harness-copilot` into `<plugin-dir>/sofa-harness-copilot/linux_<arch>/`, verified by `internal/harness/installer.NewInstaller().Prepare(...)`.

**Independent Test**: Tier 1 test in `examples/examples_test.go` pre-seeds `<cacheDir>/copilot-bundler-linux-amd64/zcopilot_linux_amd64.go` in `t.TempDir()`, runs `go run ./scripts/install-local-plugins.go -only copilot -arch amd64 -plugin-dir <pluginDir> -cache-dir <cacheDir>` with neither `-copilot-src` nor `-copilot-linux-bin`, confirms `<pluginDir>/sofa-harness-copilot/linux_amd64/sofa-harness-copilot` exists without `bin/copilot`, and confirms `installer.NewInstaller().Prepare(...)` succeeds.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `plugins/sofa-harness-copilot/internal/server/server.go` `resolveCLIPath` MUST prioritize (1) explicit `options.cli_path`, (2) `<execDir>/bin/copilot` regular file, (3) `LookPath("copilot")` ONLY when `options.use_path_cli == true`, and otherwise return `"", nil` to delegate to `copilot-sdk/go`'s built-in `embeddedcli`.
- **FR-002**: `plugins/sofa-harness-copilot/cmd/sofa-harness-copilot/main.go` and `Server.OpenSession` MUST ensure `COPILOT_HOME` is non-empty (defaulting to `filepath.Join(os.TempDir(), "sofa-copilot")`) and created (`0o700`) in the plugin host process, and `defaultClientFactory` MUST propagate `BaseDirectory` on `sdk.ClientOptions` from the resolved child `COPILOT_HOME` (`HarnessEnv["COPILOT_HOME"]`, else `SessionStoreDirectory`, else host `COPILOT_HOME`).
- **FR-003**: `plugins/sofa-harness-copilot/go.mod` and `go.sum` MUST include `github.com/klauspost/compress` (`zstd`) and complete checksums aligned with root `go.mod`/`go.sum` so both `go test ./...` at repo root and `go test ./...` inside `plugins/sofa-harness-copilot` pass cleanly.
- **FR-004**: `plugins/sofa-harness-copilot/scripts/build-bundle.go` MUST support `-out`, `-arch`, `-cache-dir`, `-cli-version`, `-copilotbin`, and `-copilotsrc`; in default mode (neither `-copilotbin` nor `-copilotsrc`), it MUST stage an isolated temp copy of `plugins/sofa-harness-copilot` (rewriting `replace github.com/lonegunmanb/sofa => ../..` to the absolute repo root), restore or generate `zcopilot_*` via `github.com/github/copilot-sdk/go/cmd/bundler` + cache into `<cache-dir>/copilot-bundler-linux-<arch>/`, and build a single self-contained `sofa-harness-copilot` binary without writing `zcopilot_*` into the working tree.
- **FR-005**: `scripts/install-local-plugins.go` MUST remove `fetchNpmTarball`, `extractCopilotFromTgz`, and all `npm`/`npm.cmd` logic, delegate default Copilot builds to `build-bundle.go -cache-dir <cacheDir>`, and only require `<destDir>/bin/copilot` when `-copilot-linux-bin` or `-copilot-src` was explicitly supplied.
- **FR-006**: `examples/copilot-agy-self-host/README.md` MUST document that `install-local-plugins.go` uses `copilot-sdk/go`'s official `bundler` without requiring Node.js/`npm`.

## Success Criteria *(mandatory)*

- **SC-001**: `Push-Location plugins/sofa-harness-copilot; go test ./... -count=1; Pop-Location` passes 100%.
- **SC-002**: `gofmt -l .` returns empty and `go vet ./...` passes cleanly across the repository.
- **SC-003**: `go test ./... -count=1` passes all root packages including `./examples/...` fast and offline.
- **SC-004**: `go test -tags=integration -run TestWorkerRunnerWithExternalCopilotPluginPodmanIntegration ./internal/worker/runner/... -count=1 -v` (and/or Copilot plugin integration test) passes cleanly against real Podman.
- **SC-005**: `git status` shows no untracked `zcopilot_*` or `.sofa/` artifacts in the repository.
