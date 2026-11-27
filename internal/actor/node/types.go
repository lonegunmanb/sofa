package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/session"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

const (
	// ActorType is the fixed Dapr Actor type name for per-node authoritative state machines.
	ActorType = "NodeActor"
	// DefaultSlotRetryInterval is the default reminder retry period when worker slots are saturated.
	DefaultSlotRetryInterval = 1 * time.Second
	// DefaultSlotRetryReminderName is the persistent Dapr reminder name for QUEUED_SLOT dispatch retries.
	DefaultSlotRetryReminderName = "node-slot-retry"
)

const (
	errCodeStaleEpoch              = "STALE_EPOCH"
	errCodeInvalidBaseSHA          = "INVALID_BASE_SHA"
	errCodeProtectedSourceBranch   = "PROTECTED_SOURCE_BRANCH"
	errCodeBranchOwnershipConflict = "BRANCH_OWNERSHIP_CONFLICT"
	errCodeBlobDigestMismatch      = "BLOB_DIGEST_MISMATCH"
	errCodeBlobInvalidKey          = "BLOB_INVALID_KEY"
	errCodeInvalidInput            = "INVALID_INPUT"
	errCodeNotFound                = "NOT_FOUND"
	errCodeConflict                = "CONFLICT"
)

var (
	// ErrInvalidActorID indicates a malformed NodeActor ID that cannot be decoded losslessly.
	ErrInvalidActorID = errors.New("node: invalid node actor id")
	// ErrInvalidDecision indicates an invalid, stale, or version-mismatched SubmitDecision call.
	ErrInvalidDecision = errors.New("node: invalid human decision submission")
)

// EncodeNodeActorID encodes (runID, canonicalNodeAddr) into a URL-safe, lossless Dapr Actor ID
// containing strictly [A-Za-z0-9_-] characters.
func EncodeNodeActorID(runID, canonicalNodeAddr string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", fmt.Errorf("%w: run_id is required", ErrInvalidActorID)
	}
	if strings.TrimSpace(canonicalNodeAddr) == "" {
		return "", fmt.Errorf("%w: canonical_node_addr is required", ErrInvalidActorID)
	}
	payload := strconv.Itoa(len(runID)) + ":" + runID + canonicalNodeAddr
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	prefix := "na-" + sanitizeActorHint(runID, "run") + "-" + sanitizeActorHint(canonicalNodeAddr, "node")
	return prefix + "__" + encoded, nil
}

