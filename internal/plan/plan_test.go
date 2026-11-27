package plan_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/plan"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestPlanInvariantsAndDefensiveCloning(t *testing.T) {
	t.Parallel()

	child, err := plan.New(plan.Args{
		Directory:  "/tmp/child",
		BundlePath: "modules/audit",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.audit",
				CanonicalAddress: "module.audit.worker.audit",
				Kind:             "worker",
				Config:           cty.ObjectVal(map[string]cty.Value{"model": cty.StringVal("gpt-5")}),
			},
		},
	})
	require.NoError(t, err)

	p := 4
	timeout := 30 * time.Minute
	desc := "Output desc"
	root, err := plan.New(plan.Args{
		Directory:  "/tmp/root",
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "source.repo",
				CanonicalAddress: "source.repo",
				Kind:             "source",
				Config:           cty.ObjectVal(map[string]cty.Value{"branch": cty.StringVal("main")}),
			},
			{
				Address:          "module.audit",
				CanonicalAddress: "module.audit",
				Kind:             "module",
				Dependencies:     []string{"source.repo"},
				Config:           cty.ObjectVal(map[string]cty.Value{"source": cty.StringVal("./audit")}),
				Module: &plan.ModuleSpec{
					Plan:             child,
					Source:           "./audit",
					CanonicalAddress: "module.audit",
					BundlePath:       "modules/audit",
					Parallelism:      &p,
					Timeout:          &timeout,
					TimeoutRaw:       "30m",
					Inputs:           map[string]cty.Value{"branch": cty.UnknownVal(cty.String)},
					InputExprs:       map[string]string{"branch": "source.repo.branch"},
				},
			},
		},
		Outputs: map[string]plan.OutputSpec{
			"res": {
				Value:       cty.StringVal("ok"),
				Description: &desc,
				Expression:  `"ok"`,
				SourceURLs:  []string{"https://example.com/repo.git"},
			},
		},
		Context:             map[string]cty.Value{"var": cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("1")})},
		LocalExpressions:    map[string]string{"l": "var.x"},
		DeferredValidations: map[string][]corespec.Condition{"b": {{Expression: "var.b != \"\"", ErrorMessage: "empty"}}},
		Tools: map[string]plan.ToolSpec{
			"tool_go_tool_check_12345678-1234-8234-9234-123456789abc": {
				ID:               "tool_go_tool_check_12345678-1234-8234-9234-123456789abc",
				Name:             "check",
				Address:          "go_tool.check",
				CanonicalAddress: "go_tool.check",
				Kind:             "go",
				Description:      "check",
				Source:           "package tool",
				Dependencies:     []string{"example.com/dep"},
			},
		},
		PluginLock: &harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins: []harness.PluginLockEntry{
				{
					Name:              "antigravity",
					Source:            "github.com/lonegunmanb/sofa-harness-antigravity",
					Version:           "0.1.0",
					VerifiedSignature: "none",
					Assets: map[string]harness.PluginAsset{
						"linux_amd64": {
							Filename: "sofa-harness-antigravity_linux_amd64.zip",
							SHA256:   strings.Repeat("a", 64),
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)

	// Mutating returned slices/maps/pointers must not mutate Plan.
	nodes := root.Nodes()
	nodes[1].Dependencies[0] = "mutated"
	*nodes[1].Module.Parallelism = 99
	assert.Equal(t, "source.repo", root.Nodes()[1].Dependencies[0])
	assert.Equal(t, 4, *root.Nodes()[1].Module.Parallelism)

	lock := root.PluginLock()
	lock.Plugins[0].Name = "mutated"
	assert.Equal(t, "antigravity", root.PluginLock().Plugins[0].Name)

	// Invalid invariants
	_, err = plan.New(plan.Args{
		Nodes: []plan.NodeSpec{
			{Address: "source.a", Kind: "source", Config: cty.EmptyObjectVal},
			{Address: "source.a", Kind: "source", Config: cty.EmptyObjectVal},
		},
	})
	require.ErrorContains(t, err, "declared more than once")

	_, err = plan.New(plan.Args{
		Nodes: []plan.NodeSpec{
			{Address: "unknown.a", Kind: "bogus", Config: cty.EmptyObjectVal},
		},
	})
	require.ErrorContains(t, err, "unsupported node kind")

	_, err = plan.New(plan.Args{
		Nodes: []plan.NodeSpec{
			{Address: "module.a", Kind: "module", Config: cty.EmptyObjectVal},
		},
	})
	require.ErrorContains(t, err, "child plan")

	_, err = plan.New(plan.Args{
		Nodes: []plan.NodeSpec{
			{Address: "worker.a", Kind: "worker", Dependencies: []string{"worker.missing"}, Config: cty.EmptyObjectVal},
		},
	})
	require.ErrorContains(t, err, "unknown node")

	_, err = plan.New(plan.Args{
		Nodes: []plan.NodeSpec{
			{Address: "worker.a", Kind: "worker", Dependencies: []string{"worker.b"}, Config: cty.EmptyObjectVal},
			{Address: "worker.b", Kind: "worker", Dependencies: []string{"worker.a"}, Config: cty.EmptyObjectVal},
		},
	})
	require.ErrorContains(t, err, "cycle")

	_, err = plan.New(plan.Args{
		Tools: map[string]plan.ToolSpec{
			"bad_id": {ID: "bad_id", Address: "go_tool.x", Kind: "go"},
		},
	})
	require.ErrorContains(t, err, "invalid")

	_, err = plan.New(plan.Args{
		PluginLock: &harness.PluginLockFile{
			FormatVersion: 99,
		},
	})
	require.ErrorContains(t, err, "format_version")

	_, err = plan.New(plan.Args{
		PluginLock: &harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins: []harness.PluginLockEntry{
				{
					Name:              "antigravity",
					Source:            "github.com/lonegunmanb/sofa-harness-antigravity",
					Version:           "0.1.0",
					VerifiedSignature: "bogus",
				},
			},
		},
	})
	require.ErrorContains(t, err, "verified_signature")
}

func TestPlanRoundTripPreservesUnknownsSensitivityAndNestedModules(t *testing.T) {
	t.Parallel()

	child, err := plan.New(plan.Args{
		Directory:  "/tmp/child",
		BundlePath: "modules/audit",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.audit",
				CanonicalAddress: "module.audit.worker.audit",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"based_source": cty.UnknownVal(cty.String),
					"prompt":       corespec.MarkSensitive(cty.StringVal("child-secret-prompt")),
				}),
			},
		},
		Outputs: map[string]plan.OutputSpec{
			"branch": {
				Value:      cty.UnknownVal(cty.String),
				Expression: "worker.audit.work_branch",
			},
		},
		DeferredValidations: map[string][]corespec.Condition{
			"branch_in": {{Expression: `var.branch_in != ""`, ErrorMessage: "must not be blank"}},
		},
	})
	require.NoError(t, err)

	p := 2
	dur := 15 * time.Minute
	outDesc := "Sensitive branch output"
	original, err := plan.New(plan.Args{
		Directory:  "/tmp/root",
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "model_provider.primary",
				CanonicalAddress: "model_provider.primary",
				Kind:             "model_provider",
				Config: cty.ObjectVal(map[string]cty.Value{
					"endpoint": cty.StringVal("https://api.example.com/v1"),
					"api_key":  corespec.MarkSensitive(cty.StringVal("sk-super-secret")),
					"headers": cty.MapVal(map[string]cty.Value{
						"X-Public": cty.StringVal("ok"),
						"X-Secret": corespec.MarkSensitive(cty.StringVal("hdr-secret")),
					}),
					"tokens": cty.TupleVal([]cty.Value{
						cty.StringVal("public-tok"),
						corespec.MarkSensitive(cty.StringVal("list-secret-tok")),
					}),
					"whole_secret_obj": corespec.MarkSensitive(cty.ObjectVal(map[string]cty.Value{
						"nested": cty.StringVal("obj-secret-inner"),
					})),
				}),
			},
			{
				Address:          "worker.implement",
				CanonicalAddress: "worker.implement",
				Kind:             "worker",
				Dependencies:     []string{"model_provider.primary"},
				Config: cty.ObjectVal(map[string]cty.Value{
					"based_source": cty.UnknownVal(cty.String),
					"meta": cty.ObjectVal(map[string]cty.Value{
						"work_branch": cty.UnknownVal(cty.String),
						"static":      cty.NumberIntVal(7),
					}),
				}),
			},
			{
				Address:          "module.audit",
				CanonicalAddress: "module.audit",
				Kind:             "module",
				Dependencies:     []string{"worker.implement"},
				Config:           cty.ObjectVal(map[string]cty.Value{"source": cty.StringVal("./audit")}),
				Module: &plan.ModuleSpec{
					Plan:             child,
					Source:           "./audit",
					CanonicalAddress: "module.audit",
					BundlePath:       "modules/audit",
					Parallelism:      &p,
					Timeout:          &dur,
					TimeoutRaw:       "15m",
					Inputs: map[string]cty.Value{
						"branch_in": cty.UnknownVal(cty.String),
						"token_in":  corespec.MarkSensitive(cty.StringVal("input-secret")),
					},
					InputExprs: map[string]string{
						"branch_in": "worker.implement.work_branch",
						"token_in":  "var.token",
					},
				},
			},
		},
		Outputs: map[string]plan.OutputSpec{
			"secret_out": {
				Value:       corespec.MarkSensitive(cty.StringVal("output-secret")),
				Description: &outDesc,
				Sensitive:   true,
				Expression:  "var.token",
			},
			"unknown_out": {
				Value:      cty.UnknownVal(cty.String),
				Expression: "worker.implement.work_branch",
			},
		},
		Context: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				"token": corespec.MarkSensitive(cty.StringVal("ctx-secret")),
			}),
		},
		LocalExpressions: map[string]string{"l": "var.token"},
		Tools: map[string]plan.ToolSpec{
			"tool_go_tool_verify_12345678-1234-8234-9234-123456789abc": {
				ID:               "tool_go_tool_verify_12345678-1234-8234-9234-123456789abc",
				Name:             "verify",
				Address:          "go_tool.verify",
				CanonicalAddress: "go_tool.verify",
				Kind:             "go",
				Description:      "verify",
				Source:           "package tool",
				InputSchema:      json.RawMessage(`{"type":"object"}`),
				InputType:        json.RawMessage(`["object",{"ok":"bool"}]`),
				OutputType:       json.RawMessage(`"string"`),
				QuotePaths:       [][]string{{"quote"}},
				Postconditions:   []corespec.Condition{{Expression: "output != \"\"", ErrorMessage: "blank"}},
			},
		},
	})
	require.NoError(t, err)

	encoded, err := plan.Marshal(original)
	require.NoError(t, err)

	restored, err := plan.Unmarshal(encoded)
	require.NoError(t, err)

	assert.Equal(t, original.Directory(), restored.Directory())
	assert.Equal(t, original.BundlePath(), restored.BundlePath())
	assert.Equal(t, original.LocalExpressions(), restored.LocalExpressions())
	assert.Equal(t, original.Tools(), restored.Tools())

	prov := restored.Nodes()[0].Config
	assert.True(t, prov.GetAttr("api_key").HasMark(golden.SensitiveMark))
	assert.True(t, prov.GetAttr("headers").Index(cty.StringVal("X-Secret")).HasMark(golden.SensitiveMark))
	assert.False(t, prov.GetAttr("headers").Index(cty.StringVal("X-Public")).HasMark(golden.SensitiveMark))
	assert.True(t, prov.GetAttr("tokens").Index(cty.NumberIntVal(1)).HasMark(golden.SensitiveMark))
	assert.True(t, prov.GetAttr("whole_secret_obj").HasMark(golden.SensitiveMark))

	impl := restored.Nodes()[1].Config
	assert.False(t, impl.GetAttr("based_source").IsKnown())
	assert.True(t, impl.GetAttr("based_source").Type().Equals(cty.String))
	assert.False(t, impl.GetAttr("meta").GetAttr("work_branch").IsKnown())

	mod := restored.Nodes()[2].Module
	require.NotNil(t, mod)
	assert.False(t, mod.Inputs["branch_in"].IsKnown())
	assert.True(t, mod.Inputs["token_in"].HasMark(golden.SensitiveMark))
	assert.Equal(t, "15m", mod.TimeoutRaw)
	assert.Equal(t, child.DeferredValidations(), mod.Plan.DeferredValidations())
	assert.True(t, mod.Plan.Nodes()[0].Config.GetAttr("prompt").HasMark(golden.SensitiveMark))

	assert.True(t, restored.Outputs()["secret_out"].Value.HasMark(golden.SensitiveMark))
	assert.False(t, restored.Outputs()["unknown_out"].Value.IsKnown())
	assert.True(t, restored.Context()["var"].GetAttr("token").HasMark(golden.SensitiveMark))

	// Display must never leak any secret and must render unknown values as "<known after apply>".
	displayText, err := plan.Display(restored)
	require.NoError(t, err)
	for _, secret := range []string{
		"sk-super-secret", "hdr-secret", "list-secret-tok",
		"obj-secret-inner", "child-secret-prompt", "input-secret", "output-secret", "ctx-secret",
	} {
		assert.NotContains(t, displayText, secret)
	}
	assert.Contains(t, displayText, `"<sensitive>"`)
	assert.Contains(t, displayText, `"<known after apply>"`)
	assert.NotContains(t, displayText, `"<unknown>"`)
	assert.True(t, plan.ContainsSensitiveValues(restored))
}

