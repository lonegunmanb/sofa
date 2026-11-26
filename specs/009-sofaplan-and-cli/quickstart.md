# Quickstart: `.sofaplan` & Offline CLI (`009`)

## Run Targeted Tests

```powershell
$ErrorActionPreference = 'Stop'
go test ./internal/plan/... ./internal/planner/... ./internal/variableschema/... ./internal/cli/... ./cmd/sofa/... -count=1
```

## Run Full Quality Gates

```powershell
$ErrorActionPreference = 'Stop'
$fmt = gofmt -l .
if ($fmt) { Write-Error "Unformatted files:`n$fmt"; exit 1 }
go vet ./...
go test ./... -count=1
```
