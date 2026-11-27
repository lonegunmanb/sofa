# Quickstart: `internal/daprhost` (`022-local-dapr-host`)

## Run Tier 1 Unit Tests (Offline)

```powershell
go test ./internal/daprhost/... -count=1
```

## Run Tier 2 Integration Tests (Real `scheduler` + `daprd`)

Requires Dapr CLI binaries (`scheduler` and `daprd` v1.19.0) in `~/.dapr/bin` or `PATH`:

```powershell
go test -v -tags=integration ./internal/daprhost/... -count=1
```

## Usage Sketch

```go
// 1. Start internal Dapr callback HTTP service on 127.0.0.1:<appPort>
//    enforcing header "dapr-api-token" == mgr.AppAPIToken()
mgr, err := daprhost.NewManager(daprhost.Options{
    StateDir: stateDir,
    AppID:    "sofa-dev",
    AppPort:  appPort,
})
client, err := mgr.Start(ctx)
defer mgr.Stop(context.Background())
```
