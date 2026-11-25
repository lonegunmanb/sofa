# Contract: `source` Block

## Syntax

```hcl
source "<name>" {
  url         = "<repository address>"
  branch      = "<source branch>"
  token_ref   = "<optional environment variable>" # optional
  ssh_key_ref = "<optional environment variable>" # optional
}
```

The block has exactly one label and supports the planning engine's standard `precondition` metadata. Collection expansion is delegated to the planning engine.

## Validation

- `url` and `branch` are required known strings.
- Supported URL forms follow [../data-model.md](../data-model.md).
- Branch validation follows the shared `BranchRef` rules.
- Credential references must match `^[A-Za-z_][A-Za-z0-9_]*$`.
- Token and SSH-key references cannot both be present.
- No credential environment variable is read.
- Unknown attributes and non-meta nested blocks fail.

## Exposed value

```text
{
  address = "source.project"
  kind = "source"
  name = "project"
  url = "<validated URL>"
  branch = "<validated branch>"
  commit_sha = <unknown string>
}
```

The value is immutable after planning.
