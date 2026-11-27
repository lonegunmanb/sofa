package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	daprclient "github.com/dapr/go-sdk/client"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/applyeval"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const (
	// OperatorActorType is the singleton Dapr Actor type name for cross-run indexing and recovery.
	OperatorActorType = "OperatorActor"
	// GlobalOperatorActorID is the singleton actor ID for OperatorActor.
	GlobalOperatorActorID = "global"
	// RunActorType is the Dapr Actor type name for per-Run DAG orchestration.
	RunActorType = "RunActor"

	// DefaultReconcileInterval is the default safety-net reminder cadence while a Run is active.
	DefaultReconcileInterval = 5 * time.Second
	// DefaultWorkerCleanupTimeout bounds best-effort worker bundle cleanup during DeleteRun.
	DefaultWorkerCleanupTimeout = 3 * time.Second
	// DefaultReconcileReminderName is the persistent reminder name used by RunActor.
	DefaultReconcileReminderName = "run-reconcile"

	// DefaultNamespace normalizes blank Run namespaces.
	DefaultNamespace = "default"
	// DefaultPubSubName is the default Dapr pubsub component name used for lifecycle events.
	DefaultPubSubName = "sofa-pubsub"
	// DefaultRunEventsTopic is the default pubsub topic for Run status and node event notifications.
	DefaultRunEventsTopic = "sofa.run.events"
)

const (
	ErrCodeAlreadyMergedSharedBranch = "ALREADY_MERGED_SHARED_BRANCH"
	ErrCodeNotFound                  = "NOT_FOUND"
	ErrCodeConflict                  = "CONFLICT"
	ErrCodeInvalidInput              = "INVALID_INPUT"
	ErrCodeProtectedSourceBranch     = "PROTECTED_SOURCE_BRANCH"
	ErrCodeBranchOwnershipConflict   = "BRANCH_OWNERSHIP_CONFLICT"
	ErrCodeInvalidBaseSHA            = "INVALID_BASE_SHA"
)

var (
	// ErrAlreadyMergedSharedBranch indicates RerunNode targeted a node or transitive downstream
	// node already merged into an explicit shared target_branch (maps to HTTP 409 Conflict).
	ErrAlreadyMergedSharedBranch = errors.New("run: target or downstream node already merged into shared target_branch")
	// ErrRunNotFound indicates the target Run does not exist or has no persisted state.
	ErrRunNotFound = errors.New("run: run not found")
	// ErrNodeNotFound indicates the requested node address does not exist in the Run plan/state.
	ErrNodeNotFound = errors.New("run: node not found")
	// ErrRunConflict indicates an operation conflicts with the current Run lifecycle state.
	ErrRunConflict = errors.New("run: state conflict")
)

// RunStatus identifies the authoritative lifecycle state of a RunActor.
type RunStatus string

const (
	RunStatusPending      RunStatus = "PENDING"
	RunStatusRunning      RunStatus = "RUNNING"
	RunStatusWaitingHuman RunStatus = "WAITING_HUMAN"
	RunStatusPausing      RunStatus = "PAUSING"
	RunStatusPaused       RunStatus = "PAUSED"
	RunStatusSucceeded    RunStatus = "SUCCEEDED"
	RunStatusFailed       RunStatus = "FAILED"
	RunStatusDeleted      RunStatus = "DELETED"
)

