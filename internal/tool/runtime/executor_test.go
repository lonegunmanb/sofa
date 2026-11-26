package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/plan"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/lonegunmanb/sofa/internal/tool/gotool"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func buildTestGoToolSpec(t *testing.T, id, name, source string, quotePaths [][]string, postconditions []corespec.Condition) plan.ToolSpec {
	t.Helper()
	analysis, err := gotool.Analyze(source)
	require.NoError(t, err)
	schemaMap, err := toolspec.NewConstraint(analysis.InputType).JSONSchema()
	require.NoError(t, err)
	schemaBytes, err := json.Marshal(schemaMap)
	require.NoError(t, err)
	inTypeBytes, err := ctyjson.MarshalType(analysis.InputType)
	require.NoError(t, err)
	outTypeBytes, err := ctyjson.MarshalType(analysis.OutputType)
	require.NoError(t, err)
	if len(quotePaths) == 0 {
		quotePaths = analysis.QuotePaths
	}
	return plan.ToolSpec{
		ID:             id,
		Name:           name,
		Address:        "go_tool." + name,
		Kind:           "go",
		Description:    "test go tool " + name,
		Source:         source,
		InputSchema:    schemaBytes,
		InputType:      inTypeBytes,
		OutputType:     outTypeBytes,
		QuotePaths:     quotePaths,
		Postconditions: postconditions,
	}
}

func runNativeCompiledBinary(ctx context.Context, binPath string, stdin []byte) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), -1, err
}

func TestCompilerCompileCacheHitConcurrencyAndStaging(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	var buildInvocations atomic.Int32

	compiler := toolruntime.NewCompiler(toolruntime.CompilerOptions{
		CacheDir:   cacheDir,
		TargetOS:   goruntime.GOOS,
		TargetArch: goruntime.GOARCH,
		BuildFunc: func(ctx context.Context, dir string, env []string, args ...string) error {
			if len(args) > 0 && args[0] == "build" {
				buildInvocations.Add(1)
			}
			cmd := exec.CommandContext(ctx, "go", args...)
			cmd.Dir = dir
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				return errors.New(string(out))
			}
			return nil
		},
	})

	source := `
import (
	"context"
	stdstr "strings"
)

type Input struct {
	Name string ` + "`json:\"name\"`" + `
}

type Output struct {
	Greeting string ` + "`json:\"greeting\"`" + `
}

func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
	out := Output{Greeting: "hello " + stdstr.ToUpper(in.Name)}
	return ToolResponse[Output]{Accepted: true, Output: &out}, nil
}
`
	spec := buildTestGoToolSpec(t, "tool_go_tool_greeter_12345678-1234-8234-9234-123456789abc", "greeter", source, nil, nil)

	// Concurrent compiles for the same key must serialize and only build once.
	const workers = 4
	results := make([]toolruntime.CompiledTool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = compiler.Compile(t.Context(), spec)
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, spec.ID, results[i].ToolID)
		assert.Equal(t, "/.sofa/tools/"+spec.ID, results[i].ContainerPath)
		assert.FileExists(t, results[i].BinaryPath)
	}
	assert.Equal(t, int32(1), buildInvocations.Load(), "concurrent compiles should invoke go build once")

	// Sequential second Compile hits cache with zero go build calls.
	cached, err := compiler.Compile(t.Context(), spec)
	require.NoError(t, err)
	assert.Equal(t, results[0].CacheKey, cached.CacheKey)
	assert.Equal(t, int32(1), buildInvocations.Load(), "second Compile must hit cache")

	// Stage into tools dir at <destToolsDir>/<toolID>.
	stageDir := filepath.Join(t.TempDir(), "tools")
	require.NoError(t, toolruntime.StageToolsDir(t.Context(), stageDir, []toolruntime.CompiledTool{cached}))
	stagedBin := filepath.Join(stageDir, spec.ID)
	info, err := os.Stat(stagedBin)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())

	// Execute compiled binary directly on host.
	stdout, stderr, exitCode, err := runNativeCompiledBinary(t.Context(), cached.BinaryPath, []byte(`{"name":"sofa"}`))
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode, "stderr: %s", stderr)
	assert.Contains(t, stdout, `"greeting":"hello SOFA"`)
}

