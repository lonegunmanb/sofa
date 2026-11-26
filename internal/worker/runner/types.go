package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/session"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

// DefaultMaxGatekeeperReworks bounds deterministic gatekeeper rework loops when not overridden.
const DefaultMaxGatekeeperReworks = 3

// DefaultMaxArtifactReworks bounds post-freeze artifact validation rework loops.
const DefaultMaxArtifactReworks = 3

// OutcomeStatus classifies the terminal or paused outcome of a WorkerRunner node execution.
type OutcomeStatus string

const (
	StatusSucceeded         OutcomeStatus = "succeeded"
	StatusWaitingHuman      OutcomeStatus = "waiting_human"
	StatusFallbackTriggered OutcomeStatus = "fallback_triggered"
	StatusFailed            OutcomeStatus = "failed"
)

// ApprovalTarget identifies the exact commit SHA and artifact IDs awaiting human_approval = "required".
type ApprovalTarget struct {
	CommitSHA   string                       `json:"commit_sha"`
	ArtifactIDs []string                     `json:"artifact_ids,omitempty"`
	Artifacts   []artifact.PublishedArtifact `json:"artifacts,omitempty"`
	ResultJSON  json.RawMessage              `json:"result_json,omitempty"`
}

// ApprovalRecord captures a recorded human approval or upstream return decision.
type ApprovalRecord struct {
	Approved         bool     `json:"approved"`
	ReturnedUpstream bool     `json:"returned_upstream"`
	ReturnFallback   string   `json:"return_fallback,omitempty"`
	CommitSHA        string   `json:"commit_sha"`
	ArtifactIDs      []string `json:"artifact_ids,omitempty"`
}

// PendingPublication persists verified publication state across human_approval pauses and epoch recoveries.
type PendingPublication struct {
	CommitSHA   string                       `json:"commit_sha"`
	BaseSHA     string                       `json:"base_sha,omitempty"`
	ArtifactIDs []string                     `json:"artifact_ids,omitempty"`
	Artifacts   []artifact.PublishedArtifact `json:"artifacts,omitempty"`
	ResultJSON  json.RawMessage              `json:"result_json,omitempty"`
}

// NodeOutcome captures the full structured result of executing a single worker node.
type NodeOutcome struct {
	Status            OutcomeStatus                `json:"status"`
	WorkBranch        string                       `json:"work_branch,omitempty"`
	CommitSHA         string                       `json:"commit_sha,omitempty"`
	ResultJSON        json.RawMessage              `json:"result_json,omitempty"`
	Artifacts         []artifact.PublishedArtifact `json:"artifacts,omitempty"`
	FallbackName      string                       `json:"fallback_name,omitempty"`
	FallbackTarget    string                       `json:"fallback_target,omitempty"`
	FallbackReason    string                       `json:"fallback_reason,omitempty"`
	FallbackArtifacts []artifact.PublishedArtifact `json:"fallback_artifacts,omitempty"`
	DecisionRequest   *session.DecisionRequest     `json:"decision_request,omitempty"`
	PauseReason       string                       `json:"pause_reason,omitempty"`
	PendingApproval   *ApprovalTarget              `json:"pending_approval,omitempty"`
	FailureReason     string                       `json:"failure_reason,omitempty"`
}

// NodeControlPlane abstracts epoch-fenced control-plane state for a single worker execution.
type NodeControlPlane interface {
	checkpoint.NodeStateStore
	DrainInterjections(ctx context.Context) ([]session.HumanInterjection, error)
	HasPendingInterjections(ctx context.Context) (bool, error)
	SaveQCLedgerSnapshot(ctx context.Context, epoch uint64, snapshot []byte) error
	LoadQCLedgerSnapshot(ctx context.Context) ([]byte, error)
	SavePendingPublication(ctx context.Context, epoch uint64, pending *PendingPublication) error
	LoadPendingPublication(ctx context.Context) (*PendingPublication, error)
	GetApproval(ctx context.Context) (*ApprovalRecord, error)
	RegisterWorkBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error
}

