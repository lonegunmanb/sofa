# Data Model: Vue 3 Dashboard & Embedded WebUI (`031-vue3-dashboard`)

## 1. Go Embedded WebUI Model (`internal/webui`)

- `distFS fs.FS`: Embedded `dist` subdirectory (`//go:embed all:dist` stripped via `fs.Sub(embeddedDist, "dist")`) or injected test `fs.FS`.
- Reserved prefixes excluded from UI serving: `/api`, `/api/*`, `/actors`, `/actors/*`, `/dapr`, `/dapr/*`, `/healthz`, `/healthz/*`.
- Static asset extensions returning `404` when missing (instead of `index.html` fallback): `.js`, `.mjs`, `.css`, `.map`, `.png`, `.jpg`, `.jpeg`, `.gif`, `.svg`, `.ico`, `.woff`, `.woff2`, `.ttf`, `.eot`, `.json`, `.wasm`, `.txt`.

## 2. Frontend TypeScript Contracts (`web/src/api/types.ts`)

Matches Go server JSON payloads 1:1:

- `RunStatus`: `'PENDING' | 'RUNNING' | 'WAITING_HUMAN' | 'PAUSING' | 'PAUSED' | 'SUCCEEDED' | 'FAILED' | 'DELETED'`
- `NodeStatus`: `'PENDING' | 'QUEUED_SLOT' | 'RUNNING' | 'WAITING_HUMAN' | 'SUCCEEDED' | 'FALLBACK_TRIGGERED' | 'FAILED' | 'PAUSED'`
- `RunSummary`: `{ run_id, namespace, status, plan_blob_key?, plan_sha256?, error_message?, created_at, updated_at }`
- `NodeRunSummary`: `{ address, canonical_address, kind, module_path?, status, current_epoch, work_branch?, commit_sha?, artifacts?, merged_into_shared_branch?, dynamic_keys?, error_message?, depends_on?, parent_dynamic_addr? }`
- `RunOutputView`: `{ name, description?, sensitive, unknown?, value? }`
- `RunDetailResponse`: `{ run_id, namespace, status, manual_paused, plan_blob_key?, plan_sha256?, error_message?, nodes?, outputs?, created_at, updated_at }`
- `CheckpointRecord`: `{ seq, epoch, branch, initial_sha, final_sha, status, diary: DiaryInput }`
- `DiaryInput`: `{ completed: string[], hypotheses?: string[], decisions?: string[], test_evidence?: string[], open_questions?: string[], next_steps: string[] }`
- `InterjectionRecord`: `{ id, message, wait_for_confirmation, status, created_at, drained_at?, confirmed_at? }`
- `PublishedArtifact`: `{ id, name, type, rel_path, container_path, blob_key, sha256, size_bytes }`
- `NodeDetailResponse`: `{ run_id, node_addr, summary?, current_epoch, status, manual_paused, worker_process_id?, worker_app_id?, last_error?, checkpoints?, pending_interjections?, interjection_log?, decision_cards?, artifacts?, pending_publication?, approval?, outcome? }`
- `LogEvent`: `{ timestamp, run_id, node_addr, epoch, process_id, event_id?, kind, tool_call_id?, tool_name?, text?, error? }`
- `WSEvent`: `{ topic, type?, run_id?, node_addr?, epoch?, timestamp, payload }`
- `DecisionCard`: `{ card_id, run_id, node_addr, epoch, kind: 'request_decision' | 'pause_for_human' | 'human_approval', status: 'PENDING' | 'ANSWERED' | 'INVALIDATED', question?, context?, options?, recommended?, pause_reason?, commit_sha?, artifact_ids?, artifacts?, selected_option?, answer_text?, approved?, returned_upstream?, return_fallback?, invalidated_reason?, created_at, updated_at, answered_at? }`
- `EnvEntry`: `{ name, value, scope, namespace?, updated_at }`
- `SecretMeta`: `{ name, scope, namespace?, updated_at }` (strictly write-only; no `value` property)
