package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/harness"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"golang.org/x/sync/singleflight"
)

const (
	// PubSubComponentName is the Dapr PubSub component used for real-time worker event streaming.
	PubSubComponentName = daprhost.PubSubComponentName
	// EventsTopic is the Dapr PubSub topic carrying structured worker node log events.
	EventsTopic = "sofa-events"
	// DefaultMaxLogBufferBytes bounds the per-task local JSONL spool file before capping writes.
	DefaultMaxLogBufferBytes int64 = 16 * 1024 * 1024
	bundleMarkerFile               = ".bundle-ready.json"
	cleanupAbortGraceTimeout       = 1500 * time.Millisecond
)

// RegistryClient abstracts ProcessRegistryActor calls made by WorkerDaemon.
type RegistryClient interface {
	RegisterProcess(ctx context.Context, req *registry.RegisterProcessReq) (*registry.RegisterProcessResp, error)
	Heartbeat(ctx context.Context, req *registry.HeartbeatReq) (*registry.HeartbeatResp, error)
	ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error)
}

// PubSubPublisher abstracts Dapr PubSub event publishing.
type PubSubPublisher interface {
	PublishEvent(ctx context.Context, pubsubName, topicName string, data any) error
}

// NodeExecutor executes a single worker node request (defaulting to Stage 2 WorkerRunner).
type NodeExecutor interface {
	ExecuteNode(ctx context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error)
}

// MergeExecutor executes a single merge candidate check (defaulting to runner.ExecuteMergeCandidate).
type MergeExecutor interface {
	ExecuteMergeCandidate(ctx context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error)
}

// NodeOutcomeReport delivers completed or failed node execution outcomes back to NodeActor.
type NodeOutcomeReport struct {
	RunID      string             `json:"run_id"`
	NodeAddr   string             `json:"node_addr"`
	Epoch      uint64             `json:"epoch"`
	LeaseID    string             `json:"lease_id"`
	LeaseKey   string             `json:"lease_key,omitempty"`
	ProcessID  string             `json:"process_id"`
	Aborted    bool               `json:"aborted,omitempty"`
	Outcome    runner.NodeOutcome `json:"outcome"`
	ErrMessage string             `json:"err_message,omitempty"`
}

// NodeResultReporter reports node completion back to the control plane.
type NodeResultReporter interface {
	ReportNodeOutcome(ctx context.Context, report NodeOutcomeReport) error
}

// MergeOutcomeReport delivers completed or failed merge candidate outcomes back to MergeActor.
type MergeOutcomeReport struct {
	MergeActorID    string                      `json:"merge_actor_id,omitempty"`
	RequestID       string                      `json:"request_id"`
	RunID           string                      `json:"run_id"`
	MergeNodeAddr   string                      `json:"merge_node_addr"`
	Epoch           uint64                      `json:"epoch"`
	CandidateBranch string                      `json:"candidate_branch"`
	LeaseID         string                      `json:"lease_id"`
	LeaseKey        string                      `json:"lease_key,omitempty"`
	ProcessID       string                      `json:"process_id"`
	Aborted         bool                        `json:"aborted,omitempty"`
	Result          runner.MergeCandidateResult `json:"result"`
	ErrMessage      string                      `json:"err_message,omitempty"`
}

// MergeResultReporter reports merge candidate completion back to the control plane.
type MergeResultReporter interface {
	ReportCandidateResult(ctx context.Context, report MergeOutcomeReport) error
}

// PingRequest aliases registry.PingRequest for Dapr Service Invocation "ping".
type PingRequest = registry.PingRequest

// PingResponse aliases registry.PingResponse for Dapr Service Invocation "ping".
type PingResponse = registry.PingResponse

// PluginResolveConfig carries JSON-serializable harness plugin resolution settings over Dapr RPC.
type PluginResolveConfig struct {
	LockFile           harness.PluginLockFile `json:"lock_file,omitempty"`
	MaxSingleFileBytes int64                  `json:"max_single_file_bytes,omitempty"`
	MaxTotalBytes      int64                  `json:"max_total_bytes,omitempty"`
	MaxEntries         int                    `json:"max_entries,omitempty"`
}

// StartNodeRequest is the control-plane dispatch payload for worker "start_node".
type StartNodeRequest struct {
	ProcessID             string                                  `json:"process_id,omitempty"`
	LeaseID               string                                  `json:"lease_id"`
	LeaseKey              string                                  `json:"lease_key,omitempty"`
	RunID                 string                                  `json:"run_id"`
	NodeAddr              string                                  `json:"node_addr"`
	Epoch                 uint64                                  `json:"epoch"`
	RepoURL               string                                  `json:"repo_url"`
	BasedSource           string                                  `json:"based_source,omitempty"`
	BaseSHA               string                                  `json:"base_sha,omitempty"`
	Auth                  git.AuthConfig                          `json:"auth,omitempty"`
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
	ResolvedEnv           map[string]string                       `json:"resolved_env,omitempty"`
	PlanBlobKey           string                                  `json:"plan_blob_key,omitempty"`
	PlanSHA256            string                                  `json:"plan_sha256,omitempty"`
	BundlePath            string                                  `json:"bundle_path,omitempty"`
	TargetPlatform        *harnessinstaller.Platform              `json:"target_platform,omitempty"`
	PluginResolve         PluginResolveConfig                     `json:"plugin_resolve,omitempty"`
	MaxGatekeeperReworks  int                                     `json:"max_gatekeeper_reworks,omitempty"`
	MaxArtifactReworks    int                                     `json:"max_artifact_reworks,omitempty"`
	StallTimeout          time.Duration                           `json:"stall_timeout,omitempty"`
	WatchdogCheckInterval time.Duration                           `json:"watchdog_check_interval,omitempty"`
}

