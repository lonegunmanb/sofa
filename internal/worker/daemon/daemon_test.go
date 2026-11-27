package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	actorconfig "github.com/dapr/go-sdk/actor/config"
	"github.com/dapr/go-sdk/service/common"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/harness"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRegistryClient struct {
	mu          sync.Mutex
	registered  []registry.RegisterProcessReq
	heartbeats  []registry.HeartbeatReq
	released    []registry.ReleaseSlotReq
	nextHBResp  registry.HeartbeatResp
	hbErr       error
	onHeartbeat func(req registry.HeartbeatReq) registry.HeartbeatResp
	onRegister  func(req registry.RegisterProcessReq)
}

func (f *fakeRegistryClient) RegisterProcess(_ context.Context, req *registry.RegisterProcessReq) (*registry.RegisterProcessResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, *req)
	if f.onRegister != nil {
		f.onRegister(*req)
	}
	return &registry.RegisterProcessResp{Registered: true, Status: registry.StatusHealthy}, nil
}

func (f *fakeRegistryClient) Heartbeat(_ context.Context, req *registry.HeartbeatReq) (*registry.HeartbeatResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, *req)
	if f.hbErr != nil {
		return nil, f.hbErr
	}
	if f.onHeartbeat != nil {
		resp := f.onHeartbeat(*req)
		return &resp, nil
	}
	resp := f.nextHBResp
	if resp.Status == "" {
		resp.Status = registry.StatusHealthy
	}
	return &resp, nil
}

func (f *fakeRegistryClient) ReleaseSlot(_ context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, *req)
	return &registry.ReleaseSlotResp{Released: true}, nil
}

func (f *fakeRegistryClient) Registrations() []registry.RegisterProcessReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]registry.RegisterProcessReq, len(f.registered))
	copy(out, f.registered)
	return out
}

func (f *fakeRegistryClient) Releases() []registry.ReleaseSlotReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]registry.ReleaseSlotReq, len(f.released))
	copy(out, f.released)
	return out
}

type publishedRecord struct {
	PubSub string
	Topic  string
	Event  daemon.LogEvent
}

type fakePubSub struct {
	mu      sync.Mutex
	records []publishedRecord
}

func (p *fakePubSub) PublishEvent(_ context.Context, pubsubName, topicName string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var ev daemon.LogEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, publishedRecord{
		PubSub: pubsubName,
		Topic:  topicName,
		Event:  ev,
	})
	return nil
}

func (p *fakePubSub) Records() []publishedRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]publishedRecord, len(p.records))
	copy(out, p.records)
	return out
}

type fakeNodeReporter struct {
	mu      sync.Mutex
	reports []daemon.NodeOutcomeReport
}

func (r *fakeNodeReporter) ReportNodeOutcome(_ context.Context, report daemon.NodeOutcomeReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report)
	return nil
}

func (r *fakeNodeReporter) Reports() []daemon.NodeOutcomeReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]daemon.NodeOutcomeReport, len(r.reports))
	copy(out, r.reports)
	return out
}

type fakeMergeReporter struct {
	mu      sync.Mutex
	reports []daemon.MergeOutcomeReport
}

func (r *fakeMergeReporter) ReportCandidateResult(_ context.Context, report daemon.MergeOutcomeReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report)
	return nil
}

func (r *fakeMergeReporter) Reports() []daemon.MergeOutcomeReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]daemon.MergeOutcomeReport, len(r.reports))
	copy(out, r.reports)
	return out
}

type funcNodeExecutor func(ctx context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error)

func (fn funcNodeExecutor) ExecuteNode(ctx context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
	return fn(ctx, req)
}

type funcMergeExecutor func(ctx context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error)

func (fn funcMergeExecutor) ExecuteMergeCandidate(ctx context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
	return fn(ctx, req)
}

type fakeService struct {
	common.Service
	handlers map[string]common.ServiceInvocationHandler
}

func (s *fakeService) AddServiceInvocationHandler(route string, fn common.ServiceInvocationHandler) error {
	if s.handlers == nil {
		s.handlers = make(map[string]common.ServiceInvocationHandler)
	}
	s.handlers[route] = fn
	return nil
}

func (s *fakeService) AddTopicEventHandler(_ *common.Subscription, _ common.TopicEventHandler) error {
	return nil
}
func (s *fakeService) AddTopicEventSubscriber(_ *common.Subscription, _ common.TopicEventSubscriber) error {
	return nil
}
func (s *fakeService) AddBindingInvocationHandler(_ string, _ common.BindingInvocationHandler) error {
	return nil
}
func (s *fakeService) RegisterActorImplFactory(_ actor.Factory, _ ...actorconfig.Option) {}
func (s *fakeService) RegisterActorImplFactoryContext(_ actor.FactoryContext, _ ...actorconfig.Option) {
}
func (s *fakeService) AddJobEventHandler(_ string, _ common.JobEventHandler) error { return nil }
func (s *fakeService) Start() error                                                { return nil }
func (s *fakeService) Stop() error                                                 { return nil }
func (s *fakeService) GracefulStop() error                                         { return nil }
func (s *fakeService) AddHealthCheckHandler(_ string, _ common.HealthCheckHandler) error {
	return nil
}

func invokeFakeHandler[Req any, Resp any](t *testing.T, svc *fakeService, route string, req Req) (Resp, error) {
	return invokeFakeHandlerCtx[Req, Resp](t, context.Background(), svc, route, req)
}

func invokeFakeHandlerCtx[Req any, Resp any](t *testing.T, ctx context.Context, svc *fakeService, route string, req Req) (Resp, error) {
	t.Helper()
	h, ok := svc.handlers[route]
	require.True(t, ok, "route %q not registered", route)
	data, err := json.Marshal(req)
	require.NoError(t, err)
	out, err := h(ctx, &common.InvocationEvent{
		Verb:        "POST",
		ContentType: "application/json",
		Data:        data,
	})
	var resp Resp
	if err != nil {
		return resp, err
	}
	if out != nil && len(out.Data) > 0 {
		require.NoError(t, json.Unmarshal(out.Data, &resp))
	}
	return resp, nil
}

