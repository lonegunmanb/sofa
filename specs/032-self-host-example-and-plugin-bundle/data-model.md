# Data Model: Self-Hosting Template & Local Linux Dual-Plugin Installer (`032`)

## 1. Self-Hosting Template Schema (`examples/copilot-agy-self-host/`)

### Input Variables (`variables.sofa.hcl`)

| Variable | Type | Default | Validation |
| :--- | :--- | :--- | :--- |
| `repo_url` | `string` | *(required)* | `length(trimspace(var.repo_url)) > 0` |
| `base_branch` | `string` | `"main"` | `length(trimspace(var.base_branch)) > 0` |
| `sandbox_image` | `string` | `"golang:1.27"` | `length(trimspace(var.sandbox_image)) > 0` |
| `work_model` | `string` | `"claude-sonnet-4.5"` | `length(trimspace(var.work_model)) > 0` |
| `qc_model` | `string` | `"gemini-2.5-pro"` | `length(trimspace(var.qc_model)) > 0` |
| `slice_prompt` | `string` | *(required)* | `length(trimspace(var.slice_prompt)) > 0` |
| `writable_paths` | `list(string)` | `["internal/**", "cmd/**", "specs/**", "examples/**"]` | `length(var.writable_paths) > 0` |
| `human_approval` | `string` | `"required"` | `contains(["required", "optional"], var.human_approval)` |

### Workflow Blocks (`main.sofa.hcl`)

| Block | Address | Key Attributes |
| :--- | :--- | :--- |
| `source` | `source.repo` | `url = var.repo_url`, `branch = var.base_branch`, `token_ref = "GITHUB_TOKEN"` |
| `sandbox` | `sandbox.go_dev` | `runtime = "podman"`, `image = var.sandbox_image` |
| `sandbox` | `sandbox.qc_audit` | `runtime = "podman"`, `image = var.sandbox_image` |
| `harness` | `harness.copilot` | Local mode (`source`/`version` omitted), `env_refs = { GITHUB_TOKEN = "COPILOT_GITHUB_TOKEN" }` |
| `harness` | `harness.antigravity` | Local mode (`source`/`version` omitted), `env_refs = { GEMINI_API_KEY = "GEMINI_API_KEY" }` |
| `worker` | `worker.implement_slice` | `based_source = source.repo.branch`, `sandbox = sandbox.go_dev`, `harness = harness.copilot`, `model = var.work_model`, `writable_paths = var.writable_paths`, `human_approval = var.human_approval`, `system_prompt`, `prompt = var.slice_prompt` |
| `artifact` (nested) | `artifact "summary"` | `type = "file"`, `path = "summary.md"`, `required = true`, `non_empty = true` |
| `gatekeeper` (nested) | `gatekeeper "go_quality"` | `command = ["sh", "-c", "...gofmt -l . ... && go vet ./... && go test ./... -count=1"]`, `require_clean_worktree = true` |
| `qc` (nested) | `qc` | `sandbox = sandbox.qc_audit`, `harness = harness.antigravity`, `model_provider = null`, `model = var.qc_model`, `max_qc_rounds = 3`, `criteria = { ... }` |

### Outputs (`outputs.sofa.hcl`)

| Output | Value Expression |
| :--- | :--- |
| `work_branch` | `worker.implement_slice.work_branch` |
| `commit_sha` | `worker.implement_slice.commit_sha` |

### Extracted Secret/Env References (`config.ExtractPlanEnvRefs`)

Sorted deduplicated slice: `["COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "GITHUB_TOKEN"]`.

---

## 2. Installer CLI Flags & Directory Layout (`scripts/install-local-plugins.go`)

### Flags

- `-plugin-dir`: target plugin root (default `~/.sofa/plugins`)
- `-arch`: `"amd64"` or `"arm64"` (empty = auto-detect via Podman, fallback host arch/`"amd64"`)
- `-runtime-bin`: container CLI binary (default `"podman"`)
- `-copilot-linux-bin`: pre-downloaded Linux `copilot` binary path
- `-copilot-src`: Go package or local stub path to cross-compile as `bin/copilot`
- `-cache-dir`: cache directory for downloaded `@github/copilot` binary (default `.sofa/cache`)
- `-skip-antigravity-sdk`: pass `--skip-sdk` to `build_linux_bundle.py`
- `-only`: `""` (both), `"copilot"`, or `"antigravity"`

### Installed Target Tree

```text
<plugin-dir>/
├── sofa-harness-copilot/
│   └── linux_<arch>/
│       ├── sofa-harness-copilot          (ELF regular file, 0755)
│       └── bin/
│           └── copilot                   (ELF regular file, 0755)
└── sofa-harness-antigravity/
    └── linux_<arch>/
        ├── sofa-harness-antigravity      (POSIX sh launcher regular file, 0755)
        └── python/
            ├── bin/python3(.12)
            └── lib/python3.12/site-packages/...
```