// StartNodeResponse acknowledges asynchronous acceptance of a node task.
type StartNodeResponse struct {
	Accepted       bool   `json:"accepted"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
	ProcessID      string `json:"process_id"`
}

// AbortNodeRequest cancels an in-flight node execution identified by (RunID, NodeAddr, Epoch).
type AbortNodeRequest struct {
	RunID    string `json:"run_id"`
	NodeAddr string `json:"node_addr"`
	Epoch    uint64 `json:"epoch"`
}

// AbortNodeResponse reports whether an active task was found and cancelled.
type AbortNodeResponse struct {
	Aborted bool `json:"aborted"`
}

// AbortMergeRequest cancels an in-flight merge candidate execution identified by RequestID.
type AbortMergeRequest struct {
	RequestID string `json:"request_id"`
}

// AbortMergeResponse reports whether an active merge task was found and cancelled.
type AbortMergeResponse struct {
	Aborted bool `json:"aborted"`
}

// RunMergeCandidateRequest is the control-plane dispatch payload for worker "run_merge_candidate".
type RunMergeCandidateRequest struct {
	ProcessID       string                        `json:"process_id,omitempty"`
	MergeActorID    string                        `json:"merge_actor_id,omitempty"`
	RequestID       string                        `json:"request_id"`
	LeaseID         string                        `json:"lease_id"`
	LeaseKey        string                        `json:"lease_key,omitempty"`
	RunID           string                        `json:"run_id"`
	MergeNodeAddr   string                        `json:"merge_node_addr"`
	Epoch           uint64                        `json:"epoch"`
	RepoURL         string                        `json:"repo_url"`
	BasedSource     string                        `json:"based_source,omitempty"`
	BaseSHA         string                        `json:"base_sha,omitempty"`
	TargetBranch    *string                       `json:"target_branch,omitempty"`
	CandidateBranch string                        `json:"candidate_branch"`
	Auth            git.AuthConfig                `json:"auth,omitempty"`
	ResolvedEnv     map[string]string             `json:"resolved_env,omitempty"`
	Sandbox         sandboxspec.SandboxConfig     `json:"sandbox"`
	Gatekeepers     []workerspec.GatekeeperConfig `json:"gatekeepers,omitempty"`
	PlanBlobKey     string                        `json:"plan_blob_key,omitempty"`
	PlanSHA256      string                        `json:"plan_sha256,omitempty"`
	BundlePath      string                        `json:"bundle_path,omitempty"`
	ImportsDir      string                        `json:"imports_dir,omitempty"`
}

// RunMergeCandidateResponse acknowledges asynchronous acceptance of a merge candidate execution.
type RunMergeCandidateResponse struct {
	Accepted       bool   `json:"accepted"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
	ProcessID      string `json:"process_id"`
}

// CleanupRunRequest requests removal of a run's worker-local extracted bundle directory.
type CleanupRunRequest struct {
	RunID string `json:"run_id"`
}

// CleanupRunResponse reports whether the run's extracted bundle directory was removed or busy.
type CleanupRunResponse struct {
	Removed bool `json:"removed"`
	Busy    bool `json:"busy,omitempty"`
}

