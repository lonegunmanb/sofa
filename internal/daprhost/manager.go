package daprhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/dapr/dapr/pkg/proto/runtime/v1"
	"github.com/dapr/go-sdk/actor"
	actorstate "github.com/dapr/go-sdk/actor/state"
	daprclient "github.com/dapr/go-sdk/client"
)

// NewActorStateManager binds an actor instance's StateManagerContext to a specific
// Manager-issued Dapr client instead of the SDK's process-global default client.
func NewActorStateManager(actorType, actorID string, client daprclient.Client) actor.StateManagerContext {
	if client == nil {
		return nil
	}
	return actorstate.NewActorStateManagerContext(
		actorType,
		actorID,
		actorstate.NewDaprStateAsyncProvider(client),
	)
}

const (
	defaultReadinessTimeout = 30 * time.Second
	defaultStopTimeout      = 10 * time.Second
	maxOutputBufferBytes    = 64 * 1024
)

// Options configures a managed local Dapr runtime (scheduler + daprd).
type Options struct {
	StateDir              string // Required: persistent root directory
	AppID                 string // Required: Dapr app ID (e.g. "sofa-dev" or "sofa-worker-<id>")
	AppPort               int    // Local dapr/go-sdk callback port (127.0.0.1:<AppPort>)
	AppAPIToken           string // Optional: auto-generated 32-byte random hex token when empty
	DaprAPIToken          string // Optional: auto-generated 32-byte random hex token when empty
	SchedulerBin          string // Optional explicit path to scheduler binary
	DaprdBin              string // Optional explicit path to daprd binary
	ExternalSchedulerAddr string // Optional external scheduler host:port; skips embedded scheduler launch when non-empty
}

type managedProc struct {
	cmd     *exec.Cmd
	done    chan struct{}
	exitErr error
	output  *boundedBuffer
}

// Manager supervises local Dapr scheduler and daprd processes and issues a
// token-authenticated Dapr gRPC client.
type Manager struct {
	mu sync.Mutex

	stateDir              string
	appID                 string
	appPort               int
	appAPIToken           string
	daprAPIToken          string
	schedulerBin          string
	daprdBin              string
	externalSchedulerAddr string

	started          bool
	schedulerPort    int
	daprHTTPPort     int
	daprGRPCPort     int
	schedProc        *managedProc
	daprdProc        *managedProc
	client           daprclient.Client
	httpProbeClient  *http.Client
	readinessTimeout time.Duration
}

// NewManager validates options, resolves Dapr binaries, and prepares per-instance API tokens.
func NewManager(opts Options) (*Manager, error) {
	stateDir := strings.TrimSpace(opts.StateDir)
	if stateDir == "" {
		return nil, errors.New("daprhost: StateDir is required")
	}
	absStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("daprhost: resolve StateDir: %w", err)
	}

	appID := strings.TrimSpace(opts.AppID)
	if appID == "" {
		return nil, errors.New("daprhost: AppID is required")
	}
	if opts.AppPort < 0 || opts.AppPort > 65535 {
		return nil, fmt.Errorf("daprhost: invalid AppPort %d", opts.AppPort)
	}

	homeDir, _ := os.UserHomeDir()
	daprdBin, err := resolveBinary(opts.DaprdBin, "daprd", homeDir, exec.LookPath)
	if err != nil {
		return nil, fmt.Errorf("daprhost: resolve daprd binary: %w", err)
	}

	externalSched := strings.TrimSpace(opts.ExternalSchedulerAddr)
	var schedulerBin string
	if externalSched == "" {
		schedulerBin, err = resolveBinary(opts.SchedulerBin, "scheduler", homeDir, exec.LookPath)
		if err != nil {
			return nil, fmt.Errorf("daprhost: resolve scheduler binary: %w", err)
		}
	}

	appToken := opts.AppAPIToken
	if appToken == "" {
		appToken, err = randomHex32()
		if err != nil {
			return nil, fmt.Errorf("daprhost: generate AppAPIToken: %w", err)
		}
	}

	daprToken := opts.DaprAPIToken
	if daprToken == "" {
		daprToken, err = randomHex32()
		if err != nil {
			return nil, fmt.Errorf("daprhost: generate DaprAPIToken: %w", err)
		}
	}

	return &Manager{
		stateDir:              absStateDir,
		appID:                 appID,
		appPort:               opts.AppPort,
		appAPIToken:           appToken,
		daprAPIToken:          daprToken,
		schedulerBin:          schedulerBin,
		daprdBin:              daprdBin,
		externalSchedulerAddr: externalSched,
		httpProbeClient:       &http.Client{Timeout: 2 * time.Second},
		readinessTimeout:      defaultReadinessTimeout,
	}, nil
}

