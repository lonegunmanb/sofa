# Data Model: Source Repositories and Preserved Branches

## SourceConfig

| Field | Type | Rules |
|---|---|---|
| `url` | string | Required, known, non-empty, valid supported repository address |
| `branch` | string | Required, known, non-empty, safe Git branch name |
| `token_ref` | optional string | Valid environment-variable name |
| `ssh_key_ref` | optional string | Valid environment-variable name |

Validation: `token_ref` and `ssh_key_ref` are mutually exclusive. Their environment values are never read.

## Source

| Field | Type | Rules |
|---|---|---|
| `address` | string | `source.<name>` |
| `kind` | string | Literal `source` |
| `name` | string | Block label |
| `url` | string | Validated source URL |
| `branch` | string | Validated source branch |
| `commit_sha` | string | Unknown during planning; resolved only during execution |

State transition: `declared` → `decoded` → `validated` → `published`.

## BranchRef

| Field | Type | Rules |
|---|---|---|
| `value` | cty value | Must be a string or unknown string |
| `expression` | string | Original HCL expression |

Known values must pass branch-name validation. Unknown values are retained without conversion.

## Preserve

| Field | Type | Rules |
|---|---|---|
| `address` | string | `preserve.<name>` |
| `kind` | string | Literal `preserve` |
| `name` | string | Block label |
| `url` | string | Known valid repository URL |
| `branch` | BranchRef | Required string-valued branch reference |
| `branch_expr` | string | Original branch expression |

State transition: `declared` → optional URL inference → `decoded` → `validated` → `published`.

## RootSourceSet

| Field | Type | Rules |
|---|---|---|
| `sources` | ordered source declarations | Count and names are used only for omitted preserve URL resolution |

Resolution:

- 0 + omitted URL → error naming preserve.
- 1 + omitted URL → inject `source.<name>.url`.
- 2+ + omitted URL → ambiguity error listing source names.
- Explicit URL → no inference.

## Repository URL

Accepted forms:

- `http://`
- `https://`
- `ssh://`
- `file://`
- local filesystem path
- `user@host:path` SCP syntax

Rejected forms:

- whitespace or control characters;
- unsupported scheme;
- HTTP(S) embedded password;
- empty or structurally invalid address.