// DecodeNodeActorID decodes a URL-safe NodeActor ID back into the exact original (runID, canonicalNodeAddr).
func DecodeNodeActorID(actorID string) (string, string, error) {
	idx := strings.Index(actorID, "__")
	if idx <= 0 || idx+2 >= len(actorID) {
		return "", "", fmt.Errorf("%w: missing encoded payload separator", ErrInvalidActorID)
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(actorID[idx+2:])
	if err != nil {
		return "", "", fmt.Errorf("%w: decode base64 payload: %w", ErrInvalidActorID, err)
	}
	payload := string(rawPayload)
	colonIdx := strings.IndexByte(payload, ':')
	if colonIdx <= 0 {
		return "", "", fmt.Errorf("%w: missing run_id length prefix", ErrInvalidActorID)
	}
	runLen, err := strconv.Atoi(payload[:colonIdx])
	if err != nil || runLen <= 0 {
		return "", "", fmt.Errorf("%w: invalid run_id length prefix", ErrInvalidActorID)
	}
	rest := payload[colonIdx+1:]
	if len(rest) <= runLen {
		return "", "", fmt.Errorf("%w: truncated run_id/node_addr payload", ErrInvalidActorID)
	}
	runID := rest[:runLen]
	nodeAddr := rest[runLen:]
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(nodeAddr) == "" {
		return "", "", fmt.Errorf("%w: empty decoded run_id or node_addr", ErrInvalidActorID)
	}
	return runID, nodeAddr, nil
}

func sanitizeActorHint(raw, fallback string) string {
	var b strings.Builder
	prevDash := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 && b.Len() < 24 {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 24 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fallback
	}
	return out
}

// NodeStatus represents the authoritative lifecycle state of a NodeActor.
type NodeStatus string

const (
	StatusPending           NodeStatus = "PENDING"
	StatusQueuedSlot        NodeStatus = "QUEUED_SLOT"
	StatusRunning           NodeStatus = "RUNNING"
	StatusWaitingHuman      NodeStatus = "WAITING_HUMAN"
	StatusSucceeded         NodeStatus = "SUCCEEDED"
	StatusFallbackTriggered NodeStatus = "FALLBACK_TRIGGERED"
	StatusFailed            NodeStatus = "FAILED"
	StatusPaused            NodeStatus = "PAUSED"
)

// DecisionCardKind classifies the source of a human pause requiring operator response.
type DecisionCardKind string

const (
	DecisionKindRequestDecision DecisionCardKind = "request_decision"
	DecisionKindPauseForHuman   DecisionCardKind = "pause_for_human"
	DecisionKindHumanApproval   DecisionCardKind = "human_approval"
)

// DecisionCardStatus represents the lifecycle state of a DecisionCard.
type DecisionCardStatus string

const (
	DecisionStatusPending     DecisionCardStatus = "PENDING"
	DecisionStatusAnswered    DecisionCardStatus = "ANSWERED"
	DecisionStatusInvalidated DecisionCardStatus = "INVALIDATED"
)

// DecisionCard represents an actionable human decision or version-bound approval card.
type DecisionCard struct {
	CardID            string                       `json:"card_id"`
	RunID             string                       `json:"run_id"`
	NodeAddr          string                       `json:"node_addr"`
	Epoch             uint64                       `json:"epoch"`
	Kind              DecisionCardKind             `json:"kind"`
	Status            DecisionCardStatus           `json:"status"`
	Question          string                       `json:"question,omitempty"`
	Context           string                       `json:"context,omitempty"`
	Options           []string                     `json:"options,omitempty"`
	Recommended       string                       `json:"recommended,omitempty"`
	PauseReason       string                       `json:"pause_reason,omitempty"`
	CommitSHA         string                       `json:"commit_sha,omitempty"`
	ArtifactIDs       []string                     `json:"artifact_ids,omitempty"`
	Artifacts         []artifact.PublishedArtifact `json:"artifacts,omitempty"`
	ResultJSON        json.RawMessage              `json:"result_json,omitempty"`
	SelectedOption    string                       `json:"selected_option,omitempty"`
	AnswerText        string                       `json:"answer_text,omitempty"`
	Approved          *bool                        `json:"approved,omitempty"`
	ReturnedUpstream  bool                         `json:"returned_upstream,omitempty"`
	ReturnFallback    string                       `json:"return_fallback,omitempty"`
	InvalidatedReason string                       `json:"invalidated_reason,omitempty"`
	CreatedAt         time.Time                    `json:"created_at"`
	UpdatedAt         time.Time                    `json:"updated_at"`
	AnsweredAt        *time.Time                   `json:"answered_at,omitempty"`
}

// InterjectionStatus tracks delivery and wait_for_confirmation acknowledgment for human interjections.
type InterjectionStatus string

const (
	InterjectionStatusPending              InterjectionStatus = "PENDING"
	InterjectionStatusDrained              InterjectionStatus = "DRAINED"
	InterjectionStatusAwaitingConfirmation InterjectionStatus = "AWAITING_CONFIRMATION"
	InterjectionStatusConfirmed            InterjectionStatus = "CONFIRMED"
)

// InterjectionRecord captures an auditable human interjection and its confirmation status.
type InterjectionRecord struct {
	ID                  string             `json:"id"`
	Message             string             `json:"message"`
	WaitForConfirmation bool               `json:"wait_for_confirmation"`
	Status              InterjectionStatus `json:"status"`
	CreatedAt           time.Time          `json:"created_at"`
	DrainedAt           *time.Time         `json:"drained_at,omitempty"`
	ConfirmedAt         *time.Time         `json:"confirmed_at,omitempty"`
}

// ConfigResolver resolves scoped minimal Env/Secret keys via ConfigActor.
type ConfigResolver interface {
	ResolveForNode(ctx context.Context, req *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error)
}

// SlotManager allocates, releases, and suspects worker slots via ProcessRegistryActor.
type SlotManager interface {
	ClaimSlot(ctx context.Context, req *registry.ClaimSlotReq) (*registry.ClaimSlotResp, error)
	ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error)
	ReportSuspect(ctx context.Context, req *registry.ReportSuspectReq) (*registry.ReportSuspectResp, error)
}

