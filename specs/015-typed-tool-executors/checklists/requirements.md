# Specification Quality Checklist: Typed Tool Executors & Cross-Compiler Cache

- [x] Standalone `go_tool` wrapper (`_sofa*` import aliases, single-JSON-object `DisallowUnknownFields()`, signal handling, `ToolResponse` protocol invariants) is specified
- [x] SHA-256 cache key, per-key mutex serialization, `CGO_ENABLED=0` cross-compilation, and `StageToolsDir` are specified
- [x] `tool_use.input` schema stripping and tamper-proof overwrite merge before `InputType` validation are specified
- [x] Clear separation between `HandlerError` (`HostToolResultFailure`) and business-level rejections (`Accepted: false` with `HostToolResultSuccess`) is specified
- [x] `external_tool` `env` / `env_refs` secret materialization, `LookupEnv` injection, and secret redaction are specified
- [x] `QuotePaths` `os.OpenRoot` anti-hallucination verification (`workspace`, `/.sofa/imports/`, `/.sofa/bundle/`) and HCL `postcondition` evaluation (`config.Functions`) are specified
- [x] `sofa_read_mcp_resource` conditional registration and exact `(server_name, uri)` whitelist enforcement are specified
- [x] Tier 1 (`executor_test.go`) and Tier 2 (`executor_integration_test.go` against real Podman `alpine:latest`) are separated and planned
