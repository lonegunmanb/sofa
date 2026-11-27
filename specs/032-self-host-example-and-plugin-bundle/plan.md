# Implementation Plan: `copilot + antigravity` Self-Hosting Template & Local Linux Dual-Plugin Installer (`032-self-host-example-and-plugin-bundle`)

**Branch**: `032-self-host-example-and-plugin-bundle` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/032-self-host-example-and-plugin-bundle/spec.md)

## Summary

Deliver Stage 3 Slice `032` in three cohesive parts:
1. **Anti-Rot Tier 1 Tests (`examples/examples_test.go`)**: Written first (Red → Green) to validate `examples/copilot-agy-self-host` compilation, `.sofaplan` archive round-trip, `applyeval.EvalReadyWorker` materialization, `config.ExtractPlanEnvRefs`, and `scripts/install-local-plugins.go` output compatibility with `internal/harness/installer.NewInstaller().Prepare(...)`.
2. **Local Linux Dual-Plugin One-Command Installer (`scripts/install-local-plugins.go`)**: `//go:build ignore` CLI tool reusing `internal/sandbox/runtime.PodmanEngine.DetectPlatform`, `plugins/sofa-harness-copilot/scripts/build-bundle.go` (`-mod=readonly`), and `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py`, with Go stdlib `.tgz` extraction, binary caching, and same-volume atomic directory swapping safe for Windows deep paths.
3. **Self-Hosting Dogfooding Template (`examples/copilot-agy-self-host/`)**: `variables.sofa.hcl`, `main.sofa.hcl`, `outputs.sofa.hcl`, and `README.md` matching the exact Go HCL schemas without re-implementing or circumventing `golden` or `sofa` planning rules.

## Technical Context

- **Language / Runtime**: Go 1.27 (`github.com/lonegunmanb/sofa`), HCL v2 via `golden` + `internal/planner`, Python 3.12 via `uv` for `build_linux_bundle.py`, Node `npm.cmd` (Windows) / `npm` (Unix) or Podman container fallback for `@github/copilot-linux-<x64|arm64>` tarball fetch.
- **Testing**: Tier 1 offline unit tests (`go test ./examples/...`, `go test ./... -count=1`) using `-copilot-src` pointing to a local Go stub package so zero network access or external binaries are required during `go test ./...`.
- **Constraints**:
  - Do not run `git commit` or `git push`.
  - Keep `plugins/sofa-harness-copilot/go.mod` and `go.sum` untouched (`-mod=readonly`).
  - Handle Windows paths (`npm.cmd`, deep `site-packages` paths, same-volume `os.Rename` with backup/retry + extended-length copy fallback).

## Project Structure

```text
examples/
├── examples_test.go
└── copilot-agy-self-host/
    ├── README.md
    ├── main.sofa.hcl
    ├── outputs.sofa.hcl
    └── variables.sofa.hcl
scripts/
└── install-local-plugins.go
specs/032-self-host-example-and-plugin-bundle/
├── contracts/self-host-template-and-installer.md
├── data-model.md
├── plan.md
├── spec.md
└── tasks.md
```

## Complexity Tracking

No constitution exceptions required. All HCL parsing, DAG resolution, archive round-tripping, platform detection, and plugin discovery reuse existing `golden` and `sofa` packages directly.
