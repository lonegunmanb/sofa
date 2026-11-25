# Quickstart

```powershell
go test ./internal/provider ./internal/mcp/... ./internal/sandbox/... ./internal/planner -count=1
go vet ./...
go test ./... -count=1
```

Expected: provider protocol/auth/retry validations pass, MCP IDs are stable, sandbox unsafe mounts fail, and RootDefaults covers 0/1/2 candidates.
