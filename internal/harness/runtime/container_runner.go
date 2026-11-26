package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
)

const containerPluginUnixDir = "/.sofa/plugin"

// ContainerRunner implements hashicorp/go-plugin's runner.Runner and runner.AddrTranslator
// to execute an external AgentHarness plugin as the primary PID inside a Podman/OCI container.
type ContainerRunner struct {
	runtimeBin    string
	containerName string
	hostOS        string
	hostTmpDir    string
	runArgs       []string
	logger        hclog.Logger

	cmd        *exec.Cmd
	stdout     io.ReadCloser
	stderrPipe io.ReadCloser
	stderrBuf  *tailBuffer
	stderrCopy io.ReadCloser

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}
}

var _ runner.Runner = (*ContainerRunner)(nil)
var _ runner.AddrTranslator = (*ContainerRunner)(nil)

// NewContainerRunnerFunc returns a plugin.ClientConfig.RunnerFunc that executes the plugin
// inside a sandbox container configured by baseLayout.
func NewContainerRunnerFunc(baseLayout sandboxruntime.SessionMountLayout, runtimeBin string) func(l hclog.Logger, cmd *exec.Cmd, tmpDir string) (runner.Runner, error) {
	return func(l hclog.Logger, cmd *exec.Cmd, tmpDir string) (runner.Runner, error) {
		return newContainerRunner(l, cmd, tmpDir, baseLayout, runtimeBin)
	}
}

func newContainerRunner(
	logger hclog.Logger,
	cmd *exec.Cmd,
	tmpDir string,
	baseLayout sandboxruntime.SessionMountLayout,
	runtimeBin string,
) (*ContainerRunner, error) {
	bin := strings.TrimSpace(runtimeBin)
	if bin == "" {
		bin = strings.TrimSpace(baseLayout.Sandbox.Runtime)
	}
	if bin == "" {
		bin = "podman"
	}

	hostOS := strings.TrimSpace(baseLayout.HostOS)
	if hostOS == "" {
		hostOS = goruntime.GOOS
	}

	layout := baseLayout
	layout.HostOS = hostOS
	layout.ForAgentSession = true
	layout.Detached = false
	if strings.TrimSpace(layout.ContainerName) == "" {
		layout.ContainerName = "sofa-harness-" + uuid.NewString()[:12]
	}

	var srcEnv []string
	if cmd != nil {
		srcEnv = cmd.Env
	}
	handshakeEnv := buildContainerHandshakeEnv(srcEnv, hostOS)
	layout.HandshakeEnv = handshakeEnv

	if hostOS == "linux" {
		layout.PluginSocketDir = tmpDir
		layout.PublishPluginPort = false
	} else {
		layout.PluginSocketDir = ""
		layout.PublishPluginPort = true
	}

	runArgs, err := sandboxruntime.BuildRunArgs(layout)
	if err != nil {
		return nil, err
	}

	return &ContainerRunner{
		runtimeBin:    bin,
		containerName: layout.ContainerName,
		hostOS:        hostOS,
		hostTmpDir:    tmpDir,
		runArgs:       runArgs,
		logger:        logger,
		waitDone:      make(chan struct{}),
	}, nil
}

func buildContainerHandshakeEnv(cmdEnv []string, hostOS string) []string {
	var out []string
	for _, entry := range cmdEnv {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if key != "SOFA_HARNESS_PLUGIN" && !strings.HasPrefix(key, "PLUGIN_") {
			continue
		}
		if hostOS == "linux" {
			if key == "PLUGIN_UNIX_SOCKET_DIR" {
				out = append(out, "PLUGIN_UNIX_SOCKET_DIR="+containerPluginUnixDir)
				continue
			}
			out = append(out, entry)
			continue
		}
		// Non-Linux hosts (Windows/macOS) strip Unix socket env and pin the container TCP port.
		if key == "PLUGIN_UNIX_SOCKET_DIR" || key == "PLUGIN_MIN_PORT" || key == "PLUGIN_MAX_PORT" {
			continue
		}
		out = append(out, entry)
	}

	if hostOS != "linux" {
		portStr := strconv.Itoa(sandboxruntime.ContainerPluginPort)
		out = append(out,
			"PLUGIN_MIN_PORT="+portStr,
			"PLUGIN_MAX_PORT="+portStr,
		)
	}
	return out
}

