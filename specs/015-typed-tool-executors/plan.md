# Implementation Plan: Typed Tool Executors & Cross-Compiler Cache (`015-typed-tool-executors`)

**Branch**: `015-typed-tool-executors` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `015` under `internal/tool/runtime/`:
1. `compiler.go`: standalone `package main` wrapper generator for inline `go_tool` snippets, SHA-256 keyed concurrent-safe cross-compiler cache (`CGO_ENABLED=0`), and `StageToolsDir` for `/.sofa/tools/<toolID>:ro`.
2. `executor.go`: unified typed `go_tool` & `external_tool` executor with `ContainerExecFunc` / `NewPodmanContainerExec`, `tool_use.input` schema stripping & forced merge, `Constraint`-based `InputType`/`OutputType` validation, `env_refs` materialization & secret redaction, `os.OpenRoot` `QuotePaths` anti-hallucination verification, and `config.Functions`-backed HCL `postcondition` evaluation.
3. `mcp_resource.go`: `sofa_read_mcp_resource` whitelist-enforced MCP resource reader tool.
4. `executor_test.go` (Tier 1) and `executor_integration_test.go` (Tier 2 `//go:build integration` against real Podman `alpine:latest`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **New Direct Dependencies**: None (uses Go stdlib + existing repo dependencies).
- **Existing Packages Reused**:
  - `internal/tool/gotool` (`ValidateDependency`)
  - `internal/tool/spec` (`Constraint`, `NewConstraint`, `JSONSchema`)
  - `internal/spec` (`Condition`, `Issue`, `ToolResponse`)
  - `internal/plan` (`ToolSpec`)
  - `internal/config` (`Functions`)
  - `internal/sandbox/runtime` (`Engine`, `ResolveContainerWorkDir`)
  - `internal/harness/runtime` (`HostToolSpec`, `ToolInvocation`, `ToolOutput`, `HostToolResultSuccess`, `HostToolResultFailure`, `ToolHandler`, `MCPResourceContent`)
- **Testing**: Tier 1 offline unit tests (`go test ./... -count=1`) + Tier 2 real Podman `alpine:latest` integration tests (`go test -tags=integration ./internal/tool/runtime/... -count=1`).

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored in `specs/015-typed-tool-executors/` prior to production Go implementation.
- **Principle II (Strict TDD)**: Failing Tier 1 (`executor_test.go`) and Tier 2 (`executor_integration_test.go`) tests written and verified Red before implementing `compiler.go`, `executor.go`, and `mcp_resource.go`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: Container execution abstracted behind `ContainerExecFunc` (and `NewPodmanContainerExec` adapter around `sandboxruntime.Engine`) and `LookupEnv` / `ReadMCPResource` callbacks, verified in Tier 2 against real Podman `alpine:latest` containers.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Enforces host-side `tool_use.input` preset overwrite, `InputType`/`OutputType` validation, `os.OpenRoot` `QuotePaths` file-quote verification, HCL `postcondition` checks, and exact `(server_name, uri)` MCP resource whitelisting.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `gotool/analyze.go`, `tool/spec/constraint.go`, `spec/condition.go`, `spec/response.go`, `config/functions.go`, `sandbox/runtime/{podman,spec}.go`, and `harness/runtime/types.go` before planning; scoped strictly to Slice `015`.

## Files

```text
internal/tool/runtime/compiler.go
internal/tool/runtime/executor.go
internal/tool/runtime/mcp_resource.go
internal/tool/runtime/executor_test.go
internal/tool/runtime/executor_integration_test.go
specs/015-typed-tool-executors/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