// AppAPIToken returns the per-instance token daprd sends to the application callback server.
func (m *Manager) AppAPIToken() string {
	return m.appAPIToken
}

// DaprAPIToken returns the per-instance token required when calling daprd APIs.
func (m *Manager) DaprAPIToken() string {
	return m.daprAPIToken
}

// GRPCAddress returns daprd's loopback gRPC address ("127.0.0.1:<port>") once started.
func (m *Manager) GRPCAddress() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.daprGRPCPort == 0 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(m.daprGRPCPort))
}

// HTTPAddress returns daprd's loopback HTTP address ("127.0.0.1:<port>") once started.
func (m *Manager) HTTPAddress() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.daprHTTPPort == 0 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(m.daprHTTPPort))
}

// HTTPPort returns daprd's allocated HTTP port (0 before Start).
func (m *Manager) HTTPPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.daprHTTPPort
}

// GRPCPort returns daprd's allocated gRPC port (0 before Start).
func (m *Manager) GRPCPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.daprGRPCPort
}

// SchedulerAddress returns the scheduler host:port used by daprd.
func (m *Manager) SchedulerAddress() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.externalSchedulerAddr != "" {
		return m.externalSchedulerAddr
	}
	if m.schedulerPort == 0 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(m.schedulerPort))
}

// Client returns the active Dapr client once Start succeeds, or nil when stopped.
func (m *Manager) Client() daprclient.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