// RunSummary captures lightweight metadata stored in OperatorActor("global").
type RunSummary struct {
	RunID        string    `json:"run_id"`
	Namespace    string    `json:"namespace"`
	Status       RunStatus `json:"status"`
	PlanBlobKey  string    `json:"plan_blob_key,omitempty"`
	PlanSHA256   string    `json:"plan_sha256,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// NodeRunSummary provides an authoritative snapshot of one Run DAG node for REST/UI queries.
type NodeRunSummary struct {
	Address                string                       `json:"address"`
	CanonicalAddress       string                       `json:"canonical_address"`
	Kind                   string                       `json:"kind"`
	ModulePath             string                       `json:"module_path,omitempty"`
	Status                 node.NodeStatus              `json:"status"`
	CurrentEpoch           uint64                       `json:"current_epoch"`
	WorkBranch             string                       `json:"work_branch,omitempty"`
	CommitSHA              string                       `json:"commit_sha,omitempty"`
	Artifacts              []artifact.PublishedArtifact `json:"artifacts,omitempty"`
	MergedIntoSharedBranch bool                         `json:"merged_into_shared_branch,omitempty"`
	DynamicKeys            []string                     `json:"dynamic_keys,omitempty"`
	ErrorMessage           string                       `json:"error_message,omitempty"`
}

// WirePathStep serializes one step of a cty.Path for lossless sensitivity mark transport.
type WirePathStep struct {
	Attribute *string    `json:"attribute,omitempty"`
	Index     *WireIndex `json:"index,omitempty"`
}

// WireIndex encodes an index key along a cty.Path.
type WireIndex struct {
	Type  json.RawMessage `json:"type"`
	Value json.RawMessage `json:"value"`
}

// WireValue serializes a cty.Value alongside type info and golden.SensitiveMark paths.
type WireValue struct {
	TypeJSON       json.RawMessage  `json:"type_json,omitempty"`
	ValueJSON      json.RawMessage  `json:"value_json,omitempty"`
	Unknown        bool             `json:"unknown,omitempty"`
	Sensitive      bool             `json:"sensitive,omitempty"`
	HasMarks       bool             `json:"has_marks,omitempty"`
	SensitivePaths [][]WirePathStep `json:"sensitive_paths,omitempty"`
}

// EncodeWireValue encodes val into a JSON-safe WireValue preserving golden.SensitiveMark metadata.
func EncodeWireValue(val cty.Value) (WireValue, error) {
	if val == cty.NilVal {
		return WireValue{Unknown: true}, nil
	}
	unmarked, pathMarks := val.UnmarkDeepWithPaths()
	typeJSON, err := ctyjson.MarshalType(unmarked.Type())
	if err != nil {
		return WireValue{}, fmt.Errorf("encode cty type: %w", err)
	}

	var sensitivePaths [][]WirePathStep
	hasSensitive := val.HasMark(golden.SensitiveMark)
	for _, pvm := range pathMarks {
		if _, ok := pvm.Marks[golden.SensitiveMark]; !ok {
			continue
		}
		hasSensitive = true
		steps, err := encodeWirePath(pvm.Path)
		if err != nil {
			return WireValue{}, err
		}
		sensitivePaths = append(sensitivePaths, steps)
	}

	hasMarks := len(pathMarks) > 0 || val.HasMark(golden.SensitiveMark)
	if !unmarked.IsWhollyKnown() {
		return WireValue{
			TypeJSON:       typeJSON,
			Unknown:        true,
			Sensitive:      hasSensitive,
			HasMarks:       hasMarks,
			SensitivePaths: sensitivePaths,
		}, nil
	}

	valJSON, err := ctyjson.Marshal(unmarked, unmarked.Type())
	if err != nil {
		return WireValue{}, fmt.Errorf("encode cty value: %w", err)
	}
	return WireValue{
		TypeJSON:       typeJSON,
		ValueJSON:      valJSON,
		Sensitive:      hasSensitive,
		HasMarks:       hasMarks,
		SensitivePaths: sensitivePaths,
	}, nil
}

// DecodeWireValue reconstructs a cty.Value (including golden.SensitiveMark paths) from WireValue.
func (w WireValue) DecodeWireValue() (cty.Value, error) {
	if len(w.TypeJSON) == 0 {
		if w.Unknown {
			val := cty.DynamicVal
			if w.Sensitive || w.HasMarks {
				val = val.Mark(golden.SensitiveMark)
			}
			return val, nil
		}
		return cty.NilVal, nil
	}
	t, err := ctyjson.UnmarshalType(w.TypeJSON)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode cty type: %w", err)
	}
	var val cty.Value
	if w.Unknown || len(w.ValueJSON) == 0 {
		val = cty.UnknownVal(t)
	} else {
		val, err = ctyjson.Unmarshal(w.ValueJSON, t)
		if err != nil {
			return cty.NilVal, fmt.Errorf("decode cty value: %w", err)
		}
	}
	if len(w.SensitivePaths) > 0 {
		pvs := make([]cty.PathValueMarks, 0, len(w.SensitivePaths))
		for _, steps := range w.SensitivePaths {
			p, err := decodeWirePath(steps)
			if err != nil {
				return cty.NilVal, err
			}
			pvs = append(pvs, cty.PathValueMarks{
				Path:  p,
				Marks: cty.NewValueMarks(golden.SensitiveMark),
			})
		}
		val = val.MarkWithPaths(pvs)
	} else if w.Sensitive || w.HasMarks {
		val = val.Mark(golden.SensitiveMark)
	}
	return val, nil
}

func encodeWirePath(p cty.Path) ([]WirePathStep, error) {
	steps := make([]WirePathStep, 0, len(p))
	for _, step := range p {
		switch s := step.(type) {
		case cty.GetAttrStep:
			name := s.Name
			steps = append(steps, WirePathStep{Attribute: &name})
		case cty.IndexStep:
			unmarkedKey, _ := s.Key.UnmarkDeep()
			kt, err := ctyjson.MarshalType(unmarkedKey.Type())
			if err != nil {
				return nil, fmt.Errorf("encode path key type: %w", err)
			}
			kv, err := ctyjson.Marshal(unmarkedKey, unmarkedKey.Type())
			if err != nil {
				return nil, fmt.Errorf("encode path key value: %w", err)
			}
			steps = append(steps, WirePathStep{Index: &WireIndex{Type: kt, Value: kv}})
		default:
			return nil, fmt.Errorf("unsupported cty path step %T", step)
		}
	}
	return steps, nil
}

func decodeWirePath(steps []WirePathStep) (cty.Path, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	p := make(cty.Path, 0, len(steps))
	for _, step := range steps {
		switch {
		case step.Attribute != nil:
			p = p.GetAttr(*step.Attribute)
		case step.Index != nil:
			kt, err := ctyjson.UnmarshalType(step.Index.Type)
			if err != nil {
				return nil, fmt.Errorf("decode path key type: %w", err)
			}
			kv, err := ctyjson.Unmarshal(step.Index.Value, kt)
			if err != nil {
				return nil, fmt.Errorf("decode path key value: %w", err)
			}
			p = p.Index(kv)
		default:
			return nil, errors.New("invalid empty wire path step")
		}
	}
	return p, nil
}

// EvaluatedOutput stores an evaluated Run output along with its declared sensitivity metadata.
type EvaluatedOutput struct {
	Name           string           `json:"name"`
	Description    string           `json:"description,omitempty"`
	Sensitive      bool             `json:"sensitive,omitempty"`
	HasMarks       bool             `json:"has_marks,omitempty"`
	Unknown        bool             `json:"unknown,omitempty"`
	TypeJSON       json.RawMessage  `json:"type_json,omitempty"`
	ValueJSON      json.RawMessage  `json:"value_json,omitempty"`
	SensitivePaths [][]WirePathStep `json:"sensitive_paths,omitempty"`
}

// DecodeValue reconstructs the evaluated cty.Value (including golden.SensitiveMark metadata).
func (o EvaluatedOutput) DecodeValue() (cty.Value, error) {
	wire := WireValue{
		TypeJSON:       o.TypeJSON,
		ValueJSON:      o.ValueJSON,
		Unknown:        o.Unknown,
		Sensitive:      o.Sensitive,
		HasMarks:       o.HasMarks,
		SensitivePaths: o.SensitivePaths,
	}
	val, err := wire.DecodeWireValue()
	if err != nil {
		return cty.NilVal, err
	}
	if o.Sensitive && !val.HasMark(golden.SensitiveMark) {
		val = val.Mark(golden.SensitiveMark)
	}
	return val, nil
}

// UpsertRunReq inserts or updates a RunSummary in OperatorActor("global").
type UpsertRunReq struct {
	Summary RunSummary `json:"summary"`
}

// UpsertRunResp returns the normalized stored RunSummary.
type UpsertRunResp struct {
	OK      bool       `json:"ok"`
	Summary RunSummary `json:"summary"`
}

// GetRunSummaryReq fetches a single RunSummary by run_id from OperatorActor.
type GetRunSummaryReq struct {
	RunID string `json:"run_id"`
}

// GetRunSummaryResp returns whether the Run exists and its RunSummary.
type GetRunSummaryResp struct {
	Found     bool       `json:"found"`
	Summary   RunSummary `json:"summary,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
}

