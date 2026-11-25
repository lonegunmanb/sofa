# Contract: Configuration Directory Loading

## Input

```text
LoadDirectoryContext(context.Context, directory string) -> Loaded, Diagnostics, error
```

`directory` may be absolute or relative. It is resolved to an absolute, clean filesystem path before scanning.

## Discovery rules

1. The target MUST exist and MUST be a directory.
2. Only direct children are inspected.
3. Directories and non-regular entries are ignored.
4. A file is accepted if and only if its name ends with `.sofa.hcl`.
5. Accepted files are ordered by filename, ascending using deterministic lexical ordering.
6. If no accepted file exists, the loader returns an error containing:
   - the resolved directory, and
   - the text `no .sofa.hcl files found`.

## Parsing rules

For each accepted file:

1. Read the complete file once.
2. Parse the bytes as HCL syntax and as HCL write representation.
3. Return all syntax/write diagnostics with their source locations.
4. If top-level bare attributes exist, append an error diagnostic stating that only top-level blocks are allowed.
5. Do not derive planning blocks for a file with syntax errors.
6. For a valid file, derive blocks with the planning engine's adapter.

## Output

`Loaded.Files` preserves the accepted filename order and original bytes.

`Loaded.Blocks` appends declarations in the same file order. Blocks from a single file preserve the parser's declaration order.

## Failure categories

| Condition | Result |
|---|---|
| path missing | error naming path |
| path is a file | error naming path |
| no matching files | error naming directory and suffix |
| unreadable matching file | error naming file |
| malformed HCL | non-empty error diagnostics |
| top-level bare attribute | explicit error diagnostic |
| unsupported block | planning-time error naming block type/name |
| malformed labels | planning-time error explaining expected syntax |

## Determinism

Given identical file names and contents, repeated loads MUST produce the same file order, declaration order, and diagnostics order.
