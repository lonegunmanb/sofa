# Quickstart: Running Slice `028-operator-and-run-actor` Verification

## Tier 1 Unit Tests (In-Memory, Fast, Zero External Daemons)

```powershell
go test -v ./internal/actor/run/... -count=1
go test ./... -count=1
```

## Tier 2 Integration Tests (`daprhost` + Git + `WorkerDaemon`)

```powershell
go test -v -tags=integration ./internal/actor/run/... -count=1
```

## Formatting & Static Analysis

```powershell
gofmt -l .
go vet ./...
```
