package spec

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func decodeDynamicWorkerHCL(t *testing.T, source string, variables map[string]cty.Value) (*DynamicWorkerBlock, error) {
	t.Helper()
	syntaxFile, diags := hclsyntax.ParseConfig([]byte(source), "dynamic_worker.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	writeFile, diags := hclwrite.ParseConfig([]byte(source), "dynamic_worker.sofa.hcl", hcl.InitialPos)
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
	base := config.NewBaseConfig(golden.NewBaseConfigArgs{
		Basedir:         t.TempDir(),
		DslAbbreviation: "sofa",
		DslFullName:     "sofa",
	})
	d := &DynamicWorkerBlock{
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
	})
	ctx.Variables["harness"] = cty.ObjectVal(map[string]cty.Value{
		"local": cty.ObjectVal(map[string]cty.Value{
			"address": cty.StringVal("harness.local"),
			"kind":    cty.StringVal("harness"),
			"name":    cty.StringVal("local"),
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
	err := d.Decode(hb, ctx)
	return d, err
}

func baseDiscoverVars() map[string]cty.Value {
	return map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"discover": cty.ObjectVal(map[string]cty.Value{
				"work_branch": cty.UnknownVal(cty.String),
				"commit_sha":  cty.UnknownVal(cty.String),
				"result":      cty.UnknownVal(cty.String),
			}),
		}),
	}
}

func TestDynamicWorkerExplicitAndDefaultIterator(t *testing.T) {
	explicitHCL := `
dynamic_worker "verify_notifications" {
  parallelism = 4
  serial      = false

  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(string) })
    ).checks
    iterator = check

    content {
      based_source  = worker.discover.work_branch
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify notification."
      prompt        = "Check ${check.key}: ${check.value}"

      gatekeeper "test" {
        command = ["go", "test", "./..."]
      }
    }
  }
}
`
	d, err := decodeDynamicWorkerHCL(t, explicitHCL, baseDiscoverVars())
	require.NoError(t, err)
	require.True(t, d.Value().RawEquals(cty.DynamicVal))
	require.NoError(t, d.ExecuteDuringPlan())

	cfg := d.DynamicWorkerConfig()
	require.Equal(t, "verify_notifications", cfg.Name)
	require.NotNil(t, cfg.Parallelism)
	require.Equal(t, 4, *cfg.Parallelism)
	require.False(t, cfg.Serial)
	require.Equal(t, "check", cfg.IteratorName)
	require.NotEmpty(t, cfg.ContentTemplate)
	_, diags := hclsyntax.ParseExpression([]byte(cfg.ContentTemplate), "template.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	require.Len(t, cfg.Content.Gatekeepers, 1)

	defaultIterHCL := `
dynamic_worker "verify_default" {
  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(string) })
    ).checks

    content {
      based_source  = worker.discover.work_branch
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify notification."
      prompt        = "Check ${worker.key}: ${worker.value}"
    }
  }
}
`
	d2, err := decodeDynamicWorkerHCL(t, defaultIterHCL, baseDiscoverVars())
	require.NoError(t, err)
	require.NoError(t, d2.ExecuteDuringPlan())
	cfg2 := d2.DynamicWorkerConfig()
	require.Nil(t, cfg2.Parallelism)
	require.False(t, cfg2.Serial)
	require.Equal(t, "worker", cfg2.IteratorName)
	require.NotEmpty(t, cfg2.ContentTemplate)

	// Static worker named "key" or "value" conflicts with omitted iterator = "worker".
	conflictVars := map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"key": cty.ObjectVal(map[string]cty.Value{
				"work_branch": cty.UnknownVal(cty.String),
			}),
		}),
	}
	_, err = decodeDynamicWorkerHCL(t, defaultIterHCL, conflictVars)
	require.Error(t, err)
}

