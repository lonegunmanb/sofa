# Tasks: `020-plugin-antigravity`

**Input**: Design documents from `specs/020-plugin-antigravity/`

## Phase 1: Python Project & Git Attributes Setup

- [X] T001 Configure `.gitattributes` (`plugins/sofa-harness-antigravity/bin/* text eol=lf`, `*.sh text eol=lf`), `plugins/sofa-harness-antigravity/.gitignore`, `plugins/sofa-harness-antigravity/pyproject.toml`, `plugins/sofa-harness-antigravity/bin/sofa-harness-antigravity` (pure LF, `0755`), and `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/__init__.py` (`proto/` path setup).

## Phase 2: Handshake & Servicer Offline Unit Tests (TDD Red First)

- [X] T002 [Red] Author `plugins/sofa-harness-antigravity/tests/test_handshake.py` covering `SOFA_HARNESS_PLUGIN` validation, `PLUGIN_CLIENT_CERT` requirement, immediate `PLUGIN_*` / `SOFA_HARNESS_PLUGIN` scrubbing from `os.environ`, single-line handshake format with unpadded DER base64 cert, `Health("plugin") == SERVING`, mTLS verification with P-256 and P-521 client cert/key, and rogue mTLS rejection.
- [X] T003 [Red] Author `plugins/sofa-harness-antigravity/tests/test_servicer.py` covering all 7 RPCs via `ScriptedFakeBackend` (and testing `GoogleAntigravityBackend` config/env mapping): `GetCapabilities`, `OpenSession` (`openai` provider -> `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY`, `resume_session_id` missing -> `ERROR_CATEGORY_SESSION_NOT_FOUND`, `ToolFilter.configured`, `harness_env` isolation), `RunTurn` (thoughts + text + usage streaming, built-in `PreToolUse` allow/deny/modified_args/additional_context -> `PostToolUse`, `host_tools` `InvokeHostTool` without `PreToolUse`/`PostToolUse`), `AbortTurn` with `turn_id` guard, `ResumeSession`, `ReadMCPResource`, `CloseSession`, and HTTP/SDK error classification (`429`, `401`, `403`, quota, context limit, `5xx`/timeout).
- [X] T004 Run `pytest` and verify tests fail before implementation.

## Phase 3: Handshake, Backend, Servicer & Bundle Script Implementation (Green + Refactor)

- [X] T005 Implement `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/handshake.py` (with OpenSSL mTLS frontend supporting `go-plugin` P-521 `AutoMTLS` client certs).
- [X] T006 Implement `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/backend.py` (`AgentBackend`, `AgentSession`, `GoogleAntigravityBackend`, `ScriptedFakeBackend`, `HybridBackend`).
- [X] T007 Implement `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/servicer.py` and `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/__main__.py`.
- [X] T008 Run `uv run --directory plugins/sofa-harness-antigravity --python 3.12 --extra dev pytest -v` until all offline unit tests pass.
- [X] T009 Implement `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py` producing symlink-free `<out>/sofa-harness-antigravity/{sofa-harness-antigravity, python/...}` runnable in Podman `golang:1.27`.

## Phase 4: Tier 2 Podman `golang:1.27` End-to-End Integration (TDD Red -> Green)

- [X] T010 [Red] Author `internal/worker/runner/antigravity_plugin_integration_test.go` (`//go:build integration`) building the self-contained Linux bundle via `scripts/build_linux_bundle.py`, staging `<plugin_dir>/sofa-harness-antigravity/linux_<arch>/`, configuring `harness "antigravity"` + `model_provider = null` + `env_refs` (`GEMINI_API_KEY`) + `options` (`{"backend": "fake_scripted"}`), and executing `WorkerRunner.Run` inside Podman `golang:1.27`.
- [X] T011 Run `go test -tags=integration ./internal/worker/runner/... -count=1` and verify Work (`PreToolUse`/`PostToolUse` + `sofa_checkpoint` + `sofa_complete_work`) -> Freeze -> Gatekeeper -> Read-Only QC -> `work_branch` + `BlobStore` artifacts pass with zero leftover Podman containers.

## Phase 5: Final Isolation & Verification Gate

- [X] T012 Verify `git diff go.mod` is empty, `git status -s` has no `.venv`/`__pycache__`/`.pytest_cache`/build junk, run `pytest`, `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -tags=integration ./internal/worker/runner/... -count=1`, and leave working tree uncommitted for parent review.