func TestWorkerDaemon_PingStartAbortEnvIsolationAndDualLogging(t *testing.T) {
	t.Setenv("HOST_ONLY_SECRET", "must-never-leak-to-worker-runner")

	store, err := blob.NewLocalStore(t.TempDir())
	require.NoError(t, err)

	reg := &fakeRegistryClient{}
	pubsub := &fakePubSub{}
	nodeRep := &fakeNodeReporter{}

	var (
		capturedLookupOK   bool
		capturedLookupVal  string
		capturedManagedVal string
		nodeStarted        = make(chan struct{})
		allowFinish        = make(chan struct{})
		checkpointDone     = make(chan struct{})
	)

	d, err := daemon.New(daemon.Options{
		ProcessID:         "proc-init-1",
		AppID:             "sofa-worker-proc-init-1",
		MaxConcurrency:    registry.DefaultWorkerMaxConcurrency,
		Arch:              "linux/amd64",
		Registry:          reg,
		PubSub:            pubsub,
		BlobStore:         store,
		NodeReporter:      nodeRep,
		HeartbeatInterval: 10 * time.Second,
		NodeExecutor: funcNodeExecutor(func(ctx context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			require.NotNil(t, req.LookupEnv, "LookupEnv must never be nil (would fall back to os.LookupEnv)")
			capturedLookupVal, capturedLookupOK = req.LookupEnv("HOST_ONLY_SECRET")
			capturedManagedVal, _ = req.LookupEnv("MANAGED_KEY")

			if req.OnEvent != nil {
				req.OnEvent(harnessruntime.Event{
					EventID: "ev-1",
					Kind:    harnessruntime.EventAssistantMessageDelta,
					Text:    "working on task",
				})
				req.OnEvent(harnessruntime.Event{
					EventID:  "ev-2",
					Kind:     harnessruntime.EventToolExecutionComplete,
					ToolName: "sofa_checkpoint",
				})
			}
			close(checkpointDone)
			close(nodeStarted)

			select {
			case <-allowFinish:
				return runner.NodeOutcome{
					Status:     runner.StatusSucceeded,
					WorkBranch: "sofa/work/run-1/step1",
					CommitSHA:  strings.Repeat("a", 40),
				}, nil
			case <-ctx.Done():
				return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: ctx.Err().Error()}, ctx.Err()
			}
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	// Ping matches active ProcessID and rejects mismatched ProcessID.
	pingOK, err := invokeFakeHandler[daemon.PingRequest, daemon.PingResponse](t, svc, "ping", daemon.PingRequest{ProcessID: "proc-init-1"})
	require.NoError(t, err)
	assert.True(t, pingOK.OK)
	assert.Equal(t, "proc-init-1", pingOK.ProcessID)

	_, err = invokeFakeHandler[daemon.PingRequest, daemon.PingResponse](t, svc, "ping", daemon.PingRequest{ProcessID: "stale-proc-0"})
	require.Error(t, err)

	// StartNode returns immediately (non-blocking) and executes asynchronously.
	startBegin := time.Now()
	startResp, err := invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID: "proc-init-1",
		LeaseID:   "lease-1",
		LeaseKey:  "run-1/dynamic_worker.dyn[\"check/1\"]/e2",
		RunID:     "run-1",
		NodeAddr:  `dynamic_worker.dyn["check/1"]`,
		Epoch:     2,
		RepoURL:   "https://example.com/repo.git",
		ResolvedEnv: map[string]string{
			"MANAGED_KEY": "managed-secret-val",
		},
	})
	require.NoError(t, err)
	assert.True(t, startResp.Accepted)
	assert.False(t, startResp.AlreadyRunning)
	assert.Less(t, time.Since(startBegin), 500*time.Millisecond)

	// Duplicate StartNode for same (RunID, NodeAddr, Epoch) is idempotent.
	dupResp, err := invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID: "proc-init-1",
		LeaseID:   "lease-1",
		RunID:     "run-1",
		NodeAddr:  `dynamic_worker.dyn["check/1"]`,
		Epoch:     2,
		RepoURL:   "https://example.com/repo.git",
	})
	require.NoError(t, err)
	assert.True(t, dupResp.Accepted)
	assert.True(t, dupResp.AlreadyRunning)

	<-nodeStarted
	<-checkpointDone

	// Security red line: HOST_ONLY_SECRET from host os.Environ MUST NOT be visible!
	assert.False(t, capturedLookupOK)
	assert.Empty(t, capturedLookupVal)
	assert.Equal(t, "managed-secret-val", capturedManagedVal)

	// Verify checkpoint uploaded JSONL to BlobStore AND PubSub received events!
	logKey := daemon.NodeLogBlobKey("run-1", `dynamic_worker.dyn["check/1"]`, 2)
	require.NoError(t, blob.ValidateKey(logKey))
	require.Eventually(t, func() bool {
		_, statErr := store.Stat(context.Background(), logKey)
		return statErr == nil
	}, 2*time.Second, 15*time.Millisecond, "checkpoint event must flush JSONL log to BlobStore")

	rc, _, err := store.Get(context.Background(), logKey, "")
	require.NoError(t, err)
	ckptBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Contains(t, string(ckptBytes), "working on task")
	assert.Contains(t, string(ckptBytes), "sofa_checkpoint")

	pubRecs := pubsub.Records()
	require.GreaterOrEqual(t, len(pubRecs), 2)
	assert.Equal(t, "sofa-pubsub", pubRecs[0].PubSub)
	assert.Equal(t, "sofa-events", pubRecs[0].Topic)

	// Let node finish and verify NodeOutcomeReport + ReleaseSlot.
	close(allowFinish)
	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 1 && len(reg.Releases()) == 1
	}, 2*time.Second, 15*time.Millisecond)
	assert.Equal(t, runner.StatusSucceeded, nodeRep.Reports()[0].Outcome.Status)
	assert.False(t, nodeRep.Reports()[0].Aborted)
	assert.Equal(t, "lease-1", reg.Releases()[0].LeaseID)

	// Verify AbortNode cancels an active task immediately, marks Aborted=true, and is idempotent.
	abortedChan := make(chan struct{})
	abortRep := &fakeNodeReporter{}
	d2, err := daemon.New(daemon.Options{
		ProcessID:    "proc-abort",
		AppID:        "sofa-worker-proc-abort",
		Registry:     reg,
		BlobStore:    store,
		NodeReporter: abortRep,
		NodeExecutor: funcNodeExecutor(func(ctx context.Context, _ runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			<-ctx.Done()
			close(abortedChan)
			return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: "aborted"}, ctx.Err()
		}),
	})
	require.NoError(t, err)
	svc2 := &fakeService{}
	require.NoError(t, d2.RegisterHandlers(svc2))

	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc2, "start_node", daemon.StartNodeRequest{
		LeaseID:  "lease-abort",
		RunID:    "run-abort",
		NodeAddr: "worker.b",
		Epoch:    1,
		RepoURL:  "https://example.com/repo.git",
	})
	require.NoError(t, err)

	abortResp, err := invokeFakeHandler[daemon.AbortNodeRequest, daemon.AbortNodeResponse](t, svc2, "abort_node", daemon.AbortNodeRequest{
		RunID:    "run-abort",
		NodeAddr: "worker.b",
		Epoch:    1,
	})
	require.NoError(t, err)
	assert.True(t, abortResp.Aborted)

	select {
	case <-abortedChan:
	case <-time.After(2 * time.Second):
		t.Fatal("abort_node did not cancel in-flight node task")
	}

	require.Eventually(t, func() bool {
		return len(abortRep.Reports()) == 1
	}, 2*time.Second, 15*time.Millisecond)
	assert.True(t, abortRep.Reports()[0].Aborted)

	// Second abort is idempotent (Aborted: false, no error).
	abortDup, err := invokeFakeHandler[daemon.AbortNodeRequest, daemon.AbortNodeResponse](t, svc2, "abort_node", daemon.AbortNodeRequest{
		RunID:    "run-abort",
		NodeAddr: "worker.b",
		Epoch:    1,
	})
	require.NoError(t, err)
	assert.False(t, abortDup.Aborted)

	// Even when aborted, JSONL exit log is archived.
	abortLogKey := daemon.NodeLogBlobKey("run-abort", "worker.b", 1)
	require.Eventually(t, func() bool {
		_, statErr := store.Stat(context.Background(), abortLogKey)
		return statErr == nil
	}, 2*time.Second, 15*time.Millisecond)
}