func TestCompilerWrapperProtocolValidation(t *testing.T) {
	t.Parallel()

	compiler := toolruntime.NewCompiler(toolruntime.CompilerOptions{
		CacheDir:   t.TempDir(),
		TargetOS:   goruntime.GOOS,
		TargetArch: goruntime.GOARCH,
	})

	source := `
import (
	"context"
	"errors"
	_ "os"
)

type Input struct {
	Mode string ` + "`json:\"mode\"`" + `
}

type Output struct {
	Value string ` + "`json:\"value\"`" + `
}

func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
	out := Output{Value: "ok"}
	switch in.Mode {
	case "ok":
		return ToolResponse[Output]{Accepted: true, Output: &out}, nil
	case "reject_with_path":
		return ToolResponse[Output]{
			Accepted: false,
			Issues:   []Issue{{Code: "bad_mode", Message: "rejected by tool", Path: "mode"}},
		}, nil
	case "err":
		return ToolResponse[Output]{}, errors.New("boom")
	case "bad_accepted_issues":
		return ToolResponse[Output]{
			Accepted: true,
			Output:   &out,
			Issues:   []Issue{{Code: "oops", Message: "should not exist"}},
		}, nil
	case "bad_accepted_nil_output":
		return ToolResponse[Output]{Accepted: true}, nil
	case "bad_rejected_with_output":
		return ToolResponse[Output]{
			Accepted: false,
			Output:   &out,
			Issues:   []Issue{{Code: "x", Message: "y"}},
		}, nil
	case "bad_rejected_no_issues":
		return ToolResponse[Output]{Accepted: false}, nil
	case "bad_rejected_blank_issue":
		return ToolResponse[Output]{
			Accepted: false,
			Issues:   []Issue{{Code: " ", Message: "msg"}},
		}, nil
	default:
		return ToolResponse[Output]{Accepted: true, Output: &out}, nil
	}
}
`
	spec := buildTestGoToolSpec(t, "tool_go_tool_proto_12345678-1234-8234-9234-123456789abc", "proto", source, nil, nil)
	compiled, err := compiler.Compile(t.Context(), spec)
	require.NoError(t, err)

	badStdinCases := []struct {
		name  string
		stdin string
	}{
		{name: "unknown field", stdin: `{"mode":"ok","extra":1}`},
		{name: "trailing JSON", stdin: `{"mode":"ok"}{"mode":"ok"}`},
		{name: "null input", stdin: `null`},
		{name: "array input", stdin: `[]`},
		{name: "invalid JSON", stdin: `{broken`},
	}
	for _, tc := range badStdinCases {
		t.Run("stdin_"+tc.name, func(t *testing.T) {
			_, stderr, exitCode, runErr := runNativeCompiledBinary(t.Context(), compiled.BinaryPath, []byte(tc.stdin))
			require.NoError(t, runErr)
			assert.NotEqual(t, 0, exitCode)
			assert.NotEmpty(t, strings.TrimSpace(stderr))
		})
	}

	badRespModes := []string{
		"err",
		"bad_accepted_issues",
		"bad_accepted_nil_output",
		"bad_rejected_with_output",
		"bad_rejected_no_issues",
		"bad_rejected_blank_issue",
	}
	for _, mode := range badRespModes {
		t.Run("resp_"+mode, func(t *testing.T) {
			_, stderr, exitCode, runErr := runNativeCompiledBinary(t.Context(), compiled.BinaryPath, []byte(`{"mode":"`+mode+`"}`))
			require.NoError(t, runErr)
			assert.NotEqual(t, 0, exitCode)
			assert.NotEmpty(t, strings.TrimSpace(stderr))
		})
	}

	stdout, stderr, exitCode, runErr := runNativeCompiledBinary(t.Context(), compiled.BinaryPath, []byte(`{"mode":"reject_with_path"}`))
	require.NoError(t, runErr)
	assert.Equal(t, 0, exitCode, "stderr: %s", stderr)
	assert.Contains(t, stdout, `"accepted":false`)
	assert.Contains(t, stdout, `"path":"mode"`)
}

