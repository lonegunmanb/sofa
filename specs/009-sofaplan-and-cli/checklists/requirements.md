# Specification Quality Checklist: Immutable `Plan` Serialization, `.sofaplan` Self-Contained Archive, and Offline CLI

- [x] User stories and acceptance scenarios cover `internal/plan`, `internal/planner/export.go`, `internal/variableschema`, `internal/cli`, and `cmd/sofa`
- [x] Lossless `cty.Value` codec, `"<sensitive>"` / `"<known after apply>"` display rules, and deterministic `.sofaplan` archive security invariants are specified
- [x] Edge cases (Windows handle closing/ACL, `mcp_server`/`harness` sensitivity preservation, interleaved `-var`/`-var-file`, `skill_directories` validation) are documented
- [x] Upstream `golden` and reference `r42` implementations were inspected prior to planning
- [x] Out-of-scope items (Stage 2 Dapr Actor runtime, `sofa apply`, container execution) are excluded
