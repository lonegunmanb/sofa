---

description: "Tasks for providers, MCP servers, sandboxes, and root defaults"
---

# Tasks

**Input**: `specs/003-providers-sandbox-defaults/`

**Tests**: Mandatory and written before implementation.

## Phase 1: Setup

- [X] T001 Create the `internal/provider`, `internal/mcp/spec`, and `internal/sandbox/spec` package skeletons.

## Phase 2: Foundational RootDefaults

- [X] T002 [P] Write failing tests for source/sandbox/provider inventory and all 0/1/2 resolver branches in `internal/planner/defaults_test.go`, covering `data-model.md` fields `RootDefaults.Sources`, `RootDefaults.Sandboxes`, and `RootDefaults.ModelProviders`.
- [X] T003 Implement `RootDefaults`, `InspectRootDefaults`, `ResolveDefaultSandbox`, and `ResolveDefaultModelProvider` in `internal/planner/defaults.go`.

## Phase 3: US1 Model Provider

- [X] T004 [P] [US1] Write failing retry/default/regex tests in `internal/provider/retry_test.go`.
- [X] T005 [P] [US1] Write failing protocol, endpoint, authentication, header, and strict-type tests in `internal/provider/config_test.go`, covering `ModelProvider.type`, `ModelProvider.endpoint`, `ModelProvider.wire_api`, `ModelProvider.transport`, `ModelProvider.headers`, `ModelProvider.auth`, and `RetryPolicy`.
- [X] T006 [US1] Implement provider enums, validation, and retry merge in `internal/provider/config.go` and `internal/provider/retry.go`.
- [X] T007 [US1] Write failing golden block decode/value/sensitive-secret tests in `internal/planner/provider_test.go`.
- [X] T008 [US1] Implement and register `ModelProviderBlock` in `internal/provider/schema.go` and `internal/planner/config.go`.

## Phase 4: US2 MCP Server

- [X] T009 [P] [US2] Write failing HTTP/stdio/selection/timeout tests in `internal/mcp/config_test.go`, covering `MCPServer.tools/resources`, `MCPServer.timeout`, and `MCPServer.transport`.
- [X] T010 [P] [US2] Write failing deterministic ID tests including canonical-address separation in `internal/mcp/spec/schema_test.go`, covering `MCPServer.tool_ids/resource_ids`.
- [X] T011 [US2] Implement MCP config validation in `internal/mcp/config.go`.
- [X] T012 [US2] Implement and register `ServerBlock` in `internal/mcp/spec/schema.go` and `internal/planner/config.go`.
- [X] T013 [US2] Add planner integration tests for stable IDs and invalid transport selection in `internal/planner/mcp_test.go`.

## Phase 5: US3 Sandbox

- [X] T014 [P] [US3] Write failing runtime/network/mount/resource validation tests in `internal/sandbox/spec/schema_test.go`, covering `Sandbox.runtime`, `Sandbox.image`, `Sandbox.network`, `MountSpec.target`, `MountSpec.read_only`, and `ResourcesSpec`.
- [X] T015 [US3] Implement and register `SandboxBlock` in `internal/sandbox/spec/schema.go` and `internal/planner/config.go`.
- [X] T016 [US3] Add planner sandbox integration tests for default network, read-only mount rejection, and duplicate targets in `internal/planner/sandbox_test.go`.

## Phase 6: Polish and Commit Gate

- [X] T017 Run `gofmt`, `go vet ./...`, and `go test ./... -count=1`.
- [X] T018 Verify no network/container/environment credential access occurs in 003 production paths.
- [X] T019 Verify `internal/config` has no dependency on provider, MCP, sandbox, or planner packages.
- [X] T020 Mark all tasks complete and commit/push 003 to `origin/main` with a clean worktree before starting 004.

## Dependencies

RootDefaults precedes provider/sandbox consumers. Provider and MCP are independent. Sandbox is independent after setup. The commit/push gate is last.

## Phase 7: Review Remediation

- [X] T021 Reject unknown and null elements in provider and MCP map fields without panicking.
- [X] T022 Accept explicit null for optional provider attributes while rejecting null required attributes.
- [X] T023 Allow `sandbox.entrypoint = null`, reject an explicit empty list, and reject non-finite CPU values.
- [X] T024 Export deep-copied planned configs from provider, MCP server, and sandbox blocks for 005/007 consumers.
