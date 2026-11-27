# Tasks: Adopt Official `copilot-sdk/go` `embeddedcli` & `cmd/bundler` (`033-copilot-sdk-bundler`)

## Phase 1: Pre-Task Investigation & Spec Kit Artifacts
- [X] T001 Inspect `plugins/sofa-harness-copilot/{go.mod,cmd/sofa-harness-copilot/main.go,internal/server/sdk_iface.go,internal/server/server.go,internal/server/server_test.go,scripts/build-bundle.go}`, `internal/worker/runner/copilot_plugin_integration_test.go`, `scripts/install-local-plugins.go`, `examples/examples_test.go`, `examples/copilot-agy-self-host/README.md`, `internal/sandbox/runtime/spec.go`, and `copilot-sdk/go@v1.0.19` (`internal/embeddedcli/embeddedcli.go`, `cmd/bundler/main.go`, `client.go`)
- [X] T002 Author `specs/033-copilot-sdk-bundler/{spec,plan,data-model,tasks}.md` and `contracts/copilot-bundler.md`

## Phase 2: Dependency Alignment & Red Tests First
- [X] T003 Align `plugins/sofa-harness-copilot/go.mod` and `go.sum` with `github.com/klauspost/compress/zstd` and complete checksums so `go test ./...` inside `plugins/sofa-harness-copilot` runs cleanly
- [X] T004 Write failing unit tests in `plugins/sofa-harness-copilot/internal/server/server_test.go` covering:
  - Default `OpenSession` with neither `options.cli_path` nor `<execDir>/bin/copilot` passes `cliPath == ""` to `clientFactory` (ignoring `LookPath` unless `options.use_path_cli == true`)
  - Override precedence: `options.cli_path` > `<execDir>/bin/copilot` > `options.use_path_cli == true` > `""`
  - Writable `COPILOT_HOME` creation when unset and `SessionStoreDirectory` propagation into child `COPILOT_HOME` without mutating host `COPILOT_HOME`
- [X] T005 Write failing Tier 1 test in `examples/examples_test.go` pre-seeding `<cacheDir>/copilot-bundler-linux-amd64/zcopilot_linux_amd64.go` and running `go run ./scripts/install-local-plugins.go -only copilot -arch amd64 -plugin-dir <pluginDir> -cache-dir <cacheDir>` without `-copilot-src` or `-copilot-linux-bin`, asserting single-binary `<pluginDir>/sofa-harness-copilot/linux_amd64/sofa-harness-copilot` with NO `bin/copilot` and passing `installer.NewInstaller().Prepare(...)`
- [X] T006 Run tests in `plugins/sofa-harness-copilot` and `examples/` to confirm expected Red failures before implementation

## Phase 3: Implementation (Green)
- [X] T007 Update `plugins/sofa-harness-copilot/internal/server/{server.go,sdk_iface.go}` and `cmd/sofa-harness-copilot/main.go` for `resolveCLIPath`, `EnsureCopilotHome()`, child `COPILOT_HOME`, and `BaseDirectory`
- [X] T008 Upgrade `plugins/sofa-harness-copilot/scripts/build-bundle.go` to support `-cache-dir`, `-cli-version`, and isolated temp-staged `github.com/github/copilot-sdk/go/cmd/bundler` default mode while preserving `-copilotbin` / `-copilotsrc`
- [X] T009 Simplify `scripts/install-local-plugins.go` (remove `npm` / `.tgz` extraction, delegate default mode to `build-bundle.go -cache-dir`, verify `bin/copilot` only on overrides) and update `examples/copilot-agy-self-host/README.md`
- [X] T010 Confirm all tests in `plugins/sofa-harness-copilot` and `examples/` turn Green

## Phase 4: Verification Gates
- [X] T011 Run `Push-Location plugins/sofa-harness-copilot; go test ./... -count=1; Pop-Location`
- [X] T012 Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`
- [X] T013 Run `go test -tags=integration -run TestWorkerRunnerWithExternalCopilotPluginPodmanIntegration ./internal/worker/runner/... -count=1 -v`
- [X] T014 Verify `git status` is clean of untracked `zcopilot_*` or `.sofa/` artifacts and mark all tasks `[X]`
