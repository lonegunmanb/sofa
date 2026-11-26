# Specification Quality Checklist: AgentHarness Protocol & Go-Plugin Bridge

- [x] `proto/sofa/harness/v1/harness.proto` matches `docs/harness-plugin-contract.md` §3 verbatim (all 7 RPCs, `RunTurn` stream frames, enums, error model)
- [x] `buf.yaml` and `buf.gen.yaml` generate checked-in Go stubs (`pkg/harnessplugin/v1`) and Python stubs (`plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/proto`)
- [x] `pkg/harnessplugin` has zero `internal/` imports and handles local subprocess, Linux UDS, and container `0.0.0.0:19500` TCP `AutoMTLS` modes plus post-init `ScrubPluginEnv()`
- [x] `SessionMountLayout.HandshakeEnv` strictly allows only `SOFA_HARNESS_PLUGIN` and `PLUGIN_*` keys
- [x] `internal/harness/runtime` separates pure Go `Opener`/`Session` domain types from `harnessv1`, implements `ContainerRunner` (`SkipHostEnv: true`, `AutoMTLS: true`, Linux UDS vs non-Linux `-p 127.0.0.1::19500` + `podman port` + `podman rm -f -t 2`), in-memory secret materialization, non-blocking `Abort`, and concurrent `RunTurn` tool/hook callbacks
- [x] Strict Tier 1 (`go test ./...`) and Tier 2 (`//go:build integration` against real Podman `alpine:latest`) separation is preserved
