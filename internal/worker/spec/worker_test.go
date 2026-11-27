package spec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func decodeWorkerHCL(t *testing.T, source string, variables map[string]cty.Value) (*WorkerBlock, error) {
	t.Helper()
	syntaxFile, diags := hclsyntax.ParseConfig([]byte(source), "worker.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	writeFile, diags := hclwrite.ParseConfig([]byte(source), "worker.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())

	syntaxBody := syntaxFile.Body.(*hclsyntax.Body)
	writeBlocks := writeFile.Body().Blocks()
	require.Len(t, syntaxBody.Blocks, 1)
	require.Len(t, writeBlocks, 1)

	sb := syntaxBody.Blocks[0]
	labels := append([]string{""}, sb.Labels...)
	clonedSyntax := &hclsyntax.Block{
		Type:            sb.Type,
		Labels:          labels,
		Body:            sb.Body,
		TypeRange:       sb.TypeRange,
		LabelRanges:     sb.LabelRanges,
		OpenBraceRange:  sb.OpenBraceRange,
		CloseBraceRange: sb.CloseBraceRange,
	}
	hb := golden.NewHclBlock(clonedSyntax, writeBlocks[0], nil)
	base := golden.NewBasicConfig(t.TempDir(), "sofa", "sofa", nil, nil, nil)
	w := &WorkerBlock{
		BaseBlock: golden.NewBaseBlock(base, hb),
	}
	ctx := base.EmptyEvalContext()
	if ctx.Variables == nil {
		ctx.Variables = map[string]cty.Value{}
	}
	ctx.Variables["sandbox"] = cty.ObjectVal(map[string]cty.Value{
		"go_env": cty.ObjectVal(map[string]cty.Value{
			"address": cty.StringVal("sandbox.go_env"),
			"kind":    cty.StringVal("sandbox"),
			"name":    cty.StringVal("go_env"),
		}),
		"multi": cty.ObjectVal(map[string]cty.Value{
			"a": cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("sandbox.multi[a]"),
				"kind":    cty.StringVal("sandbox"),
			}),
		}),
	})
	ctx.Variables["harness"] = cty.ObjectVal(map[string]cty.Value{
		"local": cty.ObjectVal(map[string]cty.Value{
			"address": cty.StringVal("harness.local"),
			"kind":    cty.StringVal("harness"),
			"name":    cty.StringVal("local"),
		}),
		"alt": cty.ObjectVal(map[string]cty.Value{
			"address": cty.StringVal("harness.alt"),
			"kind":    cty.StringVal("harness"),
			"name":    cty.StringVal("alt"),
		}),
	})
	ctx.Variables["model_provider"] = cty.ObjectVal(map[string]cty.Value{
		"openai": cty.ObjectVal(map[string]cty.Value{
			"address": cty.StringVal("model_provider.openai"),
			"kind":    cty.StringVal("model_provider"),
			"name":    cty.StringVal("openai"),
		}),
	})
	for k, v := range variables {
		ctx.Variables[k] = v
	}
	err := w.Decode(hb, ctx)
	return w, err
}

func validWorkerAttrs() string {
	return `
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"`
}

func TestWorkspaceRelativePathValidation(t *testing.T) {
	for _, value := range []string{"docs/spec.md", ".sofa/imports/design"} {
		require.NoError(t, validateWorkspaceRelativePath(value, "path", false))
	}
	require.NoError(t, validateWorkspaceRelativePath(".", "working_dir", true))
	require.Error(t, validateWorkspaceRelativePath(".", "artifact path", false))
	for _, value := range []string{"", "   ", "/absolute", "../outside", "a/../../outside", `..\outside`, `a\b`, `C:foo`, `C:/foo`} {
		require.Error(t, validateWorkspaceRelativePath(value, "path", false), value)
		require.Error(t, validateWorkspaceRelativePath(value, "working_dir", true), value)
	}
}

func TestGatekeeperDefaultsAndValidation(t *testing.T) {
	config := GatekeeperBlock{
		Name:    "test",
		Command: []string{"go", "test", "./..."},
	}.Config()
	require.Equal(t, ".", config.WorkingDir)
	require.Equal(t, "10m", config.Timeout)
	require.True(t, config.RequireCleanTree)
	require.NoError(t, validateGatekeeper(config))

	config.Command = nil
	require.ErrorContains(t, validateGatekeeper(config), "command")
	config.Command = []string{"   "}
	require.ErrorContains(t, validateGatekeeper(config), "command")
	config.Command = []string{"go"}
	config.WorkingDir = "../outside"
	require.ErrorContains(t, validateGatekeeper(config), "workspace-relative")
}

func TestP01EvaluationErrorsNotSwallowed(t *testing.T) {
	tests := []struct {
		name string
		hcl  string
	}{
		{
			name: "undeclared artifact function call in prompt",
			hcl: fmt.Sprintf(`worker "w" { %s
  prompt = "${artifact("undeclared").path}"
}`, validWorkerAttrs()),
		},
		{
			name: "unknown function in system_prompt",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = unknown_fn("x")
}`,
		},
		{
			name: "missing attribute on known object in based_source",
			hcl: `worker "w" {
  based_source  = sandbox.go_env.missing_attr
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "missing attribute on known object in tool_use input",
			hcl: fmt.Sprintf(`worker "w" { %s
  tool_use "submit" {
    tool_id = "tool_go_tool_submit_1234"
    input = {
      bad = harness.local.does_not_exist
    }
  }
}`, validWorkerAttrs()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeWorkerHCL(t, tt.hcl, nil)
			require.Error(t, err)
		})
	}
}

