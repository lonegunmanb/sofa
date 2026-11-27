# Research: `OperatorActor` & `RunActor` (`028-operator-and-run-actor`)

## Key Codebase Findings

1. **`NodeActor` & `MergeActor` Callback Contracts**:
   - `internal/actor/node/actor.go` (`daprRunNotifier`) invokes `RunActor(run_id)` methods `"RegisterManagedBranch"` (payload `node.RegisterManagedBranchReq`) and `"OnNodeEvent"` (payload `node.NodeEventNotification`).
   - `internal/actor/merge/actor.go` (`daprRunMergeNotifier`) invokes `RunActor(run_id)` methods `"RegisterManagedBranch"` (payload `merge.RegisterManagedBranchReq`) and `"OnMergeCandidateResult"` (payload `merge.MergeCandidateNotification`).
   - Both `RegisterManagedBranchReq` types share identical JSON field names (`run_id`, `node_addr`, `epoch`, `repo_url`, `branch`, `base_sha`, `kind`).

2. **`applyeval` Integration (`internal/applyeval`)**:
   - `BuildEvalContext(planned, RuntimeState, WithPlan(planned), WithParentPlans(parents...))` indexes `RuntimeState.{Sources,Workers,DynamicWorkers,Merges,Modules}` by **local `node.Address`** within `planned`. Therefore `RunActor` MUST track per-module `RuntimeState` keyed by local address while addressing `NodeActor`, `MergeActor`, and `git.Ledger` by globally unique **`CanonicalAddress`**.
   - `EvalMergeBase` returns `SourcesKnown = false, Sources = nil, err = nil` when `based_source` is wholly known while `sources` still references incomplete `dynamic_worker` or `worker` nodes. Inspecting `sources_expr` AST (recursing through `local` expressions in `Plan.LocalExpressions()`) identifies which upstream `worker` / `dynamic_worker` nodes supply candidate branches so completed instances stream into `MergeActor.EnqueueCandidate` immediately without waiting for siblings.
   - `EvalModuleInputsAndValidate` returns a bound `*plan.Plan` whose `Context()["var"]` holds validated runtime inputs and sensitivity marks. Persisting module inputs (in wire format preserving sensitivity marks) allows `RunActor` to re-bind child module plans cleanly upon cold restart.
   - `EvalPlanOutputs` returns `map[string]cty.Value` preserving `golden.SensitiveMark` and `OutputSpec.Sensitive`. Serializing type, unmarked JSON value, `Sensitive` flag, and `SensitivePaths` gives Slice 029 exact masking/reveal fidelity.

3. **Artifact & Resource Materialization**:
   - `NodeActor.StartOrResumeReq` expects `Sandboxes`, `Harnesses`, `ModelProviders`, `MCPServers`, `MCPResources`, `Tools`, `ToolEnvs`, `Imports`, `BundleDir`, `RequiredEnvKeys` (`config.ExtractNodeEnvRefs(rootPlan, canonicalNodeAddr)`), `TokenRef`/`SSHKeyRef` (from matching `source`).
   - `plan.Plan` encodes all resource blocks as `NodeSpec.Config` `cty.Value` objects (`encodeSandboxConfig`, `encodeHarnessConfig`, `encodeModelProviderConfig`, `encodeMCPServerConfig`, `encodeExternalToolConfig` in `internal/planner/export.go`). Decoding those directly from the `Plan` hierarchy populates the catalogs cleanly with zero host HCL file reads.
   - `workerspec.Config.Imports` carries resolved `Sources` (`cty.Tuple` of artifact objects with `.id`); mapping each `.id` via `RunActor`'s persisted `PublishedArtifacts` index produces exact `[]artifact.ImportRequest`s with `BlobKey` and `SHA256`.

4. **`dapr/go-sdk@v1.16.0` Restart & Lossless Error Semantics**:
   - `daprhost.Manager` supports stopping and restarting on the same `StateDir` (reusing `master-key.json`, encrypted `actor-state.db`, and `scheduler` etcd store).
   - Inside a single Go test process simulating restart, cached actor instances in `runtime.GetActorRuntimeInstanceContext()` can be deactivated prior to stopping `mgr1` and re-registered on the restarted HTTP service so Phase 2 activates fresh `OperatorActor`/`RunActor`/`NodeActor`/`ProcessRegistryActor` instances against the persisted SQLite state.
   - Because `InvokeMethod`/`InvokeActor` discards Go error messages into `ErrActorInvokeFailed`, domain rejections (`ErrAlreadyMergedSharedBranch`, `ErrRunNotFound`, `ErrNodeNotFound`, `ErrRunConflict`) are returned via structured response fields (`AlreadyMergedSharedBranch`, `ErrorCode`, `ErrorMessage`) and translated by `RunClient` and exported error-decoding helpers.
