# Quickstart Validation: 004b Harness Plugin Block & Sandbox Bridge Default

1. Verify `sandbox` default network is `"bridge"` when omitted or `null`, while explicit `"none"` and `"host"` remain honored:
   ```powershell
   go test ./internal/sandbox/spec ./internal/planner -run TestSandbox -count=1
   ```
2. Verify `harness` schema validation, `Value()`, `HarnessConfig()` deep-copy/sensitivity, and `model_provider` orthogonality:
   ```powershell
   go test ./internal/harness/spec ./internal/planner -run TestHarness -count=1
   ```
3. Verify `RootDefaults.ResolveDefaultHarness` (0 / 1 / 2+ candidates):
   ```powershell
   go test ./internal/planner -run TestRootDefaults -count=1
   ```
4. Verify full suite, formatting, and static analysis:
   ```powershell
   gofmt -l .
   go vet ./...
   go test ./... -count=1
   ```
