package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	actorconfig "github.com/dapr/go-sdk/actor/config"
	"github.com/dapr/go-sdk/service/common"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/blob"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
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
	t.Helper()
	h, ok := svc.handlers[route]
	require.True(t, ok, "route %q not registered", route)
	data, err := json.Marshal(req)
	require.NoError(t, err)
	out, err := h(context.Background(), &common.InvocationEvent{
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
