package plan_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

var _ = hex.EncodeToString
