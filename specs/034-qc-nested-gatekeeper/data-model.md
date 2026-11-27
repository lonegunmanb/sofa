# Data Model: `qc`-Nested `gatekeeper` (`034`)

## 1. `workerspec.Config` and `workerspec.QCConfig` (`internal/worker/spec/`)

### `Config` (`schema.go`)
- Remove `Gatekeepers []GatekeeperConfig` from `Config` and `Config.Clone()`.
- Remove `"gatekeeper"` from `workerNestedBlockTypes`.

### `QCConfig` (`qc.go`)
| Field | Type | Default / Rule |
| :--- | :--- | :--- |
| `Criteria` | `cty.Value` | Optional (`cty.MapValEmpty(cty.String)` when omitted or `null`); when non-null must be non-empty map/object of non-empty string keys and values |
| `MaxQCRounds` | `int` | Optional whole int > 0, default `DefaultMaxQCRounds` (`3`) |
| `MaxGatekeeperReworks` | `int` | Optional whole int >= 0, default `DefaultMaxGatekeeperReworks` (`3`) |
| `MaxGatekeeperReworksSet` | `bool` | `true` when `max_gatekeeper_reworks` was explicitly set to a non-null integer in HCL |
| `Gatekeepers` | `[]GatekeeperConfig` | Repeatable `gatekeeper "<name>"` (including `dynamic "gatekeeper"`), unique names within `qc` |

Validation invariant: `len(Gatekeepers) > 0 || hasCriteria` MUST hold; otherwise decoding `qc` fails.

## 2. Plan & Deferred Expression Representation

### `planner/export.go` (`encodeWorkerConfig`)
- Top-level worker object no longer includes `"gatekeepers"`.
- `qc` object adds:
  - `"max_gatekeeper_reworks"`: `cty.NumberIntVal(int64(cfg.QC.MaxGatekeeperReworks))`
  - `"max_gatekeeper_reworks_set"`: `cty.BoolVal(cfg.QC.MaxGatekeeperReworksSet)`
  - `"gatekeepers"`: `encodeGatekeepers(cfg.QC.Gatekeepers)`

### `worker/spec/deferred.go` (`buildDeferredExpression` / `writeDeferredQC`)
- Top-level worker deferred object no longer emits `gatekeeper = { ... }`.
- `qc` deferred object emits:
  - `max_qc_rounds = 3` (if omitted)
  - `max_gatekeeper_reworks = 3` and `max_gatekeeper_reworks_set = false` (if omitted), or `max_gatekeeper_reworks_set = true` (if present; `null` resolves to unset at decode)
  - `gatekeeper = { "<name>" = { working_dir = ".", timeout = "10m", expect_exit_code = 0, require_clean_worktree = true, ... } }`

### `applyeval/eval.go` (`decodeQCFromPlanVal`, `validateMaterializedWorker`)
- Reads `max_gatekeeper_reworks` + `max_gatekeeper_reworks_set` using `optionalWholeIntVal` (validating `>= 0`).
- Reads `gatekeepers` (list from Plan) OR `gatekeeper` (object/map from deferred expression, preserving planned declaration order via `orderByPlannedNames`).
- Validates materialized `QC.Gatekeepers` via `workerspec.ValidateGatekeeper` and non-empty criteria entries.

## 3. Execution State (`internal/worker/runner/`)

- Resolve `qcSandbox` (`req.Worker.QC.Sandbox`, falling back to `workSandbox`) when `req.Worker.QC != nil`.
- `maxGKReworks`:
  1. If `req.Worker.QC != nil && req.Worker.QC.MaxGatekeeperReworksSet` -> `req.Worker.QC.MaxGatekeeperReworks`
  2. Else if `req.MaxGatekeeperReworks > 0` -> `req.MaxGatekeeperReworks`
  3. Else if `req.Worker.QC != nil && req.Worker.QC.MaxGatekeeperReworks > 0` -> `req.Worker.QC.MaxGatekeeperReworks`
  4. Else -> `DefaultMaxGatekeeperReworks` (`3`)
- Gatekeeper failures increment `gatekeeperReworks`; QC `revise` increments `qcRound` independently; next iteration reruns gatekeepers before LLM QC.
- When `qcCriteria` is empty, `qcLedger` stays `nil` and Step 2d (LLM QC) is skipped.
