---
description: "Stage 2 Slice 015 TDD tasks: go_tool cross-compiler cache, typed tool executor, QuotePaths/postcondition checks, and sofa_read_mcp_resource"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 tests in `internal/tool/runtime/executor_test.go` covering:
  - Native `go_tool` compilation, execution, and cache hit (second `Compile` makes 0 `go build` invocations) + concurrent `Compile` serialization;
  - Generated binary rejecting unknown fields, trailing JSON tokens, non-object JSON, `Invoke` Go `error`, and `ToolResponse` invariant violations;
  - `tool_use.input` preset properties omitted from `HostToolSpec` JSON Schema and forcibly overwritten on invocation;
  - `go_tool` `Accepted: true`, `Accepted: false` (`HandlerError == ""`), and non-zero exit / invalid JSON stdout (`HandlerError != ""`);
  - `QuotePaths` verification against `workspaceDir`, `/.sofa/imports/...`, and `/.sofa/bundle/...` (passing on exact UTF-8 match, downgraded to `quote_verification_failed` on fabricated quote, missing file, or symlink);
  - HCL `postcondition` evaluation using `config.Functions` (passing vs downgraded to `postcondition_failed` with custom `error_message`);
  - `external_tool` execution, `env_refs` secret resolution, error redaction, and non-zero exit code -> `HandlerError`;
  - `sofa_read_mcp_resource` conditional creation, authorized `(server_name, uri)` read, and unauthorized `(server_name, uri)` rejection without calling plugin.
- [X] T002 [Red] Write failing Tier 2 real-Podman integration tests in `internal/tool/runtime/executor_integration_test.go` (`//go:build integration`) cross-compiling a `go_tool` (`CGO_ENABLED=0 GOOS=linux`) on the host, staging it into `/.sofa/tools:ro` on `alpine:latest` (no Go toolchain inside container) with `/workspace` and `/.sofa/imports:ro`, and verifying `go_tool` (`QuotePaths` + `postcondition` pass and fabricated quote rejection) and `/bin/sh` `external_tool` via `PodmanEngine.Exec`.
- [X] T003 [Green] Implement `internal/tool/runtime/compiler.go`, `internal/tool/runtime/executor.go`, and `internal/tool/runtime/mcp_resource.go` so `go test ./internal/tool/runtime/... -count=1` passes.
- [X] T004 [Green & Verify] Run `go test -tags=integration ./internal/tool/runtime/... -count=1`, `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, resolve any issues, and mark all tasks `[X]`.