// Start launches the container CLI foreground process with stdout and stderr pipes attached.
func (r *ContainerRunner) Start(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// Do NOT use exec.CommandContext(ctx, ...) because go-plugin cancels startCtx immediately
	// after Start returns; container shutdown is managed explicitly by Kill.
	cmd := exec.Command(r.runtimeBin, r.runArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("attach container stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return fmt.Errorf("attach container stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("start harness container %q: %w", r.containerName, err)
	}

	r.cmd = cmd
	r.stdout = stdout
	r.stderrPipe = stderr
	r.stderrBuf = newTailBuffer(8192)
	r.stderrCopy = &teeReadCloser{
		r: io.TeeReader(stderr, r.stderrBuf),
		c: stderr,
	}
	return nil
}

// Wait waits for the container CLI process to exit.
func (r *ContainerRunner) Wait(ctx context.Context) error {
	if r.cmd == nil {
		return nil
	}
	go r.doWait()
	if ctx == nil {
		<-r.waitDone
		return r.waitErr
	}
	select {
	case <-r.waitDone:
		return r.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *ContainerRunner) doWait() {
	r.waitOnce.Do(func() {
		defer close(r.waitDone)
		r.waitErr = r.cmd.Wait()
	})
}

// Kill forcefully stops and removes the running container so zero orphan containers remain.
func (r *ContainerRunner) Kill(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, rmErr := exec.CommandContext(rmCtx, r.runtimeBin, "rm", "-f", "-t", "2", r.containerName).CombinedOutput()
	if r.cmd != nil && r.cmd.Process != nil {
		select {
		case <-r.waitDone:
		case <-time.After(3 * time.Second):
			_ = r.cmd.Process.Kill()
		}
		r.doWait()
	}

	if rmErr != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "no such container") || strings.Contains(msg, "not found") {
			return nil
		}
		return fmt.Errorf("remove harness container %q: %w (%s)", r.containerName, rmErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// ID returns the unique container name, usable directly by PodmanEngine.Exec.
func (r *ContainerRunner) ID() string {
	return r.containerName
}

// Name returns a human-readable plugin container identifier.
func (r *ContainerRunner) Name() string {
	return r.containerName
}

// Stdout exposes the container stdout stream for go-plugin's handshake line.
func (r *ContainerRunner) Stdout() io.ReadCloser {
	return r.stdout
}

// Stderr exposes the container stderr stream for go-plugin log forwarding.
func (r *ContainerRunner) Stderr() io.ReadCloser {
	return r.stderrCopy
}

// Diagnose returns recent container stderr output to aid startup failure debugging.
func (r *ContainerRunner) Diagnose(_ context.Context) string {
	if r.stderrBuf == nil {
		return ""
	}
	return strings.TrimSpace(r.stderrBuf.String())
}

// PluginToHost translates a container-side plugin listener address to a host-reachable address.
func (r *ContainerRunner) PluginToHost(pluginNet, pluginAddr string) (string, string, error) {
	if r.hostOS == "linux" {
		if pluginNet != "unix" {
			return "", "", fmt.Errorf("expected unix network on linux host, got %q (%q)", pluginNet, pluginAddr)
		}
		cleaned := path.Clean(pluginAddr)
		prefix := containerPluginUnixDir + "/"
		if !strings.HasPrefix(cleaned, prefix) {
			return "", "", fmt.Errorf("plugin unix socket %q must live under %s", pluginAddr, containerPluginUnixDir)
		}
		rel := strings.TrimPrefix(cleaned, prefix)
		if rel == "" || strings.Contains(rel, "/") || rel == "." || rel == ".." {
			return "", "", fmt.Errorf("invalid plugin unix socket relative name %q", rel)
		}
		return "unix", filepath.Join(r.hostTmpDir, rel), nil
	}

	if pluginNet != "tcp" {
		return "", "", fmt.Errorf("expected tcp network on %s host, got %q (%q)", r.hostOS, pluginNet, pluginAddr)
	}

	portArg := strconv.Itoa(sandboxruntime.ContainerPluginPort)
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		out, err := exec.Command(r.runtimeBin, "port", r.containerName, portArg).CombinedOutput()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				if strings.HasPrefix(trimmed, "0.0.0.0:") {
					trimmed = "127.0.0.1:" + strings.TrimPrefix(trimmed, "0.0.0.0:")
				}
				return "tcp", trimmed, nil
			}
			lastErr = errors.New("empty podman port output")
		} else {
			lastErr = fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", "", fmt.Errorf("resolve published port %s for container %q: %w", portArg, r.containerName, lastErr)
}

// HostToPlugin passes host addresses through unchanged (no host-side reverse dial is used).
func (r *ContainerRunner) HostToPlugin(hostNet, hostAddr string) (string, string, error) {
	return hostNet, hostAddr, nil
}

type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeReadCloser) Close() error               { return t.c.Close() }

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func newTailBuffer(maxBytes int) *tailBuffer {
	return &tailBuffer{max: maxBytes}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		b.buf.Reset()
		b.buf.Write(p[len(p)-b.max:])
		return len(p), nil
	}
	if b.buf.Len()+len(p) > b.max {
		excess := b.buf.Len() + len(p) - b.max
		data := b.buf.Bytes()
		b.buf.Reset()
		b.buf.Write(data[excess:])
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
