# Quickstart: `020-plugin-antigravity`

## 1. Run Python Unit Tests (Host)

```powershell
uv run --directory plugins/sofa-harness-antigravity --python 3.12 pytest -v
```

## 2. Build Self-Contained Linux Bundle (`golang:1.27` compatible)

```powershell
uv run --python 3.12 plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py --out dist --arch amd64
```

## 3. Run Root Go Unit + Tier 2 Podman Integration Tests

```powershell
go test ./... -count=1
go test -tags=integration ./internal/worker/runner/... -run TestWorkerRunnerWithExternalAntigravityPluginPodmanIntegration -count=1
```
