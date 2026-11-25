# Phase 0 Research

## Provider and MCP alignment

Inspected r42 provider config/retry/schema and MCP config/schema implementations. Reuse their protocol enums, timeout bounds, transport validation, retry defaults, environment-reference semantics, and deterministic SHA-256 UUID strategy. Sofa changes only ID prefixes and integrates golden planning.

## Sandbox safety

Container execution is deferred. Extra sandbox mounts are host material and must be read-only; writable workspace scopes belong to `worker.writable_paths`.

## Root defaults

Root singleton inspection is pure and Init-time. It must never run during directory loading, schema inspection, or child-module discovery.

No unresolved clarifications.
