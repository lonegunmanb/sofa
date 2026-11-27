# Data Model: `internal/server` (`029-rest-ws-api-server`)

## 1. Server Configuration & Lifecycle

- **`Config`**:
  - `PublicAddr string` (default `"127.0.0.1:8080"`; must resolve to `127.0.0.1` or `::1` when `APIToken == ""` and `DevMode == true`; non-loopback or non-`DevMode` with empty `APIToken` fails in `New`)
  - `InternalAddr string` (optional internal Dapr callback listener, e.g. `"127.0.0.1:3501"`, must be loopback when non-empty)
  - `APIToken string` (`SOFA_API_TOKEN`)
  - `AppAPIToken string` / `AppAPITokenProvider func() string` (Dapr `APP_API_TOKEN` for internal callback server)
  - `DevMode bool`
  - `PreviewSecret []byte` (optional HMAC key; defaults to `APIToken` or ephemeral random 32-byte key)
  - `PreviewTicketTTL time.Duration` (default `5 * time.Minute`)
  - `Backend Backend` (Dapr-backed or in-memory test backend)
  - `BlobStore blob.Store`
  - `UIHandler http.Handler` (optional fallback handler for `031` SPA)
  - `RegisterInternal func(svc common.Service) error` (registers Control Plane Actor factories & optional dev `WorkerDaemon` handlers onto internal Dapr service)
  - `RunDefaults RunStartDefaults` (`RequiredArch`, `HostOS`, `TargetPlatform`, `PluginResolve`, `TempRoot`, `MaxGatekeeperReworks`, `MaxArtifactReworks`, `StallTimeout`, `WatchdogCheckInterval`)

## 2. Backend Interfaces (Pure Boundary)

- **`Backend`**:
  - `ListRuns(ctx, req *run.ListRunsReq) (*run.ListRunsResp, error)`
  - `StartRun(ctx, req *run.StartRunReq) (*run.StartRunResp, error)`
  - `GetRunState(ctx, runID string) (*run.GetRunStateResp, error)`
  - `PauseRun(ctx, runID string, req *run.PauseRunReq) (*run.PauseRunResp, error)`
  - `ResumeRun(ctx, runID string) (*run.ResumeRunResp, error)`
  - `DeleteRun(ctx, runID string) (*run.DeleteRunResp, error)`
  - `RerunNode(ctx, runID string, req *run.RerunNodeReq) (*run.RerunNodeResp, error)`
  - `ListDecisions(ctx, runID string) (*run.ListDecisionsResp, error)`
  - `GetNodeState(ctx, runID, nodeAddr string) (*node.GetNodeStateResp, error)`
  - `EnqueueInterjection(ctx, runID, nodeAddr string, req *node.EnqueueInterjectionReq) (*node.EnqueueInterjectionResp, error)`
  - `SubmitDecision(ctx, runID, nodeAddr string, req *node.SubmitDecisionReq) (*node.SubmitDecisionResp, error)`
  - `PreflightCheck(ctx, req *config.PreflightCheckReq) (*config.PreflightCheckResp, error)`
  - `UpsertConfig(ctx, req *config.UpsertEntriesReq) (*config.UpsertEntriesResp, error)`
  - `DeleteConfig(ctx, req *config.DeleteEntryReq) (*config.DeleteEntryResp, error)`
  - `ListConfig(ctx, req *config.ListMetadataReq) (*config.ListMetadataResp, error)`
  - `DeleteRunScope(ctx, runID string) (*config.DeleteRunScopeResp, error)`

## 3. REST View & Request Models

- **`RunDetailResponse`**:
  - `RunID`, `Namespace`, `Status`, `ManualPaused`, `PlanBlobKey`, `PlanSHA256`, `ErrorMessage`, `Nodes`, `Outputs map[string]RunOutputView`, `Sources`, `Preserves`, `Ledger`, `CreatedAt`, `UpdatedAt`.
- **`RunOutputView`**:
  - `Name string`
  - `Description string`
  - `Sensitive bool`
  - `Unknown bool`
  - `Value any` (recursively masked as `"<sensitive>"` whenever top-level `Sensitive` or `golden.SensitiveMark` is present unless `?reveal_sensitive=true`)
- **`NodeDetailResponse`**:
  - `RunID`, `NodeAddr`, `Summary *run.NodeRunSummary`, `CurrentEpoch`, `Status`, `ManualPaused`, `WorkerProcessID`, `WorkerAppID`, `LastError`, `Checkpoints []checkpoint.CheckpointRecord`, `PendingInterjections []session.HumanInterjection`, `InterjectionLog []node.InterjectionRecord`, `DecisionCards []node.DecisionCard`, `Artifacts []artifact.PublishedArtifact`, `PendingPublication *runner.PendingPublication`, `Approval *runner.ApprovalRecord`, `Outcome *runner.NodeOutcome`.
- **`SecretView`** (strictly omits `Value`):
  - `Name`, `Scope`, `Namespace`, `UpdatedAt`.
- **`PreviewTicketResponse`**:
  - `Ticket string`, `ExpiresAt time.Time`.
- **`WSEvent`**:
  - `Topic string`, `Type string`, `RunID string`, `NodeAddr string`, `Epoch uint64`, `Timestamp time.Time`, `Payload json.RawMessage`.
