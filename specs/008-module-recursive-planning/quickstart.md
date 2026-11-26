# Quickstart: Recursive `module` Planning (`008`)

## Verify Tests Offline

Run unit tests for `internal/module/spec` and `internal/planner`:

```powershell
$ErrorActionPreference = 'Stop'
go test ./internal/module/spec/... ./internal/planner/... -count=1
```

## Full Quality Gates

```powershell
$ErrorActionPreference = 'Stop'
$fmt = gofmt -l .
if ($fmt) { Write-Error "Unformatted files:`n$fmt"; exit 1 }
go vet ./...
go test ./... -count=1
```
