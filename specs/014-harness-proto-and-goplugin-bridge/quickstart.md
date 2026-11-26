# Quickstart: AgentHarness Protocol & Go-Plugin Bridge (`014`)

## Protobuf & Stub Verification

```powershell
$ErrorActionPreference = 'Stop'
go run github.com/bufbuild/buf/cmd/buf@latest lint
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go run github.com/bufbuild/buf/cmd/buf@latest generate
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```

## Tier 1 Unit & Local Subprocess Tests

```powershell
$ErrorActionPreference = 'Stop'
go test ./pkg/harnessplugin/... ./internal/sandbox/... ./internal/harness/... -count=1
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```

## Tier 2 Real-Podman Container Integration Tests

```powershell
$ErrorActionPreference = 'Stop'
go test -tags=integration ./internal/harness/runtime/... -count=1
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```

## Full Quality Gates

```powershell
$ErrorActionPreference = 'Stop'
$fmt = gofmt -l .
if ($fmt) { Write-Error "Unformatted files:`n$fmt"; exit 1 }
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go test ./... -count=1
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go test -tags=integration ./internal/harness/runtime/... -count=1
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```