// SessionLaunchRequest describes the container and plugin parameters for launching a Work or QC session.
type SessionLaunchRequest struct {
	Phase        string
	Sandbox      sandboxspec.SandboxConfig
	Harness      harnessspec.HarnessConfig
	MountLayout  sandboxruntime.SessionMountLayout
	GoTools      []plan.ToolSpec
	Platform     harnessinstaller.Platform
	ResolveOpts  harnessinstaller.ResolveOptions
	StartTimeout time.Duration
}

// SessionHandle represents an active launched harness plugin session container.
type SessionHandle interface {
	Opener() harnessruntime.Opener
	ContainerID() string
	Close() error
}

// SessionLauncher abstracts launching an isolated Work or QC harness session container.
type SessionLauncher interface {
	Launch(ctx context.Context, req SessionLaunchRequest) (SessionHandle, error)
}

// ContainerSessionLauncher is the default Stage 2 SessionLauncher using installer.Prepare,
// toolruntime.Compiler, toolruntime.StageToolsDir, and harnessruntime.NewContainerOpener.
type ContainerSessionLauncher struct {
	Installer    *harnessinstaller.Installer
	Compiler     *toolruntime.Compiler
	ToolCacheDir string
	StartTimeout time.Duration
}

type containerSessionHandle struct {
	opener *harnessruntime.GRPCClientOpener
}

func (h *containerSessionHandle) Opener() harnessruntime.Opener { return h.opener }
func (h *containerSessionHandle) ContainerID() string {
	if h.opener == nil {
		return ""
	}
	return h.opener.ContainerID()
}
func (h *containerSessionHandle) Close() error {
	if h.opener == nil {
		return nil
	}
	return h.opener.Close()
}

