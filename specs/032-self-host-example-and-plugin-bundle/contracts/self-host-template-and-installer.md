# Contract: Self-Hosting Template & Local Linux Dual-Plugin Installer (`032`)

## 1. `scripts/install-local-plugins.go` Contract

1. Must compile and run from repository root via `go run ./scripts/install-local-plugins.go [flags]` with `//go:build ignore`.
2. **Arch Resolution**:
   - If `-arch` is provided, must equal `"amd64"` or `"arm64"`.
   - If `-arch` is omitted/empty, calls `sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{RuntimeBin: runtimeBin}).DetectPlatform(ctx)` (bounded timeout). If detection succeeds and OS is `"linux"`, uses `platform.Arch`; otherwise falls back to `runtime.GOARCH` if `"amd64"` or `"arm64"`, else `"amd64"`.
3. **Copilot Build & Cache Resolution**:
   - Rejects mutually exclusive `-copilot-linux-bin` + `-copilot-src`.
   - When `-copilot-src` is set:
     - If `-copilot-src` resolves to a filesystem directory or `.go` file outside `plugins/sofa-harness-copilot` (e.g., a test stub in `t.TempDir()` or `./internal/...`), cross-compiles it (`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`) to a temporary `copilot` binary and passes `-copilotbin` to `build-bundle.go`; otherwise passes `-copilotsrc <pkg>` directly to `build-bundle.go`.
   - When `-copilot-src` is empty:
     - Uses `-copilot-linux-bin` if set (verifying it is an existing regular file);
     - Else uses `<cache-dir>/copilot-linux-<arch>/copilot` if present and non-empty;
     - Else downloads npm package `@github/copilot-linux-x64` (`amd64`) or `@github/copilot-linux-arm64` (`arm64`) using `npm.cmd pack` (Windows) / `npm pack` (Unix), or `<runtime-bin> run --rm -v <tmp>:/out -w /out docker.io/library/node:22-alpine npm pack <pkg>` if `npm` is missing on host; extracts `package/copilot` from the `.tgz` using Go stdlib `compress/gzip` + `archive/tar` into `<cache-dir>/copilot-linux-<arch>/copilot` (`0755`).
   - Invokes `go run -mod=readonly ./scripts/build-bundle.go -out <stageParent> -arch <arch> ...` with `Dir = <repoRoot>/plugins/sofa-harness-copilot`. Must NEVER modify `plugins/sofa-harness-copilot/go.mod` or `go.sum`.
4. **Antigravity Build**:
   - Invokes `uv run python scripts/build_linux_bundle.py --out <stageParent> --arch <arch>` (plus `--skip-sdk` if `-skip-antigravity-sdk`) with `Dir = <repoRoot>/plugins/sofa-harness-antigravity`, falling back to `python3` / `python` if `uv` is not found on `PATH`.
5. **Atomic Install & Verification**:
   - Stages inside `<plugin-dir>/<exeName>/.stage-*` (same volume as destination `<plugin-dir>/<exeName>/linux_<arch>`) so `os.Rename` swaps atomically with backup/restore and Windows long-path (`\\?\`) safe copy/remove fallback.
   - Verifies `<plugin-dir>/<exeName>/linux_<arch>/<exeName>` via `os.Lstat` is a regular file (not a symlink or directory).
   - Must match `internal/harness/installer.NewInstaller().Prepare(ctx, HarnessConfig{Name: ...}, Platform{OS: "linux", Arch: arch}, ResolveOptions{PluginDir: pluginDir})` 1:1.

## 2. `examples/copilot-agy-self-host/` Contract

1. Must pass `project.Init`, `config.LoadDirectoryContext`, `planner.NewSofaConfig`, `Init`, `RunPlan`, `ExportPlan`, `plan.SaveArchive`, and `plan.LoadArchive`.
2. `worker.implement_slice` evaluated via `applyeval.BuildEvalContext` + `applyeval.EvalReadyWorker` must resolve:
   - `Sandbox` -> `sandbox.go_dev`
   - `Harness` -> `harness.copilot`
   - `ModelProvider` -> null
   - `HumanApproval` -> `"required"` (default)
   - `Artifacts["summary"]` -> `required = true`, `non_empty = true`, `path = "summary.md"`
   - `Gatekeepers` -> `go_quality` with `RequireCleanTree == true` and command executing `gofmt`, `go vet ./...`, and `go test ./... -count=1`
   - `QC` -> `Sandbox = sandbox.qc_audit`, `Harness = harness.antigravity`, `ModelProvider = null` (`ModelProviderSet = true`), non-empty `Criteria`
3. `config.ExtractPlanEnvRefs(plan)` must equal `[]string{"COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "GITHUB_TOKEN"}`.