func TestExecutorToolUsePresetSchemaAndTamperProofMerge(t *testing.T) {
	t.Parallel()

	inType := cty.Object(map[string]cty.Type{
		"env":  cty.String,
		"name": cty.String,
	})
	outType := cty.Object(map[string]cty.Type{
		"echo_env":  cty.String,
		"echo_name": cty.String,
	})
	schemaMap, err := toolspec.NewConstraint(inType).JSONSchema()
	require.NoError(t, err)
	schemaBytes, _ := json.Marshal(schemaMap)
	inTypeBytes, _ := ctyjson.MarshalType(inType)
	outTypeBytes, _ := ctyjson.MarshalType(outType)

	spec := plan.ToolSpec{
		ID:          "tool_external_tool_deploy_12345678-1234-8234-9234-123456789abc",
		Name:        "deploy",
		Address:     "external_tool.deploy",
		Kind:        "external",
		Description: "deploy helper",
		Program:     []string{"/bin/echo"},
		InputSchema: schemaBytes,
		InputType:   inTypeBytes,
		OutputType:  outTypeBytes,
	}

	// Partial preset binding: "env" hidden from LLM schema and forcibly overwritten.
	partialBinding := toolruntime.ToolBinding{
		Spec:        spec,
		ExposedName: "deploy_prod",
		PresetInput: cty.ObjectVal(map[string]cty.Value{
			"env": cty.StringVal("production"),
		}),
	}

	hostSpec, err := toolruntime.HostToolSpec(partialBinding)
	require.NoError(t, err)
	assert.Equal(t, "deploy_prod", hostSpec.Name)

	var parsedSchema map[string]any
	require.NoError(t, json.Unmarshal(hostSpec.ParametersJSONSchema, &parsedSchema))
	props := parsedSchema["properties"].(map[string]any)
	assert.NotContains(t, props, "env", "preset field must be stripped from LLM JSON Schema")
	assert.Contains(t, props, "name")
	assert.Equal(t, []any{"name"}, parsedSchema["required"])

	// All-preset binding: schema must collapse to empty properties object.
	allPresetBinding := toolruntime.ToolBinding{
		Spec:        spec,
		ExposedName: "deploy_fixed",
		PresetInput: cty.ObjectVal(map[string]cty.Value{
			"env":  cty.StringVal("staging"),
			"name": cty.StringVal("api"),
		}),
	}
	allSpec, err := toolruntime.HostToolSpec(allPresetBinding)
	require.NoError(t, err)
	var allSchema map[string]any
	require.NoError(t, json.Unmarshal(allSpec.ParametersJSONSchema, &allSchema))
	assert.Equal(t, "object", allSchema["type"])
	assert.Equal(t, false, allSchema["additionalProperties"])
	assert.Empty(t, allSchema["properties"])
	assert.Nil(t, allSchema["required"])

	var capturedStdin []byte
	exec := toolruntime.NewExecutor(toolruntime.ExecutorOptions{
		WorkspaceDir: t.TempDir(),
		Exec: func(ctx context.Context, workDir string, command []string, env map[string]string, stdin []byte) (string, string, int, error) {
			capturedStdin = append([]byte(nil), stdin...)
			var in struct {
				Env  string `json:"env"`
				Name string `json:"name"`
			}
			require.NoError(t, json.Unmarshal(stdin, &in))
			respBytes, _ := json.Marshal(map[string]any{
				"echo_env":  in.Env,
				"echo_name": in.Name,
			})
			return string(respBytes), "", 0, nil
		},
	})

	// LLM attempts to tamper with preset "env" field -> must be overwritten with "production".
	res, err := exec.Invoke(t.Context(), partialBinding, []byte(`{"env":"dev_hacked","name":"worker-a"}`))
	require.NoError(t, err)
	assert.True(t, res.Accepted)
	assert.Empty(t, res.ToolOutput.HandlerError)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, res.ToolOutput.ResultType)
	assert.Equal(t, "production", res.ValidatedInput.GetAttr("env").AsString())
	assert.Equal(t, "production", res.ValidatedOutput.GetAttr("echo_env").AsString())
	assert.Contains(t, string(capturedStdin), `"env":"production"`)

	// LLM passes unknown field -> model-repairable invalid_input rejection (HandlerError == "").
	badRes, err := exec.Invoke(t.Context(), partialBinding, []byte(`{"name":"worker-a","unknown_field":true}`))
	require.NoError(t, err)
	assert.False(t, badRes.Accepted)
	assert.Empty(t, badRes.ToolOutput.HandlerError)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, badRes.ToolOutput.ResultType)
	require.Len(t, badRes.Issues, 1)
	assert.Equal(t, "invalid_input", badRes.Issues[0].Code)
}