// ListRunsReq lists indexed Runs optionally filtered by namespace or status.
type ListRunsReq struct {
	Namespace string    `json:"namespace,omitempty"`
	Status    RunStatus `json:"status,omitempty"`
}

// ListRunsResp returns matching RunSummary items sorted newest-first (CreatedAt desc, RunID asc).
type ListRunsResp struct {
	Runs []RunSummary `json:"runs"`
}

// RemoveRunReq removes a Run entry from OperatorActor("global") after DeleteRun completes.
type RemoveRunReq struct {
	RunID string `json:"run_id"`
}

// RemoveRunResp indicates whether an entry was deleted from OperatorActor.
type RemoveRunResp struct {
	Removed bool `json:"removed"`
}

// RecoverActiveRunsReq triggers OperatorActor to wake all RUNNING/PAUSING Runs after control-plane restart.
type RecoverActiveRunsReq struct {
	Reason string `json:"reason,omitempty"`
}

// RecoverActiveRunsResp reports which RunIDs were woken via Reconcile and any per-run failures.
type RecoverActiveRunsResp struct {
	RecoveredRunIDs []string          `json:"recovered_run_ids,omitempty"`
	FailedRuns      map[string]string `json:"failed_runs,omitempty"`
}

// StartRunReq initializes a new RunActor execution from an uploaded .sofaplan BlobStore archive.
type StartRunReq struct {
	RunID                 string                     `json:"run_id"`
	Namespace             string                     `json:"namespace,omitempty"`
	PlanBlobKey           string                     `json:"plan_blob_key,omitempty"`
	PlanSHA256            string                     `json:"plan_sha256,omitempty"`
	RunOverrides          map[string]string          `json:"run_overrides,omitempty"`
	RequiredArch          string                     `json:"required_arch,omitempty"`
	TargetPlatform        *harnessinstaller.Platform `json:"target_platform,omitempty"`
	MaxGatekeeperReworks  int                        `json:"max_gatekeeper_reworks,omitempty"`
	MaxArtifactReworks    int                        `json:"max_artifact_reworks,omitempty"`
	StallTimeout          time.Duration              `json:"stall_timeout,omitempty"`
	WatchdogCheckInterval time.Duration              `json:"watchdog_check_interval,omitempty"`
}

// StartRunResp reports whether StartRun was accepted and the resulting RunStatus.
type StartRunResp struct {
	Accepted     bool      `json:"accepted"`
	RunID        string    `json:"run_id"`
	Status       RunStatus `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// ReconcileReq drives a non-blocking DAG reconciliation pass on a RunActor.
type ReconcileReq struct {
	Reason string `json:"reason,omitempty"`
}

// ReconcileResp reports RunStatus and counts of dispatched nodes / enqueued merges.
type ReconcileResp struct {
	RunID           string    `json:"run_id"`
	Status          RunStatus `json:"status"`
	DispatchedNodes int       `json:"dispatched_nodes,omitempty"`
	EnqueuedMerges  int       `json:"enqueued_merges,omitempty"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
}

// RegisterManagedBranchReq matches node.RegisterManagedBranchReq and merge.RegisterManagedBranchReq.
type RegisterManagedBranchReq struct {
	RunID    string         `json:"run_id"`
	NodeAddr string         `json:"node_addr"`
	Epoch    uint64         `json:"epoch"`
	RepoURL  string         `json:"repo_url"`
	Branch   string         `json:"branch"`
	BaseSHA  string         `json:"base_sha"`
	Kind     git.BranchKind `json:"kind"`
}