// Launch prepares the harness plugin, compiles any go_tools, and starts the container opener.
func (l *ContainerSessionLauncher) Launch(ctx context.Context, req SessionLaunchRequest) (SessionHandle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	inst := l.Installer
	if inst == nil {
		inst = harnessinstaller.NewInstaller()
	}
	prepared, err := inst.Prepare(ctx, req.Harness, req.Platform, req.ResolveOpts)
	if err != nil {
		return nil, fmt.Errorf("prepare harness %q: %w", req.Harness.Name, err)
	}

	layout := req.MountLayout
	layout.Sandbox = req.Sandbox
	layout.HarnessDir = prepared.HostDir
	layout.Entrypoint = prepared.ContainerPath
	layout.ForAgentSession = true

	if len(req.GoTools) > 0 {
		comp := l.Compiler
		if comp == nil {
			comp = toolruntime.NewCompiler(toolruntime.CompilerOptions{
				CacheDir:   l.ToolCacheDir,
				TargetOS:   req.Platform.OS,
				TargetArch: req.Platform.Arch,
			})
		}
		compiled := make([]toolruntime.CompiledTool, 0, len(req.GoTools))
		for _, toolSpec := range req.GoTools {
			ct, err := comp.Compile(ctx, toolSpec)
			if err != nil {
				return nil, fmt.Errorf("compile go_tool %q: %w", toolSpec.ID, err)
			}
			compiled = append(compiled, ct)
		}
		if err := toolruntime.StageToolsDir(ctx, layout.ToolsDir, compiled); err != nil {
			return nil, fmt.Errorf("stage tools dir: %w", err)
		}
	} else {
		layout.ToolsDir = ""
	}

	timeout := req.StartTimeout
	if timeout <= 0 {
		timeout = l.StartTimeout
	}
	opener, err := harnessruntime.NewContainerOpener(layout, harnessruntime.OpenerOptions{
		RuntimeBin:   req.Sandbox.Runtime,
		StartTimeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("open container harness %q: %w", req.Harness.Name, err)
	}
	return &containerSessionHandle{opener: opener}, nil
}

// ExternalToolEnvConfig holds optional runtime env/env_refs/timeout overrides for an external_tool.
type ExternalToolEnvConfig struct {
	Env     map[string]string
	EnvRefs map[string]string
	Timeout time.Duration
}

// NodeExecutionRequest provides all planned inputs and resolved dependencies for running one worker node.
type NodeExecutionRequest struct {
	RunID       string
	NodeAddr    string
	Epoch       uint64
	RepoURL     string
	BasedSource string
	BaseSHA     string
	Auth        git.AuthConfig

	Worker       workerspec.Config
	SystemPrompt string
	Prompt       string
	QCPrompt     string

	Sandboxes      map[string]sandboxspec.SandboxConfig
	Harnesses      map[string]harnessspec.HarnessConfig
	ModelProviders map[string]provider.Config
	MCPServers     []mcppkg.Config
	MCPResources   []toolruntime.MCPResourceSpec
	Tools          map[string]plan.ToolSpec
	ToolEnvs       map[string]ExternalToolEnvConfig
	Imports        []artifact.ImportRequest

	BundleDir             string
	TempRoot              string
	HostOS                string
	TargetPlatform        *harnessinstaller.Platform
	PluginResolveOpts     harnessinstaller.ResolveOptions
	LookupEnv             func(string) (string, bool)
	MaxGatekeeperReworks  int
	MaxArtifactReworks    int
	StallTimeout          time.Duration
	WatchdogCheckInterval time.Duration
	Sleep                 func(ctx context.Context, d time.Duration) error
	Jitter                func(d time.Duration) time.Duration
	OnEvent               func(harnessruntime.Event)
}

// WorkerRunnerDeps injects external boundaries into WorkerRunner.
type WorkerRunnerDeps struct {
	ControlPlane    NodeControlPlane
	GitAdapter      git.Adapter
	BlobStore       blob.Store
	ArtifactSvc     *artifact.Service
	SandboxEngine   sandboxruntime.Engine
	SessionLauncher SessionLauncher
	ToolExecFactory func(containerID string) toolruntime.ContainerExecFunc
}

// MergeStatus classifies the outcome of ExecuteMergeCandidate.
type MergeStatus string

const (
	MergeStatusSucceeded    MergeStatus = "succeeded"
	MergeStatusReworkSource MergeStatus = "rework_source"
)

// MergeCandidateRequest configures a single deterministic candidate rebase + gatekeeper merge check.
type MergeCandidateRequest struct {
	RunID           string
	MergeNodeAddr   string
	Epoch           uint64
	RepoURL         string
	BasedSource     string
	BaseSHA         string
	TargetBranch    *string
	CandidateBranch string
	Auth            git.AuthConfig
	Sandbox         sandboxspec.SandboxConfig
	Gatekeepers     []workerspec.GatekeeperConfig
	GitAdapter      git.Adapter
	Ledger          *git.Ledger
	SandboxEngine   sandboxruntime.Engine
	BundleDir       string
	ImportsDir      string
	HostOS          string
	TempRoot        string
}

// MergeCandidateResult captures the deterministic result of ExecuteMergeCandidate.
type MergeCandidateResult struct {
	Status            MergeStatus
	TargetBranch      string
	LatestTargetSHA   string
	MergedCommitSHA   string
	ConflictFiles     []string
	GatekeeperFailure *sandboxruntime.GatekeeperFailure
	FailureReport     string
}

// LocalNodeControlPlaneOptions configures a Stage 2 LocalNodeControlPlane.
type LocalNodeControlPlaneOptions struct {
	StateStore          *checkpoint.LocalNodeStateStore
	InitialEpoch        uint64
	CheckpointStatePath string
	Ledger              *git.Ledger
	OwnerNode           string
	StatePath           string
}

type registeredWorkBranchEntry struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
	Epoch   uint64 `json:"epoch"`
}