func TestP02StrictScalarTypesNullabilityKnownnessAndWholeIntegers(t *testing.T) {
	vars := map[string]cty.Value{
		"var": cty.ObjectVal(map[string]cty.Value{
			"unknown_str": cty.UnknownVal(cty.String),
			"unknown_num": cty.UnknownVal(cty.Number),
		}),
	}
	invalid := []struct {
		name string
		hcl  string
	}{
		{
			name: "model cannot be number",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = 123
  system_prompt = "sys"
}`,
		},
		{
			name: "model cannot be unknown",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = var.unknown_str
  system_prompt = "sys"
}`,
		},
		{
			name: "model cannot be blank",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "   "
  system_prompt = "sys"
}`,
		},
		{
			name: "profile cannot be number",
			hcl: fmt.Sprintf(`worker "w" { %s
  profile = 42
}`, validWorkerAttrs()),
		},
		{
			name: "profile cannot be blank when set",
			hcl: fmt.Sprintf(`worker "w" { %s
  profile = "   "
}`, validWorkerAttrs()),
		},
		{
			name: "reasoning_effort cannot be blank when set",
			hcl: fmt.Sprintf(`worker "w" { %s
  reasoning_effort = " "
}`, validWorkerAttrs()),
		},
		{
			name: "human_approval cannot be number",
			hcl: fmt.Sprintf(`worker "w" { %s
  human_approval = 99
}`, validWorkerAttrs()),
		},
		{
			name: "checkpoint_every_rounds cannot be string",
			hcl: fmt.Sprintf(`worker "w" { %s
  checkpoint_every_rounds = "five"
}`, validWorkerAttrs()),
		},
		{
			name: "checkpoint_every_rounds cannot be fractional",
			hcl: fmt.Sprintf(`worker "w" { %s
  checkpoint_every_rounds = 1.5
}`, validWorkerAttrs()),
		},
		{
			name: "checkpoint_every_rounds cannot be unknown",
			hcl: fmt.Sprintf(`worker "w" { %s
  checkpoint_every_rounds = var.unknown_num
}`, validWorkerAttrs()),
		},
		{
			name: "based_source cannot be number",
			hcl: `worker "w" {
  based_source  = 123
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "based_source cannot be null",
			hcl: `worker "w" {
  based_source  = null
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "system_prompt cannot be null",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = null
}`,
		},
		{
			name: "system_prompt cannot be blank when known",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "   "
}`,
		},
		{
			name: "prompt cannot be number",
			hcl: fmt.Sprintf(`worker "w" { %s
  prompt = 123
}`, validWorkerAttrs()),
		},
		{
			name: "gatekeeper expect_exit_code cannot be fractional",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    gatekeeper "unit" {
      command          = ["go", "test"]
      expect_exit_code = 1.5
    }
  }
}`, validWorkerAttrs()),
		},
		{
			name: "qc max_qc_rounds cannot be string",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    criteria      = { a = "b" }
    max_qc_rounds = "three"
  }
}`, validWorkerAttrs()),
		},
		{
			name: "import_artifact desc cannot be blank when set",
			hcl: fmt.Sprintf(`worker "w" { %s
  import_artifact "spec" {
    desc    = "   "
    sources = []
  }
}`, validWorkerAttrs()),
		},
		{
			name: "artifact description cannot be blank",
			hcl: fmt.Sprintf(`worker "w" { %s
  artifact "out" {
    type        = "file"
    path        = "out.md"
    description = "   "
  }
}`, validWorkerAttrs()),
		},
	}

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeWorkerHCL(t, tt.hcl, vars)
			require.Error(t, err)
		})
	}

	// Valid null optionals and unknown string fields.
	validHCL := `worker "w" {
  based_source            = var.unknown_str
  sandbox                 = sandbox.go_env
  harness                 = harness.local
  model_provider          = null
  model                   = "gpt-5"
  profile                 = null
  reasoning_effort        = null
  system_prompt           = var.unknown_str
  prompt                  = null
  permission              = null
  timeout                 = null
  human_approval          = null
  checkpoint_every_rounds = null
  max_protocol_attempts   = null
}`
	w, err := decodeWorkerHCL(t, validHCL, vars)
	require.NoError(t, err)
	require.NotEmpty(t, w.Values()["work_branch"])
}

func TestP03ContractAttributesRetryQuotaAndNestedWhitelists(t *testing.T) {
	// Valid full attributes + retry + qc.
	validHCL := fmt.Sprintf(`worker "w" { %s
  permission            = "approve_all"
  timeout               = "45m"
  max_protocol_attempts = 12
  tool_ids              = ["tool_go_tool_check_1"]
  terminate_tool_id     = "tool_go_tool_done_1"
  tool_call_quota       = { bash = 10, tool_go_tool_check_1 = 2 }
  allowed_tools         = ["bash", "custom:tool_go_tool_done_1"]
  disallowed_tools      = ["ask_user"]
  skill_directories     = ["skills"]
  skills                = ["golang-tdd"]
  disabled_skills       = ["legacy"]

  retry {
    lifecycle_retries    = 4
    model_call_retries   = 3
    interval_seconds     = 5
    max_interval_seconds = 60
    error_message_regex  = ["rate.*limit"]
  }

  qc {
    criteria          = { quality = "must pass" }
    permission        = "approve_all"
    tool_ids          = ["tool_go_tool_qc_1"]
    tool_call_quota   = { tool_go_tool_qc_1 = 3 }
    allowed_tools     = ["tool_go_tool_qc_1"]
    disallowed_tools  = ["ask_user"]
    skill_directories = ["qc-skills"]
    skills            = ["review"]
    disabled_skills   = ["skip"]
    max_qc_rounds     = 4
    retry {
      model_call_retries = 2
      interval_seconds   = 10
    }
  }
}`, validWorkerAttrs())
	_, err := decodeWorkerHCL(t, validHCL, nil)
	require.NoError(t, err)

	invalid := []struct {
		name string
		hcl  string
	}{
		{
			name: "invalid permission",
			hcl: fmt.Sprintf(`worker "w" { %s
  permission = "manual"
}`, validWorkerAttrs()),
		},
		{
			name: "non-positive timeout",
			hcl: fmt.Sprintf(`worker "w" { %s
  timeout = "-5m"
}`, validWorkerAttrs()),
		},
		{
			name: "zero max_protocol_attempts",
			hcl: fmt.Sprintf(`worker "w" { %s
  max_protocol_attempts = 0
}`, validWorkerAttrs()),
		},
		{
			name: "quota rejects mcp_tool prefix",
			hcl: fmt.Sprintf(`worker "w" { %s
  tool_call_quota = { mcp_tool_search_1 = 2 }
}`, validWorkerAttrs()),
		},
		{
			name: "quota rejects unconfigured typed tool",
			hcl: fmt.Sprintf(`worker "w" { %s
  tool_call_quota = { tool_go_tool_missing_1 = 2 }
}`, validWorkerAttrs()),
		},
		{
			name: "allowed_tools empty blocks terminate_tool_id",
			hcl: fmt.Sprintf(`worker "w" { %s
  terminate_tool_id = "tool_go_tool_done_1"
  allowed_tools     = []
}`, validWorkerAttrs()),
		},
		{
			name: "disallowed_tools blocks terminate_tool_id",
			hcl: fmt.Sprintf(`worker "w" { %s
  terminate_tool_id = "tool_go_tool_done_1"
  disallowed_tools  = ["tool_go_tool_done_1"]
}`, validWorkerAttrs()),
		},
		{
			name: "retry invalid interval",
			hcl: fmt.Sprintf(`worker "w" { %s
  retry {
    interval_seconds = 0
  }
}`, validWorkerAttrs()),
		},
		{
			name: "retry max_interval less than interval",
			hcl: fmt.Sprintf(`worker "w" { %s
  retry {
    interval_seconds     = 20
    max_interval_seconds = 10
  }
}`, validWorkerAttrs()),
		},
		{
			name: "qc invalid tool_ids not discarded",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    criteria = { a = "b" }
    tool_ids = "not-a-list"
  }
}`, validWorkerAttrs()),
		},
		{
			name: "unknown attr in artifact",
			hcl: fmt.Sprintf(`worker "w" { %s
  artifact "out" {
    type        = "file"
    path        = "out.md"
    description = "out"
    bogus       = true
  }
}`, validWorkerAttrs()),
		},
		{
			name: "unexpected nested block in gatekeeper",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    gatekeeper "unit" {
      command = ["go", "test"]
      extra {}
    }
  }
}`, validWorkerAttrs()),
		},
		{
			name: "unknown attr in retry",
			hcl: fmt.Sprintf(`worker "w" { %s
  retry {
    unknown_field = 1
  }
}`, validWorkerAttrs()),
		},
		{
			name: "unknown attr in qc",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    criteria = { a = "b" }
    bogus    = "x"
  }
}`, validWorkerAttrs()),
		},
		{
			name: "unexpected nested block in fallback",
			hcl: fmt.Sprintf(`worker "w" { %s
  fallback "r" {
    on     = ["qc_exhausted"]
    target = worker.up
    extra {}
  }
}`, validWorkerAttrs()),
		},
		{
			name: "unknown attr in tool_use",
			hcl: fmt.Sprintf(`worker "w" { %s
  tool_use "t" {
    tool_id = "tool_go_tool_a_1"
    bogus   = 1
  }
}`, validWorkerAttrs()),
		},
	}

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeWorkerHCL(t, tt.hcl, nil)
			require.Error(t, err)
		})
	}
}

