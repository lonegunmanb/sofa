# Data Model: `internal/actor/config` (`023-config-actor-env-secrets`)

## 1. Scopes & Key Rules

| Constant / Type | Value / Rule | Notes |
| :--- | :--- | :--- |
| `ActorType` | `"ConfigActor"` | Dapr Actor Type |
| `GlobalActorID` | `"global"` | Singleton Actor ID |
| `DefaultNamespace` | `"default"` | Normalized value when `Namespace` is empty/whitespace |
| `ScopeGlobal` | `"global"` | Lowest precedence; `RunID` and `Namespace` unused/must be empty |
| `ScopeNamespace` | `"namespace"` | Middle precedence; `Namespace` normalized to `"default"` if empty |
| `ScopeRun` | `"run"` | Highest persisted precedence; `RunID` required non-empty |
| Key Name Regex | `^[A-Za-z_][A-Za-z0-9_]*$` | Enforced on all upserts, deletes, preflight, and resolve calls |

## 2. Persisted Actor State (`config_store_v1`)

Stored as a single JSON-serialized document under key `"config_store_v1"` in Dapr `StateManagerContext` (transparently AES-GCM encrypted by `daprd` via `primaryEncryptionKey`):

```go
type storedItem struct {
    Value     string    `json:"value"`
    IsSecret  bool      `json:"is_secret"`
    UpdatedAt time.Time `json:"updated_at"`
}

type scopeBucket struct {
    Items map[string]storedItem `json:"items,omitempty"`
}

type persistedStore struct {
    Global     scopeBucket            `json:"global"`
    Namespaces map[string]scopeBucket `json:"namespaces,omitempty"`
    Runs       map[string]scopeBucket `json:"runs,omitempty"`
}
```

### Invariants

1. **Same-Scope Mutual Exclusion**: Within a single `scopeBucket`, `storedItem.IsSecret` cannot flip between `true` and `false` via `UpsertEntries`. Caller must explicitly call `DeleteEntry` before re-creating with the opposite sensitivity.
2. **Cross-Scope Isolation & Override**: Distinct namespaces (`"proj-a"` vs `"proj-b"`) and distinct runs (`"run-1"` vs `"run-2"`) live in separate map buckets. Resolution checks `RunOverrides` -> `Runs[runID]` -> `Namespaces[ns]` -> `Global`.
3. **Write-Only Secrets**: `SecretMeta` (`Name`, `Scope`, `Namespace`, `RunID`, `UpdatedAt`) contains no `Value` field; `ListMetadata` and `PreflightCheck` never read or emit secret plaintext.
