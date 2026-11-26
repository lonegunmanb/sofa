# Research: Typed Tools (`go_tool` and `external_tool`)

## Decision 1: Pure Offline Go AST Analysis vs `go/types` / `packages.Load`

- **Decision**: Use standard library `go/scanner`, `go/parser`, `go/ast`, and `go/build` (`build.FindOnly` for stdlib) directly on `"package gotoolwrap\n" + source` without calling `go get`, `packages.Load`, or `go/types`.
- **Rationale**: `docs/stage1/004-typed-tools.md` explicitly mandates pure offline Plan-time analysis (`使用标准库 go/parser + go/ast 分析内联 Go 片段`, zero `go get`). Using `go/types` with `importer.Default()` fails whenever `dependencies` declares an uncached third-party module such as `github.com/google/uuid@v1.6.0`. Pure AST analysis handles all type-mapping rules (`Input` struct, `Output`, `Invoke(context.Context, Input) (ToolResponse[Output], error)`, local defined types, cycle detection, JSON tags, pointer optionality, `[]byte` / `chan` / `func` / imported named type rejection) deterministically with zero network or subprocess execution.
- **Alternatives considered**: Running `go/types` when `dependencies` is empty and AST fallback when non-empty. Rejected because maintaining two parallel type analyzers duplicates logic and risks behavioral drift between dependency and dependency-free tools.

## Decision 2: `dependencies` Validation Rules

- **Decision**: Validate each dependency string offline against `module@version` rules: non-empty, no whitespace/control characters, no leading `-`, no local relative/absolute path tokens (`./`, `../`, `/`, `\`, `=>`), exactly one `@` separating a valid module path and non-empty version starting with `v` (or valid semver-like token without `/` or `-` prefix).
- **Rationale**: Prevents command-line flag injection (e.g. `"-modfile=x"`) and local path replacement tricks before Stage 2/3 container builds, without requiring network access.

## Decision 3: `tool_name()` Decoupling in `internal/config`

- **Decision**: Implement `tool_name()` in `internal/config/functions.go` using structural `cty.Value` inspection (`address`, `kind` in `{"go", "external", "builtin"}`, and optional `id` string) without importing `internal/tool/spec`.
- **Rationale**: Preserves `sofa`'s package layering rule from `001`–`003` where `internal/config` never imports business block packages.

## Decision 4: `golden` CustomDecode, Meta Capabilities, and `dynamic "postcondition"`

- **Decision**: Filter attributes/nested blocks using `golden.MetaAttributeNames.Contains` and `golden.MetaNestedBlockNames.Contains`, validate `dynamic` block labels so only `dynamic "postcondition"` is allowed, and expand `dynamic "postcondition"` directly in `decodePostconditions` (evaluating `for_each` and `error_message` with `postcondition = { key, value }` while evaluating `condition` via `Condition.Evaluate` with `UnknownVal` `input`/`output` placeholders).
- **Rationale**: `golden.HclBlock.ExpandDynamicBlocks` eagerly evaluates all nested block attributes against the parent `EvalContext`, which fails on `postcondition.condition` because `input` and `output` are scoped only to `Condition.Evaluate`. Expanding `dynamic "postcondition"` locally preserves full `for_each`/`depends_on`/`precondition`/`dynamic` semantics while rejecting invalid `dynamic "<other>"` blocks.