func TestP04FallbackArtifactsContract(t *testing.T) {
	validHCL := fmt.Sprintf(`worker "w" { %s
  artifact "test_report" {
    type        = "file"
    path        = "reports/test.json"
    description = "test summary"
  }
  fallback "rework" {
    on        = ["qc_exhausted"]
    target    = worker.tasks
    artifacts = [artifact("test_report")]
  }
}`, validWorkerAttrs())
	_, err := decodeWorkerHCL(t, validHCL, nil)
	require.NoError(t, err)

	otherWorkerVars := map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"other": cty.ObjectVal(map[string]cty.Value{
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"test_report": cty.ObjectVal(map[string]cty.Value{
						"id":          cty.UnknownVal(cty.String),
						"name":        cty.StringVal("test_report"),
						"kind":        cty.StringVal("artifact"),
						"type":        cty.StringVal("file"),
						"path":        cty.StringVal("reports/test.json"),
						"description": cty.StringVal("other"),
						"required":    cty.BoolVal(false),
						"non_empty":   cty.BoolVal(false),
					}),
				}),
			}),
		}),
	}
	for _, bad := range []string{
		fmt.Sprintf(`worker "w" { %s
  artifact "test_report" {
    type        = "file"
    path        = "reports/test.json"
    description = "test summary"
  }
  fallback "rework" {
    on        = ["qc_exhausted"]
    target    = worker.tasks
    artifacts = ["test_report"]
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  artifact "test_report" {
    type        = "file"
    path        = "reports/test.json"
    description = "test summary"
  }
  fallback "rework" {
    on        = ["qc_exhausted"]
    target    = worker.tasks
    artifacts = [worker.other.artifact.test_report]
  }
}`, validWorkerAttrs()),
	} {
		_, err := decodeWorkerHCL(t, bad, otherWorkerVars)
		require.Error(t, err)
	}
}

