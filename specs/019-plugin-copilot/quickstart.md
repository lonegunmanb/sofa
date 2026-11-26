# Quickstart: `019-plugin-copilot`

## Run Offline Plugin Unit Tests

```powershell
cd plugins/sofa-harness-copilot
gofmt -l .
go vet ./...
go test ./... -count=1
```

## Build Self-Contained Linux Bundle

```powershell
cd plugins/sofa-harness-copilot
go run ./scripts/build-bundle.go -out ./dist -arch amd64 -copilotbin /path/to/linux/copilot
```

Or build with a Go-based copilot CLI source (such as the Tier 2 stub):

```powershell
cd plugins/sofa-harness-copilot
go run ./scripts/build-bundle.go -out ./dist -arch amd64 -copilotsrc ../../internal/worker/runner/testdata/fakecopilot
```

## Run Root Unit & Podman Integration Tests

```powershell
cd d:\project\sofa
git diff go.mod
gofmt -l .
go vet ./...
go test ./... -count=1
go test -tags=integration ./internal/worker/runner/... -count=1
```