// RegisterManagedBranchResp carries lossless error metadata across Dapr InvokeMethod.
type RegisterManagedBranchResp struct {
	OK           bool   `json:"ok"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// OnNodeEventResp acknowledges a NodeActor lifecycle notification.
type OnNodeEventResp struct {
	Accepted     bool      `json:"accepted"`
	StaleEpoch   bool      `json:"stale_epoch,omitempty"`
	RunStatus    RunStatus `json:"run_status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// OnMergeCandidateResultResp acknowledges a MergeActor candidate integration notification.
type OnMergeCandidateResultResp struct {
	Accepted     bool      `json:"accepted"`
	IgnoredStale bool      `json:"ignored_stale,omitempty"`
	RunStatus    RunStatus `json:"run_status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// PauseRunReq transitions a Run into PAUSING ("planned") or PAUSED ("immediate").
type PauseRunReq struct {
	Mode   string `json:"mode,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// PauseRunResp reports the Run status after PauseRun.
type PauseRunResp struct {
	Paused       bool      `json:"paused"`
	RunID        string    `json:"run_id"`
	Status       RunStatus `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// ResumeRunReq clears manual pause and resumes orchestration.
type ResumeRunReq struct{}

// ResumeRunResp reports the Run status after ResumeRun.
type ResumeRunResp struct {
	Resumed      bool      `json:"resumed"`
	RunID        string    `json:"run_id"`
	Status       RunStatus `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// RerunNodeReq requests an epoch++ rerun cascade starting at NodeAddr.
type RerunNodeReq struct {
	NodeAddr string `json:"node_addr"`
	Reason   string `json:"reason,omitempty"`
}

// RerunNodeResp reports rerun acceptance or structured 409/404 domain errors.
type RerunNodeResp struct {
	Accepted                  bool      `json:"accepted"`
	AlreadyMergedSharedBranch bool      `json:"already_merged_shared_branch,omitempty"`
	RunStatus                 RunStatus `json:"run_status"`
	ErrorCode                 string    `json:"error_code,omitempty"`
	ErrorMessage              string    `json:"error_message,omitempty"`
}

// DeleteRunReq triggers deferred branch, blob, ConfigActor scope, and OperatorActor cleanup.
type DeleteRunReq struct{}

// DeleteRunResp reports cleanup results for DeleteRun.
type DeleteRunResp struct {
	Deleted         bool               `json:"deleted"`
	DeletedBranches []git.BranchTarget `json:"deleted_branches,omitempty"`
	Status          RunStatus          `json:"status"`
	ErrorCode       string             `json:"error_code,omitempty"`
	ErrorMessage    string             `json:"error_message,omitempty"`
}

// GetRunStateReq queries the authoritative snapshot of a RunActor.
type GetRunStateReq struct{}

// GetRunStateResp contains the full authoritative Run state, nodes, outputs, and ledger.
type GetRunStateResp struct {
	RunID        string                         `json:"run_id"`
	Namespace    string                         `json:"namespace"`
	Status       RunStatus                      `json:"status"`
	ManualPaused bool                           `json:"manual_paused"`
	PlanBlobKey  string                         `json:"plan_blob_key,omitempty"`
	PlanSHA256   string                         `json:"plan_sha256,omitempty"`
	ErrorMessage string                         `json:"error_message,omitempty"`
	Nodes        []NodeRunSummary               `json:"nodes,omitempty"`
	Outputs      map[string]EvaluatedOutput     `json:"outputs,omitempty"`
	Sources      []applyeval.SourceRuntimeState `json:"sources,omitempty"`
	Preserves    []git.PreserveRef              `json:"preserves,omitempty"`
	Ledger       git.LedgerSnapshot             `json:"ledger,omitempty"`
	CreatedAt    time.Time                      `json:"created_at"`
	UpdatedAt    time.Time                      `json:"updated_at"`
	ErrorCode    string                         `json:"error_code,omitempty"`
}

// ListDecisionsReq queries all human DecisionCards across a Run's nodes.
type ListDecisionsReq struct{}

// ListDecisionsResp returns DecisionCards collected across Run nodes.
type ListDecisionsResp struct {
	RunID        string              `json:"run_id"`
	Decisions    []node.DecisionCard `json:"decisions,omitempty"`
	ErrorCode    string              `json:"error_code,omitempty"`
	ErrorMessage string              `json:"error_message,omitempty"`
}

// DecodeRerunNodeError translates RerunNodeResp domain codes back into Go sentinel errors.
func DecodeRerunNodeError(resp *RerunNodeResp, err error) error {
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("run: nil RerunNode response")
	}
	if resp.AlreadyMergedSharedBranch || resp.ErrorCode == ErrCodeAlreadyMergedSharedBranch {
		if resp.ErrorMessage != "" {
			return fmt.Errorf("%w: %s", ErrAlreadyMergedSharedBranch, resp.ErrorMessage)
		}
		return ErrAlreadyMergedSharedBranch
	}
	if resp.ErrorCode != "" {
		return mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	if !resp.Accepted {
		return ErrRunConflict
	}
	return nil
}

// DecodeRunStateError translates GetRunStateResp domain codes back into Go sentinel errors.
func DecodeRunStateError(resp *GetRunStateResp, err error) error {
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("run: nil GetRunState response")
	}
	if resp.ErrorCode != "" {
		return mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return nil
}

// DecodePauseRunError translates PauseRunResp error fields into Go sentinel errors.
func DecodePauseRunError(resp *PauseRunResp, err error) error {
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("run: nil PauseRun response")
	}
	if resp.ErrorCode != "" {
		return mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return nil
}

// DecodeResumeRunError translates ResumeRunResp error fields into Go sentinel errors.
func DecodeResumeRunError(resp *ResumeRunResp, err error) error {
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("run: nil ResumeRun response")
	}
	if resp.ErrorCode != "" {
		return mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return nil
}

// DecodeDeleteRunError translates DeleteRunResp error fields into Go sentinel errors.
func DecodeDeleteRunError(resp *DeleteRunResp, err error) error {
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("run: nil DeleteRun response")
	}
	if resp.ErrorCode != "" {
		return mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return nil
}

// DecodeRegisterManagedBranchError translates RegisterManagedBranchResp codes into Go errors.
func DecodeRegisterManagedBranchError(resp *RegisterManagedBranchResp, err error) error {
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("run: nil RegisterManagedBranch response")
	}
	if resp.ErrorCode != "" {
		return mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	if !resp.OK {
		return ErrRunConflict
	}
	return nil
}

func mapRunErrorCode(code, msg string) error {
	wrap := func(sentinel error) error {
		if msg != "" {
			return fmt.Errorf("%w: %s", sentinel, msg)
		}
		return sentinel
	}
	switch code {
	case ErrCodeAlreadyMergedSharedBranch:
		return wrap(ErrAlreadyMergedSharedBranch)
	case ErrCodeNotFound:
		if strings.Contains(strings.ToLower(msg), "node") {
			return wrap(ErrNodeNotFound)
		}
		return wrap(ErrRunNotFound)
	case ErrCodeConflict:
		return wrap(ErrRunConflict)
	case ErrCodeProtectedSourceBranch:
		return wrap(git.ErrProtectedSourceBranch)
	case ErrCodeBranchOwnershipConflict:
		return wrap(git.ErrBranchOwnershipConflict)
	case ErrCodeInvalidBaseSHA:
		return wrap(git.ErrInvalidBaseSHA)
	default:
		if msg != "" {
			return errors.New(msg)
		}
		return fmt.Errorf("run: error %s", code)
	}
}

// NodeController dispatches and controls per-node NodeActors.
type NodeController interface {
	StartOrResume(ctx context.Context, runID, canonicalNodeAddr string, req *node.StartOrResumeReq) (*node.StartOrResumeResp, error)
	PauseNode(ctx context.Context, runID, canonicalNodeAddr string, req *node.PauseNodeReq) (*node.PauseNodeResp, error)
	AdvanceForRework(ctx context.Context, runID, canonicalNodeAddr string, req *node.AdvanceForReworkReq) (*node.AdvanceForReworkResp, error)
	ResetForRerun(ctx context.Context, runID, canonicalNodeAddr string, req *node.ResetForRerunReq) (*node.ResetForRerunResp, error)
	GetNodeState(ctx context.Context, runID, canonicalNodeAddr string) (*node.GetNodeStateResp, error)
}

// MergeController interacts with target-branch MergeActors.
type MergeController interface {
	EnqueueCandidate(ctx context.Context, repoURL, effectiveTargetBranch string, req *merge.EnqueueCandidateReq) (*merge.EnqueueCandidateResp, error)
	CancelByRun(ctx context.Context, repoURL, effectiveTargetBranch string, req *merge.CancelByRunReq) (*merge.CancelByRunResp, error)
	GetMergeState(ctx context.Context, repoURL, effectiveTargetBranch string) (*merge.GetMergeStateResp, error)
}

// OperatorIndex updates the global OperatorActor index on Run transitions and deletion.
type OperatorIndex interface {
	UpsertRun(ctx context.Context, req *UpsertRunReq) (*UpsertRunResp, error)
	RemoveRun(ctx context.Context, req *RemoveRunReq) (*RemoveRunResp, error)
}

// ConfigManager resolves credential references and cleans up Run-scoped config on deletion.
type ConfigManager interface {
	ResolveForNode(ctx context.Context, req *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error)
	DeleteRunScope(ctx context.Context, req *config.DeleteRunScopeReq) (*config.DeleteRunScopeResp, error)
}

// EventPublisher emits lifecycle events to a pubsub topic.
type EventPublisher interface {
	PublishEvent(ctx context.Context, pubsubName, topic string, data any) error
}

// ReminderScheduler registers and removes RunActor reconciliation reminders.
type ReminderScheduler interface {
	EnsureReconcileReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error
	UnregisterReconcileReminder(ctx context.Context, actorType, actorID, reminderName string) error
}

// RunReconciler wakes active RunActors during OperatorActor recovery.
type RunReconciler interface {
	Reconcile(ctx context.Context, runID string, req *ReconcileReq) (*ReconcileResp, error)
}

// ProcessLister enumerates registered worker processes from ProcessRegistryActor.
type ProcessLister interface {
	ListProcesses(ctx context.Context, req *registry.ListProcessesReq) (*registry.ListProcessesResp, error)
}

// WorkerBundleCleaner invokes worker-side run bundle cleanup ("cleanup_run").
type WorkerBundleCleaner interface {
	CleanupRun(ctx context.Context, appID string, req daemon.CleanupRunRequest) (daemon.CleanupRunResponse, error)
}

// RunClient provides a lossless client facade over RunActor (both direct and Dapr-backed).
type RunClient interface {
	StartRun(ctx context.Context, req *StartRunReq) (*StartRunResp, error)
	PauseRun(ctx context.Context, req *PauseRunReq) (*PauseRunResp, error)
	ResumeRun(ctx context.Context, req *ResumeRunReq) (*ResumeRunResp, error)
	RerunNode(ctx context.Context, req *RerunNodeReq) (*RerunNodeResp, error)
	DeleteRun(ctx context.Context, req *DeleteRunReq) (*DeleteRunResp, error)
	GetRunState(ctx context.Context, req *GetRunStateReq) (*GetRunStateResp, error)
	ListDecisions(ctx context.Context, req *ListDecisionsReq) (*ListDecisionsResp, error)
}

// OperatorClientStub wraps Dapr Actor RPCs for OperatorActor("global").
type OperatorClientStub struct {
	ActorID           string
	UpsertRun         func(context.Context, *UpsertRunReq) (*UpsertRunResp, error)
	GetRunSummary     func(context.Context, *GetRunSummaryReq) (*GetRunSummaryResp, error)
	ListRuns          func(context.Context, *ListRunsReq) (*ListRunsResp, error)
	RemoveRun         func(context.Context, *RemoveRunReq) (*RemoveRunResp, error)
	RecoverActiveRuns func(context.Context, *RecoverActiveRunsReq) (*RecoverActiveRunsResp, error)
}

func (c *OperatorClientStub) Type() string { return OperatorActorType }
func (c *OperatorClientStub) ID() string   { return c.ActorID }

// NewOperatorClientStub constructs an OperatorActor client stub bound to client.
func NewOperatorClientStub(client daprclient.Client) *OperatorClientStub {
	stub := &OperatorClientStub{ActorID: GlobalOperatorActorID}
	if client != nil {
		client.ImplActorClientStub(stub)
	}
	return stub
}

// RunClientStub wraps Dapr Actor RPCs for RunActor(<run_id>).
type RunClientStub struct {
	ActorID                string
	StartRun               func(context.Context, *StartRunReq) (*StartRunResp, error)
	Reconcile              func(context.Context, *ReconcileReq) (*ReconcileResp, error)
	RegisterManagedBranch  func(context.Context, *RegisterManagedBranchReq) (*RegisterManagedBranchResp, error)
	OnNodeEvent            func(context.Context, *node.NodeEventNotification) (*OnNodeEventResp, error)
	OnMergeCandidateResult func(context.Context, *merge.MergeCandidateNotification) (*OnMergeCandidateResultResp, error)
	PauseRun               func(context.Context, *PauseRunReq) (*PauseRunResp, error)
	ResumeRun              func(context.Context, *ResumeRunReq) (*ResumeRunResp, error)
	RerunNode              func(context.Context, *RerunNodeReq) (*RerunNodeResp, error)
	DeleteRun              func(context.Context, *DeleteRunReq) (*DeleteRunResp, error)
	GetRunState            func(context.Context, *GetRunStateReq) (*GetRunStateResp, error)
	ListDecisions          func(context.Context, *ListDecisionsReq) (*ListDecisionsResp, error)
}

func (c *RunClientStub) Type() string { return RunActorType }
func (c *RunClientStub) ID() string   { return c.ActorID }

// NewRunClientStub constructs a RunActor client stub for runID bound to client.
func NewRunClientStub(runID string, client daprclient.Client) *RunClientStub {
	stub := &RunClientStub{ActorID: runID}
	if client != nil {
		client.ImplActorClientStub(stub)
	}
	return stub
}

type directRunClient struct {
	actor *RunActor
}

// NewDirectRunClient wraps an in-process RunActor with lossless domain error translation.
func NewDirectRunClient(a *RunActor) RunClient {
	return &directRunClient{actor: a}
}

func (c *directRunClient) StartRun(ctx context.Context, req *StartRunReq) (*StartRunResp, error) {
	resp, err := c.actor.StartRun(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.ErrorCode != "" {
		return resp, mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return resp, nil
}

func (c *directRunClient) PauseRun(ctx context.Context, req *PauseRunReq) (*PauseRunResp, error) {
	resp, err := c.actor.PauseRun(ctx, req)
	if decErr := DecodePauseRunError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *directRunClient) ResumeRun(ctx context.Context, req *ResumeRunReq) (*ResumeRunResp, error) {
	resp, err := c.actor.ResumeRun(ctx, req)
	if decErr := DecodeResumeRunError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *directRunClient) RerunNode(ctx context.Context, req *RerunNodeReq) (*RerunNodeResp, error) {
	resp, err := c.actor.RerunNode(ctx, req)
	if decErr := DecodeRerunNodeError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *directRunClient) DeleteRun(ctx context.Context, req *DeleteRunReq) (*DeleteRunResp, error) {
	resp, err := c.actor.DeleteRun(ctx, req)
	if decErr := DecodeDeleteRunError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *directRunClient) GetRunState(ctx context.Context, req *GetRunStateReq) (*GetRunStateResp, error) {
	resp, err := c.actor.GetRunState(ctx, req)
	if decErr := DecodeRunStateError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *directRunClient) ListDecisions(ctx context.Context, req *ListDecisionsReq) (*ListDecisionsResp, error) {
	resp, err := c.actor.ListDecisions(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.ErrorCode != "" {
		return resp, mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return resp, nil
}

type daprRunClient struct {
	stub *RunClientStub
}

// NewDaprRunClient wraps a RunActor Dapr ClientStub with lossless domain error translation.
func NewDaprRunClient(runID string, client daprclient.Client) (RunClient, error) {
	if client == nil {
		return nil, errors.New("run: dapr client is required")
	}
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run: run_id is required")
	}
	return &daprRunClient{stub: NewRunClientStub(runID, client)}, nil
}

func (c *daprRunClient) StartRun(ctx context.Context, req *StartRunReq) (*StartRunResp, error) {
	resp, err := c.stub.StartRun(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.ErrorCode != "" {
		return resp, mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return resp, nil
}

func (c *daprRunClient) PauseRun(ctx context.Context, req *PauseRunReq) (*PauseRunResp, error) {
	resp, err := c.stub.PauseRun(ctx, req)
	if decErr := DecodePauseRunError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *daprRunClient) ResumeRun(ctx context.Context, req *ResumeRunReq) (*ResumeRunResp, error) {
	resp, err := c.stub.ResumeRun(ctx, req)
	if decErr := DecodeResumeRunError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *daprRunClient) RerunNode(ctx context.Context, req *RerunNodeReq) (*RerunNodeResp, error) {
	resp, err := c.stub.RerunNode(ctx, req)
	if decErr := DecodeRerunNodeError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *daprRunClient) DeleteRun(ctx context.Context, req *DeleteRunReq) (*DeleteRunResp, error) {
	resp, err := c.stub.DeleteRun(ctx, req)
	if decErr := DecodeDeleteRunError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *daprRunClient) GetRunState(ctx context.Context, req *GetRunStateReq) (*GetRunStateResp, error) {
	resp, err := c.stub.GetRunState(ctx, req)
	if decErr := DecodeRunStateError(resp, err); decErr != nil {
		return resp, decErr
	}
	return resp, nil
}

func (c *daprRunClient) ListDecisions(ctx context.Context, req *ListDecisionsReq) (*ListDecisionsResp, error) {
	resp, err := c.stub.ListDecisions(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.ErrorCode != "" {
		return resp, mapRunErrorCode(resp.ErrorCode, resp.ErrorMessage)
	}
	return resp, nil
}

// Dapr-backed adapters for cross-actor communication.

type daprNodeController struct {
	clientProvider func() daprclient.Client
}

// NewDaprNodeController constructs a NodeController backed by Dapr actor invocations.
func NewDaprNodeController(clientProvider func() daprclient.Client) NodeController {
	return &daprNodeController{clientProvider: clientProvider}
}

func (n *daprNodeController) stub(runID, canonicalNodeAddr string) (*node.ClientStub, error) {
	c := n.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	return node.NewClientStubForNode(runID, canonicalNodeAddr, c)
}

func (n *daprNodeController) StartOrResume(ctx context.Context, runID, canonicalNodeAddr string, req *node.StartOrResumeReq) (*node.StartOrResumeResp, error) {
	stub, err := n.stub(runID, canonicalNodeAddr)
	if err != nil {
		return nil, err
	}
	return stub.StartOrResume(ctx, req)
}

func (n *daprNodeController) PauseNode(ctx context.Context, runID, canonicalNodeAddr string, req *node.PauseNodeReq) (*node.PauseNodeResp, error) {
	stub, err := n.stub(runID, canonicalNodeAddr)
	if err != nil {
		return nil, err
	}
	return stub.PauseNode(ctx, req)
}

func (n *daprNodeController) AdvanceForRework(ctx context.Context, runID, canonicalNodeAddr string, req *node.AdvanceForReworkReq) (*node.AdvanceForReworkResp, error) {
	stub, err := n.stub(runID, canonicalNodeAddr)
	if err != nil {
		return nil, err
	}
	return stub.AdvanceForRework(ctx, req)
}

func (n *daprNodeController) ResetForRerun(ctx context.Context, runID, canonicalNodeAddr string, req *node.ResetForRerunReq) (*node.ResetForRerunResp, error) {
	stub, err := n.stub(runID, canonicalNodeAddr)
	if err != nil {
		return nil, err
	}
	return stub.ResetForRerun(ctx, req)
}

func (n *daprNodeController) GetNodeState(ctx context.Context, runID, canonicalNodeAddr string) (*node.GetNodeStateResp, error) {
	stub, err := n.stub(runID, canonicalNodeAddr)
	if err != nil {
		return nil, err
	}
	return stub.GetState(ctx, &node.GetNodeStateReq{})
}

type daprMergeController struct {
	clientProvider func() daprclient.Client
}

// NewDaprMergeController constructs a MergeController backed by Dapr actor invocations.
func NewDaprMergeController(clientProvider func() daprclient.Client) MergeController {
	return &daprMergeController{clientProvider: clientProvider}
}

func (m *daprMergeController) stub(repoURL, effectiveTargetBranch string) (*merge.ClientStub, error) {
	c := m.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	return merge.NewClientStubForTarget(repoURL, effectiveTargetBranch, c)
}

func (m *daprMergeController) EnqueueCandidate(ctx context.Context, repoURL, effectiveTargetBranch string, req *merge.EnqueueCandidateReq) (*merge.EnqueueCandidateResp, error) {
	stub, err := m.stub(repoURL, effectiveTargetBranch)
	if err != nil {
		return nil, err
	}
	return stub.EnqueueCandidate(ctx, req)
}

func (m *daprMergeController) CancelByRun(ctx context.Context, repoURL, effectiveTargetBranch string, req *merge.CancelByRunReq) (*merge.CancelByRunResp, error) {
	stub, err := m.stub(repoURL, effectiveTargetBranch)
	if err != nil {
		return nil, err
	}
	return stub.CancelByRun(ctx, req)
}

func (m *daprMergeController) GetMergeState(ctx context.Context, repoURL, effectiveTargetBranch string) (*merge.GetMergeStateResp, error) {
	stub, err := m.stub(repoURL, effectiveTargetBranch)
	if err != nil {
		return nil, err
	}
	return stub.GetState(ctx, &merge.GetMergeStateReq{})
}

type daprOperatorIndex struct {
	clientProvider func() daprclient.Client
}

// NewDaprOperatorIndex constructs an OperatorIndex backed by Dapr actor invocations.
func NewDaprOperatorIndex(clientProvider func() daprclient.Client) OperatorIndex {
	return &daprOperatorIndex{clientProvider: clientProvider}
}

func (o *daprOperatorIndex) UpsertRun(ctx context.Context, req *UpsertRunReq) (*UpsertRunResp, error) {
	c := o.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	return NewOperatorClientStub(c).UpsertRun(ctx, req)
}

func (o *daprOperatorIndex) RemoveRun(ctx context.Context, req *RemoveRunReq) (*RemoveRunResp, error) {
	c := o.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	return NewOperatorClientStub(c).RemoveRun(ctx, req)
}

type daprConfigManager struct {
	clientProvider func() daprclient.Client
}

// NewDaprConfigManager constructs a ConfigManager backed by ConfigActor("global") Dapr RPCs.
func NewDaprConfigManager(clientProvider func() daprclient.Client) ConfigManager {
	return &daprConfigManager{clientProvider: clientProvider}
}

func (cm *daprConfigManager) ResolveForNode(ctx context.Context, req *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error) {
	c := cm.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: config.ActorType,
		ActorID:   config.GlobalActorID,
		Method:    "ResolveForNode",
		Data:      data,
	})
	if err != nil {
		return nil, err
	}
	var out config.ResolveForNodeResp
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return nil, fmt.Errorf("decode config ResolveForNode response: %w", err)
		}
	}
	return &out, nil
}

func (cm *daprConfigManager) DeleteRunScope(ctx context.Context, req *config.DeleteRunScopeReq) (*config.DeleteRunScopeResp, error) {
	c := cm.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: config.ActorType,
		ActorID:   config.GlobalActorID,
		Method:    "DeleteRunScope",
		Data:      data,
	})
	if err != nil {
		return nil, err
	}
	var out config.DeleteRunScopeResp
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return nil, fmt.Errorf("decode config DeleteRunScope response: %w", err)
		}
	}
	return &out, nil
}

type daprEventPublisher struct {
	clientProvider func() daprclient.Client
}

// NewDaprEventPublisher constructs an EventPublisher backed by Dapr PublishEvent.
func NewDaprEventPublisher(clientProvider func() daprclient.Client) EventPublisher {
	return &daprEventPublisher{clientProvider: clientProvider}
}

func (p *daprEventPublisher) PublishEvent(ctx context.Context, pubsubName, topic string, data any) error {
	c := p.clientProvider()
	if c == nil {
		return nil
	}
	if strings.TrimSpace(pubsubName) == "" {
		pubsubName = DefaultPubSubName
	}
	if strings.TrimSpace(topic) == "" {
		topic = DefaultRunEventsTopic
	}
	return c.PublishEvent(ctx, pubsubName, topic, data)
}

type daprReminderScheduler struct {
	clientProvider func() daprclient.Client
}

// NewDaprReminderScheduler constructs a ReminderScheduler backed by Dapr actor reminders.
func NewDaprReminderScheduler(clientProvider func() daprclient.Client) ReminderScheduler {
	return &daprReminderScheduler{clientProvider: clientProvider}
}

func (r *daprReminderScheduler) EnsureReconcileReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error {
	c := r.clientProvider()
	if c == nil {
		return errors.New("run: dapr client unavailable")
	}
	return c.RegisterActorReminder(ctx, &daprclient.RegisterActorReminderRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Name:      reminderName,
		DueTime:   dueTime.String(),
		Period:    period.String(),
	})
}

