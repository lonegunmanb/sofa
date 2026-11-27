//go:build integration

package merge_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type itHTTPServer struct {
	port  int
	svc   common.Service
	errCh chan error
}

func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func startHTTPService(t *testing.T, srv *itHTTPServer) {
	t.Helper()
	go func() {
		err := srv.svc.Start()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.errCh <- err
		}
		close(srv.errCh)
	}()
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", srv.port), 100*time.Millisecond)
		if err != nil {
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

func runGitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initBareRepoForMerge(t *testing.T) (bareDir, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")

	runGitCLI(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runGitCLI(t, seedDir, "init")
	runGitCLI(t, seedDir, "config", "user.name", "seed")
	runGitCLI(t, seedDir, "config", "user.email", "seed@sofa.local")
	runGitCLI(t, seedDir, "config", "commit.gpgsign", "false")

	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-base\nline2-base\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "initial")
	runGitCLI(t, seedDir, "branch", "-M", "main")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	baseSHA = runGitCLI(t, seedDir, "rev-parse", "HEAD")

	// Candidate W1: updates shared.txt line1 to w1 and adds feat1.txt.
	runGitCLI(t, seedDir, "checkout", "-b", "sofa/work/run-it-026/w1")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-w1\nline2-base\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "feat1.txt"), []byte("feat1-ok\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "w1 feature")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/sofa/work/run-it-026/w1")

	// Candidate W2: adds feat2.txt (non-conflicting with W1, passes Podman gatekeeper).
	runGitCLI(t, seedDir, "checkout", "main")
	runGitCLI(t, seedDir, "checkout", "-b", "sofa/work/run-it-026/w2")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "feat2.txt"), []byte("feat2-ok\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "w2 feature")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/sofa/work/run-it-026/w2")

	// Candidate W3: modifies shared.txt line1 differently -> guaranteed rebase conflict with W1!
	runGitCLI(t, seedDir, "checkout", "main")
	runGitCLI(t, seedDir, "checkout", "-b", "sofa/work/run-it-026/w3")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-w3-conflict\nline2-base\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "w3 conflicting change")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/sofa/work/run-it-026/w3")

	return bareDir, baseSHA
}

type recordingRunMergeNotifier struct {
	mu            sync.Mutex
	ledger        *git.Ledger
	branches      []merge.RegisterManagedBranchReq
	notifications []merge.MergeCandidateNotification
}

func (n *recordingRunMergeNotifier) RegisterManagedBranch(_ context.Context, req merge.RegisterManagedBranchReq) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.ledger.Register(req.RepoURL, req.Branch, req.BaseSHA, req.Kind, req.NodeAddr, req.Epoch); err != nil {
		return err
	}
	n.branches = append(n.branches, req)
	return nil
}

func (n *recordingRunMergeNotifier) OnMergeCandidateResult(_ context.Context, notice merge.MergeCandidateNotification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notifications = append(n.notifications, notice)
	return nil
}

func (n *recordingRunMergeNotifier) Notifications() []merge.MergeCandidateNotification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]merge.MergeCandidateNotification, len(n.notifications))
	copy(out, n.notifications)
	return out
}

func (n *recordingRunMergeNotifier) Branches() []merge.RegisterManagedBranchReq {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]merge.RegisterManagedBranchReq, len(n.branches))
	copy(out, n.branches)
	return out
}

type daprMergeResultReporter struct {
	client daprclient.Client
}

func (r *daprMergeResultReporter) ReportCandidateResult(ctx context.Context, report daemon.MergeOutcomeReport) error {
	stub := merge.NewClientStub(report.MergeActorID, r.client)
	_, err := stub.ReportCandidateResult(ctx, &report)
	return err
}