// Start prepares keys/components, starts local scheduler (unless ExternalSchedulerAddr is set)
// and daprd, waits for health and actor placement readiness, and returns an authenticated Dapr client.
func (m *Manager) Start(ctx context.Context) (daprclient.Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return nil, errors.New("daprhost: manager already started")
	}

	keyPath, err := EnsureMasterKey(m.stateDir)
	if err != nil {
		return nil, err
	}
	componentsDir, err := WriteComponents(m.stateDir, keyPath)
	if err != nil {
		return nil, err
	}

	rollback := func() {
		_ = m.stopLocked(context.Background())
	}

	var schedAddr string
	if m.externalSchedulerAddr != "" {
		schedAddr = m.externalSchedulerAddr
	} else {
		schedDir := filepath.Join(m.stateDir, "scheduler")
		if err := os.MkdirAll(schedDir, 0o700); err != nil {
			return nil, fmt.Errorf("daprhost: create scheduler data dir: %w", err)
		}

		schedPorts, err := reserveLoopbackPorts(5) // grpc, healthz, metrics, etcd-client, etcd-peer
		if err != nil {
			return nil, fmt.Errorf("daprhost: allocate scheduler loopback ports: %w", err)
		}
		schedPort := schedPorts[0]
		schedHealthzPort := schedPorts[1]
		schedMetricsPort := schedPorts[2]
		etcdClientPort := schedPorts[3]
		etcdPeerPort := schedPorts[4]

		schedArgs := []string{
			"--mode=standalone",
			"--id=dapr-scheduler-server-0",
			"--etcd-embed=true",
			"--etcd-data-dir=" + schedDir,
			"--placement-enabled=true",
			"--listen-address=127.0.0.1",
			"--port=" + strconv.Itoa(schedPort),
			"--healthz-listen-address=127.0.0.1",
			"--healthz-port=" + strconv.Itoa(schedHealthzPort),
			"--metrics-listen-address=127.0.0.1",
			"--metrics-port=" + strconv.Itoa(schedMetricsPort),
			"--enable-metrics=false",
			"--etcd-client-listen-address=127.0.0.1",
			"--etcd-client-port=" + strconv.Itoa(etcdClientPort),
			"--etcd-initial-cluster=dapr-scheduler-server-0=http://127.0.0.1:" + strconv.Itoa(etcdPeerPort),
			"--log-level=info",
		}

		schedProc, err := startManagedProcess(m.schedulerBin, schedArgs, nil)
		if err != nil {
			return nil, fmt.Errorf("daprhost: start scheduler: %w", err)
		}
		m.schedProc = schedProc
		m.schedulerPort = schedPort
		schedAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(schedPort))

		healthzURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", schedHealthzPort)
		if err := m.waitForHTTPReady(ctx, healthzURL, schedProc, http.StatusOK); err != nil {
			rollback()
			return nil, fmt.Errorf("daprhost: scheduler readiness check failed: %w\noutput:\n%s", err, schedProc.output.String())
		}
	}

	daprPorts, err := reserveLoopbackPorts(5) // http, grpc, internal-grpc, public, metrics
	if err != nil {
		rollback()
		return nil, fmt.Errorf("daprhost: allocate daprd loopback ports: %w", err)
	}
	daprHTTPPort := daprPorts[0]
	daprGRPCPort := daprPorts[1]
	daprInternalGRPCPort := daprPorts[2]
	daprPublicPort := daprPorts[3]
	daprMetricsPort := daprPorts[4]

	daprdArgs := []string{
		"--mode=standalone",
		"--app-id=" + m.appID,
		"--dapr-listen-addresses=127.0.0.1",
		"--dapr-http-port=" + strconv.Itoa(daprHTTPPort),
		"--dapr-grpc-port=" + strconv.Itoa(daprGRPCPort),
		"--dapr-internal-grpc-listen-address=127.0.0.1",
		"--dapr-internal-grpc-port=" + strconv.Itoa(daprInternalGRPCPort),
		"--dapr-public-listen-address=127.0.0.1",
		"--dapr-public-port=" + strconv.Itoa(daprPublicPort),
		"--metrics-listen-address=127.0.0.1",
		"--metrics-port=" + strconv.Itoa(daprMetricsPort),
		"--enable-metrics=false",
		"--scheduler-host-address=" + schedAddr,
		"--resources-path=" + componentsDir,
		"--disable-builtin-k8s-secret-store=true",
		"--enable-mtls=false",
		"--dapr-graceful-shutdown-seconds=2",
		"--log-level=info",
	}
	if m.appPort > 0 {
		daprdArgs = append(daprdArgs,
			"--app-port="+strconv.Itoa(m.appPort),
			"--app-channel-address=127.0.0.1",
			"--app-protocol=http",
		)
	}

	childEnv := buildIsolatedEnv(os.Environ(), map[string]string{
		"APP_API_TOKEN":  m.appAPIToken,
		"DAPR_API_TOKEN": m.daprAPIToken,
	})
	daprdProc, err := startManagedProcess(m.daprdBin, daprdArgs, childEnv)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("daprhost: start daprd: %w", err)
	}
	m.daprdProc = daprdProc
	m.daprHTTPPort = daprHTTPPort
	m.daprGRPCPort = daprGRPCPort

	outboundURL := fmt.Sprintf("http://127.0.0.1:%d/v1.0/healthz/outbound", daprHTTPPort)
	if err := m.waitForHTTPReady(ctx, outboundURL, daprdProc, http.StatusNoContent, http.StatusOK); err != nil {
		rollback()
		return nil, fmt.Errorf("daprhost: daprd outbound readiness check failed: %w\noutput:\n%s", err, daprdProc.output.String())
	}

	grpcAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(daprGRPCPort))
	client, err := daprclient.NewClientWithAddressContext(ctx, grpcAddr)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("daprhost: connect Dapr gRPC client: %w\noutput:\n%s", err, daprdProc.output.String())
	}
	client.WithAuthToken(m.daprAPIToken)
	m.client = client

	if m.appPort > 0 {
		if err := m.waitForActorPlacementReady(ctx, client, daprdProc); err != nil {
			rollback()
			return nil, fmt.Errorf("daprhost: actor runtime readiness check failed: %w\noutput:\n%s", err, daprdProc.output.String())
		}
	} else {
		waitCtx, cancel := m.withReadinessDeadline(ctx)
		err := client.Wait(waitCtx, m.readinessTimeout)
		cancel()
		if err != nil {
			rollback()
			return nil, fmt.Errorf("daprhost: dapr gRPC readiness check failed: %w\noutput:\n%s", err, daprdProc.output.String())
		}
	}

	m.started = true
	return client, nil
}

// Stop gracefully closes the Dapr client, shuts down daprd and scheduler, and
// waits for both processes to exit so SQLite and etcd locks are released.
func (m *Manager) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked(ctx)
}

func (m *Manager) stopLocked(ctx context.Context) error {
	var errs []error

	if m.client != nil {
		shutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = m.client.Shutdown(shutCtx)
		cancel()
		m.client.Close()
		m.client = nil
	}

	if m.daprdProc != nil {
		if err := stopOrAwaitProcess(ctx, m.daprdProc, 5*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("stop daprd: %w", err))
		}
		m.daprdProc = nil
	}

	if m.schedProc != nil {
		if err := stopChildProcess(m.schedProc.cmd, m.schedProc.done, 3*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("stop scheduler: %w", err))
		}
		m.schedProc = nil
	}

	m.started = false
	m.schedulerPort = 0
	m.daprHTTPPort = 0
	m.daprGRPCPort = 0

	return errors.Join(errs...)
}