func TestExecutorGoToolAcceptedRejectedErrorAndPostcondition(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	compiler := toolruntime.NewCompiler(toolruntime.CompilerOptions{
		CacheDir:   t.TempDir(),
		TargetOS:   goruntime.GOOS,
		TargetArch: goruntime.GOARCH,
	})

	source := `
import (
	"context"
	"errors"
)

type Input struct {
	Action string ` + "`json:\"action\"`" + `
	Min    int    ` + "`json:\"min\"`" + `
}

type Output struct {
	Count int ` + "`json:\"count\"`" + `
}

func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
	switch in.Action {
	case "pass":
		out := Output{Count: in.Min + 2}
		return ToolResponse[Output]{Accepted: true, Output: &out}, nil
	case "fail_postcondition":
		out := Output{Count: in.Min - 1}
		return ToolResponse[Output]{Accepted: true, Output: &out}, nil
	case "reject":
		return ToolResponse[Output]{
			Accepted: false,
			Issues:   []Issue{{Code: "needs_fix", Message: "fix input", Path: "action"}},
		}, nil
	case "crash":
		return ToolResponse[Output]{}, errors.New("tool crashed")
	default:
		out := Output{Count: 0}
		return ToolResponse[Output]{Accepted: true, Output: &out}, nil
	}
}
`
	spec := buildTestGoToolSpec(
		t,
		"tool_go_tool_check_12345678-1234-8234-9234-123456789abc",
		"check",
		source,
		nil,
		[]corespec.Condition{
			{
				Expression:   "output.count >= max(input.min, 1)",
				ErrorMessage: "count must meet minimum threshold",
			},
		},
	)
	compiled, err := compiler.Compile(t.Context(), spec)
	require.NoError(t, err)

	exec := toolruntime.NewExecutor(toolruntime.ExecutorOptions{
		WorkspaceDir: ws,
		Exec: func(ctx context.Context, workDir string, command []string, env map[string]string, stdin []byte) (string, string, int, error) {
			assert.Equal(t, "/workspace", workDir)
			assert.Equal(t, []string{"/.sofa/tools/" + spec.ID}, command)
			assert.Empty(t, env)
			return runNativeCompiledBinary(ctx, compiled.BinaryPath, stdin)
		},
	})
	binding := toolruntime.ToolBinding{Spec: spec, Terminate: true}

	// 1. Accepted: true and postcondition passes.
	resPass, err := exec.Invoke(t.Context(), binding, []byte(`{"action":"pass","min":3}`))
	require.NoError(t, err)
	assert.True(t, resPass.Accepted)
	assert.True(t, resPass.Terminate)
	assert.Empty(t, resPass.ToolOutput.HandlerError)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, resPass.ToolOutput.ResultType)
	assert.Contains(t, string(resPass.OutputJSON), `"count":5`)

	// 2. Accepted: false from go_tool -> HostToolResultSuccess, HandlerError == "", Accepted == false.
	resReject, err := exec.Invoke(t.Context(), binding, []byte(`{"action":"reject","min":3}`))
	require.NoError(t, err)
	assert.False(t, resReject.Accepted)
	assert.Empty(t, resReject.ToolOutput.HandlerError)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, resReject.ToolOutput.ResultType)
	assert.Contains(t, resReject.ToolOutput.TextResultForLLM, `"accepted":false`)
	require.Len(t, resReject.Issues, 1)
	assert.Equal(t, "needs_fix", resReject.Issues[0].Code)

	// 3. Postcondition fails -> downgraded to Accepted: false with ErrorMessage, HandlerError == "".
	resPostFail, err := exec.Invoke(t.Context(), binding, []byte(`{"action":"fail_postcondition","min":5}`))
	require.NoError(t, err)
	assert.False(t, resPostFail.Accepted)
	assert.Empty(t, resPostFail.ToolOutput.HandlerError)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, resPostFail.ToolOutput.ResultType)
	require.Len(t, resPostFail.Issues, 1)
	assert.Equal(t, "postcondition_failed", resPostFail.Issues[0].Code)
	assert.Equal(t, "count must meet minimum threshold", resPostFail.Issues[0].Message)

	// 4. Invoke returns Go error -> exit 1 -> HostToolResultFailure with non-empty HandlerError.
	resCrash, err := exec.Invoke(t.Context(), binding, []byte(`{"action":"crash","min":1}`))
	require.NoError(t, err)
	assert.False(t, resCrash.Accepted)
	assert.Equal(t, harnessruntime.HostToolResultFailure, resCrash.ToolOutput.ResultType)
	assert.NotEmpty(t, resCrash.ToolOutput.HandlerError)
	assert.Contains(t, resCrash.ToolOutput.HandlerError, "tool crashed")
}