// WorkerDispatcher dispatches and aborts node tasks on WorkerDaemon instances.
type WorkerDispatcher interface {
	StartNode(ctx context.Context, appID string, req daemon.StartNodeRequest) (daemon.StartNodeResponse, error)
	AbortNode(ctx context.Context, appID string, req daemon.AbortNodeRequest) (daemon.AbortNodeResponse, error)
}

// RegisterManagedBranchReq notifies RunActor to record a managed branch in the Run Git ledger before remote push.
type RegisterManagedBranchReq struct {
	RunID    string         `json:"run_id"`
	NodeAddr string         `json:"node_addr"`
	Epoch    uint64         `json:"epoch"`
	RepoURL  string         `json:"repo_url"`
	Branch   string         `json:"branch"`
	BaseSHA  string         `json:"base_sha"`
	Kind     git.BranchKind `json:"kind"`
}

// NodeEventNotification informs RunActor when a NodeActor transitions state, pauses for human input, or finishes.
type NodeEventNotification struct {
	RunID        string              `json:"run_id"`
	NodeAddr     string              `json:"node_addr"`
	Epoch        uint64              `json:"epoch"`
	Status       NodeStatus          `json:"status"`
	Outcome      *runner.NodeOutcome `json:"outcome,omitempty"`
	DecisionCard *DecisionCard       `json:"decision_card,omitempty"`
	ErrorMessage string              `json:"error_message,omitempty"`
	OccurredAt   time.Time           `json:"occurred_at"`
}

// RunNotifier abstracts synchronous RunActor ledger registration and node lifecycle callbacks.
type RunNotifier interface {
	RegisterManagedBranch(ctx context.Context, req RegisterManagedBranchReq) error
	NotifyNodeEvent(ctx context.Context, event NodeEventNotification) error
}

// ReminderScheduler registers and unregisters QUEUED_SLOT retry reminders on NodeActor.
type ReminderScheduler interface {
	RegisterSlotRetryReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error
	UnregisterSlotRetryReminder(ctx context.Context, actorType, actorID, reminderName string) error
}