func TestContainsSensitiveValuesFalseWhenClean(t *testing.T) {
	t.Parallel()

	p, err := plan.New(plan.Args{
		Directory: "/tmp/clean",
		Nodes: []plan.NodeSpec{
			{
				Address: "source.repo",
				Kind:    "source",
				Config:  cty.ObjectVal(map[string]cty.Value{"url": cty.StringVal("https://example.com/r.git")}),
			},
		},
		Outputs: map[string]plan.OutputSpec{
			"url": {Value: cty.StringVal("https://example.com/r.git")},
		},
	})
	require.NoError(t, err)
	assert.False(t, plan.ContainsSensitiveValues(p))
}

func TestDisplayValues(t *testing.T) {
	t.Parallel()

	text, err := plan.DisplayValues(map[string]cty.Value{
		"pub":     cty.StringVal("hello"),
		"secret":  corespec.MarkSensitive(cty.StringVal("top-secret")),
		"pending": cty.UnknownVal(cty.String),
	})
	require.NoError(t, err)
	assert.NotContains(t, text, "top-secret")
	assert.Contains(t, text, `"<sensitive>"`)
	assert.Contains(t, text, `"<known after apply>"`)
}

func TestDisplayStripsInternalExprFieldsByKindWithoutDroppingUserAttrs(t *testing.T) {
	t.Parallel()

	child, err := plan.New(plan.Args{
		Directory:  "/tmp/child",
		BundlePath: "modules/child",
	})
	require.NoError(t, err)

	p, err := plan.New(plan.Args{
		Directory:  "/tmp/root",
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address: "harness.custom",
				Kind:    "harness",
				Config: cty.ObjectVal(map[string]cty.Value{
					"name": cty.StringVal("custom"),
					"options": cty.ObjectVal(map[string]cty.Value{
						"content_template": cty.StringVal("keep-harness-option"),
					}),
				}),
			},
			{
				Address: "module.child",
				Kind:    "module",
				Config: cty.ObjectVal(map[string]cty.Value{
					"source": cty.StringVal("./child"),
					"inputs": cty.ObjectVal(map[string]cty.Value{
						"input_exprs": cty.StringVal("keep-module-input"),
					}),
					"input_exprs": cty.MapVal(map[string]cty.Value{
						"secret": cty.StringVal(`"leak-me"`),
					}),
				}),
				Module: &plan.ModuleSpec{
					Plan:       child,
					Source:     "./child",
					BundlePath: "modules/child",
					Inputs: map[string]cty.Value{
						"input_exprs": cty.StringVal("keep-module-input"),
					},
				},
			},
		},
	})
	require.NoError(t, err)

	displayed, err := plan.Display(p)
	require.NoError(t, err)
	assert.Contains(t, displayed, "keep-harness-option")
	assert.Contains(t, displayed, "keep-module-input")
	assert.NotContains(t, displayed, "leak-me")
}

