package merge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/git"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

const (
	// ActorType is the fixed Dapr Actor type name for target-branch serial merge queues.
	ActorType = "MergeActor"
	// DefaultSlotRetryInterval is the default reminder retry period when worker slots are saturated.
	DefaultSlotRetryInterval = 1 * time.Second
	// DefaultSlotRetryReminderName is the persistent Dapr reminder name driving queued merge retries.
	DefaultSlotRetryReminderName = "merge-slot-retry"
)

var (
	// ErrInvalidTarget indicates an invalid repo_url, run_id, node address, or branch name.
	ErrInvalidTarget = errors.New("merge: invalid merge target")
)

// EffectiveTargetBranch resolves the effective target branch and whether it is an explicit shared branch.
// When explicitTarget is nil or blank, it returns git.WorkBranchName(runID, mergeNodeCanonicalAddr) with shared=false.
// When explicitTarget is non-empty, it validates/normalizes the branch name and returns shared=true.
func EffectiveTargetBranch(runID, mergeNodeCanonicalAddr string, explicitTarget *string) (branch string, shared bool, err error) {
	if explicitTarget != nil && strings.TrimSpace(*explicitTarget) != "" {
		cleanBranch, normErr := git.NormalizeBranchName(*explicitTarget)
		if normErr != nil {
			return "", false, fmt.Errorf("%w: %w", ErrInvalidTarget, normErr)
		}
		return cleanBranch, true, nil
	}
	if strings.TrimSpace(runID) == "" {
		return "", false, fmt.Errorf("%w: run_id is required when target_branch is unset", ErrInvalidTarget)
	}
	if strings.TrimSpace(mergeNodeCanonicalAddr) == "" {
		return "", false, fmt.Errorf("%w: merge_node_addr is required when target_branch is unset", ErrInvalidTarget)
	}
	return git.WorkBranchName(runID, mergeNodeCanonicalAddr), false, nil
}

// MergeActorID computes the deterministic, URL-safe 64-char lowercase hex Dapr Actor ID for (repoURL, effectiveTargetBranch).
func MergeActorID(repoURL, effectiveTargetBranch string) (string, error) {
	if strings.TrimSpace(repoURL) == "" {
		return "", fmt.Errorf("%w: repo_url is required", ErrInvalidTarget)
	}
	normRepo := git.NormalizeRepoURL(repoURL)
	if normRepo == "" {
		return "", fmt.Errorf("%w: repo_url normalized to empty", ErrInvalidTarget)
	}
	cleanBranch, err := git.NormalizeBranchName(effectiveTargetBranch)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidTarget, err)
	}
	sum := sha256.Sum256([]byte(normRepo + "\x00" + cleanBranch))
	return hex.EncodeToString(sum[:]), nil
}

// TicketStatus represents the lifecycle state of a queued merge candidate ticket.
type TicketStatus string

const (
	TicketStatusPending        TicketStatus = "PENDING"
	TicketStatusInFlight       TicketStatus = "IN_FLIGHT"
	TicketStatusCompleted      TicketStatus = "COMPLETED"
	TicketStatusReworkRequired TicketStatus = "REWORK_REQUIRED"
	TicketStatusFailed         TicketStatus = "FAILED"
	TicketStatusCancelled      TicketStatus = "CANCELLED"
)