func (r *daprReminderScheduler) UnregisterReconcileReminder(ctx context.Context, actorType, actorID, reminderName string) error {
	c := r.clientProvider()
	if c == nil {
		return errors.New("run: dapr client unavailable")
	}
	return c.UnregisterActorReminder(ctx, &daprclient.UnregisterActorReminderRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Name:      reminderName,
	})
}

type daprRunReconciler struct {
	clientProvider func() daprclient.Client
}

// NewDaprRunReconciler constructs a RunReconciler backed by RunActor.Reconcile Dapr invocations.
func NewDaprRunReconciler(clientProvider func() daprclient.Client) RunReconciler {
	return &daprRunReconciler{clientProvider: clientProvider}
}

func (r *daprRunReconciler) Reconcile(ctx context.Context, runID string, req *ReconcileReq) (*ReconcileResp, error) {
	c := r.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	if req == nil {
		req = &ReconcileReq{}
	}
	return NewRunClientStub(runID, c).Reconcile(ctx, req)
}

type daprProcessLister struct {
	clientProvider func() daprclient.Client
}

// NewDaprProcessLister constructs a ProcessLister backed by ProcessRegistryActor("global").
func NewDaprProcessLister(clientProvider func() daprclient.Client) ProcessLister {
	return &daprProcessLister{clientProvider: clientProvider}
}

