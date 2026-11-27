# Feature Specification: `copilot + antigravity` Self-Hosting Template & Local Linux Dual-Plugin Installer (`032-self-host-example-and-plugin-bundle`)

**Feature Branch**: `032-self-host-example-and-plugin-bundle`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `032` blueprint (`docs/stage3/032-self-host-example-and-plugin-bundle.md`), `docs/stage3/README.md`, `docs/hcl-contract.md`, `internal/source/schema.go`, `internal/sandbox/spec/schema.go`, `internal/harness/spec/schema.go`, `internal/worker/spec/{schema,deferred,gatekeeper,qc,artifact}.go`, `internal/harness/installer/installer.go`, `internal/actor/config/plan_refs.go`, `plugins/sofa-harness-copilot/scripts/build-bundle.go`, and `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - One-Command Local Linux Dual-Plugin Builder & Installer (`scripts/install-local-plugins.go`) (Priority: P1)

Developers cloning `sofa` run a single command (`go run ./scripts/install-local-plugins.go`) from the repository root to cross-build and atomically install both `sofa-harness-copilot` and `sofa-harness-antigravity` Linux container bundles into `<plugin-dir>/sofa-harness-<name>/linux_<arch>/` so `sofa server --dev` and `internal/harness/installer.Installer.Prepare(...)` discover them immediately in local plugin mode.

- **Platform Detection & Overrides**: Accepts `-arch <amd64|arm64>` (auto-detected via `sandboxruntime.NewPodmanEngine(PodmanEngineOptions{RuntimeBin: *runtimeBin}).DetectPlatform(ctx)` when omitted, falling back to `runtime.GOARCH` if `amd64`/`arm64`, else `amd64`), `-runtime-bin` (default `"podman"`), `-plugin-dir` (default `~/.sofa/plugins`), `-cache-dir` (default `.sofa/cache`), `-only <copilot|antigravity>`, `-copilot-linux-bin`, `-copilot-src`, and `-skip-antigravity-sdk`.
- **Copilot Bundle**: Resolves the Linux `@github/copilot` CLI binary via (1) `-copilot-linux-bin`, (2) cached `<cache-dir>/copilot-linux-<arch>/copilot`, or (3) `npm.cmd pack` / `npm pack` (with container `podman run --rm` fallback when host `npm` is absent) extracting `package/copilot` from `.tgz` using Go stdlib `compress/gzip` + `archive/tar` and caching with mode `0755`, unless `-copilot-src` is supplied for deterministic offline cross-compilation. Invokes `go run -mod=readonly ./scripts/build-bundle.go ...` inside `plugins/sofa-harness-copilot` without modifying `go.mod` or `go.sum`.
- **Antigravity Bundle**: Invokes `uv run python scripts/build_linux_bundle.py --out <tmpStage> --arch <arch>` (adding `--skip-sdk` when `-skip-antigravity-sdk` is set; falling back to `python3`/`python` if `uv` is unavailable) inside `plugins/sofa-harness-antigravity`.
- **Atomic Symlink-Free Installation**: Stages on the target volume and swaps atomically into `<plugin-dir>/sofa-harness-<name>/linux_<arch>/` (handling Windows deep paths safely), then verifies via `os.Lstat` that both entrypoints are regular non-symlink files.

**Independent Test**: Tier 1 test in `examples/examples_test.go` runs `go run ./scripts/install-local-plugins.go -only copilot -copilot-src ... -arch amd64 -plugin-dir <tmp>` (plus verifies `antigravity` layout compatibility) and asserts `installer.NewInstaller().Prepare(...)` succeeds for both `copilot` and `antigravity` on `linux/amd64` with expected `ContainerPath` and `bin/copilot`.

---

### User Story 2 - `copilot + antigravity` Self-Hosting Dogfooding Template (`examples/copilot-agy-self-host/`) (Priority: P1)

Developers launch a full heterogeneous self-hosting workflow on `sofa` where `harness.copilot` implements a slice inside `sandbox.go_dev` under `writable_paths` and `human_approval`, deterministic `gatekeeper "go_quality"` enforces `gofmt`, `go vet`, `go test`, and `require_clean_worktree = true`, and an independent read-only container `sandbox.qc_audit` running `harness.antigravity` with `model_provider = null` audits the result against `qc.criteria`.

- **Explicit Multi-Instance Disambiguation**: Because two `sandbox` blocks (`go_dev`, `qc_audit`) and two `harness` blocks (`copilot`, `antigravity`) exist, `worker "implement_slice"` explicitly sets `sandbox = sandbox.go_dev` and `harness = harness.copilot`, while `qc` explicitly sets `sandbox = sandbox.qc_audit`, `harness = harness.antigravity`, `model_provider = null`, `model = var.qc_model`, and `criteria = { ... }`.
- **Variables & Outputs**: `variables.sofa.hcl` declares `repo_url` (required, non-empty validation), `base_branch` (`"main"`), `sandbox_image` (`"golang:1.27"`), `work_model`, `qc_model`, `slice_prompt` (required, non-empty validation), `writable_paths`, and `human_approval` (`"required"` with validation `"required"` or `"optional"`). `outputs.sofa.hcl` exposes `work_branch` and `commit_sha`.
- **Credential Refs**: `source "repo"` uses `token_ref = "GITHUB_TOKEN"`, `harness "copilot"` uses `env_refs = { GITHUB_TOKEN = "COPILOT_GITHUB_TOKEN" }`, and `harness "antigravity"` uses `env_refs = { GEMINI_API_KEY = "GEMINI_API_KEY" }`.
- **Quickstart Documentation**: `examples/copilot-agy-self-host/README.md` documents local dual-plugin installation, `sofa server --dev`, `sofa secret set --from-env GITHUB_TOKEN COPILOT_GITHUB_TOKEN GEMINI_API_KEY`, `sofa apply`, and dashboard / `sofa decisions respond --approve` approval.

**Independent Test**: Tier 1 anti-rot test in `examples/examples_test.go` compiles `examples/copilot-agy-self-host` via `project.Init` + `planner.NewSofaConfig` + `RunPlan` + `ExportPlan` + `plan.SaveArchive` + `plan.LoadArchive`, evaluates `worker.implement_slice` via `applyeval.BuildEvalContext` + `applyeval.EvalReadyWorker`, and asserts every config invariant and `config.ExtractPlanEnvRefs(loaded.Plan) == []string{"COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "GITHUB_TOKEN"}`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `scripts/install-local-plugins.go` (`//go:build ignore`) MUST support `-plugin-dir`, `-arch`, `-runtime-bin`, `-copilot-linux-bin`, `-copilot-src`, `-cache-dir`, `-skip-antigravity-sdk`, and `-only`, and exit non-zero on invalid flags or failed builds.
- **FR-002**: `scripts/install-local-plugins.go` MUST auto-detect target Linux arch via `sandboxruntime.NewPodmanEngine(...).DetectPlatform(ctx)` when `-arch` is empty, falling back cleanly to `amd64`/`arm64` host arch or `"amd64"`.
- **FR-003**: `scripts/install-local-plugins.go` MUST resolve Linux `copilot` from `-copilot-linux-bin`, `<cache-dir>/copilot-linux-<arch>/copilot`, or `npm.cmd`/`npm` (or container `podman run --rm` fallback) `pack` extraction (`compress/gzip` + `archive/tar`), unless `-copilot-src` is specified, and MUST run `go run -mod=readonly ./scripts/build-bundle.go` in `plugins/sofa-harness-copilot` without modifying `go.mod`/`go.sum`.
- **FR-004**: `scripts/install-local-plugins.go` MUST build `sofa-harness-antigravity` via `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py` (passing `--skip-sdk` when `-skip-antigravity-sdk` is set), install both bundles atomically into `<plugin-dir>/sofa-harness-<name>/linux_<arch>/`, and verify regular non-symlink entrypoints via `os.Lstat`.
- **FR-005**: `examples/copilot-agy-self-host/{variables,main,outputs}.sofa.hcl` MUST conform 100% to `internal/source`, `internal/sandbox/spec`, `internal/harness/spec`, and `internal/worker/spec` schemas (including `require_clean_worktree = true` on `gatekeeper "go_quality"` and `criteria` on `qc`) and pass `project.Init`, `planner.NewSofaConfig`, `RunPlan`, `ExportPlan`, `.sofaplan` archive round-trip, and `applyeval.EvalReadyWorker`.
- **FR-006**: `examples/examples_test.go` MUST enforce both the template plan/apply/env-ref round-trip test and the `install-local-plugins.go` → `installer.NewInstaller().Prepare(...)` integration contract test offline with zero network access.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` returns empty and `go vet ./...` passes cleanly.
- **SC-002**: `go test ./... -count=1` passes all packages including `./examples/...` fast and offline.
- **SC-003**: `npm.cmd --prefix web test -- --run` passes all frontend tests.
- **SC-004**: `plugins/sofa-harness-copilot/go.mod` and `go.sum` remain unmodified and no untracked `.sofa/` or build temp artifacts remain in the repository.