func TestSaveAndLoadArchiveDeterministicAndCrossDirectory(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	configDir := filepath.Join(stateDir, "config")
	modulesDir := filepath.Join(stateDir, "modules")
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "skills", "golang-tdd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "skills", "golang-tdd", "SKILL.md"), []byte("# TDD\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ".initialized.json"), []byte(`{"ignored":true}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(modulesDir, "audit", "skills", "audit-skill"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(modulesDir, "audit", "skills", "audit-skill", "SKILL.md"), []byte("# Audit\n"), 0o600))

	child, err := plan.New(plan.Args{
		Directory:  filepath.Join(modulesDir, "audit"),
		BundlePath: "modules/audit",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.review",
				CanonicalAddress: "module.audit.worker.review",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("modules/audit/skills")}),
				}),
			},
		},
	})
	require.NoError(t, err)

	lockFile := &harness.PluginLockFile{
		FormatVersion: harness.LockFormatVersion,
		Plugins: []harness.PluginLockEntry{
			{
				Name:              "antigravity",
				Source:            "github.com/lonegunmanb/sofa-harness-antigravity",
				Version:           "0.1.0",
				VerifiedSignature: "none",
				Assets: map[string]harness.PluginAsset{
					"linux_amd64": {
						Filename: "sofa-harness-antigravity_linux_amd64.zip",
						SHA256:   strings.Repeat("b", 64),
					},
				},
			},
		},
	}

	root, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.implement",
				CanonicalAddress: "worker.implement",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("config/skills")}),
					"qc": cty.ObjectVal(map[string]cty.Value{
						"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("config/skills")}),
					}),
				}),
			},
			{
				Address:          "module.audit",
				CanonicalAddress: "module.audit",
				Kind:             "module",
				Config:           cty.ObjectVal(map[string]cty.Value{"source": cty.StringVal("./audit")}),
				Module: &plan.ModuleSpec{
					Plan:             child,
					Source:           "./audit",
					CanonicalAddress: "module.audit",
					BundlePath:       "modules/audit",
				},
			},
		},
		PluginLock: lockFile,
	})
	require.NoError(t, err)

	archiveOne := filepath.Join(t.TempDir(), "one.sofaplan")
	archiveTwo := filepath.Join(t.TempDir(), "two.sofaplan")
	src := plan.ArchiveSource{
		ConfigDir:  configDir,
		ModulesDir: modulesDir,
		PluginLock: lockFile,
	}
	require.NoError(t, plan.SaveArchive(archiveOne, root, src))
	require.NoError(t, plan.SaveArchive(archiveTwo, root, src))

	bytesOne, err := os.ReadFile(archiveOne)
	require.NoError(t, err)
	bytesTwo, err := os.ReadFile(archiveTwo)
	require.NoError(t, err)
	sumOne := sha256.Sum256(bytesOne)
	sumTwo := sha256.Sum256(bytesTwo)
	assert.Equal(t, sumOne, sumTwo, "SaveArchive must produce deterministic bytes")

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(archiveOne)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	extractDir := t.TempDir()
	loaded, err := plan.LoadArchive(archiveOne, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, loaded.Manifest.FormatVersion)
	assert.Equal(t, "sofa", loaded.Manifest.SofaDSL)
	require.NotNil(t, loaded.PluginLock)
	assert.Equal(t, "antigravity", loaded.PluginLock.Plugins[0].Name)

	skillBytes, err := os.ReadFile(filepath.Join(loaded.ConfigDir, "skills", "golang-tdd", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# TDD\n", string(skillBytes))

	modSkillBytes, err := os.ReadFile(filepath.Join(loaded.ModulesDir, "audit", "skills", "audit-skill", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Audit\n", string(modSkillBytes))

	_, err = os.Stat(filepath.Join(loaded.ConfigDir, ".initialized.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestSaveArchiveRejectsMissingSkillDirectoryInBundle(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "main.sofa.hcl"), []byte("# cfg\n"), 0o600))

	p, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.implement",
				CanonicalAddress: "worker.implement",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("config/missing_skills")}),
				}),
			},
		},
	})
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "missing.sofaplan")
	err = plan.SaveArchive(archivePath, p, plan.ArchiveSource{ConfigDir: configDir})
	require.ErrorContains(t, err, "missing_skills")

	dynPlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "dynamic_worker.sweep",
				CanonicalAddress: "dynamic_worker.sweep",
				Kind:             "dynamic_worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"content": cty.ObjectVal(map[string]cty.Value{
						"qc": cty.ObjectVal(map[string]cty.Value{
							"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("config/missing_qc_skills")}),
						}),
					}),
				}),
			},
		},
	})
	require.NoError(t, err)
	err = plan.SaveArchive(filepath.Join(t.TempDir(), "missing-dyn.sofaplan"), dynPlan, plan.ArchiveSource{ConfigDir: configDir})
	require.ErrorContains(t, err, "missing_qc_skills")
}

