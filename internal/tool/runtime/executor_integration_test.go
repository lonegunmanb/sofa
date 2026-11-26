//go:build integration

package runtime_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/plan"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const integrationAlpineImage = "alpine:latest"

func TestExecutorIntegrationPodmanGoToolAndExternalTool(t *testing.T) {
	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(t.Context())
	require.NoError(t, err)
	require.Equal(t, "linux", platform.OS)

	ws := t.TempDir()
	importsDir := t.TempDir()
	toolsDir := filepath.Join(t.TempDir(), "tools")

	const srcText = "func Verify() string {\n\treturn \"trusted-podman-quote\"\n}\n"
	const importText = "Spec Requirement: Podman container must have no Go compiler.\n"
	require.NoError(t, os.WriteFile(filepath.Join(ws, "src.txt"), []byte(srcText), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(importsDir, "spec.md"), []byte(importText), 0o644))

	goToolSource := `
import (
	"context"
	"os"
	"strings"
)

type Input struct {
	Fabricate bool ` + "`json:\"fabricate\"`" + `
}

type QuoteItem struct {
	Path       string ` + "`json:\"path\"`" + `
	ExactQuote string ` + "`json:\"exact_quote\"`" + `
}

type Output struct {
	Summary string      ` + "`json:\"summary\"`" + `
	Quotes  []QuoteItem ` + "`json:\"quotes\"`" + `
}

func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
	srcBytes, err := os.ReadFile("/workspace/src.txt")
	if err != nil {
		return ToolResponse[Output]{}, err
	}
	impBytes, err := os.ReadFile("/.sofa/imports/spec.md")
	if err != nil {
		return ToolResponse[Output]{}, err
	}
	quote1 := "return \"trusted-podman-quote\""
	if in.Fabricate {
		quote1 = "FABRICATED_LINE_NOT_IN_FILE"
	}
	if !in.Fabricate && (!strings.Contains(string(srcBytes), quote1) || !strings.Contains(string(impBytes), "Podman container must have no Go compiler.")) {
		return ToolResponse[Output]{
			Accepted: false,
			Issues:   []Issue{{Code: "read_mismatch", Message: "container files unexpected"}},
		}, nil
	}
	out := Output{
		Summary: "verified",
		Quotes: []QuoteItem{
			{Path: "/workspace/src.txt", ExactQuote: quote1},
			{Path: "/.sofa/imports/spec.md", ExactQuote: "Podman container must have no Go compiler."},
		},
	}
	return ToolResponse[Output]{Accepted: true, Output: &out}, nil
}
`
	goSpec := buildTestGoToolSpec(
		t,
		"tool_go_tool_podman_verify_12345678-1234-8234-9234-123456789abc",
		"podman_verify",
		goToolSource,
		[][]string{{"quotes", "[]"}},
		[]corespec.Condition{
			{
				Expression:   `output.summary == "verified" && length(output.quotes) == 2`,
				ErrorMessage: "must verify two quotes",
			},
		},
	)

	compiler := toolruntime.NewCompiler(toolruntime.CompilerOptions{
		CacheDir:   t.TempDir(),
		TargetOS:   platform.OS,
		TargetArch: platform.Arch,
	})
	compiled, err := compiler.Compile(t.Context(), goSpec)
	require.NoError(t, err)
	require.NoError(t, toolruntime.StageToolsDir(t.Context(), toolsDir, []toolruntime.CompiledTool{compiled}))

	cid, err := eng.StartDetached(t.Context(), sandboxruntime.SessionMountLayout{
		Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: integrationAlpineImage, Network: "none"},
		HostOS:           "linux",
		Entrypoint:       "sleep",
		Args:             []string{"30"},
		HostWorkspaceDir: ws,
		ImportsDir:       importsDir,
		ToolsDir:         toolsDir,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = eng.RemoveContainer(cleanupCtx, cid)
	})

	// Confirm alpine:latest truly has no Go compiler installed.
	goCheck, err := eng.Exec(t.Context(), cid, ".", []string{"sh", "-c", "command -v go"}, nil, nil)
	require.NoError(t, err)
	assert.NotEqual(t, 0, goCheck.ExitCode, "alpine container must not have go installed")

	exec := toolruntime.NewExecutor(toolruntime.ExecutorOptions{
		WorkspaceDir: ws,
		ImportsDir:   importsDir,
		Exec:         toolruntime.NewPodmanContainerExec(eng, cid),
	})

	// 1. go_tool reads /workspace/src.txt and /.sofa/imports/spec.md inside alpine and passes QuotePaths + postcondition.
	goHandler := exec.Handler(toolruntime.ToolBinding{Spec: goSpec})
	okOut, err := goHandler(t.Context(), harnessruntime.ToolInvocation{
		ToolCallID:    "tc-go-1",
		ToolName:      goSpec.ID,
		ArgumentsJSON: []byte(`{"fabricate":false}`),
	})
	require.NoError(t, err)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, okOut.ResultType)
	assert.Empty(t, okOut.HandlerError)
	assert.Contains(t, okOut.TextResultForLLM, `"accepted":true`)

	// 2. Fabricated quote is caught by host QuotePaths verification and downgraded cleanly.
	fabOut, err := goHandler(t.Context(), harnessruntime.ToolInvocation{
		ToolCallID:    "tc-go-2",
		ToolName:      goSpec.ID,
		ArgumentsJSON: []byte(`{"fabricate":true}`),
	})
	require.NoError(t, err)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, fabOut.ResultType)
	assert.Empty(t, fabOut.HandlerError)
	assert.Contains(t, fabOut.TextResultForLLM, `"accepted":false`)
	assert.Contains(t, fabOut.TextResultForLLM, `quote_verification_failed`)

	// 3. external_tool runs /bin/sh -c inside alpine:latest reading stdin JSON & returning validated OutputType JSON.
	extInType := cty.Object(map[string]cty.Type{"tag": cty.String})
	extOutType := cty.Object(map[string]cty.Type{"received": cty.String, "env": cty.String})
	extSchema, err := toolspec.NewConstraint(extInType).JSONSchema()
	require.NoError(t, err)
	extSchemaBytes, _ := json.Marshal(extSchema)
	extInBytes, _ := ctyjson.MarshalType(extInType)
	extOutBytes, _ := ctyjson.MarshalType(extOutType)

	extSpec := plan.ToolSpec{
		ID:          "tool_external_tool_sh_12345678-1234-8234-9234-123456789abc",
		Name:        "sh_echo",
		Address:     "external_tool.sh_echo",
		Kind:        "external",
		Description: "shell json echo",
		Program:     []string{"/bin/sh", "-c", `body=$(cat); printf '{"received":%s,"env":"%s"}' "$body" "$EXT_MODE"`},
		InputSchema: extSchemaBytes,
		InputType:   extInBytes,
		OutputType:  extOutBytes,
		Postconditions: []corespec.Condition{
			{Expression: `output.env == "prod"`, ErrorMessage: "env must be prod"},
		},
	}
	// Note: $body is a JSON object, so let's make extOutType match object or extract string!
	extOutType = cty.Object(map[string]cty.Type{
		"payload": cty.Object(map[string]cty.Type{"tag": cty.String}),
		"env":     cty.String,
	})
	extOutBytes, _ = ctyjson.MarshalType(extOutType)
	extSpec.OutputType = extOutBytes
	extSpec.Program = []string{"/bin/sh", "-c", `body=$(cat); printf '{"payload":%s,"env":"%s"}' "$body" "$EXT_MODE"`}

	extRes, err := exec.Invoke(t.Context(), toolruntime.ToolBinding{
		Spec: extSpec,
		Env:  map[string]string{"EXT_MODE": "prod"},
	}, []byte(`{"tag":"alpine-ok"}`))
	require.NoError(t, err)
	assert.True(t, extRes.Accepted)
	assert.Empty(t, extRes.ToolOutput.HandlerError)
	assert.Equal(t, harnessruntime.HostToolResultSuccess, extRes.ToolOutput.ResultType)
	assert.Equal(t, "alpine-ok", extRes.ValidatedOutput.GetAttr("payload").GetAttr("tag").AsString())
	assert.Equal(t, "prod", extRes.ValidatedOutput.GetAttr("env").AsString())
}