type persistedControlPlaneState struct {
	Interjections          []session.HumanInterjection          `json:"interjections,omitempty"`
	QCLedgerSnapshot       []byte                               `json:"qc_ledger_snapshot,omitempty"`
	PendingPublication     *PendingPublication                  `json:"pending_publication,omitempty"`
	Approval               *ApprovalRecord                      `json:"approval,omitempty"`
	RegisteredWorkBranches map[string]registeredWorkBranchEntry `json:"registered_work_branches,omitempty"`
}

// LocalNodeControlPlane is a thread-safe in-memory and optional JSON-backed Stage 2 NodeControlPlane
// wrapping checkpoint.LocalNodeStateStore with strict epoch fencing.
type LocalNodeControlPlane struct {
	*checkpoint.LocalNodeStateStore

	mu                     sync.RWMutex
	ledger                 *git.Ledger
	ownerNode              string
	statePath              string
	interjections          []session.HumanInterjection
	qcLedgerSnapshot       []byte
	pendingPublication     *PendingPublication
	approval               *ApprovalRecord
	registeredWorkBranches map[string]registeredWorkBranchEntry
}

var _ NodeControlPlane = (*LocalNodeControlPlane)(nil)

// NewLocalNodeControlPlane constructs a LocalNodeControlPlane wrapping LocalNodeStateStore.
func NewLocalNodeControlPlane(opts LocalNodeControlPlaneOptions) (*LocalNodeControlPlane, error) {
	store := opts.StateStore
	if store == nil {
		var err error
		store, err = checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
			InitialEpoch: opts.InitialEpoch,
			Ledger:       opts.Ledger,
			OwnerNode:    opts.OwnerNode,
			StatePath:    opts.CheckpointStatePath,
		})
		if err != nil {
			return nil, err
		}
	}
	if opts.Ledger != nil && strings.TrimSpace(opts.OwnerNode) == "" {
		return nil, errors.New("owner_node is required when ledger is configured")
	}

	cp := &LocalNodeControlPlane{
		LocalNodeStateStore:    store,
		ledger:                 opts.Ledger,
		ownerNode:              strings.TrimSpace(opts.OwnerNode),
		statePath:              strings.TrimSpace(opts.StatePath),
		registeredWorkBranches: make(map[string]registeredWorkBranchEntry),
	}

	if cp.statePath != "" {
		absPath, err := filepath.Abs(cp.statePath)
		if err != nil {
			return nil, fmt.Errorf("resolve control-plane state path %q: %w", cp.statePath, err)
		}
		cp.statePath = filepath.Clean(absPath)
		if err := os.MkdirAll(filepath.Dir(cp.statePath), 0o700); err != nil {
			return nil, fmt.Errorf("create control-plane state directory: %w", err)
		}
		data, err := os.ReadFile(cp.statePath)
		if err == nil {
			var loaded persistedControlPlaneState
			if uerr := json.Unmarshal(data, &loaded); uerr != nil {
				return nil, fmt.Errorf("decode control-plane state %q: %w", cp.statePath, uerr)
			}
			cp.interjections = append([]session.HumanInterjection(nil), loaded.Interjections...)
			cp.qcLedgerSnapshot = append([]byte(nil), loaded.QCLedgerSnapshot...)
			cp.pendingPublication = clonePendingPublication(loaded.PendingPublication)
			cp.approval = cloneApprovalRecord(loaded.Approval)
			if loaded.RegisteredWorkBranches != nil {
				cp.registeredWorkBranches = loaded.RegisteredWorkBranches
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read control-plane state %q: %w", cp.statePath, err)
		}
	}

	return cp, nil
}

// EnqueueInterjection queues a human interjection for the active Work session.
func (c *LocalNodeControlPlane) EnqueueInterjection(item session.HumanInterjection) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := append(append([]session.HumanInterjection(nil), c.interjections...), item)
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          next,
		QCLedgerSnapshot:       c.qcLedgerSnapshot,
		PendingPublication:     c.pendingPublication,
		Approval:               c.approval,
		RegisteredWorkBranches: c.registeredWorkBranches,
	}); err != nil {
		return err
	}
	c.interjections = next
	return nil
}

