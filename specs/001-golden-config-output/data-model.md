# Data Model: Configuration Foundation and Outputs

## ConfigurationDirectory

Represents one directory inspected by the loader.

| Field | Type | Rules |
|---|---|---|
| `path` | absolute path string | MUST exist and be a directory |
| `files` | ordered list of ConfigurationFile | One level only; suffix exactly `.sofa.hcl`; regular files |

Relationships: contains zero or more matching files; zero matching files is an error.

## ConfigurationFile

Represents one source file that contributes declarations.

| Field | Type | Rules |
|---|---|---|
| `path` | path string | Exact source path used in diagnostics |
| `source` | bytes | Complete original file content |
| `declarations` | ordered list of ConfigurationDeclaration | Derived from the same source bytes |

Validation:

- Filename ordering is lexical and stable.
- Syntax and write ASTs must represent the same source.
- A file may be empty but still participates in ordering.

## ConfigurationDeclaration

Represents a named or structural declaration in the combined configuration scope.

| Field | Type | Rules |
|---|---|---|
| `type` | string | Must be a registered top-level type |
| `labels` | string list | Must match the registered block's label contract |
| `address` | stable string | Used for dependency detection and duplicate reporting |
| `source_location` | file and range | Must identify the originating declaration |

Validation:

- Top-level attributes are prohibited.
- Unsupported nested attributes/blocks are prohibited by their owning model.
- Duplicate addresses produce an error rather than last-write-wins behavior.

## Variable

Represents a typed planning input.

| Field | Type | Rules |
|---|---|---|
| `name` | string | Unique declaration name |
| `type` | concrete cty type | Required; not Nil, DynamicPseudoType, or capsule |
| `description` | optional string | If present, must be a known string |
| `default` | optional value | Must convert to the declared type |
| `sensitive` | boolean | Defaults false |
| `validations` | list of validation rules | Every condition must pass |

Value source precedence, lowest to highest:

1. Declared default
2. `SOFA_VAR_<name>` environment value
3. `sofa.sofavars`
4. `sofa.sofavars.json`
5. lexically ordered `*.auto.sofavars`
6. explicit command-line `-var` and `-var-file` assignments in supplied order

State transition during planning:

`declared` → `typed` → `value selected` → `defaults applied` → `converted` → `validated` → `evaluable`.

An error at any transition stops planning. A sensitive variable additionally transitions to `marked sensitive` after value selection.

## LocalValue

Represents a named derived expression.

| Field | Type | Rules |
|---|---|---|
| `name` | string | Unique local name |
| `value` | cty value | May be unknown while planning |
| `dependencies` | declaration references | Used only by the planning engine for ordering |

Validation: dependency cycles fail planning.

## PlanningContext

Represents the evaluation namespace.

| Field | Type | Rules |
|---|---|---|
| `path.module` | string | Absolute module directory with `/` separators |
| `functions` | function map | Includes upstream functions and sofa helper overrides |
| `scope values` | object map | Variables, locals, and registered block values |

## Output

Represents a named planning result.

| Field | Type | Rules |
|---|---|---|
| `name` | string | Unique non-empty block label |
| `value` | cty value | Required; may be wholly or partly unknown |
| `description` | optional string | If present, known and non-empty |
| `sensitive` | boolean | Defaults false; true marks the planned value |

Export snapshot:

```text
Output {
  name: string
  value: any
  description?: string
  sensitive?: boolean
}
```

Validation:

- Unknown attributes are errors.
- Unknown nested blocks other than planning-engine metadata are errors.
- Description must be a known string.
- Sensitivity must be a known boolean.

## PlanSnapshot

Represents the stable, inspectable result of a successful plan.

| Field | Type | Rules |
|---|---|---|
| `variables` | named values | Declared variables after evaluation |
| `locals` | named values | Derived values after dependency evaluation |
| `outputs` | named Output snapshots | Stable names and values |

The snapshot preserves cty sensitivity marks. JSON representation and archive packaging are deferred to slice 007.

## Function Result Contracts

| Function | Input | Success result | Failure |
|---|---|---|---|
| `cwd()` | none | absolute process working directory with `/` | none under normal process context |
| `one(collection)` | a collection/null | null for empty; sole element for singleton | null input, non-collection, or more than one element |
| `local_timestamp()` | none | current local time formatted as RFC3339Nano | none under normal process context |
| `jsondecodewithtype(json, type)` | string JSON and type expression | typed decoded value | malformed JSON or invalid type expression |

Unknown handling:

- `one(unknown collection)` returns an unknown value of the element type when that type is inferable.
- `jsondecodewithtype(unknown string, known type)` returns an unknown value of exactly that type.
