# Data Model: Static Workers and Nested Execution Contracts

## `Config` (`internal/worker/spec/schema.go`)

| Field | Type / Default | Rules |
|---|---|---|
| `BasedSource` | `cty.Value` (required) | Non-null `cty.String` (known or unknown); known values must pass `corespec.ValidateBranchName`; unknown triggers `DeferredExpression` |
| `SourceURL` | `string` (derived) | Resolved in `ExecuteDuringPlan()` from `based_source` (`local.*` -> `source.*` / `worker.*`), or single root `source`; ambiguity across `>= 2` root sources fails |
| `Sandbox` | `cty.Value` (required) | Wholly known object with non-empty `address` and `kind == "sandbox"` |
| `Harness` | `cty.Value` (required) | Wholly known object with non-empty `address` and `kind == "harness"` |
| `ModelProvider` | `cty.Value` (optional) | `null` when omitted/explicit `null`; otherwise wholly known object with non-empty `address` and `kind == "model_provider"` |
| `Model` | `string` (required) | Wholly known non-null non-blank `cty.String` |
| `Profile` | `string` (optional) | `""` when omitted/`null`; non-blank wholly known `cty.String` when set |
| `ReasoningEffort` | `string` (optional) | `""` when omitted/`null`; non-blank wholly known `cty.String` when set |
| `SystemPrompt` | `cty.Value` (required) | Non-null `cty.String` (known or unknown); known value must be non-blank; unknown triggers `DeferredExpression` |
| `Prompt` | `cty.Value` (optional) | `cty.NilVal` when omitted/`null`; otherwise `cty.String`; unknown triggers `DeferredExpression` |
| `WritablePaths` | `[]string` (optional) | Known list/tuple/set of non-empty workspace-relative paths without `\`, drive prefixes, `/abs`, or `..` escapes |
| `HumanApproval` | `string` (default `"optional"`) | Known string in `{"optional", "required", "disabled"}` |
| `CheckpointRounds` | `int` (default `5`) | Known whole integer `> 0` |
| `Permission` | `string` (default `"approve_all"`) | Known string `"approve_all"` |
| `MaxProtocolAttempts` | `int` (default `10`) | Known whole integer `> 0` |
| `Timeout` | `*time.Duration` + `TimeoutRaw string` (optional) | Known non-blank duration string parsing to `> 0` |
| `Retry` | `provider.RetryOverride` (optional) | Validated via `provider.MergeRetry(provider.DefaultRetryPolicy(), retry)` |
| `ToolIDs` / `MCPToolIDs` / `MCPResourceIDs` | `[]string` (optional) | Known non-blank string lists; verified registered in current module during `ExecuteDuringPlan()` |
| `TerminateToolID` | `string` (optional) | Non-blank known string; mutually exclusive with `tool_use`; verified registered and not blocked by `allowed_tools`/`disallowed_tools` |
| `ToolCallQuota` | `map[string]int` (optional) | Known map/object of whole integers `>= 0`; non-blank keys; rejects `mcp_tool_*`; `tool_*` keys must be configured in session tools |
| `AllowedTools` / `DisallowedTools` | `[]string` (optional) | `nil` when omitted/`null`; `[]string{}` when `[]`; non-blank elements; must not block terminating tool |
| `SkillDirectories` / `Skills` / `DisabledSkills` | `[]string` (optional) | Known non-blank string lists |
| `ToolUses` | `[]ToolUse` | Unique non-empty labels; unique `tool_id`; `<= 1` `terminate = true`; `input` known-or-unknown object/map validated against registered tool `InputConstraint` |
| `Artifacts` | `map[string]Artifact` | Unique safe labels; unique cleaned paths (no `.` or `..` escapes); `type` in `{"file", "directory"}`; non-blank `description` |
| `Imports` | `map[string]ImportArtifact` | Unique safe labels; non-blank `desc` when set; required non-null list/tuple `sources` of `kind == "artifact"` (never current worker's own artifact); unique cleaned `path` defaulting to `.sofa/imports/<name>` |
| `Gatekeepers` | `[]GatekeeperConfig` | Unique non-empty labels; non-empty `command` list with non-blank `command[0]`; `working_dir` default `"."` (allows `"."`); `timeout` default `"10m"` (`> 0`); whole int `expect_exit_code` (default `0`); `require_clean_worktree` (default `true`) |
| `QC` | `*QCConfig` (0..1) | Non-empty wholly known `criteria` `map(string)`; optional `harness`, `model_provider` (with `ModelProviderSet bool`), `model`, `reasoning_effort`, `permission`, `max_qc_rounds` (default `3`, `> 0`), `tool_ids`, `tool_call_quota`, `allowed_tools`, `disallowed_tools`, `skill_directories`, `skills`, `disabled_skills`, and `retry` (0..1) |
| `Fallbacks` | `[]FallbackConfig` | Unique non-empty labels; non-empty `on` triggers in `{"qc_exhausted", "gatekeeper_exhausted", "human_returned_upstream", "agent_escalated"}` with unique trigger->fallback mapping; `target` traversal `worker.<name>` or `worker.<name>[<key>]` referencing an existing positive ancestor; `max_reworks` (default `2`, `> 0`); `artifacts` list of local `artifact("...")` objects |
| `DeferredExpression` | `string` | Valid parseable HCL object expression containing all non-meta top-level attributes and nested blocks when any allowed Apply-time field contains unknowns |
