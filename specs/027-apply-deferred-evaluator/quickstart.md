# Quickstart: Running `internal/applyeval` Tests

## Tier 1 Pure Core Unit Tests

```powershell
go test -v ./internal/applyeval/... -count=1
```

## Full Verification Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
