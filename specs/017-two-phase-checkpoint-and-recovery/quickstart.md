# Quickstart: Running Slice 017 Tests

## Tier 1 Unit & Fault-Injection Tests (Offline)

```powershell
go test ./internal/checkpoint/... -v -count=1
```

## Tier 2 Real Git + BlobStore Cross-Directory Integration Tests

```powershell
go test -tags=integration ./internal/git/... ./internal/checkpoint/... -v -count=1
```

## Full Repository Verification Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
