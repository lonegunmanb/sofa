# Phase 0 Research: Source Repositories and Preserved Branches

## Decision: Inject defaults before block adaptation through a generic config hook

**Decision**: `config.LoadDirectoryContext` parses a module, invokes a package-level root-defaults hook if registered, and only then calls `golden.AsHclBlocks`. The hook is installed by `planner.init()`.

**Rationale**: `golden.HclBlock` exposes the syntax block but keeps its write block private. Golden's DAG walker needs the syntax traversal, while expression-token preservation and the blueprint require the matching hclwrite body. Mutating both raw bodies before adaptation is the only clean public-API route. Keeping only the hook transport in `config` preserves the package dependency direction.

**Alternatives considered**:

- Put source semantics directly in `config`: rejected because it pollutes the low-level package with business rules.
- Mutate adapted golden blocks: rejected because their write representation is private and the syntax-only workaround would not satisfy the frozen dual-AST contract.
- Rebuild the entire loader in planner: rejected as duplicated HCL parsing and poor package boundaries.

## Decision: Use exact raw-expression type checks before gohcl conversion

**Decision**: Validate source attributes and preserve fields from `Body.Attributes` and cty values before relying on Go struct decoding.

**Rationale**: gohcl safe conversions can turn numbers into strings. The contract requires exact string types and must reject deferred non-string branches.

**Alternatives considered**:

- Rely on struct decoding diagnostics: rejected because conversion is too permissive.

## Decision: Model deferred branches with `BranchRef`

**Decision**: Store the evaluated cty value and original expression string together.

**Rationale**: Known strings can be validated immediately, while unknown string values must remain legal and traceable for later plan serialization.

**Alternatives considered**:

- Decode branch directly into `string`: rejected because unknown traversal values cannot be decoded.
- Store only the expression string: rejected because static type checking would require reparsing later.

## Decision: Use defensive URL and branch validators

**Decision**: Use `net/url` for HTTP(S), SSH, and file schemes, explicit regex-like checks for SCP syntax, safe-filepath checks for local paths, and shared branch/environment-name validators.

**Rationale**: Validation must be deterministic and offline. Local paths need syntactic validation only.

**Alternatives considered**:

- Invoke `git check-ref-format` or `git ls-remote`: prohibited in Stage 1.

## Upstream inspection notes

- `golden.AsHclBlocks` depends on prior custom block registration for one-label blocks.
- `golden.CustomDecode` bypasses dynamic expansion but `BaseBlock` has already captured `precondition` metadata.
- Golden DAG traversal walks syntax expressions and automatically creates edges from scope traversals.
- Golden's HclBlock write representation is private, motivating pre-adaptation mutation.

## Unresolved clarifications

None.