func TestDynamicWorkerForEachTypeRules(t *testing.T) {
	makeHCL := func(forEachExpr, promptExpr string) string {
		return fmt.Sprintf(`
dynamic_worker "checks" {
  dynamic "worker" {
    for_each = %s
    iterator = check

    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Check."
      prompt        = %s
    }
  }
}
`, forEachExpr, promptExpr)
	}

	// Empty object {} is valid at Plan time.
	d, err := decodeDynamicWorkerHCL(t, makeHCL(`{}`, `"${check.key}: ${check.value}"`), nil)
	require.NoError(t, err)
	require.NoError(t, d.ExecuteDuringPlan())

	// set(string) is valid.
	d, err = decodeDynamicWorkerHCL(t, makeHCL(`toset(["a", "b"])`, `"${check.key}: ${check.value}"`), nil)
	require.NoError(t, err)
	require.NoError(t, d.ExecuteDuringPlan())

	// Homogeneous object is valid.
	d, err = decodeDynamicWorkerHCL(t, makeHCL(`{ a = "one", b = "two" }`, `"${check.key}: ${check.value}"`), nil)
	require.NoError(t, err)
	require.NoError(t, d.ExecuteDuringPlan())

	// Invalid for_each expressions MUST fail.
	invalidCases := []struct {
		name string
		expr string
		vars map[string]cty.Value
	}{
		{name: "null", expr: `null`},
		{name: "number", expr: `123`},
		{name: "string", expr: `"abc"`},
		{name: "list", expr: `["a", "b"]`},
		{name: "number_set", expr: `toset([1, 2])`},
		{
			name: "unknown_map_dynamic",
			expr: `local.dyn_map`,
			vars: map[string]cty.Value{
				"local": cty.ObjectVal(map[string]cty.Value{
					"dyn_map": cty.UnknownVal(cty.Map(cty.DynamicPseudoType)),
				}),
			},
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeDynamicWorkerHCL(t, makeHCL(tc.expr, `"${check.key}"`), tc.vars)
			require.Error(t, err)
		})
	}
}

func TestDynamicWorkerTypedForEachFieldCheckAtPlanTime(t *testing.T) {
	hclTemplate := func(field string) string {
		return fmt.Sprintf(`
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(object({ rule = string })) })
    ).checks
    iterator = check

    content {
      based_source  = worker.discover.work_branch
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify."
      prompt        = "Verify ${check.key}: ${check.value.%s}"
    }
  }
}
`, field)
	}

	d, err := decodeDynamicWorkerHCL(t, hclTemplate("rule"), baseDiscoverVars())
	require.NoError(t, err)
	require.NoError(t, d.ExecuteDuringPlan())

	_, err = decodeDynamicWorkerHCL(t, hclTemplate("wrong_field"), baseDiscoverVars())
	require.Error(t, err)
}

func TestDynamicWorkerReservedAndInvalidIterators(t *testing.T) {
	for _, iterExpr := range []string{
		`var`,
		`local`,
		`worker`,
		`source`,
		`merge`,
		`dynamic_worker`,
		`each`,
		`"check"`,
		`a.b`,
	} {
		t.Run(iterExpr, func(t *testing.T) {
			src := fmt.Sprintf(`
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = %s

    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify."
    }
  }
}
`, iterExpr)
			_, err := decodeDynamicWorkerHCL(t, src, nil)
			require.Error(t, err)
		})
	}
}

func TestDynamicWorkerSchemaValidation(t *testing.T) {
	invalidConfigs := []struct {
		name string
		src  string
	}{
		{
			name: "missing_dynamic_block",
			src: `
dynamic_worker "verify" {
  parallelism = 2
}
`,
		},
		{
			name: "two_dynamic_blocks",
			src: `
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = {}
    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "s"
    }
  }
  dynamic "worker" {
    for_each = {}
    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "s"
    }
  }
}
`,
		},
		{
			name: "wrong_dynamic_label",
			src: `
dynamic_worker "verify" {
  dynamic "other" {
    for_each = {}
    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "s"
    }
  }
}
`,
		},
		{
			name: "labeled_content",
			src: `
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = {}
    content "extra" {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "s"
    }
  }
}
`,
		},
		{
			name: "non_positive_parallelism",
			src: `
dynamic_worker "verify" {
  parallelism = 0
  dynamic "worker" {
    for_each = {}
    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "s"
    }
  }
}
`,
		},
		{
			name: "unknown_top_attr",
			src: `
dynamic_worker "verify" {
  unknown_arg = true
  dynamic "worker" {
    for_each = {}
    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "s"
    }
  }
}
`,
		},
	}

	for _, tc := range invalidConfigs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeDynamicWorkerHCL(t, tc.src, nil)
			require.Error(t, err)
		})
	}
}

