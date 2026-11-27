# Quickstart: Running `internal/actor/merge` & `internal/worker/daemon` Tests

## Tier 1 Pure Unit Tests (Zero External Daemons)

```powershell
go test -v ./internal/actor/merge/... ./internal/worker/daemon/... -count=1
```

## Tier 2 Real Dapr + Git + Podman Integration Tests

Requires `scheduler`, `daprd`, `git`, and `podman` (with `alpine:latest` available):

```powershell
go test -v -tags=integration ./internal/actor/merge/... ./internal/worker/daemon/... -count=1
```

## Full Verification Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
