# Quickstart: `024-process-registry-and-worker-daemon`

## Tier 1 Unit Tests (No External Daemons)

```powershell
go test ./internal/daprhost/... ./internal/actor/registry/... ./internal/worker/daemon/... -count=1
```

## Tier 2 Integration Tests (Real `scheduler` + Multi-`daprd` + SQLite Name Resolution)

```powershell
go test -v -tags=integration ./internal/daprhost/... ./internal/actor/registry/... ./internal/worker/daemon/... -count=1
```

## Full Workspace Gate

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
