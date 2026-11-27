# Contract: `sofa-harness-copilot` `embeddedcli` & `cmd/bundler` Packaging (`033`)

## 1. `plugins/sofa-harness-copilot` Server Contract

1. `resolveCLIPath(optionsJSON []byte) (string, error)` MUST resolve in exact order:
   - `options.cli_path` (trimmed non-empty)
   - `<execDir>/bin/copilot` (regular non-directory file)
   - `s.lookPath("copilot")` ONLY when `options.use_path_cli == true` (returning error if `use_path_cli == true` and not found)
   - `"", nil` otherwise (never failing on missing external binary, never falling back to `PATH` unrequested).
2. `EnsureCopilotHome()` MUST be invoked in `cmd/sofa-harness-copilot/main.go` and `Server.OpenSession` before `s.clientFactory`:
   - If `os.Getenv("COPILOT_HOME")` is empty, sets `COPILOT_HOME = filepath.Join(os.TempDir(), "sofa-copilot")`.
   - Calls `os.MkdirAll(copilotHome, 0o700)`.
3. `OpenSession` MUST propagate `COPILOT_HOME` into `childEnv` (preferring `HarnessEnv["COPILOT_HOME"]`, then `SessionStoreDirectory`, then host `COPILOT_HOME`) without mutating process `COPILOT_HOME` to `SessionStoreDirectory`, and `defaultClientFactory` MUST set `sdk.ClientOptions.BaseDirectory` from `COPILOT_HOME` in `env`.

## 2. `plugins/sofa-harness-copilot/scripts/build-bundle.go` Contract

1. When `-copilotbin` and `-copilotsrc` are both empty (default mode):
   - Must NEVER create or modify `zcopilot_*` files inside the repository working tree.
   - Stages an isolated temporary copy of `plugins/sofa-harness-copilot` with `replace github.com/lonegunmanb/sofa => <absRepoRoot>`.
   - If `-cache-dir` is set and `<cache-dir>/copilot-bundler-linux-<arch>/zcopilot_linux_<arch>.go` exists as a non-empty regular file (and matches `-cli-version` when `-cli-version` is non-empty), copies all `zcopilot*` regular files from that cache dir into `<stageDir>/cmd/sofa-harness-copilot/`.
   - Otherwise runs `go run github.com/github/copilot-sdk/go/cmd/bundler --platform linux/<arch> --output <stagedCmdDir> [--cli-version <ver>]` inside `<stageDir>` (with `-mod`/`-modfile` stripped from `GOFLAGS`), and if `-cache-dir` is set, saves generated `zcopilot*` files into `<cache-dir>/copilot-bundler-linux-<arch>/`.
   - Compiles `./cmd/sofa-harness-copilot` with `CGO_ENABLED=0 GOOS=linux GOARCH=<arch>` to `<out>/sofa-harness-copilot/sofa-harness-copilot` (`0755`) with no `bin/copilot` file.
2. When `-copilotbin` or `-copilotsrc` is set (mutually exclusive):
   - Preserves Stage 2 behavior producing `<out>/sofa-harness-copilot/{sofa-harness-copilot, bin/copilot}`.

## 3. `scripts/install-local-plugins.go` Contract

1. Must contain zero `npm`, `npm.cmd`, `fetchNpmTarball`, or `extractCopilotFromTgz` logic.
2. In default Copilot mode (neither `-copilot-linux-bin` nor `-copilot-src`), invokes `build-bundle.go -out <stageParent> -arch <arch> -cache-dir <cacheDir>` and verifies `<destDir>/sofa-harness-copilot` only (not `bin/copilot`).
3. In override mode (`-copilot-linux-bin` or `-copilot-src`), verifies both `<destDir>/sofa-harness-copilot` and `<destDir>/bin/copilot`.
4. Installed `<plugin-dir>/sofa-harness-copilot/linux_<arch>/` in default single-binary mode MUST pass `internal/harness/installer.NewInstaller().Prepare(...)`.