func TestP05StrictBlockReferenceKindAndKnownness(t *testing.T) {
	badRefs := []struct {
		name string
		hcl  string
	}{
		{
			name: "sandbox gets harness",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = harness.local
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "sandbox gets for_each map",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.multi
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "sandbox gets arbitrary object",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = { foo = "bar" }
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "harness gets sandbox",
			hcl: `worker "w" {
  based_source  = "main"
  sandbox       = sandbox.go_env
  harness       = sandbox.go_env
  model         = "gpt-5"
  system_prompt = "sys"
}`,
		},
		{
			name: "model_provider gets number",
			hcl: fmt.Sprintf(`worker "w" { %s
  model_provider = 123
}`, validWorkerAttrs()),
		},
		{
			name: "model_provider gets sandbox",
			hcl: fmt.Sprintf(`worker "w" { %s
  model_provider = sandbox.go_env
}`, validWorkerAttrs()),
		},
		{
			name: "qc harness gets sandbox",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    criteria = { a = "b" }
    harness  = sandbox.go_env
  }
}`, validWorkerAttrs()),
		},
		{
			name: "qc model_provider gets harness",
			hcl: fmt.Sprintf(`worker "w" { %s
  qc {
    criteria       = { a = "b" }
    model_provider = harness.local
  }
}`, validWorkerAttrs()),
		},
	}

	for _, tt := range badRefs {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeWorkerHCL(t, tt.hcl, nil)
			require.Error(t, err)
		})
	}
}

func TestP06ImportArtifactValidationDuplicatesAndInterpolation(t *testing.T) {
	vars := map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"up": cty.ObjectVal(map[string]cty.Value{
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"spec": cty.ObjectVal(map[string]cty.Value{
						"id":   cty.UnknownVal(cty.String),
						"name": cty.StringVal("spec"),
						"kind": cty.StringVal("artifact"),
					}),
				}),
			}),
			"w": cty.ObjectVal(map[string]cty.Value{
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"own": cty.ObjectVal(map[string]cty.Value{
						"id":   cty.UnknownVal(cty.String),
						"name": cty.StringVal("own"),
						"kind": cty.StringVal("artifact"),
					}),
				}),
			}),
		}),
	}

	okHCL := fmt.Sprintf(`worker "w" { %s
  prompt = "Read $${import_artifact.spec.path}"
  import_artifact "spec" {
    desc    = "upstream spec"
    sources = [worker.up.artifact.spec]
  }
}`, validWorkerAttrs())
	okHCL = strings.ReplaceAll(okHCL, "$$", "$")
	_, err := decodeWorkerHCL(t, okHCL, vars)
	require.NoError(t, err)

	for _, bad := range []string{
		fmt.Sprintf(`worker "w" { %s
  import_artifact "spec" {
    desc    = "a"
    sources = []
  }
  import_artifact "spec" {
    desc    = "b"
    sources = []
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  import_artifact "spec" {
    desc    = "a"
    sources = null
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  import_artifact "spec" {
    desc    = "a"
    sources = "not-a-list"
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  artifact "own" {
    type        = "file"
    path        = "own.md"
    description = "own"
  }
  import_artifact "spec" {
    desc    = "self import"
    sources = [worker.w.artifact.own]
  }
}`, validWorkerAttrs()),
	} {
		_, err := decodeWorkerHCL(t, bad, vars)
		require.Error(t, err)
	}
}

func TestP11DeferredExpressionCompleteness(t *testing.T) {
	vars := map[string]cty.Value{
		"var": cty.ObjectVal(map[string]cty.Value{
			"unknown_str": cty.UnknownVal(cty.String),
		}),
		"worker": cty.ObjectVal(map[string]cty.Value{
			"up": cty.ObjectVal(map[string]cty.Value{
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"spec": cty.ObjectVal(map[string]cty.Value{
						"id":   cty.UnknownVal(cty.String),
						"name": cty.StringVal("spec"),
						"kind": cty.StringVal("artifact"),
					}),
				}),
			}),
		}),
	}

	// Even when based_source, system_prompt, and prompt are known, unknown import_artifact.sources triggers deferred.
	hclCode := fmt.Sprintf(`worker "w" { %s
  prompt = "Read ${import_artifact.spec.path}"
  retry {
    lifecycle_retries = 3
  }
  import_artifact "spec" {
    desc    = "spec"
    sources = [worker.up.artifact.spec]
  }
  artifact "report" {
    type        = "file"
    path        = "reports/r.md"
    description = "report"
  }
  qc {
    criteria = { a = "b" }
    gatekeeper "unit" {
      command = ["go", "test"]
    }
  }
  fallback "rework" {
    on        = ["qc_exhausted"]
    target    = worker.up
    artifacts = [artifact("report")]
  }
}`, validWorkerAttrs())
	w, err := decodeWorkerHCL(t, hclCode, vars)
	require.NoError(t, err)
	expr := w.WorkerConfig().DeferredExpression
	require.NotEmpty(t, expr)
	_, diags := hclsyntax.ParseExpression([]byte(expr), "deferred.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	require.Contains(t, expr, "retry =")
	require.Contains(t, expr, "artifact =")
	require.Contains(t, expr, "import_artifact =")
	require.Contains(t, expr, `"/.sofa/imports/spec"`)
	require.Contains(t, expr, "qc =")
	require.Contains(t, expr, "gatekeeper =")
	require.Contains(t, expr, "fallback =")
	require.Contains(t, expr, `"worker.up"`)
}

func TestP14LabelsAndUniqueNestedNames(t *testing.T) {
	for _, bad := range []string{
		fmt.Sprintf("worker \"  \" { %s\n}", validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  artifact ".." {
    type        = "file"
    path        = "out.md"
    description = "out"
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  import_artifact "a/b" {
    desc    = "x"
    sources = []
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  qc {
    gatekeeper "unit" {
      command = ["go", "test"]
    }
    gatekeeper "unit" {
      command = ["go", "vet"]
    }
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  tool_use "dup" {
    tool_id = "tool_go_tool_a_1"
  }
  tool_use "dup" {
    tool_id = "tool_go_tool_b_2"
  }
}`, validWorkerAttrs()),
	} {
		_, err := decodeWorkerHCL(t, bad, nil)
		require.Error(t, err)
	}
}

func TestWorkerConfigDeepCopyAndQCNullModelProvider(t *testing.T) {
	hclCode := fmt.Sprintf(`worker "w" { %s
  model_provider    = model_provider.openai
  writable_paths    = ["internal/**"]
  tool_ids          = ["tool_go_tool_a_1"]
  mcp_tool_ids      = ["mcp_tool_m_1"]
  mcp_resource_ids  = ["mcp_resource_r_1"]
  tool_call_quota   = { bash = 5 }
  allowed_tools     = ["bash"]
  disallowed_tools  = ["ask_user"]
  skill_directories = ["skills"]
  skills            = ["tdd"]
  disabled_skills   = ["old"]
  timeout           = "15m"

  artifact "out" {
    type        = "file"
    path        = "out.md"
    description = "output"
  }

  import_artifact "in" {
    desc    = "input"
    sources = []
  }

  retry {
    lifecycle_retries   = 2
    error_message_regex = ["timeout"]
  }

  qc {
    model_provider = null
    criteria       = { ok = "yes" }
    tool_ids       = ["tool_go_tool_qc_1"]
    tool_call_quota = { tool_go_tool_qc_1 = 1 }
    gatekeeper "unit" {
      command = ["go", "test", "./..."]
    }
    retry {
      model_call_retries = 3
    }
  }

  fallback "rework" {
    on        = ["qc_exhausted"]
    target    = worker.up
    artifacts = [artifact("out")]
  }
}`, validWorkerAttrs())

	w, err := decodeWorkerHCL(t, hclCode, nil)
	require.NoError(t, err)

	snapshot := w.WorkerConfig()
	require.NotNil(t, snapshot.QC)
	require.True(t, snapshot.QC.ModelProviderSet)
	require.True(t, snapshot.QC.ModelProvider.IsNull())

	// Mutate every slice, map, and pointer on snapshot; subsequent WorkerConfig() must stay unchanged.
	snapshot.WritablePaths[0] = "mutated"
	snapshot.ToolIDs[0] = "mutated"
	snapshot.MCPToolIDs[0] = "mutated"
	snapshot.MCPResourceIDs[0] = "mutated"
	snapshot.ToolCallQuota["bash"] = 999
	snapshot.AllowedTools[0] = "mutated"
	snapshot.DisallowedTools[0] = "mutated"
	snapshot.SkillDirectories[0] = "mutated"
	snapshot.Skills[0] = "mutated"
	snapshot.DisabledSkills[0] = "mutated"
	*snapshot.Timeout = 1
	*snapshot.Retry.LifecycleRetries = 999
	snapshot.Retry.ErrorMessageRegex[0] = "mutated"
	snapshot.Artifacts["extra"] = Artifact{Name: "extra"}
	snapshot.Imports["extra"] = ImportArtifact{Name: "extra"}
	snapshot.QC.Gatekeepers[0].Command[0] = "mutated"
	snapshot.QC.ToolIDs[0] = "mutated"
	snapshot.QC.ToolCallQuota["tool_go_tool_qc_1"] = 999
	*snapshot.QC.Retry.ModelCallRetries = 999
	snapshot.Fallbacks[0].On[0] = "mutated"
	snapshot.Fallbacks[0].Artifacts[0] = "mutated"

	fresh := w.WorkerConfig()
	require.Equal(t, []string{"internal/**"}, fresh.WritablePaths)
	require.Equal(t, []string{"tool_go_tool_a_1"}, fresh.ToolIDs)
	require.Equal(t, []string{"mcp_tool_m_1"}, fresh.MCPToolIDs)
	require.Equal(t, []string{"mcp_resource_r_1"}, fresh.MCPResourceIDs)
	require.Equal(t, 5, fresh.ToolCallQuota["bash"])
	require.Equal(t, []string{"bash"}, fresh.AllowedTools)
	require.Equal(t, []string{"ask_user"}, fresh.DisallowedTools)
	require.Equal(t, []string{"skills"}, fresh.SkillDirectories)
	require.Equal(t, []string{"tdd"}, fresh.Skills)
	require.Equal(t, []string{"old"}, fresh.DisabledSkills)
	require.Equal(t, 15, int(fresh.Timeout.Minutes()))
	require.Equal(t, 2, *fresh.Retry.LifecycleRetries)
	require.Equal(t, []string{"timeout"}, fresh.Retry.ErrorMessageRegex)
	require.NotContains(t, fresh.Artifacts, "extra")
	require.NotContains(t, fresh.Imports, "extra")
	require.Equal(t, "go", fresh.QC.Gatekeepers[0].Command[0])
	require.Equal(t, []string{"tool_go_tool_qc_1"}, fresh.QC.ToolIDs)
	require.Equal(t, 1, fresh.QC.ToolCallQuota["tool_go_tool_qc_1"])
	require.Equal(t, 3, *fresh.QC.Retry.ModelCallRetries)
	require.Equal(t, []string{"qc_exhausted"}, fresh.Fallbacks[0].On)
	require.Equal(t, []string{"out"}, fresh.Fallbacks[0].Artifacts)
}

func TestDeferredDynamicGatekeeperEvaluatesWithoutIteratorVar(t *testing.T) {
	vars := map[string]cty.Value{
		"var": cty.ObjectVal(map[string]cty.Value{
			"unknown_branch": cty.UnknownVal(cty.String),
		}),
	}
	hclCode := `worker "w" {
  based_source  = var.unknown_branch
  sandbox       = sandbox.go_env
  harness       = harness.local
  model         = "gpt-5"
  system_prompt = "sys"

  qc {
    dynamic "gatekeeper" {
      for_each = toset(["test"])
      content "unit" {
        command = ["go", gatekeeper.value, "./..."]
      }
    }
  }
}`
	w, err := decodeWorkerHCL(t, hclCode, vars)
	require.NoError(t, err)
	exprStr := w.WorkerConfig().DeferredExpression
	require.NotEmpty(t, exprStr)
	require.NotContains(t, exprStr, "gatekeeper.value")

	parsed, diags := hclsyntax.ParseExpression([]byte(exprStr), "deferred.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())

	evalCtx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				"unknown_branch": cty.StringVal("feature/x"),
			}),
			"sandbox": cty.ObjectVal(map[string]cty.Value{
				"go_env": cty.ObjectVal(map[string]cty.Value{
					"address": cty.StringVal("sandbox.go_env"),
					"kind":    cty.StringVal("sandbox"),
				}),
			}),
			"harness": cty.ObjectVal(map[string]cty.Value{
				"local": cty.ObjectVal(map[string]cty.Value{
					"address": cty.StringVal("harness.local"),
					"kind":    cty.StringVal("harness"),
				}),
			}),
		},
	}
	val, valDiags := parsed.Value(evalCtx)
	require.False(t, valDiags.HasErrors(), valDiags.Error())
	cmd := val.GetAttr("qc").GetAttr("gatekeeper").GetAttr("unit").GetAttr("command").AsValueSlice()
	require.Len(t, cmd, 3)
	require.Equal(t, "test", cmd[1].AsString())
}

