# Quickstart: `BlobStore` & `Artifact` Service (`011`)

## Run Targeted Tests

```powershell
$ErrorActionPreference = 'Stop'
go test ./internal/blob/... ./internal/artifact/... -count=1
```

## Run Full Quality Gates

```powershell
$ErrorActionPreference = 'Stop'
$fmt = gofmt -l .
if ($fmt) { Write-Error "Unformatted files:`n$fmt"; exit 1 }
go vet ./...
go test ./... -count=1
```
