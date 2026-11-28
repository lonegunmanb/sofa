# Data Model: Auto-Injected Runtime & Protocol System Prompt (`041-auto-runtime-system-prompt`)

No persisted state, HCL schema, or Actor state changes. The slice introduces two in-memory value inputs and a small path-layout value used only while rendering prompts.

## `workRuntimeContractInput`

| Field            | Type                                  | Source in `WorkerRunner`                                         | Notes |
|------------------|---------------------------------------|------------------------------------------------------------------|-------|
| `SandboxRuntime` | `string`                              | `workSandbox.Runtime`                                            | `"host"` selects host layout; any other value selects container layout. |
| `WorkingDir`     | `string`                              | `sandboxruntime.DefaultContainerWorkingDir` or `workspaceDir`    | Host mode passes the host workspace directory. |
| `WritablePaths`  | `[]string`                            | `req.Worker.WritablePaths`                                       | `nil`/empty means no declared glob restriction. |
| `Artifacts`      | `map[string]workerspec.Artifact`      | `req.Worker.Artifacts`                                           | Rendered sorted by `Name`. |
| `Imports`        | `map[string]workerspec.ImportArtifact`| `req.Worker.Imports`                                             | Rendered sorted by `Name`. |
| `TerminateTool`  | `string`                              | terminate binding `EffectiveName()` or `sofa_complete_work`      | Custom `tool_use` alias wins. |
| `Fallbacks`      | `[]workerspec.FallbackConfig`         | `req.Worker.Fallbacks`                                           | Only `agent_escalated` targets are surfaced. |

## `qcRuntimeContractInput`

| Field            | Type                             | Source in `runQCRound`                                           | Notes |
|------------------|----------------------------------|------------------------------------------------------------------|-------|
| `SandboxRuntime` | `string`                         | `qcSandbox.Runtime`                                              | Selects host vs container layout. |
| `WorkingDir`     | `string`                         | `sandboxruntime.DefaultContainerWorkingDir` or `qc-workspace-r<n>` | Host mode passes the QC workspace directory. |
| `Artifacts`      | `map[string]workerspec.Artifact` | `req.Worker.Artifacts`                                           | Rendered sorted by `Name`. |

## `promptPathLayout`

| Field           | Container                      | Host                      |
|-----------------|--------------------------------|---------------------------|
| `ArtifactsRoot` | `/.sofa/artifacts`             | `.sofa/artifacts`         |
| `ImportsRoot`   | `/.sofa/imports`               | `.sofa/imports`           |

## Rendering Invariants

- Deterministic output: sorted artifacts and imports, stable bullet order.
- Omitted sections: no imports ⇒ no import block; no artifacts ⇒ no artifact block; no `agent_escalated` fallbacks ⇒ no escalate line.
- Path joining uses `path.Join` (slash semantics) so host-relative prompts stay POSIX-style and match `workerspec.Artifact.ContainerPath()` semantics.
- The contract is appended after the user `system_prompt` and any recovery diary; it never replaces user content.