func TestExecutorQuotePathsVerification(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	importsDir := t.TempDir()
	bundleDir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package app\nfunc Hello() string { return \"real-code\" }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "binary.dat"), []byte{0xff, 0xfe, 'a', 'b'}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(importsDir, "spec.md"), []byte("Requirement 1: verify exact quotes.\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(bundleDir, "policy.txt"), []byte("Never hallucinate citations.\n"), 0o644))

	inType := cty.Object(map[string]cty.Type{"mode": cty.String})
	quoteObjType := cty.Object(map[string]cty.Type{
		"path":        cty.String,
		"exact_quote": cty.String,
	})
	outType := cty.Object(map[string]cty.Type{
		"primary": quoteObjType,
		"items":   cty.List(quoteObjType),
	})
	inTypeBytes, _ := ctyjson.MarshalType(inType)
	outTypeBytes, _ := ctyjson.MarshalType(outType)

	spec := plan.ToolSpec{
		ID:          "tool_go_tool_cit_12345678-1234-8234-9234-123456789abc",
		Name:        "cit",
		Address:     "go_tool.cit",
		Kind:        "go",
		Description: "cite files",
		InputType:   inTypeBytes,
		OutputType:  outTypeBytes,
		QuotePaths:  [][]string{{"primary"}, {"items", "[]"}},
	}

	var cannedOutput string
	exec := toolruntime.NewExecutor(toolruntime.ExecutorOptions{
		WorkspaceDir: ws,
		ImportsDir:   importsDir,
		BundleDir:    bundleDir,
		Exec: func(ctx context.Context, workDir string, command []string, env map[string]string, stdin []byte) (string, string, int, error) {
			return cannedOutput, "", 0, nil
		},
	})
	binding := toolruntime.ToolBinding{Spec: spec}

	// 1. Real quotes across workspace, /.sofa/imports/, and /.sofa/bundle/ pass.
	cannedOutput = `{"accepted":true,"output":{"primary":{"path":"/workspace/src/main.go","exact_quote":"func Hello() string { return \"real-code\" }"},"items":[{"path":"/.sofa/imports/spec.md","exact_quote":"verify exact quotes."},{"path":"/.sofa/bundle/policy.txt","exact_quote":"Never hallucinate"}]}}`
	res, err := exec.Invoke(t.Context(), binding, []byte(`{"mode":"valid"}`))
	require.NoError(t, err)
	assert.True(t, res.Accepted)
	assert.Empty(t, res.ToolOutput.HandlerError)

	// 2. Fabricated quote, missing file, traversal, .git, empty quote, and non-UTF8 must be downgraded.
	badCases := []struct {
		name    string
		payload string
	}{
		{
			name:    "fabricated quote",
			payload: `{"accepted":true,"output":{"primary":{"path":"src/main.go","exact_quote":"func Hallucinated() {}"},"items":[]}}`,
		},
		{
			name:    "missing file",
			payload: `{"accepted":true,"output":{"primary":{"path":"src/nope.go","exact_quote":"anything"},"items":[]}}`,
		},
		{
			name:    "empty quote",
			payload: `{"accepted":true,"output":{"primary":{"path":"src/main.go","exact_quote":""},"items":[]}}`,
		},
		{
			name:    "path traversal",
			payload: `{"accepted":true,"output":{"primary":{"path":"../outside.txt","exact_quote":"x"},"items":[]}}`,
		},
		{
			name:    "dot git blocked",
			payload: `{"accepted":true,"output":{"primary":{"path":".git/HEAD","exact_quote":"refs/heads/main"},"items":[]}}`,
		},
		{
			name:    "non utf8 rejected",
			payload: `{"accepted":true,"output":{"primary":{"path":"binary.dat","exact_quote":"ab"},"items":[]}}`,
		},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			cannedOutput = tc.payload
			badRes, invokeErr := exec.Invoke(t.Context(), binding, []byte(`{"mode":"bad"}`))
			require.NoError(t, invokeErr)
			assert.False(t, badRes.Accepted)
			assert.Empty(t, badRes.ToolOutput.HandlerError)
			assert.Equal(t, harnessruntime.HostToolResultSuccess, badRes.ToolOutput.ResultType)
			require.NotEmpty(t, badRes.Issues)
			assert.Equal(t, "quote_verification_failed", badRes.Issues[0].Code)
		})
	}
}

