# Specification Quality Checklist: Three-Tier Managed Env / Secret ConfigActor

- [x] `ConfigActor` (`ActorType = "ConfigActor"`, `GlobalActorID = "global"`) embeds `actor.ServerImplBaseCtx` and supports `daprhost.NewActorStateManager` injection as well as Tier 1 in-memory `StateManagerContext`
- [x] Three-tier precedence (`RunOverrides` > `Run` > `Namespace` normalized to `"default"` > `Global`) and cross-namespace isolation are specified
- [x] Same-scope `Env` vs `Secret` mutual exclusion, key regex `^[A-Za-z_][A-Za-z0-9_]*$`, and strictly write-only `SecretMeta` (no `Value` field) are specified
- [x] All 6 Dapr single-parameter pointer RPC signatures and `NewClientStub` (`ImplActorClientStub`) are specified
- [x] Recursive `ExtractPlanEnvRefs` and `ExtractNodeEnvRefs` covering `source`, `model_provider`, `harness`, `mcp_server`, `external_tool`, and nested `Module.Plan` (using `env_refs` map values, not keys) are specified
- [x] Tier 1 unit tests and Tier 2 encrypted Dapr restart persistence integration tests (`//go:build integration`) are defined