// DispatchSpec holds the persisted execution configuration used for initial dispatch,
// reminder retries, worker dead takeover, and post-decision auto-resume.
type DispatchSpec struct {
	RunID                 string                                  `json:"run_id"`
	NodeAddr              string                                  `json:"node_addr"`
	Namespace             string                                  `json:"namespace,omitempty"`
	RunOverrides          map[string]string                       `json:"run_overrides,omitempty"`
	RequiredEnvKeys       []string                                `json:"required_env_keys,omitempty"`
	TokenRef              *string                                 `json:"token_ref,omitempty"`
	SSHKeyRef             *string                                 `json:"ssh_key_ref,omitempty"`
	RepoURL               string                                  `json:"repo_url"`
	BasedSource           string                                  `json:"based_source,omitempty"`
	BaseSHA               string                                  `json:"base_sha,omitempty"`
	RequiredArch          string                                  `json:"required_arch,omitempty"`
	Worker                workerspec.Config                       `json:"worker"`
	SystemPrompt          string                                  `json:"system_prompt,omitempty"`
	Prompt                string                                  `json:"prompt,omitempty"`
	QCPrompt              string                                  `json:"qc_prompt,omitempty"`
	Sandboxes             map[string]sandboxspec.SandboxConfig    `json:"sandboxes,omitempty"`
	Harnesses             map[string]harnessspec.HarnessConfig    `json:"harnesses,omitempty"`
	ModelProviders        map[string]provider.Config              `json:"model_providers,omitempty"`
	MCPServers            []mcppkg.Config                         `json:"mcp_servers,omitempty"`
	MCPResources          []toolruntime.MCPResourceSpec           `json:"mcp_resources,omitempty"`
	Tools                 map[string]plan.ToolSpec                `json:"tools,omitempty"`
	ToolEnvs              map[string]runner.ExternalToolEnvConfig `json:"tool_envs,omitempty"`
	Imports               []artifact.ImportRequest                `json:"imports,omitempty"`
	PlanBlobKey           string                                  `json:"plan_blob_key,omitempty"`
	PlanSHA256            string                                  `json:"plan_sha256,omitempty"`
	BundlePath            string                                  `json:"bundle_path,omitempty"`
	TargetPlatform        *harnessinstaller.Platform              `json:"target_platform,omitempty"`
	MaxGatekeeperReworks  int                                     `json:"max_gatekeeper_reworks,omitempty"`
	MaxArtifactReworks    int                                     `json:"max_artifact_reworks,omitempty"`
	StallTimeout          time.Duration                           `json:"stall_timeout,omitempty"`
	WatchdogCheckInterval time.Duration                           `json:"watchdog_check_interval,omitempty"`
}

// StartOrResumeReq dispatches or resumes a NodeActor execution.
type StartOrResumeReq struct {
	RunID                 string                                  `json:"run_id"`
	NodeAddr              string                                  `json:"node_addr"`
	Namespace             string                                  `json:"namespace,omitempty"`
	RunOverrides          map[string]string                       `json:"run_overrides,omitempty"`
	RequiredEnvKeys       []string                                `json:"required_env_keys,omitempty"`
	TokenRef              *string                                 `json:"token_ref,omitempty"`
	SSHKeyRef             *string                                 `json:"ssh_key_ref,omitempty"`
	Auth                  git.AuthConfig                          `json:"auth,omitempty"`
	RepoURL               string                                  `json:"repo_url"`
	BasedSource           string                                  `json:"based_source,omitempty"`
	BaseSHA               string                                  `json:"base_sha,omitempty"`
	RequiredArch          string                                  `json:"required_arch,omitempty"`
	Worker                workerspec.Config                       `json:"worker"`
	SystemPrompt          string                                  `json:"system_prompt,omitempty"`
	Prompt                string                                  `json:"prompt,omitempty"`
	QCPrompt              string                                  `json:"qc_prompt,omitempty"`
	Sandboxes             map[string]sandboxspec.SandboxConfig    `json:"sandboxes,omitempty"`
	Harnesses             map[string]harnessspec.HarnessConfig    `json:"harnesses,omitempty"`
	ModelProviders        map[string]provider.Config              `json:"model_providers,omitempty"`
	MCPServers            []mcppkg.Config                         `json:"mcp_servers,omitempty"`
	MCPResources          []toolruntime.MCPResourceSpec           `json:"mcp_resources,omitempty"`
	Tools                 map[string]plan.ToolSpec                `json:"tools,omitempty"`
	ToolEnvs              map[string]runner.ExternalToolEnvConfig `json:"tool_envs,omitempty"`
	Imports               []artifact.ImportRequest                `json:"imports,omitempty"`
	PlanBlobKey           string                                  `json:"plan_blob_key,omitempty"`
	PlanSHA256            string                                  `json:"plan_sha256,omitempty"`
	BundlePath            string                                  `json:"bundle_path,omitempty"`
	TargetPlatform        *harnessinstaller.Platform              `json:"target_platform,omitempty"`
	MaxGatekeeperReworks  int                                     `json:"max_gatekeeper_reworks,omitempty"`
	MaxArtifactReworks    int                                     `json:"max_artifact_reworks,omitempty"`
	StallTimeout          time.Duration                           `json:"stall_timeout,omitempty"`
	WatchdogCheckInterval time.Duration                           `json:"watchdog_check_interval,omitempty"`
	ForceResume           bool                                    `json:"force_resume,omitempty"`
}

