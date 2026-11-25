# Implementation Plan: Providers, MCP Servers, Sandboxes, and Root Defaults

**Branch**: `003-providers-sandbox-defaults` | **Date**: 2026-11-25 | **Spec**: [spec.md](./spec.md)

## Summary

Add offline provider, MCP server, and sandbox block families. Provider and MCP behavior follows the inspected r42 implementations, with sensitivity and golden integration adapted to sofa. RootDefaults becomes the shared resolver for source, sandbox, and model-provider names and remains invoked from `SofaConfig.Init`, never the loader.

## Technical Context

- Go 1.27.3, golden, HCL v2, cty, testify.
- Pure local unit tests; no container, network, environment credential lookup, or MCP connection.
- Existing planner registration and Init-time Root default architecture remains authoritative.

## Constitution Check

- Spec-first, TDD, pure boundaries, runtime enforcement, parity, and surgical sizing: PASS.
- No new external dependency or integration tier is introduced.

## Project Structure

```text
internal/
├── provider/
│   ├── config.go
│   ├── retry.go
│   └── schema.go
├── mcp/
│   ├── config.go
│   └── spec/schema.go
├── sandbox/spec/schema.go
└── planner/
    ├── config.go
    └── defaults.go
```

## Design Decisions

- Provider and MCP blocks use golden's normal decode path for unknown field/block rejection.
- Literal provider secrets are marked with `golden.SensitiveMark`; credential refs are syntax-only.
- MCP IDs use SHA-256 input `sofa/mcp-tool-id/v1` or `sofa/mcp-resource-id/v1`, canonical address, then selection name, with UUID-v4-style bit setting.
- Sandbox extra host mounts are read-only; worker writable paths remain the only future writable mechanism.
- RootDefaults is a pure `InspectRootDefaults` inventory plus resolver methods. `applyRootDefaults` uses it for source selection and leaves block slices unmodified.

## Post-Design Constitution Check

All gates pass. `internal/config` remains free of business block imports and Root default logic.
