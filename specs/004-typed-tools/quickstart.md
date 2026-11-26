# Quickstart Validation: Typed Tools

1. Run unit and planner tests:
   ```powershell
   go test ./internal/spec/... ./internal/tool/... ./internal/config/... ./internal/planner/... -count=1
   ```
2. Verify formatting, static analysis, and full repository suite offline:
   ```powershell
   gofmt -l .
   go vet ./...
   go test ./... -count=1
   ```
3. Expected outcomes:
   - `gofmt -l .` produces no output.
   - `go vet ./...` exits 0.
   - All `go_tool`, `external_tool`, `postcondition`, deterministic ID, `tool_name()`, and cold-start tests pass without network access or external `go get`.