// SetApproval stores an operator approval or upstream return decision.
func (c *LocalNodeControlPlane) SetApproval(rec ApprovalRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := cloneApprovalRecord(&rec)
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          c.interjections,
		QCLedgerSnapshot:       c.qcLedgerSnapshot,
		PendingPublication:     c.pendingPublication,
		Approval:               cp,
		RegisteredWorkBranches: c.registeredWorkBranches,
	}); err != nil {
		return err
	}
	c.approval = cp
	return nil
}

// ClearApproval clears any stored operator approval decision.
func (c *LocalNodeControlPlane) ClearApproval() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          c.interjections,
		QCLedgerSnapshot:       c.qcLedgerSnapshot,
		PendingPublication:     c.pendingPublication,
		Approval:               nil,
		RegisteredWorkBranches: c.registeredWorkBranches,
	}); err != nil {
		return err
	}
	c.approval = nil
	return nil
}

// DrainInterjections atomically drains and returns queued human interjections.
func (c *LocalNodeControlPlane) DrainInterjections(ctx context.Context) ([]session.HumanInterjection, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.interjections) == 0 {
		return nil, nil
	}
	drained := append([]session.HumanInterjection(nil), c.interjections...)
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          nil,
		QCLedgerSnapshot:       c.qcLedgerSnapshot,
		PendingPublication:     c.pendingPublication,
		Approval:               c.approval,
		RegisteredWorkBranches: c.registeredWorkBranches,
	}); err != nil {
		return nil, err
	}
	c.interjections = nil
	return drained, nil
}

// HasPendingInterjections reports whether one or more operator interjections are queued.
func (c *LocalNodeControlPlane) HasPendingInterjections(ctx context.Context) (bool, error) {
	if ctx != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.interjections) > 0, nil
}

// SaveQCLedgerSnapshot persists the cross-round QC ledger snapshot under strict epoch fencing.
func (c *LocalNodeControlPlane) SaveQCLedgerSnapshot(ctx context.Context, epoch uint64, snapshot []byte) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.CurrentEpoch() {
		return checkpoint.ErrStaleEpoch
	}
	copied := append([]byte(nil), snapshot...)
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          c.interjections,
		QCLedgerSnapshot:       copied,
		PendingPublication:     c.pendingPublication,
		Approval:               c.approval,
		RegisteredWorkBranches: c.registeredWorkBranches,
	}); err != nil {
		return err
	}
	c.qcLedgerSnapshot = copied
	return nil
}

// LoadQCLedgerSnapshot returns a defensive copy of the saved QC ledger snapshot, if any.
func (c *LocalNodeControlPlane) LoadQCLedgerSnapshot(ctx context.Context) ([]byte, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.qcLedgerSnapshot) == 0 {
		return nil, nil
	}
	return append([]byte(nil), c.qcLedgerSnapshot...), nil
}

// SavePendingPublication persists version-bound approval target metadata under strict epoch fencing.
func (c *LocalNodeControlPlane) SavePendingPublication(ctx context.Context, epoch uint64, pending *PendingPublication) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.CurrentEpoch() {
		return checkpoint.ErrStaleEpoch
	}
	copied := clonePendingPublication(pending)
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          c.interjections,
		QCLedgerSnapshot:       c.qcLedgerSnapshot,
		PendingPublication:     copied,
		Approval:               c.approval,
		RegisteredWorkBranches: c.registeredWorkBranches,
	}); err != nil {
		return err
	}
	c.pendingPublication = copied
	return nil
}

// LoadPendingPublication returns a defensive copy of any persisted PendingPublication.
func (c *LocalNodeControlPlane) LoadPendingPublication(ctx context.Context) (*PendingPublication, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return clonePendingPublication(c.pendingPublication), nil
}

// GetApproval returns a defensive copy of the current ApprovalRecord, if any.
func (c *LocalNodeControlPlane) GetApproval(ctx context.Context) (*ApprovalRecord, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneApprovalRecord(c.approval), nil
}

