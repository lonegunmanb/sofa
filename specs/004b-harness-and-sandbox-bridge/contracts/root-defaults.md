# Root Defaults Contract (Extended)

```text
InspectRootDefaults(blocks) -> RootDefaults
ResolveDefaultSandbox(consumerAddr) -> (name, error)
ResolveDefaultHarness(consumerAddr) -> (name, error)
ResolveDefaultModelProvider(consumerAddr) -> (*name, error)
```

- `Harnesses` collects `block.Labels[1]` for root blocks (`len(Labels) == 2 && Labels[0] == "" && Type == "harness"`).
- `ResolveDefaultHarness(consumerAddr)`:
  - `0` candidates: error indicating `<consumerAddr>` requires a harness and no root `harness` block is declared.
  - `1` candidate: returns `(d.Harnesses[0], nil)`.
  - `>=2` candidates: ambiguity error naming `<consumerAddr>`, listing all candidate harness names, and instructing to set `harness` explicitly.
- Zero special-case default for `"copilot"` or any other harness name.
