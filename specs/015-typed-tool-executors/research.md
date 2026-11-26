# Research: Typed Tool Executors & Cross-Compiler Cache (`015-typed-tool-executors`)

## Decision 1: Wrapper Layout & Import Isolation

- **Decision**: Place `package main` and the wrapper's `_sofa*`-aliased stdlib `import (...)` block immediately before the inline `go_tool` source, and emit `Issue`, `ToolResponse[T]`, and `main()` immediately after the inline source.
- **Rationale**: Go allows multiple `import` declarations in a single `.go` file as long as all `import` declarations precede non-import declarations (`type`, `func`, `var`, `const`). Because `gotool.Analyze` guarantees user `source` contains no `package` clause, no `func main`, and no `Issue`/`ToolResponse` declarations, placing the wrapper's `_sofa*`-aliased imports at the top preserves user `import` blocks verbatim without parsing/rewriting AST imports and avoids any local identifier collisions.
- **Note on `Issue.Path`**: In the standalone `package main` wrapper, `Issue.Path` is `string` (`json:"path,omitempty"`) so inline `go_tool` authors can construct `Issue{Code: "...", Message: "...", Path: "..."}` directly without pointer helpers; on the host side, JSON unmarshals seamlessly into `internal/spec.Issue` (`Path *string`).

## Decision 2: Strict Single-JSON-Object Stdin & `ToolResponse` Protocol Validation

- **Decision**: Inside `main()`, decode `os.Stdin` into `json.RawMessage`, check for trailing tokens (`dec.Decode(&extra) == io.EOF`), verify the trimmed payload begins with `'{'` (rejecting `null`, arrays, and primitives), and decode into `Input` with `DisallowUnknownFields()`. After calling `Invoke(ctx, input)`, reject `err != nil`, `Accepted && (len(Issues) != 0 || Output == nil)`, or `!Accepted && (Output != nil || len(Issues) == 0 || blank Issue.Code/Message)` by writing to `os.Stderr` and exiting `1`.
- **Rationale**: Ensures malformed caller inputs or buggy tool code fail loudly with `HandlerError` (triggering quota rollback in `016`) rather than silently succeeding on zero-valued structs.

## Decision 3: `QuotePaths` Traversal Over `cty.Value` & `os.OpenRoot` Containment

- **Decision**: Traverse `ValidatedOutput` along each path in `plan.ToolSpec.QuotePaths` (`field`, `[]` for list/tuple/set, `*` for map/object) to extract `{path, exact_quote}` objects, resolve container prefixes (`/.sofa/imports/`, `/.sofa/bundle/`, `/workspace/`, or relative workspace paths) while rejecting `..`, `\`, `.git`, symlinks, and non-UTF8 files via `os.OpenRoot`, and downgrade any failure to `Accepted = false` (`HostToolResultSuccess`, issue code `quote_verification_failed`).
- **Rationale**: Reuses Go's `os.OpenRoot` containment (consistent with `internal/sandbox/runtime/podman.go`) to eliminate symlink escapes and ensures hallucinated quotes are returned to the LLM for self-repair rather than treated as container crashes.

## Decision 4: Classification of Model-Repairable Rejections vs Execution Failures

- **Decision**:
  - Container non-zero exit code, spawn/timeout error, invalid tool `stdout` JSON, `ToolResponse` invariant violation, or `OutputType` mismatch -> `HostToolResultFailure` with non-empty redacted `HandlerError` (triggers `tool_call_quota` rollback in `016`).
  - `go_tool` `Accepted == false`, invalid LLM arguments (bad JSON / `InputType` validation error), `QuotePaths` verification failure, or HCL `postcondition` failure -> `HostToolResultSuccess` (`HandlerError == ""`) with `InvocationResult.Accepted == false` and structured `{"accepted":false,"issues":[...]}` text for the LLM.
- **Rationale**: Aligns directly with `docs/stage2/015-typed-tool-executors.md` §3.2 and `docs/stage2/016-session-gates-and-watchdog.md` (`terminate` tool completion checks `InvocationResult.Accepted == true`, while infrastructure failures trigger quota rollback).
