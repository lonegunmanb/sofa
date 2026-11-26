# Quickstart: Plugin Cache Verification & Podman Sandbox Runtime (`013`)

## Tier 1 Unit Tests

```powershell
$ErrorActionPreference = 'Stop'
go test ./internal/harness/... ./internal/sandbox/... -count=1
```

## Tier 2 Real-Podman Integration Tests

```powershell
$ErrorActionPreference = 'Stop'
go test -tags=integration ./internal/sandbox/runtime/... -count=1
```

## Full Quality Gates

```powershell
$ErrorActionPreference = 'Stop'
$fmt = gofmt -l .
if ($fmt) { Write-Error "Unformatted files:`n$fmt"; exit 1 }
go vet ./...
go test ./... -count=1
go test -tags=integration ./internal/sandbox/runtime/... -count=1
```
