# Root Defaults Contract

```text
InspectRootDefaults(blocks) -> RootDefaults
ResolveDefaultSandbox(consumerAddr) -> (name, error)
ResolveDefaultModelProvider(consumerAddr) -> (*name, error)
```

- Sandbox: 0 or 2+ candidates error; 1 returns name.
- Provider: 0 returns nil; 1 returns name; 2+ returns ambiguity error with names.
- Resolvers never read environment, start processes, or mutate blocks.
