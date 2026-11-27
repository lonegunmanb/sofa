//go:build integration

package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockNodeActor receives OnWorkerDead callbacks from ProcessRegistryActor over real Dapr Actor RPC.
type MockNodeActor struct {
	actor.ServerImplBaseCtx
	onDead func(actorID string, notice registry.WorkerDeadNotification)
}

func (a *MockNodeActor) Type() string { return "NodeActor" }

func (a *MockNodeActor) OnWorkerDead(_ context.Context, notice *registry.WorkerDeadNotification) (*struct{}, error) {
	if notice != nil && a.onDead != nil {
		a.onDead(a.ID(), *notice)
	}
	return &struct{}{}, nil
}

type itHTTPServer struct {
	port        int
	svc         common.Service
	errCh       chan error
	validTokens atomic.Int64
	badTokens   atomic.Int64
}

func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func startControlPlaneCallbackServer(
	t *testing.T,
	clientRef *atomic.Pointer[daprclient.Client],
	onDead func(actorID string, notice registry.WorkerDeadNotification),
) (*itHTTPServer, *atomic.Value) {
	t.Helper()
	port := reservePort(t)
	var expectedToken atomic.Value
	expectedToken.Store("")

	srv := &itHTTPServer{port: port, errCh: make(chan error, 1)}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := expectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				srv.badTokens.Add(1)
				http.Error(w, "unauthorized dapr-api-token", http.StatusUnauthorized)
				return
			}
			srv.validTokens.Add(1)
			next.ServeHTTP(w, r)
		})
	})

	svc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", port), router)
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return registry.NewProcessRegistryActor(registry.Options{
			ClientProvider: func() daprclient.Client {
				c := clientRef.Load()
				if c == nil {
					return nil
				}
				return *c
			},
			SuspectTimeout: 400 * time.Millisecond,
			ProbeInterval:  150 * time.Millisecond,
			DeadThreshold:  3,
			PingTimeout:    250 * time.Millisecond,
		})
	})
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return &MockNodeActor{onDead: onDead}
	})
	srv.svc = svc
	startHTTPService(t, srv, port)
	return srv, &expectedToken
}

