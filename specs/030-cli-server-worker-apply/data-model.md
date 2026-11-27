# Data Model: CLI Client, Daemon Config, and Command Payloads (`030-cli-server-worker-apply`)

## Client Configuration (`internal/cli/client.go`)

- `clientConfig`:
  - `ServerURL string`: resolved from `--server` -> `SOFA_SERVER_ADDR` -> `http://127.0.0.1:8080`
  - `APIToken string`: resolved from `--token` -> `SOFA_API_TOKEN` -> `""`
- `APIError`:
  - `StatusCode int`
  - `Message string` (`error` JSON field)
  - `MissingKeys []string` (`missing_keys` JSON field, populated on HTTP `422`)

## Daemon Configuration (`server_cmd.go`, `worker_cmd.go`)

- `blobStoreFlags`:
  - `BlobDir string`
  - `S3Bucket string`, `S3Region string`, `S3Endpoint string`, `S3Prefix string`, `S3ForcePathStyle bool`
- `serverFlags`:
  - `Dev bool`, `Listen string` (`127.0.0.1:8080`), `InternalListen string` (`127.0.0.1:0` in `--dev`, `127.0.0.1:50050` in distributed), `StateDir string` (`~/.sofa/dev-state`), `MaxConcurrency int` (`2` in `--dev`), `PluginDir string`, `RuntimeBin string` (`podman`), `APIToken string`, `AppAPIToken string`, `DaprGRPCPort string`, `blobStoreFlags`
- `workerFlags`:
  - `Listen string` (`127.0.0.1:50051` or `APP_PORT`), `AppID string`, `AppAPIToken string`, `DaprGRPCPort string`, `MaxConcurrency int` (`10`), `PluginDir string`, `StateDir string`, `TempDir string`, `RuntimeBin string` (`podman`), `blobStoreFlags`

## CLI Options Extension (`internal/cli/command.go`)

- `Options.UIHandler http.Handler`: optional web UI fallback handler mounted onto `server.Config.UIHandler` in `sofa server` / `sofa server --dev`.

## API Wire DTOs Used by CLI

- `CreateRunRequest` (multipart form `namespace`, `envs`, `secrets`, `plan`) -> `CreateRunResponse{RunID, Namespace, Status}`
- `server.RunDetailResponse`, `server.NodeDetailResponse`, `run.ListRunsResp`, `node.DecisionCard`
- `config.EnvEntry`, `config.SecretMeta`, `config.UpsertEntriesResp`, `config.DeleteEntryResp`
- `node.EnqueueInterjectionReq` / `node.EnqueueInterjectionResp`
- `DecisionRespondRequest`: `RunID`, `NodeAddr`, `SelectedOption`, `AnswerText`, `Approved`, `ReturnedUpstream`, `ReturnFallback`, `CommitSHA`, `ArtifactIDs`