func (l *daprProcessLister) ListProcesses(ctx context.Context, req *registry.ListProcessesReq) (*registry.ListProcessesResp, error) {
	c := l.clientProvider()
	if c == nil {
		return nil, errors.New("run: dapr client unavailable")
	}
	if req == nil {
		req = &registry.ListProcessesReq{}
	}
	return registry.NewClientStub(c).ListProcesses(ctx, req)
}

type daprWorkerBundleCleaner struct {
	clientProvider func() daprclient.Client
}

// NewDaprWorkerBundleCleaner constructs a WorkerBundleCleaner backed by Dapr Service Invocation.
func NewDaprWorkerBundleCleaner(clientProvider func() daprclient.Client) WorkerBundleCleaner {
	return &daprWorkerBundleCleaner{clientProvider: clientProvider}
}

func (w *daprWorkerBundleCleaner) CleanupRun(ctx context.Context, appID string, req daemon.CleanupRunRequest) (daemon.CleanupRunResponse, error) {
	c := w.clientProvider()
	if c == nil {
		return daemon.CleanupRunResponse{}, errors.New("run: dapr client unavailable")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return daemon.CleanupRunResponse{}, err
	}
	raw, err := c.InvokeMethodWithContent(ctx, appID, "cleanup_run", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        payload,
	})
	if err != nil {
		return daemon.CleanupRunResponse{}, err
	}
	var resp daemon.CleanupRunResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return daemon.CleanupRunResponse{}, fmt.Errorf("decode cleanup_run response: %w", err)
		}
	}
	return resp, nil
}
