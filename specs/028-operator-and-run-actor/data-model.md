# Data Model: `OperatorActor` & `RunActor` (`028-operator-and-run-actor`)

## 1. `OperatorActor` State (`"operator_actor_v1"`)

- **`OperatorActorType`**: `"OperatorActor"`, singleton ID `"global"`.
- **`persistedOperatorState`**:
  - `Runs map[string]RunSummary` keyed by `run_id`.
- **`RunSummary`**:
  - `RunID`, `Namespace`, `Status` (`RunStatus`), `PlanBlobKey`, `PlanSHA256`, `ErrorMessage`, `CreatedAt`, `UpdatedAt`.

## 2. `RunActor` State (`"run_actor_v1"`)

- **`RunActorType`**: `"RunActor"`, ID = `<run_id>`.
- **`RunStatus`**:
  - `"PENDING"`, `"RUNNING"`, `"WAITING_HUMAN"`, `"PAUSING"`, `"PAUSED"`, `"SUCCEEDED"`, `"FAILED"`, `"DELETED"`.
- **`persistedRunState`**:
  - `RunID`, `Namespace`, `Status`, `ManualPaused`, `ErrorMessage`
  - `PlanBlobKey`, `PlanSHA256`, `BundleExtractDir`
  - `RunOverrides map[string]string`
  - Host execution defaults: `RequiredArch`, `HostOS`, `TargetPlatform`, `PluginResolve`, `TempRoot`, `MaxGatekeeperReworks`, `MaxArtifactReworks`, `StallTimeout`, `WatchdogCheckInterval`
  - `Sources map[string]SourceRecord` (keyed by canonical `source` address: `Address`, `CanonicalAddress`, `Name`, `URL`, `Branch`, `CommitSHA`, `TokenRef`, `SSHKeyRef`)
  - `Ledger git.LedgerSnapshot`
  - `Nodes map[string]NodeStateRecord` (keyed by canonical address — includes static `worker`, `dynamic_worker`, `dynamic_worker[k]`, `merge`, `module`, `preserve`)
  - `PublishedArtifacts map[string]artifact.PublishedArtifact` (keyed by `art.ID`)
  - `EvaluatedPreserves map[string]git.PreserveRef` (keyed by canonical `preserve` address)
  - `Outputs map[string]EvaluatedOutput`
  - `CreatedAt`, `UpdatedAt`
- **`NodeStateRecord`**:
  - `Address`, `CanonicalAddress`, `Kind`, `ModulePath`, `ParentDynamicAddr`, `DynamicKey`
  - `Status` (`node.NodeStatus`, e.g. `PENDING`, `QUEUED_SLOT`, `RUNNING`, `WAITING_HUMAN`, `SUCCEEDED`, `FAILED`, `PAUSED`, `FALLBACK_TRIGGERED`)
  - `CurrentEpoch uint64`, `MergeEpoch uint64`
  - `BasedSource`, `BaseSHA`, `WorkBranch`, `CommitSHA`, `Result *string`
  - `Artifacts []artifact.PublishedArtifact`
  - `MergedIntoSharedBranch bool`
  - Dynamic expansion state (persisted once): `DynamicExpanded bool`, `DynamicKeys []string`, `DynamicElements map[string]WireValue` (preserving `golden.SensitiveMark` paths)
  - Merge tracking state: `TargetBranch`, `SharedTargetBranch`, `LatestTargetSHA`, `ExpectedSources []string`, `SourcesKnown bool`, `EnqueuedSources map[string]string` (source canonAddr -> `"ENQUEUED"`/`"MERGED"`), `ReworkCounts map[string]int`
  - Module tracking state: `BoundInputs map[string]WireValue`, `ModuleOutputs map[string]EvaluatedOutput`
  - Fallback tracking: `FallbackCounts map[string]int`, `PendingFallbackReason string`, `PendingFallbackArtifacts []artifact.PublishedArtifact`
  - `ErrorMessage string`

## 3. Public `NodeRunSummary` & `EvaluatedOutput`

- `GetRunStateResp` exposes:
  - `RunSummary` fields (`RunID`, `Namespace`, `Status`, `ManualPaused`, `ErrorMessage`, `CreatedAt`, `UpdatedAt`)
  - `Nodes []NodeRunSummary` (deterministic order: `Address`, `CanonicalAddress`, `Kind`, `ModulePath`, `Status`, `CurrentEpoch`, `WorkBranch`, `CommitSHA`, `Artifacts`, `MergedIntoSharedBranch`, `DynamicKeys`, `ErrorMessage`)
  - `Outputs map[string]EvaluatedOutput` (`Name`, `Description`, `Sensitive`, `TypeJSON`, `ValueJSON`, `Unknown`)
  - `Sources []applyeval.SourceRuntimeState`
  - `Preserves []git.PreserveRef`
  - `Ledger git.LedgerSnapshot`
