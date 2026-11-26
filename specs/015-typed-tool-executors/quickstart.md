# Quickstart: Running Slice 015 Tests

## Tier 1 Unit & Compiler Cache Tests (Offline)

```powershell
go test ./internal/tool/runtime/... -v -count=1
```

## Tier 2 Real Podman Integration Tests (`alpine:latest`, No Go Inside Container)

Requires local `podman` with `alpine:latest` image available:

```powershell
go test -tags=integration ./internal/tool/runtime/... -v -count=1
```

## Full Repository Verification Gates

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