func TestLoadArchiveSecurityDefenses(t *testing.T) {
	t.Parallel()

	baseConfigDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(baseConfigDir, "main.sofa.hcl"), []byte("# cfg\n"), 0o600))
	basePlan, err := plan.New(plan.Args{
		Directory:  baseConfigDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{Address: "source.repo", Kind: "source", Config: cty.EmptyObjectVal},
		},
	})
	require.NoError(t, err)

	validZip := filepath.Join(t.TempDir(), "valid.sofaplan")
	require.NoError(t, plan.SaveArchive(validZip, basePlan, plan.ArchiveSource{ConfigDir: baseConfigDir}))

	t.Run("rejects tampered plan.json", func(t *testing.T) {
		t.Parallel()
		badZip := filepath.Join(t.TempDir(), "tampered-plan.sofaplan")
		rewriteZip(t, validZip, badZip, func(entries []zipEntry) []zipEntry {
			for i := range entries {
				if entries[i].name == "plan.json" {
					entries[i].data = append(entries[i].data, ' ')
				}
			}
			return entries
		})
		_, loadErr := plan.LoadArchive(badZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "plan_sha256")
	})

	t.Run("rejects tampered bundle file", func(t *testing.T) {
		t.Parallel()
		badZip := filepath.Join(t.TempDir(), "tampered-bundle.sofaplan")
		rewriteZip(t, validZip, badZip, func(entries []zipEntry) []zipEntry {
			for i := range entries {
				if entries[i].name == "bundle/config/main.sofa.hcl" {
					entries[i].data = []byte("# tampered\n")
				}
			}
			return entries
		})
		_, loadErr := plan.LoadArchive(badZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "bundle_sha256")
	})

	t.Run("rejects unlisted plugins.lock.json", func(t *testing.T) {
		t.Parallel()
		badZip := filepath.Join(t.TempDir(), "unlisted-lock.sofaplan")
		rewriteZip(t, validZip, badZip, func(entries []zipEntry) []zipEntry {
			return append(entries, zipEntry{
				name: "plugins.lock.json",
				data: []byte(`{"format_version":1,"plugins":[]}`),
			})
		})
		_, loadErr := plan.LoadArchive(badZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "plugins.lock.json")
	})

	t.Run("rejects zip slip traversal", func(t *testing.T) {
		t.Parallel()
		badZip := filepath.Join(t.TempDir(), "zip-slip.sofaplan")
		rewriteZip(t, validZip, badZip, func(entries []zipEntry) []zipEntry {
			return append(entries, zipEntry{name: "../../evil", data: []byte("boom")})
		})
		_, loadErr := plan.LoadArchive(badZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.Error(t, loadErr)
	})

	t.Run("rejects backslash entry path", func(t *testing.T) {
		t.Parallel()
		badZip := filepath.Join(t.TempDir(), "backslash.sofaplan")
		rewriteZip(t, validZip, badZip, func(entries []zipEntry) []zipEntry {
			return append(entries, zipEntry{name: `bundle\config\evil.txt`, data: []byte("boom")})
		})
		_, loadErr := plan.LoadArchive(badZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.Error(t, loadErr)
	})

	t.Run("rejects symlink zip entry", func(t *testing.T) {
		t.Parallel()
		badZip := filepath.Join(t.TempDir(), "symlink.sofaplan")
		rewriteZip(t, validZip, badZip, func(entries []zipEntry) []zipEntry {
			return append(entries, zipEntry{
				name: "bundle/config/link",
				mode: os.ModeSymlink | 0o777,
				data: []byte("/etc/passwd"),
			})
		})
		_, loadErr := plan.LoadArchive(badZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "symlink")
	})

	t.Run("rejects zip bomb exceeding MaxSingleFileBytes", func(t *testing.T) {
		t.Parallel()
		bigConfigDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(bigConfigDir, "big.bin"), bytes.Repeat([]byte("A"), 4096), 0o600))
		bigPlan, err := plan.New(plan.Args{Directory: bigConfigDir, BundlePath: "config"})
		require.NoError(t, err)
		bigZip := filepath.Join(t.TempDir(), "big.sofaplan")
		require.NoError(t, plan.SaveArchive(bigZip, bigPlan, plan.ArchiveSource{ConfigDir: bigConfigDir}))

		_, loadErr := plan.LoadArchive(bigZip, t.TempDir(), plan.LoadArchiveOptions{MaxSingleFileBytes: 1024})
		require.ErrorContains(t, loadErr, "exceeds")
	})

	t.Run("rejects archive exceeding MaxTotalBytes", func(t *testing.T) {
		t.Parallel()
		_, loadErr := plan.LoadArchive(validZip, t.TempDir(), plan.LoadArchiveOptions{
			MaxSingleFileBytes: 1024 * 1024,
			MaxTotalBytes:      64,
		})
		require.ErrorContains(t, loadErr, "exceeds")
	})

	t.Run("rejects missing or tampered plugins.lock.json when declared in manifest", func(t *testing.T) {
		t.Parallel()
		lock := &harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins: []harness.PluginLockEntry{
				{
					Name:              "antigravity",
					Source:            "github.com/lonegunmanb/sofa-harness-antigravity",
					Version:           "0.1.0",
					VerifiedSignature: "none",
					Assets: map[string]harness.PluginAsset{
						"linux_amd64": {
							Filename: "sofa-harness-antigravity_linux_amd64.zip",
							SHA256:   strings.Repeat("a", 64),
						},
					},
				},
			},
		}
		lockZip := filepath.Join(t.TempDir(), "with-lock.sofaplan")
		require.NoError(t, plan.SaveArchive(lockZip, basePlan, plan.ArchiveSource{
			ConfigDir:  baseConfigDir,
			PluginLock: lock,
		}))

		missingLockZip := filepath.Join(t.TempDir(), "missing-lock.sofaplan")
		rewriteZip(t, lockZip, missingLockZip, func(entries []zipEntry) []zipEntry {
			filtered := entries[:0]
			for _, e := range entries {
				if e.name != "plugins.lock.json" {
					filtered = append(filtered, e)
				}
			}
			return filtered
		})
		_, loadErr := plan.LoadArchive(missingLockZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "missing plugins.lock.json")

		tamperedLockZip := filepath.Join(t.TempDir(), "tampered-lock.sofaplan")
		rewriteZip(t, lockZip, tamperedLockZip, func(entries []zipEntry) []zipEntry {
			for i := range entries {
				if entries[i].name == "plugins.lock.json" {
					entries[i].data = append(entries[i].data, ' ')
				}
			}
			return entries
		})
		_, loadErr = plan.LoadArchive(tamperedLockZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "plugins_lock_sha256")
	})

	t.Run("rejects malformed plugins.lock.json payload", func(t *testing.T) {
		t.Parallel()
		badLockBytes := []byte(`{"format_version": 99, "plugins": []}` + "\n")
		badLockSum := sha256.Sum256(badLockBytes)
		badLockZip := filepath.Join(t.TempDir(), "malformed-lock.sofaplan")
		rewriteZip(t, validZip, badLockZip, func(entries []zipEntry) []zipEntry {
			for i := range entries {
				if entries[i].name == "manifest.json" {
					var m plan.Manifest
					require.NoError(t, json.Unmarshal(entries[i].data, &m))
					m.PluginsLockSHA256 = hex.EncodeToString(badLockSum[:])
					updated, err := json.MarshalIndent(m, "", "  ")
					require.NoError(t, err)
					entries[i].data = append(updated, '\n')
				}
			}
			return append(entries, zipEntry{name: "plugins.lock.json", data: badLockBytes})
		})
		_, loadErr := plan.LoadArchive(badLockZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "format_version")
	})

	t.Run("rejects archive with remote harness unsatisfied by lockfile", func(t *testing.T) {
		t.Parallel()
		remoteHarnessPlan, err := plan.New(plan.Args{
			Directory:  baseConfigDir,
			BundlePath: "config",
			Nodes: []plan.NodeSpec{
				{
					Address:          "harness.antigravity",
					CanonicalAddress: "harness.antigravity",
					Kind:             "harness",
					Config: cty.ObjectVal(map[string]cty.Value{
						"name":      cty.StringVal("antigravity"),
						"source":    cty.StringVal("github.com/lonegunmanb/sofa-harness-antigravity"),
						"version":   cty.StringVal("0.1.0"),
						"signature": cty.StringVal("none"),
					}),
				},
			},
		})
		require.NoError(t, err)
		unlockedZip := filepath.Join(t.TempDir(), "unlocked-harness.sofaplan")
		require.NoError(t, plan.SaveArchive(unlockedZip, remoteHarnessPlan, plan.ArchiveSource{ConfigDir: baseConfigDir}))
		_, loadErr := plan.LoadArchive(unlockedZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "antigravity")

		mismatchLock := &harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins: []harness.PluginLockEntry{
				{
					Name:              "antigravity",
					Source:            "github.com/lonegunmanb/sofa-harness-antigravity",
					Version:           "0.2.0",
					VerifiedSignature: "none",
					Assets: map[string]harness.PluginAsset{
						"linux_amd64": {
							Filename: "sofa-harness-antigravity_linux_amd64.zip",
							SHA256:   strings.Repeat("c", 64),
						},
					},
				},
			},
		}
		mismatchZip := filepath.Join(t.TempDir(), "mismatch-harness.sofaplan")
		require.NoError(t, plan.SaveArchive(mismatchZip, remoteHarnessPlan, plan.ArchiveSource{
			ConfigDir:  baseConfigDir,
			PluginLock: mismatchLock,
		}))
		_, loadErr = plan.LoadArchive(mismatchZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "lockfile mismatch")
	})

	t.Run("rejects archive with unknown harness lock attribute", func(t *testing.T) {
		t.Parallel()
		unknownHarnessPlan, err := plan.New(plan.Args{
			Directory:  baseConfigDir,
			BundlePath: "config",
			Nodes: []plan.NodeSpec{
				{
					Address:          "harness.antigravity",
					CanonicalAddress: "harness.antigravity",
					Kind:             "harness",
					Config: cty.ObjectVal(map[string]cty.Value{
						"name":   cty.StringVal("antigravity"),
						"source": cty.UnknownVal(cty.String),
					}),
				},
			},
		})
		require.NoError(t, err)
		unknownZip := filepath.Join(t.TempDir(), "unknown-harness.sofaplan")
		require.NoError(t, plan.SaveArchive(unknownZip, unknownHarnessPlan, plan.ArchiveSource{ConfigDir: baseConfigDir}))
		_, loadErr := plan.LoadArchive(unknownZip, t.TempDir(), plan.LoadArchiveOptions{})
		require.ErrorContains(t, loadErr, "unknown")
	})
}

