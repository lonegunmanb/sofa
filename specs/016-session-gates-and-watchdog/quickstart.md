# Quickstart: Running Slice 016 Tests

## Tier 1 Unit & State-Machine Tests (Offline)

```powershell
go test ./internal/session/... -v -count=1
```

## Full Repository Verification Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