// RegisterWorkBranch enforces epoch fencing and registers the node's work_branch in git.Ledger.
func (c *LocalNodeControlPlane) RegisterWorkBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if strings.TrimSpace(repoURL) == "" {
		return errors.New("repository url is required")
	}
	cleanBranch, err := git.NormalizeBranchName(branch)
	if err != nil {
		return err
	}
	if !git.IsValidCommitSHA(baseSHA) {
		return fmt.Errorf("%w: %q", git.ErrInvalidBaseSHA, baseSHA)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if epoch != c.CurrentEpoch() {
		return checkpoint.ErrStaleEpoch
	}
	key := git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch
	if existing, ok := c.registeredWorkBranches[key]; ok {
		if epoch < existing.Epoch {
			return checkpoint.ErrStaleEpoch
		}
		if existing.Epoch == epoch && existing.BaseSHA != baseSHA {
			return fmt.Errorf("work branch %q already registered at epoch %d with base_sha %s (got %s)",
				cleanBranch, epoch, existing.BaseSHA, baseSHA)
		}
	}
	if c.ledger != nil {
		if err := c.ledger.Register(repoURL, cleanBranch, baseSHA, git.KindWorkBranch, c.ownerNode, epoch); err != nil {
			return err
		}
	}

	nextBranches := make(map[string]registeredWorkBranchEntry, len(c.registeredWorkBranches)+1)
	for k, v := range c.registeredWorkBranches {
		nextBranches[k] = v
	}
	nextBranches[key] = registeredWorkBranchEntry{
		RepoURL: strings.TrimSpace(repoURL),
		Branch:  cleanBranch,
		BaseSHA: baseSHA,
		Epoch:   epoch,
	}
	if err := c.persistLocked(persistedControlPlaneState{
		Interjections:          c.interjections,
		QCLedgerSnapshot:       c.qcLedgerSnapshot,
		PendingPublication:     c.pendingPublication,
		Approval:               c.approval,
		RegisteredWorkBranches: nextBranches,
	}); err != nil {
		return err
	}
	c.registeredWorkBranches = nextBranches
	return nil
}

func (c *LocalNodeControlPlane) persistLocked(state persistedControlPlaneState) error {
	if c.statePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode control-plane state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(c.statePath)
	tmpFile, err := os.CreateTemp(dir, ".cp-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp control-plane state file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	_, writeErr := tmpFile.Write(data)
	closeErr := tmpFile.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write temp control-plane state file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restrict temp control-plane state permissions: %w", err)
	}
	if err := os.Rename(tmpPath, c.statePath); err != nil {
		return fmt.Errorf("commit control-plane state %q: %w", c.statePath, err)
	}
	cleanup = false
	return nil
}

func clonePendingPublication(p *PendingPublication) *PendingPublication {
	if p == nil {
		return nil
	}
	cp := *p
	cp.ArtifactIDs = append([]string(nil), p.ArtifactIDs...)
	cp.Artifacts = append([]artifact.PublishedArtifact(nil), p.Artifacts...)
	cp.ResultJSON = append([]byte(nil), p.ResultJSON...)
	return &cp
}

func cloneApprovalRecord(a *ApprovalRecord) *ApprovalRecord {
	if a == nil {
		return nil
	}
	cp := *a
	cp.ArtifactIDs = append([]string(nil), a.ArtifactIDs...)
	return &cp
}

func sanitizePathSegment(raw, fallback string) string {
	var b strings.Builder
	prevDash := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return fallback
	}
	return out
}

func sanitizeNodeDirSegment(nodeAddr string) string {
	sum := sha256.Sum256([]byte(nodeAddr))
	shortHash := hex.EncodeToString(sum[:4])
	prefix := sanitizePathSegment(nodeAddr, "node")
	if len(prefix) > 48 {
		prefix = strings.TrimRight(prefix[:48], "-_")
		if prefix == "" {
			prefix = "node"
		}
	}
	return prefix + "_" + shortHash
}