func stopOrAwaitProcess(ctx context.Context, proc *managedProc, grace time.Duration) error {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	select {
	case <-proc.done:
		return nil
	case <-waitCtx.Done():
		return stopChildProcess(proc.cmd, proc.done, 2*time.Second)
	}
}

func (m *Manager) waitForHTTPReady(ctx context.Context, url string, proc *managedProc, acceptedStatuses ...int) error {
	waitCtx, cancel := m.withReadinessDeadline(ctx)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-proc.done:
			return fmt.Errorf("process exited prematurely (%v)", proc.exitErr)
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for %s: %w", url, lastErr)
			}
			return fmt.Errorf("timed out waiting for %s: %w", url, waitCtx.Err())
		case <-ticker.C:
			reqCtx, reqCancel := context.WithTimeout(waitCtx, time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
			if err != nil {
				reqCancel()
				return err
			}
			resp, err := m.httpProbeClient.Do(req)
			reqCancel()
			if err != nil {
				lastErr = err
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			for _, code := range acceptedStatuses {
				if resp.StatusCode == code {
					return nil
				}
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
	}
}

func (m *Manager) waitForActorPlacementReady(ctx context.Context, client daprclient.Client, proc *managedProc) error {
	waitCtx, cancel := m.withReadinessDeadline(ctx)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	var lastState string
	var lastErr error
	for {
		select {
		case <-proc.done:
			return fmt.Errorf("daprd exited prematurely (%v)", proc.exitErr)
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for actor runtime placement (%s): %w", lastState, lastErr)
			}
			return fmt.Errorf("timed out waiting for actor runtime placement (%s): %w", lastState, waitCtx.Err())
		case <-ticker.C:
			callCtx, callCancel := context.WithTimeout(waitCtx, time.Second)
			resp, err := client.GrpcClient().GetMetadata(callCtx, &pb.GetMetadataRequest{})
			callCancel()
			if err != nil {
				lastErr = err
				continue
			}
			ar := resp.GetActorRuntime()
			if ar == nil {
				lastState = "actorRuntime=nil"
				continue
			}
			lastState = fmt.Sprintf("status=%s hostReady=%v placement=%q", ar.GetRuntimeStatus().String(), ar.GetHostReady(), ar.GetPlacement())
			if ar.GetRuntimeStatus() != pb.ActorRuntime_RUNNING {
				continue
			}
			if m.appPort > 0 && (!ar.GetHostReady() || !strings.Contains(ar.GetPlacement(), "connected")) {
				continue
			}
			return nil
		}
	}
}

func (m *Manager) withReadinessDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, m.readinessTimeout)
}

func resolveBinary(explicit, name, homeDir string, lookPath func(string) (string, error)) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		info, err := os.Stat(explicit)
		if err != nil {
			return "", fmt.Errorf("explicit %s binary %q not accessible: %w", name, explicit, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("explicit %s binary %q is a directory", name, explicit)
		}
		return filepath.Abs(explicit)
	}

	if strings.TrimSpace(homeDir) != "" {
		candidates := []string{name}
		if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
			candidates = []string{name + ".exe", name}
		}
		for _, cand := range candidates {
			p := filepath.Join(homeDir, ".dapr", "bin", cand)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p, nil
			}
		}
	}

	resolved, err := lookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found in ~/.dapr/bin or PATH: %w", name, err)
	}
	return resolved, nil
}

func reserveLoopbackPorts(count int) ([]int, error) {
	listeners := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	closeAll := func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}

	for i := 0; i < count; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			closeAll()
			return nil, err
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	closeAll()
	return ports, nil
}

func buildIsolatedEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[k]; replaced {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func startManagedProcess(bin string, args []string, env []string) (*managedProc, error) {
	cmd := exec.Command(bin, args...)
	if env != nil {
		cmd.Env = env
	}
	out := &boundedBuffer{max: maxOutputBufferBytes}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	proc := &managedProc{
		cmd:    cmd,
		done:   done,
		output: out,
	}
	go func() {
		proc.exitErr = cmd.Wait()
		close(done)
	}()
	return proc, nil
}

type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && b.buf.Len()+len(p) > b.max {
		overflow := b.buf.Len() + len(p) - b.max
		if overflow >= b.buf.Len() {
			b.buf.Reset()
		} else {
			b.buf.Next(overflow)
		}
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
