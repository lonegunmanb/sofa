package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"

	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/tool/gotool"
)

const containerToolsDir = "/.sofa/tools"

// CompiledTool describes a host-compiled and cached static binary for a go_tool.
type CompiledTool struct {
	ToolID        string
	CacheKey      string
	BinaryPath    string
	ContainerPath string
	TargetOS      string
	TargetArch    string
}

// BuildFunc runs a Go toolchain command (such as `go get` or `go build`) inside dir.
type BuildFunc func(ctx context.Context, dir string, env []string, args ...string) error

// CompilerOptions configures Go tool wrapper compilation and caching.
type CompilerOptions struct {
	CacheDir   string
	TargetOS   string
	TargetArch string
	GoVersion  string
	BuildFunc  BuildFunc
}

// Compiler generates standalone Go wrappers and compiles cached static go_tool binaries.
type Compiler struct {
	cacheDir   string
	targetOS   string
	targetArch string
	goVersion  string
	buildFn    BuildFunc

	mu       sync.Mutex
	keyLocks map[string]*sync.Mutex
}

// NewCompiler constructs a Compiler with defaults for host OS/arch and Go version.
func NewCompiler(opts CompilerOptions) *Compiler {
	cacheDir := strings.TrimSpace(opts.CacheDir)
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "sofa-tool-cache")
	}
	targetOS := strings.TrimSpace(opts.TargetOS)
	if targetOS == "" {
		targetOS = goruntime.GOOS
	}
	targetArch := strings.TrimSpace(opts.TargetArch)
	if targetArch == "" {
		targetArch = goruntime.GOARCH
	}
	goVersion := normalizeGoVersion(opts.GoVersion)
	buildFn := opts.BuildFunc
	if buildFn == nil {
		buildFn = defaultGoBuildFunc
	}
	return &Compiler{
		cacheDir:   cacheDir,
		targetOS:   targetOS,
		targetArch: targetArch,
		goVersion:  goVersion,
		buildFn:    buildFn,
		keyLocks:   make(map[string]*sync.Mutex),
	}
}

func normalizeGoVersion(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		v = goruntime.Version()
	}
	v = strings.TrimPrefix(v, "go")
	if v == "" {
		return "1.24"
	}
	return v
}

