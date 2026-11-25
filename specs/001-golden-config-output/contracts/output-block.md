# Contract: `output` Block

## Syntax

```hcl
output "<name>" {
  value       = <expression>
  description = "<optional string>" # optional
  sensitive   = true                # optional, default false
}
```

Names and attributes use `lower_snake_case`. The block has exactly one user label.

## Schema

| Attribute | Required | Type | Rules |
|---|---|---|---|
| `value` | yes | any cty value | May be unknown |
| `description` | no | string | Known and non-empty |
| `sensitive` | no | bool | Known; defaults false |

Only the planning engine's standard `precondition` and `dynamic` nested metadata is accepted. Other nested blocks are errors.

## Evaluation

1. Validate the block label and schema.
2. Evaluate `value` in the planning context.
3. If `sensitive` is true, mark the evaluated value with `golden.SensitiveMark`.
4. Publish `output.<name>` as a single value.
5. Mark the block ready for downstream evaluation only after publication.

An unknown value is valid and remains unknown with its static type.

## Snapshot

```json
{
  "name": "artifact_path",
  "value": ".sofa/artifacts/report.json",
  "description": "Where the report was written",
  "sensitive": false
}
```

For a sensitive output, formatted display MUST show a redacted value such as `<sensitive>`. Raw cty inspection may retain the marked value for downstream planning.

## Diagnostics

- Missing `value`: schema error naming `value`.
- Unknown attribute: schema error naming the attribute.
- Unsupported nested block: schema error naming the nested block.
- Invalid `description`: type error naming the attribute.
- Invalid `sensitive`: type error naming the attribute.
- Value evaluation error: propagated with source location.
- Dependency cycle: planning error identifying the cycle/dependency condition without recursion failure.
