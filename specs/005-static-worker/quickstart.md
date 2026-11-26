# Quickstart Validation: Static Workers

1. Run worker spec and planner tests (including cold-start subprocess test):
   ```powershell
   go test ./internal/worker/spec/... ./internal/planner/... -count=1
   ```
2. Verify formatting, static analysis, and full repository test suite:
   ```powershell
   gofmt -l .
   go vet ./...
   go test ./... -count=1
   ```
3. Expected outcomes:
   - `gofmt -l .` prints no files.
   - `go vet ./...` exits 0.
   - All unit, planner, and cold-start tests pass offline with zero container, Git, or model calls.