func TestMergeActor_RealDaprGitAndPodmanSerialIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	psBeforeOut, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	require.NoError(t, err, "podman must be available for Tier 2 integration test")
	psBefore := strings.TrimSpace(string(psBeforeOut))

	rootDir := t.TempDir()
	nrDBPath := filepath.Join(rootDir, "nameresolution.db")
	bareDir, baseSHA := initBareRepoForMerge(t)

	runID := "run-it-026"
	mergeNodeAddr := "merge.integrate"
	targetBranch, shared, err := merge.EffectiveTargetBranch(runID, mergeNodeAddr, nil)
	require.NoError(t, err)
	require.False(t, shared)

	ledger := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	runNotifier := &recordingRunMergeNotifier{ledger: ledger}

	// 1. Control-plane Dapr sidecar hosting ConfigActor, ProcessRegistryActor, and MergeActor.
	var cpClientRef atomic.Pointer[daprclient.Client]
	cpClientProvider := func() daprclient.Client {
		c := cpClientRef.Load()
		if c == nil {
			return nil
		}
		return *c
	}

	cpPort := reservePort(t)
	var cpExpectedToken atomic.Value
	cpExpectedToken.Store("")
	cpRouter := chi.NewRouter()
	cpRouter.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := cpExpectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	cpSvc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", cpPort), cpRouter)
	cpSvc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return config.NewConfigActor(cpClientProvider)
	})
	cpSvc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return registry.NewProcessRegistryActor(registry.Options{ClientProvider: cpClientProvider})
	})
	cpSvc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return merge.NewMergeActor(merge.Options{
			ClientProvider:    cpClientProvider,
			RunNotifier:       runNotifier,
			SlotRetryInterval: 100 * time.Millisecond,
		})
	})
	cpSrv := &itHTTPServer{port: cpPort, svc: cpSvc, errCh: make(chan error, 1)}
	startHTTPService(t, cpSrv)
	defer cpSrv.stop(t)

	mgrCP, err := daprhost.NewManager(daprhost.Options{
		StateDir:             filepath.Join(rootDir, "cp"),
		AppID:                "sofa-control-plane",
		AppPort:              cpPort,
		NameResolutionDBPath: nrDBPath,
	})
	require.NoError(t, err)
	cpExpectedToken.Store(mgrCP.AppAPIToken())
	cpClient, err := mgrCP.Start(ctx)
	require.NoError(t, err)
	cpClientRef.Store(&cpClient)
	defer func() {
		_ = mgrCP.Stop(context.Background())
	}()

	// 2. Worker Dapr sidecar hosting WorkerDaemon with default GitAdapter + PodmanEngine.
	wPort := reservePort(t)
	var wExpectedToken atomic.Value
	wExpectedToken.Store("")
	wRouter := chi.NewRouter()
	wRouter.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := wExpectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/dapr/config" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"entities":[]}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	wSvc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", wPort), wRouter)
	wSrv := &itHTTPServer{port: wPort, svc: wSvc, errCh: make(chan error, 1)}
	startHTTPService(t, wSrv)
	defer wSrv.stop(t)

	mgrW, err := daprhost.NewManager(daprhost.Options{
		StateDir:              filepath.Join(rootDir, "w1"),
		AppID:                 "sofa-worker-it-merge",
		AppPort:               wPort,
		ExternalSchedulerAddr: mgrCP.SchedulerAddress(),
		NameResolutionDBPath:  nrDBPath,
	})
	require.NoError(t, err)
	wExpectedToken.Store(mgrW.AppAPIToken())
	wClient, err := mgrW.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = mgrW.Stop(context.Background())
	}()

	workerDaemon, err := daemon.New(daemon.Options{
		ProcessID:         "proc-merge-it",
		AppID:             "sofa-worker-it-merge",
		FixedAppID:        "sofa-worker-it-merge",
		MaxConcurrency:    2,
		Arch:              "linux/amd64",
		Registry:          daemon.NewDaprRegistryClient(wClient),
		MergeReporter:     &daprMergeResultReporter{client: wClient},
		HeartbeatInterval: 250 * time.Millisecond,
		// Leave GitAdapter and SandboxEngine nil to exercise real isolated CLIAdapter + PodmanEngine!
	})
	require.NoError(t, err)
	require.NoError(t, workerDaemon.RegisterHandlers(wSvc))
	require.NoError(t, workerDaemon.Start(ctx))
	defer func() {
		_ = workerDaemon.Stop(context.Background())
	}()

	// 3. Submit W1, W2, and W3 concurrently to MergeActor with a real Podman alpine:latest gatekeeper.
	mergeStub, err := merge.NewClientStubForTarget(bareDir, targetBranch, cpClient)
	require.NoError(t, err)

	sb := sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"}
	gks := []workerspec.GatekeeperConfig{{
		Name:             "verify_tree",
		Command:          []string{"sh", "-c", "test -f shared.txt"},
		WorkingDir:       ".",
		Timeout:          "1m",
		ExpectExitCode:   0,
		RequireCleanTree: true,
	}}
	tempRoot := filepath.Join(rootDir, "merge-tmp")
	require.NoError(t, os.MkdirAll(tempRoot, 0o755))

	enqueue := func(src, candBranch string) error {
		resp, eErr := mergeStub.EnqueueCandidate(ctx, &merge.EnqueueCandidateReq{
			RunID:           runID,
			MergeNodeAddr:   mergeNodeAddr,
			MergeEpoch:      1,
			SourceNodeAddr:  src,
			SourceEpoch:     1,
			RepoURL:         bareDir,
			BasedSource:     "main",
			BaseSHA:         baseSHA,
			CandidateBranch: candBranch,
			RequiredArch:    "linux/amd64",
			Sandbox:         sb,
			Gatekeepers:     gks,
			TempRoot:        tempRoot,
		})
		if eErr != nil {
			return eErr
		}
		if !resp.Accepted {
			return errors.New("expected Accepted=true")
		}
		return nil
	}

	// Concurrently enqueue W1 and W2, then concurrently enqueue W3 while the first merge is in-flight
	// so W1+W2 merge linearly before W3 hits the rebase conflict and receives the post-W1+W2 LatestTargetSHA.
	var wg12 sync.WaitGroup
	errs12 := make(chan error, 2)
	for _, item := range []struct {
		src, branch string
	}{
		{"worker.w1", "sofa/work/run-it-026/w1"},
		{"worker.w2", "sofa/work/run-it-026/w2"},
	} {
		wg12.Add(1)
		go func(src, branch string) {
			defer wg12.Done()
			if e := enqueue(src, branch); e != nil {
				errs12 <- e
			}
		}(item.src, item.branch)
	}
	wg12.Wait()
	close(errs12)
	for e := range errs12 {
		require.NoError(t, e)
	}

	var wg3 sync.WaitGroup
	errs3 := make(chan error, 1)
	wg3.Add(1)
	go func() {
		defer wg3.Done()
		if e := enqueue("worker.w3", "sofa/work/run-it-026/w3"); e != nil {
			errs3 <- e
		}
	}()
	wg3.Wait()
	close(errs3)
	for e := range errs3 {
		require.NoError(t, e)
	}

	// Wait for all 3 candidates to finish and notify RunNotifier.
	require.Eventually(t, func() bool {
		return len(runNotifier.Notifications()) == 3
	}, 60*time.Second, 100*time.Millisecond)

	notifs := runNotifier.Notifications()
	var succeeded []merge.MergeCandidateNotification
	var reworked []merge.MergeCandidateNotification
	for _, n := range notifs {
		switch n.Result.Status {
		case runner.MergeStatusSucceeded:
			succeeded = append(succeeded, n)
		case runner.MergeStatusReworkSource:
			reworked = append(reworked, n)
		default:
			t.Fatalf("unexpected candidate notification status=%q err=%q", n.Result.Status, n.ErrorMessage)
		}
	}
	require.Len(t, succeeded, 2, "expected W1 and W2 to succeed")
	require.Len(t, reworked, 1, "expected W3 to be rejected for rework")

	assert.Equal(t, "worker.w3", reworked[0].SourceNodeAddr)
	assert.Equal(t, []string{"shared.txt"}, reworked[0].Result.ConflictFiles)
	assert.False(t, reworked[0].MergedIntoSharedBranch)

	latestAfterTwo := succeeded[1].Result.MergedCommitSHA
	require.NotEmpty(t, latestAfterTwo)
	assert.Equal(t, latestAfterTwo, reworked[0].Result.LatestTargetSHA,
		"rework candidate must receive exact LatestTargetSHA after the first 2 merges")

	// Verify MergeActor state and KindMergeBranch registration.
	st, err := mergeStub.GetState(ctx, &merge.GetMergeStateReq{})
	require.NoError(t, err)
	assert.Nil(t, st.InFlight)
	assert.Empty(t, st.Queue)
	assert.Equal(t, latestAfterTwo, st.LatestTargetSHA)

	regBranches := runNotifier.Branches()
	require.Len(t, regBranches, 1)
	assert.Equal(t, git.KindMergeBranch, regBranches[0].Kind)
	assert.Equal(t, targetBranch, regBranches[0].Branch)
	require.NoError(t, ledger.EnsureRegistered(bareDir, targetBranch))

	// Verify real bare Git remote targetBranch has clean linear history containing BOTH W1 and W2!
	cliGit := git.NewCLIAdapter(git.CLIAdapterOptions{})
	tipSHA, containsFirst, err := cliGit.BranchContainsSHA(ctx, bareDir, targetBranch, succeeded[0].Result.MergedCommitSHA, git.AuthConfig{})
	require.NoError(t, err)
	assert.Equal(t, latestAfterTwo, tipSHA)
	assert.True(t, containsFirst, "target branch must linearly contain first merged commit")

	logOut := runGitCLI(t, bareDir, "rev-list", "--first-parent", "--count", baseSHA+".."+tipSHA)
	assert.Equal(t, "2", logOut, "expected exactly 2 linear rebased commits on targetBranch")

	verifyWs := filepath.Join(t.TempDir(), "verify-target")
	require.NoError(t, cliGit.PrepareWorkspace(ctx, bareDir, targetBranch, tipSHA, verifyWs, git.AuthConfig{}))
	b1, err := os.ReadFile(filepath.Join(verifyWs, "feat1.txt"))
	require.NoError(t, err)
	assert.Equal(t, "feat1-ok\n", strings.ReplaceAll(string(b1), "\r\n", "\n"))
	b2, err := os.ReadFile(filepath.Join(verifyWs, "feat2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "feat2-ok\n", strings.ReplaceAll(string(b2), "\r\n", "\n"))
	bShared, err := os.ReadFile(filepath.Join(verifyWs, "shared.txt"))
	require.NoError(t, err)
	assert.Equal(t, "line1-w1\nline2-base\n", strings.ReplaceAll(string(bShared), "\r\n", "\n"))

	// Verify zero leftover Podman containers created by this test.
	psAfterOut, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, psBefore, strings.TrimSpace(string(psAfterOut)))
}