func TestFallbackWithDynamicGatekeeperAndQuotedTarget(t *testing.T) {
	otherWorkerVars := map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"other": cty.ObjectVal(map[string]cty.Value{
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"test_report": cty.ObjectVal(map[string]cty.Value{
						"id":          cty.UnknownVal(cty.String),
						"name":        cty.StringVal("test_report"),
						"kind":        cty.StringVal("artifact"),
						"type":        cty.StringVal("file"),
						"path":        cty.StringVal("reports/test.json"),
						"description": cty.StringVal("test summary"),
						"required":    cty.BoolVal(false),
						"non_empty":   cty.BoolVal(false),
					}),
				}),
			}),
			"tasks": cty.ObjectVal(map[string]cty.Value{
				"work_branch": cty.UnknownVal(cty.String),
			}),
		}),
	}

	// (a) Worker containing qc dynamic "gatekeeper" AND fallback referencing worker.other.artifact.test_report MUST fail.
	dynamicAndForeignArtifact := fmt.Sprintf(`worker "w" { %s
  artifact "test_report" {
    type        = "file"
    path        = "reports/test.json"
    description = "test summary"
  }
  qc {
    dynamic "gatekeeper" {
      for_each = toset(["unit"])
      content "unit" {
        command = ["go", "test"]
      }
    }
  }
  fallback "rework" {
    on        = ["qc_exhausted"]
    target    = worker.tasks
    artifacts = [worker.other.artifact.test_report]
  }
}`, validWorkerAttrs())
	_, err := decodeWorkerHCL(t, dynamicAndForeignArtifact, otherWorkerVars)
	require.Error(t, err)

	// (b) Quoted string literal fallback target MUST fail.
	quotedTarget := fmt.Sprintf(`worker "w" { %s
  fallback "rework" {
    on     = ["qc_exhausted"]
    target = "worker.tasks"
  }
}`, validWorkerAttrs())
	_, err = decodeWorkerHCL(t, quotedTarget, otherWorkerVars)
	require.Error(t, err)
}

