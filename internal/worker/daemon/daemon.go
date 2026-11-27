package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
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
)

const (
	// PubSubComponentName is the Dapr PubSub component used for real-time worker event streaming.
	PubSubComponentName = daprhost.PubSubComponentName
	// EventsTopic is the Dapr PubSub topic carrying structured worker node log events.
	EventsTopic = "sofa-events"
	// DefaultMaxLogBufferBytes bounds the per-task local JSONL spool file before capping writes.
	DefaultMaxLogBufferBytes int64 = 16 * 1024 * 1024
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
	PluginDir          string                 `json:"plugin_dir,omitempty"`
	WorkingDir         string                 `json:"working_dir,omitempty"`
	UserHomeDir        string                 `json:"user_home_dir,omitempty"`
	CacheRoot          string                 `json:"cache_root,omitempty"`
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
	BundleDir             string                                  `json:"bundle_dir,omitempty"`
	TempRoot              string                                  `json:"temp_root,omitempty"`
	HostOS                string                                  `json:"host_os,omitempty"`
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
	Sandbox         sandboxspec.SandboxConfig     `json:"sandbox"`
	Gatekeepers     []workerspec.GatekeeperConfig `json:"gatekeepers,omitempty"`
	BundleDir       string                        `json:"bundle_dir,omitempty"`
	ImportsDir      string                        `json:"imports_dir,omitempty"`
	HostOS          string                        `json:"host_os,omitempty"`
	TempRoot        string                        `json:"temp_root,omitempty"`
}

// RunMergeCandidateResponse acknowledges asynchronous acceptance of a merge candidate execution.
type RunMergeCandidateResponse struct {
	Accepted       bool   `json:"accepted"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
	ProcessID      string `json:"process_id"`
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
	PubSubName             string
	EventsTopic            string
	OnFence                func(ctx context.Context, deadProcessID string)
}

type activeTask struct {
	cancel     context.CancelFunc
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

	mu         sync.Mutex
	processID  string
	appID      string
	generation uint64
	nodeTasks  map[string]activeTask
	mergeTasks map[string]activeTask

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

// RegisterHandlers mounts ping, start_node, abort_node, run_merge_candidate, and abort_merge onto svc.
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
	d.nodeTasks[key] = activeTask{cancel: cancel, generation: gen, aborted: abortedFlag}
	d.wg.Add(1)
	d.mu.Unlock()

	go d.runNodeTask(taskCtx, cancel, key, pid, gen, abortedFlag, req)

	return marshalInvocationContent(StartNodeResponse{
		Accepted:  true,
		ProcessID: pid,
	})
}

func (d *WorkerDaemon) runNodeTask(
	ctx context.Context,
	cancel context.CancelFunc,
	taskKey string,
	processID string,
	generation uint64,
	abortedFlag *atomic.Bool,
	req StartNodeRequest,
) {
	defer d.wg.Done()
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

	isolatedLookup := makeIsolatedLookupEnv(req.ResolvedEnv)

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
		BundleDir:      req.BundleDir,
		TempRoot:       req.TempRoot,
		HostOS:         req.HostOS,
		TargetPlatform: req.TargetPlatform,
		PluginResolveOpts: harnessinstaller.ResolveOptions{
			PluginDir:          req.PluginResolve.PluginDir,
			WorkingDir:         req.PluginResolve.WorkingDir,
			UserHomeDir:        req.PluginResolve.UserHomeDir,
			CacheRoot:          req.PluginResolve.CacheRoot,
			LockFile:           req.PluginResolve.LockFile,
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

	outcome, execErr := d.opts.NodeExecutor.ExecuteNode(ctx, execReq)
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
	d.mergeTasks[req.RequestID] = activeTask{cancel: cancel, generation: gen, aborted: abortedFlag}
	d.wg.Add(1)
	d.mu.Unlock()

	go d.runMergeTask(taskCtx, cancel, req.RequestID, pid, gen, abortedFlag, req)

	return marshalInvocationContent(RunMergeCandidateResponse{
		Accepted:  true,
		ProcessID: pid,
	})
}

func (d *WorkerDaemon) runMergeTask(
	ctx context.Context,
	cancel context.CancelFunc,
	requestID string,
	processID string,
	generation uint64,
	abortedFlag *atomic.Bool,
	req RunMergeCandidateRequest,
) {
	defer d.wg.Done()
	defer cancel()
	defer d.removeMergeTask(requestID, generation)

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
		GitAdapter:      d.opts.GitAdapter,
		SandboxEngine:   d.opts.SandboxEngine,
		BundleDir:       req.BundleDir,
		ImportsDir:      req.ImportsDir,
		HostOS:          req.HostOS,
		TempRoot:        req.TempRoot,
	}

	res, execErr := d.opts.MergeExecutor.ExecuteMergeCandidate(ctx, mergeReq)
	if !d.isGenerationCurrent(generation) {
		return
	}

	if d.opts.MergeReporter != nil {
		report := MergeOutcomeReport{
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