func TestExecutorExternalToolEnvRefsAndRedaction(t *testing.T) {
	t.Parallel()

	inType := cty.Object(map[string]cty.Type{"query": cty.String})
	outType := cty.Object(map[string]cty.Type{"rows": cty.Number})
	inTypeBytes, _ := ctyjson.MarshalType(inType)
	outTypeBytes, _ := ctyjson.MarshalType(outType)

	spec := plan.ToolSpec{
		ID:          "tool_external_tool_db_12345678-1234-8234-9234-123456789abc",
		Name:        "db",
		Address:     "external_tool.db",
		Kind:        "external",
		Description: "db tool",
		Program:     []string{"/usr/local/bin/db-query", "--json"},
		WorkingDir:  "scripts",
		InputType:   inTypeBytes,
		OutputType:  outTypeBytes,
	}

	const secretVal = "super-secret-db-token-999"
	var gotWorkDir string
	var gotEnv map[string]string
	var execStdout, execStderr string
	var execExit int

	exec := toolruntime.NewExecutor(toolruntime.ExecutorOptions{
		WorkspaceDir: t.TempDir(),
		LookupEnv: func(key string) (string, bool) {
			if key == "HOST_DB_TOKEN" {
				return secretVal, true
			}
			return "", false
		},
		Exec: func(ctx context.Context, workDir string, command []string, env map[string]string, stdin []byte) (string, string, int, error) {
			gotWorkDir = workDir
			gotEnv = env
			return execStdout, execStderr, execExit, nil
		},
	})

	binding := toolruntime.ToolBinding{
		Spec:    spec,
		Env:     map[string]string{"STATIC_MODE": "readonly"},
		EnvRefs: map[string]string{"DB_TOKEN": "HOST_DB_TOKEN"},
		Timeout: 5 * time.Second,
	}

	// 1. Success path.
	execStdout = `{"rows": 42}`
	execStderr = ""
	execExit = 0
	res, err := exec.Invoke(t.Context(), binding, []byte(`{"query":"select 1"}`))
	require.NoError(t, err)
	assert.True(t, res.Accepted)
	assert.Equal(t, "/workspace/scripts", gotWorkDir)
	assert.Equal(t, "readonly", gotEnv["STATIC_MODE"])
	assert.Equal(t, secretVal, gotEnv["DB_TOKEN"])
	assert.Equal(t, harnessruntime.HostToolResultSuccess, res.ToolOutput.ResultType)

	// 2. Non-zero exit code leaking secret in stderr -> HandlerError redacts secret!
	execStdout = ""
	execStderr = "auth failed using token " + secretVal
	execExit = 2
	failRes, err := exec.Invoke(t.Context(), binding, []byte(`{"query":"select 1"}`))
	require.NoError(t, err)
	assert.False(t, failRes.Accepted)
	assert.Equal(t, harnessruntime.HostToolResultFailure, failRes.ToolOutput.ResultType)
	assert.NotContains(t, failRes.ToolOutput.HandlerError, secretVal)
	assert.NotContains(t, failRes.ToolOutput.TextResultForLLM, secretVal)
	assert.Contains(t, failRes.ToolOutput.HandlerError, "[REDACTED]")
}