func TestWorkerDaemon_RunMergeCandidateAndSelfFenceRotation(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistryClient{}
	mergeRep := &fakeMergeReporter{}
	store, err := blob.NewLocalStore(t.TempDir())
	require.NoError(t, err)

	var (
		fenceSignal   = make(chan string, 1)
		taskCancelled = make(chan struct{})
		fenceOnce     sync.Once
	)

	reg.onHeartbeat = func(req registry.HeartbeatReq) registry.HeartbeatResp {
		// Force DEAD on first heartbeat for proc-initial, healthy thereafter.
		if req.ProcessID == "proc-initial" {
			return registry.HeartbeatResp{Status: registry.StatusDead, FenceRequired: true}
		}
		return registry.HeartbeatResp{Status: registry.StatusHealthy, FenceRequired: false}
	}

	d, err := daemon.New(daemon.Options{
		ProcessID:         "proc-initial",
		AppID:             "sofa-worker-proc-initial",
		DevMode:           false,
		MaxConcurrency:    registry.DefaultWorkerMaxConcurrency,
		Arch:              "linux/amd64",
		Registry:          reg,
		BlobStore:         store,
		MergeReporter:     mergeRep,
		HeartbeatInterval: 25 * time.Millisecond,
		OnFence: func(_ context.Context, deadPID string) {
			fenceOnce.Do(func() {
				fenceSignal <- deadPID
			})
		},
		NodeExecutor: funcNodeExecutor(func(ctx context.Context, _ runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			<-ctx.Done()
			close(taskCancelled)
			return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: "fenced"}, ctx.Err()
		}),
		MergeExecutor: funcMergeExecutor(func(_ context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			return runner.MergeCandidateResult{
				Status:          runner.MergeStatusSucceeded,
				TargetBranch:    "main",
				LatestTargetSHA: strings.Repeat("b", 40),
				MergedCommitSHA: strings.Repeat("b", 40),
			}, nil
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	// Execute run_merge_candidate asynchronously and verify MergeOutcomeReport + ReleaseSlot.
	mergeResp, err := invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svc, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-initial",
		RequestID:       "mreq-1",
		LeaseID:         "lease-merge-1",
		LeaseKey:        "merge/m1/e1",
		RunID:           "run-merge-1",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-merge-1/w1",
	})
	require.NoError(t, err)
	assert.True(t, mergeResp.Accepted)

	require.Eventually(t, func() bool {
		return len(mergeRep.Reports()) == 1 && len(reg.Releases()) == 1
	}, 2*time.Second, 15*time.Millisecond)
	assert.Equal(t, runner.MergeStatusSucceeded, mergeRep.Reports()[0].Result.Status)
	assert.False(t, mergeRep.Reports()[0].Aborted)
	assert.Equal(t, "lease-merge-1", reg.Releases()[0].LeaseID)

	// Start a blocking node task, then start daemon heartbeat loop -> must fence, cancel task, call OnFence, rotate ProcessID & re-register!
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID: "proc-initial",
		LeaseID:   "lease-fence-1",
		RunID:     "run-fence-1",
		NodeAddr:  "worker.a",
		Epoch:     1,
		RepoURL:   "https://example.com/repo.git",
	})
	require.NoError(t, err)

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	require.NoError(t, d.Start(runCtx))
	defer func() {
		_ = d.Stop(context.Background())
	}()

	select {
	case deadPID := <-fenceSignal:
		assert.Equal(t, "proc-initial", deadPID)
	case <-time.After(3 * time.Second):
		t.Fatal("expected OnFence callback when heartbeat returned DEAD")
	}

	select {
	case <-taskCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected active task context to be cancelled on self-fence")
	}

	require.Eventually(t, func() bool {
		return len(reg.Registrations()) >= 2
	}, 2*time.Second, 15*time.Millisecond)

	regs := reg.Registrations()
	assert.Equal(t, "proc-initial", regs[0].ProcessID)
	assert.NotEqual(t, "proc-initial", regs[1].ProcessID)
	assert.Equal(t, regs[1].ProcessID, d.ProcessID())
	assert.Equal(t, "sofa-worker-"+regs[1].ProcessID, d.AppID())

	// Old ProcessID ping must now fail, new ProcessID ping must succeed.
	_, err = invokeFakeHandler[daemon.PingRequest, daemon.PingResponse](t, svc, "ping", daemon.PingRequest{ProcessID: "proc-initial"})
	require.Error(t, err)

	pingNew, err := invokeFakeHandler[daemon.PingRequest, daemon.PingResponse](t, svc, "ping", daemon.PingRequest{ProcessID: regs[1].ProcessID})
	require.NoError(t, err)
	assert.True(t, pingNew.OK)
	assert.Equal(t, regs[1].ProcessID, pingNew.ProcessID)

	// Verify DevMode keeps fixed AppID on rotation.
	devReg := &fakeRegistryClient{
		onHeartbeat: func(req registry.HeartbeatReq) registry.HeartbeatResp {
			if req.ProcessID == "dev-pid-1" {
				return registry.HeartbeatResp{Status: registry.StatusDead, FenceRequired: true}
			}
			return registry.HeartbeatResp{Status: registry.StatusHealthy}
		},
	}
	dDev, err := daemon.New(daemon.Options{
		ProcessID:         "dev-pid-1",
		AppID:             "sofa-dev",
		DevMode:           true,
		MaxConcurrency:    registry.DefaultDevMaxConcurrency,
		Registry:          devReg,
		HeartbeatInterval: 25 * time.Millisecond,
	})
	require.NoError(t, err)
	devCtx, cancelDev := context.WithCancel(context.Background())
	defer cancelDev()
	require.NoError(t, dDev.Start(devCtx))
	defer func() {
		_ = dDev.Stop(context.Background())
	}()

	require.Eventually(t, func() bool {
		return len(devReg.Registrations()) >= 2
	}, 2*time.Second, 15*time.Millisecond)
	devRegs := devReg.Registrations()
	assert.NotEqual(t, "dev-pid-1", devRegs[1].ProcessID)
	assert.Equal(t, "sofa-dev", devRegs[1].AppID, "dev mode must preserve fixed AppID across ProcessID rotation")
}

