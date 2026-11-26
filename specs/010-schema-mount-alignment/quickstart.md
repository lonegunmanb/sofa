# Quickstart: Verifying `010-schema-mount-alignment`

Run all unit tests, `go vet`, and `gofmt` checks (offline, zero external daemons):

```powershell
$ErrorActionPreference = 'Stop'
$fmt = gofmt -l .
if ($fmt) { Write-Error "Unformatted files:`n$fmt" }
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go test ./... -count=1
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```
