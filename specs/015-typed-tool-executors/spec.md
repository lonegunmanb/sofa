# Feature Specification: Typed Tool Executors & Cross-Compiler Cache (`015-typed-tool-executors`)

**Feature Branch**: `015-typed-tool-executors`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `015` blueprint (`docs/stage2/015-typed-tool-executors.md`), `docs/stage2/016-session-gates-and-watchdog.md`, `docs/stage2/018-worker-qc-and-merge-runner.md`, `internal/tool/gotool/analyze.go`, `internal/tool/spec/{constraint,identity,schema}.go`, `internal/plan/model.go`, `internal/config/functions.go`, `internal/sandbox/runtime/podman.go`, and `internal/harness/runtime/types.go`.

## User Scenarios & Testing

### User Story 1 - Host-Side `go_tool` Wrapper Generation, Cross-Compilation & Staging (`internal/tool/runtime/compiler.go`) (Priority: P1)

`Compiler` generates a standalone `package main` wrapper around inline `go_tool` source, computes a deterministic SHA-256 cache key over `(wrappedSource, sortedDependencies, targetOS, targetArch, goVersion)`, serializes concurrent builds per cache key, cross-compiles static binaries (`CGO_ENABLED=0`), and stages binaries into `<destToolsDir>/<toolID>` for read-only container mounting at `/.sofa/tools/<toolID>`.

**Independent Test**: Tier 1 tests compile a native `go_tool` twice and verify the second `Compile` hits the cache with zero `go build` invocations, and verify the generated binary enforces strict single-JSON-object stdin decoding (`DisallowUnknownFields()`, trailing-token rejection), signal-aware `Invoke(ctx, input)`, and `ToolResponse` invariants.

**Acceptance Scenarios**:

1. **Given** a valid `plan.ToolSpec` (`Kind == "go"`), **When** `Compile` runs twice for the same target OS/arch and Go version, **Then** the first call builds and caches an executable binary and the second call returns a cache hit with zero build tool invocations.
2. **Given** concurrent `Compile` calls sharing the same cache key, **When** they execute in parallel, **Then** per-key locking ensures only one build runs and both callers receive a valid `CompiledTool`.
3. **Given** a compiled `go_tool` binary receiving unknown fields, trailing JSON tokens, non-object JSON (e.g. `null`), or invalid JSON on `stdin`, **When** executed, **Then** it writes a clear error to `stderr` and exits with code `1`.
4. **Given** a `go_tool` whose `Invoke` returns `err != nil` or violates `ToolResponse` invariants (`Accepted == true` with non-empty `Issues` or `Output == nil`; `Accepted == false` with non-nil `Output`, empty `Issues`, or blank `Code`/`Message`), **When** executed, **Then** it writes an error to `stderr` and exits with code `1`.
5. **Given** `CompiledTool` binaries, **When** `StageToolsDir` runs, **Then** each binary is placed at `<destToolsDir>/<toolID>` with executable permissions (`0755`) via hardlink or copy fallback.

### User Story 2 - Unified Typed `go_tool` & `external_tool` Executor (`internal/tool/runtime/executor.go`) (Priority: P1)

`Executor` builds `HostToolSpec` schemas (stripping `tool_use.input` preset properties), merges and validates tool arguments against `InputType`, runs tools inside the active sandbox container via `ContainerExecFunc` (or `NewPodmanContainerExec`), validates outputs against `OutputType`, verifies `QuotePaths` against host `workspace` / `imports` / `bundle` files via `os.OpenRoot`, and evaluates HCL `postcondition` blocks.

**Independent Test**: Tier 1 tests exercise `tool_use.input` schema stripping and tamper-proof overwrite, `Accepted: true`/`false` and `HandlerError` classification, `external_tool` `env_refs` secret materialization and redaction, `QuotePaths` anti-hallucination checks, and `postcondition` evaluation; Tier 2 integration tests cross-compile a `go_tool` on Windows for `linux/amd64` and run both `go_tool` and `external_tool` inside a real Go-less `alpine:latest` Podman container via `PodmanEngine.Exec`.

**Acceptance Scenarios**:

1. **Given** a `tool_use "<alias>"` binding with preset `input` fields, **When** `HostToolSpec` is generated, **Then** `Name` is `<alias>` and preset top-level properties are omitted from `ParametersJSONSchema` (`{"type":"object","properties":{},"additionalProperties":false}` if all properties are preset).
2. **Given** an LLM tool invocation attempting to override or omit a `tool_use.input` preset field, **When** `Invoke` merges arguments, **Then** preset keys are forcibly overwritten with the HCL-configured values before `InputType` validation.
3. **Given** a `go_tool` exiting `0` with `Accepted == false` and valid `Issues`, **When** `Invoke` completes, **Then** `InvocationResult.Accepted == false` and `ToolOutput` returns `HostToolResultSuccess` with `HandlerError == ""` and JSON `{"accepted":false,"issues":[...]}` so the LLM can self-repair and `016` does not terminate.
4. **Given** a `go_tool` or `external_tool` exiting non-zero, crashing, or returning invalid JSON / violating `ToolResponse` / `OutputType`, **When** `Invoke` completes, **Then** `ToolOutput` returns `HostToolResultFailure` with non-empty redacted `HandlerError` so `016` rolls back `tool_call_quota`.
5. **Given** an `Accepted == true` `go_tool` output with `QuotePaths` pointing to `{path, exact_quote}` objects, **When** every quote exists verbatim in the resolved UTF-8 file under `workspaceDir`, `importsDir`, or `bundleDir`, **Then** verification succeeds; when a file is missing, is a symlink, contains non-UTF8 bytes, or lacks `exact_quote`, the call is downgraded to `Accepted == false` with `HostToolResultSuccess` and issue code `quote_verification_failed`.
6. **Given** `Accepted == true` output failing any HCL `postcondition` evaluated with `input`, `output`, and `config.Functions(workspaceDir)`, **When** evaluated, **Then** the call is downgraded to `Accepted == false` with `HostToolResultSuccess`, issue code `postcondition_failed`, and the configured `error_message`.
7. **Given** an `external_tool` with `env` and `env_refs`, **When** executed, **Then** `LookupEnv` resolves `env_refs` in memory, passes `env` to `ContainerExecFunc` at `ResolveContainerWorkDir(spec.WorkingDir)`, and redacts all resolved secret values from any error messages.