func TestWorkerDaemon_AbortMergeIdempotentAndFlagged(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistryClient{}
	mergeRep := &fakeMergeReporter{}
	mergeCancelled := make(chan struct{})

	d, err := daemon.New(daemon.Options{
		ProcessID:     "proc-merge-abort",
		AppID:         "sofa-worker-proc-merge-abort",
		Registry:      reg,
		MergeReporter: mergeRep,
		MergeExecutor: funcMergeExecutor(func(ctx context.Context, _ runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			<-ctx.Done()
			close(mergeCancelled)
			return runner.MergeCandidateResult{}, ctx.Err()
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	startResp, err := invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svc, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-merge-abort",
		RequestID:       "mreq-abort-1",
		LeaseID:         "lease-m-abort",
		RunID:           "run-m-abort",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-m-abort/w1",
	})
	require.NoError(t, err)
	assert.True(t, startResp.Accepted)

	abortResp, err := invokeFakeHandler[daemon.AbortMergeRequest, daemon.AbortMergeResponse](t, svc, "abort_merge", daemon.AbortMergeRequest{
		RequestID: "mreq-abort-1",
	})
	require.NoError(t, err)
	assert.True(t, abortResp.Aborted)

	select {
	case <-mergeCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("abort_merge did not cancel in-flight merge candidate")
	}

	require.Eventually(t, func() bool {
		return len(mergeRep.Reports()) == 1 && len(reg.Releases()) == 1
	}, 2*time.Second, 15*time.Millisecond)
	assert.True(t, mergeRep.Reports()[0].Aborted)
	assert.Equal(t, "mreq-abort-1", mergeRep.Reports()[0].RequestID)
	assert.Equal(t, "lease-m-abort", reg.Releases()[0].LeaseID)

	// Second abort_merge is idempotent.
	abortDup, err := invokeFakeHandler[daemon.AbortMergeRequest, daemon.AbortMergeResponse](t, svc, "abort_merge", daemon.AbortMergeRequest{
		RequestID: "mreq-abort-1",
	})
	require.NoError(t, err)
	assert.False(t, abortDup.Aborted)
}

func TestWorkerDaemon_RunMergeCandidateIsolatedGitEnv(t *testing.T) {
	t.Setenv("HOST_GIT_TOKEN", "host-secret-must-not-leak")

	reg := &fakeRegistryClient{}
	mergeRep := &fakeMergeReporter{}

	var (
		hostOK      bool
		hostVal     string
		managedOK   bool
		managedVal  string
		adapterUsed bool
	)

	d, err := daemon.New(daemon.Options{
		ProcessID:     "proc-merge-env",
		AppID:         "sofa-worker-proc-merge-env",
		Registry:      reg,
		MergeReporter: mergeRep,
		MergeExecutor: funcMergeExecutor(func(_ context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			cli, ok := req.GitAdapter.(*git.CLIAdapter)
			require.True(t, ok, "expected default git.CLIAdapter when Options.GitAdapter is nil")
			require.NotNil(t, cli.LookupEnv, "LookupEnv on default git.CLIAdapter must never be nil")
			adapterUsed = true
			hostVal, hostOK = cli.LookupEnv("HOST_GIT_TOKEN")
			managedVal, managedOK = cli.LookupEnv("MANAGED_GIT_TOKEN")
			return runner.MergeCandidateResult{
				Status:          runner.MergeStatusSucceeded,
				TargetBranch:    "main",
				LatestTargetSHA: strings.Repeat("c", 40),
				MergedCommitSHA: strings.Repeat("c", 40),
			}, nil
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	tokRef := "MANAGED_GIT_TOKEN"
	resp, err := invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svc, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-merge-env",
		RequestID:       "mreq-env-1",
		LeaseID:         "lease-m-env",
		RunID:           "run-m-env",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-m-env/w1",
		Auth:            git.AuthConfig{TokenRef: &tokRef},
		ResolvedEnv: map[string]string{
			"MANAGED_GIT_TOKEN": "managed-tok-val",
		},
	})
	require.NoError(t, err)
	assert.True(t, resp.Accepted)

	require.Eventually(t, func() bool {
		return len(mergeRep.Reports()) == 1
	}, 2*time.Second, 15*time.Millisecond)
	assert.True(t, adapterUsed)
	assert.False(t, hostOK, "host os.Environ secret must not leak into merge GitAdapter")
	assert.Empty(t, hostVal)
	assert.True(t, managedOK)
	assert.Equal(t, "managed-tok-val", managedVal)
}

func TestWorkerDaemon_UsesWorkerOptionsPluginDirTempDirAndHostOS(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistryClient{}
	nodeRep := &fakeNodeReporter{}
	mergeRep := &fakeMergeReporter{}

	var (
		mu        sync.Mutex
		nodeReqs  []runner.NodeExecutionRequest
		mergeReqs []runner.MergeCandidateRequest
	)

	d, err := daemon.New(daemon.Options{
		ProcessID:     "proc-defaults",
		AppID:         "sofa-worker-proc-defaults",
		Registry:      reg,
		NodeReporter:  nodeRep,
		MergeReporter: mergeRep,
		PluginDir:     "/opt/sofa/plugins",
		TempDir:       "/var/tmp/sofa-worker",
		NodeExecutor: funcNodeExecutor(func(_ context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			mu.Lock()
			nodeReqs = append(nodeReqs, req)
			mu.Unlock()
			return runner.NodeOutcome{Status: runner.StatusSucceeded}, nil
		}),
		MergeExecutor: funcMergeExecutor(func(_ context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			mu.Lock()
			mergeReqs = append(mergeReqs, req)
			mu.Unlock()
			return runner.MergeCandidateResult{
				Status:          runner.MergeStatusSucceeded,
				TargetBranch:    "main",
				LatestTargetSHA: strings.Repeat("d", 40),
				MergedCommitSHA: strings.Repeat("d", 40),
			}, nil
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID: "proc-defaults",
		LeaseID:   "lease-1",
		RunID:     "run-def",
		NodeAddr:  "worker.a",
		Epoch:     1,
		RepoURL:   "https://example.com/repo.git",
	})
	require.NoError(t, err)

	_, err = invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svc, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-defaults",
		RequestID:       "mreq-def-1",
		LeaseID:         "lease-m-1",
		RunID:           "run-def",
		MergeNodeAddr:   "merge.int",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-def/worker.a",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 1 && len(mergeRep.Reports()) == 1
	}, 2*time.Second, 15*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, nodeReqs, 1)
	require.Len(t, mergeReqs, 1)
	assert.Equal(t, "/opt/sofa/plugins", nodeReqs[0].PluginResolveOpts.PluginDir)
	assert.Equal(t, "/var/tmp/sofa-worker", nodeReqs[0].TempRoot)
	assert.Equal(t, runtime.GOOS, nodeReqs[0].HostOS)

	assert.Equal(t, "/var/tmp/sofa-worker", mergeReqs[0].TempRoot)
	assert.Equal(t, runtime.GOOS, mergeReqs[0].HostOS)
}

type countingBlobStore struct {
	blob.Store
	getCalls atomic.Int32
	onGet    func(key string)
}

func (s *countingBlobStore) Get(ctx context.Context, key, expectedSHA256 string) (io.ReadCloser, blob.ObjectMeta, error) {
	s.getCalls.Add(1)
	if s.onGet != nil {
		s.onGet(key)
	}
	return s.Store.Get(ctx, key, expectedSHA256)
}

func buildDaemonTestPlanBlob(t *testing.T, store blob.Store, runID string, withLock bool) (blob.ObjectMeta, harness.PluginLockFile) {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "skills"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "policy.txt"), []byte("Never hallucinate\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "skills", "SKILL.md"), []byte("# skill\n"), 0o644))

	modDir := filepath.Join(t.TempDir(), "modules")
	require.NoError(t, os.MkdirAll(filepath.Join(modDir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "sub", "mod.txt"), []byte("sub module\n"), 0o644))

	var lockFile *harness.PluginLockFile
	if withLock {
		lf := harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins: []harness.PluginLockEntry{
				{
					Name:              "copilot",
					Source:            "github.com/example/sofa-harness-copilot",
					Version:           "0.1.0",
					VerifiedSignature: "none",
					Assets: map[string]harness.PluginAsset{
						"linux_amd64": {
							Filename: "sofa-harness-copilot_linux_amd64.zip",
							SHA256:   strings.Repeat("a", 64),
						},
					},
				},
			},
		}
		lockFile = &lf
	}

	childPlan, err := plan.New(plan.Args{
		Directory:  filepath.Join(modDir, "sub"),
		BundlePath: "modules/sub",
	})
	require.NoError(t, err)

	planned, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		PluginLock: lockFile,
		Nodes: []plan.NodeSpec{
			{
				Address:          "module.sub",
				CanonicalAddress: "module.sub",
				Kind:             "module",
				Module: &plan.ModuleSpec{
					Plan:             childPlan,
					Source:           "./modules/sub",
					CanonicalAddress: "module.sub",
					BundlePath:       "modules/sub",
				},
			},
		},
	})
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "plan.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, planned, plan.ArchiveSource{
		ConfigDir:  configDir,
		ModulesDir: modDir,
		PluginLock: lockFile,
	}))

	f, err := os.Open(archivePath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	meta, err := store.Put(context.Background(), "runs/"+runID+"/plan/plan.sofaplan", f, "")
	require.NoError(t, err)
	if lockFile == nil {
		return meta, harness.PluginLockFile{}
	}
	return meta, *lockFile
}

