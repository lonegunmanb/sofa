# Phase 0 Research: Source Repositories and Preserved Branches

## Decision: Inject defaults from `SofaConfig.Init` using fresh golden blocks

**Decision**: `config.LoadDirectoryContext` remains a pure loader. `SofaConfig.Init` resolves the single-source default before `golden.InitConfig` and rebuilds each affected preserve block.

**Rationale**: `golden.HclBlock` exposes cloned syntax blocks and attribute expression tokens. A fresh hclwrite block can be assembled without private state, then passed with a cloned syntax body to `golden.NewHclBlock`. This creates the dependency edge while leaving loaded blocks unchanged. It also prevents `sofa schema`, child modules, and loader syntax diagnostics from being affected by Root-only defaults.

**Alternatives considered**:

- Put source semantics or a global hook in `config`: rejected because schema/child-module loading must not trigger Root singleton errors and `config` must not own business rules.
- Mutate adapted golden blocks in place: rejected because their write representation is private and originals must remain untouched.
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
- Golden exposes `CloneHclSyntaxBlock`, `HclAttribute.ExprTokens`, and `NewHclBlock`, enabling fresh block reconstruction after loading.

## Unresolved clarifications

None.