func TestQCSandboxAndProfileOverrides(t *testing.T) {
	// Default (omitted) and explicit null qc.sandbox normalize to null empty object.
	for _, qcBody := range []string{
		"criteria = { a = \"b\" }",
		"sandbox  = null\n    profile  = null\n    criteria = { a = \"b\" }",
	} {
		w, err := decodeWorkerHCL(t, fmt.Sprintf("worker \"w\" { %s\n  qc {\n    %s\n  }\n}", validWorkerAttrs(), qcBody), nil)
		require.NoError(t, err)
		qc := w.WorkerConfig().QC
		require.NotNil(t, qc)
		require.True(t, qc.Sandbox.IsNull())
		require.Equal(t, "", qc.Profile)
	}

	// Explicit qc.sandbox and qc.profile succeed.
	w, err := decodeWorkerHCL(t, fmt.Sprintf(`worker "w" { %s
  qc {
    sandbox  = sandbox.go_env
    profile  = "opus-audit"
    criteria = { a = "b" }
  }
}`, validWorkerAttrs()), nil)
	require.NoError(t, err)
	qc := w.WorkerConfig().QC
	require.NotNil(t, qc)
	require.False(t, qc.Sandbox.IsNull())
	require.Equal(t, "sandbox.go_env", qc.Sandbox.GetAttr("address").AsString())
	require.Equal(t, "opus-audit", qc.Profile)

	// Invalid qc.sandbox and blank qc.profile MUST fail.
	for _, badQC := range []string{
		"sandbox  = harness.local\n    criteria = { a = \"b\" }",
		"profile  = \"   \"\n    criteria = { a = \"b\" }",
		"profile  = 42\n    criteria = { a = \"b\" }",
	} {
		_, err := decodeWorkerHCL(t, fmt.Sprintf("worker \"w\" { %s\n  qc {\n    %s\n  }\n}", validWorkerAttrs(), badQC), nil)
		require.Error(t, err)
	}
}

