# Contract: `internal/actor/config`

## 1. `ConfigActor` Signatures & Constants

```go
package config

const (
	ActorType        = "ConfigActor"
	GlobalActorID    = "global"
	DefaultNamespace = "default"
)

type Scope string

const (
	ScopeGlobal    Scope = "global"
	ScopeNamespace Scope = "namespace"
	ScopeRun       Scope = "run"
)
```

Actor methods (all `func(context.Context, *Req) (*Resp, error)`):
- `UpsertEntries(ctx context.Context, req *UpsertEntriesReq) (*UpsertEntriesResp, error)`
- `DeleteEntry(ctx context.Context, req *DeleteEntryReq) (*DeleteEntryResp, error)`
- `DeleteRunScope(ctx context.Context, req *DeleteRunScopeReq) (*DeleteRunScopeResp, error)`
- `ListMetadata(ctx context.Context, req *ListMetadataReq) (*ListMetadataResp, error)`
- `PreflightCheck(ctx context.Context, req *PreflightCheckReq) (*PreflightCheckResp, error)`
- `ResolveForNode(ctx context.Context, req *ResolveForNodeReq) (*ResolveForNodeResp, error)`

Client stub:
```go
type ClientStub struct {
	UpsertEntries  func(context.Context, *UpsertEntriesReq) (*UpsertEntriesResp, error)
	DeleteEntry    func(context.Context, *DeleteEntryReq) (*DeleteEntryResp, error)
	DeleteRunScope func(context.Context, *DeleteRunScopeReq) (*DeleteRunScopeResp, error)
	ListMetadata   func(context.Context, *ListMetadataReq) (*ListMetadataResp, error)
	PreflightCheck func(context.Context, *PreflightCheckReq) (*PreflightCheckResp, error)
	ResolveForNode func(context.Context, *ResolveForNodeReq) (*ResolveForNodeResp, error)
}

func NewClientStub(client daprclient.Client) *ClientStub
```

## 2. `.sofaplan` Scanner Pure Functions

```go
func ExtractPlanEnvRefs(p *plan.Plan) []string
func ExtractNodeEnvRefs(p *plan.Plan, nodeAddr string) ([]string, error)
```

- `ExtractPlanEnvRefs`: Recursively walks `p` and all nested `Module.Plan` nodes, returning a sorted, deduplicated slice of host env/secret ref names from `source` (`token_ref`, `ssh_key_ref`), `model_provider` (`api_key_ref`, `bearer_token_ref`), `harness` (`env_refs` values), `mcp_server` (`http.bearer_token_ref`, `stdio.env_refs` values), and `external_tool` (`env_refs` values).
- `ExtractNodeEnvRefs`: Resolves `nodeAddr` (root address, canonical `module.<name>....` address, or `dynamic_worker.<name>[<key>]` instance address) and returns the sorted, deduplicated slice of host ref names actually needed by that node (including QC overrides and cross-module inherited singletons).

## 3. Security & Error Invariants

- Same-scope `Env` <-> `Secret` overwrite returns `ErrKindConflict` (`errors.Is(err, ErrKindConflict)`).
- Invalid key names return `ErrInvalidKeyName`.
- `ListMetadata` returns `EnvEntry` (with `Value`) and `SecretMeta` (no `Value` field).
- `PreflightCheck` returns `OK` and sorted deduplicated `MissingKeys` (never values).
- `ResolveForNode` is control-plane internal only and returns only entries for requested `Keys` (plus sorted `MissingKeys` if any requested key is unset).
