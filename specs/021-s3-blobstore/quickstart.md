# Quickstart: Running `internal/blob/s3` Unit & Integration Tests

## Tier 1 (Offline Unit Tests)

```powershell
go test -v ./internal/blob/s3/... -count=1
```

## Tier 2 (Podman `ministack` Integration Tests)

Requires Podman and cached image `docker.io/ministackorg/ministack:latest`.

```powershell
go test -v -tags=integration ./internal/blob/... -count=1
```

## Full Quality Gate

```powershell
gofmt -l .
go vet ./...
go test ./... -count=1
```
