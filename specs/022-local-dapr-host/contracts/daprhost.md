# Contract: `internal/daprhost`

## 1. Public API

```go
package daprhost

type Options struct {
	StateDir              string // Required: persistent root directory
	AppID                 string // Required: Dapr app ID (e.g. "sofa-dev" or "sofa-worker-<id>")
	AppPort               int    // Local dapr/go-sdk callback port on 127.0.0.1 (0..65535)
	AppAPIToken           string // Optional; auto-generated 32-byte random hex if empty
	DaprAPIToken          string // Optional; auto-generated 32-byte random hex if empty
	SchedulerBin          string // Optional explicit path to scheduler binary
	DaprdBin              string // Optional explicit path to daprd binary
	ExternalSchedulerAddr string // Optional external scheduler host:port (skips embedded scheduler launch)
}

type Manager struct { ... }

func NewManager(opts Options) (*Manager, error)
func (m *Manager) AppAPIToken() string
func (m *Manager) DaprAPIToken() string
func (m *Manager) GRPCAddress() string
func (m *Manager) HTTPAddress() string
func (m *Manager) HTTPPort() int
func (m *Manager) GRPCPort() int
func (m *Manager) SchedulerAddress() string
func (m *Manager) Client() daprclient.Client
func (m *Manager) Start(ctx context.Context) (daprclient.Client, error)
func (m *Manager) Stop(ctx context.Context) error

func NewActorStateManager(actorType, actorID string, client daprclient.Client) actor.StateManagerContext
```

## 2. Behavioral Invariants

1. **Binary Discovery**: `Options.{SchedulerBin,DaprdBin}` > `~/.dapr/bin/{scheduler,daprd}` (`.exe` on Windows) > `exec.LookPath`.
2. **Master Key (`<stateDir>/keys/master-key.json`)**:
   - First creation generates 32 random bytes (64 lowercase hex characters) under JSON key `"masterEncryptionKey"`, written atomically with `0600` (Unix) / protected current-user DACL (Windows).
   - Subsequent runs validate and reuse the existing key verbatim; invalid/corrupt existing key files fail loudly instead of overwriting.
3. **Component Manifests (`<stateDir>/components/`)**:
   - `local-secrets.yaml`: `secretstores.local.file` named `"sofa-secrets"` pointing `secretsFile` at `<stateDir>/keys/master-key.json`.
   - `statestore.yaml`: `state.sqlite` named `"sofa-statestore"`, `actorStateStore: "true"`, `connectionString: "file:<slashStateDir>/actor-state.db"`, `primaryEncryptionKey` referencing `secretKeyRef: {name: masterEncryptionKey, key: masterEncryptionKey}`, and top-level `auth: {secretStore: sofa-secrets}`.
   - `pubsub.yaml`: `pubsub.in-memory` named `"sofa-pubsub"`.
4. **Isolation & Security**:
   - All listeners bind exclusively to `127.0.0.1` on dynamically allocated free ports.
   - `APP_API_TOKEN` and `DAPR_API_TOKEN` are passed solely via child `cmd.Env` (never `os.Setenv`).
   - Returned `daprclient.Client` is pre-configured with `WithAuthToken(m.DaprAPIToken())`.
5. **Lifecycle**:
   - `Start` waits for `scheduler /healthz`, `daprd /v1.0/healthz/outbound`, and Actor Runtime placement readiness, rolling back launched processes on any failure.
   - `Stop` closes the Dapr client (issuing graceful `Shutdown` first), reaps both `daprd` and `scheduler`, and guarantees SQLite and `etcd` locks are released before returning.
