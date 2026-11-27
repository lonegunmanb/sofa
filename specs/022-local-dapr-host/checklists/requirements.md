# Specification Quality Checklist: Local Dapr Host & Transparent Encryption Base

- [x] `Options`, `Manager`, `NewManager`, `AppAPIToken`, `DaprAPIToken`, `GRPCAddress`, `Start`, and `Stop` signatures align with `docs/stage3/022-local-dapr-host.md` §3.1
- [x] Binary lookup precedence (`Options` > `~/.dapr/bin` > `PATH`, Windows `.exe` support) is specified
- [x] Master key (`<stateDir>/keys/master-key.json`) atomic generation, 64-hex format, idempotent reuse, and Unix `0600` / Windows protected DACL permissions are specified
- [x] All three Dapr Component manifests (`local-secrets.yaml`, `statestore.yaml` with `primaryEncryptionKey`, `pubsub.yaml`) match `dapr@v1.19.0` schema
- [x] Dynamic loopback port allocation, isolated child env tokens (`APP_API_TOKEN`, `DAPR_API_TOKEN`), readiness gates, and lock-releasing shutdown are covered
- [x] Tier 1 offline unit tests and Tier 2 real-binary integration tests (`//go:build integration`) are defined