// StartOrResumeResp describes the resulting epoch, status, and optional worker lease.
type StartOrResumeResp struct {
	Accepted        bool       `json:"accepted"`
	Epoch           uint64     `json:"epoch"`
	Status          NodeStatus `json:"status"`
	LeaseID         string     `json:"lease_id,omitempty"`
	WorkerProcessID string     `json:"worker_process_id,omitempty"`
	WorkerAppID     string     `json:"worker_app_id,omitempty"`
	ErrorMessage    string     `json:"error_message,omitempty"`
}

// OnWorkerDeadResp reports whether the dead worker notification triggered epoch++ takeover.
type OnWorkerDeadResp struct {
	Handled bool       `json:"handled"`
	Epoch   uint64     `json:"epoch"`
	Status  NodeStatus `json:"status"`
}

// PauseNodeReq requests planned or immediate node pause.
type PauseNodeReq struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason,omitempty"`
}

// PauseNodeResp reports the resulting paused state and whether an active worker was aborted.
type PauseNodeResp struct {
	Paused        bool       `json:"paused"`
	Epoch         uint64     `json:"epoch"`
	Status        NodeStatus `json:"status"`
	AbortedWorker bool       `json:"aborted_worker,omitempty"`
}

// AdvanceForReworkReq advances epoch for fallback or MergeReworkSource repair.
type AdvanceForReworkReq struct {
	BaseSHA           string                       `json:"base_sha,omitempty"`
	BasedSource       string                       `json:"based_source,omitempty"`
	Reason            string                       `json:"reason,omitempty"`
	FallbackArtifacts []artifact.PublishedArtifact `json:"fallback_artifacts,omitempty"`
	ClearCheckpoints  bool                         `json:"clear_checkpoints,omitempty"`
}

// AdvanceForReworkResp reports the new epoch and reset status after rework preparation.
type AdvanceForReworkResp struct {
	Epoch         uint64     `json:"epoch"`
	Status        NodeStatus `json:"status"`
	AbortedWorker bool       `json:"aborted_worker,omitempty"`
}

