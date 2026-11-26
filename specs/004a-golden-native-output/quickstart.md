# Quickstart

```powershell
go test ./internal/planner -count=1
go vet ./...
go test ./... -count=1
```

Expected: all output regressions pass with no sofa-defined or sofa-registered output block.