### User Story 3 - Controlled MCP Resource Reader (`internal/tool/runtime/mcp_resource.go`) (Priority: P1)

When a session declares a non-empty list of allowed `MCPResourceSpec`, `NewMCPResourceReader` exposes `sofa_read_mcp_resource` with strict `(server_name, uri)` JSON Schema and enforces exact whitelist membership before invoking `Session.ReadMCPResource`.

**Independent Test**: Tier 1 tests verify `NewMCPResourceReader(nil, ...)` returns `false`, whitelisted `(server_name, uri)` invokes the reader and returns JSON contents, and unauthorized `(server_name, uri)` is rejected before calling the reader.

**Acceptance Scenarios**:

1. **Given** an empty `MCPResourceSpec` list, **When** `NewMCPResourceReader` is called, **Then** no `sofa_read_mcp_resource` tool is constructed.
2. **Given** a non-empty `MCPResourceSpec` list, **When** `sofa_read_mcp_resource` is invoked with an authorized `(server_name, uri)`, **Then** `Session.ReadMCPResource` is called and its contents are returned as JSON with `HostToolResultSuccess`.
3. **Given** a non-empty `MCPResourceSpec` list, **When** `sofa_read_mcp_resource` is invoked with an unauthorized `(server_name, uri)`, **Then** the call is rejected with `HostToolResultFailure` without calling `Session.ReadMCPResource`.

### Edge Cases

- Wrapper imports use `_sofa*` aliases (`_sofabytes`, `_sofacontext`, `_sofajson`, `_sofafmt`, `_sofaio`, `_sofaos`, `_sofasignal`, `_sofastrings`, `_sofasyscall`) placed before user `source` so user `import` declarations remain valid Go syntax and never collide.
- `Compiler.Compile` uses `os.MkdirTemp` sibling staging dirs and atomic move/copy into `<cacheDir>/<sha256>/<binName>` with `0755` permissions.
- `QuotePaths` rejects `..` path traversal, backslashes `\`, `.git` paths, absolute paths outside `/workspace/`, `/.sofa/imports/`, and `/.sofa/bundle/`, and symlinks via `os.OpenRoot` + `Lstat`.
- Invalid LLM arguments (malformed JSON, trailing tokens, non-object JSON, unknown attributes, `InputType` mismatch) return model-repairable `Accepted == false` (`HostToolResultSuccess`, `HandlerError == ""`, issue code `invalid_input`) rather than crashing quota accounting.

## Requirements

- **FR-001**: `internal/tool/runtime/compiler.go` MUST generate standalone `package main` wrappers around inline `go_tool` source using `_sofa*` standard-library aliases, enforce single-JSON-object `DisallowUnknownFields()` decoding, signal-aware `Invoke(ctx, input)`, `ToolResponse` invariants, deterministic SHA-256 caching with per-key concurrency locking, cross-compilation (`CGO_ENABLED=0`, `GO111MODULE=on`, `GOWORK=off`), and `StageToolsDir`.
- **FR-002**: `internal/tool/runtime/executor.go` MUST support `ContainerExecFunc` and `NewPodmanContainerExec`, `tool_use.input` schema stripping and forced overwrite merge, `InputType` / `OutputType` validation via `internal/tool/spec.Constraint`, `go_tool` vs `external_tool` execution & exit-code / protocol classification, `external_tool` `env_refs` secret materialization & redaction, `QuotePaths` `os.OpenRoot` verification, and HCL `postcondition` evaluation using `config.Functions`, exposing `InvocationResult`.
- **FR-003**: `internal/tool/runtime/mcp_resource.go` MUST define `MCPResourceToolName = "sofa_read_mcp_resource"`, construct the tool only when allowed `MCPResourceSpec` entries are non-empty, and enforce exact `(server_name, uri)` whitelist checks before invoking `Session.ReadMCPResource`.

## Success Criteria

- **SC-001**: `gofmt -l .` produces empty output.
- **SC-002**: `go vet ./...` passes with zero diagnostics.
- **SC-003**: `go test ./... -count=1` passes offline (including `internal/tool/runtime/executor_test.go`).
- **SC-004**: `go test -tags=integration ./internal/tool/runtime/... -count=1` passes against real Podman `alpine:latest` (without Go installed) executing both cross-compiled `go_tool` and `/bin/sh` `external_tool`.
