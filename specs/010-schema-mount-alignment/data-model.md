# Data Model: Stage 2 Schema & Mount Alignment (`010-schema-mount-alignment`)

## `internal/sandbox/spec`

### `SandboxConfig` (`schema.go`)
- Fields: `Runtime string`, `Image string`, `Network string`, `Mounts []MountSpec`, `Resources *ResourcesSpec`.
- Removed: `Entrypoint []string`.

### `SandboxBlock` (`schema.go`)
- HCL attributes: `runtime`, `image`, `network` (optional). Nested blocks: `mount`, `resources`.
- Removed: `Entrypoint` struct field and `entrypoint` attribute decoding.

## `internal/worker/spec`

### `QCConfig` (`qc.go`)
- Added fields:
  - `Sandbox cty.Value` — defaults to `cty.NullVal(cty.EmptyObject)` when omitted or `null`; otherwise validated `sandbox.<name>` object value (`kind == "sandbox"`).
  - `Profile string` — optional non-blank string profile override.

### Path Constants & Methods (`artifact.go`)
- Constants:
  - `ContainerArtifactsRoot = "/.sofa/artifacts"`
  - `ContainerImportsRoot = "/.sofa/imports"`
- `Artifact`:
  - `Path string` — cleaned relative sub-path (e.g., `"docs/plan.md"`).
  - `(a Artifact) ContainerPath() string` — `path.Join(ContainerArtifactsRoot, a.Path)` (e.g., `"/.sofa/artifacts/docs/plan.md"`).
- `artifactValueType` / `artifactValue`:
  - `"path"`: `cty.StringVal(artifact.ContainerPath())`
  - `"rel_path"`: `cty.StringVal(artifact.Path)`
  - Preserves `"id"`, `"name"`, `"kind"`, `"type"`, `"description"`, `"required"`, `"non_empty"`.
- `ImportArtifact`:
  - `Path string` — cleaned relative sub-path (`path.Clean(rawPath)` when specified, else `Name`).
  - `EffectiveImportRelPath(imported ImportArtifact) string` — `path.Clean(imported.Path)` when non-empty, else `imported.Name`.
  - `(i ImportArtifact) ContainerPath() string` — `path.Join(ContainerImportsRoot, effectiveImportRelPath(i))`.
- `WorkerBlock` `import_artifact.<name>` context object:
  - `"path"`: `cty.StringVal(imp.ContainerPath())`
  - `"desc"`: `cty.StringVal(imp.Desc)`

## `internal/planner/export.go`

- `encodeSandboxConfig`: emits `runtime`, `image`, `network`, `mounts`, `resources` (no `entrypoint`).
- `encodeWorkerConfig`:
  - `qc` object includes `"sandbox"` (`cty.NullVal(cty.EmptyObject)` when nil/omitted/null) and `"profile"` (`cty.StringVal(cfg.QC.Profile)`).
  - `artifacts[name]` includes `"path"` (`art.Path`) and `"container_path"` (`art.ContainerPath()`).
  - `imports[name]` includes `"path"` (`EffectiveImportRelPath(imp)`) and `"container_path"` (`imp.ContainerPath()`).
