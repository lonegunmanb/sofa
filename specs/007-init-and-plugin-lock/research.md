# Research Notes: Workspace Init, Recursive Module Installation, and Harness Plugin Locking

## 1. `golden.AsHclBlocks` Label Layout

Inspection of `internal/config/loader.go` and `internal/config/loader_test.go` confirms:
- `config.LoadDirectoryContext` delegates block conversion to `golden.AsHclBlocks(syntaxBody.Blocks, writeFile.Body().Blocks())`.
- For single-label blocks (`module "child"` or `harness "copilot"`), `golden.HclBlock.Labels` is `["", "<name>"]` (length 2 with `Labels[0] == ""`).
- Therefore, both `internal/module/init.go` and `internal/harness/lock.go` MUST normalize labels accepting `len(labels) == 1` OR `(len(labels) == 2 && labels[0] == "")`, while rejecting all other label counts.

## 2. Harness `init`-Time vs `plan`-Time Expression Evaluation

Inspection of `docs/hcl-contract.md` and `internal/harness/spec/schema.go` confirms:
- A `harness "<name>"` block may legitimately reference `var.*` or `local.*` inside `options` or `env_refs` (which are resolved at `sofa plan` time).
- However, `source`, `version`, `signature`, and `signing_key` MUST be statically known at `sofa init` time so plugin artifacts can be fetched, verified, and locked before planning.
- Calling `HarnessBlock.BaseDecode` on the whole block during `init` would evaluate `options` / `env_refs` against an empty `EvalContext` and falsely fail on `var.*` references. Inspecting only `source`, `version`, `signature`, and `signing_key` expressions directly (requiring `len(attr.Expr.Variables()) == 0` and evaluating with `&hcl.EvalContext{}`) and then delegating validation to `harnessspec.HarnessConfig.Validate()` solves this cleanly with zero schema duplication.

## 3. Windows File Handle Locking with `os.Root`

- On Windows, holding an unclosed `*os.Root` or `*os.File` inside a staging or backup directory prevents `os.Rename` or `os.RemoveAll` with `ERROR_SHARING_VIOLATION`.
- All `os.OpenRoot`, `sourceRoot.Open`, and `destRoot.OpenFile` calls MUST execute inside helper functions that explicitly `Close()` all file and root handles (and check close errors) before returning to any caller that performs directory renames or cleanup.

## 4. `r42/internal/project` & `r42/internal/module` Alignment

Inspection of `d:\project\r42\internal\project\{project,state}.go` and `d:\project\r42\internal\module\init.go` confirms:
- `.initializing` transaction lock, `.initialized.json` (`format_version = 2`), `state.json` (`format_version = 1`), `SaveOutputs`/`ReadOutputs`, and `remoteSourceDisplay` URL redaction patterns map 1:1 to `sofa` (replacing `.r42` / `r42` with `.sofa` / `sofa`, and skipping `*.sofaplan` alongside `.sofa` and `.git`).
- `r42/internal/module/init.go` used `os.CopyFS` for local modules; `sofa` improves on this by using `os.OpenRoot` with `.sofa`/`.git`/`*.sofaplan` filtering and explicit non-regular file rejection.
