# Data Model: Harness Plugin Block and Sandbox Bridge Default

## Sandbox (Updated)

| Entity.field | Rules |
|---|---|
| `Sandbox.network` | `"bridge"` default (when omitted or `null`); `"bridge"`, `"none"`, or `"host"` allowed |

## Harness

| Entity.field | Type | Rules |
|---|---|---|
| `HarnessConfig.Name` | `string` | Block label `<name>` matching `^[A-Za-z0-9][A-Za-z0-9_-]*$` (non-empty, starts with ASCII alphanumeric, no path separators or whitespace); no special-case privilege for `"copilot"` or any other name |
| `HarnessConfig.Source` | `string` | Optional, paired with `Version` (both set or both omitted/`null`); `<host>/<owner>/<repo>`, 3 non-empty segments, valid hostname (optional `:port` in `1..65535`), no scheme `://`, no whitespace/control chars, no `.`/`..`, no `.git` suffix |
| `HarnessConfig.SourceHost` | `string` | Parsed `<host>` from `Source` (empty in local mode) |
| `HarnessConfig.SourceOwner` | `string` | Parsed `<owner>` from `Source` (empty in local mode) |
| `HarnessConfig.SourceRepo` | `string` | Parsed `<repo>` from `Source` (empty in local mode) |
| `HarnessConfig.Version` | `string` | Optional, paired with `Source`; exact SemVer 2.0.0 (`MAJOR.MINOR.PATCH[-pre][+build]`), no leading `"v"`/`"V"`, no range operators |
| `HarnessConfig.Signature` | `SignatureMode` | `"auto"` default (when omitted or `null`); allowed: `"auto"`, `"attestation"`, `"pgp"`, `"none"`; local mode forbids explicit non-`"auto"` |
| `HarnessConfig.SigningKey` | `string` | Required non-blank when `Signature == "pgp"`; forbidden in local mode or when `Signature` is `"attestation"` or `"none"`; `null` treated as unset |
| `HarnessConfig.EnvRefs` | `map[string]string` | Optional (`null` treated as empty); wholly known `map`/`object` of non-null `string` values; keys and values match `^[A-Za-z_][A-Za-z0-9_]*$`; cloned in `HarnessConfig()` |
| `HarnessConfig.Options` | `cty.Value` | Optional (`null` treated as `cty.NilVal`); wholly known `object` or `map` allowing known `null` fields (including untyped HCL `null`) and empty collections while rejecting capsule types; retains `SensitiveMark` |

## HarnessBlock Planned Value (`Value()`)

- Pre-plan: `cty.DynamicVal`
- Post-plan: `cty.ObjectVal` with attributes:
  - `address`: `cty.StringVal("harness.<name>")`
  - `kind`: `cty.StringVal("harness")`
  - `name`: `cty.StringVal("<name>")`
  - `source`: `cty.StringVal(Source)` (preserving sensitivity marks) or `cty.NullVal(cty.String)` (preserving outer marks if any)
  - `version`: `cty.StringVal(Version)` (preserving sensitivity marks) or `cty.NullVal(cty.String)` (preserving outer marks if any)

## RootDefaults (Extended)

| Entity.field | Rules |
|---|---|
| `RootDefaults.Sources` | Ordered root `source` block names |
| `RootDefaults.Sandboxes` | Ordered root `sandbox` block names |
| `RootDefaults.Harnesses` | Ordered root `harness` block names |
| `RootDefaults.ModelProviders` | Ordered root `model_provider` block names |
