# Contract: Typed Tool Executors & Cross-Compiler Cache (`015`)

## 1. `internal/tool/runtime/compiler.go`

- `GenerateGoToolWrapper(source string) string`:
  - Emits `package main` + `_sofa*`-aliased stdlib imports before `source`, and `Issue`, `ToolResponse[T any]`, and `main()` after `source`.
  - `main()` reads strictly one JSON object from `os.Stdin` (`DisallowUnknownFields()`, rejects trailing tokens and non-object JSON), passes a `SIGINT`/`SIGTERM`-cancellable context to `Invoke(ctx, input)`, exits `1` on `Invoke` error or `ToolResponse` protocol violation, and writes JSON to `os.Stdout`.
- `Compiler.Compile(ctx context.Context, spec plan.ToolSpec) (CompiledTool, error)`:
  - Validates `spec.Kind == "go"`, non-empty `spec.ID` and `spec.Source`, and `gotool.ValidateDependency` for each dependency.
  - Computes SHA-256 key over `(targetOS, targetArch, goVersion, wrappedSource, sortedDependencies)` and acquires a per-key mutex.
  - On cache hit (existing regular non-empty binary at `<cacheDir>/<sha256>/<binName>`), returns immediately without running `go get` or `go build`.
  - On miss, builds inside a sibling temp dir (`module sofa.inline.tool`, `CGO_ENABLED=0`, `GOOS=targetOS`, `GOARCH=targetArch`, `GO111MODULE=on`, `GOWORK=off`), chmods `0o755`, and atomically populates `<cacheDir>/<sha256>/<binName>`.
- `StageToolsDir(ctx context.Context, destToolsDir string, compiled []CompiledTool) error`:
  - Places each compiled binary at `<destToolsDir>/<toolID>` with `0o755` permissions (hardlink with copy fallback) matching container mount `"/.sofa/tools/" + toolID`.

## 2. `internal/tool/runtime/executor.go`

- `ContainerExecFunc` & `NewPodmanContainerExec(engine sandboxruntime.Engine, containerName string) ContainerExecFunc`.
- `HostToolSpec(binding ToolBinding) (harnessruntime.HostToolSpec, error)`:
  - Uses `ExposedName` (falling back to `Spec.ID`).
  - Strips top-level keys present in `PresetInput` / `PresetInputJSON` from schema `properties` and `required` (emitting `{"type":"object","properties":{},"additionalProperties":false}` when empty).
- `Executor.Invoke(ctx context.Context, binding ToolBinding, argumentsJSON []byte) (InvocationResult, error)` & `Executor.Handler(binding ToolBinding) harnessruntime.ToolHandler`:
  - Decodes LLM JSON object, forcibly overwrites keys from `PresetInput` / `PresetInputJSON`, and validates/converts against `Spec.InputType` via `toolspec.Constraint`. Invalid LLM input yields `Accepted: false` (`HostToolResultSuccess`, `HandlerError: ""`, issue code `invalid_input`).
  - `go_tool`: executes `/.sofa/tools/<Spec.ID>` in `/workspace` with no secret env; non-zero exit code, invalid single-JSON stdout, `ToolResponse` invariant violation, or `OutputType` mismatch yields `HostToolResultFailure` with non-empty `HandlerError`. Exit `0` with `Accepted == false` yields `HostToolResultSuccess` (`Accepted: false`).
  - `external_tool`: resolves `Env` + `EnvRefs` via `LookupEnv`, executes `Spec.Program` at `sandboxruntime.ResolveContainerWorkDir(Spec.WorkingDir)`, redacts resolved `EnvRefs` secrets from error strings, and validates single-JSON stdout against `Spec.OutputType`.
  - `QuotePaths`: for `Accepted == true` `go_tool` outputs with non-empty `Spec.QuotePaths`, verifies every `{path, exact_quote}` object along `QuotePaths` against `os.OpenRoot(importsDir)` (`/.sofa/imports/...`), `os.OpenRoot(bundleDir)` (`/.sofa/bundle/...`), or `os.OpenRoot(workspaceDir)` (`/workspace/...` or relative path), rejecting missing files, symlinks, `.git`, traversal, non-UTF8 files, empty quotes, or missing substrings as `Accepted: false` (`HostToolResultSuccess`, issue code `quote_verification_failed`).
  - `Postconditions`: evaluates `Spec.Postconditions` with `{"input": validatedInput, "output": validatedOutput}` and `config.Functions(workspaceDir)`; any false/unknown condition downgrades to `Accepted: false` (`HostToolResultSuccess`, issue code `postcondition_failed`).

## 3. `internal/tool/runtime/mcp_resource.go`

- `const MCPResourceToolName = "sofa_read_mcp_resource"`
- `NewMCPResourceReader(allowed []MCPResourceSpec, reader MCPResourceReadFunc) (*MCPResourceTool, bool)`:
  - Returns `(nil, false)` when `len(allowed) == 0`.
  - Enforces exact `(server_name, uri)` membership in `allowed` before invoking `reader` (`Session.ReadMCPResource`).