func startWorkerCallbackServer(
	t *testing.T,
	blockPing *atomic.Bool,
	setup func(svc common.Service),
) (*itHTTPServer, *atomic.Value) {
	t.Helper()
	port := reservePort(t)
	var expectedToken atomic.Value
	expectedToken.Store("")

	srv := &itHTTPServer{port: port, errCh: make(chan error, 1)}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := expectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				srv.badTokens.Add(1)
				http.Error(w, "unauthorized dapr-api-token", http.StatusUnauthorized)
				return
			}
			srv.validTokens.Add(1)

			// Worker callback servers in this shared-process test must NOT advertise the process-global
			// Actor runtime's entities to their worker daprd sidecars.
			if r.URL.Path == "/dapr/config" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"entities":[]}`))
				return
			}
			if blockPing != nil && blockPing.Load() && r.URL.Path == "/ping" {
				http.Error(w, "simulated worker outage", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	svc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", port), router)
	if setup != nil {
		setup(svc)
	}
	srv.svc = svc
	startHTTPService(t, srv, port)
	return srv, &expectedToken
}

func startHTTPService(t *testing.T, srv *itHTTPServer, port int) {
	t.Helper()
	go func() {
		err := srv.svc.Start()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.errCh <- err
		}
		close(srv.errCh)
	}()

	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 15*time.Millisecond)
}

func (s *itHTTPServer) stop(t *testing.T) {
	t.Helper()
	if s.svc != nil {
		_ = s.svc.Stop()
	}
	select {
	case err := <-s.errCh:
		if err != nil {
			t.Logf("callback server exited with: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for callback server shutdown")
	}
}

func TestProcessRegistryAndWorkerDaemon_MultiDaprdSchedulingAndReminderFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rootDir := t.TempDir()
	nrDBPath := filepath.Join(rootDir, "nameresolution.db")
	blobStore, err := blob.NewLocalStore(filepath.Join(rootDir, "blobs"))
	require.NoError(t, err)

	var cpClientRef atomic.Pointer[daprclient.Client]

	var deadMu sync.Mutex
	type deadNoticeEntry struct {
		ActorID string
		Notice  registry.WorkerDeadNotification
	}
	var deadNotices []deadNoticeEntry

	cpSrv, cpTok := startControlPlaneCallbackServer(t, &cpClientRef, func(actorID string, notice registry.WorkerDeadNotification) {
		deadMu.Lock()
		defer deadMu.Unlock()
		deadNotices = append(deadNotices, deadNoticeEntry{ActorID: actorID, Notice: notice})
	})
	defer cpSrv.stop(t)

	mgrCP, err := daprhost.NewManager(daprhost.Options{
		StateDir:             filepath.Join(rootDir, "cp"),
		AppID:                "sofa-control-plane",
		AppPort:              cpSrv.port,
		NameResolutionDBPath: nrDBPath,
	})
	require.NoError(t, err)
	cpTok.Store(mgrCP.AppAPIToken())

	cpClient, err := mgrCP.Start(ctx)
	require.NoError(t, err)
	cpClientRef.Store(&cpClient)
	defer func() {
		_ = mgrCP.Stop(context.Background())
	}()

	// Worker 1 (healthy worker executing start_node + PubSub + BlobStore archival)
	var (
		w1BlockPing atomic.Bool
		pubMu       sync.Mutex
		pubEvents   []daemon.LogEvent
	)
	var w1Daemon *daemon.WorkerDaemon
	w1Srv, w1Tok := startWorkerCallbackServer(t, &w1BlockPing, func(svc common.Service) {
		require.NoError(t, svc.AddTopicEventHandler(&common.Subscription{
			PubsubName: daemon.PubSubComponentName,
			Topic:      daemon.EventsTopic,
			Route:      "/events/sofa",
		}, func(_ context.Context, e *common.TopicEvent) (bool, error) {
			var ev daemon.LogEvent
			if uErr := json.Unmarshal(e.RawData, &ev); uErr == nil {
				pubMu.Lock()
				pubEvents = append(pubEvents, ev)
				pubMu.Unlock()
			}
			return false, nil
		}))
	})
	defer w1Srv.stop(t)

	mgrW1, err := daprhost.NewManager(daprhost.Options{
		StateDir:              filepath.Join(rootDir, "w1"),
		AppID:                 "sofa-worker-it-1",
		AppPort:               w1Srv.port,
		ExternalSchedulerAddr: mgrCP.SchedulerAddress(),
		NameResolutionDBPath:  nrDBPath,
	})
	require.NoError(t, err)
	w1Tok.Store(mgrW1.AppAPIToken())

	w1Client, err := mgrW1.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = mgrW1.Stop(context.Background())
	}()

	var outcomeMu sync.Mutex
	var nodeOutcomes []daemon.NodeOutcomeReport
	w1Daemon, err = daemon.New(daemon.Options{
		ProcessID:         "proc-w1",
		AppID:             "sofa-worker-it-1",
		FixedAppID:        "sofa-worker-it-1",
		MaxConcurrency:    4,
		Arch:              "linux/amd64",
		Registry:          daemon.NewDaprRegistryClient(w1Client),
		PubSub:            daemon.NewDaprPubSubPublisher(w1Client),
		BlobStore:         blobStore,
		HeartbeatInterval: 200 * time.Millisecond,
		NodeExecutor: funcNodeExecutor(func(_ context.Context, req runner.NodeExecutionRequest) (runner.NodeOutcome, error) {
			if req.OnEvent != nil {
				req.OnEvent(harnessruntime.Event{
					EventID: "it-ev-1",
					Kind:    harnessruntime.EventAssistantMessageDelta,
					Text:    "integration worker event",
				})
				req.OnEvent(harnessruntime.Event{
					EventID:  "it-ev-2",
					Kind:     harnessruntime.EventToolExecutionComplete,
					ToolName: "sofa_checkpoint",
				})
			}
			return runner.NodeOutcome{
				Status:     runner.StatusSucceeded,
				WorkBranch: "sofa/work/run-it/step1",
				CommitSHA:  strings.Repeat("c", 40),
			}, nil
		}),
		NodeReporter: nodeReporterFunc(func(_ context.Context, r daemon.NodeOutcomeReport) error {
			outcomeMu.Lock()
			defer outcomeMu.Unlock()
			nodeOutcomes = append(nodeOutcomes, r)
			return nil
		}),
	})
	require.NoError(t, err)
	require.NoError(t, w1Daemon.RegisterHandlers(w1Srv.svc))
	require.NoError(t, w1Daemon.Start(ctx))
	defer func() {
		_ = w1Daemon.Stop(context.Background())
	}()

	// Worker 2 (will later fail ping and transition HEALTHY -> SUSPECT -> DEAD via real Dapr Reminder)
	var w2BlockPing atomic.Bool
	w2Srv, w2Tok := startWorkerCallbackServer(t, &w2BlockPing, nil)
	defer w2Srv.stop(t)

	mgrW2, err := daprhost.NewManager(daprhost.Options{
		StateDir:              filepath.Join(rootDir, "w2"),
		AppID:                 "sofa-worker-it-2",
		AppPort:               w2Srv.port,
		ExternalSchedulerAddr: mgrCP.SchedulerAddress(),
		NameResolutionDBPath:  nrDBPath,
	})
	require.NoError(t, err)
	w2Tok.Store(mgrW2.AppAPIToken())

	w2Client, err := mgrW2.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = mgrW2.Stop(context.Background())
	}()

	w2Daemon, err := daemon.New(daemon.Options{
		ProcessID:         "proc-w2",
		AppID:             "sofa-worker-it-2",
		FixedAppID:        "sofa-worker-it-2",
		MaxConcurrency:    4,
		Arch:              "linux/amd64",
		Registry:          daemon.NewDaprRegistryClient(w2Client),
		BlobStore:         blobStore,
		HeartbeatInterval: 10 * time.Second, // silent long enough for SuspectTimeout=400ms to fire
	})
	require.NoError(t, err)
	require.NoError(t, w2Daemon.RegisterHandlers(w2Srv.svc))
	require.NoError(t, w2Daemon.Start(ctx))
	defer func() {
		_ = w2Daemon.Stop(context.Background())
	}()

	regStub := registry.NewClientStub(cpClient)

	// Load w1 with 2 slots so w2 has 0/4 utilization (strictly lower than w1's 2/4),
	// OR first claim 2 slots on w1? Wait: both start at 0/4; let's claim 1 on each, then release w2's?
	// Simpler: claim 3 slots total; first verify both workers are registered, then claim a slot for w1 execution.
	listResp, err := regStub.ListProcesses(ctx, &registry.ListProcessesReq{})
	require.NoError(t, err)
	require.Len(t, listResp.Processes, 2)

	// Claim slot on least-loaded worker (both 0/4 -> p1 lexicographically "proc-w1").
	slot1, err := regStub.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-it/worker.step1/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-it__step1",
		RequiredArch:   "linux/amd64",
	})
	require.NoError(t, err)
	require.True(t, slot1.Granted)
	require.Equal(t, "proc-w1", slot1.ProcessID)

	// Dispatch start_node across real Dapr Service Invocation (mgrCP -> mgrW1 via SQLite name resolution)!
	startPayload, err := json.Marshal(daemon.StartNodeRequest{
		ProcessID: slot1.ProcessID,
		LeaseID:   slot1.LeaseID,
		LeaseKey:  slot1.LeaseKey,
		RunID:     "run-it",
		NodeAddr:  "worker.step1",
		Epoch:     1,
		RepoURL:   "https://example.com/repo.git",
	})
	require.NoError(t, err)

	rawStartResp, err := cpClient.InvokeMethodWithContent(ctx, slot1.AppID, "start_node", "POST", &daprclient.DataContent{
		ContentType: "application/json",
		Data:        startPayload,
	})
	require.NoError(t, err)
	var startResp daemon.StartNodeResponse
	require.NoError(t, json.Unmarshal(rawStartResp, &startResp))
	assert.True(t, startResp.Accepted)

	// Verify async completion, slot release, PubSub event delivery, and BlobStore JSONL log archival.
	require.Eventually(t, func() bool {
		outcomeMu.Lock()
		defer outcomeMu.Unlock()
		return len(nodeOutcomes) == 1
	}, 10*time.Second, 25*time.Millisecond)
	assert.Equal(t, runner.StatusSucceeded, nodeOutcomes[0].Outcome.Status)

	logKey := daemon.NodeLogBlobKey("run-it", "worker.step1", 1)
	require.Eventually(t, func() bool {
		_, statErr := blobStore.Stat(ctx, logKey)
		return statErr == nil
	}, 5*time.Second, 25*time.Millisecond)

	rc, _, err := blobStore.Get(ctx, logKey, "")
	require.NoError(t, err)
	logBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Contains(t, string(logBytes), "integration worker event")

	require.Eventually(t, func() bool {
		pubMu.Lock()
		defer pubMu.Unlock()
		return len(pubEvents) >= 2
	}, 5*time.Second, 25*time.Millisecond, "expected Dapr PubSub delivery of worker log events")

	// Wait until w1's slot is released so w1=0/4 and w2=0/4, then keep w1 busy with 1 slot so next ClaimSlot picks w2!
	require.Eventually(t, func() bool {
		lr, lErr := regStub.ListProcesses(ctx, &registry.ListProcessesReq{})
		if lErr != nil {
			return false
		}
		for _, p := range lr.Processes {
			if p.ProcessID == "proc-w1" {
				return p.ActiveSlots == 0
			}
		}
		return false
	}, 5*time.Second, 25*time.Millisecond)

	busyW1, err := regStub.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-it/busy-w1/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-it__busy-w1",
	})
	require.NoError(t, err)
	require.True(t, busyW1.Granted)
	require.Equal(t, "proc-w1", busyW1.ProcessID)

	// Next ClaimSlot must select proc-w2 (0/4 < 1/4)!
	slotW2, err := regStub.ClaimSlot(ctx, &registry.ClaimSlotReq{
		LeaseKey:       "run-it/victim-w2/e1",
		OwnerActorType: "NodeActor",
		OwnerActorID:   "run-it__victim-w2",
	})
	require.NoError(t, err)
	require.True(t, slotW2.Granted)
	require.Equal(t, "proc-w2", slotW2.ProcessID)

	// Block w2's ping endpoint (and stop w2's heartbeat) -> Dapr ReminderCall drives proc-w2:
	// HEALTHY -> SUSPECT (400ms) -> 3 failed cross-daprd pings (150ms each) -> DEAD + OnWorkerDead to NodeActor!
	w2BlockPing.Store(true)
	require.NoError(t, w2Daemon.Stop(ctx))

	require.Eventually(t, func() bool {
		deadMu.Lock()
		defer deadMu.Unlock()
		return len(deadNotices) >= 1
	}, 15*time.Second, 50*time.Millisecond, "expected real Dapr Reminder to drive proc-w2 DEAD and invoke NodeActor.OnWorkerDead")

	deadMu.Lock()
	gotDead := deadNotices
	deadMu.Unlock()
	assert.Equal(t, "run-it__victim-w2", gotDead[0].ActorID)
	assert.Equal(t, slotW2.LeaseID, gotDead[0].Notice.LeaseID)
	assert.Equal(t, slotW2.LeaseKey, gotDead[0].Notice.LeaseKey)
	assert.Equal(t, "proc-w2", gotDead[0].Notice.ProcessID)

	// Verify proc-w2 is persisted as DEAD with ActiveSlots=0.
	finalList, err := regStub.ListProcesses(ctx, &registry.ListProcessesReq{})
	require.NoError(t, err)
	for _, p := range finalList.Processes {
		if p.ProcessID == "proc-w2" {
			assert.Equal(t, registry.StatusDead, p.Status)
			assert.Equal(t, 0, p.ActiveSlots)
		}
	}
}

type nodeReporterFunc func(ctx context.Context, report daemon.NodeOutcomeReport) error

func (f nodeReporterFunc) ReportNodeOutcome(ctx context.Context, report daemon.NodeOutcomeReport) error {
	return f(ctx, report)
}
