# Contract: `preserve` Block

## Syntax

```hcl
preserve "<name>" {
  url    = <known repository URL expression> # optional with exactly one root source
  branch = <known or deferred string>
}
```

The block has exactly one label and supports `precondition` plus the planning engine's `depends_on` metadata.

## Decoding

- URL must evaluate to a known non-null valid string.
- Branch must evaluate to a string type.
- Null and non-string branches fail.
- Known branch strings pass shared branch-name validation.
- Unknown branch strings remain valid and retain their original expression.

## Root URL inference

| Root sources | Preserve URL | Result |
|---|---|---|
| 0 | omitted | error naming preserve |
| 1 | omitted | inject `source.<only>.url` in syntax/write AST |
| 2+ | omitted | ambiguity error listing source names |
| any | explicit | no inference |

The injected traversal must create a real planning dependency.

## Exposed value

```text
{
  address = "preserve.final_implementation"
  kind = "preserve"
  name = "final_implementation"
  url = "<known URL>"
  branch = <known string or unknown string>
  branch_expr = "<original expression>"
}
```
