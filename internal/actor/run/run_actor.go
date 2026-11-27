package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	actorcfg "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/applyeval"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

const runStateKey = "run_actor_v1"

// RunOptions configures RunActor dependencies and defaults.
type RunOptions struct {
	ClientProvider    func() daprclient.Client
	BlobStore         blob.Store
	BundleCacheRoot   string
	NodeController    NodeController
	MergeController   MergeController
	OperatorIndex     OperatorIndex
	ConfigManager     ConfigManager
	EventPublisher    EventPublisher
	ReminderScheduler ReminderScheduler
	GitAdapter        git.Adapter
	PubSubName        string
	EventsTopic       string
	ReconcileInterval time.Duration
	ReconcileReminder string
	Clock             func() time.Time
}

type sourceRecord struct {
	Address          string  `json:"address"`
	CanonicalAddress string  `json:"canonical_address"`
	Name             string  `json:"name"`
	URL              string  `json:"url"`
	Branch           string  `json:"branch"`
	CommitSHA        string  `json:"commit_sha"`
	TokenRef         *string `json:"token_ref,omitempty"`
	SSHKeyRef        *string `json:"ssh_key_ref,omitempty"`
}

type nodeStateRecord struct {
	Address                  string                       `json:"address"`
	CanonicalAddress         string                       `json:"canonical_address"`
	Kind                     string                       `json:"kind"`
	ModulePath               string                       `json:"module_path,omitempty"`
	ParentDynamicAddr        string                       `json:"parent_dynamic_addr,omitempty"`
	DynamicKey               string                       `json:"dynamic_key,omitempty"`
	Status                   node.NodeStatus              `json:"status"`
	CurrentEpoch             uint64                       `json:"current_epoch"`
	MergeEpoch               uint64                       `json:"merge_epoch,omitempty"`
	BasedSource              string                       `json:"based_source,omitempty"`
	BaseSHA                  string                       `json:"base_sha,omitempty"`
	RepoURL                  string                       `json:"repo_url,omitempty"`
	WorkBranch               string                       `json:"work_branch,omitempty"`
	CommitSHA                string                       `json:"commit_sha,omitempty"`
	Result                   *string                      `json:"result,omitempty"`
	Artifacts                []artifact.PublishedArtifact `json:"artifacts,omitempty"`
	MergedIntoSharedBranch   bool                         `json:"merged_into_shared_branch,omitempty"`
	DynamicExpanded          bool                         `json:"dynamic_expanded,omitempty"`
	DynamicKeys              []string                     `json:"dynamic_keys,omitempty"`
	DynamicElements          map[string]WireValue         `json:"dynamic_elements,omitempty"`
	TargetBranch             string                       `json:"target_branch,omitempty"`
	SharedTargetBranch       bool                         `json:"shared_target_branch,omitempty"`
	LatestTargetSHA          string                       `json:"latest_target_sha,omitempty"`
	ExpectedSources          []string                     `json:"expected_sources,omitempty"`
	SourcesKnown             bool                         `json:"sources_known,omitempty"`
	EnqueuedSources          map[string]string            `json:"enqueued_sources,omitempty"`
	ReworkCounts             map[string]int               `json:"rework_counts,omitempty"`
	BoundInputs              map[string]WireValue         `json:"bound_inputs,omitempty"`
	ModuleOutputs            map[string]EvaluatedOutput   `json:"module_outputs,omitempty"`
	FallbackCounts           map[string]int               `json:"fallback_counts,omitempty"`
	PendingFallbackReason    string                       `json:"pending_fallback_reason,omitempty"`
	PendingFallbackArtifacts []artifact.PublishedArtifact `json:"pending_fallback_artifacts,omitempty"`
	ErrorMessage             string                       `json:"error_message,omitempty"`
}

type persistedRunState struct {
	RunID                 string                                `json:"run_id"`
	Namespace             string                                `json:"namespace"`
	Status                RunStatus                             `json:"status"`
	ManualPaused          bool                                  `json:"manual_paused"`
	ErrorMessage          string                                `json:"error_message,omitempty"`
	PlanBlobKey           string                                `json:"plan_blob_key,omitempty"`
	PlanSHA256            string                                `json:"plan_sha256,omitempty"`
	BundleExtractDir      string                                `json:"bundle_extract_dir,omitempty"`
	RunOverrides          map[string]string                     `json:"run_overrides,omitempty"`
	RequiredArch          string                                `json:"required_arch,omitempty"`
	HostOS                string                                `json:"host_os,omitempty"`
	TargetPlatform        *harnessinstaller.Platform            `json:"target_platform,omitempty"`
	PluginResolve         daemon.PluginResolveConfig            `json:"plugin_resolve,omitempty"`
	TempRoot              string                                `json:"temp_root,omitempty"`
	MaxGatekeeperReworks  int                                   `json:"max_gatekeeper_reworks,omitempty"`
	MaxArtifactReworks    int                                   `json:"max_artifact_reworks,omitempty"`
	StallTimeout          time.Duration                         `json:"stall_timeout,omitempty"`
	WatchdogCheckInterval time.Duration                         `json:"watchdog_check_interval,omitempty"`
	Sources               map[string]sourceRecord               `json:"sources,omitempty"`
	Ledger                git.LedgerSnapshot                    `json:"ledger,omitempty"`
	Nodes                 map[string]*nodeStateRecord           `json:"nodes,omitempty"`
	PublishedArtifacts    map[string]artifact.PublishedArtifact `json:"published_artifacts,omitempty"`
	EvaluatedPreserves    map[string]git.PreserveRef            `json:"evaluated_preserves,omitempty"`
	Outputs               map[string]EvaluatedOutput            `json:"outputs,omitempty"`
	CreatedAt             time.Time                             `json:"created_at"`
	UpdatedAt             time.Time                             `json:"updated_at"`
}

// RunActor orchestrates a single Run DAG non-blockingly across NodeActors and MergeActors.
type RunActor struct {
	actor.ServerImplBaseCtx

	mu            sync.Mutex
	opts          RunOptions
	cachedArchive *plan.LoadedArchive
	reconciling   bool
	reconcileWake bool
}

var _ actor.ReminderCallee = (*RunActor)(nil)

// NewRunActor constructs a RunActor instance.
func NewRunActor(opts RunOptions) *RunActor {
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = DefaultReconcileInterval
	}
	if strings.TrimSpace(opts.ReconcileReminder) == "" {
		opts.ReconcileReminder = DefaultReconcileReminderName
	}
	if strings.TrimSpace(opts.PubSubName) == "" {
		opts.PubSubName = DefaultPubSubName
	}
	if strings.TrimSpace(opts.EventsTopic) == "" {
		opts.EventsTopic = DefaultRunEventsTopic
	}
	if opts.NodeController == nil && opts.ClientProvider != nil {
		opts.NodeController = NewDaprNodeController(opts.ClientProvider)
	}
	if opts.MergeController == nil && opts.ClientProvider != nil {
		opts.MergeController = NewDaprMergeController(opts.ClientProvider)
	}
	if opts.OperatorIndex == nil && opts.ClientProvider != nil {
		opts.OperatorIndex = NewDaprOperatorIndex(opts.ClientProvider)
	}
	if opts.ConfigManager == nil && opts.ClientProvider != nil {
		opts.ConfigManager = NewDaprConfigManager(opts.ClientProvider)
	}
	if opts.EventPublisher == nil && opts.ClientProvider != nil {
		opts.EventPublisher = NewDaprEventPublisher(opts.ClientProvider)
	}
	if opts.ReminderScheduler == nil && opts.ClientProvider != nil {
		opts.ReminderScheduler = NewDaprReminderScheduler(opts.ClientProvider)
	}
	if opts.GitAdapter == nil {
		opts.GitAdapter = git.NewCLIAdapter(git.CLIAdapterOptions{})
	}
	return &RunActor{opts: opts}
}

// Type implements actor.ServerContext.
func (a *RunActor) Type() string {
	return RunActorType
}

// SetStateManager binds daprhost.NewActorStateManager when ClientProvider yields a Dapr client,
// preserving direct StateManagerContext injection in Tier 1 unit tests.
func (a *RunActor) SetStateManager(sm actor.StateManagerContext) {
	if a.opts.ClientProvider != nil {
		if c := a.opts.ClientProvider(); c != nil {
			if bound := daprhost.NewActorStateManager(a.Type(), a.ID(), c); bound != nil {
				a.ServerImplBaseCtx.SetStateManager(bound)
				return
			}
		}
	}
	a.ServerImplBaseCtx.SetStateManager(sm)
}

// ReminderCall implements actor.ReminderCallee. State mutations are saved explicitly.
func (a *RunActor) ReminderCall(reminderName string, _ []byte, _, _ string) {
	if reminderName != a.opts.ReconcileReminder {
		return
	}
	ctx := context.Background()
	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	a.mu.Unlock()
	if err != nil || !exists {
		return
	}
	if isTerminalRunStatus(st.Status) || st.Status == RunStatusPaused || st.Status == RunStatusDeleted {
		if a.opts.ReminderScheduler != nil && a.ID() != "" {
			_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
		}
		return
	}
	_, _ = a.Reconcile(ctx, &ReconcileReq{Reason: "reminder"})
}

func (a *RunActor) loadStateLocked(ctx context.Context) (persistedRunState, bool, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return persistedRunState{}, false, errors.New("run: state manager not initialized")
	}
	ok, err := sm.Contains(ctx, runStateKey)
	if err != nil {
		return persistedRunState{}, false, fmt.Errorf("run: check state: %w", err)
	}
	if !ok {
		return persistedRunState{}, false, nil
	}
	var st persistedRunState
	if err := sm.Get(ctx, runStateKey, &st); err != nil {
		return persistedRunState{}, false, fmt.Errorf("run: load state: %w", err)
	}
	if st.Nodes == nil {
		st.Nodes = make(map[string]*nodeStateRecord)
	}
	if st.Sources == nil {
		st.Sources = make(map[string]sourceRecord)
	}
	if st.PublishedArtifacts == nil {
		st.PublishedArtifacts = make(map[string]artifact.PublishedArtifact)
	}
	if st.EvaluatedPreserves == nil {
		st.EvaluatedPreserves = make(map[string]git.PreserveRef)
	}
	if st.Outputs == nil {
		st.Outputs = make(map[string]EvaluatedOutput)
	}
	return st, true, nil
}

func (a *RunActor) saveStateLocked(ctx context.Context, st persistedRunState) error {
	sm := a.GetStateManager()
	if sm == nil {
		return errors.New("run: state manager not initialized")
	}
	st.UpdatedAt = a.opts.Clock()
	if err := sm.Set(ctx, runStateKey, st); err != nil {
		return fmt.Errorf("run: stage state: %w", err)
	}
	if err := sm.Save(ctx); err != nil {
		return fmt.Errorf("run: save state: %w", err)
	}
	return nil
}

func (a *RunActor) runIDFallback(reqRunID string) string {
	if id := strings.TrimSpace(a.ID()); id != "" {
		return id
	}
	return strings.TrimSpace(reqRunID)
}

func (a *RunActor) syncOperatorAndEvents(ctx context.Context, st persistedRunState) {
	if a.opts.OperatorIndex != nil && st.Status != RunStatusDeleted {
		_, _ = a.opts.OperatorIndex.UpsertRun(ctx, &UpsertRunReq{
			Summary: summaryFromState(st),
		})
	}
	if a.opts.EventPublisher != nil {
		_ = a.opts.EventPublisher.PublishEvent(ctx, a.opts.PubSubName, a.opts.EventsTopic, map[string]any{
			"run_id":        st.RunID,
			"namespace":     st.Namespace,
			"status":        st.Status,
			"error_message": st.ErrorMessage,
			"updated_at":    st.UpdatedAt,
		})
	}
}

func summaryFromState(st persistedRunState) RunSummary {
	return RunSummary{
		RunID:        st.RunID,
		Namespace:    st.Namespace,
		Status:       st.Status,
		PlanBlobKey:  st.PlanBlobKey,
		PlanSHA256:   st.PlanSHA256,
		ErrorMessage: st.ErrorMessage,
		CreatedAt:    st.CreatedAt,
		UpdatedAt:    st.UpdatedAt,
	}
}

func isTerminalRunStatus(s RunStatus) bool {
	return s == RunStatusSucceeded || s == RunStatusFailed || s == RunStatusDeleted
}

// StartRun initializes a Run from its .sofaplan archive, pins source SHAs, seeds git.Ledger,
// registers the Run in OperatorActor, starts the reconcile reminder, and dispatches ready nodes.
func (a *RunActor) StartRun(ctx context.Context, req *StartRunReq) (*StartRunResp, error) {
	if req == nil {
		return nil, errors.New("run: StartRun request is nil")
	}
	runID := a.runIDFallback(req.RunID)
	if runID == "" {
		return &StartRunResp{
			Accepted:     false,
			ErrorCode:    ErrCodeInvalidInput,
			ErrorMessage: "run_id is required",
		}, nil
	}
	planKey := strings.TrimSpace(req.PlanBlobKey)
	if planKey == "" {
		planKey = fmt.Sprintf("runs/%s/plan/plan.sofaplan", runID)
	}

	a.mu.Lock()
	existing, exists, err := a.loadStateLocked(ctx)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if exists && existing.Status != "" {
		return &StartRunResp{
			Accepted: true,
			RunID:    existing.RunID,
			Status:   existing.Status,
		}, nil
	}

	loaded, extractDir, err := a.downloadAndExtractArchive(ctx, runID, planKey, req.PlanSHA256)
	if err != nil {
		return &StartRunResp{
			Accepted:     false,
			RunID:        runID,
			Status:       RunStatusFailed,
			ErrorCode:    ErrCodeInvalidInput,
			ErrorMessage: err.Error(),
		}, nil
	}

	namespace := normalizeNamespace(req.Namespace)
	sources, protectedRefs, err := a.resolvePlanSources(ctx, runID, namespace, req.RunOverrides, loaded.Plan)
	if err != nil {
		return &StartRunResp{
			Accepted:     false,
			RunID:        runID,
			Status:       RunStatusFailed,
			ErrorCode:    ErrCodeInvalidInput,
			ErrorMessage: err.Error(),
		}, nil
	}

	ledger := git.NewLedger(protectedRefs...)
	now := a.opts.Clock()
	st := persistedRunState{
		RunID:                 runID,
		Namespace:             namespace,
		Status:                RunStatusRunning,
		PlanBlobKey:           planKey,
		PlanSHA256:            strings.TrimSpace(req.PlanSHA256),
		BundleExtractDir:      extractDir,
		RunOverrides:          maps.Clone(req.RunOverrides),
		RequiredArch:          req.RequiredArch,
		HostOS:                req.HostOS,
		TargetPlatform:        req.TargetPlatform,
		PluginResolve:         req.PluginResolve,
		TempRoot:              req.TempRoot,
		MaxGatekeeperReworks:  req.MaxGatekeeperReworks,
		MaxArtifactReworks:    req.MaxArtifactReworks,
		StallTimeout:          req.StallTimeout,
		WatchdogCheckInterval: req.WatchdogCheckInterval,
		Sources:               sources,
		Ledger:                ledger.Snapshot(),
		Nodes:                 make(map[string]*nodeStateRecord),
		PublishedArtifacts:    make(map[string]artifact.PublishedArtifact),
		EvaluatedPreserves:    make(map[string]git.PreserveRef),
		Outputs:               make(map[string]EvaluatedOutput),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	seedPlanNodesRecursive(&st, loaded.Plan, "")

	a.mu.Lock()
	// Re-check under lock in case StartRun raced with itself.
	if again, okAgain, lErr := a.loadStateLocked(ctx); lErr == nil && okAgain && again.Status != "" {
		a.mu.Unlock()
		return &StartRunResp{Accepted: true, RunID: again.RunID, Status: again.Status}, nil
	}
	a.cachedArchive = loaded
	if err := a.saveStateLocked(ctx, st); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	a.syncOperatorAndEvents(ctx, st)
	if a.opts.ReminderScheduler != nil && a.ID() != "" {
		_ = a.opts.ReminderScheduler.EnsureReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder, a.opts.ReconcileInterval, a.opts.ReconcileInterval)
	}

	recResp, err := a.Reconcile(ctx, &ReconcileReq{Reason: "start_run"})
	if err != nil {
		return nil, err
	}
	return &StartRunResp{
		Accepted:     true,
		RunID:        runID,
		Status:       recResp.Status,
		ErrorCode:    recResp.ErrorCode,
		ErrorMessage: recResp.ErrorMessage,
	}, nil
}