func TestMCPResourceReaderWhitelist(t *testing.T) {
	t.Parallel()

	// Empty allowed list -> not constructed.
	nilTool, ok := toolruntime.NewMCPResourceReader(nil, nil)
	assert.False(t, ok)
	assert.Nil(t, nilTool)

	var called atomic.Int32
	allowed := []toolruntime.MCPResourceSpec{
		{
			ID:         "mcp_resource_docs__readme_12345678-1234-8234-9234-123456789abc",
			ServerName: "docs",
			URI:        "doc://readme",
		},
	}
	readerTool, ok := toolruntime.NewMCPResourceReader(allowed, func(ctx context.Context, serverName, uri string) ([]harnessruntime.MCPResourceContent, error) {
		called.Add(1)
		return []harnessruntime.MCPResourceContent{
			{URI: uri, MIMEType: "text/plain", Text: "hello from mcp"},
		}, nil
	})
	require.True(t, ok)
	require.NotNil(t, readerTool)

	spec := readerTool.HostToolSpec()
	assert.Equal(t, toolruntime.MCPResourceToolName, spec.Name)
	assert.Equal(t, "sofa_read_mcp_resource", spec.Name)

	handler := readerTool.Handler()

	// Unauthorized URI rejected without calling plugin.
	deniedOut, err := handler(t.Context(), harnessruntime.ToolInvocation{
		ToolCallID:    "tc-1",
		ToolName:      toolruntime.MCPResourceToolName,
		ArgumentsJSON: []byte(`{"server_name":"docs","uri":"doc://secrets"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, harnessruntime.HostToolResultFailure, deniedOut.ResultType)
	assert.NotEmpty(t, deniedOut.HandlerError)
	assert.Equal(t, int32(0), called.Load())

	// Authorized (server_name, uri) succeeds.
	okOut, err := handler(t.Context(), harnessruntime.ToolInvocation{
		ToolCallID:    "tc-2",
		ToolName:      toolruntime.MCPResourceToolName,
		ArgumentsJSON: []byte(`{"server_name":"docs","uri":"doc://readme"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, okOut.ResultType)
	assert.Empty(t, okOut.HandlerError)
	assert.Contains(t, okOut.TextResultForLLM, "hello from mcp")
	assert.Equal(t, int32(1), called.Load())
}
