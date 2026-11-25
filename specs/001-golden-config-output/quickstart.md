# Quickstart Validation: Configuration Foundation and Outputs

## Prerequisites

- Go 1.27.3 or a compatible newer toolchain
- Repository root as the current working directory
- No network, container runtime, Git remote, or Dapr sidecar required after dependencies are present

## Install dependencies

```powershell
go mod tidy
```

## Run the focused contract tests

```powershell
go test ./internal/spec ./internal/config ./internal/module/spec ./internal/planner -count=1
```

Expected: all packages pass, and no test waits for interactive input.

## Run the complete fast suite

```powershell
go vet ./...
go test ./... -count=1
```

Expected: both commands exit 0.

## Minimal planning scenario

Create a temporary directory containing:

```hcl
# workflow.sofa.hcl
variable "artifact_name" {
  type      = string
  default   = "report.json"
  sensitive = true
}

locals {
  artifact_path = ".sofa/artifacts/${var.artifact_name}"
}

output "artifact_path" {
  value       = local.artifact_path
  description = "Generated artifact location"
}

output "module_root" {
  value = path.module
}
```

Through the planning test helper:

1. Load the directory.
2. Construct the planner with `DslFullName = "sofa"` and `DslAbbreviation = "sofa"`.
3. Run initialization and plan.
4. Inspect the `artifact_path` and `module_root` outputs.

Expected:

- `artifact_path` evaluates to `.sofa/artifacts/report.json`.
- Its sensitivity is detectable and formatted display redacts the value.
- `module_root` uses `/` separators.
- Planning performs no network or process launch.

## Negative scenarios

| Scenario | Expected result |
|---|---|
| Empty directory | error naming directory and `.sofa.hcl` |
| Top-level `x = 1` | error stating top-level blocks only |
| `variable "x" {}` | error requiring concrete `type` |
| `type = any` | error rejecting dynamic type |
| `one(["a", "b"])` | cardinality error |
| Unknown output attribute | schema diagnostic |
| Locals reference each other cyclically | cycle error |
| Failed variable validation | configured validation message |

Contract details: [configuration-loading.md](./contracts/configuration-loading.md), [planning-functions.md](./contracts/planning-functions.md), and [output-block.md](./contracts/output-block.md).
