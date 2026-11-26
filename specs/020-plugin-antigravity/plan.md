# Implementation Plan: Isolated In-Repo `sofa-harness-antigravity` Plugin (`020-plugin-antigravity`)

**Branch**: `020-plugin-antigravity` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/020-plugin-antigravity/spec.md)

## Summary

Implement `sofa-harness-antigravity` inside an isolated Python project `plugins/sofa-harness-antigravity/` (`pyproject.toml`) implementing `hashicorp/go-plugin` AutoMTLS handshake (`handshake.py`), async 7-RPC `AgentHarnessServicer` (`servicer.py`), `GoogleAntigravityBackend` wrapping `google-antigravity` (`backend.py`) alongside deterministic `ScriptedFakeBackend`, a self-contained Linux bundle builder (`scripts/build_linux_bundle.py`), and a Tier 2 Podman `golang:1.27` end-to-end integration test (`internal/worker/runner/antigravity_plugin_integration_test.go`).

## Technical Context

**Language/Version**: Python 3.12 (`plugins/sofa-harness-antigravity/pyproject.toml`, managed via `uv` 0.13.6; root `go.mod` untouched)
**Primary Dependencies**: `grpcio`, `grpcio-health-checking`, `protobuf` (`7.36.2` matching generated `harness_pb2.py`), `cryptography`, `google-antigravity` (`0.1.24`), dev: `pytest`, `pytest-asyncio`
**Testing**: Tier 1 offline `pytest` (`tests/test_handshake.py`, `tests/test_servicer.py`); Tier 2 real Podman `golang:1.27` + Git integration test (`internal/worker/runner/antigravity_plugin_integration_test.go`, `//go:build integration`)
**Target Platform**: Linux container (`linux/amd64`, `linux/arm64`) packaged from Windows/macOS/Linux host using `uv` standalone Linux CPython + manylinux wheels (symlink-free)

## Constitution Check

*GATE: Verified against `.specify/memory/constitution.md` v1.5.1:*

- **Principle I (Spec-First Delivery)**: Complete `specs/020-plugin-antigravity/` artifacts authored before production code.
- **Principle II (Strict TDD)**: Failing `pytest` tests (`test_handshake.py`, `test_servicer.py`) and Tier 2 integration test (`antigravity_plugin_integration_test.go`) authored before implementations (`Red -> Green -> Refactor`).
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `AgentBackend`/`AgentSession` protocol isolates `google-antigravity` SDK for fast deterministic Tier 1 `pytest` tests; Tier 2 verifies real Go host <-> Python container `AutoMTLS` gRPC inside Podman `golang:1.27` and real Git bare repos.
- **Principle IV (Runtime Enforcement)**: `host_tool_names` guard prevents double quota accounting; `PLUGIN_*` and `SOFA_HARNESS_PLUGIN` scrubbed from `os.environ` prior to gRPC bind; `harness_env` never written to `os.environ`; `WorkerRunner` enforces `writable_paths`, Gatekeeper, and read-only QC.
- **Principle VI (Research-First & Surgical Sizing)**: Inspected `pkg/harnessplugin/plugin.go`, `internal/harness/runtime/container_runner.go`, `internal/harness/installer/installer.go`, `google-antigravity@0.1.24` (`Agent`, `LocalAgentConfig`, `CapabilitiesConfig`, `ToolWithSchema`, `PreToolCallDecideHook`, `PostToolCallHook`, `OnToolErrorHook`, `ChatResponse`), and verified `uv` Linux CPython + manylinux wheels inside `golang:1.27` Podman before planning. Scope is strictly `020-plugin-antigravity`.

## Project Structure

```text
.gitattributes                              # Enforce LF for plugins/sofa-harness-antigravity/bin/* and *.sh
specs/020-plugin-antigravity/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/antigravity-plugin.md
├── checklists/requirements.md
└── tasks.md

plugins/sofa-harness-antigravity/
├── .gitignore
├── pyproject.toml
├── bin/
│   └── sofa-harness-antigravity            # Pure LF POSIX /bin/sh entrypoint (0755)
├── src/sofa_harness_antigravity/
│   ├── __init__.py                         # Ensures proto/ on sys.path
│   ├── __main__.py                         # Async grpc.aio server + AutoMTLS handshake + signal shutdown
│   ├── handshake.py                        # 5-step go-plugin AutoMTLS + unpadded DER b64 + env scrub
│   ├── backend.py                          # AgentBackend protocol + GoogleAntigravityBackend + ScriptedFakeBackend
│   ├── servicer.py                         # 7-RPC AgentHarnessServicer + error classification
│   └── proto/sofa/harness/v1/
│       ├── harness_pb2.py                  # Pre-generated (existing)
│       └── harness_pb2_grpc.py             # Pre-generated (existing)
├── tests/
│   ├── test_handshake.py
│   └── test_servicer.py
└── scripts/
    └── build_linux_bundle.py               # Self-contained symlink-free Linux bundle builder

internal/worker/runner/
└── antigravity_plugin_integration_test.go  # //go:build integration (Podman golang:1.27)
```