func TestWorkerDaemon_BundleSingleflightReuseSHAMismatchAndMerge(t *testing.T) {
	t.Parallel()

	localStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	meta, wantLock := buildDaemonTestPlanBlob(t, localStore, "run-bundle-1", true)

	getEntered := make(chan struct{})
	releaseGet := make(chan struct{})
	var getOnce sync.Once
	store := &countingBlobStore{
		Store: localStore,
		onGet: func(_ string) {
			getOnce.Do(func() {
				close(getEntered)
				<-releaseGet
			})
		},
	}

	workerTempDir := filepath.Join(t.TempDir(), "worker-tmp")
	workerPluginDir := filepath.Join(t.TempDir(), "worker-plugins")
	reg := &fakeRegistryClient{}
	nodeRep := &fakeNodeReporter{}
	mergeRep := &fakeMergeReporter{}

	var (
		mu        sync.Mutex
		nodeReqs  []runner.NodeExecutionRequest
		mergeReqs []runner.MergeCandidateRequest
	)

	d, err := daemon.New(daemon.Options{
		ProcessID:     "proc-bundle-1",
		AppID:         "sofa-worker-proc-bundle-1",
		Registry:      reg,
		BlobStore:     store,
		NodeReporter:  nodeRep,
		MergeReporter: mergeRep,
		TempDir:       workerTempDir,
		PluginDir:     workerPluginDir,
		NodeExecutor: funcNodeExecutor(func(_ context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			mu.Lock()
			nodeReqs = append(nodeReqs, req)
			mu.Unlock()
			return runner.NodeOutcome{Status: runner.StatusSucceeded}, nil
		}),
		MergeExecutor: funcMergeExecutor(func(_ context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			mu.Lock()
			mergeReqs = append(mergeReqs, req)
			mu.Unlock()
			return runner.MergeCandidateResult{
				Status:          runner.MergeStatusSucceeded,
				TargetBranch:    "main",
				LatestTargetSHA: strings.Repeat("e", 40),
				MergedCommitSHA: strings.Repeat("e", 40),
			}, nil
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	// Dispatch two concurrent start_node requests for the same run -> singleflight must invoke BlobStore.Get only once!
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-bundle-1",
		LeaseID:     "lease-sf-1",
		RunID:       "run-bundle-1",
		NodeAddr:    "worker.a",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
		BundlePath:  "config",
	})
	require.NoError(t, err)

	<-getEntered

	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-bundle-1",
		LeaseID:     "lease-sf-2",
		RunID:       "run-bundle-1",
		NodeAddr:    "module.sub.worker.b",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
		BundlePath:  "modules/sub",
	})
	require.NoError(t, err)

	close(releaseGet)

	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 2
	}, 5*time.Second, 15*time.Millisecond)
	assert.Equal(t, int32(1), store.getCalls.Load(), "concurrent tasks of same run must share one BlobStore download via singleflight")

	// Third task (worker.c) + merge task must reuse extracted bundle without calling BlobStore.Get again.
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-bundle-1",
		LeaseID:     "lease-sf-3",
		RunID:       "run-bundle-1",
		NodeAddr:    "worker.c",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
		BundlePath:  "config",
	})
	require.NoError(t, err)

	_, err = invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svc, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-bundle-1",
		RequestID:       "mreq-bundle-1",
		LeaseID:         "lease-m-1",
		RunID:           "run-bundle-1",
		MergeNodeAddr:   "merge.final",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-bundle-1/worker.a",
		PlanBlobKey:     meta.Key,
		PlanSHA256:      meta.SHA256,
		BundlePath:      "config",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 3 && len(mergeRep.Reports()) == 1
	}, 5*time.Second, 15*time.Millisecond)
	assert.Equal(t, int32(1), store.getCalls.Load(), "subsequent node and merge tasks must reuse extracted bundle")

	extractRoot := filepath.Join(workerTempDir, "bundles", "run-bundle-1")
	mu.Lock()
	byNode := make(map[string]runner.NodeExecutionRequest, len(nodeReqs))
	for _, r := range nodeReqs {
		byNode[r.NodeAddr] = r
	}
	require.Len(t, mergeReqs, 1)
	mReq := mergeReqs[0]
	mu.Unlock()

	assert.Equal(t, extractRoot, byNode["worker.a"].BundleDir)
	assert.Equal(t, extractRoot, byNode["module.sub.worker.b"].BundleDir)
	assert.Equal(t, extractRoot, byNode["worker.c"].BundleDir)
	assert.Equal(t, runtime.GOOS, byNode["worker.a"].HostOS)
	assert.Equal(t, workerTempDir, byNode["worker.a"].TempRoot)
	assert.Equal(t, workerPluginDir, byNode["worker.a"].PluginResolveOpts.PluginDir)
	assert.Equal(t, wantLock, byNode["worker.a"].PluginResolveOpts.LockFile)

	assert.Equal(t, extractRoot, mReq.BundleDir)
	assert.Equal(t, runtime.GOOS, mReq.HostOS)
	assert.Equal(t, workerTempDir, mReq.TempRoot)

	// Invalid bundle_path must still be rejected even though extractRoot is passed directly.
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-bundle-1",
		LeaseID:     "lease-bad-bp",
		RunID:       "run-bundle-1",
		NodeAddr:    "worker.bad_bp",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
		BundlePath:  "../escape",
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 4
	}, 5*time.Second, 15*time.Millisecond)
	assert.Equal(t, runner.StatusFailed, nodeRep.Reports()[3].Outcome.Status)
	assert.Contains(t, nodeRep.Reports()[3].ErrMessage, "bundle_path")

	// Task with mismatched SHA against existing marker must fail clearly without re-downloading.
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-bundle-1",
		LeaseID:     "lease-bad-sha",
		RunID:       "run-bundle-1",
		NodeAddr:    "worker.bad_sha",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  strings.Repeat("f", 64),
		BundlePath:  "config",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 5
	}, 5*time.Second, 15*time.Millisecond)
	badReport := nodeRep.Reports()[4]
	assert.Equal(t, runner.StatusFailed, badReport.Outcome.Status)
	assert.NotEmpty(t, badReport.ErrMessage)
	assert.Equal(t, int32(1), store.getCalls.Load())

	// Fresh run with mismatched SHA against blob must fail clearly and not leave a bundle marker.
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-bundle-1",
		LeaseID:     "lease-fresh-bad-sha",
		RunID:       "run-bundle-bad-fresh",
		NodeAddr:    "worker.bad_fresh",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  strings.Repeat("e", 64),
		BundlePath:  "config",
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 6
	}, 5*time.Second, 15*time.Millisecond)
	assert.Equal(t, runner.StatusFailed, nodeRep.Reports()[5].Outcome.Status)
	assert.NotEmpty(t, nodeRep.Reports()[5].ErrMessage)
	_, freshBadStat := os.Stat(filepath.Join(workerTempDir, "bundles", "run-bundle-bad-fresh"))
	assert.ErrorIs(t, freshBadStat, os.ErrNotExist)
}