func TestSaveArchiveExcludesOrphanedModulesAndOverwritesExistingArchive(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	modulesDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "main.sofa.hcl"), []byte("# root\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(modulesDir, "orphan"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(modulesDir, "orphan", "stale.txt"), []byte("stale"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(modulesDir, "active"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(modulesDir, "active", "main.sofa.hcl"), []byte("# active\n"), 0o600))

	// 1. Plan with zero module nodes must not bundle orphaned ModulesDir content,
	//    and skill_directories = ["modules"] must fail when no modules are bundled.
	noModulePlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{Address: "source.repo", Kind: "source", Config: cty.EmptyObjectVal},
		},
	})
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "workflow.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, noModulePlan, plan.ArchiveSource{
		ConfigDir:  configDir,
		ModulesDir: modulesDir,
	}))
	loaded, err := plan.LoadArchive(archivePath, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	_, orphanErr := os.Stat(filepath.Join(loaded.ModulesDir, "orphan", "stale.txt"))
	assert.ErrorIs(t, orphanErr, os.ErrNotExist)
	_, activeErr := os.Stat(filepath.Join(loaded.ModulesDir, "active", "main.sofa.hcl"))
	assert.ErrorIs(t, activeErr, os.ErrNotExist)

	badSkillNoModPlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address: "worker.w",
				Kind:    "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("modules")}),
				}),
			},
		},
	})
	require.NoError(t, err)
	err = plan.SaveArchive(filepath.Join(t.TempDir(), "bad-modules-skill.sofaplan"), badSkillNoModPlan, plan.ArchiveSource{
		ConfigDir:  configDir,
		ModulesDir: modulesDir,
	})
	require.ErrorContains(t, err, "modules")

	// 2. Plan referencing only "active" module excludes "orphan" and cleanly overwrites existing archivePath.
	childPlan, err := plan.New(plan.Args{
		Directory:  filepath.Join(modulesDir, "active"),
		BundlePath: "modules/active",
	})
	require.NoError(t, err)
	activeModPlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address: "module.active",
				Kind:    "module",
				Config:  cty.ObjectVal(map[string]cty.Value{"source": cty.StringVal("./modules/active")}),
				Module: &plan.ModuleSpec{
					Plan:       childPlan,
					Source:     "./modules/active",
					BundlePath: "modules/active",
				},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, plan.SaveArchive(archivePath, activeModPlan, plan.ArchiveSource{
		ConfigDir:  configDir,
		ModulesDir: modulesDir,
	}))
	loadedActive, err := plan.LoadArchive(archivePath, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	_, orphanErr = os.Stat(filepath.Join(loadedActive.ModulesDir, "orphan", "stale.txt"))
	assert.ErrorIs(t, orphanErr, os.ErrNotExist)
	activeBytes, err := os.ReadFile(filepath.Join(loadedActive.ModulesDir, "active", "main.sofa.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "# active\n", string(activeBytes))
}

type zipEntry struct {
	name string
	mode os.FileMode
	data []byte
}

func rewriteZip(t *testing.T, srcPath, dstPath string, mutate func([]zipEntry) []zipEntry) {
	t.Helper()
	reader, err := zip.OpenReader(srcPath)
	require.NoError(t, err)
	entries := make([]zipEntry, 0, len(reader.File))
	for _, f := range reader.File {
		rc, openErr := f.Open()
		require.NoError(t, openErr)
		var buf bytes.Buffer
		_, copyErr := buf.ReadFrom(rc)
		_ = rc.Close()
		require.NoError(t, copyErr)
		entries = append(entries, zipEntry{name: f.Name, mode: f.Mode(), data: buf.Bytes()})
	}
	require.NoError(t, reader.Close())

	entries = mutate(entries)

	out, err := os.Create(dstPath)
	require.NoError(t, err)
	zw := zip.NewWriter(out)
	for _, entry := range entries {
		hdr := &zip.FileHeader{
			Name:   entry.name,
			Method: zip.Deflate,
		}
		if entry.mode != 0 {
			hdr.SetMode(entry.mode)
		}
		w, createErr := zw.CreateHeader(hdr)
		require.NoError(t, createErr)
		_, writeErr := w.Write(entry.data)
		require.NoError(t, writeErr)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, out.Close())
}

func TestArchivePreservesPermissionsAndIncludesModeInBundleSHA256(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "scripts"), 0o755))
	plainPath := filepath.Join(configDir, "main.sofa.hcl")
	shebangPath := filepath.Join(configDir, "scripts", "run.sh")
	require.NoError(t, os.WriteFile(plainPath, []byte("# comment\n"), 0o600))
	// Write shebang file WITHOUT host +x bit (0600 / 0644).
	require.NoError(t, os.WriteFile(shebangPath, []byte("#!/usr/bin/env bash\necho hi\n"), 0o644))

	p, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		PluginLock: &harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins:       []harness.PluginLockEntry{},
		},
	})
	require.NoError(t, err)

	lock := &harness.PluginLockFile{
		FormatVersion: harness.LockFormatVersion,
		Plugins: []harness.PluginLockEntry{
			{
				Name:              "local",
				Source:            "github.com/example/local",
				Version:           "0.1.0",
				VerifiedSignature: "none",
				Assets: map[string]harness.PluginAsset{
					"linux_amd64": {
						Filename: "sofa-harness-local_linux_amd64.zip",
						SHA256:   strings.Repeat("a", 64),
					},
				},
			},
		},
	}

	archivePath := filepath.Join(t.TempDir(), "perms.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, p, plan.ArchiveSource{
		ConfigDir:  configDir,
		PluginLock: lock,
	}))

	zr, err := zip.OpenReader(archivePath)
	require.NoError(t, err)
	defer func() { _ = zr.Close() }()

	modesByName := make(map[string]fs.FileMode, len(zr.File))
	for _, f := range zr.File {
		modesByName[f.Name] = f.Mode()
	}
	assert.Equal(t, fs.ModeDir|0o755, modesByName["bundle/config/scripts/"])
	assert.Equal(t, fs.FileMode(0o644), modesByName["bundle/config/main.sofa.hcl"])
	assert.Equal(t, fs.FileMode(0o755), modesByName["bundle/config/scripts/run.sh"])
	assert.Equal(t, fs.FileMode(0o644), modesByName["plugins.lock.json"])

	extractDir := t.TempDir()
	loaded, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Join(loaded.ConfigDir, "scripts"))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o755), dirInfo.Mode().Perm())

		plainInfo, err := os.Stat(filepath.Join(loaded.ConfigDir, "main.sofa.hcl"))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o644), plainInfo.Mode().Perm())

		scriptInfo, err := os.Stat(filepath.Join(loaded.ConfigDir, "scripts", "run.sh"))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o755), scriptInfo.Mode().Perm())

		lockInfo, err := os.Stat(filepath.Join(loaded.ExtractDir, "plugins.lock.json"))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o644), lockInfo.Mode().Perm())
	}

	// Changing a file's mode (e.g. turning plain text into shebang) changes BundleSHA256 even if
	// we compare against another archive whose zip header has 0755 vs 0644 on identical bytes.
	tamperedModeZip := filepath.Join(t.TempDir(), "tampered-mode.sofaplan")
	rewriteZip(t, archivePath, tamperedModeZip, func(entries []zipEntry) []zipEntry {
		for i := range entries {
			if entries[i].name == "bundle/config/main.sofa.hcl" {
				entries[i].mode = 0o755
			}
		}
		return entries
	})
	_, loadErr := plan.LoadArchive(tamperedModeZip, t.TempDir(), plan.LoadArchiveOptions{})
	require.ErrorContains(t, loadErr, "bundle_sha256")

	// Also verify BundleSHA256 differs between 0644 and 0755 on identical non-shebang file (on Unix via chmod,
	// and everywhere via zip mode rewriting / manifest check).
	if runtime.GOOS != "windows" {
		hostExecDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(hostExecDir, "run.bin"), []byte("binary-ish"), 0o755))
		hostExecPlan, err := plan.New(plan.Args{Directory: hostExecDir, BundlePath: "config"})
		require.NoError(t, err)
		hostExecArchive := filepath.Join(t.TempDir(), "host-exec.sofaplan")
		require.NoError(t, plan.SaveArchive(hostExecArchive, hostExecPlan, plan.ArchiveSource{ConfigDir: hostExecDir}))

		loadedExec, err := plan.LoadArchive(hostExecArchive, t.TempDir(), plan.LoadArchiveOptions{})
		require.NoError(t, err)
		execInfo, err := os.Stat(filepath.Join(loadedExec.ConfigDir, "run.bin"))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o755), execInfo.Mode().Perm())

		require.NoError(t, os.Chmod(filepath.Join(hostExecDir, "run.bin"), 0o644))
		hostPlainArchive := filepath.Join(t.TempDir(), "host-plain.sofaplan")
		require.NoError(t, plan.SaveArchive(hostPlainArchive, hostExecPlan, plan.ArchiveSource{ConfigDir: hostExecDir}))
		loadedPlain, err := plan.LoadArchive(hostPlainArchive, t.TempDir(), plan.LoadArchiveOptions{})
		require.NoError(t, err)
		assert.NotEqual(t, loadedPlain.Manifest.BundleSHA256, loadedExec.Manifest.BundleSHA256)
	}

	altDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(altDir, "main.sofa.hcl"), []byte("#!/bin/sh\n"), 0o644))
	altArchive := filepath.Join(t.TempDir(), "alt.sofaplan")
	altPlan, err := plan.New(plan.Args{Directory: altDir, BundlePath: "config"})
	require.NoError(t, err)
	require.NoError(t, plan.SaveArchive(altArchive, altPlan, plan.ArchiveSource{ConfigDir: altDir}))
	loadedAlt, err := plan.LoadArchive(altArchive, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)

	plainAltDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(plainAltDir, "main.sofa.hcl"), []byte("# /bin/sh\n"), 0o644))
	plainAltArchive := filepath.Join(t.TempDir(), "plain-alt.sofaplan")
	plainAltPlan, err := plan.New(plan.Args{Directory: plainAltDir, BundlePath: "config"})
	require.NoError(t, err)
	require.NoError(t, plan.SaveArchive(plainAltArchive, plainAltPlan, plan.ArchiveSource{ConfigDir: plainAltDir}))
	loadedPlainAlt, err := plan.LoadArchive(plainAltArchive, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	assert.NotEqual(t, loadedPlainAlt.Manifest.BundleSHA256, loadedAlt.Manifest.BundleSHA256)
}