// CandidateTicket captures a single idempotent merge candidate request in the persistent FIFO queue.
type CandidateTicket struct {
	RequestID          string                        `json:"request_id"`
	RunID              string                        `json:"run_id"`
	MergeNodeAddr      string                        `json:"merge_node_addr"`
	MergeEpoch         uint64                        `json:"merge_epoch"`
	SourceNodeAddr     string                        `json:"source_node_addr"`
	SourceEpoch        uint64                        `json:"source_epoch"`
	RepoURL            string                        `json:"repo_url"`
	BasedSource        string                        `json:"based_source,omitempty"`
	BaseSHA            string                        `json:"base_sha,omitempty"`
	TargetBranch       string                        `json:"target_branch"`
	ExplicitTarget     *string                       `json:"explicit_target,omitempty"`
	SharedTargetBranch bool                          `json:"shared_target_branch"`
	CandidateBranch    string                        `json:"candidate_branch"`
	Namespace          string                        `json:"namespace,omitempty"`
	RunOverrides       map[string]string             `json:"run_overrides,omitempty"`
	RequiredEnvKeys    []string                      `json:"required_env_keys,omitempty"`
	TokenRef           *string                       `json:"token_ref,omitempty"`
	SSHKeyRef          *string                       `json:"ssh_key_ref,omitempty"`
	Auth               git.AuthConfig                `json:"auth,omitempty"`
	RequiredArch       string                        `json:"required_arch,omitempty"`
	Sandbox            sandboxspec.SandboxConfig     `json:"sandbox"`
	Gatekeepers        []workerspec.GatekeeperConfig `json:"gatekeepers,omitempty"`
	PlanBlobKey        string                        `json:"plan_blob_key,omitempty"`
	PlanSHA256         string                        `json:"plan_sha256,omitempty"`
	BundlePath         string                        `json:"bundle_path,omitempty"`
	ImportsDir         string                        `json:"imports_dir,omitempty"`
	Status             TicketStatus                  `json:"status"`
	Attempt            uint64                        `json:"attempt,omitempty"`
	LeaseID            string                        `json:"lease_id,omitempty"`
	LeaseKey           string                        `json:"lease_key,omitempty"`
	WorkerProcessID    string                        `json:"worker_process_id,omitempty"`
	WorkerAppID        string                        `json:"worker_app_id,omitempty"`
	MergedCommitSHA    string                        `json:"merged_commit_sha,omitempty"`
	LatestTargetSHA    string                        `json:"latest_target_sha,omitempty"`
	ErrorMessage       string                        `json:"error_message,omitempty"`
	EnqueuedAt         time.Time                     `json:"enqueued_at"`
	UpdatedAt          time.Time                     `json:"updated_at"`
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

// MergeWorkerDispatcher dispatches and aborts merge candidate checks on WorkerDaemon instances.
type MergeWorkerDispatcher interface {
	RunMergeCandidate(ctx context.Context, appID string, req daemon.RunMergeCandidateRequest) (daemon.RunMergeCandidateResponse, error)
	AbortMergeCandidate(ctx context.Context, appID string, req daemon.AbortMergeRequest) (daemon.AbortMergeResponse, error)
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

// MergeCandidateNotification informs RunActor of a merge candidate's outcome (success, rework_source, or error).
type MergeCandidateNotification struct {
	RequestID              string                      `json:"request_id"`
	RunID                  string                      `json:"run_id"`
	MergeNodeAddr          string                      `json:"merge_node_addr"`
	MergeEpoch             uint64                      `json:"merge_epoch"`
	SourceNodeAddr         string                      `json:"source_node_addr"`
	SourceEpoch            uint64                      `json:"source_epoch"`
	CandidateBranch        string                      `json:"candidate_branch"`
	TargetBranch           string                      `json:"target_branch"`
	SharedTargetBranch     bool                        `json:"shared_target_branch"`
	MergedIntoSharedBranch bool                        `json:"merged_into_shared_branch"`
	Result                 runner.MergeCandidateResult `json:"result"`
	ErrorMessage           string                      `json:"error_message,omitempty"`
	OccurredAt             time.Time                   `json:"occurred_at"`
}

// RunMergeNotifier abstracts RunActor ledger registration and merge outcome callbacks.
type RunMergeNotifier interface {
	RegisterManagedBranch(ctx context.Context, req RegisterManagedBranchReq) error
	OnMergeCandidateResult(ctx context.Context, notice MergeCandidateNotification) error
}

// ReminderScheduler registers and unregisters slot retry reminders on MergeActor.
type ReminderScheduler interface {
	RegisterSlotRetryReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error
	UnregisterSlotRetryReminder(ctx context.Context, actorType, actorID, reminderName string) error
}

// EnqueueCandidateReq submits a candidate branch for serial integration onto the target branch.
type EnqueueCandidateReq struct {
	RunID               string                        `json:"run_id"`
	MergeNodeAddr       string                        `json:"merge_node_addr"`
	MergeEpoch          uint64                        `json:"merge_epoch,omitempty"`
	SourceNodeAddr      string                        `json:"source_node_addr"`
	SourceEpoch         uint64                        `json:"source_epoch"`
	RepoURL             string                        `json:"repo_url"`
	BasedSource         string                        `json:"based_source,omitempty"`
	BaseSHA             string                        `json:"base_sha,omitempty"`
	TargetBranch        *string                       `json:"target_branch,omitempty"`
	BranchPreRegistered bool                          `json:"branch_pre_registered,omitempty"`
	CandidateBranch     string                        `json:"candidate_branch"`
	Namespace           string                        `json:"namespace,omitempty"`
	RunOverrides        map[string]string             `json:"run_overrides,omitempty"`
	RequiredEnvKeys     []string                      `json:"required_env_keys,omitempty"`
	TokenRef            *string                       `json:"token_ref,omitempty"`
	SSHKeyRef           *string                       `json:"ssh_key_ref,omitempty"`
	Auth                git.AuthConfig                `json:"auth,omitempty"`
	RequiredArch        string                        `json:"required_arch,omitempty"`
	Sandbox             sandboxspec.SandboxConfig     `json:"sandbox"`
	Gatekeepers         []workerspec.GatekeeperConfig `json:"gatekeepers,omitempty"`
	PlanBlobKey         string                        `json:"plan_blob_key,omitempty"`
	PlanSHA256          string                        `json:"plan_sha256,omitempty"`
	BundlePath          string                        `json:"bundle_path,omitempty"`
	ImportsDir          string                        `json:"imports_dir,omitempty"`
}

// EnqueueCandidateResp confirms queue acceptance and reports deduplication status.
type EnqueueCandidateResp struct {
	Accepted     bool            `json:"accepted"`
	Deduplicated bool            `json:"deduplicated,omitempty"`
	Ticket       CandidateTicket `json:"ticket"`
}

// ReportCandidateResultResp acknowledges worker outcome reporting or flags stale/cancelled reports.
type ReportCandidateResultResp struct {
	Accepted        bool         `json:"accepted"`
	IgnoredStale    bool         `json:"ignored_stale,omitempty"`
	Status          TicketStatus `json:"status,omitempty"`
	LatestTargetSHA string       `json:"latest_target_sha,omitempty"`
}

// CancelByRunReq cancels pending/in-flight merge tickets matching RunID and optional MergeNodeAddr/SourceNodeAddr.
type CancelByRunReq struct {
	RunID          string `json:"run_id"`
	MergeNodeAddr  string `json:"merge_node_addr,omitempty"`
	SourceNodeAddr string `json:"source_node_addr,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// CancelByRunResp reports how many pending and in-flight tickets were cancelled.
type CancelByRunResp struct {
	CancelledPending  int    `json:"cancelled_pending"`
	CancelledInFlight bool   `json:"cancelled_in_flight"`
	AbortedWorker     bool   `json:"aborted_worker,omitempty"`
	InFlightRequestID string `json:"in_flight_request_id,omitempty"`
}

// OnWorkerDeadResp reports whether an active IN_FLIGHT merge ticket was reset and re-dispatched.
type OnWorkerDeadResp struct {
	Handled   bool   `json:"handled"`
	RequestID string `json:"request_id,omitempty"`
}

// GetMergeStateReq queries the current persistent state of a MergeActor.
type GetMergeStateReq struct{}

// GetMergeStateResp returns a defensive snapshot of the MergeActor queue and LatestTargetSHA.
type GetMergeStateResp struct {
	RepoURL            string            `json:"repo_url,omitempty"`
	TargetBranch       string            `json:"target_branch,omitempty"`
	SharedTargetBranch bool              `json:"shared_target_branch,omitempty"`
	LatestTargetSHA    string            `json:"latest_target_sha,omitempty"`
	InFlight           *CandidateTicket  `json:"in_flight,omitempty"`
	Queue              []CandidateTicket `json:"queue,omitempty"`
	History            []CandidateTicket `json:"history,omitempty"`
}