// ResetForRerunReq resets the node (or cascades downstream invalidation) for fresh rerun.
type ResetForRerunReq struct {
	BaseSHA            string `json:"base_sha,omitempty"`
	BasedSource        string `json:"based_source,omitempty"`
	ClearBaseSHA       bool   `json:"clear_base_sha,omitempty"`
	ClearInterjections bool   `json:"clear_interjections,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// ResetForRerunResp reports the new epoch and reset PENDING status.
type ResetForRerunResp struct {
	Epoch         uint64     `json:"epoch"`
	Status        NodeStatus `json:"status"`
	AbortedWorker bool       `json:"aborted_worker,omitempty"`
}

// ReportNodeOutcomeResp acknowledges worker outcome reporting or flags stale/aborted runs.
type ReportNodeOutcomeResp struct {
	Accepted       bool          `json:"accepted"`
	StaleEpoch     bool          `json:"stale_epoch,omitempty"`
	IgnoredAborted bool          `json:"ignored_aborted,omitempty"`
	CurrentEpoch   uint64        `json:"current_epoch"`
	Status         NodeStatus    `json:"status"`
	DecisionCard   *DecisionCard `json:"decision_card,omitempty"`
}

// SubmitDecisionReq submits an operator answer or version-bound approval for a DecisionCard.
type SubmitDecisionReq struct {
	CardID           string   `json:"card_id,omitempty"`
	SelectedOption   string   `json:"selected_option,omitempty"`
	AnswerText       string   `json:"answer_text,omitempty"`
	Approved         *bool    `json:"approved,omitempty"`
	ReturnedUpstream bool     `json:"returned_upstream,omitempty"`
	ReturnFallback   string   `json:"return_fallback,omitempty"`
	CommitSHA        string   `json:"commit_sha,omitempty"`
	ArtifactIDs      []string `json:"artifact_ids,omitempty"`
	RunPaused        bool     `json:"run_paused,omitempty"`
}

// SubmitDecisionResp returns the updated DecisionCard and whether execution auto-resumed.
type SubmitDecisionResp struct {
	Accepted    bool          `json:"accepted"`
	Card        *DecisionCard `json:"card,omitempty"`
	Epoch       uint64        `json:"epoch"`
	Status      NodeStatus    `json:"status"`
	AutoResumed bool          `json:"auto_resumed"`
}

// GetNodeStateReq queries the full authoritative state of a NodeActor.
type GetNodeStateReq struct{}

// GetNodeStateResp returns a deep-copied snapshot of the NodeActor state for 028/029 APIs.
type GetNodeStateResp struct {
	RunID                string                        `json:"run_id"`
	NodeAddr             string                        `json:"node_addr"`
	CurrentEpoch         uint64                        `json:"current_epoch"`
	Status               NodeStatus                    `json:"status"`
	ManualPaused         bool                          `json:"manual_paused"`
	LeaseID              string                        `json:"lease_id,omitempty"`
	LeaseKey             string                        `json:"lease_key,omitempty"`
	WorkerProcessID      string                        `json:"worker_process_id,omitempty"`
	WorkerAppID          string                        `json:"worker_app_id,omitempty"`
	LastError            string                        `json:"last_error,omitempty"`
	Checkpoints          []checkpoint.CheckpointRecord `json:"checkpoints,omitempty"`
	PendingInterjections []session.HumanInterjection   `json:"pending_interjections,omitempty"`
	InterjectionLog      []InterjectionRecord          `json:"interjection_log,omitempty"`
	DecisionCards        []DecisionCard                `json:"decision_cards,omitempty"`
	PendingPublication   *runner.PendingPublication    `json:"pending_publication,omitempty"`
	Approval             *runner.ApprovalRecord        `json:"approval,omitempty"`
	Outcome              *runner.NodeOutcome           `json:"outcome,omitempty"`
}

// EpochFencedResp carries lossless epoch/domain error metadata across dapr/go-sdk InvokeActor.
type EpochFencedResp struct {
	OK           bool   `json:"ok"`
	StaleEpoch   bool   `json:"stale_epoch,omitempty"`
	CurrentEpoch uint64 `json:"current_epoch,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// RegisterCheckpointBranchReq registers an epoch-scoped checkpoint branch before first remote push.
type RegisterCheckpointBranchReq struct {
	Epoch   uint64 `json:"epoch"`
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
}

// RegisterWorkBranchReq registers a final work branch before push.
type RegisterWorkBranchReq struct {
	Epoch   uint64 `json:"epoch"`
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
}

// BeginCheckpointReq initiates Phase 1 pending checkpoint diary persistence.
type BeginCheckpointReq struct {
	Epoch      uint64             `json:"epoch"`
	Branch     string             `json:"branch"`
	InitialSHA string             `json:"initial_sha"`
	Diary      session.DiaryInput `json:"diary"`
}

// BeginCheckpointResp returns the allocated checkpoint sequence number or lossless error metadata.
type BeginCheckpointResp struct {
	Seq          uint64 `json:"seq,omitempty"`
	StaleEpoch   bool   `json:"stale_epoch,omitempty"`
	CurrentEpoch uint64 `json:"current_epoch,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// AppendFinalSHAReq records a committed final SHA on a pending checkpoint.
type AppendFinalSHAReq struct {
	Epoch    uint64 `json:"epoch"`
	Seq      uint64 `json:"seq"`
	FinalSHA string `json:"final_sha"`
}

// CompleteCheckpointReq finalizes Phase 3 three-way-aligned checkpoint completion.
type CompleteCheckpointReq struct {
	Epoch             uint64           `json:"epoch"`
	Seq               uint64           `json:"seq"`
	FinalSHA          string           `json:"final_sha"`
	ArtifactsSnapshot *blob.ObjectMeta `json:"artifacts_snapshot,omitempty"`
}

// ListCheckpointsReq requests all checkpoint records ordered newest-to-oldest.
type ListCheckpointsReq struct{}

// ListCheckpointsResp returns checkpoint records ordered descending by Seq.
type ListCheckpointsResp struct {
	Records []checkpoint.CheckpointRecord `json:"records,omitempty"`
}

// EnqueueInterjectionReq queues a live human interjection into the node's FIFO queue.
type EnqueueInterjectionReq struct {
	ID                  string `json:"id,omitempty"`
	Message             string `json:"message"`
	WaitForConfirmation bool   `json:"wait_for_confirmation,omitempty"`
}

// EnqueueInterjectionResp returns the assigned interjection ID and queue status.
type EnqueueInterjectionResp struct {
	ID                  string             `json:"id"`
	WaitForConfirmation bool               `json:"wait_for_confirmation"`
	Status              InterjectionStatus `json:"status"`
	QueuedCount         int                `json:"queued_count"`
}

// DrainInterjectionsReq atomically drains pending interjections, optionally fenced by Epoch.
type DrainInterjectionsReq struct {
	Epoch uint64 `json:"epoch,omitempty"`
}

// DrainInterjectionsResp returns drained interjections or lossless StaleEpoch fencing.
type DrainInterjectionsResp struct {
	Items        []session.HumanInterjection `json:"items,omitempty"`
	StaleEpoch   bool                        `json:"stale_epoch,omitempty"`
	CurrentEpoch uint64                      `json:"current_epoch,omitempty"`
}

// HasPendingInterjectionsReq checks whether undrained interjections remain in the FIFO queue.
type HasPendingInterjectionsReq struct{}

// HasPendingInterjectionsResp reports queue non-emptiness.
type HasPendingInterjectionsResp struct {
	HasPending bool `json:"has_pending"`
}

// SaveQCLedgerSnapshotReq persists the cross-round QC ledger snapshot under epoch fencing.
type SaveQCLedgerSnapshotReq struct {
	Epoch    uint64 `json:"epoch"`
	Snapshot []byte `json:"snapshot,omitempty"`
}

// LoadQCLedgerSnapshotReq reads the saved QC ledger snapshot.
type LoadQCLedgerSnapshotReq struct{}

// LoadQCLedgerSnapshotResp returns the saved QC ledger snapshot bytes.
type LoadQCLedgerSnapshotResp struct {
	Snapshot []byte `json:"snapshot,omitempty"`
}

// SavePendingPublicationReq saves or clears version-bound pending publication state under epoch fencing.
type SavePendingPublicationReq struct {
	Epoch   uint64                     `json:"epoch"`
	Pending *runner.PendingPublication `json:"pending,omitempty"`
}

// LoadPendingPublicationReq reads any saved PendingPublication.
type LoadPendingPublicationReq struct{}

// LoadPendingPublicationResp returns the persisted PendingPublication, if any.
type LoadPendingPublicationResp struct {
	Pending *runner.PendingPublication `json:"pending,omitempty"`
}

// GetApprovalReq reads any persisted human ApprovalRecord.
type GetApprovalReq struct{}

// GetApprovalResp returns the persisted ApprovalRecord, if any.
type GetApprovalResp struct {
	Approval *runner.ApprovalRecord `json:"approval,omitempty"`
}
