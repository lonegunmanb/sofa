export type RunStatus =
  | 'PENDING'
  | 'RUNNING'
  | 'WAITING_HUMAN'
  | 'PAUSING'
  | 'PAUSED'
  | 'SUCCEEDED'
  | 'FAILED'
  | 'DELETED'

export type NodeStatus =
  | 'PENDING'
  | 'QUEUED_SLOT'
  | 'RUNNING'
  | 'WAITING_HUMAN'
  | 'SUCCEEDED'
  | 'FALLBACK_TRIGGERED'
  | 'FAILED'
  | 'PAUSED'

export type DecisionCardKind = 'request_decision' | 'pause_for_human' | 'human_approval'
export type DecisionCardStatus = 'PENDING' | 'ANSWERED' | 'INVALIDATED'
export type ConfigScope = 'global' | 'namespace'

export interface RunSummary {
  run_id: string
  namespace: string
  status: RunStatus
  plan_blob_key?: string
  plan_sha256?: string
  error_message?: string
  created_at: string
  updated_at: string
}

export interface PublishedArtifact {
  id: string
  name: string
  type: string
  rel_path: string
  container_path: string
  blob_key: string
  sha256: string
  size_bytes: number
}

export interface NodeRunSummary {
  address: string
  canonical_address: string
  kind: string
  module_path?: string
  status: NodeStatus
  current_epoch: number
  work_branch?: string
  commit_sha?: string
  artifacts?: PublishedArtifact[]
  merged_into_shared_branch?: boolean
  dynamic_keys?: string[]
  error_message?: string
  depends_on?: string[]
  parent_dynamic_addr?: string
}

export interface RunOutputView {
  name: string
  description?: string
  sensitive: boolean
  unknown?: boolean
  value?: unknown
}

export interface RunDetailResponse {
  run_id: string
  namespace: string
  status: RunStatus
  manual_paused: boolean
  plan_blob_key?: string
  plan_sha256?: string
  error_message?: string
  nodes?: NodeRunSummary[]
  outputs?: Record<string, RunOutputView>
  created_at: string
  updated_at: string
}

export interface DiaryInput {
  completed: string[]
  hypotheses?: string[]
  decisions?: string[]
  test_evidence?: string[]
  open_questions?: string[]
  next_steps: string[]
}

export interface CheckpointRecord {
  seq: number
  epoch: number
  branch: string
  initial_sha: string
  final_sha: string
  status: 'PENDING' | 'COMPLETE' | string
  diary: DiaryInput
}

export interface HumanInterjection {
  id: string
  message: string
  wait_for_confirmation: boolean
}

export interface InterjectionRecord {
  id: string
  message: string
  wait_for_confirmation: boolean
  status: string
  created_at: string
  drained_at?: string
  confirmed_at?: string
}

export interface DecisionCard {
  card_id: string
  run_id: string
  node_addr: string
  epoch: number
  kind: DecisionCardKind
  status: DecisionCardStatus
  question?: string
  context?: string
  options?: string[]
  recommended?: string
  pause_reason?: string
  commit_sha?: string
  artifact_ids?: string[]
  artifacts?: PublishedArtifact[]
  selected_option?: string
  answer_text?: string
  approved?: boolean
  returned_upstream?: boolean
  return_fallback?: string
  invalidated_reason?: string
  created_at: string
  updated_at: string
  answered_at?: string
}

export interface PendingPublication {
  commit_sha: string
  base_sha?: string
  artifact_ids?: string[]
  artifacts?: PublishedArtifact[]
}

export interface ApprovalRecord {
  approved: boolean
  returned_upstream: boolean
  return_fallback?: string
  commit_sha: string
  artifact_ids?: string[]
}

export interface NodeOutcome {
  status: string
  work_branch?: string
  commit_sha?: string
  artifacts?: PublishedArtifact[]
  fallback_name?: string
  fallback_target?: string
  fallback_reason?: string
  pause_reason?: string
  failure_reason?: string
}

export interface NodeDetailResponse {
  run_id: string
  node_addr: string
  summary?: NodeRunSummary
  current_epoch: number
  status: NodeStatus
  manual_paused: boolean
  worker_process_id?: string
  worker_app_id?: string
  last_error?: string
  checkpoints?: CheckpointRecord[]
  pending_interjections?: HumanInterjection[]
  interjection_log?: InterjectionRecord[]
  decision_cards?: DecisionCard[]
  artifacts?: PublishedArtifact[]
  pending_publication?: PendingPublication
  approval?: ApprovalRecord
  outcome?: NodeOutcome
}

export interface LogEvent {
  timestamp: string
  run_id: string
  node_addr: string
  epoch: number
  process_id: string
  event_id?: string
  kind: string
  tool_call_id?: string
  tool_name?: string
  text?: string
  error?: string
}

export interface WSEvent<T = Record<string, unknown>> {
  topic: string
  type?: string
  run_id?: string
  node_addr?: string
  epoch?: number
  timestamp: string
  payload: T
}

export interface EnvEntry {
  name: string
  value: string
  scope: ConfigScope
  namespace?: string
  updated_at: string
}

export interface SecretMeta {
  name: string
  scope: ConfigScope
  namespace?: string
  updated_at: string
}

export interface RespondDecisionPayload {
  run_id?: string
  node_addr?: string
  selected_option?: string
  answer_text?: string
  approved?: boolean
  returned_upstream?: boolean
  return_fallback?: string
  commit_sha?: string
  artifact_ids?: string[]
}