func TestContainerExternalArtifactAndImportPaths(t *testing.T) {
	vars := map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"up": cty.ObjectVal(map[string]cty.Value{
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"spec": cty.ObjectVal(map[string]cty.Value{
						"id":   cty.UnknownVal(cty.String),
						"name": cty.StringVal("spec"),
						"kind": cty.StringVal("artifact"),
					}),
				}),
			}),
		}),
	}
	hclCode := fmt.Sprintf(`worker "w" { %s
  prompt = "$${artifact("design").path}|$${artifact("design").rel_path}|$${import_artifact.defaulted.path}|$${import_artifact.explicit.path}"

  artifact "design" {
    type        = "file"
    path        = "docs/plan.md"
    description = "Plan"
  }

  import_artifact "defaulted" {
    desc    = "defaulted"
    sources = [worker.up.artifact.spec]
  }

  import_artifact "explicit" {
    desc    = "explicit"
    path    = "inputs/plan.md"
    sources = [worker.up.artifact.spec]
  }
}`, validWorkerAttrs())
	hclCode = strings.ReplaceAll(hclCode, "$$", "$")
	w, err := decodeWorkerHCL(t, hclCode, vars)
	require.NoError(t, err)
	cfg := w.WorkerConfig()
	require.Equal(t, "/.sofa/artifacts/docs/plan.md|docs/plan.md|/.sofa/imports/defaulted|/.sofa/imports/inputs/plan.md", cfg.Prompt.AsString())
	artVal := w.Values()["artifact"].GetAttr("design")
	require.Equal(t, "/.sofa/artifacts/docs/plan.md", artVal.GetAttr("path").AsString())
	require.Equal(t, "docs/plan.md", artVal.GetAttr("rel_path").AsString())

	// Absolute or escaping artifact/import paths MUST fail.
	for _, badPath := range []string{"/etc/passwd", "../escape", "."} {
		_, err := decodeWorkerHCL(t, fmt.Sprintf(`worker "w" { %s
  artifact "out" {
    type        = "file"
    path        = %q
    description = "out"
  }
}`, validWorkerAttrs(), badPath), nil)
		require.Error(t, err)
	}
}