// LogEvent is the structured JSONL schema published to Dapr PubSub and archived to BlobStore.
type LogEvent struct {
	Timestamp  time.Time                  `json:"timestamp"`
	RunID      string                     `json:"run_id"`
	NodeAddr   string                     `json:"node_addr"`
	Epoch      uint64                     `json:"epoch"`
	ProcessID  string                     `json:"process_id"`
	EventID    string                     `json:"event_id,omitempty"`
	Kind       string                     `json:"kind"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	ToolName   string                     `json:"tool_name,omitempty"`
	Text       string                     `json:"text,omitempty"`
	Error      string                     `json:"error,omitempty"`
	Usage      *harnessruntime.TokenUsage `json:"usage,omitempty"`
}

// Options configures a WorkerDaemon instance.
type Options struct {
	ProcessID              string
	AppID                  string
	AppIDPrefix            string
	FixedAppID             string
	DevMode                bool
	MaxConcurrency         int
	Arch                   string
	Registry               RegistryClient
	PubSub                 PubSubPublisher
	BlobStore              blob.Store
	GitAdapter             git.Adapter
	SandboxEngine          sandboxruntime.Engine
	NodeExecutor           NodeExecutor
	NodeRunnerDepsProvider func(ctx context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error)
	MergeExecutor          MergeExecutor
	NodeReporter           NodeResultReporter
	MergeReporter          MergeResultReporter
	HeartbeatInterval      time.Duration
	MaxLogBufferBytes      int64
	TempDir                string
	PluginDir              string
	PubSubName             string
	EventsTopic            string
	OnFence                func(ctx context.Context, deadProcessID string)
}

type activeTask struct {
	runID      string
	cancel     context.CancelFunc
	generation uint64
	aborted    *atomic.Bool
}

type liveRunTask struct {
	runID      string
	generation uint64
	aborted    *atomic.Bool
}

// WorkerDaemon hosts worker Dapr Service Invocation handlers, dual-pipeline JSONL logging,
// and the heartbeat/self-fencing lifecycle loop.
type WorkerDaemon struct {
	opts              Options
	pubsubName        string
	eventsTopic       string
	maxLogBufferBytes int64
	heartbeatInterval time.Duration

	mu          sync.Mutex
	processID   string
	appID       string
	generation  uint64
	nodeTasks   map[string]activeTask
	mergeTasks  map[string]activeTask
	nextTaskSeq uint64
	liveTasks   map[uint64]liveRunTask
	liveTaskCh  chan struct{}

	bundleSF     singleflight.Group
	bundleLockMu sync.Mutex
	bundleLocks  map[string]*sync.Mutex

	loopCancel context.CancelFunc
	loopDone   chan struct{}
	wg         sync.WaitGroup
}

// New validates opts and constructs a WorkerDaemon.
func New(opts Options) (*WorkerDaemon, error) {
	pid := strings.TrimSpace(opts.ProcessID)
	if pid == "" {
		pid = uuid.NewString()
	}

	prefix := opts.AppIDPrefix
	if prefix == "" {
		prefix = "sofa-worker-"
	}

	appID := strings.TrimSpace(opts.AppID)
	if appID == "" {
		if strings.TrimSpace(opts.FixedAppID) != "" {
			appID = strings.TrimSpace(opts.FixedAppID)
		} else {
			appID = prefix + pid
		}
	}

	maxConc := opts.MaxConcurrency
	if maxConc <= 0 {
		if opts.DevMode {
			maxConc = registry.DefaultDevMaxConcurrency
		} else {
			maxConc = registry.DefaultWorkerMaxConcurrency
		}
	}
	opts.MaxConcurrency = maxConc
	opts.AppIDPrefix = prefix

	arch := strings.TrimSpace(opts.Arch)
	if arch == "" {
		arch = "linux/" + runtime.GOARCH
	}
	opts.Arch = arch

	hb := opts.HeartbeatInterval
	if hb <= 0 {
		hb = registry.DefaultHeartbeatInterval
	}
	maxBuf := opts.MaxLogBufferBytes
	if maxBuf <= 0 {
		maxBuf = DefaultMaxLogBufferBytes
	}
	pubsubName := strings.TrimSpace(opts.PubSubName)
	if pubsubName == "" {
		pubsubName = PubSubComponentName
	}
	topic := strings.TrimSpace(opts.EventsTopic)
	if topic == "" {
		topic = EventsTopic
	}

	if opts.NodeExecutor == nil && opts.NodeRunnerDepsProvider != nil {
		opts.NodeExecutor = &RunnerNodeExecutor{
			DepsProvider: opts.NodeRunnerDepsProvider,
			BlobStore:    opts.BlobStore,
		}
	}
	if opts.MergeExecutor == nil {
		opts.MergeExecutor = defaultMergeExecutor{}
	}

	return &WorkerDaemon{
		opts:              opts,
		pubsubName:        pubsubName,
		eventsTopic:       topic,
		maxLogBufferBytes: maxBuf,
		heartbeatInterval: hb,
		processID:         pid,
		appID:             appID,
		generation:        1,
		nodeTasks:         make(map[string]activeTask),
		mergeTasks:        make(map[string]activeTask),
		liveTasks:         make(map[uint64]liveRunTask),
		liveTaskCh:        make(chan struct{}),
		bundleLocks:       make(map[string]*sync.Mutex),
	}, nil
}

// ProcessID returns the daemon's current active process ID.
func (d *WorkerDaemon) ProcessID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.processID
}

// AppID returns the daemon's current Dapr App ID.
func (d *WorkerDaemon) AppID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.appID
}

// ActiveTaskCount returns the number of currently running node and merge tasks.
func (d *WorkerDaemon) ActiveTaskCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.nodeTasks) + len(d.mergeTasks)
}

// RegisterHandlers mounts ping, start_node, abort_node, run_merge_candidate, abort_merge, and cleanup_run onto svc.
func (d *WorkerDaemon) RegisterHandlers(svc common.Service) error {
	if svc == nil {
		return errors.New("daemon: Dapr service is required")
	}
	routes := map[string]common.ServiceInvocationHandler{
		"ping":                d.handlePingInvocation,
		"start_node":          d.handleStartNodeInvocation,
		"abort_node":          d.handleAbortNodeInvocation,
		"run_merge_candidate": d.handleRunMergeInvocation,
		"abort_merge":         d.handleAbortMergeInvocation,
		"cleanup_run":         d.handleCleanupRunInvocation,
	}
	for route, handler := range routes {
		if err := svc.AddServiceInvocationHandler(route, handler); err != nil {
			return fmt.Errorf("daemon: register handler %q: %w", route, err)
		}
	}
	return nil
}

// Start registers the worker with ProcessRegistryActor and begins the heartbeat/self-fencing loop.
func (d *WorkerDaemon) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.opts.Registry == nil {
		return errors.New("daemon: RegistryClient is required for Start")
	}

	d.mu.Lock()
	if d.loopCancel != nil {
		d.mu.Unlock()
		return errors.New("daemon: already started")
	}
	pid := d.processID
	appID := d.appID
	d.mu.Unlock()

	if _, err := d.opts.Registry.RegisterProcess(ctx, &registry.RegisterProcessReq{
		ProcessID:      pid,
		AppID:          appID,
		MaxConcurrency: d.opts.MaxConcurrency,
		Arch:           d.opts.Arch,
	}); err != nil {
		return fmt.Errorf("daemon: initial register process: %w", err)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	d.mu.Lock()
	d.loopCancel = cancel
	d.loopDone = done
	d.mu.Unlock()

	go d.heartbeatLoop(loopCtx, done)
	return nil
}

// Stop stops the heartbeat loop, cancels all in-flight tasks, and waits for goroutines to exit.
func (d *WorkerDaemon) Stop(_ context.Context) error {
	d.mu.Lock()
	cancel := d.loopCancel
	done := d.loopDone
	d.loopCancel = nil
	d.loopDone = nil
	d.generation++
	cancels := d.drainTaskCancelsLocked()
	d.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, fn := range cancels {
		fn()
	}
	if done != nil {
		<-done
	}
	d.wg.Wait()
	return nil
}

func (d *WorkerDaemon) heartbeatLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(d.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.mu.Lock()
			pid := d.processID
			active := len(d.nodeTasks) + len(d.mergeTasks)
			d.mu.Unlock()

			hbCtx, hbCancel := context.WithTimeout(ctx, 3*time.Second)
			resp, err := d.opts.Registry.Heartbeat(hbCtx, &registry.HeartbeatReq{
				ProcessID:   pid,
				ActiveTasks: active,
			})
			hbCancel()
			if err != nil {
				continue
			}
			if resp != nil && (resp.FenceRequired || resp.Status == registry.StatusDead) {
				d.selfFenceAndRotate(ctx, pid)
			}
		}
	}
}

func (d *WorkerDaemon) selfFenceAndRotate(ctx context.Context, deadPID string) {
	d.mu.Lock()
	// Ignore if another path already rotated away from deadPID.
	if d.processID != deadPID {
		d.mu.Unlock()
		return
	}
	d.generation++
	cancels := d.drainTaskCancelsLocked()
	newPID := uuid.NewString()
	d.processID = newPID
	if !d.opts.DevMode && strings.TrimSpace(d.opts.FixedAppID) == "" {
		d.appID = d.opts.AppIDPrefix + newPID
	}
	newAppID := d.appID
	d.mu.Unlock()

	for _, fn := range cancels {
		fn()
	}
	if d.opts.OnFence != nil {
		fenceCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		d.opts.OnFence(fenceCtx, deadPID)
		cancel()
	}

	regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, _ = d.opts.Registry.RegisterProcess(regCtx, &registry.RegisterProcessReq{
		ProcessID:      newPID,
		AppID:          newAppID,
		MaxConcurrency: d.opts.MaxConcurrency,
		Arch:           d.opts.Arch,
	})
	cancel()
}

func (d *WorkerDaemon) drainTaskCancelsLocked() []context.CancelFunc {
	cancels := make([]context.CancelFunc, 0, len(d.nodeTasks)+len(d.mergeTasks))
	for k, t := range d.nodeTasks {
		cancels = append(cancels, t.cancel)
		delete(d.nodeTasks, k)
	}
	for k, t := range d.mergeTasks {
		cancels = append(cancels, t.cancel)
		delete(d.mergeTasks, k)
	}
	return cancels
}

func (d *WorkerDaemon) handlePingInvocation(_ context.Context, in *common.InvocationEvent) (*common.Content, error) {
	var req PingRequest
	if in != nil && len(in.Data) > 0 {
		if err := json.Unmarshal(in.Data, &req); err != nil {
			return nil, fmt.Errorf("daemon: decode ping request: %w", err)
		}
	}

	d.mu.Lock()
	pid := d.processID
	appID := d.appID
	active := len(d.nodeTasks) + len(d.mergeTasks)
	d.mu.Unlock()

	if expected := strings.TrimSpace(req.ProcessID); expected != "" && expected != pid {
		return nil, fmt.Errorf("daemon: process_id mismatch: expected %q, active %q", expected, pid)
	}

	return marshalInvocationContent(PingResponse{
		OK:          true,
		ProcessID:   pid,
		AppID:       appID,
		ActiveTasks: active,
	})
}

func (d *WorkerDaemon) handleStartNodeInvocation(_ context.Context, in *common.InvocationEvent) (*common.Content, error) {
	if in == nil || len(in.Data) == 0 {
		return nil, errors.New("daemon: start_node payload is required")
	}
	var req StartNodeRequest
	if err := json.Unmarshal(in.Data, &req); err != nil {
		return nil, fmt.Errorf("daemon: decode start_node request: %w", err)
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.NodeAddr) == "" || req.Epoch == 0 {
		return nil, errors.New("daemon: run_id, node_addr, and epoch > 0 are required")
	}
	if strings.TrimSpace(req.RepoURL) == "" {
		return nil, errors.New("daemon: repo_url is required")
	}
	if d.opts.NodeExecutor == nil {
		return nil, errors.New("daemon: node executor is not configured")
	}

	key := nodeTaskKey(req.RunID, req.NodeAddr, req.Epoch)

	d.mu.Lock()
	pid := d.processID
	gen := d.generation
	if expected := strings.TrimSpace(req.ProcessID); expected != "" && expected != pid {
		d.mu.Unlock()
		return nil, fmt.Errorf("daemon: start_node process_id mismatch: expected %q, active %q", expected, pid)
	}
	if _, exists := d.nodeTasks[key]; exists {
		d.mu.Unlock()
		return marshalInvocationContent(StartNodeResponse{
			Accepted:       true,
			AlreadyRunning: true,
			ProcessID:      pid,
		})
	}
	taskCtx, cancel := context.WithCancel(context.Background())
	abortedFlag := &atomic.Bool{}
	d.nodeTasks[key] = activeTask{runID: req.RunID, cancel: cancel, generation: gen, aborted: abortedFlag}
	seq := d.registerLiveTaskLocked(req.RunID, gen, abortedFlag)
	d.wg.Add(1)
	d.mu.Unlock()

	go d.runNodeTask(taskCtx, cancel, key, seq, pid, gen, abortedFlag, req)

	return marshalInvocationContent(StartNodeResponse{
		Accepted:  true,
		ProcessID: pid,
	})
}

func (d *WorkerDaemon) runNodeTask(
	ctx context.Context,
	cancel context.CancelFunc,
	taskKey string,
	taskSeq uint64,
	processID string,
	generation uint64,
	abortedFlag *atomic.Bool,
	req StartNodeRequest,
) {
	defer d.wg.Done()
	defer d.removeLiveTask(taskSeq)
	defer cancel()
	defer d.removeNodeTask(taskKey, generation)

	logKey := NodeLogBlobKey(req.RunID, req.NodeAddr, req.Epoch)
	pipeline := newNodeLogPipeline(logPipelineConfig{
		RunID:      req.RunID,
		NodeAddr:   req.NodeAddr,
		Epoch:      req.Epoch,
		ProcessID:  processID,
		BlobKey:    logKey,
		MaxBytes:   d.maxLogBufferBytes,
		TempDir:    d.opts.TempDir,
		PubSub:     d.opts.PubSub,
		PubSubName: d.pubsubName,
		Topic:      d.eventsTopic,
		BlobStore:  d.opts.BlobStore,
	})
	defer pipeline.FlushAndClose()

	var (
		outcome runner.NodeOutcome
		execErr error
	)

	bundleInfo, bundleErr := d.ensureRunBundleExtracted(ctx, req.RunID, req.PlanBlobKey, req.PlanSHA256)
	var bundleDir string
	if bundleErr == nil {
		bundleDir, bundleErr = bundleDirForExtract(bundleInfo.ExtractDir, req.BundlePath)
	}
	if bundleErr != nil {
		outcome = runner.NodeOutcome{
			Status:        runner.StatusFailed,
			FailureReason: bundleErr.Error(),
		}
		execErr = bundleErr
	} else {
		isolatedLookup := makeIsolatedLookupEnv(req.ResolvedEnv)
		lockFile := req.PluginResolve.LockFile
		if strings.TrimSpace(req.PlanBlobKey) != "" {
			lockFile = bundleInfo.PluginLock
		}

		execReq := runner.NodeExecutionRequest{
			RunID:          req.RunID,
			NodeAddr:       req.NodeAddr,
			Epoch:          req.Epoch,
			RepoURL:        req.RepoURL,
			BasedSource:    req.BasedSource,
			BaseSHA:        req.BaseSHA,
			Auth:           req.Auth,
			Worker:         req.Worker,
			SystemPrompt:   req.SystemPrompt,
			Prompt:         req.Prompt,
			QCPrompt:       req.QCPrompt,
			Sandboxes:      req.Sandboxes,
			Harnesses:      req.Harnesses,
			ModelProviders: req.ModelProviders,
			MCPServers:     req.MCPServers,
			MCPResources:   req.MCPResources,
			Tools:          req.Tools,
			ToolEnvs:       req.ToolEnvs,
			Imports:        req.Imports,
			BundleDir:      bundleDir,
			TempRoot:       strings.TrimSpace(d.opts.TempDir),
			HostOS:         runtime.GOOS,
			TargetPlatform: req.TargetPlatform,
			PluginResolveOpts: harnessinstaller.ResolveOptions{
				PluginDir:          strings.TrimSpace(d.opts.PluginDir),
				LockFile:           lockFile,
				LookupEnv:          isolatedLookup,
				MaxSingleFileBytes: req.PluginResolve.MaxSingleFileBytes,
				MaxTotalBytes:      req.PluginResolve.MaxTotalBytes,
				MaxEntries:         req.PluginResolve.MaxEntries,
			},
			LookupEnv:             isolatedLookup,
			MaxGatekeeperReworks:  req.MaxGatekeeperReworks,
			MaxArtifactReworks:    req.MaxArtifactReworks,
			StallTimeout:          req.StallTimeout,
			WatchdogCheckInterval: req.WatchdogCheckInterval,
			OnEvent: func(ev harnessruntime.Event) {
				pipeline.Emit(ev)
			},
		}

		outcome, execErr = d.opts.NodeExecutor.ExecuteNode(ctx, execReq)
	}
	pipeline.FlushAndClose()

	if !d.isGenerationCurrent(generation) {
		// Daemon was self-fenced as DEAD; do not report stale outcomes or touch leases.
		return
	}

	if d.opts.NodeReporter != nil {
		report := NodeOutcomeReport{
			RunID:     req.RunID,
			NodeAddr:  req.NodeAddr,
			Epoch:     req.Epoch,
			LeaseID:   req.LeaseID,
			LeaseKey:  req.LeaseKey,
			ProcessID: processID,
			Aborted:   abortedFlag != nil && abortedFlag.Load(),
			Outcome:   outcome,
		}
		if execErr != nil {
			report.ErrMessage = execErr.Error()
		}
		repCtx, repCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.opts.NodeReporter.ReportNodeOutcome(repCtx, report)
		repCancel()
	}

	d.releaseSlotBestEffort(req.LeaseID, req.LeaseKey)
}

func (d *WorkerDaemon) handleAbortNodeInvocation(_ context.Context, in *common.InvocationEvent) (*common.Content, error) {
	if in == nil || len(in.Data) == 0 {
		return nil, errors.New("daemon: abort_node payload is required")
	}
	var req AbortNodeRequest
	if err := json.Unmarshal(in.Data, &req); err != nil {
		return nil, fmt.Errorf("daemon: decode abort_node request: %w", err)
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.NodeAddr) == "" || req.Epoch == 0 {
		return nil, errors.New("daemon: run_id, node_addr, and epoch > 0 are required")
	}

	key := nodeTaskKey(req.RunID, req.NodeAddr, req.Epoch)
	d.mu.Lock()
	t, exists := d.nodeTasks[key]
	if exists {
		if t.aborted != nil {
			t.aborted.Store(true)
		}
		delete(d.nodeTasks, key)
	}
	d.mu.Unlock()

	if exists {
		t.cancel()
	}
	return marshalInvocationContent(AbortNodeResponse{Aborted: exists})
}

func (d *WorkerDaemon) handleAbortMergeInvocation(_ context.Context, in *common.InvocationEvent) (*common.Content, error) {
	if in == nil || len(in.Data) == 0 {
		return nil, errors.New("daemon: abort_merge payload is required")
	}
	var req AbortMergeRequest
	if err := json.Unmarshal(in.Data, &req); err != nil {
		return nil, fmt.Errorf("daemon: decode abort_merge request: %w", err)
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return nil, errors.New("daemon: request_id is required")
	}

	d.mu.Lock()
	t, exists := d.mergeTasks[requestID]
	if exists {
		if t.aborted != nil {
			t.aborted.Store(true)
		}
		delete(d.mergeTasks, requestID)
	}
	d.mu.Unlock()

	if exists {
		t.cancel()
	}
	return marshalInvocationContent(AbortMergeResponse{Aborted: exists})
}

func (d *WorkerDaemon) handleRunMergeInvocation(_ context.Context, in *common.InvocationEvent) (*common.Content, error) {
	if in == nil || len(in.Data) == 0 {
		return nil, errors.New("daemon: run_merge_candidate payload is required")
	}
	var req RunMergeCandidateRequest
	if err := json.Unmarshal(in.Data, &req); err != nil {
		return nil, fmt.Errorf("daemon: decode run_merge_candidate request: %w", err)
	}
	if strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.MergeNodeAddr) == "" || req.Epoch == 0 {
		return nil, errors.New("daemon: request_id, run_id, merge_node_addr, and epoch > 0 are required")
	}
	if strings.TrimSpace(req.RepoURL) == "" || strings.TrimSpace(req.CandidateBranch) == "" {
		return nil, errors.New("daemon: repo_url and candidate_branch are required")
	}

	d.mu.Lock()
	pid := d.processID
	gen := d.generation
	if expected := strings.TrimSpace(req.ProcessID); expected != "" && expected != pid {
		d.mu.Unlock()
		return nil, fmt.Errorf("daemon: run_merge_candidate process_id mismatch: expected %q, active %q", expected, pid)
	}
	if _, exists := d.mergeTasks[req.RequestID]; exists {
		d.mu.Unlock()
		return marshalInvocationContent(RunMergeCandidateResponse{
			Accepted:       true,
			AlreadyRunning: true,
			ProcessID:      pid,
		})
	}
	taskCtx, cancel := context.WithCancel(context.Background())
	abortedFlag := &atomic.Bool{}
	d.mergeTasks[req.RequestID] = activeTask{runID: req.RunID, cancel: cancel, generation: gen, aborted: abortedFlag}
	seq := d.registerLiveTaskLocked(req.RunID, gen, abortedFlag)
	d.wg.Add(1)
	d.mu.Unlock()

	go d.runMergeTask(taskCtx, cancel, req.RequestID, seq, pid, gen, abortedFlag, req)

	return marshalInvocationContent(RunMergeCandidateResponse{
		Accepted:  true,
		ProcessID: pid,
	})
}

func (d *WorkerDaemon) runMergeTask(
	ctx context.Context,
	cancel context.CancelFunc,
	requestID string,
	taskSeq uint64,
	processID string,
	generation uint64,
	abortedFlag *atomic.Bool,
	req RunMergeCandidateRequest,
) {
	defer d.wg.Done()
	defer d.removeLiveTask(taskSeq)
	defer cancel()
	defer d.removeMergeTask(requestID, generation)

	var (
		res     runner.MergeCandidateResult
		execErr error
	)

	bundleInfo, bundleErr := d.ensureRunBundleExtracted(ctx, req.RunID, req.PlanBlobKey, req.PlanSHA256)
	var bundleDir string
	if bundleErr == nil {
		bundleDir, bundleErr = bundleDirForExtract(bundleInfo.ExtractDir, req.BundlePath)
	}
	if bundleErr != nil {
		execErr = bundleErr
	} else {
		gitAdapter := d.opts.GitAdapter
		if gitAdapter == nil {
			gitAdapter = git.NewCLIAdapter(git.CLIAdapterOptions{
				LookupEnv: makeIsolatedLookupEnv(req.ResolvedEnv),
			})
		}

		mergeReq := runner.MergeCandidateRequest{
			RunID:           req.RunID,
			MergeNodeAddr:   req.MergeNodeAddr,
			Epoch:           req.Epoch,
			RepoURL:         req.RepoURL,
			BasedSource:     req.BasedSource,
			BaseSHA:         req.BaseSHA,
			TargetBranch:    req.TargetBranch,
			CandidateBranch: req.CandidateBranch,
			Auth:            req.Auth,
			Sandbox:         req.Sandbox,
			Gatekeepers:     req.Gatekeepers,
			GitAdapter:      gitAdapter,
			SandboxEngine:   d.opts.SandboxEngine,
			BundleDir:       bundleDir,
			ImportsDir:      req.ImportsDir,
			HostOS:          runtime.GOOS,
			TempRoot:        strings.TrimSpace(d.opts.TempDir),
		}

		res, execErr = d.opts.MergeExecutor.ExecuteMergeCandidate(ctx, mergeReq)
	}
	if !d.isGenerationCurrent(generation) {
		return
	}

	if d.opts.MergeReporter != nil {
		report := MergeOutcomeReport{
			MergeActorID:    req.MergeActorID,
			RequestID:       req.RequestID,
			RunID:           req.RunID,
			MergeNodeAddr:   req.MergeNodeAddr,
			Epoch:           req.Epoch,
			CandidateBranch: req.CandidateBranch,
			LeaseID:         req.LeaseID,
			LeaseKey:        req.LeaseKey,
			ProcessID:       processID,
			Aborted:         abortedFlag != nil && abortedFlag.Load(),
			Result:          res,
		}
		if execErr != nil {
			report.ErrMessage = execErr.Error()
		}
		repCtx, repCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.opts.MergeReporter.ReportCandidateResult(repCtx, report)
		repCancel()
	}

	d.releaseSlotBestEffort(req.LeaseID, req.LeaseKey)
}

func (d *WorkerDaemon) releaseSlotBestEffort(leaseID, leaseKey string) {
	if d.opts.Registry == nil {
		return
	}
	if strings.TrimSpace(leaseID) == "" && strings.TrimSpace(leaseKey) == "" {
		return
	}
	relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = d.opts.Registry.ReleaseSlot(relCtx, &registry.ReleaseSlotReq{
		LeaseID:  leaseID,
		LeaseKey: leaseKey,
	})
}

func (d *WorkerDaemon) removeNodeTask(key string, generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.nodeTasks[key]; ok && t.generation == generation {
		delete(d.nodeTasks, key)
	}
}

func (d *WorkerDaemon) removeMergeTask(requestID string, generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.mergeTasks[requestID]; ok && t.generation == generation {
		delete(d.mergeTasks, requestID)
	}
}

func (d *WorkerDaemon) isGenerationCurrent(gen uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.generation == gen
}

func nodeTaskKey(runID, nodeAddr string, epoch uint64) string {
	return runID + "\x00" + nodeAddr + "\x00" + strconv.FormatUint(epoch, 10)
}

// makeIsolatedLookupEnv enforces that WorkerRunner reads exclusively from the control-plane-resolved
// environment map and never falls back to host os.LookupEnv.
func makeIsolatedLookupEnv(resolved map[string]string) func(string) (string, bool) {
	copied := maps.Clone(resolved)
	if copied == nil {
		copied = map[string]string{}
	}
	return func(key string) (string, bool) {
		val, ok := copied[key]
		return val, ok
	}
}

func marshalInvocationContent(v any) (*common.Content, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &common.Content{
		Data:        data,
		ContentType: "application/json",
	}, nil
}

// NodeLogBlobKey builds a BlobStore-safe object key: runs/<run_id>/logs/<node_key>/e<epoch>.jsonl.
func NodeLogBlobKey(runID, nodeAddr string, epoch uint64) string {
	return fmt.Sprintf("runs/%s/logs/%s/e%d.jsonl",
		sanitizeSegment(runID, "run"),
		sanitizeNodeKey(nodeAddr),
		epoch,
	)
}

func sanitizeSegment(raw, fallback string) string {
	var b strings.Builder
	prevSep := false
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			prevSep = false
			continue
		}
		if !prevSep && b.Len() > 0 {
			b.WriteByte('-')
			prevSep = true
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return fallback
	}
	return out
}

func sanitizeNodeKey(nodeAddr string) string {
	sum := sha256.Sum256([]byte(nodeAddr))
	shortHash := hex.EncodeToString(sum[:4])

	var b strings.Builder
	prevUnderscore := false
	for _, r := range nodeAddr {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	prefix := strings.Trim(b.String(), "_-")
	if prefix == "" {
		prefix = "node"
	}
	if len(prefix) > 64 {
		prefix = strings.TrimRight(prefix[:64], "_-")
		if prefix == "" {
			prefix = "node"
		}
	}
	return prefix + "_" + shortHash
}

type logPipelineConfig struct {
	RunID      string
	NodeAddr   string
	Epoch      uint64
	ProcessID  string
	BlobKey    string
	MaxBytes   int64
	TempDir    string
	PubSub     PubSubPublisher
	PubSubName string
	Topic      string
	BlobStore  blob.Store
}

type nodeLogPipeline struct {
	cfg logPipelineConfig

	mu           sync.Mutex
	file         *os.File
	filePath     string
	writtenBytes int64
	closed       bool
}

func newNodeLogPipeline(cfg logPipelineConfig) *nodeLogPipeline {
	p := &nodeLogPipeline{cfg: cfg}
	if cfg.BlobStore != nil {
		if strings.TrimSpace(cfg.TempDir) != "" {
			_ = os.MkdirAll(cfg.TempDir, 0o755)
		}
		f, err := os.CreateTemp(cfg.TempDir, "sofa-node-log-*.jsonl")
		if err == nil {
			if rerr := restrictLogFile(f.Name(), f); rerr != nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
				return p
			}
			p.file = f
			p.filePath = f.Name()
		}
	}
	return p
}

func (p *nodeLogPipeline) Emit(ev harnessruntime.Event) {
	ts := time.Now().UTC()
	if ev.TimestampUnixNano > 0 {
		ts = time.Unix(0, ev.TimestampUnixNano).UTC()
	}
	logEv := LogEvent{
		Timestamp:  ts,
		RunID:      p.cfg.RunID,
		NodeAddr:   p.cfg.NodeAddr,
		Epoch:      p.cfg.Epoch,
		ProcessID:  p.cfg.ProcessID,
		EventID:    ev.EventID,
		Kind:       eventKindLabel(ev.Kind),
		ToolCallID: ev.ToolCallID,
		ToolName:   ev.ToolName,
		Text:       ev.Text,
		Error:      ev.ErrorMessage,
		Usage:      ev.Usage,
	}

	if p.cfg.PubSub != nil {
		pubCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = p.cfg.PubSub.PublishEvent(pubCtx, p.cfg.PubSubName, p.cfg.Topic, logEv)
		cancel()
	}

	line, err := json.Marshal(logEv)
	if err != nil {
		return
	}
	line = append(line, '\n')

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.file == nil {
		return
	}
	if p.cfg.MaxBytes <= 0 || p.writtenBytes+int64(len(line)) <= p.cfg.MaxBytes {
		if n, werr := p.file.Write(line); werr == nil {
			p.writtenBytes += int64(n)
		}
	}
	if ev.Kind == harnessruntime.EventToolExecutionComplete && ev.ToolName == "sofa_checkpoint" {
		p.uploadLocked()
	}
}

func (p *nodeLogPipeline) FlushAndClose() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.file != nil {
		p.uploadLocked()
		_ = p.file.Close()
		p.file = nil
	}
	if p.filePath != "" {
		_ = os.Remove(p.filePath)
		p.filePath = ""
	}
}

func (p *nodeLogPipeline) uploadLocked() {
	if p.cfg.BlobStore == nil || p.file == nil {
		return
	}
	if _, err := p.file.Seek(0, 0); err != nil {
		return
	}
	uploadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.cfg.BlobStore.Put(uploadCtx, p.cfg.BlobKey, p.file, "")
}

func eventKindLabel(k harnessruntime.EventKind) string {
	switch k {
	case harnessruntime.EventAssistantMessageDelta:
		return "assistant_message_delta"
	case harnessruntime.EventAssistantMessage:
		return "assistant_message"
	case harnessruntime.EventAssistantReasoningDelta:
		return "assistant_reasoning_delta"
	case harnessruntime.EventAssistantReasoning:
		return "assistant_reasoning"
	case harnessruntime.EventAssistantUsage:
		return "assistant_usage"
	case harnessruntime.EventAssistantToolCallDelta:
		return "assistant_tool_call_delta"
	case harnessruntime.EventToolExecutionStart:
		return "tool_execution_start"
	case harnessruntime.EventToolExecutionProgress:
		return "tool_execution_progress"
	case harnessruntime.EventToolShellOutput:
		return "tool_shell_output"
	case harnessruntime.EventToolExecutionComplete:
		return "tool_execution_complete"
	case harnessruntime.EventSubagentStarted:
		return "subagent_started"
	case harnessruntime.EventSubagentCompleted:
		return "subagent_completed"
	case harnessruntime.EventSubagentFailed:
		return "subagent_failed"
	case harnessruntime.EventSessionWarning:
		return "session_warning"
	case harnessruntime.EventSessionIdle:
		return "session_idle"
	default:
		return "unspecified"
	}
}

// RunnerNodeExecutor adapts Stage 2 runner.WorkerRunner to NodeExecutor.
type RunnerNodeExecutor struct {
	DepsProvider func(ctx context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error)
	BlobStore    blob.Store
}

// ExecuteNode constructs a runner.WorkerRunner using DepsProvider and executes req.
func (e *RunnerNodeExecutor) ExecuteNode(ctx context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
	if e.DepsProvider == nil {
		return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: "deps provider is nil"}, errors.New("daemon: runner deps provider is nil")
	}
	deps, err := e.DepsProvider(ctx, req)
	if err != nil {
		return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: err.Error()}, err
	}
	if deps.BlobStore == nil {
		deps.BlobStore = e.BlobStore
	}
	wr, err := runner.NewWorkerRunner(deps)
	if err != nil {
		return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: err.Error()}, err
	}
	return wr.Run(ctx, req)
}

type defaultMergeExecutor struct{}

func (defaultMergeExecutor) ExecuteMergeCandidate(ctx context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
	return runner.ExecuteMergeCandidate(ctx, req)
}

type stubRegistryClient struct {
	stub *registry.ClientStub
}

// NewStubRegistryClient adapts a *registry.ClientStub to RegistryClient.
func NewStubRegistryClient(stub *registry.ClientStub) RegistryClient {
	return &stubRegistryClient{stub: stub}
}

// NewDaprRegistryClient builds a RegistryClient wired to ProcessRegistryActor("global") via client.
func NewDaprRegistryClient(client daprclient.Client) RegistryClient {
	return NewStubRegistryClient(registry.NewClientStub(client))
}

func (c *stubRegistryClient) RegisterProcess(ctx context.Context, req *registry.RegisterProcessReq) (*registry.RegisterProcessResp, error) {
	return c.stub.RegisterProcess(ctx, req)
}

func (c *stubRegistryClient) Heartbeat(ctx context.Context, req *registry.HeartbeatReq) (*registry.HeartbeatResp, error) {
	return c.stub.Heartbeat(ctx, req)
}

func (c *stubRegistryClient) ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	return c.stub.ReleaseSlot(ctx, req)
}

type daprPubSubPublisher struct {
	client daprclient.Client
}

// NewDaprPubSubPublisher adapts daprclient.Client to PubSubPublisher.
func NewDaprPubSubPublisher(client daprclient.Client) PubSubPublisher {
	return &daprPubSubPublisher{client: client}
}

func (p *daprPubSubPublisher) PublishEvent(ctx context.Context, pubsubName, topicName string, data any) error {
	return p.client.PublishEvent(ctx, pubsubName, topicName, data)
}

type bundleMarker struct {
	PlanBlobKey string `json:"plan_blob_key,omitempty"`
	PlanSHA256  string `json:"plan_sha256"`
}

type extractedBundle struct {
	ExtractDir string
	PluginLock harness.PluginLockFile
}

func (d *WorkerDaemon) effectiveTempDir() string {
	if t := strings.TrimSpace(d.opts.TempDir); t != "" {
		return t
	}
	return os.TempDir()
}

func (d *WorkerDaemon) runBundleExtractDir(runID string) string {
	return filepath.Join(d.effectiveTempDir(), "bundles", sanitizeSegment(runID, "run"))
}

func (d *WorkerDaemon) lockRunBundle(sanitizedRunID string) func() {
	d.bundleLockMu.Lock()
	if d.bundleLocks == nil {
		d.bundleLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := d.bundleLocks[sanitizedRunID]
	if !ok {
		mu = &sync.Mutex{}
		d.bundleLocks[sanitizedRunID] = mu
	}
	d.bundleLockMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

func (d *WorkerDaemon) registerLiveTaskLocked(runID string, generation uint64, aborted *atomic.Bool) uint64 {
	d.nextTaskSeq++
	seq := d.nextTaskSeq
	if d.liveTasks == nil {
		d.liveTasks = make(map[uint64]liveRunTask)
	}
	if d.liveTaskCh == nil {
		d.liveTaskCh = make(chan struct{})
	}
	d.liveTasks[seq] = liveRunTask{
		runID:      runID,
		generation: generation,
		aborted:    aborted,
	}
	return seq
}

func (d *WorkerDaemon) removeLiveTask(seq uint64) {
	d.mu.Lock()
	delete(d.liveTasks, seq)
	if d.liveTaskCh != nil {
		close(d.liveTaskCh)
		d.liveTaskCh = make(chan struct{})
	}
	d.mu.Unlock()
}

func (d *WorkerDaemon) inspectLiveTasksForRunLocked(runID string) (hasLive bool, hasNonAborting bool) {
	for _, t := range d.liveTasks {
		if t.runID != runID {
			continue
		}
		hasLive = true
		isAborting := (t.aborted != nil && t.aborted.Load()) || t.generation != d.generation
		if !isAborting {
			hasNonAborting = true
		}
	}
	return hasLive, hasNonAborting
}

func (d *WorkerDaemon) hasLiveTasksForRun(runID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	hasLive, _ := d.inspectLiveTasksForRunLocked(runID)
	return hasLive
}

func (d *WorkerDaemon) waitForAbortingRunTasks(ctx context.Context, runID string) bool {
	waitCtx, cancel := context.WithTimeout(ctx, cleanupAbortGraceTimeout)
	defer cancel()

	for {
		d.mu.Lock()
		hasLive, hasNonAborting := d.inspectLiveTasksForRunLocked(runID)
		ch := d.liveTaskCh
		d.mu.Unlock()

		if !hasLive {
			return true
		}
		if hasNonAborting || ch == nil {
			return false
		}

		select {
		case <-ch:
		case <-waitCtx.Done():
			return !d.hasLiveTasksForRun(runID)
		}
	}
}

func (d *WorkerDaemon) handleCleanupRunInvocation(ctx context.Context, in *common.InvocationEvent) (*common.Content, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if in == nil || len(in.Data) == 0 {
		return nil, errors.New("daemon: cleanup_run payload is required")
	}
	var req CleanupRunRequest
	if err := json.Unmarshal(in.Data, &req); err != nil {
		return nil, fmt.Errorf("daemon: decode cleanup_run request: %w", err)
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		return nil, errors.New("daemon: run_id is required")
	}

	if !d.waitForAbortingRunTasks(ctx, runID) {
		return marshalInvocationContent(CleanupRunResponse{
			Removed: false,
			Busy:    true,
		})
	}

	sanitized := sanitizeSegment(runID, "run")
	unlock := d.lockRunBundle(sanitized)
	defer unlock()

	if d.hasLiveTasksForRun(runID) {
		return marshalInvocationContent(CleanupRunResponse{
			Removed: false,
			Busy:    true,
		})
	}

	extractDir := d.runBundleExtractDir(runID)
	if _, err := os.Stat(extractDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return marshalInvocationContent(CleanupRunResponse{
				Removed: false,
			})
		}
		return nil, fmt.Errorf("daemon: inspect bundle dir %q: %w", extractDir, err)
	}
	if err := os.RemoveAll(extractDir); err != nil {
		return nil, fmt.Errorf("daemon: remove bundle dir %q: %w", extractDir, err)
	}
	return marshalInvocationContent(CleanupRunResponse{
		Removed: true,
	})
}

func (d *WorkerDaemon) ensureRunBundleExtracted(ctx context.Context, runID, planBlobKey, expectedSHA string) (extractedBundle, error) {
	key := strings.TrimSpace(planBlobKey)
	expSHA := strings.TrimSpace(expectedSHA)
	if key == "" {
		if expSHA != "" {
			return extractedBundle{}, errors.New("daemon: plan_blob_key is required when plan_sha256 is set")
		}
		return extractedBundle{}, nil
	}

	sanitized := sanitizeSegment(runID, "run")
	ch := d.bundleSF.DoChan(sanitized, func() (any, error) {
		extractCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		unlock := d.lockRunBundle(sanitized)
		defer unlock()

		extractDir := d.runBundleExtractDir(runID)
		markerPath := filepath.Join(extractDir, bundleMarkerFile)
		if markerBytes, err := os.ReadFile(markerPath); err == nil {
			var marker bundleMarker
			if uErr := json.Unmarshal(markerBytes, &marker); uErr != nil {
				return nil, fmt.Errorf("daemon: invalid bundle marker for run %q: %w", runID, uErr)
			}
			if expSHA != "" && !strings.EqualFold(marker.PlanSHA256, expSHA) {
				return nil, fmt.Errorf("daemon: bundle sha256 mismatch for run %q: cached %s, requested %s", runID, marker.PlanSHA256, expSHA)
			}
			if info, sErr := os.Stat(extractDir); sErr == nil && info.IsDir() {
				lockFile, lErr := loadPluginLockFromExtract(extractDir)
				if lErr != nil {
					return nil, lErr
				}
				return extractedBundle{
					ExtractDir: extractDir,
					PluginLock: lockFile,
				}, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("daemon: read bundle marker for run %q: %w", runID, err)
		}

		if d.opts.BlobStore == nil {
			return nil, errors.New("daemon: blob_store is required to download plan bundle")
		}
		bundlesRoot := filepath.Join(d.effectiveTempDir(), "bundles")
		if err := os.MkdirAll(bundlesRoot, 0o755); err != nil {
			return nil, fmt.Errorf("daemon: create bundles root %q: %w", bundlesRoot, err)
		}

		rc, _, err := d.opts.BlobStore.Get(extractCtx, key, expSHA)
		if err != nil {
			return nil, fmt.Errorf("daemon: fetch plan %q from blob store: %w", key, err)
		}
		defer func() { _ = rc.Close() }()

		tmpFile, err := os.CreateTemp(bundlesRoot, ".plan-"+sanitized+"-*.sofaplan")
		if err != nil {
			return nil, fmt.Errorf("daemon: create temp plan file: %w", err)
		}
		tmpPath := tmpFile.Name()
		defer func() {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}()

		hasher := sha256.New()
		if _, err := io.Copy(io.MultiWriter(tmpFile, hasher), rc); err != nil {
			return nil, fmt.Errorf("daemon: stream plan %q: %w", key, err)
		}
		if err := tmpFile.Close(); err != nil {
			return nil, fmt.Errorf("daemon: close temp plan file: %w", err)
		}
		actualSHA := hex.EncodeToString(hasher.Sum(nil))
		if expSHA != "" && !strings.EqualFold(actualSHA, expSHA) {
			return nil, fmt.Errorf("daemon: plan sha256 mismatch for run %q: expected %s, got %s", runID, expSHA, actualSHA)
		}

		stagingDir, err := os.MkdirTemp(bundlesRoot, ".staging-"+sanitized+"-*")
		if err != nil {
			return nil, fmt.Errorf("daemon: create staging bundle dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(stagingDir) }()

		loaded, err := plan.LoadArchive(tmpPath, stagingDir, plan.LoadArchiveOptions{})
		if err != nil {
			return nil, fmt.Errorf("daemon: load plan archive for run %q: %w", runID, err)
		}

		markerData, err := json.Marshal(bundleMarker{
			PlanBlobKey: key,
			PlanSHA256:  actualSHA,
		})
		if err != nil {
			return nil, fmt.Errorf("daemon: encode bundle marker: %w", err)
		}
		if err := os.WriteFile(filepath.Join(stagingDir, bundleMarkerFile), markerData, 0o600); err != nil {
			return nil, fmt.Errorf("daemon: write bundle marker: %w", err)
		}

		if err := os.RemoveAll(extractDir); err != nil {
			return nil, fmt.Errorf("daemon: clear stale bundle dir %q: %w", extractDir, err)
		}
		if err := os.Rename(stagingDir, extractDir); err != nil {
			return nil, fmt.Errorf("daemon: activate bundle dir %q: %w", extractDir, err)
		}

		var lockFile harness.PluginLockFile
		if loaded.PluginLock != nil {
			lockFile = *loaded.PluginLock
		}
		return extractedBundle{
			ExtractDir: extractDir,
			PluginLock: lockFile,
		}, nil
	})

	select {
	case <-ctx.Done():
		return extractedBundle{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return extractedBundle{}, res.Err
		}
		bundle, ok := res.Val.(extractedBundle)
		if !ok {
			return extractedBundle{}, errors.New("daemon: unexpected bundle extraction result type")
		}
		return bundle, nil
	}
}

func loadPluginLockFromExtract(extractDir string) (harness.PluginLockFile, error) {
	lockPath := filepath.Join(extractDir, "plugins.lock.json")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return harness.PluginLockFile{}, nil
		}
		return harness.PluginLockFile{}, fmt.Errorf("daemon: read %q: %w", lockPath, err)
	}
	var lockFile harness.PluginLockFile
	if err := json.Unmarshal(raw, &lockFile); err != nil {
		return harness.PluginLockFile{}, fmt.Errorf("daemon: decode %q: %w", lockPath, err)
	}
	if err := harness.ValidateLockFile(lockFile); err != nil {
		return harness.PluginLockFile{}, fmt.Errorf("daemon: invalid %q: %w", lockPath, err)
	}
	return lockFile, nil
}

func bundleDirForExtract(extractDir, bundlePath string) (string, error) {
	if strings.TrimSpace(extractDir) == "" {
		return "", nil
	}
	if bp := strings.TrimSpace(bundlePath); bp != "" {
		if strings.Contains(bp, `\`) || strings.HasPrefix(bp, "/") || (len(bp) >= 2 && isASCIIAlpha(bp[0]) && bp[1] == ':') {
			return "", fmt.Errorf("daemon: invalid bundle_path %q", bundlePath)
		}
		cleaned := path.Clean(filepath.ToSlash(bp))
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
			return "", fmt.Errorf("daemon: invalid bundle_path %q", bundlePath)
		}
		if cleaned != "." {
			candidate := filepath.Join(extractDir, filepath.FromSlash(cleaned))
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate, nil
			}
		}
	}
	cfgDir := filepath.Join(extractDir, "config")
	if info, err := os.Stat(cfgDir); err == nil && info.IsDir() {
		return cfgDir, nil
	}
	return extractDir, nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