func TestDynamicWorkerValueAndDeepCopyIsolation(t *testing.T) {
	noTermHCL := `
dynamic_worker "verify" {
  parallelism = 2
  serial      = true

  dynamic "worker" {
    for_each = toset(["a", "b"])
    iterator = item

    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify ${item.key}"

      artifact "report" {
        type        = "file"
        path        = "reports/check.json"
        description = "check report"
      }
    }
  }
}
`
	d, err := decodeDynamicWorkerHCL(t, noTermHCL, nil)
	require.NoError(t, err)
	require.NoError(t, d.ExecuteDuringPlan())

	val := d.Value()
	require.Equal(t, "dynamic_worker.verify", val.GetAttr("address").AsString())
	require.Equal(t, "dynamic_worker", val.GetAttr("kind").AsString())
	require.Equal(t, "verify", val.GetAttr("name").AsString())

	workersType := val.Type().AttributeType("workers")
	require.True(t, workersType.IsMapType())
	itemType := workersType.ElementType()
	require.True(t, itemType.HasAttribute("work_branch"))
	require.True(t, itemType.HasAttribute("commit_sha"))
	require.True(t, itemType.HasAttribute("artifact"))
	require.True(t, itemType.AttributeType("artifact").HasAttribute("report"))
	require.False(t, itemType.HasAttribute("result"))

	branchesType := val.Type().AttributeType("work_branches")
	require.True(t, branchesType.IsListType())
	require.True(t, branchesType.ElementType().Equals(cty.String))

	// Deep copy isolation check.
	cfg1 := d.DynamicWorkerConfig()
	*cfg1.Parallelism = 99
	cfg1.Content.Artifacts["extra"] = Artifact{Name: "extra"}
	cfg2 := d.DynamicWorkerConfig()
	require.Equal(t, 2, *cfg2.Parallelism)
	require.NotContains(t, cfg2.Content.Artifacts, "extra")

	// Terminating tool exposes .result on item type.
	withTermHCL := `
dynamic_worker "verify_term" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = item

    content {
      based_source      = "main"
      sandbox           = sandbox.go_env
      harness           = harness.local
      model             = "gpt-5"
      system_prompt     = "Verify ${item.key}"
      terminate_tool_id = "tool_go_tool_finish_abc"
    }
  }
}
`
	dTerm, err := decodeDynamicWorkerHCL(t, withTermHCL, nil)
	require.NoError(t, err)
	require.NoError(t, dTerm.ExecuteDuringPlan())
	termItemType := dTerm.Value().Type().AttributeType("workers").ElementType()
	require.True(t, termItemType.HasAttribute("result"))
	require.True(t, termItemType.AttributeType("result").Equals(cty.String))
}

func TestDynamicWorkerSameNameStaticImportAndNestedCapsuleForEach(t *testing.T) {
	// dynamic_worker "verify" importing from worker "verify" MUST succeed.
	sameNameHCL := `
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check

    content {
      based_source  = worker.verify.work_branch
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify ${check.key}"
      prompt        = "Read ${import_artifact.spec.path}"

      import_artifact "spec" {
        desc    = "spec"
        sources = [worker.verify.artifact.spec]
      }
    }
  }
}
`
	vars := map[string]cty.Value{
		"worker": cty.ObjectVal(map[string]cty.Value{
			"verify": cty.ObjectVal(map[string]cty.Value{
				"work_branch": cty.UnknownVal(cty.String),
				"artifact": cty.ObjectVal(map[string]cty.Value{
					"spec": artifactValue(Artifact{
						Name:        "spec",
						Type:        "file",
						Path:        "docs/spec.md",
						Description: "spec",
					}),
				}),
			}),
		}),
	}
	d, err := decodeDynamicWorkerHCL(t, sameNameHCL, vars)
	require.NoError(t, err)
	require.NoError(t, d.ExecuteDuringPlan())

	// dynamic_worker "verify" importing from dynamic_worker.verify MUST fail.
	selfDynamicImportHCL := `
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check

    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify ${check.key}"

      import_artifact "self" {
        desc    = "self"
        sources = [dynamic_worker.verify.workers["a"].artifact.spec]
      }
    }
  }
}
`
	vars["dynamic_worker"] = cty.ObjectVal(map[string]cty.Value{
		"verify": cty.ObjectVal(map[string]cty.Value{
			"workers": cty.UnknownVal(cty.Map(cty.Object(map[string]cty.Type{
				"artifact": cty.Object(map[string]cty.Type{
					"spec": artifactValueType,
				}),
			}))),
		}),
	})
	_, err = decodeDynamicWorkerHCL(t, selfDynamicImportHCL, vars)
	require.Error(t, err)

	// Nested capsule in MapType and ObjectType for_each MUST be rejected.
	type capsulePayload struct{}
	capType := cty.Capsule("test_capsule", reflect.TypeOf(capsulePayload{}))
	capsuleHCL := `
dynamic_worker "bad_capsule" {
  dynamic "worker" {
    for_each = local.bad_map
    iterator = check

    content {
      based_source  = "main"
      sandbox       = sandbox.go_env
      harness       = harness.local
      model         = "gpt-5"
      system_prompt = "Verify ${check.key}"
    }
  }
}
`
	for _, badType := range []cty.Type{
		cty.Map(cty.Object(map[string]cty.Type{"bad": capType})),
		cty.Object(map[string]cty.Type{"a": cty.Object(map[string]cty.Type{"bad": capType})}),
	} {
		_, err = decodeDynamicWorkerHCL(t, capsuleHCL, map[string]cty.Value{
			"local": cty.ObjectVal(map[string]cty.Value{
				"bad_map": cty.UnknownVal(badType),
			}),
		})
		require.Error(t, err)
	}
}
