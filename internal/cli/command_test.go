package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/sofa/internal/cli"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeReleaseFetcher struct {
	assets map[string][]byte
}

func (f *fakeReleaseFetcher) FetchReleaseAsset(_ context.Context, _, _, _, _, filename, _ string) ([]byte, error) {
	data, ok := f.assets[filename]
	if !ok {
		return nil, fmt.Errorf("asset %q not found", filename)
	}
	return data, nil
}

func TestNormalizeArgsRewritesSingleDashVarFlagsBeforeDelimiter(t *testing.T) {
	t.Parallel()

	in := []string{
		"plan",
		"-var", "a=1",
		"-var=b=2",
		"-var-file", "x.sofavars",
		"-var-file=y.sofavars",
		"--",
		"-var", "literal=kept",
	}
	got := cli.NormalizeArgs(in)
	assert.Equal(t, []string{
		"plan",
		"--var", "a=1",
		"--var=b=2",
		"--var-file", "x.sofavars",
		"--var-file=y.sofavars",
		"--",
		"-var", "literal=kept",
	}, got)
	// Must not mutate input slice.
	assert.Equal(t, "-var", in[1])
}

func TestEndToEndInitSchemaPlanArchiveAndLoad(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	srcDir := filepath.Join(workspace, "workflow")
	childDir := filepath.Join(srcDir, "modules", "audit")
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "skills", "golang-tdd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "skills", "golang-tdd", "SKILL.md"), []byte("# TDD\n"), 0o600))
	require.NoError(t, os.MkdirAll(childDir, 0o700))

	writeCLIFile(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "branch_in" {
  type = string
}

worker "review" {
  based_source  = var.branch_in
  model         = "gpt-5"
  system_prompt = "Review changes."
}

output "reviewed" {
  value = worker.review.work_branch
}
`)

	writeCLIFile(t, filepath.Join(srcDir, "main.sofa.hcl"), `
variable "model" {
  description = "Model ID"
  type        = string
}

variable "note" {
  type    = string
  default = "from-default"
}

variable "api_secret" {
  type      = string
  sensitive = true
  default   = "schema-secret-default"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "none"
}

model_provider "openai" {
  type     = "openai"
  endpoint = "https://api.openai.com/v1"
  api_key  = var.api_secret
}

worker "implement" {
  based_source      = source.project.branch
  model             = var.model
  system_prompt     = "Implement with ${var.note}"
  skill_directories = ["${path.module}/skills"]
}

module "audit" {
  source    = "./modules/audit"
  branch_in = worker.implement.work_branch
}

output "final_branch" {
  value = module.audit.reviewed
}

output "effective_model" {
  value = var.model
}

output "effective_note" {
  value = var.note
}
`)

	// Var files in workspace working dir to test interleaved -var and -var-file precedence.
	writeCLIFile(t, filepath.Join(workspace, "first.sofavars"), `
model = "model-from-first-file"
note  = "note-from-first-file"
`)
	writeCLIFile(t, filepath.Join(workspace, "second.sofavars"), `
note = "note-from-second-file"
`)

	sumA := sha256.Sum256([]byte("amd64-archive"))
	sumB := sha256.Sum256([]byte("arm64-archive"))
	checksums := fmt.Sprintf(
		"%s  sofa-harness-antigravity_linux_amd64.zip\n%s  sofa-harness-antigravity_linux_arm64.zip\n",
		hex.EncodeToString(sumA[:]),
		hex.EncodeToString(sumB[:]),
	)
	opts := cli.Options{
		WorkingDir: workspace,
		ReleaseFetcher: &fakeReleaseFetcher{
			assets: map[string][]byte{"checksums.txt": []byte(checksums)},
		},
	}

	// 1. sofa init
	var initOut, initErr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "./workflow", "--state-dir", ".sofa"}, strings.NewReader(""), &initOut, &initErr, opts)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", initErr.String())
	assert.Empty(t, initErr.String())
	assert.Contains(t, initOut.String(), "Initialized")

	// 2. sofa schema (text + --json) without providing required variable values
	var schemaTextOut, schemaTextErr bytes.Buffer
	code = cli.Run(t.Context(), []string{"schema", "--state-dir", ".sofa"}, strings.NewReader(""), &schemaTextOut, &schemaTextErr, opts)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", schemaTextErr.String())
	assert.Contains(t, schemaTextOut.String(), "model (string)")
	assert.Contains(t, schemaTextOut.String(), "<sensitive>")
	assert.NotContains(t, schemaTextOut.String(), "schema-secret-default")

	var schemaJSONOut, schemaJSONErr bytes.Buffer
	code = cli.Run(t.Context(), []string{"schema", "--state-dir", ".sofa", "--json"}, strings.NewReader(""), &schemaJSONOut, &schemaJSONErr, opts)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", schemaJSONErr.String())
	assert.NotContains(t, schemaJSONOut.String(), "schema-secret-default")
	var schemaDoc struct {
		SchemaVersion int `json:"schema_version"`
		Variables     []struct {
			Name            string `json:"name"`
			Required        bool   `json:"required"`
			DefaultRedacted bool   `json:"default_redacted"`
		} `json:"variables"`
	}
	require.NoError(t, json.Unmarshal(schemaJSONOut.Bytes(), &schemaDoc))
	assert.Equal(t, 1, schemaDoc.SchemaVersion)
	require.Len(t, schemaDoc.Variables, 3)

	// 3. sofa plan with interleaved -var and -var-file:
	//    -var model=initial -> -var-file first.sofavars (sets model & note) -> -var model=final-model -> -var-file second.sofavars (sets note)
	var planOut, planErr bytes.Buffer
	code = cli.Run(t.Context(), []string{
		"plan",
		"--state-dir", ".sofa",
		"-var", "model=initial",
		"-var-file", "first.sofavars",
		"-var", "model=final-model",
		"-var-file", "second.sofavars",
		"--out", "workflow.sofaplan",
	}, strings.NewReader(""), &planOut, &planErr, opts)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", planErr.String())

	// Stdout must contain redacted Display JSON ("<sensitive>" and "<known after apply>") and no secret plaintext.
	assert.NotContains(t, planOut.String(), "schema-secret-default")
	assert.Contains(t, planOut.String(), `"<sensitive>"`)
	assert.Contains(t, planOut.String(), `"<known after apply>"`)
	assert.Contains(t, planOut.String(), `"final-model"`)
	assert.Contains(t, planOut.String(), `"note-from-second-file"`)

	// Stderr must warn that the saved archive is unencrypted and contains sensitive values.
	assert.Contains(t, planErr.String(), "warning:")
	assert.Contains(t, planErr.String(), "unencrypted")
	assert.Contains(t, planErr.String(), "sensitive")

	// 4. LoadArchive into a brand-new temp dir and verify Plan + bundle + plugins.lock.json.
	archivePath := filepath.Join(workspace, "workflow.sofaplan")
	extractDir := t.TempDir()
	loaded, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)
	require.NotNil(t, loaded.PluginLock)
	assert.Equal(t, "antigravity", loaded.PluginLock.Plugins[0].Name)
	assert.Equal(t, "final-model", loaded.Plan.Outputs()["effective_model"].Value.AsString())
	assert.Equal(t, "note-from-second-file", loaded.Plan.Outputs()["effective_note"].Value.AsString())
	skillBytes, err := os.ReadFile(filepath.Join(loaded.ConfigDir, "skills", "golang-tdd", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# TDD\n", string(skillBytes))
}

func TestCleanPlanArchiveOmitsSensitiveWarningAndResolvesConfigDirVarFile(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	srcDir := filepath.Join(workspace, "workflow")
	require.NoError(t, os.MkdirAll(srcDir, 0o700))
	writeCLIFile(t, filepath.Join(srcDir, "main.sofa.hcl"), `
variable "model" {
  type = string
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

worker "implement" {
  based_source  = source.project.branch
  model         = var.model
  system_prompt = "Implement cleanly."
}
`)
	writeCLIFile(t, filepath.Join(srcDir, "cfg.sofavars"), `model = "from-config-dir"`)

	opts := cli.Options{WorkingDir: workspace}
	var out, errBuf bytes.Buffer
	require.Equal(t, cli.ExitSuccess, cli.Run(t.Context(), []string{"init", "./workflow"}, strings.NewReader(""), &out, &errBuf, opts))

	out.Reset()
	errBuf.Reset()
	code := cli.Run(t.Context(), []string{"plan", "-var-file", "cfg.sofavars", "--out", "clean.sofaplan"}, strings.NewReader(""), &out, &errBuf, opts)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", errBuf.String())
	assert.Empty(t, errBuf.String())
}

func TestCLIErrorHandlingAndExitCodes(t *testing.T) {
	t.Parallel()

	t.Run("plan and schema before init return ExitFailure with sofa init guidance", func(t *testing.T) {
		t.Parallel()
		opts := cli.Options{WorkingDir: t.TempDir()}

		var out, errBuf bytes.Buffer
		code := cli.Run(t.Context(), []string{"plan"}, strings.NewReader(""), &out, &errBuf, opts)
		assert.Equal(t, cli.ExitFailure, code)
		assert.Contains(t, errBuf.String(), "sofa init")

		out.Reset()
		errBuf.Reset()
		code = cli.Run(t.Context(), []string{"schema"}, strings.NewReader(""), &out, &errBuf, opts)
		assert.Equal(t, cli.ExitFailure, code)
		assert.Contains(t, errBuf.String(), "sofa init")
	})

	t.Run("invalid flags and bad -var syntax return ExitUsage", func(t *testing.T) {
		t.Parallel()
		opts := cli.Options{WorkingDir: t.TempDir()}

		for _, args := range [][]string{
			{"plan", "--unknown-flag"},
			{"plan", "-var", "missing_equals"},
			{"plan", "-var", "=empty_name"},
			{"schema", "unexpected-arg"},
			{"init", "a", "b"},
		} {
			var out, errBuf bytes.Buffer
			code := cli.Run(t.Context(), args, strings.NewReader(""), &out, &errBuf, opts)
			assert.Equal(t, cli.ExitUsage, code, "args: %v, stderr: %s", args, errBuf.String())
		}
	})

	t.Run("canceled context returns 130 and hcl diagnostics format cleanly", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var out, errBuf bytes.Buffer
		code := cli.Run(ctx, []string{"init"}, strings.NewReader(""), &out, &errBuf, cli.Options{WorkingDir: t.TempDir()})
		assert.Equal(t, 130, code)

		// Verify hcl.Diagnostics formatting helper path on bad HCL during init/plan.
		badDir := t.TempDir()
		writeCLIFile(t, filepath.Join(badDir, "bad.sofa.hcl"), `worker "broken" {`)
		_, diags := hclsyntax.ParseConfig([]byte(`worker "broken" {`), "bad.sofa.hcl", hcl.InitialPos)
		require.True(t, diags.HasErrors())

		out.Reset()
		errBuf.Reset()
		code = cli.Run(t.Context(), []string{"init", badDir}, strings.NewReader(""), &out, &errBuf, cli.Options{WorkingDir: t.TempDir()})
		assert.Equal(t, cli.ExitFailure, code)
		assert.Contains(t, errBuf.String(), "error:")
	})
}

func writeCLIFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o600))
}