func defaultGoBuildFunc(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// GenerateGoToolWrapper wraps an inline go_tool source snippet into a standalone package main program.
func GenerateGoToolWrapper(source string) string {
	var b strings.Builder
	b.WriteString(`package main

import (
	_sofabytes "bytes"
	_sofacontext "context"
	_sofajson "encoding/json"
	_sofafmt "fmt"
	_sofaio "io"
	_sofaos "os"
	_sofasignal "os/signal"
	_sofastrings "strings"
	_sofasyscall "syscall"
)

`)
	b.WriteString(source)
	if !strings.HasSuffix(source, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(`
type Issue struct {
	ID         string ` + "`json:\"id,omitempty\"`" + `
	Code       string ` + "`json:\"code\"`" + `
	Message    string ` + "`json:\"message\"`" + `
	Path       string ` + "`json:\"path,omitempty\"`" + `
	RepairHint string ` + "`json:\"repair_hint,omitempty\"`" + `
}

type ToolResponse[T any] struct {
	Accepted bool    ` + "`json:\"accepted\"`" + `
	Output   *T      ` + "`json:\"output,omitempty\"`" + `
	Issues   []Issue ` + "`json:\"issues,omitempty\"`" + `
}

func main() {
	dec := _sofajson.NewDecoder(_sofaos.Stdin)
	var raw _sofajson.RawMessage
	if err := dec.Decode(&raw); err != nil {
		_sofafmt.Fprintf(_sofaos.Stderr, "invalid JSON input: %v\n", err)
		_sofaos.Exit(1)
	}
	var extra any
	if err := dec.Decode(&extra); err != _sofaio.EOF {
		_sofafmt.Fprintln(_sofaos.Stderr, "unexpected trailing tokens after JSON input")
		_sofaos.Exit(1)
	}
	trimmed := _sofabytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		_sofafmt.Fprintln(_sofaos.Stderr, "stdin must contain a single JSON object")
		_sofaos.Exit(1)
	}

	var input Input
	objDec := _sofajson.NewDecoder(_sofabytes.NewReader(trimmed))
	objDec.DisallowUnknownFields()
	if err := objDec.Decode(&input); err != nil {
		_sofafmt.Fprintf(_sofaos.Stderr, "invalid input object: %v\n", err)
		_sofaos.Exit(1)
	}

	ctx, stop := _sofasignal.NotifyContext(_sofacontext.Background(), _sofaos.Interrupt, _sofasyscall.SIGTERM)
	defer stop()

	resp, err := Invoke(ctx, input)
	if err != nil {
		_sofafmt.Fprintf(_sofaos.Stderr, "invoke error: %v\n", err)
		_sofaos.Exit(1)
	}

	if resp.Accepted {
		if len(resp.Issues) != 0 {
			_sofafmt.Fprintln(_sofaos.Stderr, "protocol error: accepted response must not contain issues")
			_sofaos.Exit(1)
		}
		if resp.Output == nil {
			_sofafmt.Fprintln(_sofaos.Stderr, "protocol error: accepted response must include non-nil output")
			_sofaos.Exit(1)
		}
	} else {
		if resp.Output != nil {
			_sofafmt.Fprintln(_sofaos.Stderr, "protocol error: rejected response must not contain output")
			_sofaos.Exit(1)
		}
		if len(resp.Issues) == 0 {
			_sofafmt.Fprintln(_sofaos.Stderr, "protocol error: rejected response must contain at least one issue")
			_sofaos.Exit(1)
		}
		for _, issue := range resp.Issues {
			if _sofastrings.TrimSpace(issue.Code) == "" || _sofastrings.TrimSpace(issue.Message) == "" {
				_sofafmt.Fprintln(_sofaos.Stderr, "protocol error: rejected issue code and message must be non-empty")
				_sofaos.Exit(1)
			}
		}
	}

	if err := _sofajson.NewEncoder(_sofaos.Stdout).Encode(resp); err != nil {
		_sofafmt.Fprintf(_sofaos.Stderr, "encode response: %v\n", err)
		_sofaos.Exit(1)
	}
}
`)
	return b.String()
}

// Compile compiles and caches a static executable for a go_tool specification.
func (c *Compiler) Compile(ctx context.Context, spec plan.ToolSpec) (CompiledTool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CompiledTool{}, err
	}
	if spec.Kind != "go" {
		return CompiledTool{}, fmt.Errorf("tool %q has kind %q, expected \"go\"", spec.ID, spec.Kind)
	}
	if strings.TrimSpace(spec.ID) == "" {
		return CompiledTool{}, errors.New("go_tool id is required")
	}
	if strings.TrimSpace(spec.Source) == "" {
		return CompiledTool{}, fmt.Errorf("go_tool %q source is required", spec.ID)
	}

	sortedDeps := slices.Clone(spec.Dependencies)
	slices.Sort(sortedDeps)
	for _, dep := range sortedDeps {
		if err := gotool.ValidateDependency(dep); err != nil {
			return CompiledTool{}, fmt.Errorf("go_tool %q dependency: %w", spec.ID, err)
		}
	}

	wrappedSource := GenerateGoToolWrapper(spec.Source)
	cacheKey := computeToolCacheKey(wrappedSource, sortedDeps, c.targetOS, c.targetArch, c.goVersion)

	keyMu := c.lockForKey(cacheKey)
	keyMu.Lock()
	defer keyMu.Unlock()

	binName := "tool"
	if c.targetOS == "windows" {
		binName = "tool.exe"
	}
	keyDir := filepath.Join(c.cacheDir, cacheKey)
	finalBinPath := filepath.Join(keyDir, binName)
	containerPath := containerToolsDir + "/" + spec.ID

	if isCachedBinaryReady(finalBinPath) {
		return CompiledTool{
			ToolID:        spec.ID,
			CacheKey:      cacheKey,
			BinaryPath:    finalBinPath,
			ContainerPath: containerPath,
			TargetOS:      c.targetOS,
			TargetArch:    c.targetArch,
		}, nil
	}

	if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
		return CompiledTool{}, fmt.Errorf("create compiler cache dir %q: %w", c.cacheDir, err)
	}
	stagingDir, err := os.MkdirTemp(c.cacheDir, ".build-"+cacheKey[:12]+"-*")
	if err != nil {
		return CompiledTool{}, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	goMod := fmt.Sprintf("module sofa.inline.tool\n\ngo %s\n", c.goVersion)
	if err := os.WriteFile(filepath.Join(stagingDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		return CompiledTool{}, fmt.Errorf("write go.mod: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "main.go"), []byte(wrappedSource), 0o644); err != nil {
		return CompiledTool{}, fmt.Errorf("write main.go: %w", err)
	}

	env := buildGoEnv(c.targetOS, c.targetArch)
	if len(sortedDeps) > 0 {
		getArgs := append([]string{"get"}, sortedDeps...)
		if err := c.buildFn(ctx, stagingDir, env, getArgs...); err != nil {
			return CompiledTool{}, fmt.Errorf("go get dependencies for %q: %w", spec.ID, err)
		}
	}

	tmpBinPath := filepath.Join(stagingDir, binName)
	if err := c.buildFn(ctx, stagingDir, env, "build", "-trimpath", "-o", tmpBinPath, "main.go"); err != nil {
		return CompiledTool{}, fmt.Errorf("compile go_tool %q: %w", spec.ID, err)
	}
	if err := os.Chmod(tmpBinPath, 0o755); err != nil {
		return CompiledTool{}, fmt.Errorf("chmod compiled binary %q: %w", tmpBinPath, err)
	}

	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		return CompiledTool{}, fmt.Errorf("create cache entry dir %q: %w", keyDir, err)
	}
	if err := atomicInstallFile(tmpBinPath, finalBinPath, 0o755); err != nil {
		return CompiledTool{}, fmt.Errorf("cache compiled binary %q: %w", finalBinPath, err)
	}

	return CompiledTool{
		ToolID:        spec.ID,
		CacheKey:      cacheKey,
		BinaryPath:    finalBinPath,
		ContainerPath: containerPath,
		TargetOS:      c.targetOS,
		TargetArch:    c.targetArch,
	}, nil
}

// StageToolsDir materializes compiled binaries at <destToolsDir>/<toolID> with 0755 permissions
// so the directory can be mounted read-only at /.sofa/tools inside the container.
func StageToolsDir(ctx context.Context, destToolsDir string, compiled []CompiledTool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(destToolsDir) == "" {
		return errors.New("destination tools directory is required")
	}
	if err := os.MkdirAll(destToolsDir, 0o755); err != nil {
		return fmt.Errorf("create tools staging dir %q: %w", destToolsDir, err)
	}

	for _, ct := range compiled {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(ct.ToolID) == "" || strings.ContainsAny(ct.ToolID, `/\`) || ct.ToolID == "." || ct.ToolID == ".." {
			return fmt.Errorf("invalid compiled tool id %q", ct.ToolID)
		}
		dst := filepath.Join(destToolsDir, ct.ToolID)
		_ = os.Remove(dst)
		if err := os.Link(ct.BinaryPath, dst); err == nil {
			_ = os.Chmod(dst, 0o755)
			continue
		}
		if err := atomicInstallFile(ct.BinaryPath, dst, 0o755); err != nil {
			return fmt.Errorf("stage tool %q: %w", ct.ToolID, err)
		}
	}
	return nil
}

func (c *Compiler) lockForKey(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.keyLocks[key]
	if !ok {
		m = &sync.Mutex{}
		c.keyLocks[key] = m
	}
	return m
}

func computeToolCacheKey(wrappedSource string, sortedDeps []string, targetOS, targetArch, goVersion string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, "sofa-go-tool-cache-v1\x00")
	_, _ = io.WriteString(h, targetOS)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, targetArch)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, goVersion)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, wrappedSource)
	for _, dep := range sortedDeps {
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, dep)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isCachedBinaryReady(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	_ = os.Chmod(path, 0o755)
	return true
}

func buildGoEnv(targetOS, targetArch string) []string {
	overrides := map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        targetOS,
		"GOARCH":      targetArch,
		"GO111MODULE": "on",
		"GOWORK":      "off",
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		k, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[strings.ToUpper(k)]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func atomicInstallFile(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dstDir := filepath.Dir(dstPath)
	tmpFile, err := os.CreateTemp(dstDir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	_, copyErr := io.Copy(tmpFile, src)
	closeErr := tmpFile.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
