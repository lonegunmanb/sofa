# Tasks: `copilot + antigravity` Self-Hosting Template & Local Linux Dual-Plugin Installer (`032-self-host-example-and-plugin-bundle`)

## Phase 1: Pre-Task Investigation & Spec Kit Alignment
- [X] T001 Inspect `docs/stage3/032-self-host-example-and-plugin-bundle.md`, `docs/hcl-contract.md`, `internal/source/schema.go`, `internal/sandbox/spec/schema.go`, `internal/harness/spec/schema.go`, `internal/worker/spec/{schema,deferred,gatekeeper,qc,artifact}.go`, `internal/actor/config/plan_refs.go`, `internal/harness/installer/installer.go`, `plugins/sofa-harness-copilot/scripts/build-bundle.go`, `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py`, and `scripts/build-webui.go`
- [X] T002 Create `specs/032-self-host-example-and-plugin-bundle/{spec,plan,data-model,tasks}.md` and `contracts/self-host-template-and-installer.md`

## Phase 2: Tier 1 Anti-Rot Tests (Red First)
- [X] T003 Write failing Tier 1 tests in `examples/examples_test.go`:
  - `TestCopilotAgySelfHostExampleCompilesAndRoundTripsPlan`: compiles `examples/copilot-agy-self-host` via `project.Init` + `planner.NewSofaConfig` + `RunPlan` + `ExportPlan` + `plan.SaveArchive` + `plan.LoadArchive`, evaluates `worker.implement_slice` via `applyeval.BuildEvalContext` + `applyeval.EvalReadyWorker`, asserts `sandbox.go_dev`, `harness.copilot`, `gatekeeper "go_quality"` (`require_clean_worktree = true`, `gofmt`/`go vet`/`go test`), `qc` overrides (`sandbox.qc_audit`, `harness.antigravity`, `model_provider = null`), `human_approval = "required"`, `artifact "summary"` (`required = true`, `non_empty = true`), and `config.ExtractPlanEnvRefs` returning `["COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "GITHUB_TOKEN"]`
  - `TestInstallLocalPluginsOutputLayoutMatchesHarnessInstaller`: runs `go run ./scripts/install-local-plugins.go -only copilot -arch amd64 -plugin-dir <tmp> -copilot-src <stubDir>` against a temp Go stub + stages `antigravity` layout fixture, and verifies `installer.NewInstaller().Prepare(...)` succeeds for both `copilot` and `antigravity` on `linux/amd64` with `/.sofa/harness/sofa-harness-{copilot,antigravity}` and `bin/copilot`
- [X] T004 Run `go test ./examples/... -v` to confirm Red failure before implementation

## Phase 3: Implementation (Green)
- [X] T005 Implement `scripts/install-local-plugins.go` (`//go:build ignore`) supporting `-plugin-dir`, `-arch`, `-runtime-bin`, `-copilot-linux-bin`, `-copilot-src`, `-cache-dir`, `-skip-antigravity-sdk`, and `-only`, Podman platform detection, npm/cache `.tgz` extraction, `-mod=readonly` `build-bundle.go` invocation, `build_linux_bundle.py` invocation, same-volume atomic install with Windows long-path support, and regular-file verification
- [X] T006 Author `examples/copilot-agy-self-host/variables.sofa.hcl`, `main.sofa.hcl`, `outputs.sofa.hcl`, and `README.md`
- [X] T007 Run `go test ./examples/... -count=1 -v` and confirm both Tier 1 tests turn Green

## Phase 4: Verification Gates
- [X] T008 Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `npm.cmd --prefix web test -- --run`
- [X] T009 Verify `git status` shows no modifications to `plugins/sofa-harness-copilot/go.mod` / `go.sum` and no leftover temp or `.sofa/` directories