func TestSkillDirectoriesClassificationBundlingAndValidation(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configDir := filepath.Join(workspace, "cfg")
	extSkillsDir := filepath.Join(workspace, "external-skills")
	absExtSkillsDir := filepath.Join(workspace, "abs-skills")
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "skills", "in-bundle"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "skills", "in-bundle", "SKILL.md"), []byte("# InBundle\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(extSkillsDir, "ext-a", "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(extSkillsDir, "ext-a", "SKILL.md"), []byte("# ExtA\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(extSkillsDir, "ext-a", "scripts", "helper.sh"), []byte("#!/bin/sh\necho ok\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(absExtSkillsDir, "abs-a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(absExtSkillsDir, "abs-a", "SKILL.md"), []byte("# AbsA\n"), 0o644))

	expectedExtSum := sha256.Sum256([]byte(filepath.Clean(extSkillsDir)))
	expectedExtHash := hex.EncodeToString(expectedExtSum[:])[:16]
	expectedAbsSum := sha256.Sum256([]byte(filepath.Clean(absExtSkillsDir)))
	expectedAbsHash := hex.EncodeToString(expectedAbsSum[:])[:16]

	planned, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.main",
				CanonicalAddress: "worker.main",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{
						cty.StringVal("./skills"),
						cty.StringVal("../external-skills"),
						cty.StringVal(absExtSkillsDir),
						cty.StringVal("/workspace/.agents/skills"),
						cty.StringVal("/.sofa/imports/shared-skills"),
					}),
					"qc": cty.ObjectVal(map[string]cty.Value{
						"skill_directories": cty.ListVal([]cty.Value{
							cty.StringVal("../external-skills"),
							cty.StringVal("/workspace/qc-skills"),
						}),
					}),
				}),
			},
			{
				Address:          "dynamic_worker.sweep",
				CanonicalAddress: "dynamic_worker.sweep",
				Kind:             "dynamic_worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"content": cty.ObjectVal(map[string]cty.Value{
						"skill_directories": cty.ListVal([]cty.Value{
							cty.StringVal("skills"),
						}),
						"qc": cty.ObjectVal(map[string]cty.Value{
							"skill_directories": cty.ListVal([]cty.Value{
								cty.StringVal("../external-skills"),
							}),
						}),
					}),
				}),
			},
		},
	})
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "skills.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, planned, plan.ArchiveSource{ConfigDir: configDir}))

	// Verify zip header contains bundle/external_skills/<hash>/ with expected script mode 0755.
	zr, err := zip.OpenReader(archivePath)
	require.NoError(t, err)
	defer func() { _ = zr.Close() }()
	zipModes := make(map[string]fs.FileMode, len(zr.File))
	for _, f := range zr.File {
		zipModes[f.Name] = f.Mode()
	}
	assert.Equal(t, fs.FileMode(0o644), zipModes["bundle/external_skills/"+expectedExtHash+"/ext-a/SKILL.md"])
	assert.Equal(t, fs.FileMode(0o755), zipModes["bundle/external_skills/"+expectedExtHash+"/ext-a/scripts/helper.sh"])
	assert.Equal(t, fs.FileMode(0o644), zipModes["bundle/external_skills/"+expectedAbsHash+"/abs-a/SKILL.md"])

	// Extract and verify round-trip plan rewrites and extracted files.
	extractDir := t.TempDir()
	loaded, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)

	byAddr := make(map[string]plan.NodeSpec, len(loaded.Plan.Nodes()))
	for _, n := range loaded.Plan.Nodes() {
		byAddr[n.Address] = n
	}

	workerDirs := ctyStringList(t, byAddr["worker.main"].Config.GetAttr("skill_directories"))
	assert.Equal(t, []string{
		"config/skills",
		"external_skills/" + expectedExtHash,
		"external_skills/" + expectedAbsHash,
		"/workspace/.agents/skills",
		"/.sofa/imports/shared-skills",
	}, workerDirs)

	qcDirs := ctyStringList(t, byAddr["worker.main"].Config.GetAttr("qc").GetAttr("skill_directories"))
	assert.Equal(t, []string{
		"external_skills/" + expectedExtHash,
		"/workspace/qc-skills",
	}, qcDirs)

	dynDirs := ctyStringList(t, byAddr["dynamic_worker.sweep"].Config.GetAttr("content").GetAttr("skill_directories"))
	assert.Equal(t, []string{"config/skills"}, dynDirs)
	dynQCDirs := ctyStringList(t, byAddr["dynamic_worker.sweep"].Config.GetAttr("content").GetAttr("qc").GetAttr("skill_directories"))
	assert.Equal(t, []string{"external_skills/" + expectedExtHash}, dynQCDirs)

	extMD, err := os.ReadFile(filepath.Join(loaded.ExtractDir, "external_skills", expectedExtHash, "ext-a", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# ExtA\n", string(extMD))

	if runtime.GOOS != "windows" {
		scriptInfo, err := os.Stat(filepath.Join(loaded.ExtractDir, "external_skills", expectedExtHash, "ext-a", "scripts", "helper.sh"))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o755), scriptInfo.Mode().Perm())
	}

	// Missing absolute host path must Fail-Fast with node address and raw path.
	missingAbs := filepath.Join(workspace, "does-not-exist")
	missingAbsPlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.fail",
				CanonicalAddress: "worker.fail",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal(missingAbs)}),
				}),
			},
		},
	})
	require.NoError(t, err)
	err = plan.SaveArchive(filepath.Join(t.TempDir(), "missing-abs.sofaplan"), missingAbsPlan, plan.ArchiveSource{ConfigDir: configDir})
	require.ErrorContains(t, err, "worker.fail")
	require.ErrorContains(t, err, missingAbs)

	// Missing relative escaping path in QC must Fail-Fast with node address and raw path.
	missingQCPlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address:          "worker.qc_fail",
				CanonicalAddress: "worker.qc_fail",
				Kind:             "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"qc": cty.ObjectVal(map[string]cty.Value{
						"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("../missing-qc-ext")}),
					}),
				}),
			},
		},
	})
	require.NoError(t, err)
	err = plan.SaveArchive(filepath.Join(t.TempDir(), "missing-qc-ext.sofaplan"), missingQCPlan, plan.ArchiveSource{ConfigDir: configDir})
	require.ErrorContains(t, err, "worker.qc_fail")
	require.ErrorContains(t, err, "../missing-qc-ext")

	// Reject unsafe bundle/external_skills/../x entries on LoadArchive.
	badExtZip := filepath.Join(t.TempDir(), "bad-ext.sofaplan")
	rewriteZip(t, archivePath, badExtZip, func(entries []zipEntry) []zipEntry {
		return append(entries, zipEntry{name: "bundle/external_skills/../x", data: []byte("boom")})
	})
	_, loadErr := plan.LoadArchive(badExtZip, t.TempDir(), plan.LoadArchiveOptions{})
	require.Error(t, loadErr)

	// Missing relative in-bundle skill directory fails fast.
	missingRelPlan, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address: "worker.rel_fail",
				Kind:    "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("./missing-rel-skills")}),
				}),
			},
		},
	})
	require.NoError(t, err)
	err = plan.SaveArchive(filepath.Join(t.TempDir(), "missing-rel.sofaplan"), missingRelPlan, plan.ArchiveSource{ConfigDir: configDir})
	require.ErrorContains(t, err, "worker.rel_fail")
	require.ErrorContains(t, err, "./missing-rel-skills")

	// plan.NormalizeSkillDirectories does not mutate the caller's input Plan.
	origWorkerDirs := ctyStringList(t, planned.Nodes()[0].Config.GetAttr("skill_directories"))
	assert.Equal(t, "./skills", origWorkerDirs[0])
	assert.Equal(t, "../external-skills", origWorkerDirs[1])
	normalizedOnly, err := plan.NormalizeSkillDirectories(planned, plan.ArchiveSource{ConfigDir: configDir})
	require.NoError(t, err)
	normWorkerDirs := ctyStringList(t, normalizedOnly.Nodes()[0].Config.GetAttr("skill_directories"))
	assert.Equal(t, "config/skills", normWorkerDirs[0])
	assert.Equal(t, "external_skills/"+expectedExtHash, normWorkerDirs[1])
	assert.Equal(t, "./skills", ctyStringList(t, planned.Nodes()[0].Config.GetAttr("skill_directories"))[0])
}

