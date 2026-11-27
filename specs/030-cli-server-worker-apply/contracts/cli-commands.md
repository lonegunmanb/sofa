# CLI Command Contracts (`030-cli-server-worker-apply`)

## Common Client Flags (on `apply`, `env`, `secret`, `runs`, `logs`, `pause`, `resume`, `rerun`, `delete`, `interject`, `decisions`)
- `--server <url>`: Server base URL (env: `SOFA_SERVER_ADDR`, default `http://127.0.0.1:8080`)
- `--token <token>`: Bearer API token (env: `SOFA_API_TOKEN`)

## Daemons
- `sofa server --dev [--listen 127.0.0.1:8080] [--internal-listen 127.0.0.1:0] [--state-dir ~/.sofa/dev-state] [--max-concurrency 2] [--plugin-dir <dir>] [--runtime-bin podman] [--blob-dir <dir>] [--s3-*] [--api-token <tok>]`
- `sofa server [--listen 0.0.0.0:8080] [--internal-listen 127.0.0.1:50050] [--dapr-grpc-port <port>] [--app-api-token <tok>] [--api-token <tok>] [--blob-dir <dir> | --s3-*]`
- `sofa worker [--listen 127.0.0.1:50051] [--app-id <id>] [--app-api-token <tok>] [--dapr-grpc-port <port>] [--max-concurrency 10] [--plugin-dir <dir>] [--state-dir <dir>] [--temp-dir <dir>] [--runtime-bin podman] [--blob-dir <dir> | --s3-*]`

## Apply
- `sofa apply [path] [--namespace default] [--env K=V]... [--secret-env K]... [--secret K=V]... [-var K=V]... [-var-file path]... [-d|--detach]`

## Managed Config & Secrets
- `sofa env set [--namespace <ns>] KEY=VALUE [KEY2=VALUE2...]`
- `sofa env list [--namespace <ns>] [--run <run_id>]`
- `sofa env delete [--namespace <ns>] KEY [KEY2...]`
- `sofa secret set [--namespace <ns>] --from-env KEY1 [KEY2...]`
- `sofa secret set [--namespace <ns>] KEY=VALUE [KEY2=VALUE2...]`
- `sofa secret list [--namespace <ns>] [--run <run_id>]`
- `sofa secret delete [--namespace <ns>] KEY [KEY2...]`

## Ops & Decisions
- `sofa runs [--namespace <ns>] [--status <status>]`
- `sofa runs get <run_id> [--reveal-sensitive]`
- `sofa logs <run_id> <node_id> [--epoch <N>]`
- `sofa pause [--mode planned|immediate] <run_id>`
- `sofa resume <run_id>`
- `sofa rerun <run_id> <node_id> [--reason "..."]`
- `sofa delete <run_id>`
- `sofa interject <run_id> <node_id> -m "<message>" [--wait]`
- `sofa decisions [list [--run <run_id>] [--status PENDING|ANSWERED|INVALIDATED]]`
- `sofa decisions respond <decision_id> [--run <run_id>] [--node <node_id>] [--choice <val> | --approve | --return-fallback <name>] [--comment "..."] [--commit-sha <sha>] [--artifact-id <id>...]`
