# Copilot + Antigravity Self-Hosting Template (`copilot-agy-self-host`)

This template runs a heterogeneous self-hosting pipeline on **sofa**:

1. **Implementation (`worker.implement_slice`)**: `harness.copilot` inside `sandbox.go_dev` (`golang:1.27`), restricted to `var.writable_paths` and required to produce `summary.md`.
2. **Deterministic Gatekeeper (`gatekeeper.go_quality`)**: Runs `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` inside the frozen container with `require_clean_worktree = true`.
3. **Independent Read-Only QC (`qc`)**: `harness.antigravity` inside a separate read-only `sandbox.qc_audit` container (`model_provider = null`) auditing the frozen workspace and `summary.md`.
4. **Human Approval (`human_approval = "required"`)**: Pauses for explicit sign-off via the Vue 3 dashboard or CLI before completing.

---

## 1. Install Local Linux Dual Plugins

From the `sofa` repository root, cross-build and install both `sofa-harness-copilot` and `sofa-harness-antigravity` for your Podman Linux VM into `~/.sofa/plugins` (or pass `-plugin-dir ./.sofa/plugins`):

```bash
go run ./scripts/install-local-plugins.go
```

*(Optional flags: `-arch amd64|arm64`, `-plugin-dir <dir>`, `-copilot-linux-bin <path>`, `-skip-antigravity-sdk`.)*

---

## 2. Start `sofa server --dev`

```bash
go run ./cmd/sofa server --dev
```

*(If you installed plugins into `./.sofa/plugins`, pass `--plugin-dir ./.sofa/plugins` or set `SOFA_PLUGIN_DIR`.)*

---

## 3. Import Managed Secrets (Write-Only)

Export your host tokens in the shell and import them into `sofa`'s encrypted store without exposing values in CLI arguments:

```bash
sofa secret set --from-env GITHUB_TOKEN COPILOT_GITHUB_TOKEN GEMINI_API_KEY
```

---

## 4. Submit a Self-Hosting Run

```bash
go run ./cmd/sofa apply ./examples/copilot-agy-self-host \
  -var 'repo_url="https://github.com/lonegunmanb/sofa.git"' \
  -var 'slice_prompt="Implement the requested feature slice following AGENTS.md and strict TDD."'
```

When `worker.implement_slice` reaches `WAITING_HUMAN`, inspect the diff and approve either:

- **Web Dashboard**: Open `http://127.0.0.1:8080/decisions`
- **CLI**:

```bash
go run ./cmd/sofa decisions list
go run ./cmd/sofa decisions respond <decision_id> --approve
```