func TestSkillDirectoriesHomeExpansionAndSymlinkRejection(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	configDir := t.TempDir()
	homeSkills := filepath.Join(homeDir, "my-skills")
	require.NoError(t, os.MkdirAll(filepath.Join(homeSkills, "home-skill"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(homeSkills, "home-skill", "SKILL.md"), []byte("# Home\n"), 0o644))

	expectedSum := sha256.Sum256([]byte(filepath.Clean(homeSkills)))
	expectedHash := hex.EncodeToString(expectedSum[:])[:16]

	planned, err := plan.New(plan.Args{
		Directory:  configDir,
		BundlePath: "config",
		Nodes: []plan.NodeSpec{
			{
				Address: "worker.home",
				Kind:    "worker",
				Config: cty.ObjectVal(map[string]cty.Value{
					"skill_directories": cty.ListVal([]cty.Value{cty.StringVal("~/my-skills")}),
				}),
			},
		},
	})
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "home.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, planned, plan.ArchiveSource{ConfigDir: configDir}))
	loaded, err := plan.LoadArchive(archivePath, t.TempDir(), plan.LoadArchiveOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"external_skills/" + expectedHash}, ctyStringList(t, loaded.Plan.Nodes()[0].Config.GetAttr("skill_directories")))
	content, err := os.ReadFile(filepath.Join(loaded.ExtractDir, "external_skills", expectedHash, "home-skill", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Home\n", string(content))

	// Symlink inside external skill dir must fail fast.
	symTarget := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(symTarget, "secret.txt"), []byte("secret"), 0o644))
	extWithSym := filepath.Join(t.TempDir(), "ext-sym")
	require.NoError(t, os.MkdirAll(extWithSym, 0o755))
	if symErr := os.Symlink(filepath.Join(symTarget, "secret.txt"), filepath.Join(extWithSym, "link.txt")); symErr == nil {
		symPlan, err := plan.New(plan.Args{
			Directory:  configDir,
			BundlePath: "config",
			Nodes: []plan.NodeSpec{
				{
					Address: "worker.sym",
					Kind:    "worker",
					Config: cty.ObjectVal(map[string]cty.Value{
						"skill_directories": cty.ListVal([]cty.Value{cty.StringVal(extWithSym)}),
					}),
				},
			},
		})
		require.NoError(t, err)
		err = plan.SaveArchive(filepath.Join(t.TempDir(), "sym.sofaplan"), symPlan, plan.ArchiveSource{ConfigDir: configDir})
		require.ErrorContains(t, err, "symlink")
	}
}

func ctyStringList(t *testing.T, v cty.Value) []string {
	t.Helper()
	var out []string
	for it := v.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		out = append(out, elem.AsString())
	}
	return out
}

var _ = hex.EncodeToString
