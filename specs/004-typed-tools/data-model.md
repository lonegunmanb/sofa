# Data Model: Typed Tools

## Condition (`internal/spec/condition.go`)

| Entity.field | Rules |
|---|---|
| `Condition.Expression` | Required non-blank HCL expression string; parses cleanly; variable roots restricted to provided map; must evaluate to `cty.Bool` |
| `Condition.ErrorMessage` | Required non-blank string; returned when condition evaluates to known `false` |

## ToolResponse & Issue (`internal/spec/response.go`)

| Entity.field | Rules |
|---|---|
| `Issue.Code` | Required non-blank string |
| `Issue.Message` | Required non-blank string |
| `Issue.ID` | Optional string (`omitempty`) |
| `Issue.Path` | Optional `*string` (`omitempty`) |
| `Issue.RepairHint` | Optional `*string` (`omitempty`) |
| `ToolResponse.Accepted` | Boolean; if `true`, `Issues` MUST be empty; if `false`, `Output` MUST be nil and `len(Issues) >= 1` |

## GoToolBlock (`internal/tool/spec/schema.go` + `internal/tool/gotool/analyze.go`)

| Entity.field | Rules |
|---|---|
| `GoToolBlock.Description` | Required, wholly known, non-null, non-blank string |
| `GoToolBlock.Source` | Required, wholly known, non-null, non-blank Go snippet (no `package` clause, no `func main`, stdlib or declared-dep imports only, `Input` must be `struct`, `Output` mappable, `Invoke(context.Context, Input) (ToolResponse[Output], error)`) |
| `GoToolBlock.Dependencies` | Optional `list(string)` (explicit `null` treated as empty); each entry valid `module@version` without whitespace, leading `-`, or local path replacements |
| `GoToolBlock.Postconditions` | Zero or more `postcondition` blocks with only `condition` and known non-blank `error_message`; statically verified against `UnknownVal(InputType)` and `UnknownVal(OutputType)` |
| `GoToolBlock.Id` | Deterministic `tool_go_tool_<safe_name>_<uuid>` (`<= 64` chars) from SHA-256 of `"sofa/tool-id/v1\x00go_tool\x00" + canonicalAddress` |

## ExternalToolBlock (`internal/tool/spec/schema.go` + `internal/tool/spec/constraint.go`)

| Entity.field | Rules |
|---|---|
| `ExternalToolBlock.Description` | Required, wholly known, non-null, non-blank string |
| `ExternalToolBlock.Program` | Required, wholly known, non-null `list(string)`, non-empty, no null elements, `program[0]` non-blank; host executable existence NOT checked at Plan time |
| `ExternalToolBlock.WorkingDir` | Optional known string; explicit `null` treated as `""` |
| `ExternalToolBlock.InputConstraint` | Required `typeexpr` constraint passing `spec.ValidateType`; top-level `Type().IsObjectType()` MUST be true |
| `ExternalToolBlock.OutputConstraint` | Required `typeexpr` constraint passing `spec.ValidateType` |
| `ExternalToolBlock.Postconditions` | Zero or more `postcondition` blocks statically verified against `UnknownVal(inputType)` and `UnknownVal(outputType)` |
| `ExternalToolBlock.Id` | Deterministic `tool_external_tool_<safe_name>_<uuid>` (`<= 64` chars) from SHA-256 of `"sofa/tool-id/v1\x00external_tool\x00" + canonicalAddress` |
