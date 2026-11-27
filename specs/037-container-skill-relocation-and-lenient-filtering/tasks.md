# Tasks: Container `/.sofa/bundle` Extract-Root Mounting, Path Relocation, Lenient Skill Filtering, and Harness Parity (`037-container-skill-relocation-and-lenient-filtering`)

**Input**: Design documents from `/specs/037-container-skill-relocation-and-lenient-filtering/`
**Prerequisites**: `spec.md`, `plan.md`

## Phase 1: Tests First (Red Phase — MUST fail before implementation)

- [x] T001 [US1] Update `TestWorkerDaemon_BundleSingleflightReuseSHAMismatchAndMerge` in `internal/worker/daemon/daemon_test.go` to expect `BundleDir == extractRoot` and add an invalid `bundle_path` rejection assertion.
- [x] T002 [US1][US2] Add failing unit tests in `internal/sandbox/runtime/podman_test.go` for `SkillsDir` mounting at `/.sofa/skills:ro` and `RunGatekeepers` relocating `config/check.sh` -> `/.sofa/bundle/config/check.sh` (when present in `BundleDir` and absent in `HostWorkspaceDir`) without shadowing real workspace `config/...` files.
- [x] T003 [US1] Add failing unit test in `internal/tool/runtime/executor_test.go` verifying `external_tool` with `program = ["sh", "config/tool.sh"]` relocates to `["sh", "/.sofa/bundle/config/tool.sh"]` when `BundleDir/config/tool.sh` exists (and preserves workspace files when present).
- [x] T004 [US1][US2] Add failing unit tests in `internal/worker/runner/runner_test.go` for `config/skills`, `modules/sub/skills`, `external_skills/abcd1234`, and `/workspace/.agents/skills` relocation on Work & QC sessions, `disabled_skills` overriding `skills`, staging filtered `/.sofa/skills/...` preserving `0755` on `scripts/run.sh`, and silently skipping missing skill names/directories.
- [x] T005 [US3] Add failing unit tests in `plugins/sofa-harness-copilot/internal/server/server_test.go` verifying `DisabledSkills` strips matching entries from `CustomAgents[0].Skills` and omits `CustomAgents`/`Agent` when none remain.
- [x] T006 [US3] Add failing unit tests in `plugins/sofa-harness-antigravity/tests/test_servicer.py` verifying eager `SKILL.md` injection into `system_instructions`, `disabled_skills` override + `.sofa-filtered-skills` staging, silent skipping of missing `skills`, and executable mode preservation on `scripts/helper.sh`.
- [x] T007 Run Go and Python test suites to confirm Red-phase failures.

## Phase 2: Implementation (Green Phase)

- [x] T008 [US1] Update `internal/worker/daemon/daemon.go` (`runNodeTask` and `runMergeTask`) to validate `req.BundlePath` and pass `bundleInfo.ExtractDir` as `BundleDir`.
- [x] T009 [US1][US2] Update `internal/sandbox/runtime/spec.go`, `internal/sandbox/runtime/podman.go`, and `internal/tool/runtime/executor.go` to add `SessionMountLayout.SkillsDir` (`/.sofa/skills:ro`) and shared `RelocateBundleCommandArgs` for `RunGatekeepers` and `external_tool`.
- [x] T010 [US1][US2] Implement `internal/worker/runner/skills.go` and wire it into Work and QC sessions in `internal/worker/runner/worker_runner.go`.
- [x] T011 [US3] Update `plugins/sofa-harness-copilot/internal/server/server.go` for defensive `disabled_skills` priority filtering over `skills`.
- [x] T012 [US3] Update `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/backend.py` for `_parse_skill_frontmatter_name`, `.sofa-filtered-skills` staging, and lenient eager `SKILL.md` injection into `system_instructions`.

## Phase 3: Tier 2 Integration & Full Verification

- [x] T013 [US1][US2][US3] Extend `internal/worker/runner/testdata/scriptedplugin/main.go` and `internal/worker/runner/runner_integration_test.go` with a real Podman integration test covering `/.sofa/bundle:ro` extract-root mounting, `config/skills` + `external_skills/<hash>` relocation, `disabled_skills` filtered staging at `/.sofa/skills/...` with executable `scripts/helper.sh`, and bundle `gatekeeper.sh` execution.
- [x] T014 Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1` (root and `plugins/sofa-harness-copilot`), `pytest`, and `go test -tags=integration ./internal/worker/runner/... ./internal/sandbox/runtime/... -count=1` and verify `git status -sb` has only Slice `037` changes uncommitted.