func (a *RunActor) downloadAndExtractArchive(ctx context.Context, runID, planKey, expectedSHA string) (*plan.LoadedArchive, string, error) {
	if a.opts.BlobStore == nil {
		return nil, "", errors.New("blob store is required to load .sofaplan")
	}
	rc, _, err := a.opts.BlobStore.Get(ctx, planKey, strings.TrimSpace(expectedSHA))
	if err != nil {
		return nil, "", fmt.Errorf("fetch plan %q from blob store: %w", planKey, err)
	}
	defer func() { _ = rc.Close() }()

	tmpFile, err := os.CreateTemp("", "sofa-plan-*.sofaplan")
	if err != nil {
		return nil, "", fmt.Errorf("create temp plan file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, hasher), rc); err != nil {
		return nil, "", fmt.Errorf("stream plan %q: %w", planKey, err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, "", fmt.Errorf("close temp plan file: %w", err)
	}
	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if exp := strings.TrimSpace(expectedSHA); exp != "" && !strings.EqualFold(actualSHA, exp) {
		return nil, "", fmt.Errorf("plan sha256 mismatch: expected %s, got %s", exp, actualSHA)
	}

	cacheRoot := strings.TrimSpace(a.opts.BundleCacheRoot)
	if cacheRoot == "" {
		cacheRoot = filepath.Join(os.TempDir(), "sofa-bundles")
	}
	extractDir := filepath.Join(cacheRoot, sanitizePathSegment(runID)+"-"+actualSHA[:12])
	loaded, err := plan.LoadArchive(tmpPath, extractDir, plan.LoadArchiveOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("load plan archive: %w", err)
	}
	return loaded, extractDir, nil
}

func (a *RunActor) ensureArchiveLoaded(ctx context.Context, st persistedRunState) (*plan.LoadedArchive, error) {
	a.mu.Lock()
	cached := a.cachedArchive
	a.mu.Unlock()
	if cached != nil && cached.Plan != nil {
		return cached, nil
	}
	loaded, _, err := a.downloadAndExtractArchive(ctx, st.RunID, st.PlanBlobKey, st.PlanSHA256)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.cachedArchive == nil {
		a.cachedArchive = loaded
	} else {
		loaded = a.cachedArchive
	}
	a.mu.Unlock()
	return loaded, nil
}

func (a *RunActor) resolvePlanSources(
	ctx context.Context,
	runID, namespace string,
	runOverrides map[string]string,
	root *plan.Plan,
) (map[string]sourceRecord, []git.SourceRef, error) {
	out := make(map[string]sourceRecord)
	refs := make([]git.SourceRef, 0)
	var collect func(p *plan.Plan, prefix string) error
	collect = func(p *plan.Plan, prefix string) error {
		if p == nil {
			return nil
		}
		for _, n := range p.Nodes() {
			canon := canonicalForNode(n, prefix)
			switch n.Kind {
			case "source":
				urlStr := readCtyStringAttr(n.Config, "url")
				branch := readCtyStringAttr(n.Config, "branch")
				name := nodeSimpleName(n)
				tokenRef := readOptionalCtyStringPtr(n.Config, "token_ref")
				sshKeyRef := readOptionalCtyStringPtr(n.Config, "ssh_key_ref")
				auth := git.AuthConfig{TokenRef: tokenRef, SSHKeyRef: sshKeyRef}
				var envKeys []string
				if tokenRef != nil {
					envKeys = append(envKeys, *tokenRef)
				}
				if sshKeyRef != nil {
					envKeys = append(envKeys, *sshKeyRef)
				}
				if len(envKeys) > 0 && a.opts.ConfigManager != nil {
					resolved, err := a.opts.ConfigManager.ResolveForNode(ctx, &actorcfg.ResolveForNodeReq{
						RunID:        runID,
						Namespace:    namespace,
						RunOverrides: runOverrides,
						Keys:         envKeys,
					})
					if err != nil {
						return fmt.Errorf("resolve source credentials for %s: %w", canon, err)
					}
					if len(resolved.MissingKeys) > 0 {
						return fmt.Errorf("missing source credentials for %s: %s", canon, strings.Join(resolved.MissingKeys, ", "))
					}
					if tokenRef != nil {
						v := resolved.Values[*tokenRef]
						auth.TokenRef = &v
					}
					if sshKeyRef != nil {
						v := resolved.Values[*sshKeyRef]
						auth.SSHKeyRef = &v
					}
				}
				commitSHA, err := a.opts.GitAdapter.ResolveBranchSHA(ctx, urlStr, branch, auth)
				if err != nil {
					return fmt.Errorf("resolve source %s (%s@%s): %w", canon, urlStr, branch, err)
				}
				out[canon] = sourceRecord{
					Address:          n.Address,
					CanonicalAddress: canon,
					Name:             name,
					URL:              urlStr,
					Branch:           branch,
					CommitSHA:        commitSHA,
					TokenRef:         tokenRef,
					SSHKeyRef:        sshKeyRef,
				}
				refs = append(refs, git.SourceRef{URL: urlStr, Branch: branch})
			case "module":
				if n.Module != nil && n.Module.Plan != nil {
					if err := collect(n.Module.Plan, canon); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := collect(root, ""); err != nil {
		return nil, nil, err
	}
	return out, refs, nil
}

func seedPlanNodesRecursive(st *persistedRunState, p *plan.Plan, modulePrefix string) {
	if p == nil {
		return
	}
	for _, n := range p.Nodes() {
		canon := canonicalForNode(n, modulePrefix)
		switch n.Kind {
		case "worker", "dynamic_worker", "merge", "module", "preserve":
			if _, exists := st.Nodes[canon]; !exists {
				epoch := uint64(1)
				if n.Kind != "worker" {
					epoch = 0
				}
				st.Nodes[canon] = &nodeStateRecord{
					Address:          n.Address,
					CanonicalAddress: canon,
					Kind:             n.Kind,
					ModulePath:       modulePrefix,
					Status:           node.StatusPending,
					CurrentEpoch:     epoch,
					MergeEpoch:       1,
				}
			}
			if n.Kind == "module" && n.Module != nil && n.Module.Plan != nil {
				seedPlanNodesRecursive(st, n.Module.Plan, canon)
			}
		}
	}
}

// RegisterManagedBranch enforces Run-level git.Ledger protected-source and ownership invariants.
func (a *RunActor) RegisterManagedBranch(ctx context.Context, req *RegisterManagedBranchReq) (*RegisterManagedBranchResp, error) {
	if req == nil {
		return &RegisterManagedBranchResp{OK: false, ErrorCode: ErrCodeInvalidInput, ErrorMessage: "request is nil"}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &RegisterManagedBranchResp{OK: false, ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}
	ledger, lErr := restoreLedger(st.Ledger)
	if lErr != nil {
		return &RegisterManagedBranchResp{OK: false, ErrorCode: ErrCodeInvalidInput, ErrorMessage: lErr.Error()}, nil
	}
	if err := ledger.Register(req.RepoURL, req.Branch, req.BaseSHA, req.Kind, req.NodeAddr, req.Epoch); err != nil {
		return &RegisterManagedBranchResp{
			OK:           false,
			ErrorCode:    mapGitLedgerErrorCode(err),
			ErrorMessage: err.Error(),
		}, nil
	}
	st.Ledger = ledger.Snapshot()
	if err := a.saveStateLocked(ctx, st); err != nil {
		return nil, err
	}
	return &RegisterManagedBranchResp{OK: true}, nil
}

func mapGitLedgerErrorCode(err error) string {
	switch {
	case errors.Is(err, git.ErrProtectedSourceBranch):
		return ErrCodeProtectedSourceBranch
	case errors.Is(err, git.ErrBranchOwnershipConflict):
		return ErrCodeBranchOwnershipConflict
	case errors.Is(err, git.ErrInvalidBaseSHA):
		return ErrCodeInvalidBaseSHA
	default:
		return ErrCodeInvalidInput
	}
}

type dispatchNodeAction struct {
	canonAddr string
	req       *node.StartOrResumeReq
}

type enqueueMergeAction struct {
	mergeCanonAddr string
	sourceCanon    string
	sourceEpoch    uint64
	mergeEpoch     uint64
	repoURL        string
	targetBranch   string
	sharedTarget   bool
	req            *merge.EnqueueCandidateReq
}

type cancelMergeAction struct {
	repoURL      string
	targetBranch string
	req          *merge.CancelByRunReq
}

// Reconcile evaluates the Run DAG non-blockingly and dispatches ready workers & streaming merges.
func (a *RunActor) Reconcile(ctx context.Context, req *ReconcileReq) (*ReconcileResp, error) {
	a.mu.Lock()
	if a.reconciling {
		a.reconcileWake = true
		st, exists, err := a.loadStateLocked(ctx)
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !exists {
			return &ReconcileResp{RunID: a.runIDFallback(""), ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
		}
		return &ReconcileResp{RunID: st.RunID, Status: st.Status}, nil
	}
	a.reconciling = true
	a.reconcileWake = false
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.reconciling = false
		a.reconcileWake = false
		a.mu.Unlock()
	}()

	var totalDispatched, totalEnqueued int
	var lastSt persistedRunState

	for {
		a.mu.Lock()
		st, exists, err := a.loadStateLocked(ctx)
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !exists {
			return &ReconcileResp{RunID: a.runIDFallback(""), ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
		}
		lastSt = st

		if isTerminalRunStatus(st.Status) || st.Status == RunStatusPaused {
			return &ReconcileResp{
				RunID:           st.RunID,
				Status:          st.Status,
				DispatchedNodes: totalDispatched,
				EnqueuedMerges:  totalEnqueued,
			}, nil
		}

		loaded, archErr := a.ensureArchiveLoaded(ctx, st)
		if archErr != nil {
			a.mu.Lock()
			st, _, _ = a.loadStateLocked(ctx)
			st.Status = RunStatusFailed
			st.ErrorMessage = archErr.Error()
			_ = a.saveStateLocked(ctx, st)
			a.mu.Unlock()
			a.syncOperatorAndEvents(ctx, st)
			return &ReconcileResp{
				RunID:        st.RunID,
				Status:       RunStatusFailed,
				ErrorCode:    ErrCodeInvalidInput,
				ErrorMessage: archErr.Error(),
			}, nil
		}

		a.mu.Lock()
		st, exists, err = a.loadStateLocked(ctx)
		if err != nil {
			a.mu.Unlock()
			return nil, err
		}
		if !exists || isTerminalRunStatus(st.Status) || st.Status == RunStatusPaused {
			a.mu.Unlock()
			return &ReconcileResp{RunID: st.RunID, Status: st.Status, DispatchedNodes: totalDispatched, EnqueuedMerges: totalEnqueued}, nil
		}

		planResult, err := a.planReconcileActionsLocked(ctx, &st, loaded)
		if err != nil {
			st.Status = RunStatusFailed
			st.ErrorMessage = err.Error()
			_ = a.saveStateLocked(ctx, st)
			a.mu.Unlock()
			a.syncOperatorAndEvents(ctx, st)
			return &ReconcileResp{RunID: st.RunID, Status: st.Status, ErrorCode: ErrCodeInvalidInput, ErrorMessage: err.Error()}, nil
		}

		prevStatus := st.Status
		if planResult.statusChanged || len(planResult.dispatches) > 0 || len(planResult.enqueues) > 0 || planResult.mutated {
			if err := a.saveStateLocked(ctx, st); err != nil {
				a.mu.Unlock()
				return nil, err
			}
		}
		a.mu.Unlock()

		if st.Status != prevStatus {
			if req != nil && req.Reason == "operator-recovery" {
				snapshot := st
				go a.syncOperatorAndEvents(context.Background(), snapshot)
			} else {
				a.syncOperatorAndEvents(ctx, st)
			}
		}
		if isTerminalRunStatus(st.Status) && a.opts.ReminderScheduler != nil && a.ID() != "" {
			_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
		}

		dispatchedNow := 0
		failedRun := false
		for _, act := range planResult.dispatches {
			resp, dErr := a.opts.NodeController.StartOrResume(ctx, st.RunID, act.canonAddr, act.req)
			a.mu.Lock()
			cur, ok, lErr := a.loadStateLocked(ctx)
			if lErr == nil && ok {
				if rec, exists := cur.Nodes[act.canonAddr]; exists {
					if dErr == nil && resp != nil && resp.Status == node.StatusFailed {
						rec.Status = node.StatusFailed
						rec.ErrorMessage = resp.ErrorMessage
						cur.Status = RunStatusFailed
						cur.ErrorMessage = fmt.Sprintf("node %s failed: %s", act.canonAddr, rec.ErrorMessage)
						failedRun = true
						st = cur
					} else if dErr != nil || resp == nil || !resp.Accepted {
						if rec.Status == node.StatusQueuedSlot {
							rec.Status = node.StatusPending
						}
						if dErr != nil {
							rec.ErrorMessage = dErr.Error()
						} else if resp != nil && resp.ErrorMessage != "" {
							rec.ErrorMessage = resp.ErrorMessage
						}
					} else {
						if resp.Epoch > rec.CurrentEpoch {
							rec.CurrentEpoch = resp.Epoch
						}
						if resp.Status != "" {
							rec.Status = resp.Status
						}
						rec.ErrorMessage = ""
						dispatchedNow++
					}
					_ = a.saveStateLocked(ctx, cur)
				}
			}
			a.mu.Unlock()
			if failedRun {
				break
			}
		}
		if failedRun {
			a.syncOperatorAndEvents(ctx, st)
			if a.opts.ReminderScheduler != nil && a.ID() != "" {
				_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
			}
			return &ReconcileResp{
				RunID:           st.RunID,
				Status:          st.Status,
				DispatchedNodes: totalDispatched + dispatchedNow,
				EnqueuedMerges:  totalEnqueued,
				ErrorMessage:    st.ErrorMessage,
			}, nil
		}
		totalDispatched += dispatchedNow

		enqueuedNow := 0
		for _, enq := range planResult.enqueues {
			resp, eErr := a.opts.MergeController.EnqueueCandidate(ctx, enq.repoURL, enq.targetBranch, enq.req)
			a.mu.Lock()
			cur, ok, lErr := a.loadStateLocked(ctx)
			if lErr == nil && ok {
				if mRec, exists := cur.Nodes[enq.mergeCanonAddr]; exists {
					if eErr == nil && resp != nil && resp.Ticket.Status == merge.TicketStatusFailed {
						mRec.Status = node.StatusFailed
						mRec.ErrorMessage = resp.Ticket.ErrorMessage
						cur.Status = RunStatusFailed
						cur.ErrorMessage = fmt.Sprintf("merge %s failed: %s", enq.mergeCanonAddr, mRec.ErrorMessage)
						failedRun = true
						st = cur
					} else if eErr == nil && resp != nil && resp.Accepted {
						if mRec.EnqueuedSources == nil {
							mRec.EnqueuedSources = make(map[string]string)
						}
						if mRec.EnqueuedSources[enq.sourceCanon] == "" || mRec.EnqueuedSources[enq.sourceCanon] == "ENQUEUING" {
							mRec.EnqueuedSources[enq.sourceCanon] = "ENQUEUED"
						}
						enqueuedNow++
					} else if mRec.EnqueuedSources != nil && mRec.EnqueuedSources[enq.sourceCanon] == "ENQUEUING" {
						delete(mRec.EnqueuedSources, enq.sourceCanon)
						if eErr != nil {
							mRec.ErrorMessage = eErr.Error()
						}
					}
					_ = a.saveStateLocked(ctx, cur)
				}
			}
			a.mu.Unlock()
			if failedRun {
				break
			}
		}
		if failedRun {
			a.syncOperatorAndEvents(ctx, st)
			if a.opts.ReminderScheduler != nil && a.ID() != "" {
				_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
			}
			return &ReconcileResp{
				RunID:           st.RunID,
				Status:          st.Status,
				DispatchedNodes: totalDispatched,
				EnqueuedMerges:  totalEnqueued + enqueuedNow,
				ErrorMessage:    st.ErrorMessage,
			}, nil
		}
		totalEnqueued += enqueuedNow

		a.mu.Lock()
		latest, latestOK, _ := a.loadStateLocked(ctx)
		wake := a.reconcileWake || (latestOK && latest.Status == RunStatusRunning && planResult.mutated && len(planResult.dispatches) == 0 && len(planResult.enqueues) == 0)
		a.reconcileWake = false
		a.mu.Unlock()
		if latestOK {
			lastSt = latest
		}
		if !wake {
			break
		}
	}

	return &ReconcileResp{
		RunID:           lastSt.RunID,
		Status:          lastSt.Status,
		DispatchedNodes: totalDispatched,
		EnqueuedMerges:  totalEnqueued,
	}, nil
}

type reconcilePlanResult struct {
	dispatches    []dispatchNodeAction
	enqueues      []enqueueMergeAction
	mutated       bool
	statusChanged bool
}

func (a *RunActor) planReconcileActionsLocked(
	ctx context.Context,
	st *persistedRunState,
	loaded *plan.LoadedArchive,
) (reconcilePlanResult, error) {
	var res reconcilePlanResult
	rootPlan := loaded.Plan

	// Check for PAUSING -> PAUSED before scheduling any new work.
	if st.ManualPaused || st.Status == RunStatusPausing {
		if !hasInFlightExecution(st) && st.Status != RunStatusPaused {
			st.Status = RunStatusPaused
			res.mutated = true
			res.statusChanged = true
		}
		return res, nil
	}

	if err := a.reconcilePlanScopeLocked(ctx, st, loaded, rootPlan, nil, "", nil, &res); err != nil {
		return res, err
	}

	// Evaluate top-level preserves & outputs when DAG nodes complete.
	evalCtx, err := buildScopeEvalContext(st, rootPlan, nil, "")
	if err != nil {
		return res, err
	}
	if a.evaluatePreservesRecursive(st, rootPlan, nil, "", evalCtx) {
		res.mutated = true
	}

	if allDAGNodesSucceeded(st) {
		outputs, err := evaluatePlanOutputsMap(rootPlan, evalCtx)
		if err != nil {
			return res, err
		}
		st.Outputs = outputs
		st.Status = RunStatusSucceeded
		st.ErrorMessage = ""
		res.mutated = true
		res.statusChanged = true
		return res, nil
	}

	// Update WAITING_HUMAN vs RUNNING status dynamically.
	if hasWaitingHuman(st) {
		if st.Status != RunStatusWaitingHuman {
			st.Status = RunStatusWaitingHuman
			res.mutated = true
			res.statusChanged = true
		}
	} else if st.Status == RunStatusWaitingHuman || st.Status == RunStatusPending {
		st.Status = RunStatusRunning
		res.mutated = true
		res.statusChanged = true
	}
	return res, nil
}

func (a *RunActor) reconcilePlanScopeLocked(
	ctx context.Context,
	st *persistedRunState,
	loaded *plan.LoadedArchive,
	currentPlan *plan.Plan,
	parentPlans []*plan.Plan,
	modulePrefix string,
	parallelBudget *int,
	res *reconcilePlanResult,
) error {
	if currentPlan == nil {
		return nil
	}
	nodes := currentPlan.Nodes()
	nodeByLocal := make(map[string]plan.NodeSpec, len(nodes))
	for _, n := range nodes {
		nodeByLocal[n.Address] = n
	}

	// Track active nodes in this module scope to enforce module.parallelism if configured.
	activeInModule := 0
	if parallelBudget != nil {
		for _, n := range nodes {
			canon := canonicalForNode(n, modulePrefix)
			switch n.Kind {
			case "worker", "merge":
				if rec := st.Nodes[canon]; rec != nil && isNodeSlotActive(rec.Status) {
					activeInModule++
				}
			case "dynamic_worker":
				if rec := st.Nodes[canon]; rec != nil && rec.DynamicExpanded {
					for _, k := range rec.DynamicKeys {
						if inst := st.Nodes[dynamicInstanceCanon(canon, k)]; inst != nil && isNodeSlotActive(inst.Status) {
							activeInModule++
						}
					}
				}
			}
		}
	}

	evalCtx, err := buildScopeEvalContext(st, currentPlan, parentPlans, modulePrefix)
	if err != nil {
		return err
	}
	evalOpts := []applyeval.EvalOption{
		applyeval.WithPlan(currentPlan),
		applyeval.WithParentPlans(parentPlans...),
		applyeval.WithBaseDir(bundleDirForPlan(loaded, currentPlan)),
	}

	for _, n := range nodes {
		canon := canonicalForNode(n, modulePrefix)
		switch n.Kind {
		case "worker":
			rec := ensureNodeRecord(st, n, modulePrefix)
			if rec.Status != node.StatusPending {
				continue
			}
			if !areLocalDepsSucceeded(st, nodeByLocal, modulePrefix, n.Dependencies) {
				continue
			}
			if parallelBudget != nil && activeInModule >= *parallelBudget {
				continue
			}
			mat, err := applyeval.EvalReadyWorker(evalCtx, n, evalOpts...)
			if err != nil {
				return err
			}
			if err := a.prepareWorkerDispatchLocked(ctx, st, loaded, currentPlan, parentPlans, evalCtx, evalOpts, n, rec, mat, &res.dispatches); err != nil {
				return err
			}
			rec.Status = node.StatusQueuedSlot
			activeInModule++
			res.mutated = true

		case "dynamic_worker":
			rec := ensureNodeRecord(st, n, modulePrefix)
			if !areLocalDepsSucceeded(st, nodeByLocal, modulePrefix, n.Dependencies) {
				continue
			}
			if !rec.DynamicExpanded {
				keys, elems, err := applyeval.EvalDynamicWorkerCollection(evalCtx, n)
				if err != nil {
					return err
				}
				rec.DynamicExpanded = true
				rec.DynamicKeys = slices.Clone(keys)
				rec.DynamicElements = make(map[string]WireValue, len(keys))
				for _, k := range keys {
					wv, wErr := EncodeWireValue(elems[k])
					if wErr != nil {
						return fmt.Errorf("encode dynamic_worker %s[%s]: %w", canon, k, wErr)
					}
					rec.DynamicElements[k] = wv
					instCanon := dynamicInstanceCanon(canon, k)
					if _, ok := st.Nodes[instCanon]; !ok {
						st.Nodes[instCanon] = &nodeStateRecord{
							Address:           fmt.Sprintf("%s[%s]", n.Address, k),
							CanonicalAddress:  instCanon,
							Kind:              "worker",
							ModulePath:        modulePrefix,
							ParentDynamicAddr: canon,
							DynamicKey:        k,
							Status:            node.StatusPending,
							CurrentEpoch:      1,
						}
					}
				}
				if len(keys) == 0 {
					rec.Status = node.StatusSucceeded
				} else {
					rec.Status = node.StatusRunning
				}
				res.mutated = true
			}

			if len(rec.DynamicKeys) == 0 {
				if rec.Status != node.StatusSucceeded {
					rec.Status = node.StatusSucceeded
					res.mutated = true
				}
				continue
			}

			allInstDone := true
			anyInstWaiting := false
			activeInDyn := 0
			for _, k := range rec.DynamicKeys {
				inst := st.Nodes[dynamicInstanceCanon(canon, k)]
				if inst == nil {
					allInstDone = false
					continue
				}
				if inst.Status != node.StatusSucceeded {
					allInstDone = false
				}
				if inst.Status == node.StatusWaitingHuman {
					anyInstWaiting = true
				}
				if isNodeSlotActive(inst.Status) {
					activeInDyn++
				}
			}
			if allInstDone {
				if rec.Status != node.StatusSucceeded {
					rec.Status = node.StatusSucceeded
					res.mutated = true
				}
				continue
			}
			if anyInstWaiting {
				if rec.Status != node.StatusWaitingHuman {
					rec.Status = node.StatusWaitingHuman
					res.mutated = true
				}
			} else if rec.Status != node.StatusRunning {
				rec.Status = node.StatusRunning
				res.mutated = true
			}

			dynLimit := dynamicParallelismLimit(n.Config)
			for _, k := range rec.DynamicKeys {
				inst := st.Nodes[dynamicInstanceCanon(canon, k)]
				if inst == nil || inst.Status != node.StatusPending {
					continue
				}
				if parallelBudget != nil && activeInModule >= *parallelBudget {
					break
				}
				if dynLimit > 0 && activeInDyn >= dynLimit {
					break
				}
				elemVal, dErr := rec.DynamicElements[k].DecodeWireValue()
				if dErr != nil {
					return dErr
				}
				mat, err := applyeval.ProjectDynamicInstance(evalCtx, n, k, elemVal, evalOpts...)
				if err != nil {
					return err
				}
				if err := a.prepareWorkerDispatchLocked(ctx, st, loaded, currentPlan, parentPlans, evalCtx, evalOpts, n, inst, mat, &res.dispatches); err != nil {
					return err
				}
				inst.Status = node.StatusQueuedSlot
				activeInModule++
				activeInDyn++
				res.mutated = true
			}

		case "merge":
			rec := ensureNodeRecord(st, n, modulePrefix)
			if rec.Status == node.StatusSucceeded || rec.Status == node.StatusFailed {
				continue
			}
			if err := a.reconcileMergeNodeLocked(ctx, st, loaded, currentPlan, parentPlans, modulePrefix, nodeByLocal, evalCtx, evalOpts, n, rec, res); err != nil {
				return err
			}

		case "module":
			if n.Module == nil || n.Module.Plan == nil {
				continue
			}
			rec := ensureNodeRecord(st, n, modulePrefix)
			if rec.Status == node.StatusSucceeded {
				continue
			}
			if !areLocalDepsSucceeded(st, nodeByLocal, modulePrefix, n.Dependencies) {
				continue
			}
			bound, err := applyeval.EvalModuleInputsAndValidate(evalCtx, n, evalOpts...)
			if err != nil {
				return err
			}
			if len(bound.Inputs) > 0 && len(rec.BoundInputs) == 0 {
				rec.BoundInputs = make(map[string]WireValue, len(bound.Inputs))
				for k, v := range bound.Inputs {
					wv, wErr := EncodeWireValue(v)
					if wErr != nil {
						return wErr
					}
					rec.BoundInputs[k] = wv
				}
				res.mutated = true
			}
			childParents := append(slices.Clone(parentPlans), currentPlan)
			if err := a.reconcilePlanScopeLocked(ctx, st, loaded, bound.Plan, childParents, canon, n.Module.Parallelism, res); err != nil {
				return err
			}
			if areModuleChildrenSucceeded(st, bound.Plan, canon) {
				childCtx, err := buildScopeEvalContext(st, bound.Plan, childParents, canon)
				if err != nil {
					return err
				}
				modOuts, err := evaluatePlanOutputsMap(bound.Plan, childCtx)
				if err != nil {
					return err
				}
				rec.ModuleOutputs = modOuts
				rec.Status = node.StatusSucceeded
				res.mutated = true
			} else if rec.Status == node.StatusPending {
				rec.Status = node.StatusRunning
				res.mutated = true
			}
		}
	}
	return nil
}

func (a *RunActor) reconcileMergeNodeLocked(
	ctx context.Context,
	st *persistedRunState,
	loaded *plan.LoadedArchive,
	currentPlan *plan.Plan,
	parentPlans []*plan.Plan,
	modulePrefix string,
	nodeByLocal map[string]plan.NodeSpec,
	evalCtx *hcl.EvalContext,
	evalOpts []applyeval.EvalOption,
	n plan.NodeSpec,
	rec *nodeStateRecord,
	res *reconcilePlanResult,
) error {
	baseReady := areMergeBaseDepsSucceeded(st, currentPlan, nodeByLocal, modulePrefix, n)
	if !baseReady {
		return nil
	}

	mat, err := applyeval.EvalMergeBase(evalCtx, n, evalOpts...)
	if err != nil {
		return err
	}
	repoURL := strings.TrimSpace(mat.SourceURL)
	if repoURL == "" {
		repoURL = inferSingleSourceURL(st, currentPlan)
	}
	effectiveTarget, sharedTarget, err := merge.EffectiveTargetBranch(st.RunID, rec.CanonicalAddress, mat.TargetBranch)
	if err != nil {
		return err
	}
	if rec.RepoURL != repoURL || rec.TargetBranch != effectiveTarget || rec.SharedTargetBranch != sharedTarget || rec.BasedSource != mat.BasedSource {
		rec.RepoURL = repoURL
		rec.TargetBranch = effectiveTarget
		rec.SharedTargetBranch = sharedTarget
		rec.BasedSource = mat.BasedSource
		rec.WorkBranch = effectiveTarget
		res.mutated = true
	}

	// Pin base SHA from based_source if not yet resolved.
	if rec.BaseSHA == "" {
		baseSHA, err := a.resolveBranchBaseSHA(ctx, st, currentPlan, parentPlans, repoURL, mat.BasedSource)
		if err != nil {
			return err
		}
		rec.BaseSHA = baseSHA
		res.mutated = true
	}

	if mat.SourcesKnown && (!rec.SourcesKnown || !slices.Equal(rec.ExpectedSources, mat.Sources)) {
		rec.SourcesKnown = true
		rec.ExpectedSources = slices.Clone(mat.Sources)
		res.mutated = true
	}

	if rec.MergeEpoch == 0 {
		rec.MergeEpoch = 1
	}

	// Handle 0-candidate merge (e.g., empty dynamic_worker) immediately when SourcesKnown and empty.
	if rec.SourcesKnown && len(rec.ExpectedSources) == 0 {
		rec.CommitSHA = rec.BaseSHA
		rec.LatestTargetSHA = rec.BaseSHA
		rec.Status = node.StatusSucceeded
		res.mutated = true
		return nil
	}

	// Identify candidate-producing upstream nodes and stream any completed ones immediately.
	candidates := collectMergeCandidateSources(st, currentPlan, modulePrefix, n)
	if rec.EnqueuedSources == nil {
		rec.EnqueuedSources = make(map[string]string)
	}
	sandboxCfg, err := extractMergeSandboxConfig(mat.Sandbox, evalCtx, currentPlan, parentPlans)
	if err != nil {
		return err
	}
	envKeys, _ := actorcfg.ExtractNodeEnvRefs(loaded.Plan, rec.CanonicalAddress)
	tokenRef, sshKeyRef := findSourceCredentialRefs(st, repoURL)
	bundleDir := bundleDirForPlan(loaded, currentPlan)

	// Pre-register Run-exclusive KindMergeBranch directly in st.Ledger once per (branch, epoch, baseSHA)
	// so MergeActor.EnqueueCandidate never synchronously calls back into RunActor.RegisterManagedBranch
	// under Dapr's turn lock.
	if !sharedTarget && rec.BaseSHA != "" && repoURL != "" &&
		!isMergeBranchRegisteredInLedger(st.Ledger, repoURL, effectiveTarget, rec.CanonicalAddress, rec.MergeEpoch, rec.BaseSHA) {
		ledger, lErr := restoreLedger(st.Ledger)
		if lErr != nil {
			return lErr
		}
		if err := ledger.Register(repoURL, effectiveTarget, rec.BaseSHA, git.KindMergeBranch, rec.CanonicalAddress, rec.MergeEpoch); err != nil {
			return err
		}
		st.Ledger = ledger.Snapshot()
		res.mutated = true
	}

	for _, cand := range candidates {
		if st.ManualPaused || st.Status == RunStatusPausing {
			break
		}
		if curState := rec.EnqueuedSources[cand.canonAddr]; curState != "" {
			continue
		}
		if cand.workBranch == "" {
			continue
		}
		rec.EnqueuedSources[cand.canonAddr] = "ENQUEUING"
		if rec.Status == node.StatusPending {
			rec.Status = node.StatusRunning
		}
		res.mutated = true
		res.enqueues = append(res.enqueues, enqueueMergeAction{
			mergeCanonAddr: rec.CanonicalAddress,
			sourceCanon:    cand.canonAddr,
			sourceEpoch:    cand.epoch,
			mergeEpoch:     rec.MergeEpoch,
			repoURL:        repoURL,
			targetBranch:   effectiveTarget,
			sharedTarget:   sharedTarget,
			req: &merge.EnqueueCandidateReq{
				RunID:               st.RunID,
				MergeNodeAddr:       rec.CanonicalAddress,
				MergeEpoch:          rec.MergeEpoch,
				SourceNodeAddr:      cand.canonAddr,
				SourceEpoch:         cand.epoch,
				RepoURL:             repoURL,
				BasedSource:         mat.BasedSource,
				BaseSHA:             rec.BaseSHA,
				TargetBranch:        mat.TargetBranch,
				BranchPreRegistered: !sharedTarget && rec.BaseSHA != "" && repoURL != "",
				CandidateBranch:     cand.workBranch,
				Namespace:           st.Namespace,
				RunOverrides:        maps.Clone(st.RunOverrides),
				RequiredEnvKeys:     envKeys,
				TokenRef:            tokenRef,
				SSHKeyRef:           sshKeyRef,
				Auth:                git.AuthConfig{TokenRef: tokenRef, SSHKeyRef: sshKeyRef},
				RequiredArch:        st.RequiredArch,
				Sandbox:             sandboxCfg,
				Gatekeepers:         slices.Clone(mat.Gatekeepers),
				BundleDir:           bundleDir,
				HostOS:              st.HostOS,
				TempRoot:            st.TempRoot,
			},
		})
	}

	if rec.SourcesKnown && len(rec.ExpectedSources) > 0 && len(candidates) >= len(rec.ExpectedSources) {
		allMerged := true
		for _, cand := range candidates {
			if rec.EnqueuedSources[cand.canonAddr] != "MERGED" {
				allMerged = false
				break
			}
		}
		if allMerged {
			rec.Status = node.StatusSucceeded
			if rec.CommitSHA == "" {
				rec.CommitSHA = rec.LatestTargetSHA
			}
			res.mutated = true
		}
	}
	return nil
}

func (a *RunActor) prepareWorkerDispatchLocked(
	ctx context.Context,
	st *persistedRunState,
	loaded *plan.LoadedArchive,
	currentPlan *plan.Plan,
	parentPlans []*plan.Plan,
	evalCtx *hcl.EvalContext,
	evalOpts []applyeval.EvalOption,
	plannedNode plan.NodeSpec,
	rec *nodeStateRecord,
	mat workerspec.Config,
	out *[]dispatchNodeAction,
) error {
	basedSource, err := requireCtyString(mat.BasedSource, rec.CanonicalAddress+".based_source")
	if err != nil {
		return err
	}
	repoURL := strings.TrimSpace(mat.SourceURL)
	if repoURL == "" {
		repoURL = inferSingleSourceURL(st, currentPlan)
	}

	var baseSHA string
	if rec.BaseSHA != "" {
		baseSHA = rec.BaseSHA
	} else {
		baseSHA, err = a.resolveBranchBaseSHA(ctx, st, currentPlan, parentPlans, repoURL, basedSource)
		if err != nil {
			return err
		}
	}

	rec.RepoURL = repoURL
	rec.BasedSource = basedSource
	rec.BaseSHA = baseSHA
	rec.WorkBranch = git.WorkBranchName(st.RunID, rec.CanonicalAddress)

	sysPrompt, err := optionalCtyString(mat.SystemPrompt, rec.CanonicalAddress+".system_prompt")
	if err != nil {
		return err
	}
	promptStr, err := optionalCtyString(mat.Prompt, rec.CanonicalAddress+".prompt")
	if err != nil {
		return err
	}
	qcPromptStr := ""

	sandboxes, harnesses, providers, mcpServers, mcpResources, toolsMap, toolEnvs, err := extractNodeResourceCatalog(loaded.Plan, currentPlan, parentPlans)
	if err != nil {
		return fmt.Errorf("extract resource catalog for %s: %w", rec.CanonicalAddress, err)
	}
	imports, err := resolveWorkerImports(st, mat)
	if err != nil {
		return fmt.Errorf("resolve imports for %s: %w", rec.CanonicalAddress, err)
	}
	envKeys, _ := actorcfg.ExtractNodeEnvRefs(loaded.Plan, rec.CanonicalAddress)
	tokenRef, sshKeyRef := findSourceCredentialRefs(st, repoURL)

	if rec.CurrentEpoch == 0 {
		rec.CurrentEpoch = 1
	}

	*out = append(*out, dispatchNodeAction{
		canonAddr: rec.CanonicalAddress,
		req: &node.StartOrResumeReq{
			RunID:                 st.RunID,
			NodeAddr:              rec.CanonicalAddress,
			Namespace:             st.Namespace,
			RunOverrides:          maps.Clone(st.RunOverrides),
			RequiredEnvKeys:       envKeys,
			TokenRef:              tokenRef,
			SSHKeyRef:             sshKeyRef,
			Auth:                  git.AuthConfig{TokenRef: tokenRef, SSHKeyRef: sshKeyRef},
			RepoURL:               repoURL,
			BasedSource:           basedSource,
			BaseSHA:               baseSHA,
			RequiredArch:          st.RequiredArch,
			Worker:                mat,
			SystemPrompt:          sysPrompt,
			Prompt:                promptStr,
			QCPrompt:              qcPromptStr,
			Sandboxes:             sandboxes,
			Harnesses:             harnesses,
			ModelProviders:        providers,
			MCPServers:            mcpServers,
			MCPResources:          mcpResources,
			Tools:                 toolsMap,
			ToolEnvs:              toolEnvs,
			Imports:               imports,
			BundleDir:             bundleDirForPlan(loaded, currentPlan),
			TempRoot:              st.TempRoot,
			HostOS:                st.HostOS,
			TargetPlatform:        st.TargetPlatform,
			PluginResolve:         st.PluginResolve,
			MaxGatekeeperReworks:  st.MaxGatekeeperReworks,
			MaxArtifactReworks:    st.MaxArtifactReworks,
			StallTimeout:          st.StallTimeout,
			WatchdogCheckInterval: st.WatchdogCheckInterval,
		},
	})
	_ = evalCtx
	_ = evalOpts
	_ = plannedNode
	return nil
}

// OnNodeEvent handles NodeActor lifecycle transitions and fallback rework cascades.
func (a *RunActor) OnNodeEvent(ctx context.Context, notice *node.NodeEventNotification) (*OnNodeEventResp, error) {
	if notice == nil {
		return &OnNodeEventResp{Accepted: false, ErrorCode: ErrCodeInvalidInput, ErrorMessage: "notification is nil"}, nil
	}

	var advanceAct *struct {
		canonAddr string
		req       *node.AdvanceForReworkReq
	}
	var resetActs []string
	var cancelMerges []cancelMergeAction
	var shouldReconcile bool
	var statusChanged bool

	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !exists {
		a.mu.Unlock()
		return &OnNodeEventResp{Accepted: false, ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}

	loaded, err := a.ensureArchiveLoadedNoLockHint(&st)
	if err != nil {
		a.mu.Unlock()
		loaded, err = a.ensureArchiveLoaded(ctx, st)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		st, exists, err = a.loadStateLocked(ctx)
		if err != nil || !exists {
			a.mu.Unlock()
			return &OnNodeEventResp{Accepted: false, ErrorCode: ErrCodeNotFound}, nil
		}
	}

	rec, ok := st.Nodes[notice.NodeAddr]
	if !ok {
		a.mu.Unlock()
		return &OnNodeEventResp{
			Accepted:     false,
			RunStatus:    st.Status,
			ErrorCode:    ErrCodeNotFound,
			ErrorMessage: fmt.Sprintf("node %q not found", notice.NodeAddr),
		}, nil
	}
	if notice.Epoch > 0 && rec.CurrentEpoch > notice.Epoch {
		a.mu.Unlock()
		return &OnNodeEventResp{
			Accepted:   false,
			StaleEpoch: true,
			RunStatus:  st.Status,
		}, nil
	}
	if notice.Epoch > rec.CurrentEpoch {
		rec.CurrentEpoch = notice.Epoch
	}
	prevStatus := st.Status

	switch notice.Status {
	case node.StatusQueuedSlot, node.StatusRunning:
		rec.Status = notice.Status
		if st.Status == RunStatusWaitingHuman && !hasWaitingHuman(&st) {
			st.Status = RunStatusRunning
		}

	case node.StatusWaitingHuman:
		rec.Status = node.StatusWaitingHuman
		if !st.ManualPaused && st.Status == RunStatusRunning {
			st.Status = RunStatusWaitingHuman
		}

	case node.StatusPaused:
		rec.Status = node.StatusPaused
		if (st.ManualPaused || st.Status == RunStatusPausing) && !hasInFlightExecution(&st) {
			st.Status = RunStatusPaused
		}

	case node.StatusSucceeded:
		rec.Status = node.StatusSucceeded
		rec.ErrorMessage = ""
		if notice.Outcome != nil {
			if notice.Outcome.WorkBranch != "" {
				rec.WorkBranch = notice.Outcome.WorkBranch
			}
			if notice.Outcome.CommitSHA != "" {
				rec.CommitSHA = notice.Outcome.CommitSHA
			}
			if len(notice.Outcome.ResultJSON) > 0 {
				s := string(notice.Outcome.ResultJSON)
				rec.Result = &s
			}
			if len(notice.Outcome.Artifacts) > 0 {
				rec.Artifacts = slices.Clone(notice.Outcome.Artifacts)
				for _, art := range notice.Outcome.Artifacts {
					st.PublishedArtifacts[art.ID] = art
				}
			}
		}
		if rec.ParentDynamicAddr != "" {
			if dynRec := st.Nodes[rec.ParentDynamicAddr]; dynRec != nil {
				updateDynamicAggregateStatus(&st, dynRec)
			}
		}
		shouldReconcile = true

	case node.StatusFailed:
		rec.Status = node.StatusFailed
		rec.ErrorMessage = notice.ErrorMessage
		if notice.ErrorMessage == "" && notice.Outcome != nil {
			rec.ErrorMessage = notice.Outcome.FailureReason
		}
		st.Status = RunStatusFailed
		st.ErrorMessage = fmt.Sprintf("node %s failed: %s", notice.NodeAddr, rec.ErrorMessage)

	case node.StatusFallbackTriggered:
		rec.Status = node.StatusFallbackTriggered
		if notice.Outcome == nil {
			st.Status = RunStatusFailed
			st.ErrorMessage = fmt.Sprintf("node %s triggered fallback without outcome", notice.NodeAddr)
			break
		}
		fbName := notice.Outcome.FallbackName
		maxReworks, plannedTarget, fbErr := lookupFallbackSpec(loaded.Plan, notice.NodeAddr, fbName)
		if fbErr != nil {
			rec.Status = node.StatusFailed
			rec.ErrorMessage = fbErr.Error()
			st.Status = RunStatusFailed
			st.ErrorMessage = fbErr.Error()
			break
		}
		if rec.FallbackCounts == nil {
			rec.FallbackCounts = make(map[string]int)
		}
		rec.FallbackCounts[fbName]++
		if rec.FallbackCounts[fbName] > maxReworks {
			rec.Status = node.StatusFailed
			rec.ErrorMessage = fmt.Sprintf("fallback %q exceeded max_reworks (%d)", fbName, maxReworks)
			st.Status = RunStatusFailed
			st.ErrorMessage = fmt.Sprintf("node %s %s", notice.NodeAddr, rec.ErrorMessage)
			break
		}

		targetCanon := notice.Outcome.FallbackTarget
		if strings.TrimSpace(targetCanon) == "" {
			targetCanon = plannedTarget
		}
		targetCanon = resolveModuleScopedTarget(rec.ModulePath, targetCanon)
		targetRec, targetOK := st.Nodes[targetCanon]
		if !targetOK {
			rec.Status = node.StatusFailed
			st.Status = RunStatusFailed
			st.ErrorMessage = fmt.Sprintf("fallback target %q not found", targetCanon)
			break
		}

		// Compute affected downstream nodes from targetCanon (excluding targetCanon itself, which gets AdvanceForRework).
		downstream := collectDownstreamNodes(loaded.Plan, &st, targetCanon)
		if anyMergedIntoSharedBranch(&st, downstream) || targetRec.MergedIntoSharedBranch {
			rec.Status = node.StatusFailed
			st.Status = RunStatusFailed
			st.ErrorMessage = ErrAlreadyMergedSharedBranch.Error()
			break
		}

		// Cancel affected MergeActor tickets & reset downstream state in RunActor.
		for _, dsAddr := range downstream {
			if strings.HasSuffix(dsAddr, "::outputs") {
				modAddr := strings.TrimSuffix(dsAddr, "::outputs")
				if modRec := st.Nodes[modAddr]; modRec != nil && modRec.Kind == "module" {
					modRec.Status = node.StatusPending
					modRec.ModuleOutputs = nil
					modRec.ErrorMessage = ""
				}
				continue
			}
			dsRec := st.Nodes[dsAddr]
			if dsRec == nil {
				continue
			}
			if dsRec.Kind == "merge" {
				if dsRec.RepoURL != "" && dsRec.TargetBranch != "" {
					cancelMerges = append(cancelMerges, cancelMergeAction{
						repoURL:      dsRec.RepoURL,
						targetBranch: dsRec.TargetBranch,
						req: &merge.CancelByRunReq{
							RunID:         st.RunID,
							MergeNodeAddr: dsRec.CanonicalAddress,
							Reason:        "fallback:" + fbName,
						},
					})
				}
				resetMergeRecordForCascade(dsRec)
				continue
			}
			if dsRec.Kind == "dynamic_worker" {
				resetDynamicParentForCascade(&st, dsRec, &resetActs)
				continue
			}
			if dsRec.Kind == "module" {
				resetModuleRecordForCascade(dsRec)
				continue
			}
			if dsRec.Kind == "worker" {
				resetWorkerRecordForCascade(dsRec, false)
				if dsRec.ParentDynamicAddr != "" {
					if dyn := st.Nodes[dsRec.ParentDynamicAddr]; dyn != nil {
						updateDynamicAggregateStatus(&st, dyn)
					}
				}
				resetActs = append(resetActs, dsAddr)
			}
		}

		// Re-pin target BaseSHA from based_source if needed and advance target node epoch.
		targetRec.Status = node.StatusPending
		targetRec.Result = nil
		targetRec.Artifacts = nil
		targetRec.CommitSHA = ""
		targetRec.PendingFallbackReason = notice.Outcome.FallbackReason
		targetRec.PendingFallbackArtifacts = slices.Clone(notice.Outcome.FallbackArtifacts)
		advanceAct = &struct {
			canonAddr string
			req       *node.AdvanceForReworkReq
		}{
			canonAddr: targetCanon,
			req: &node.AdvanceForReworkReq{
				BaseSHA:           targetRec.BaseSHA,
				BasedSource:       targetRec.BasedSource,
				Reason:            notice.Outcome.FallbackReason,
				FallbackArtifacts: slices.Clone(notice.Outcome.FallbackArtifacts),
			},
		}
		shouldReconcile = !st.ManualPaused
	}

	if st.Status != prevStatus {
		statusChanged = true
	}
	if err := a.saveStateLocked(ctx, st); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	if statusChanged {
		a.syncOperatorAndEvents(ctx, st)
	}
	if isTerminalRunStatus(st.Status) && a.opts.ReminderScheduler != nil && a.ID() != "" {
		_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
	}

	for _, cm := range cancelMerges {
		_, _ = a.opts.MergeController.CancelByRun(ctx, cm.repoURL, cm.targetBranch, cm.req)
	}
	for _, rs := range resetActs {
		resp, rErr := a.opts.NodeController.ResetForRerun(ctx, st.RunID, rs, &node.ResetForRerunReq{
			ClearBaseSHA: true,
			Reason:       "upstream fallback cascade",
		})
		if rErr == nil && resp != nil {
			a.updateNodeEpochFromRPC(ctx, rs, resp.Epoch, node.StatusPending)
		}
	}
	if advanceAct != nil {
		resp, aErr := a.opts.NodeController.AdvanceForRework(ctx, st.RunID, advanceAct.canonAddr, advanceAct.req)
		if aErr == nil && resp != nil {
			a.updateNodeEpochFromRPC(ctx, advanceAct.canonAddr, resp.Epoch, node.StatusPending)
		}
	}

	if shouldReconcile {
		_, _ = a.Reconcile(ctx, &ReconcileReq{Reason: "node_event:" + string(notice.Status)})
	}
	return &OnNodeEventResp{
		Accepted:  true,
		RunStatus: st.Status,
	}, nil
}

// OnMergeCandidateResult handles MergeActor candidate outcomes, single-source MergeReworkSource,
// and max_reworks exhaustion.
func (a *RunActor) OnMergeCandidateResult(ctx context.Context, notice *merge.MergeCandidateNotification) (*OnMergeCandidateResultResp, error) {
	if notice == nil {
		return &OnMergeCandidateResultResp{Accepted: false, ErrorCode: ErrCodeInvalidInput, ErrorMessage: "notification is nil"}, nil
	}

	var advanceSource *struct {
		canonAddr string
		req       *node.AdvanceForReworkReq
	}
	var shouldReconcile bool
	var statusChanged bool

	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !exists {
		a.mu.Unlock()
		return &OnMergeCandidateResultResp{Accepted: false, ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}

	loaded, err := a.ensureArchiveLoadedNoLockHint(&st)
	if err != nil {
		a.mu.Unlock()
		loaded, err = a.ensureArchiveLoaded(ctx, st)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		st, exists, err = a.loadStateLocked(ctx)
		if err != nil || !exists {
			a.mu.Unlock()
			return &OnMergeCandidateResultResp{Accepted: false, ErrorCode: ErrCodeNotFound}, nil
		}
	}

	mRec, ok := st.Nodes[notice.MergeNodeAddr]
	if !ok {
		a.mu.Unlock()
		return &OnMergeCandidateResultResp{Accepted: false, ErrorCode: ErrCodeNotFound, ErrorMessage: "merge node not found"}, nil
	}
	if notice.MergeEpoch > 0 && mRec.MergeEpoch > notice.MergeEpoch {
		a.mu.Unlock()
		return &OnMergeCandidateResultResp{Accepted: false, IgnoredStale: true, RunStatus: st.Status}, nil
	}

	srcRec := st.Nodes[notice.SourceNodeAddr]
	if notice.SourceEpoch > 0 && srcRec != nil && srcRec.CurrentEpoch > notice.SourceEpoch {
		a.mu.Unlock()
		return &OnMergeCandidateResultResp{Accepted: false, IgnoredStale: true, RunStatus: st.Status}, nil
	}

	prevStatus := st.Status
	if mRec.EnqueuedSources == nil {
		mRec.EnqueuedSources = make(map[string]string)
	}
	if notice.Result.LatestTargetSHA != "" {
		mRec.LatestTargetSHA = notice.Result.LatestTargetSHA
	}
	if notice.Result.TargetBranch != "" {
		mRec.TargetBranch = notice.Result.TargetBranch
		mRec.WorkBranch = notice.Result.TargetBranch
	} else if notice.TargetBranch != "" {
		mRec.TargetBranch = notice.TargetBranch
		mRec.WorkBranch = notice.TargetBranch
	}

	switch notice.Result.Status {
	case runner.MergeStatusSucceeded:
		mRec.EnqueuedSources[notice.SourceNodeAddr] = "MERGED"
		if notice.Result.MergedCommitSHA != "" {
			mRec.CommitSHA = notice.Result.MergedCommitSHA
		} else if notice.Result.LatestTargetSHA != "" {
			mRec.CommitSHA = notice.Result.LatestTargetSHA
		}
		if notice.SharedTargetBranch || notice.MergedIntoSharedBranch {
			mRec.MergedIntoSharedBranch = true
			if srcRec != nil {
				srcRec.MergedIntoSharedBranch = true
			}
		}
		shouldReconcile = true

	case runner.MergeStatusReworkSource:
		maxReworks := lookupMergeMaxReworks(loaded.Plan, notice.MergeNodeAddr)
		if mRec.ReworkCounts == nil {
			mRec.ReworkCounts = make(map[string]int)
		}
		mRec.ReworkCounts[notice.SourceNodeAddr]++
		if mRec.ReworkCounts[notice.SourceNodeAddr] > maxReworks {
			mRec.Status = node.StatusFailed
			mRec.ErrorMessage = fmt.Sprintf("merge rework for %s exceeded max_reworks (%d)", notice.SourceNodeAddr, maxReworks)
			st.Status = RunStatusFailed
			st.ErrorMessage = mRec.ErrorMessage
			break
		}

		delete(mRec.EnqueuedSources, notice.SourceNodeAddr)
		newBaseSHA := notice.Result.LatestTargetSHA
		if newBaseSHA == "" {
			newBaseSHA = mRec.LatestTargetSHA
		}
		if newBaseSHA == "" {
			newBaseSHA = mRec.BaseSHA
		}
		targetBranch := mRec.TargetBranch
		if targetBranch == "" {
			targetBranch = notice.TargetBranch
		}
		reason := notice.Result.FailureReport
		if reason == "" {
			reason = notice.ErrorMessage
		}
		if srcRec != nil {
			srcRec.Status = node.StatusPending
			srcRec.BasedSource = targetBranch
			srcRec.BaseSHA = newBaseSHA
			srcRec.Result = nil
			srcRec.Artifacts = nil
			srcRec.CommitSHA = ""
			if srcRec.ParentDynamicAddr != "" {
				if dyn := st.Nodes[srcRec.ParentDynamicAddr]; dyn != nil {
					updateDynamicAggregateStatus(&st, dyn)
				}
			}
		}
		advanceSource = &struct {
			canonAddr string
			req       *node.AdvanceForReworkReq
		}{
			canonAddr: notice.SourceNodeAddr,
			req: &node.AdvanceForReworkReq{
				BaseSHA:     newBaseSHA,
				BasedSource: targetBranch,
				Reason:      reason,
			},
		}
		shouldReconcile = !st.ManualPaused

	default:
		errMsg := notice.ErrorMessage
		if errMsg == "" {
			errMsg = notice.Result.FailureReport
		}
		mRec.Status = node.StatusFailed
		mRec.ErrorMessage = errMsg
		st.Status = RunStatusFailed
		st.ErrorMessage = fmt.Sprintf("merge %s failed: %s", notice.MergeNodeAddr, errMsg)
	}

	if st.Status != prevStatus {
		statusChanged = true
	}
	if err := a.saveStateLocked(ctx, st); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	if statusChanged {
		a.syncOperatorAndEvents(ctx, st)
	}
	if isTerminalRunStatus(st.Status) && a.opts.ReminderScheduler != nil && a.ID() != "" {
		_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
	}

	if advanceSource != nil {
		resp, aErr := a.opts.NodeController.AdvanceForRework(ctx, st.RunID, advanceSource.canonAddr, advanceSource.req)
		if aErr == nil && resp != nil {
			a.updateNodeEpochFromRPC(ctx, advanceSource.canonAddr, resp.Epoch, node.StatusPending)
		}
	}

	if shouldReconcile {
		_, _ = a.Reconcile(ctx, &ReconcileReq{Reason: "merge_result"})
	}
	return &OnMergeCandidateResultResp{
		Accepted:  true,
		RunStatus: st.Status,
	}, nil
}

// PauseRun handles mode="planned" (PAUSING until active workers finish) and mode="immediate" (abort active workers).
func (a *RunActor) PauseRun(ctx context.Context, req *PauseRunReq) (*PauseRunResp, error) {
	if req == nil {
		req = &PauseRunReq{Mode: "planned"}
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "planned"
	}
	if mode != "planned" && mode != "immediate" {
		return &PauseRunResp{ErrorCode: ErrCodeInvalidInput, ErrorMessage: fmt.Sprintf("invalid pause mode %q", req.Mode)}, nil
	}

	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !exists {
		a.mu.Unlock()
		return &PauseRunResp{ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}
	if isTerminalRunStatus(st.Status) {
		a.mu.Unlock()
		return &PauseRunResp{
			RunID:        st.RunID,
			Status:       st.Status,
			ErrorCode:    ErrCodeConflict,
			ErrorMessage: fmt.Sprintf("cannot pause run in terminal state %s", st.Status),
		}, nil
	}

	st.ManualPaused = true
	var toPause []string
	var toCancel []cancelMergeAction
	for canon, rec := range st.Nodes {
		if rec.Kind == "worker" && isNodeSlotActive(rec.Status) {
			toPause = append(toPause, canon)
		}
		if mode == "immediate" && rec.Kind == "merge" && rec.RepoURL != "" && rec.TargetBranch != "" {
			toCancel = append(toCancel, cancelMergeAction{
				repoURL:      rec.RepoURL,
				targetBranch: rec.TargetBranch,
				req: &merge.CancelByRunReq{
					RunID:         st.RunID,
					MergeNodeAddr: canon,
					Reason:        "pause:immediate",
				},
			})
		}
	}
	sort.Strings(toPause)

	if mode == "immediate" || !hasInFlightExecution(&st) {
		st.Status = RunStatusPaused
	} else {
		st.Status = RunStatusPausing
	}
	if err := a.saveStateLocked(ctx, st); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	for _, canon := range toPause {
		resp, pErr := a.opts.NodeController.PauseNode(ctx, st.RunID, canon, &node.PauseNodeReq{
			Mode:   mode,
			Reason: req.Reason,
		})
		if pErr == nil && resp != nil {
			a.updateNodeEpochFromRPC(ctx, canon, resp.Epoch, resp.Status)
		}
	}
	for _, cm := range toCancel {
		_, _ = a.opts.MergeController.CancelByRun(ctx, cm.repoURL, cm.targetBranch, cm.req)
		if cm.req.MergeNodeAddr != "" {
			a.mu.Lock()
			cur, ok, lErr := a.loadStateLocked(ctx)
			if lErr == nil && ok {
				if mRec := cur.Nodes[cm.req.MergeNodeAddr]; mRec != nil {
					for src, status := range mRec.EnqueuedSources {
						if status != "MERGED" {
							delete(mRec.EnqueuedSources, src)
						}
					}
					_ = a.saveStateLocked(ctx, cur)
				}
			}
			a.mu.Unlock()
		}
	}

	a.mu.Lock()
	cur, ok, _ := a.loadStateLocked(ctx)
	if ok {
		if mode == "immediate" {
			cur.Status = RunStatusPaused
			_ = a.saveStateLocked(ctx, cur)
		}
		st = cur
	}
	a.mu.Unlock()

	a.syncOperatorAndEvents(ctx, st)
	return &PauseRunResp{
		Paused: st.Status == RunStatusPaused || st.Status == RunStatusPausing,
		RunID:  st.RunID,
		Status: st.Status,
	}, nil
}

// ResumeRun clears manual pause, resets paused/failed workers back to PENDING (leaving checkpoints intact),
// and triggers reconciliation.
func (a *RunActor) ResumeRun(ctx context.Context, _ *ResumeRunReq) (*ResumeRunResp, error) {
	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !exists {
		a.mu.Unlock()
		return &ResumeRunResp{ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}
	if st.Status == RunStatusDeleted || st.Status == RunStatusSucceeded {
		a.mu.Unlock()
		return &ResumeRunResp{
			RunID:        st.RunID,
			Status:       st.Status,
			ErrorCode:    ErrCodeConflict,
			ErrorMessage: fmt.Sprintf("cannot resume run in state %s", st.Status),
		}, nil
	}

	st.ManualPaused = false
	st.Status = RunStatusRunning
	st.ErrorMessage = ""
	for _, rec := range st.Nodes {
		if rec.Status == node.StatusPaused || rec.Status == node.StatusFailed {
			rec.Status = node.StatusPending
			rec.ErrorMessage = ""
		}
		if rec.Kind == "dynamic_worker" && rec.DynamicExpanded && rec.Status != node.StatusSucceeded {
			updateDynamicAggregateStatus(&st, rec)
		}
	}
	if err := a.saveStateLocked(ctx, st); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	a.syncOperatorAndEvents(ctx, st)
	if a.opts.ReminderScheduler != nil && a.ID() != "" {
		_ = a.opts.ReminderScheduler.EnsureReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder, a.opts.ReconcileInterval, a.opts.ReconcileInterval)
	}
	recResp, err := a.Reconcile(ctx, &ReconcileReq{Reason: "resume"})
	if err != nil {
		return nil, err
	}
	return &ResumeRunResp{
		Resumed: true,
		RunID:   st.RunID,
		Status:  recResp.Status,
	}, nil
}

// RerunNode rejects reruns that touch shared merged branches with ErrAlreadyMergedSharedBranch,
// and otherwise cascades epoch++ resets across NodeAddr and its transitive downstream subgraph.
func (a *RunActor) RerunNode(ctx context.Context, req *RerunNodeReq) (*RerunNodeResp, error) {
	if req == nil || strings.TrimSpace(req.NodeAddr) == "" {
		return &RerunNodeResp{Accepted: false, ErrorCode: ErrCodeInvalidInput, ErrorMessage: "node_addr is required"}, nil
	}

	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !exists {
		a.mu.Unlock()
		return &RerunNodeResp{Accepted: false, ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}

	loaded, err := a.ensureArchiveLoadedNoLockHint(&st)
	if err != nil {
		a.mu.Unlock()
		loaded, err = a.ensureArchiveLoaded(ctx, st)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		st, exists, err = a.loadStateLocked(ctx)
		if err != nil || !exists {
			a.mu.Unlock()
			return &RerunNodeResp{Accepted: false, ErrorCode: ErrCodeNotFound}, nil
		}
	}

	targetAddr := strings.TrimSpace(req.NodeAddr)
	targetRec, ok := st.Nodes[targetAddr]
	if !ok {
		a.mu.Unlock()
		return &RerunNodeResp{
			Accepted:     false,
			RunStatus:    st.Status,
			ErrorCode:    ErrCodeNotFound,
			ErrorMessage: fmt.Sprintf("node %q not found", targetAddr),
		}, nil
	}

	affected := append([]string{targetAddr}, collectDownstreamNodes(loaded.Plan, &st, targetAddr)...)
	if anyMergedIntoSharedBranch(&st, affected) {
		a.mu.Unlock()
		return &RerunNodeResp{
			Accepted:                  false,
			AlreadyMergedSharedBranch: true,
			RunStatus:                 st.Status,
			ErrorCode:                 ErrCodeAlreadyMergedSharedBranch,
			ErrorMessage:              fmt.Sprintf("%s or a downstream node has already merged into shared target_branch", targetAddr),
		}, nil
	}

	var cancelMerges []cancelMergeAction
	var resetNodes []string
	for _, addr := range affected {
		if strings.HasSuffix(addr, "::outputs") {
			modAddr := strings.TrimSuffix(addr, "::outputs")
			if modRec := st.Nodes[modAddr]; modRec != nil && modRec.Kind == "module" {
				modRec.Status = node.StatusPending
				modRec.ModuleOutputs = nil
				modRec.ErrorMessage = ""
			}
			continue
		}
		rec := st.Nodes[addr]
		if rec == nil {
			continue
		}
		switch rec.Kind {
		case "merge":
			if rec.RepoURL != "" && rec.TargetBranch != "" {
				cancelMerges = append(cancelMerges, cancelMergeAction{
					repoURL:      rec.RepoURL,
					targetBranch: rec.TargetBranch,
					req: &merge.CancelByRunReq{
						RunID:         st.RunID,
						MergeNodeAddr: addr,
						Reason:        "rerun:" + targetAddr,
					},
				})
			}
			resetMergeRecordForCascade(rec)
		case "dynamic_worker":
			resetDynamicParentForCascade(&st, rec, &resetNodes)
		case "module":
			resetModuleRecordForCascade(rec)
		case "worker":
			clearBase := addr != targetAddr
			resetWorkerRecordForCascade(rec, clearBase)
			if rec.ParentDynamicAddr != "" {
				if dyn := st.Nodes[rec.ParentDynamicAddr]; dyn != nil {
					updateDynamicAggregateStatus(&st, dyn)
				}
			}
			resetNodes = append(resetNodes, addr)
		}
	}

	// Re-open Run status for re-execution.
	_ = targetRec
	st.Status = RunStatusRunning
	st.ManualPaused = false
	st.ErrorMessage = ""
	st.Outputs = make(map[string]EvaluatedOutput)

	if err := a.saveStateLocked(ctx, st); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()

	a.syncOperatorAndEvents(ctx, st)
	for _, cm := range cancelMerges {
		_, _ = a.opts.MergeController.CancelByRun(ctx, cm.repoURL, cm.targetBranch, cm.req)
	}
	for _, addr := range resetNodes {
		resp, rErr := a.opts.NodeController.ResetForRerun(ctx, st.RunID, addr, &node.ResetForRerunReq{
			ClearBaseSHA:       addr != targetAddr,
			ClearInterjections: true,
			Reason:             req.Reason,
		})
		if rErr == nil && resp != nil {
			a.updateNodeEpochFromRPC(ctx, addr, resp.Epoch, node.StatusPending)
		}
	}

	if a.opts.ReminderScheduler != nil && a.ID() != "" {
		_ = a.opts.ReminderScheduler.EnsureReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder, a.opts.ReconcileInterval, a.opts.ReconcileInterval)
	}
	recResp, err := a.Reconcile(ctx, &ReconcileReq{Reason: "rerun:" + targetAddr})
	if err != nil {
		return nil, err
	}
	return &RerunNodeResp{
		Accepted:  true,
		RunStatus: recResp.Status,
	}, nil
}

// DeleteRun fences active execution and destroys all non-preserved managed branches,
// Run BlobStore objects, Run-scoped ConfigActor entries, and the OperatorActor index entry.
func (a *RunActor) DeleteRun(ctx context.Context, _ *DeleteRunReq) (*DeleteRunResp, error) {
	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !exists {
		a.mu.Unlock()
		return &DeleteRunResp{Deleted: false, ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}

	loaded, _ := a.ensureArchiveLoadedNoLockHint(&st)
	a.mu.Unlock()
	if loaded == nil && st.PlanBlobKey != "" && a.opts.BlobStore != nil {
		loaded, _ = a.ensureArchiveLoaded(ctx, st)
	}

	a.mu.Lock()
	st, exists, err = a.loadStateLocked(ctx)
	if err != nil || !exists {
		a.mu.Unlock()
		return &DeleteRunResp{Deleted: false, ErrorCode: ErrCodeNotFound}, nil
	}

	var toPause []string
	var toCancel []cancelMergeAction
	for canon, rec := range st.Nodes {
		if rec.Kind == "worker" && isNodeSlotActive(rec.Status) {
			toPause = append(toPause, canon)
		}
		if rec.Kind == "merge" && rec.RepoURL != "" && rec.TargetBranch != "" {
			toCancel = append(toCancel, cancelMergeAction{
				repoURL:      rec.RepoURL,
				targetBranch: rec.TargetBranch,
				req: &merge.CancelByRunReq{
					RunID:         st.RunID,
					MergeNodeAddr: canon,
					Reason:        "delete_run",
				},
			})
		}
	}

	if loaded != nil && loaded.Plan != nil {
		if evalCtx, eErr := buildScopeEvalContext(&st, loaded.Plan, nil, ""); eErr == nil {
			a.evaluatePreservesRecursive(&st, loaded.Plan, nil, "", evalCtx)
		}
	}

	sources := make([]git.SourceRef, 0, len(st.Sources))
	for _, s := range st.Sources {
		sources = append(sources, git.SourceRef{URL: s.URL, Branch: s.Branch})
	}
	preserves := make([]git.PreserveRef, 0, len(st.EvaluatedPreserves))
	for _, p := range st.EvaluatedPreserves {
		preserves = append(preserves, p)
	}
	ledger, lErr := restoreLedger(st.Ledger)
	var cleanupTargets []git.BranchTarget
	if lErr == nil {
		cleanupTargets = ledger.PlanCleanup(sources, preserves)
	}
	sourceAuthByRepo := buildSourceAuthMap(&st)
	runID := st.RunID
	ns := st.Namespace
	runOverrides := maps.Clone(st.RunOverrides)
	extractDir := st.BundleExtractDir
	a.mu.Unlock()

	for _, canon := range toPause {
		_, _ = a.opts.NodeController.PauseNode(ctx, runID, canon, &node.PauseNodeReq{
			Mode:   "immediate",
			Reason: "delete_run",
		})
	}
	for _, cm := range toCancel {
		_, _ = a.opts.MergeController.CancelByRun(ctx, cm.repoURL, cm.targetBranch, cm.req)
	}

	var deletedBranches []git.BranchTarget
	for _, target := range cleanupTargets {
		auth, keys := authRefsForRepo(sourceAuthByRepo, target.RepoURL)
		resolvedAuth, err := a.resolveGitAuth(ctx, runID, ns, runOverrides, auth, keys)
		if err != nil {
			return &DeleteRunResp{
				Deleted:      false,
				Status:       st.Status,
				ErrorCode:    ErrCodeConflict,
				ErrorMessage: err.Error(),
			}, nil
		}
		if err := a.opts.GitAdapter.DeleteRemoteBranch(ctx, target.RepoURL, target.Branch, resolvedAuth); err != nil {
			return &DeleteRunResp{
				Deleted:      false,
				Status:       st.Status,
				ErrorCode:    ErrCodeConflict,
				ErrorMessage: fmt.Sprintf("delete remote branch %s: %v", target.Branch, err),
			}, nil
		}
		deletedBranches = append(deletedBranches, target)
	}

	if a.opts.BlobStore != nil {
		if err := a.opts.BlobStore.DeletePrefix(ctx, fmt.Sprintf("runs/%s/", runID)); err != nil {
			return nil, fmt.Errorf("delete run blobs: %w", err)
		}
	}
	if a.opts.ConfigManager != nil {
		if _, err := a.opts.ConfigManager.DeleteRunScope(ctx, &actorcfg.DeleteRunScopeReq{RunID: runID}); err != nil {
			return nil, fmt.Errorf("delete run config scope: %w", err)
		}
	}
	if a.opts.OperatorIndex != nil {
		if _, err := a.opts.OperatorIndex.RemoveRun(ctx, &RemoveRunReq{RunID: runID}); err != nil {
			return nil, fmt.Errorf("remove run from operator index: %w", err)
		}
	}
	if a.opts.ReminderScheduler != nil && a.ID() != "" {
		_ = a.opts.ReminderScheduler.UnregisterReconcileReminder(ctx, RunActorType, a.ID(), a.opts.ReconcileReminder)
	}
	if extractDir != "" {
		_ = os.RemoveAll(extractDir)
	}

	a.mu.Lock()
	st, exists, err = a.loadStateLocked(ctx)
	if err == nil && exists {
		st.Status = RunStatusDeleted
		a.cachedArchive = nil
		_ = a.saveStateLocked(ctx, st)
	}
	a.mu.Unlock()

	if a.opts.EventPublisher != nil {
		_ = a.opts.EventPublisher.PublishEvent(ctx, a.opts.PubSubName, a.opts.EventsTopic, map[string]any{
			"run_id":     runID,
			"status":     RunStatusDeleted,
			"updated_at": a.opts.Clock(),
		})
	}
	return &DeleteRunResp{
		Deleted:         true,
		DeletedBranches: deletedBranches,
		Status:          RunStatusDeleted,
	}, nil
}

// GetRunState returns the authoritative state snapshot of the RunActor.
func (a *RunActor) GetRunState(ctx context.Context, _ *GetRunStateReq) (*GetRunStateResp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	st, exists, err := a.loadStateLocked(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &GetRunStateResp{ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}

	nodes := make([]NodeRunSummary, 0, len(st.Nodes))
	for _, rec := range st.Nodes {
		nodes = append(nodes, NodeRunSummary{
			Address:                rec.Address,
			CanonicalAddress:       rec.CanonicalAddress,
			Kind:                   rec.Kind,
			ModulePath:             rec.ModulePath,
			Status:                 rec.Status,
			CurrentEpoch:           rec.CurrentEpoch,
			WorkBranch:             rec.WorkBranch,
			CommitSHA:              rec.CommitSHA,
			Artifacts:              slices.Clone(rec.Artifacts),
			MergedIntoSharedBranch: rec.MergedIntoSharedBranch,
			DynamicKeys:            slices.Clone(rec.DynamicKeys),
			ErrorMessage:           rec.ErrorMessage,
		})
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].CanonicalAddress < nodes[j].CanonicalAddress
	})

	sources := make([]applyeval.SourceRuntimeState, 0, len(st.Sources))
	for _, s := range st.Sources {
		sources = append(sources, applyeval.SourceRuntimeState{
			Address:   s.Address,
			Name:      s.Name,
			URL:       s.URL,
			Branch:    s.Branch,
			CommitSHA: s.CommitSHA,
		})
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Address < sources[j].Address
	})

	preserves := make([]git.PreserveRef, 0, len(st.EvaluatedPreserves))
	for _, p := range st.EvaluatedPreserves {
		preserves = append(preserves, p)
	}
	sort.Slice(preserves, func(i, j int) bool {
		if preserves[i].URL != preserves[j].URL {
			return preserves[i].URL < preserves[j].URL
		}
		return preserves[i].Branch < preserves[j].Branch
	})

	return &GetRunStateResp{
		RunID:        st.RunID,
		Namespace:    st.Namespace,
		Status:       st.Status,
		ManualPaused: st.ManualPaused,
		PlanBlobKey:  st.PlanBlobKey,
		PlanSHA256:   st.PlanSHA256,
		ErrorMessage: st.ErrorMessage,
		Nodes:        nodes,
		Outputs:      maps.Clone(st.Outputs),
		Sources:      sources,
		Preserves:    preserves,
		Ledger:       st.Ledger,
		CreatedAt:    st.CreatedAt,
		UpdatedAt:    st.UpdatedAt,
	}, nil
}

// ListDecisions collects DecisionCards across all Run worker nodes.
func (a *RunActor) ListDecisions(ctx context.Context, _ *ListDecisionsReq) (*ListDecisionsResp, error) {
	a.mu.Lock()
	st, exists, err := a.loadStateLocked(ctx)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !exists {
		return &ListDecisionsResp{ErrorCode: ErrCodeNotFound, ErrorMessage: "run not found"}, nil
	}

	var workerAddrs []string
	for canon, rec := range st.Nodes {
		if rec.Kind == "worker" {
			workerAddrs = append(workerAddrs, canon)
		}
	}
	sort.Strings(workerAddrs)

	var cards []node.DecisionCard
	for _, addr := range workerAddrs {
		ns, err := a.opts.NodeController.GetNodeState(ctx, st.RunID, addr)
		if err != nil || ns == nil {
			continue
		}
		cards = append(cards, ns.DecisionCards...)
	}
	return &ListDecisionsResp{
		RunID:     st.RunID,
		Decisions: cards,
	}, nil
}

func (a *RunActor) ensureArchiveLoadedNoLockHint(_ *persistedRunState) (*plan.LoadedArchive, error) {
	if a.cachedArchive != nil && a.cachedArchive.Plan != nil {
		return a.cachedArchive, nil
	}
	return nil, errors.New("archive not cached")
}

func (a *RunActor) updateNodeEpochFromRPC(ctx context.Context, canonAddr string, epoch uint64, status node.NodeStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok, err := a.loadStateLocked(ctx)
	if err != nil || !ok {
		return
	}
	if rec := cur.Nodes[canonAddr]; rec != nil {
		if epoch > rec.CurrentEpoch {
			rec.CurrentEpoch = epoch
		}
		if status != "" {
			rec.Status = status
		}
		_ = a.saveStateLocked(ctx, cur)
	}
}

func (a *RunActor) resolveGitAuth(
	ctx context.Context,
	runID, namespace string,
	runOverrides map[string]string,
	rawAuth git.AuthConfig,
	keys []string,
) (git.AuthConfig, error) {
	if len(keys) == 0 || a.opts.ConfigManager == nil {
		return rawAuth, nil
	}
	resp, err := a.opts.ConfigManager.ResolveForNode(ctx, &actorcfg.ResolveForNodeReq{
		RunID:        runID,
		Namespace:    namespace,
		RunOverrides: runOverrides,
		Keys:         keys,
	})
	if err != nil {
		return git.AuthConfig{}, err
	}
	out := git.AuthConfig{}
	if rawAuth.TokenRef != nil {
		if v, ok := resp.Values[*rawAuth.TokenRef]; ok {
			out.TokenRef = &v
		}
	}
	if rawAuth.SSHKeyRef != nil {
		if v, ok := resp.Values[*rawAuth.SSHKeyRef]; ok {
			out.SSHKeyRef = &v
		}
	}
	return out, nil
}

func (a *RunActor) resolveBranchBaseSHA(
	ctx context.Context,
	st *persistedRunState,
	currentPlan *plan.Plan,
	parentPlans []*plan.Plan,
	repoURL, branch string,
) (string, error) {
	// 1. Pinned source branches.
	for _, s := range st.Sources {
		if s.Branch == branch && (repoURL == "" || git.NormalizeRepoURL(s.URL) == git.NormalizeRepoURL(repoURL)) {
			return s.CommitSHA, nil
		}
	}
	// 2. Completed upstream worker or merge nodes producing work_branch == branch.
	for _, rec := range st.Nodes {
		if rec.WorkBranch == branch && rec.CommitSHA != "" &&
			(repoURL == "" || rec.RepoURL == "" || git.NormalizeRepoURL(rec.RepoURL) == git.NormalizeRepoURL(repoURL)) {
			return rec.CommitSHA, nil
		}
	}
	// 3. Explicit external branch via GitAdapter.
	authMap := buildSourceAuthMap(st)
	auth, keys := authRefsForRepo(authMap, repoURL)
	resolvedAuth, err := a.resolveGitAuth(ctx, st.RunID, st.Namespace, st.RunOverrides, auth, keys)
	if err != nil {
		return "", err
	}
	_ = currentPlan
	_ = parentPlans
	return a.opts.GitAdapter.ResolveBranchSHA(ctx, repoURL, branch, resolvedAuth)
}

func (a *RunActor) evaluatePreservesRecursive(
	st *persistedRunState,
	p *plan.Plan,
	parentPlans []*plan.Plan,
	modulePrefix string,
	evalCtx *hcl.EvalContext,
) bool {
	if p == nil || evalCtx == nil {
		return false
	}
	mutated := false
	for _, n := range p.Nodes() {
		canon := canonicalForNode(n, modulePrefix)
		switch n.Kind {
		case "preserve":
			urlStr := readCtyStringAttr(n.Config, "url")
			if urlStr == "" {
				urlStr = inferSingleSourceURL(st, p)
			}
			var branchVal cty.Value
			branchExpr := strings.TrimSpace(readCtyStringAttr(n.Config, "branch_expr"))
			if branchExpr != "" {
				if expr, diags := hclsyntax.ParseExpression([]byte(branchExpr), canon+".branch", hcl.InitialPos); !diags.HasErrors() {
					if v, vDiags := expr.Value(evalCtx); !vDiags.HasErrors() {
						branchVal = v
					}
				}
			} else if n.Config != cty.NilVal && n.Config.Type().IsObjectType() && n.Config.Type().HasAttribute("branch") {
				branchVal = n.Config.GetAttr("branch")
			}
			if branchVal != cty.NilVal {
				unmarked, _ := branchVal.UnmarkDeep()
				if !unmarked.IsNull() && unmarked.IsWhollyKnown() && unmarked.Type().Equals(cty.String) {
					branch := strings.TrimSpace(unmarked.AsString())
					if branch != "" {
						ref := git.PreserveRef{URL: urlStr, Branch: branch}
						if prev, ok := st.EvaluatedPreserves[canon]; !ok || prev != ref {
							st.EvaluatedPreserves[canon] = ref
							if rec := st.Nodes[canon]; rec != nil {
								rec.WorkBranch = branch
								rec.Status = node.StatusSucceeded
							}
							mutated = true
						}
					}
				}
			}
		case "module":
			if n.Module != nil && n.Module.Plan != nil {
				childParents := append(slices.Clone(parentPlans), p)
				if childCtx, err := buildScopeEvalContext(st, n.Module.Plan, childParents, canon); err == nil {
					if a.evaluatePreservesRecursive(st, n.Module.Plan, childParents, canon, childCtx) {
						mutated = true
					}
				}
			}
		}
	}
	return mutated
}

func buildScopeEvalContext(
	st *persistedRunState,
	p *plan.Plan,
	parentPlans []*plan.Plan,
	modulePrefix string,
) (*hcl.EvalContext, error) {
	effPlan := p
	if modulePrefix != "" {
		if modRec := st.Nodes[modulePrefix]; modRec != nil && len(modRec.BoundInputs) > 0 {
			varMap := make(map[string]cty.Value)
			if existingVar, ok := p.Context()["var"]; ok && existingVar != cty.NilVal {
				unmarked, _ := existingVar.UnmarkDeep()
				if !unmarked.IsNull() && unmarked.IsKnown() && (unmarked.Type().IsObjectType() || unmarked.Type().IsMapType()) {
					varMap = existingVar.AsValueMap()
				}
			}
			for k, wv := range modRec.BoundInputs {
				v, err := wv.DecodeWireValue()
				if err != nil {
					return nil, err
				}
				varMap[k] = v
			}
			updatedCtx := p.Context()
			if updatedCtx == nil {
				updatedCtx = make(map[string]cty.Value, 1)
			}
			if len(varMap) > 0 {
				updatedCtx["var"] = cty.ObjectVal(varMap)
			}
			var err error
			effPlan, err = plan.New(plan.Args{
				Directory:           p.Directory(),
				BundlePath:          p.BundlePath(),
				Nodes:               p.Nodes(),
				Outputs:             p.Outputs(),
				Context:             updatedCtx,
				LocalExpressions:    p.LocalExpressions(),
				DeferredValidations: p.DeferredValidations(),
				Tools:               p.Tools(),
				PluginLock:          p.PluginLock(),
			})
			if err != nil {
				return nil, err
			}
		}
	}

	rt := applyeval.RuntimeState{
		Sources:        make(map[string]applyeval.SourceRuntimeState),
		Workers:        make(map[string]applyeval.WorkerRuntimeResult),
		DynamicWorkers: make(map[string]applyeval.DynamicWorkerRuntimeResult),
		Merges:         make(map[string]applyeval.MergeRuntimeResult),
		Modules:        make(map[string]applyeval.ModuleRuntimeResult),
	}

	for _, n := range effPlan.Nodes() {
		canon := canonicalForNode(n, modulePrefix)
		switch n.Kind {
		case "source":
			if sRec, ok := st.Sources[canon]; ok {
				rt.Sources[n.Address] = applyeval.SourceRuntimeState{
					Address:   n.Address,
					Name:      sRec.Name,
					URL:       sRec.URL,
					Branch:    sRec.Branch,
					CommitSHA: sRec.CommitSHA,
				}
			}
		case "worker":
			if rec := st.Nodes[canon]; rec != nil && rec.Status == node.StatusSucceeded {
				rt.Workers[n.Address] = toWorkerRuntimeResult(n.Address, nodeSimpleName(n), rec)
			}
		case "dynamic_worker":
			if rec := st.Nodes[canon]; rec != nil && rec.Status == node.StatusSucceeded {
				instances := make(map[string]applyeval.WorkerRuntimeResult, len(rec.DynamicKeys))
				for _, k := range rec.DynamicKeys {
					inst := st.Nodes[dynamicInstanceCanon(canon, k)]
					if inst != nil {
						instances[k] = toWorkerRuntimeResult(fmt.Sprintf("%s[%s]", n.Address, k), k, inst)
					}
				}
				rt.DynamicWorkers[n.Address] = applyeval.DynamicWorkerRuntimeResult{
					Address:   n.Address,
					Name:      nodeSimpleName(n),
					Keys:      slices.Clone(rec.DynamicKeys),
					Instances: instances,
				}
			}
		case "merge":
			if rec := st.Nodes[canon]; rec != nil && rec.Status == node.StatusSucceeded {
				rt.Merges[n.Address] = applyeval.MergeRuntimeResult{
					Address:    n.Address,
					Name:       nodeSimpleName(n),
					WorkBranch: rec.WorkBranch,
					CommitSHA:  rec.CommitSHA,
				}
			}
		case "module":
			if rec := st.Nodes[canon]; rec != nil && rec.Status == node.StatusSucceeded {
				outs := make(map[string]cty.Value, len(rec.ModuleOutputs))
				for k, eo := range rec.ModuleOutputs {
					v, err := eo.DecodeValue()
					if err != nil {
						return nil, err
					}
					outs[k] = v
				}
				rt.Modules[n.Address] = applyeval.ModuleRuntimeResult{
					Address: n.Address,
					Outputs: outs,
				}
			}
		}
	}

	return applyeval.BuildEvalContext(effPlan, rt, applyeval.WithPlan(effPlan), applyeval.WithParentPlans(parentPlans...))
}

func toWorkerRuntimeResult(addr, name string, rec *nodeStateRecord) applyeval.WorkerRuntimeResult {
	arts := make(map[string]applyeval.ArtifactRuntimeState, len(rec.Artifacts))
	for _, a := range rec.Artifacts {
		arts[a.Name] = applyeval.ArtifactRuntimeState{
			ID:            a.ID,
			Name:          a.Name,
			Type:          a.Type,
			RelPath:       a.RelPath,
			ContainerPath: a.ContainerPath,
		}
	}
	return applyeval.WorkerRuntimeResult{
		Address:    addr,
		Name:       name,
		WorkBranch: rec.WorkBranch,
		CommitSHA:  rec.CommitSHA,
		Result:     rec.Result,
		Artifacts:  arts,
	}
}

func evaluatePlanOutputsMap(p *plan.Plan, evalCtx *hcl.EvalContext) (map[string]EvaluatedOutput, error) {
	vals, err := applyeval.EvalPlanOutputs(p, evalCtx)
	if err != nil {
		return nil, err
	}
	specs := p.Outputs()
	out := make(map[string]EvaluatedOutput, len(vals))
	for name, val := range vals {
		spec := specs[name]
		wv, err := EncodeWireValue(val)
		if err != nil {
			return nil, fmt.Errorf("encode output %q: %w", name, err)
		}
		desc := ""
		if spec.Description != nil {
			desc = *spec.Description
		}
		out[name] = EvaluatedOutput{
			Name:           name,
			Description:    desc,
			Sensitive:      spec.Sensitive || wv.Sensitive,
			HasMarks:       wv.HasMarks || spec.Sensitive,
			Unknown:        wv.Unknown,
			TypeJSON:       wv.TypeJSON,
			ValueJSON:      wv.ValueJSON,
			SensitivePaths: wv.SensitivePaths,
		}
	}
	return out, nil
}

// collectMergeBaseExprDeps distinguishes dependencies referenced by merge.based_source
// from candidate-producing sources so streaming merge starts as soon as based_source is known.
func areMergeBaseDepsSucceeded(
	st *persistedRunState,
	currentPlan *plan.Plan,
	nodeByLocal map[string]plan.NodeSpec,
	modulePrefix string,
	mergeNode plan.NodeSpec,
) bool {
	// Collect variable references used ONLY inside based_source_expr (through locals),
	// falling back to excluding worker/dynamic_worker refs that appear in sources_expr.
	basedExpr := readCtyStringAttr(mergeNode.Config, "based_source_expr")
	sourcesExpr := readCtyStringAttr(mergeNode.Config, "sources_expr")
	if strings.TrimSpace(basedExpr) != "" && strings.TrimSpace(sourcesExpr) != "" {
		basedDeps := collectExprBlockRefs(currentPlan, basedExpr)
		sourceDeps := collectExprBlockRefs(currentPlan, sourcesExpr)
		for _, dep := range mergeNode.Dependencies {
			depNode, ok := nodeByLocal[dep]
			if !ok {
				continue
			}
			// Must wait for based_source dependencies and non-candidate dependencies (explicit depends_on),
			// while NOT blocking on pure candidate-producing sources referenced in sources_expr!
			_, inBased := basedDeps[dep]
			_, inSources := sourceDeps[dep]
			if inSources && !inBased && (depNode.Kind == "worker" || depNode.Kind == "dynamic_worker") {
				continue
			}
			if !isLocalDepSucceeded(st, depNode, modulePrefix) {
				return false
			}
		}
		return true
	}

	// If no sources_expr metadata exists, only non-candidate nodes block EvalMergeBase;
	// Wait for explicit source/merge/module/non-candidate deps.
	for _, dep := range mergeNode.Dependencies {
		depNode, ok := nodeByLocal[dep]
		if !ok {
			continue
		}
		if depNode.Kind == "worker" || depNode.Kind == "dynamic_worker" {
			continue
		}
		if !isLocalDepSucceeded(st, depNode, modulePrefix) {
			return false
		}
	}
	return true
}

func collectExprBlockRefs(p *plan.Plan, rawExpr string) map[string]struct{} {
	out := make(map[string]struct{})
	if p == nil || strings.TrimSpace(rawExpr) == "" {
		return out
	}
	localExprs := p.LocalExpressions()
	visitedLocals := make(map[string]bool)
	var inspect func(hcl.Expression)
	inspect = func(expr hcl.Expression) {
		if expr == nil {
			return
		}
		for _, tr := range expr.Variables() {
			root := tr.RootName()
			if root == "local" && len(tr) >= 2 {
				if attr, ok := tr[1].(hcl.TraverseAttr); ok && !visitedLocals[attr.Name] {
					visitedLocals[attr.Name] = true
					if raw, ok := localExprs[attr.Name]; ok {
						if parsed, diags := hclsyntax.ParseExpression([]byte(raw), "local."+attr.Name, hcl.InitialPos); !diags.HasErrors() {
							inspect(parsed)
						}
					}
				}
				continue
			}
			if len(tr) >= 2 {
				if attr, ok := tr[1].(hcl.TraverseAttr); ok {
					out[root+"."+attr.Name] = struct{}{}
				}
			}
		}
	}
	if parsed, diags := hclsyntax.ParseExpression([]byte(rawExpr), "expr", hcl.InitialPos); !diags.HasErrors() {
		inspect(parsed)
	}
	return out
}

type mergeCandidateSource struct {
	canonAddr  string
	epoch      uint64
	workBranch string
}

func collectMergeCandidateSources(
	st *persistedRunState,
	currentPlan *plan.Plan,
	modulePrefix string,
	mergeNode plan.NodeSpec,
) []mergeCandidateSource {
	sourcesExpr := readCtyStringAttr(mergeNode.Config, "sources_expr")
	refSet := collectExprBlockRefs(currentPlan, sourcesExpr)
	nodeByLocal := make(map[string]plan.NodeSpec, len(currentPlan.Nodes()))
	for _, n := range currentPlan.Nodes() {
		nodeByLocal[n.Address] = n
	}

	var candidateAddrs []string
	if len(refSet) > 0 {
		for ref := range refSet {
			if n, ok := nodeByLocal[ref]; ok && (n.Kind == "worker" || n.Kind == "dynamic_worker") {
				candidateAddrs = append(candidateAddrs, ref)
			}
		}
	} else {
		for _, dep := range mergeNode.Dependencies {
			if n, ok := nodeByLocal[dep]; ok && (n.Kind == "worker" || n.Kind == "dynamic_worker") {
				candidateAddrs = append(candidateAddrs, dep)
			}
		}
	}
	sort.Strings(candidateAddrs)

	var out []mergeCandidateSource
	for _, localAddr := range candidateAddrs {
		canon := canonicalForNode(nodeByLocal[localAddr], modulePrefix)
		rec := st.Nodes[canon]
		if rec == nil {
			continue
		}
		if rec.Kind == "worker" {
			if rec.Status == node.StatusSucceeded && rec.WorkBranch != "" {
				out = append(out, mergeCandidateSource{
					canonAddr:  rec.CanonicalAddress,
					epoch:      rec.CurrentEpoch,
					workBranch: rec.WorkBranch,
				})
			}
			continue
		}
		if rec.Kind == "dynamic_worker" && rec.DynamicExpanded {
			for _, k := range rec.DynamicKeys {
				inst := st.Nodes[dynamicInstanceCanon(canon, k)]
				if inst != nil && inst.Status == node.StatusSucceeded && inst.WorkBranch != "" {
					out = append(out, mergeCandidateSource{
						canonAddr:  inst.CanonicalAddress,
						epoch:      inst.CurrentEpoch,
						workBranch: inst.WorkBranch,
					})
				}
			}
		}
	}
	return out
}

func areLocalDepsSucceeded(
	st *persistedRunState,
	nodeByLocal map[string]plan.NodeSpec,
	modulePrefix string,
	deps []string,
) bool {
	for _, dep := range deps {
		depNode, ok := nodeByLocal[dep]
		if !ok {
			continue
		}
		if !isLocalDepSucceeded(st, depNode, modulePrefix) {
			return false
		}
	}
	return true
}

func isLocalDepSucceeded(st *persistedRunState, depNode plan.NodeSpec, modulePrefix string) bool {
	switch depNode.Kind {
	case "source", "sandbox", "harness", "model_provider", "mcp_server", "go_tool", "external_tool", "preserve":
		return true
	default:
		rec := st.Nodes[canonicalForNode(depNode, modulePrefix)]
		return rec != nil && rec.Status == node.StatusSucceeded
	}
}

func areModuleChildrenSucceeded(st *persistedRunState, childPlan *plan.Plan, moduleCanon string) bool {
	if childPlan == nil {
		return true
	}
	for _, n := range childPlan.Nodes() {
		canon := canonicalForNode(n, moduleCanon)
		switch n.Kind {
		case "worker", "dynamic_worker", "merge", "module":
			rec := st.Nodes[canon]
			if rec == nil || rec.Status != node.StatusSucceeded {
				return false
			}
		}
	}
	return true
}

func allDAGNodesSucceeded(st *persistedRunState) bool {
	for _, rec := range st.Nodes {
		switch rec.Kind {
		case "worker", "dynamic_worker", "merge", "module":
			if rec.Status != node.StatusSucceeded {
				return false
			}
		}
	}
	return true
}

func hasInFlightExecution(st *persistedRunState) bool {
	for _, rec := range st.Nodes {
		switch rec.Kind {
		case "worker":
			if isNodeSlotActive(rec.Status) {
				return true
			}
		case "merge":
			for _, v := range rec.EnqueuedSources {
				if v == "ENQUEUING" || v == "ENQUEUED" {
					return true
				}
			}
		}
	}
	return false
}

func hasWaitingHuman(st *persistedRunState) bool {
	for _, rec := range st.Nodes {
		if rec.Kind == "worker" && rec.Status == node.StatusWaitingHuman {
			return true
		}
	}
	return false
}

func isNodeSlotActive(s node.NodeStatus) bool {
	return s == node.StatusQueuedSlot || s == node.StatusRunning
}

func updateDynamicAggregateStatus(st *persistedRunState, dynRec *nodeStateRecord) {
	if !dynRec.DynamicExpanded {
		dynRec.Status = node.StatusPending
		return
	}
	if len(dynRec.DynamicKeys) == 0 {
		dynRec.Status = node.StatusSucceeded
		return
	}
	allSucceeded := true
	anyWaiting := false
	for _, k := range dynRec.DynamicKeys {
		inst := st.Nodes[dynamicInstanceCanon(dynRec.CanonicalAddress, k)]
		if inst == nil || inst.Status != node.StatusSucceeded {
			allSucceeded = false
		}
		if inst != nil && inst.Status == node.StatusWaitingHuman {
			anyWaiting = true
		}
	}
	switch {
	case allSucceeded:
		dynRec.Status = node.StatusSucceeded
	case anyWaiting:
		dynRec.Status = node.StatusWaitingHuman
	default:
		dynRec.Status = node.StatusRunning
	}
}

func ensureNodeRecord(st *persistedRunState, n plan.NodeSpec, modulePrefix string) *nodeStateRecord {
	canon := canonicalForNode(n, modulePrefix)
	if rec, ok := st.Nodes[canon]; ok {
		return rec
	}
	epoch := uint64(0)
	if n.Kind == "worker" {
		epoch = 1
	}
	rec := &nodeStateRecord{
		Address:          n.Address,
		CanonicalAddress: canon,
		Kind:             n.Kind,
		ModulePath:       modulePrefix,
		Status:           node.StatusPending,
		CurrentEpoch:     epoch,
		MergeEpoch:       1,
	}
	st.Nodes[canon] = rec
	return rec
}

func collectDownstreamNodes(root *plan.Plan, st *persistedRunState, startCanon string) []string {
	type edgeGraph struct {
		adj map[string][]string
	}
	g := edgeGraph{adj: make(map[string][]string)}
	addEdge := func(from, to string) {
		g.adj[from] = append(g.adj[from], to)
	}

	var walk func(p *plan.Plan, prefix string)
	walk = func(p *plan.Plan, prefix string) {
		if p == nil {
			return
		}
		localByAddr := make(map[string]plan.NodeSpec, len(p.Nodes()))
		for _, n := range p.Nodes() {
			localByAddr[n.Address] = n
		}
		for _, n := range p.Nodes() {
			canon := canonicalForNode(n, prefix)
			for _, dep := range n.Dependencies {
				depNode, ok := localByAddr[dep]
				if !ok {
					continue
				}
				depCanon := canonicalForNode(depNode, prefix)
				addEdge(depCanon, canon)
				if depNode.Kind == "module" {
					addEdge(depCanon+"::outputs", canon)
				}
				if depNode.Kind == "dynamic_worker" {
					if dyn := st.Nodes[depCanon]; dyn != nil {
						for _, k := range dyn.DynamicKeys {
							addEdge(dynamicInstanceCanon(depCanon, k), canon)
						}
					}
				}
			}
			if n.Kind == "dynamic_worker" {
				if dyn := st.Nodes[canon]; dyn != nil {
					for _, k := range dyn.DynamicKeys {
						addEdge(canon, dynamicInstanceCanon(canon, k))
					}
				}
			}
			if n.Kind == "module" && n.Module != nil && n.Module.Plan != nil {
				addEdge(canon, canon+"::outputs")
				walk(n.Module.Plan, canon)
				for _, cn := range n.Module.Plan.Nodes() {
					cnCanon := canonicalForNode(cn, canon)
					addEdge(canon, cnCanon)
					addEdge(cnCanon, canon+"::outputs")
					if cn.Kind == "dynamic_worker" {
						if dyn := st.Nodes[cnCanon]; dyn != nil {
							for _, k := range dyn.DynamicKeys {
								addEdge(dynamicInstanceCanon(cnCanon, k), canon+"::outputs")
							}
						}
					}
				}
			}
		}
	}
	walk(root, "")

	visited := map[string]bool{startCanon: true}
	queue := []string{startCanon}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.adj[cur] {
			if visited[next] {
				continue
			}
			visited[next] = true
			out = append(out, next)
			queue = append(queue, next)
		}
	}
	return out
}

func anyMergedIntoSharedBranch(st *persistedRunState, addrs []string) bool {
	for _, addr := range addrs {
		rec := st.Nodes[addr]
		if rec == nil {
			continue
		}
		if rec.MergedIntoSharedBranch {
			return true
		}
		if rec.Kind == "dynamic_worker" && rec.DynamicExpanded {
			for _, k := range rec.DynamicKeys {
				if inst := st.Nodes[dynamicInstanceCanon(addr, k)]; inst != nil && inst.MergedIntoSharedBranch {
					return true
				}
			}
		}
	}
	return false
}

func resetMergeRecordForCascade(rec *nodeStateRecord) {
	rec.MergeEpoch++
	rec.Status = node.StatusPending
	rec.BaseSHA = ""
	rec.CommitSHA = ""
	rec.LatestTargetSHA = ""
	rec.ExpectedSources = nil
	rec.SourcesKnown = false
	rec.EnqueuedSources = make(map[string]string)
	rec.ErrorMessage = ""
}

func resetDynamicParentForCascade(st *persistedRunState, dynRec *nodeStateRecord, resetWorkerAddrs *[]string) {
	for _, k := range dynRec.DynamicKeys {
		instAddr := dynamicInstanceCanon(dynRec.CanonicalAddress, k)
		if inst := st.Nodes[instAddr]; inst != nil {
			*resetWorkerAddrs = append(*resetWorkerAddrs, instAddr)
			delete(st.Nodes, instAddr)
		}
	}
	dynRec.Status = node.StatusPending
	dynRec.DynamicExpanded = false
	dynRec.DynamicKeys = nil
	dynRec.DynamicElements = nil
	dynRec.ErrorMessage = ""
}

func resetModuleRecordForCascade(rec *nodeStateRecord) {
	rec.Status = node.StatusPending
	rec.BoundInputs = nil
	rec.ModuleOutputs = nil
	rec.ErrorMessage = ""
}

func resetWorkerRecordForCascade(rec *nodeStateRecord, clearBase bool) {
	rec.Status = node.StatusPending
	if clearBase {
		rec.BaseSHA = ""
	}
	rec.CommitSHA = ""
	rec.Result = nil
	rec.Artifacts = nil
	rec.ErrorMessage = ""
}

func lookupFallbackSpec(root *plan.Plan, nodeCanon, fbName string) (int, string, error) {
	var found *plan.NodeSpec
	var search func(p *plan.Plan, prefix string)
	search = func(p *plan.Plan, prefix string) {
		if p == nil || found != nil {
			return
		}
		for _, n := range p.Nodes() {
			canon := canonicalForNode(n, prefix)
			if canon == nodeCanon || (n.Kind == "dynamic_worker" && strings.HasPrefix(nodeCanon, canon+"[")) {
				cp := n
				found = &cp
				return
			}
			if n.Kind == "module" && n.Module != nil && n.Module.Plan != nil {
				search(n.Module.Plan, canon)
			}
		}
	}
	search(root, "")
	if found == nil {
		return 0, "", fmt.Errorf("worker node %q not found in plan", nodeCanon)
	}
	cfgVal := found.Config
	if found.Kind == "dynamic_worker" && cfgVal.Type().IsObjectType() && cfgVal.Type().HasAttribute("content") {
		cfgVal = cfgVal.GetAttr("content")
	}
	if cfgVal == cty.NilVal || !cfgVal.Type().IsObjectType() || !cfgVal.Type().HasAttribute("fallbacks") {
		return 0, "", fmt.Errorf("node %q has no fallback %q", nodeCanon, fbName)
	}
	fbs := cfgVal.GetAttr("fallbacks")
	if fbs.IsNull() || !fbs.IsKnown() || !fbs.CanIterateElements() {
		return 0, "", fmt.Errorf("node %q fallback %q list invalid", nodeCanon, fbName)
	}
	for it := fbs.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if readCtyStringAttr(elem, "name") == fbName {
			maxReworks := 0
			if maxVal := getCtyAttr(elem, "max_reworks"); maxVal != cty.NilVal && !maxVal.IsNull() && maxVal.IsKnown() && maxVal.Type().Equals(cty.Number) {
				i64, _ := maxVal.AsBigFloat().Int64()
				maxReworks = int(i64)
			}
			target := readCtyStringAttr(elem, "target")
			return maxReworks, target, nil
		}
	}
	return 0, "", fmt.Errorf("fallback %q not found on %q", fbName, nodeCanon)
}

func lookupMergeMaxReworks(root *plan.Plan, mergeCanon string) int {
	var found *plan.NodeSpec
	var search func(p *plan.Plan, prefix string)
	search = func(p *plan.Plan, prefix string) {
		if p == nil || found != nil {
			return
		}
		for _, n := range p.Nodes() {
			canon := canonicalForNode(n, prefix)
			if canon == mergeCanon {
				cp := n
				found = &cp
				return
			}
			if n.Kind == "module" && n.Module != nil && n.Module.Plan != nil {
				search(n.Module.Plan, canon)
			}
		}
	}
	search(root, "")
	if found == nil {
		return 2
	}
	if maxVal := getCtyAttr(found.Config, "max_reworks"); maxVal != cty.NilVal && !maxVal.IsNull() && maxVal.IsKnown() && maxVal.Type().Equals(cty.Number) {
		i64, _ := maxVal.AsBigFloat().Int64()
		return int(i64)
	}
	return 2
}

func resolveModuleScopedTarget(modulePath, target string) string {
	trimmed := strings.TrimSpace(target)
	if modulePath == "" || strings.HasPrefix(trimmed, modulePath+".") {
		return trimmed
	}
	return modulePath + "." + trimmed
}

func extractNodeResourceCatalog(
	rootPlan *plan.Plan,
	currentPlan *plan.Plan,
	parentPlans []*plan.Plan,
) (
	map[string]sandboxspec.SandboxConfig,
	map[string]harnessspec.HarnessConfig,
	map[string]provider.Config,
	[]mcppkg.Config,
	[]toolruntime.MCPResourceSpec,
	map[string]plan.ToolSpec,
	map[string]runner.ExternalToolEnvConfig,
	error,
) {
	sandboxes := make(map[string]sandboxspec.SandboxConfig)
	harnesses := make(map[string]harnessspec.HarnessConfig)
	providers := make(map[string]provider.Config)
	var mcpServers []mcppkg.Config
	var mcpResources []toolruntime.MCPResourceSpec
	toolEnvs := make(map[string]runner.ExternalToolEnvConfig)

	toolsMap := make(map[string]plan.ToolSpec)
	if rootPlan != nil {
		maps.Copy(toolsMap, rootPlan.Tools())
	}
	if currentPlan != nil {
		maps.Copy(toolsMap, currentPlan.Tools())
	}

	plansInOrder := make([]*plan.Plan, 0, len(parentPlans)+2)
	if rootPlan != nil {
		plansInOrder = append(plansInOrder, rootPlan)
	}
	for _, pp := range parentPlans {
		if pp != nil && pp != rootPlan {
			plansInOrder = append(plansInOrder, pp)
		}
	}
	if currentPlan != nil && currentPlan != rootPlan {
		plansInOrder = append(plansInOrder, currentPlan)
	}

	for _, p := range plansInOrder {
		for _, n := range p.Nodes() {
			canon := canonicalForNode(n, "")
			switch n.Kind {
			case "sandbox":
				sb, err := decodeSandboxFromPlanConfig(n.Config)
				if err != nil {
					return nil, nil, nil, nil, nil, nil, nil, err
				}
				sandboxes[canon] = sb
				if n.Address != canon {
					sandboxes[n.Address] = sb
				}
				sandboxes[nodeSimpleName(n)] = sb
			case "harness":
				h := decodeHarnessFromPlanConfig(n.Config, nodeSimpleName(n))
				harnesses[canon] = h
				if n.Address != canon {
					harnesses[n.Address] = h
				}
				harnesses[h.Name] = h
			case "model_provider":
				prov := decodeProviderFromPlanConfig(n.Config)
				providers[canon] = prov
				if n.Address != canon {
					providers[n.Address] = prov
				}
				providers[nodeSimpleName(n)] = prov
			case "mcp_server":
				srv, specs := decodeMCPFromPlanConfig(n, canon)
				mcpServers = append(mcpServers, srv)
				mcpResources = append(mcpResources, specs...)
			}
		}
	}
	return sandboxes, harnesses, providers, mcpServers, mcpResources, toolsMap, toolEnvs, nil
}

func extractMergeSandboxConfig(
	sandboxRef cty.Value,
	evalCtx *hcl.EvalContext,
	currentPlan *plan.Plan,
	parentPlans []*plan.Plan,
) (sandboxspec.SandboxConfig, error) {
	if sandboxRef != cty.NilVal && !sandboxRef.IsNull() && sandboxRef.IsWhollyKnown() && sandboxRef.Type().IsObjectType() {
		if addr := readCtyStringAttr(sandboxRef, "address"); addr != "" {
			for _, p := range append(slices.Clone(parentPlans), currentPlan) {
				if p == nil {
					continue
				}
				for _, n := range p.Nodes() {
					if n.Kind == "sandbox" && (canonicalForNode(n, "") == addr || n.Address == addr) {
						return decodeSandboxFromPlanConfig(n.Config)
					}
				}
			}
		}
	}
	if evalCtx != nil && evalCtx.Variables != nil {
		if sbObj, ok := evalCtx.Variables["sandbox"]; ok && sbObj != cty.NilVal && !sbObj.IsNull() && sbObj.IsKnown() && sbObj.Type().IsObjectType() {
			for _, v := range sbObj.AsValueMap() {
				if addr := readCtyStringAttr(v, "address"); addr != "" {
					for _, p := range append(slices.Clone(parentPlans), currentPlan) {
						if p == nil {
							continue
						}
						for _, n := range p.Nodes() {
							if n.Kind == "sandbox" && (canonicalForNode(n, "") == addr || n.Address == addr) {
								return decodeSandboxFromPlanConfig(n.Config)
							}
						}
					}
				}
			}
		}
	}
	return sandboxspec.SandboxConfig{}, nil
}

func decodeSandboxFromPlanConfig(val cty.Value) (sandboxspec.SandboxConfig, error) {
	cfg := sandboxspec.SandboxConfig{
		Runtime: readCtyStringAttr(val, "runtime"),
		Image:   readCtyStringAttr(val, "image"),
		Network: readCtyStringAttr(val, "network"),
	}
	if cfg.Network == "" {
		cfg.Network = "none"
	}
	if mountsVal := getCtyAttr(val, "mounts"); mountsVal != cty.NilVal && !mountsVal.IsNull() && mountsVal.IsKnown() && mountsVal.CanIterateElements() {
		for it := mountsVal.ElementIterator(); it.Next(); {
			_, m := it.Element()
			ro := false
			if roVal := getCtyAttr(m, "read_only"); roVal != cty.NilVal && !roVal.IsNull() && roVal.IsKnown() && roVal.Type().Equals(cty.Bool) {
				ro = roVal.True()
			}
			cfg.Mounts = append(cfg.Mounts, sandboxspec.MountSpec{
				Source:   readCtyStringAttr(m, "source"),
				Target:   readCtyStringAttr(m, "target"),
				ReadOnly: ro,
			})
		}
	}
	if resVal := getCtyAttr(val, "resources"); resVal != cty.NilVal && !resVal.IsNull() && resVal.IsKnown() && resVal.Type().IsObjectType() {
		cfg.Resources = &sandboxspec.ResourcesSpec{
			CPUs:   readCtyStringAttr(resVal, "cpus"),
			Memory: readCtyStringAttr(resVal, "memory"),
		}
	}
	return cfg, nil
}

func decodeHarnessFromPlanConfig(val cty.Value, fallbackName string) harnessspec.HarnessConfig {
	name := readCtyStringAttr(val, "name")
	if name == "" {
		name = fallbackName
	}
	sig := harnessspec.SignatureMode(readCtyStringAttr(val, "signature"))
	if sig == "" {
		sig = harnessspec.SignatureModeAuto
	}
	optsVal := getCtyAttr(val, "options")
	if optsVal == cty.NilVal {
		optsVal = cty.NullVal(cty.EmptyObject)
	}
	return harnessspec.HarnessConfig{
		Name:        name,
		Source:      readCtyStringAttr(val, "source"),
		SourceHost:  readCtyStringAttr(val, "source_host"),
		SourceOwner: readCtyStringAttr(val, "source_owner"),
		SourceRepo:  readCtyStringAttr(val, "source_repo"),
		Version:     readCtyStringAttr(val, "version"),
		Signature:   sig,
		SigningKey:  readCtyStringAttr(val, "signing_key"),
		EnvRefs:     readCtyStringMapAttr(val, "env_refs"),
		Options:     optsVal,
	}
}

func decodeProviderFromPlanConfig(val cty.Value) provider.Config {
	cfg := provider.Config{
		Type:           provider.Type(readCtyStringAttr(val, "type")),
		Endpoint:       readCtyStringAttr(val, "endpoint"),
		Headers:        getCtyAttr(val, "headers"),
		APIKey:         readOptionalCtyStringPtr(val, "api_key"),
		APIKeyRef:      readOptionalCtyStringPtr(val, "api_key_ref"),
		BearerToken:    readOptionalCtyStringPtr(val, "bearer_token"),
		BearerTokenRef: readOptionalCtyStringPtr(val, "bearer_token_ref"),
	}
	if w := readOptionalCtyStringPtr(val, "wire_api"); w != nil {
		wt := provider.WireAPI(*w)
		cfg.WireAPI = &wt
	}
	if tr := readOptionalCtyStringPtr(val, "transport"); tr != nil {
		tt := provider.Transport(*tr)
		cfg.Transport = &tt
	}
	if rVal := getCtyAttr(val, "retry"); rVal != cty.NilVal && !rVal.IsNull() && rVal.IsKnown() && rVal.Type().IsObjectType() {
		if n := readOptionalCtyIntPtr(rVal, "lifecycle_retries"); n != nil {
			cfg.Retry.LifecycleRetries = n
		}
		if n := readOptionalCtyIntPtr(rVal, "model_call_retries"); n != nil {
			cfg.Retry.ModelCallRetries = n
		}
		if n := readOptionalCtyIntPtr(rVal, "interval_seconds"); n != nil {
			d := time.Duration(*n) * time.Second
			cfg.Retry.Interval = &d
		}
		if n := readOptionalCtyIntPtr(rVal, "max_interval_seconds"); n != nil {
			d := time.Duration(*n) * time.Second
			cfg.Retry.MaxInterval = &d
		}
		if regex := readCtyStringSlice(rVal, "error_message_regex"); regex != nil {
			cfg.Retry.ErrorMessageRegex = regex
		}
	}
	return cfg
}

func decodeMCPFromPlanConfig(n plan.NodeSpec, canon string) (mcppkg.Config, []toolruntime.MCPResourceSpec) {
	cfg := n.Config
	name := readCtyStringAttr(cfg, "name")
	if name == "" {
		name = nodeSimpleName(n)
	}
	runtimeName := readCtyStringAttr(cfg, "runtime_name")
	if runtimeName == "" {
		runtimeName = canon
	}
	var timeout time.Duration
	if ns := readOptionalCtyIntPtr(cfg, "timeout_nanoseconds"); ns != nil && *ns > 0 {
		timeout = time.Duration(*ns)
	}
	if timeout == 0 {
		timeout = mcppkg.DefaultTimeout
	}
	srv := mcppkg.Config{
		Name:        name,
		RuntimeName: runtimeName,
		Transport:   mcppkg.Transport(readCtyStringAttr(cfg, "transport")),
		Tools:       readCtyStringSlice(cfg, "tools"),
		Resources:   readCtyStringSlice(cfg, "resources"),
		Timeout:     timeout,
	}
	if hVal := getCtyAttr(cfg, "http"); hVal != cty.NilVal && !hVal.IsNull() && hVal.IsKnown() && hVal.Type().IsObjectType() {
		srv.HTTP = &mcppkg.HTTPConfig{
			URL:            readCtyStringAttr(hVal, "url"),
			Headers:        readCtyStringMapAttr(hVal, "headers"),
			BearerToken:    readOptionalCtyStringPtr(hVal, "bearer_token"),
			BearerTokenRef: readOptionalCtyStringPtr(hVal, "bearer_token_ref"),
		}
	}
	if sVal := getCtyAttr(cfg, "stdio"); sVal != cty.NilVal && !sVal.IsNull() && sVal.IsKnown() && sVal.Type().IsObjectType() {
		srv.Stdio = &mcppkg.StdioConfig{
			Command:          readCtyStringAttr(sVal, "command"),
			Args:             readCtyStringSlice(sVal, "args"),
			Env:              readCtyStringMapAttr(sVal, "env"),
			EnvRefs:          readCtyStringMapAttr(sVal, "env_refs"),
			WorkingDirectory: readCtyStringAttr(sVal, "working_directory"),
		}
	}
	specs := make([]toolruntime.MCPResourceSpec, 0, len(srv.Resources))
	for _, uri := range srv.Resources {
		specs = append(specs, toolruntime.MCPResourceSpec{
			ID:         mcpResourceIDFor(runtimeName, name, uri),
			ServerName: name,
			URI:        uri,
		})
	}
	return srv, specs
}

func mcpResourceIDFor(canonical, name, resource string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-resource-id/v1\x00" + canonical + "\x00" + resource))
	return "mcp_resource_" + mcpIDPart(name) + "__" + mcpIDPart(resource) + "_" + mcpUUIDFromDigest(digest)
}

func mcpUUIDFromDigest(digest [sha256.Size]byte) string {
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func mcpIDPart(value string) string {
	var result strings.Builder
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '_', ch == '-':
			result.WriteRune(ch)
		default:
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "tool"
	}
	return result.String()
}

func resolveWorkerImports(st *persistedRunState, mat workerspec.Config) ([]artifact.ImportRequest, error) {
	if len(mat.Imports) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(mat.Imports))
	for name := range mat.Imports {
		names = append(names, name)
	}
	sort.Strings(names)

	reqs := make([]artifact.ImportRequest, 0, len(names))
	for _, name := range names {
		imp := mat.Imports[name]
		var srcs []artifact.ImportSource
		if imp.Sources != cty.NilVal && !imp.Sources.IsNull() && imp.Sources.IsKnown() && imp.Sources.CanIterateElements() {
			for it := imp.Sources.ElementIterator(); it.Next(); {
				_, elem := it.Element()
				id := readCtyStringAttr(elem, "id")
				if id == "" {
					continue
				}
				pub, ok := st.PublishedArtifacts[id]
				if !ok {
					return nil, fmt.Errorf("import_artifact %q references unpublished artifact id %q", name, id)
				}
				srcs = append(srcs, artifact.ImportSource{Artifact: pub})
			}
		}
		reqs = append(reqs, artifact.ImportRequest{
			Name:    name,
			RelPath: imp.Path,
			Sources: srcs,
		})
	}
	return reqs, nil
}

func buildSourceAuthMap(st *persistedRunState) map[string]git.AuthConfig {
	out := make(map[string]git.AuthConfig, len(st.Sources))
	for _, s := range st.Sources {
		norm := git.NormalizeRepoURL(s.URL)
		out[norm] = git.AuthConfig{TokenRef: s.TokenRef, SSHKeyRef: s.SSHKeyRef}
	}
	return out
}

func authRefsForRepo(byRepo map[string]git.AuthConfig, repoURL string) (git.AuthConfig, []string) {
	auth := byRepo[git.NormalizeRepoURL(repoURL)]
	var keys []string
	if auth.TokenRef != nil {
		keys = append(keys, *auth.TokenRef)
	}
	if auth.SSHKeyRef != nil {
		keys = append(keys, *auth.SSHKeyRef)
	}
	return auth, keys
}

func findSourceCredentialRefs(st *persistedRunState, repoURL string) (*string, *string) {
	norm := git.NormalizeRepoURL(repoURL)
	for _, s := range st.Sources {
		if norm == "" || git.NormalizeRepoURL(s.URL) == norm {
			return s.TokenRef, s.SSHKeyRef
		}
	}
	return nil, nil
}

func inferSingleSourceURL(st *persistedRunState, currentPlan *plan.Plan) string {
	if currentPlan != nil {
		var found string
		count := 0
		for _, n := range currentPlan.Nodes() {
			if n.Kind == "source" {
				if u := readCtyStringAttr(n.Config, "url"); u != "" {
					found = u
					count++
				}
			}
		}
		if count == 1 {
			return found
		}
	}
	urls := make(map[string]struct{}, len(st.Sources))
	var single string
	for _, s := range st.Sources {
		if s.URL != "" {
			urls[s.URL] = struct{}{}
			single = s.URL
		}
	}
	if len(urls) == 1 {
		return single
	}
	return ""
}

func dynamicParallelismLimit(cfg cty.Value) int {
	if sr := getCtyAttr(cfg, "serial"); sr != cty.NilVal && !sr.IsNull() && sr.IsKnown() && sr.Type().Equals(cty.Bool) && sr.True() {
		return 1
	}
	if p := readOptionalCtyIntPtr(cfg, "parallelism"); p != nil && *p > 0 {
		return *p
	}
	return 0
}

func bundleDirForPlan(loaded *plan.LoadedArchive, p *plan.Plan) string {
	if loaded == nil {
		return ""
	}
	if p != nil && strings.TrimSpace(p.BundlePath()) != "" {
		candidate := filepath.Join(loaded.ExtractDir, filepath.FromSlash(p.BundlePath()))
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	if loaded.ConfigDir != "" {
		return loaded.ConfigDir
	}
	return loaded.ExtractDir
}

func canonicalForNode(n plan.NodeSpec, modulePrefix string) string {
	if strings.TrimSpace(n.CanonicalAddress) != "" {
		return n.CanonicalAddress
	}
	if modulePrefix != "" {
		return modulePrefix + "." + n.Address
	}
	return n.Address
}

func dynamicInstanceCanon(dynamicCanon, key string) string {
	return fmt.Sprintf("%s[%s]", dynamicCanon, key)
}

func nodeSimpleName(n plan.NodeSpec) string {
	addr := n.Address
	dot := strings.IndexByte(addr, '.')
	if dot >= 0 && dot+1 < len(addr) {
		rest := addr[dot+1:]
		if idx := strings.IndexByte(rest, '['); idx > 0 {
			return rest[:idx]
		}
		return rest
	}
	return addr
}

func sanitizePathSegment(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

func getCtyAttr(val cty.Value, attr string) cty.Value {
	if val == cty.NilVal {
		return cty.NilVal
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.Type().IsObjectType() || !unmarked.Type().HasAttribute(attr) {
		return cty.NilVal
	}
	return val.GetAttr(attr)
}

func readCtyStringAttr(val cty.Value, attr string) string {
	v := getCtyAttr(val, attr)
	if v == cty.NilVal {
		return ""
	}
	unmarked, _ := v.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.Type().Equals(cty.String) {
		return ""
	}
	return strings.TrimSpace(unmarked.AsString())
}

func readOptionalCtyStringPtr(val cty.Value, attr string) *string {
	s := readCtyStringAttr(val, attr)
	if s == "" {
		return nil
	}
	return &s
}

func readOptionalCtyIntPtr(val cty.Value, attr string) *int {
	v := getCtyAttr(val, attr)
	if v == cty.NilVal {
		return nil
	}
	unmarked, _ := v.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.Type().Equals(cty.Number) {
		return nil
	}
	i64, _ := unmarked.AsBigFloat().Int64()
	n := int(i64)
	return &n
}

func readCtyStringSlice(val cty.Value, attr string) []string {
	v := getCtyAttr(val, attr)
	if v == cty.NilVal {
		return nil
	}
	unmarked, _ := v.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.CanIterateElements() {
		return nil
	}
	out := make([]string, 0, unmarked.LengthInt())
	for it := unmarked.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if elem.IsNull() || !elem.IsKnown() || !elem.Type().Equals(cty.String) {
			continue
		}
		out = append(out, elem.AsString())
	}
	return out
}

func readCtyStringMapAttr(val cty.Value, attr string) map[string]string {
	v := getCtyAttr(val, attr)
	if v == cty.NilVal {
		return nil
	}
	unmarked, _ := v.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsKnown() || (!unmarked.Type().IsMapType() && !unmarked.Type().IsObjectType()) {
		return nil
	}
	out := make(map[string]string, unmarked.LengthInt())
	for it := unmarked.ElementIterator(); it.Next(); {
		k, elem := it.Element()
		if elem.IsNull() || !elem.IsKnown() || !elem.Type().Equals(cty.String) {
			continue
		}
		out[k.AsString()] = elem.AsString()
	}
	return out
}

func requireCtyString(val cty.Value, scope string) (string, error) {
	if val == cty.NilVal {
		return "", fmt.Errorf("%s is required", scope)
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", fmt.Errorf("%s must be a wholly known string", scope)
	}
	s := strings.TrimSpace(unmarked.AsString())
	if s == "" {
		return "", fmt.Errorf("%s must not be empty", scope)
	}
	return s, nil
}

func optionalCtyString(val cty.Value, scope string) (string, error) {
	if val == cty.NilVal {
		return "", nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return "", nil
	}
	if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
		return "", fmt.Errorf("%s must be a wholly known string", scope)
	}
	return unmarked.AsString(), nil
}

func isMergeBranchRegisteredInLedger(snap git.LedgerSnapshot, repoURL, branch, ownerNode string, epoch uint64, baseSHA string) bool {
	normRepo := git.NormalizeRepoURL(repoURL)
	cleanBranch, err := git.NormalizeBranchName(branch)
	if err != nil {
		return false
	}
	for _, entry := range snap.Entries {
		if entry.Registered &&
			git.NormalizeRepoURL(entry.RepoURL) == normRepo &&
			entry.Branch == cleanBranch &&
			entry.Kind == git.KindMergeBranch &&
			entry.OwnerNode == ownerNode &&
			entry.Epoch == epoch &&
			entry.BaseSHA == baseSHA {
			return true
		}
	}
	return false
}

func restoreLedger(snap git.LedgerSnapshot) (*git.Ledger, error) {
	l := git.NewLedger()
	if err := l.Restore(snap); err != nil {
		return nil, err
	}
	return l, nil
}

var _ = corespec.IsSensitive
var _ = strconv.Itoa
