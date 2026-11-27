# Feature Specification: Container `/.sofa/bundle` Extract-Root Mounting, Path Relocation, Lenient Skill Filtering, and Harness Parity (`037-container-skill-relocation-and-lenient-filtering`)

**Feature Branch**: `037-container-skill-relocation-and-lenient-filtering`

**Created**: 2026-11-27

**Status**: Approved

**Input**: `internal/worker/daemon/daemon.go`, `internal/sandbox/runtime/{spec.go,podman.go}`, `internal/tool/runtime/executor.go`, `internal/worker/runner/{types.go,worker_runner.go,skills.go}`, `plugins/sofa-harness-copilot/internal/server/server.go`, `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/backend.py`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Mount Full `<extractRoot>` at `/.sofa/bundle:ro` & Deterministic Bundle Path Relocation (Priority: P1)

In Slice 036, `WorkerDaemon` extracted `.sofaplan` into `<extractRoot>` (`config/`, `modules/`, `external_skills/`, `plugins.lock.json`) while temporarily passing `<extractRoot>/<BundlePath>` as `BundleDir`. That prevented cross-module `modules/<name>/...` and `external_skills/<hash>` resources from being mounted inside containers at `/.sofa/bundle`.

In Slice 037:
- `WorkerDaemon.runNodeTask` and `WorkerDaemon.runMergeTask` pass `bundleInfo.ExtractDir` (the full `<extractRoot>`, or `""` when `PlanBlobKey` is empty) as `BundleDir` while still strictly validating `req.BundlePath` (rejecting traversal, backslashes, drive prefixes, and absolute paths).
- `path.module` evaluation at Plan/Apply time remains unchanged (`"config"` / `"modules/<name>"`) so `file("${path.module}/...")` continues resolving against `extractDir`.
- At container execution boundaries:
  1. `skill_directories` (for both `worker` and `qc`, including `dynamic_worker` Apply-time evaluated slices) are relocated to container paths:
     - Already container-mounted paths (`/workspace`, `/workspace/...`, `/.sofa/bundle`, `/.sofa/bundle/...`, `/.sofa/imports`, `/.sofa/imports/...`, or matching a custom `Sandbox.Mounts` target prefix, or other clean `/...` container path) are preserved as cleaned container paths.
     - Bundle-relative prefixes (`config`, `config/...`, `modules/...`, `external_skills/...`) are relocated to `/.sofa/bundle/<cleaned>` (with legacy direct-caller compatibility if `BundleDir/<cleaned>` does not exist while `cleaned` starts with `config/` and `BundleDir/<trimmed>` exists).
     - Any other clean relative path (e.g. `skills/local`) is relocated to `/workspace/<cleaned>`.
  2. `external_tool.program` (in `toolruntime.Executor.Invoke`) and `gatekeeper.command` (in `PodmanEngine.RunGatekeepers`) relocate any clean relative slash argument starting with `config/` or `modules/` (no `..` or `\`) to `/.sofa/bundle/<cleaned>` whenever `<BundleDir>/<cleaned>` exists on host AND `<WorkspaceDir>/<cleaned>` does NOT exist on host.

**Independent Test**:
- `internal/worker/daemon/daemon_test.go` verifies `BundleDir == extractRoot` for root and module nodes as well as merge tasks, and rejects invalid `bundle_path`.
- `internal/sandbox/runtime/podman_test.go` verifies `RunGatekeepers` relocates `config/check.sh` -> `/.sofa/bundle/config/check.sh` when present in `BundleDir` and absent in `HostWorkspaceDir`, without shadowing real workspace `config/...` files.
- `internal/tool/runtime/executor_test.go` verifies `external_tool` with `program = ["sh", "config/tool.sh"]` relocates to `["sh", "/.sofa/bundle/config/tool.sh"]` when `BundleDir/config/tool.sh` exists.

---

### User Story 2 - Lenient (`宽松模式`) `(skill_directories, skills, disabled_skills)` Filtering & Per-Session Staging (Priority: P1)

`WorkerRunner` performs host-side skill discovery, `disabled_skills`-prioritized filtering, physical isolation staging, and lenient `skills` allowlist resolution for both Work and QC sessions:
- `sandboxruntime.SessionMountLayout` gains `SkillsDir string`, which `BuildRunArgs` mounts read-only at `/.sofa/skills:ro` when non-empty.
- **Disabled Priority**: Any skill whose YAML frontmatter `name:` (or immediate directory `<entry>`) is in `disabled_skills` is excluded from `skills` and excluded on disk even if also listed in `skills`.
- **Host Mapping & Discovery**: Container `skill_directories` are mapped to host paths (`/.sofa/bundle/...` -> `BundleDir`, `/workspace/...` -> `WorkspaceDir`, `/.sofa/imports/...` -> `ImportsDir`, custom `Sandbox.Mounts` target -> `m.Source`). Missing host directories or unmapped container paths are silently allowed (lenient mode, zero error).
- **Physical Isolation of Disabled Skills**: When an existing host `skill_dir` contains >=1 skill excluded by `disabled_skills`, `WorkerRunner` stages a clean symlink-free copy of ONLY the non-disabled skill subdirectories into `<stageRootDir>/dir-<i_zero_padded>/<entry>`, preserving executable `0755` bits (and `#!` shebang executability), sets `SkillsDir = <stageRootDir>`, and replaces `SkillDirectories[i]` with `"/.sofa/skills/dir-<i_zero_padded>"`.
- **Lenient `skills` Filtering**: When host roots are present, `requestedSkills` keeps only non-disabled skills discovered across `skill_directories` (matching frontmatter `name` or directory `<entry>`, normalized to effective `name`, deduplicated in order). Missing skill names are silently skipped without error.
- Work attempts and QC rounds use distinct per-session stage roots under `execRoot` (`work-skills-r<n>`, `qc-skills-r<round>`).

**Independent Test**:
- `internal/sandbox/runtime/podman_test.go` verifies `SkillsDir` mounts as `/.sofa/skills:ro`.
- `internal/worker/runner/runner_test.go` verifies path relocation for `config/skills`, `modules/sub/skills`, `external_skills/abcd1234`, and `/workspace/.agents/skills` across Work and QC sessions, verifies `disabled_skills` overrides `skills`, stages filtered `/.sofa/skills/...` preserving `0755` on `scripts/run.sh`, and strips disabled + non-existent skill names from `SessionConfig.Skills` without error.

---

### User Story 3 - Dual-Harness Plugin Parity (`copilot` + `antigravity`) & Real Podman Integration (Priority: P1)

Both official external harness plugins enforce consistent `disabled_skills` priority and lenient skill behavior:
- **`plugins/sofa-harness-copilot`**: `OpenSession` defensively strips any entry present in `DisabledSkills` from `Skills` (trimming & deduplicating), and only sets `CustomAgents` / `Agent` when the resulting filtered `skills` slice is non-empty.
- **`plugins/sofa-harness-antigravity`**: Keeping `skills_paths` (never combining `inline_skills` and `skills_paths` due to `LocalHarness` restriction), `GoogleAntigravityBackend.build_agent_config` (and `ScriptedFakeSession`):
  1. Parses `SKILL.md` YAML frontmatter `name:` (`_parse_skill_frontmatter_name`).
  2. When `disabled_skills` matches >=1 skill in an existing `skill_directories` directory, copies only enabled skill subdirectories (skipping symlinks, preserving executable modes) to `<session_store_directory or mkdtemp()>/.sofa-filtered-skills/dir-<idx>` and substitutes that path in `effective_skills_paths`.
  3. Leniently loads surviving non-disabled `params.skills` discovered across existing `effective_skills_paths` (silently skipping missing names) and appends `Active Skill (<name>) [directory: <skill_subdir_path>]:\n<SKILL.md content>` sections to `system_instructions` while preserving `params.system_prompt` first and keeping `skills_paths=effective_skills_paths`.
- **Tier 2 Real Podman Integration**: A real Podman integration test verifies `<extractRoot>` mounted at `/.sofa/bundle:ro`, relocated `config/skills` + `external_skills/<hash>` + `disabled_skills` filtered staging at `/.sofa/skills/...` with executable `+x` `scripts/helper.sh` and absent disabled skill dir, and bundle `gatekeeper.sh` execution inside Podman.

**Independent Test**:
- `plugins/sofa-harness-copilot` Go unit tests pass (`go test ./... -count=1`).
- `plugins/sofa-harness-antigravity` pytest unit tests pass.
- `go test -tags=integration ./internal/worker/runner/... ./internal/sandbox/runtime/... -count=1` passes against real Podman.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `WorkerDaemon` (`internal/worker/daemon/daemon.go`) MUST validate `req.BundlePath` and pass `bundleInfo.ExtractDir` (full `<extractRoot>`, or `""` when `PlanBlobKey` is empty) as `BundleDir` on `runner.NodeExecutionRequest` and `runner.MergeCandidateRequest`.
- **FR-002**: `sandboxruntime.RelocateBundleCommandArgs` (`internal/sandbox/runtime/spec.go`) MUST relocate clean `config/...` and `modules/...` arguments in `PodmanEngine.RunGatekeepers` and `toolruntime.Executor.Invoke` (case `"external"`) to `"/.sofa/bundle/" + cleaned` only when `<BundleDir>/<cleaned>` exists on host and `<WorkspaceDir>/<cleaned>` does not.
- **FR-003**: `sandboxruntime.SessionMountLayout` MUST support `SkillsDir string`, mounted read-only at `/.sofa/skills:ro` by `BuildRunArgs` when non-empty.
- **FR-004**: `internal/worker/runner/skills.go` MUST relocate `skill_directories` (including legacy `config/` compatibility), discover skills via `SKILL.md` frontmatter `name:` or directory name, enforce `disabled_skills` priority over `skills`, stage symlink-free filtered copies into `/.sofa/skills/dir-<i_zero_padded>` preserving `0755` executable bits / `#!` shebang executability when any skill in a directory is disabled, and leniently filter `skills` to discovered enabled skills without erroring on missing dirs or missing skill names. Both Work and QC sessions MUST wire this into `MountLayout` and `SessionConfig`.
- **FR-005**: `plugins/sofa-harness-copilot/internal/server/server.go` MUST defensively strip `DisabledSkills` from `Skills` and omit `CustomAgents`/`Agent` when the filtered list is empty.
- **FR-006**: `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/backend.py` MUST enforce `disabled_skills` disk filtering into `.sofa-filtered-skills/dir-<idx>` and lenient eager `SKILL.md` injection into `system_instructions` while keeping `skills_paths` (never `inline_skills`) and preserving file executable modes.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` is empty in both repo root and `plugins/sofa-harness-copilot`.
- **SC-002**: `go vet ./...` passes in both repo root and `plugins/sofa-harness-copilot`.
- **SC-003**: `go test ./... -count=1` passes 100% in both repo root and `plugins/sofa-harness-copilot`.
- **SC-004**: `plugins\sofa-harness-antigravity\.venv\Scripts\pytest.exe plugins\sofa-harness-antigravity\tests` passes 100%.
- **SC-005**: `go test -tags=integration ./internal/worker/runner/... ./internal/sandbox/runtime/... -count=1` passes 100%.
- **SC-006**: `git status -sb` shows only Slice `037` files modified/untracked, uncommitted.
