# Research: Harness Plugin Block and Sandbox Bridge Default

## Decision 1: Decode Mechanism for `HarnessBlock` (`BaseDecode` + `gohcl` via `golden.Decode`)

- **Decision**: Implement `golden.BaseDecode` on `HarnessBlock` with `*string` fields (`Source`, `Version`, `Signature`, `SigningKey`) and `cty.Value` fields (`EnvRefs`, `Options`), letting `golden.Decode` handle `gohcl.DecodeBody`, unknown attribute/block rejection, and native `for_each` / `depends_on` / `precondition` / `dynamic`.
- **Rationale**: Inspected `golden` (`block.go`, `block_value.go`, `marked_decode_test.go`) and `internal/provider/schema.go`:
  1. `gohcl` silently coerces numbers/booleans into `*string` fields unless `BaseDecode` inspects the raw `cty.Value` type first. Using `BaseDecode` rejects non-string/unknown values for `source`, `version`, `signature`, and `signing_key` cleanly without panicking.
  2. `golden.Decode`'s `markedStringBody` automatically strips marks for `*string` fields and retains them in `decodedMarks`, while `BaseDecode` captures the original marked `cty.Value` for `source` and `version` so `HarnessBlock.Value()` preserves `golden.SensitiveMark` on `source` and `version`.
  3. `gohcl.DecodeBody` preserves all marks (including nested `SensitiveMark`) on `cty.Value` fields (`EnvRefs`, `Options`) directly.
- **Alternatives considered**: Implementing `golden.CustomDecode` (`Decode`), which would require manually duplicating attribute allow-listing and `golden.MetaAttributeNames` / `golden.MetaNestedBlockNames` checks. Rejected per Constitution Principle VI (`golden` already handles schema enforcement and meta-constructs natively).

## Decision 2: Strict Exact SemVer Validation Without Adding Dependencies

- **Decision**: Validate `version` using explicit prefix/range checks (rejecting leading `"v"`/`"V"` with a specific hint and rejecting range operators `>=`, `<=`, `~>`, `^`, `*`, `||`, `!=`, `=`, `<`, `>`, whitespace) followed by the exact SemVer 2.0.0 RE2 pattern (`MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]`).
- **Rationale**: `golang.org/x/mod/semver` requires a `"v"` prefix, and `hashicorp/go-version` accepts leading `"v"` and 2-segment versions (`"1.2"`). Explicit validation enforces exact 3-part SemVer without `"v"` and provides tailored diagnostics without introducing new `go.mod` dependencies.
- **Alternatives considered**: Importing `Masterminds/semver/v3` or `hashicorp/go-version`. Rejected because `go-version` is too permissive and adding a new module dependency is unnecessary.

## Decision 3: `source` `<host>/<owner>/<repo>` Validation

- **Decision**: Reject whitespace/control characters, URL schemes (`://`), query/fragment characters (`?`, `#`), and `.git` suffix; split on `/` into exactly 3 non-empty segments; forbid `.` or `..` segments (and `..` substrings); validate `SourceHost` as a valid hostname (`localhost` or dot-separated RFC-style labels without userinfo or port).
- **Rationale**: Aligns with `tflint`'s `PluginConfig.validate()` (`tflint_config.go:706-714`) and `docs/stage1/004b-harness-and-sandbox-bridge.md` §3.2.
- **Alternatives considered**: Accepting arbitrary 3-part strings without host/segment checks. Rejected because `https://github.com/org/repo` or `github.com/../repo.git` must fail deterministically during offline planning.

## Decision 4: Signature & Signing Key Rules (Local vs GitHub Releases Mode)

- **Decision**:
  - Local mode (`source` and `version` both omitted or `null`): `signature` must be omitted/`null` or `"auto"`, and `signing_key` must be omitted/`null`. Any explicit non-default `signature` (e.g. `"none"`, `"pgp"`, `"attestation"`) or non-null `signing_key` fails validation.
  - GitHub Releases mode (`source` and `version` both set): `signature` defaults to `"auto"` when omitted/`null`, must be in `{"auto", "attestation", "pgp", "none"}`; `"pgp"` requires non-blank `signing_key`; `"attestation"` and `"none"` reject non-null `signing_key` (aligned with `tflint_config.go:721`).
- **Rationale**: Matches `docs/stage1/004b-harness-and-sandbox-bridge.md` §3.2 and `tflint` semantics.

## Decision 5: `env_refs` and `options` Type & Purity Validation

- **Decision**:
  - `env_refs`: if omitted or `null`, treat as empty `map[string]string{}`. Otherwise require wholly known map/object whose every element is a non-null `cty.String` (rejecting implicit boolean/number-to-string coercion) and validate every key and value against `^[A-Za-z_][A-Za-z0-9_]*$`. Never call `os.Getenv` / `os.LookupEnv`.
  - `options`: if omitted or `null`, store `cty.NilVal`. Otherwise unmark deeply to verify `IsWhollyKnown()`, `Type().IsObjectType() || Type().IsMapType()`, and `spec.ValidateType(unmarked.Type())`, while storing the original marked `cty.Value` in `HarnessConfig.Options`.
- **Rationale**: Matches `internal/provider` and `internal/tool/spec` conventions while preventing `cty/convert` from coercing `true` into `"true"` in `env_refs`.

## Decision 6: `sandbox.network` Default `"bridge"`

- **Decision**: Change the default `network` in `internal/sandbox/spec/schema.go` (`ExecuteDuringPlan` and `SandboxConfig`) from `"none"` to `"bridge"`, and update `internal/planner/sandbox_test.go` (preserving explicit `"none"` and `"host"` behavior).
- **Rationale**: Directly implements `docs/stage1/004b-harness-and-sandbox-bridge.md` §3.1 so containerized agent harnesses have outbound network reachability to model APIs by default.
