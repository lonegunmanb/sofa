# Quickstart: `internal/actor/config` Verification

## Tier 1 Offline Unit Tests

```powershell
go test -v ./internal/actor/config/... -count=1
```

## Tier 2 Encrypted Dapr Integration Tests

Requires local `scheduler` and `daprd` (`~/.dapr/bin/` or `PATH`).

```powershell
go test -v -tags=integration ./internal/actor/config/... -count=1
```

## Workspace Quality Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
