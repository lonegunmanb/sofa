# Copilot Self-Hosting Template (`copilot-agy-self-host`)

This template runs a dual-Copilot self-hosting pipeline on **sofa** with independent custom `model_provider` configurations for Dev and QC:

1. **Implementation (`worker.implement_slice`)**: `harness.copilot` + `model_provider.work` inside `sandbox.go_dev`, restricted to `var.writable_paths` and required to produce `summary.md`.
2. **Deterministic Gatekeeper (`qc.gatekeeper.go_quality`)**: Runs `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` inside `sandbox.qc_audit` with `require_clean_worktree = true` before the LLM audit.
3. **Independent Read-Only QC (`qc`)**: `harness.copilot` + `model_provider.qc` inside the separate read-only `sandbox.qc_audit` container auditing the frozen workspace and `summary.md`.
4. **Human Approval (`human_approval = "required"`)**: Pauses for explicit sign-off via the Vue 3 dashboard or CLI before publishing the work branch.

---

## 1. Install Local Linux Copilot Plugin

From the `sofa` repository root, cross-build and install `sofa-harness-copilot` (self-contained via `copilot-sdk/go`'s official `bundler`, requiring zero Node.js/`npm` installation) for your Podman Linux VM into `~/.sofa/plugins`:

```bash
go run ./scripts/install-local-plugins.go -only copilot
```

---

## 2. Configure `copilot.auto.sofavars` & Generate a Plan

Create or edit `examples/copilot-agy-self-host/copilot.auto.sofavars` (`*.sofavars` is gitignored) with your `work_*` and `qc_*` model provider endpoints, keys, and models, then initialize and inspect the compiled plan:

```bash
go run ./cmd/sofa init ./examples/copilot-agy-self-host --state-dir ./examples/copilot-agy-self-host/.sofa
go run ./cmd/sofa plan --state-dir ./examples/copilot-agy-self-host/.sofa --out ./examples/copilot-agy-self-host/.sofa/self-host.sofaplan
```

---

## 3. Start `sofa server --dev` & Apply

```bash
go run ./cmd/sofa server --dev
go run ./cmd/sofa apply ./examples/copilot-agy-self-host
```

When `worker.implement_slice` reaches `WAITING_HUMAN`, inspect the diff and approve via `http://127.0.0.1:8080/decisions` or `go run ./cmd/sofa decisions list`.
