# Specification Quality Checklist: Plugin Cache Verification & Podman Sandbox Runtime

- [x] `NewDefaultReleaseFetcher` reuse in `internal/harness/lock.go` is specified
- [x] 4-tier local harness plugin discovery (`--plugin-dir` > `SOFA_PLUGIN_DIR` > `./.sofa/plugins` > `~/.sofa/plugins`) and regular-file verification are specified
- [x] Remote locked plugin cache, pre-extract SHA-256 verification, `os.OpenRoot` Zip-Slip/Zip-Bomb/symlink checks, and atomic activation are specified
- [x] Zero-secret-env invariant, `ComputeWorkspaceMounts` (`RO`, `nil`, `[]`, literal prefix, root file, wildcard fallback, `.git:ro` last), and cross-platform `network = "none"` guard are specified
- [x] `PodmanEngine` (`DetectPlatform`, `Exec`, `RunGatekeepers`) including pre/post `RequireCleanTree` snapshot & restoration is specified
- [x] Strict Tier 1 (`go test ./...`) and Tier 2 (`//go:build integration` against real Podman `alpine:latest`) separation is preserved
