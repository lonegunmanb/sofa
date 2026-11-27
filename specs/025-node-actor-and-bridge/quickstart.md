# Quickstart: Running `internal/actor/node` Tests

## Tier 1 Pure Unit Tests (Zero External Daemons)

```powershell
go test -v ./internal/actor/node/... -count=1
```

## Tier 2 Real-Dapr Integration Tests

Requires `scheduler`, `daprd`, and `git` on `PATH` (or `~/.dapr/bin`):

```powershell
go test -v -tags=integration ./internal/actor/node/... -count=1
```

## Full Verification Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