func TestWorkerDaemon_CleanupRun(t *testing.T) {
	t.Parallel()

	localStore, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	meta, _ := buildDaemonTestPlanBlob(t, localStore, "run-cleanup-1", false)

	workerTempDir := filepath.Join(t.TempDir(), "worker-tmp")
	reg := &fakeRegistryClient{}
	nodeRep := &fakeNodeReporter{}
	mergeRep := &fakeMergeReporter{}

	nodeStarted := make(chan struct{})
	nodeRelease := make(chan struct{})
	mergeStarted := make(chan struct{})
	mergeRelease := make(chan struct{})

	d, err := daemon.New(daemon.Options{
		ProcessID:     "proc-cleanup-1",
		AppID:         "sofa-worker-proc-cleanup-1",
		Registry:      reg,
		BlobStore:     localStore,
		NodeReporter:  nodeRep,
		MergeReporter: mergeRep,
		TempDir:       workerTempDir,
		NodeExecutor: funcNodeExecutor(func(_ context.Context, _ runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			close(nodeStarted)
			<-nodeRelease
			return runner.NodeOutcome{Status: runner.StatusSucceeded}, nil
		}),
		MergeExecutor: funcMergeExecutor(func(_ context.Context, _ runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			close(mergeStarted)
			<-mergeRelease
			return runner.MergeCandidateResult{
				Status:          runner.MergeStatusSucceeded,
				TargetBranch:    "main",
				LatestTargetSHA: strings.Repeat("1", 40),
				MergedCommitSHA: strings.Repeat("1", 40),
			}, nil
		}),
	})
	require.NoError(t, err)

	svc := &fakeService{}
	require.NoError(t, d.RegisterHandlers(svc))

	// Unknown run returns removed: false without error.
	unknownResp, err := invokeFakeHandler[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, svc, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-does-not-exist",
	})
	require.NoError(t, err)
	assert.False(t, unknownResp.Removed)
	assert.False(t, unknownResp.Busy)

	// Start blocking node task -> cleanup_run must refuse with busy: true.
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svc, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-cleanup-1",
		LeaseID:     "lease-c-1",
		RunID:       "run-cleanup-1",
		NodeAddr:    "worker.a",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: meta.Key,
		PlanSHA256:  meta.SHA256,
		BundlePath:  "config",
	})
	require.NoError(t, err)
	<-nodeStarted

	busyNodeResp, err := invokeFakeHandler[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, svc, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-cleanup-1",
	})
	require.NoError(t, err)
	assert.False(t, busyNodeResp.Removed)
	assert.True(t, busyNodeResp.Busy)

	close(nodeRelease)
	require.Eventually(t, func() bool {
		return len(nodeRep.Reports()) == 1
	}, 2*time.Second, 15*time.Millisecond)

	// Bundle directory MUST NOT be removed when task finishes!
	extractRoot := filepath.Join(workerTempDir, "bundles", "run-cleanup-1")
	_, statErr := os.Stat(extractRoot)
	require.NoError(t, statErr, "bundle directory must persist after node task finishes")

	// Start blocking merge task -> cleanup_run must refuse with busy: true.
	_, err = invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svc, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-cleanup-1",
		RequestID:       "mreq-c-1",
		LeaseID:         "lease-cm-1",
		RunID:           "run-cleanup-1",
		MergeNodeAddr:   "merge.final",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-cleanup-1/worker.a",
		PlanBlobKey:     meta.Key,
		PlanSHA256:      meta.SHA256,
		BundlePath:      "config",
	})
	require.NoError(t, err)
	<-mergeStarted

	busyMergeResp, err := invokeFakeHandler[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, svc, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-cleanup-1",
	})
	require.NoError(t, err)
	assert.False(t, busyMergeResp.Removed)
	assert.True(t, busyMergeResp.Busy)

	close(mergeRelease)
	require.Eventually(t, func() bool {
		return len(mergeRep.Reports()) == 1
	}, 2*time.Second, 15*time.Millisecond)

	// Idle run cleanup removes directory and returns removed: true.
	cleanResp, err := invokeFakeHandler[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, svc, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-cleanup-1",
	})
	require.NoError(t, err)
	assert.True(t, cleanResp.Removed)
	assert.False(t, cleanResp.Busy)

	_, statAfterErr := os.Stat(extractRoot)
	assert.ErrorIs(t, statAfterErr, os.ErrNotExist)

	// Abort + slow teardown: abort_node deletes from nodeTasks immediately, but executor goroutine
	// is still blocked winding down beyond cleanup_run's 50ms context -> must return busy: true and keep dir.
	slowMeta, _ := buildDaemonTestPlanBlob(t, localStore, "run-abort-cleanup-slow", false)
	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	defer func() {
		select {
		case <-slowRelease:
		default:
			close(slowRelease)
		}
	}()

	fastMeta, _ := buildDaemonTestPlanBlob(t, localStore, "run-abort-cleanup-fast", false)
	fastStarted := make(chan struct{})
	fastMergeMeta, _ := buildDaemonTestPlanBlob(t, localStore, "run-abort-merge-fast", false)
	fastMergeStarted := make(chan struct{})

	dAbort, err := daemon.New(daemon.Options{
		ProcessID:     "proc-cleanup-abort",
		AppID:         "sofa-worker-proc-cleanup-abort",
		Registry:      reg,
		BlobStore:     localStore,
		NodeReporter:  nodeRep,
		MergeReporter: mergeRep,
		TempDir:       workerTempDir,
		NodeExecutor: funcNodeExecutor(func(ctx context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			switch req.RunID {
			case "run-abort-cleanup-slow":
				close(slowStarted)
				<-slowRelease
				return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: "aborted slow"}, nil
			case "run-abort-cleanup-fast":
				close(fastStarted)
				<-ctx.Done()
				time.Sleep(30 * time.Millisecond)
				return runner.NodeOutcome{Status: runner.StatusFailed, FailureReason: "aborted fast"}, ctx.Err()
			default:
				return runner.NodeOutcome{Status: runner.StatusSucceeded}, nil
			}
		}),
		MergeExecutor: funcMergeExecutor(func(ctx context.Context, req runner.MergeCandidateRequest) (runner.MergeCandidateResult, error) {
			if req.RunID == "run-abort-merge-fast" {
				close(fastMergeStarted)
				<-ctx.Done()
				time.Sleep(30 * time.Millisecond)
				return runner.MergeCandidateResult{}, ctx.Err()
			}
			return runner.MergeCandidateResult{Status: runner.MergeStatusSucceeded}, nil
		}),
	})
	require.NoError(t, err)
	defer func() {
		_ = dAbort.Stop(context.Background())
	}()
	svcAbort := &fakeService{}
	require.NoError(t, dAbort.RegisterHandlers(svcAbort))

	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svcAbort, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-cleanup-abort",
		LeaseID:     "lease-slow-1",
		RunID:       "run-abort-cleanup-slow",
		NodeAddr:    "worker.slow",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: slowMeta.Key,
		PlanSHA256:  slowMeta.SHA256,
		BundlePath:  "config",
	})
	require.NoError(t, err)
	<-slowStarted

	abortSlowResp, err := invokeFakeHandler[daemon.AbortNodeRequest, daemon.AbortNodeResponse](t, svcAbort, "abort_node", daemon.AbortNodeRequest{
		RunID:    "run-abort-cleanup-slow",
		NodeAddr: "worker.slow",
		Epoch:    1,
	})
	require.NoError(t, err)
	assert.True(t, abortSlowResp.Aborted)

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	slowCleanupResp, err := invokeFakeHandlerCtx[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, shortCtx, svcAbort, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-abort-cleanup-slow",
	})
	require.NoError(t, err)
	assert.False(t, slowCleanupResp.Removed)
	assert.True(t, slowCleanupResp.Busy, "cleanup_run must report busy:true while aborted goroutine is still winding down")

	slowExtractRoot := filepath.Join(workerTempDir, "bundles", "run-abort-cleanup-slow")
	_, slowStatErr := os.Stat(slowExtractRoot)
	require.NoError(t, slowStatErr, "bundle directory must NOT be deleted while aborted goroutine is still running")

	close(slowRelease)
	require.Eventually(t, func() bool {
		resp, cErr := invokeFakeHandler[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, svcAbort, "cleanup_run", daemon.CleanupRunRequest{
			RunID: "run-abort-cleanup-slow",
		})
		return cErr == nil && resp.Removed && !resp.Busy
	}, 2*time.Second, 15*time.Millisecond)

	// Abort + fast teardown (node): abort_node followed immediately by cleanup_run waits briefly for
	// the aborted goroutine to exit, then removes the bundle directory.
	_, err = invokeFakeHandler[daemon.StartNodeRequest, daemon.StartNodeResponse](t, svcAbort, "start_node", daemon.StartNodeRequest{
		ProcessID:   "proc-cleanup-abort",
		LeaseID:     "lease-fast-1",
		RunID:       "run-abort-cleanup-fast",
		NodeAddr:    "worker.fast",
		Epoch:       1,
		RepoURL:     "https://example.com/repo.git",
		PlanBlobKey: fastMeta.Key,
		PlanSHA256:  fastMeta.SHA256,
		BundlePath:  "config",
	})
	require.NoError(t, err)
	<-fastStarted

	abortFastResp, err := invokeFakeHandler[daemon.AbortNodeRequest, daemon.AbortNodeResponse](t, svcAbort, "abort_node", daemon.AbortNodeRequest{
		RunID:    "run-abort-cleanup-fast",
		NodeAddr: "worker.fast",
		Epoch:    1,
	})
	require.NoError(t, err)
	assert.True(t, abortFastResp.Aborted)

	fastCtx, fastCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer fastCancel()
	fastCleanupResp, err := invokeFakeHandlerCtx[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, fastCtx, svcAbort, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-abort-cleanup-fast",
	})
	require.NoError(t, err)
	assert.True(t, fastCleanupResp.Removed)
	assert.False(t, fastCleanupResp.Busy)
	_, fastStatErr := os.Stat(filepath.Join(workerTempDir, "bundles", "run-abort-cleanup-fast"))
	assert.ErrorIs(t, fastStatErr, os.ErrNotExist)

	// Abort + fast teardown (merge): abort_merge followed immediately by cleanup_run waits briefly and removes bundle.
	_, err = invokeFakeHandler[daemon.RunMergeCandidateRequest, daemon.RunMergeCandidateResponse](t, svcAbort, "run_merge_candidate", daemon.RunMergeCandidateRequest{
		ProcessID:       "proc-cleanup-abort",
		RequestID:       "mreq-fast-1",
		LeaseID:         "lease-fast-m-1",
		RunID:           "run-abort-merge-fast",
		MergeNodeAddr:   "merge.fast",
		Epoch:           1,
		RepoURL:         "https://example.com/repo.git",
		CandidateBranch: "sofa/work/run-abort-merge-fast/w1",
		PlanBlobKey:     fastMergeMeta.Key,
		PlanSHA256:      fastMergeMeta.SHA256,
		BundlePath:      "config",
	})
	require.NoError(t, err)
	<-fastMergeStarted

	abortFastMergeResp, err := invokeFakeHandler[daemon.AbortMergeRequest, daemon.AbortMergeResponse](t, svcAbort, "abort_merge", daemon.AbortMergeRequest{
		RequestID: "mreq-fast-1",
	})
	require.NoError(t, err)
	assert.True(t, abortFastMergeResp.Aborted)

	fastMergeCleanupResp, err := invokeFakeHandlerCtx[daemon.CleanupRunRequest, daemon.CleanupRunResponse](t, fastCtx, svcAbort, "cleanup_run", daemon.CleanupRunRequest{
		RunID: "run-abort-merge-fast",
	})
	require.NoError(t, err)
	assert.True(t, fastMergeCleanupResp.Removed)
	assert.False(t, fastMergeCleanupResp.Busy)
	_, fastMergeStatErr := os.Stat(filepath.Join(workerTempDir, "bundles", "run-abort-merge-fast"))
	assert.ErrorIs(t, fastMergeStatErr, os.ErrNotExist)
}
