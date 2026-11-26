# Quickstart: `018-worker-qc-and-merge-runner`

## Tier 1 Offline Verification
```powershell
go test ./internal/worker/runner/... -v -count=1
go test ./... -count=1
```

## Tier 2 Real Git + Podman Integration Verification
```powershell
go test -tags=integration ./internal/git/... ./internal/checkpoint/... ./internal/worker/runner/... -v -count=1
```

## Static Checks
```powershell
gofmt -l .
go vet ./...
```