func TestQCNestedGatekeeperAndMaxReworksValidation(t *testing.T) {
	// 1. Worker-level gatekeeper and dynamic "gatekeeper" MUST fail with migration error pointing to qc.
	for _, bad := range []string{
		fmt.Sprintf(`worker "w" { %s
  gatekeeper "unit" {
    command = ["go", "test"]
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  dynamic "gatekeeper" {
    for_each = toset(["unit"])
    content "unit" {
      command = ["go", "test"]
    }
  }
}`, validWorkerAttrs()),
	} {
		_, err := decodeWorkerHCL(t, bad, nil)
		require.Error(t, err)
		require.ErrorContains(t, err, "qc")
	}

	// 2. qc with gatekeepers only (no criteria) succeeds, with default max_gatekeeper_reworks = 3.
	gkOnlyHCL := fmt.Sprintf(`worker "w" { %s
  qc {
    gatekeeper "unit" {
      command = ["go", "test", "./..."]
    }
  }
}`, validWorkerAttrs())
	wGKOnly, err := decodeWorkerHCL(t, gkOnlyHCL, nil)
	require.NoError(t, err)
	qcOnly := wGKOnly.WorkerConfig().QC
	require.NotNil(t, qcOnly)
	require.Equal(t, 3, qcOnly.MaxGatekeeperReworks)
	require.False(t, qcOnly.MaxGatekeeperReworksSet)
	require.True(t, qcOnly.Criteria.IsNull() || qcOnly.Criteria.LengthInt() == 0)
	require.Len(t, qcOnly.Gatekeepers, 1)
	require.Equal(t, "unit", qcOnly.Gatekeepers[0].Name)

	// 3. qc with explicit max_gatekeeper_reworks = 0 succeeds and marks MaxGatekeeperReworksSet = true.
	zeroReworkHCL := fmt.Sprintf(`worker "w" { %s
  qc {
    max_gatekeeper_reworks = 0
    gatekeeper "unit" {
      command = ["go", "test"]
    }
  }
}`, validWorkerAttrs())
	wZero, err := decodeWorkerHCL(t, zeroReworkHCL, nil)
	require.NoError(t, err)
	require.Equal(t, 0, wZero.WorkerConfig().QC.MaxGatekeeperReworks)
	require.True(t, wZero.WorkerConfig().QC.MaxGatekeeperReworksSet)

	// 4. dynamic "gatekeeper" inside qc expands via golden.
	dynGKHCL := fmt.Sprintf(`worker "w" { %s
  qc {
    dynamic "gatekeeper" {
      for_each = toset(["unit", "vet"])
      content "unit" {
        command = ["go", gatekeeper.value, "./..."]
      }
    }
  }
}`, validWorkerAttrs())
	// Duplicate label "unit" above should fail duplicate check; distinct labels via separate blocks or single element succeeds.
	_, err = decodeWorkerHCL(t, dynGKHCL, nil)
	require.Error(t, err)

	dynSingleGKHCL := fmt.Sprintf(`worker "w" { %s
  qc {
    dynamic "gatekeeper" {
      for_each = toset(["test"])
      content "unit" {
        command = ["go", gatekeeper.value, "./..."]
      }
    }
  }
}`, validWorkerAttrs())
	wDyn, err := decodeWorkerHCL(t, dynSingleGKHCL, nil)
	require.NoError(t, err)
	require.Len(t, wDyn.WorkerConfig().QC.Gatekeepers, 1)
	require.Equal(t, []string{"go", "test", "./..."}, wDyn.WorkerConfig().QC.Gatekeepers[0].Command)

	// 5. Invalid qc configurations: neither gatekeeper nor criteria, negative max_gatekeeper_reworks, duplicate gatekeepers.
	for _, bad := range []string{
		fmt.Sprintf(`worker "w" { %s
  qc {
    max_qc_rounds = 2
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  qc {
    criteria = null
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  qc {
    max_gatekeeper_reworks = -1
    criteria               = { ok = "yes" }
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  qc {
    max_gatekeeper_reworks = 1.5
    criteria               = { ok = "yes" }
  }
}`, validWorkerAttrs()),
		fmt.Sprintf(`worker "w" { %s
  qc {
    gatekeeper "dup" { command = ["go", "test"] }
    gatekeeper "dup" { command = ["go", "vet"] }
  }
}`, validWorkerAttrs()),
	} {
		_, err := decodeWorkerHCL(t, bad, nil)
		require.Error(t, err)
	}
}
