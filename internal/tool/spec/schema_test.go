package spec_test

import (
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/go-defaults"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

type toolTestConfig struct {
	*golden.BaseConfig
	canonicalPrefix string
}

func (c *toolTestConfig) CanonicalAddress(address string) string {
	return c.canonicalPrefix + "." + address
}

var registerToolBlocks sync.Once

func TestGoToolBlockValidAndSchemas(t *testing.T) {
	cfg := parseToolConfig(t, `
go_tool "submit_checks" {
  description  = "Submit email check mapping"
  dependencies = ["github.com/google/uuid@v1.6.0"]
  source = <<-GO
    import (
      "context"
      "github.com/google/uuid"
    )
    type Filter struct {
      Prefix string `+"`json:\"prefix\"`"+`
    }
    type Input struct {
      Checks map[string]string `+"`json:\"checks\"`"+`
      Filter *Filter           `+"`json:\"filter\"`"+`
    }
    type Output string
    func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
      out := Output(uuid.NewString())
      return ToolResponse[Output]{Accepted: true, Output: &out}, nil
    }
  GO
}
`)
	require.NoError(t, cfg.RunPlan())

	blocks := golden.Blocks[*toolspec.GoToolBlock](cfg)
	require.Len(t, blocks, 1)
	b := blocks[0]
	assert.Equal(t, "Submit email check mapping", b.Description)
	assert.Equal(t, []string{"github.com/google/uuid@v1.6.0"}, b.Dependencies)

	inSchema, err := b.InputConstraint().JSONSchema()
	require.NoError(t, err)
	assert.Equal(t, []string{"checks"}, inSchema["required"])
	props := inSchema["properties"].(map[string]any)
	assert.Contains(t, props, "checks")
	assert.Contains(t, props, "filter")

	outSchema, err := b.OutputConstraint().JSONSchema()
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"type": "string"}, outSchema)
}

func TestGoToolBlockValidationAndNullOptional(t *testing.T) {
	t.Run("explicit null dependencies treated as empty", func(t *testing.T) {
		cfg := parseToolConfig(t, `
go_tool "ok" {
  description  = "Ok"
  dependencies = null
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}
`)
		require.NoError(t, cfg.RunPlan())
		blocks := golden.Blocks[*toolspec.GoToolBlock](cfg)
		require.Len(t, blocks, 1)
		assert.Empty(t, blocks[0].Dependencies)
	})

	tests := []struct {
		name    string
		source  string
		wantErr string
	}{
		{
			name: "always analyzes source even without postcondition - non-struct Input",
			source: `go_tool "bad" {
  description = "Bad"
  source = <<-GO
    import "context"
    type Input string
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}`,
			wantErr: "Input must be a struct",
		},
		{
			name: "always analyzes source even without postcondition - missing Invoke",
			source: `go_tool "bad" {
  description = "Bad"
  source = "type Input struct{}\ntype Output string"
}`,
			wantErr: "Invoke must have signature",
		},
		{
			name: "invalid dependency flag injection",
			source: `go_tool "bad" {
  description  = "Bad"
  dependencies = ["-modfile=x"]
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}`,
			wantErr: "invalid dependency",
		},
		{
			name: "null dependency element rejected",
			source: `go_tool "bad" {
  description  = "Bad"
  dependencies = [null]
  source = "type Input struct{}"
}`,
			wantErr: "must not contain null elements",
		},
		{
			name: "null description rejected",
			source: `go_tool "bad" {
  description = null
  source      = "type Input struct{}"
}`,
			wantErr: "description must be a known string",
		},
		{
			name: "empty description rejected",
			source: `go_tool "bad" {
  description = " "
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}`,
			wantErr: "description is required",
		},
		{
			name: "postcondition typo caught at plan time",
			source: `go_tool "bad" {
  description = "Bad"
  source = <<-GO
    import "context"
    type Input struct { Query string ` + "`json:\"query\"`" + ` }
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
  postcondition {
    condition     = input.typo == ""
    error_message = "bad"
  }
}`,
			wantErr: "Unsupported attribute",
		},
		{
			name: "postcondition non-bool caught at plan time",
			source: `go_tool "bad" {
  description = "Bad"
  source = <<-GO
    import "context"
    type Input struct { Query string ` + "`json:\"query\"`" + ` }
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
  postcondition {
    condition     = input.query
    error_message = "bad"
  }
}`,
			wantErr: "condition must return bool",
		},
		{
			name: "unknown attribute rejected",
			source: `go_tool "bad" {
  description = "Bad"
  source      = "type Input struct{}"
  extra       = true
}`,
			wantErr: `An argument named "extra" is not expected here`,
		},
		{
			name: "unknown nested block rejected",
			source: `go_tool "bad" {
  description = "Bad"
  source      = "type Input struct{}"
  retry {}
}`,
			wantErr: `Blocks of type "retry" are not expected here`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := parseToolConfig(t, tt.source)
			err := cfg.RunPlan()
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestExternalToolBlockValidAndSchema(t *testing.T) {
	cfg := parseToolConfig(t, `
external_tool "search_catalog" {
  description = "Search the catalog"
  program     = ["nonexistent-python-in-container", "--json"]
  working_dir = null

  input_type = object({
    limit = optional(number, 10)
    tags  = list(string)
  })

  output_type = object({
    matches = list(string)
  })

  postcondition {
    condition     = length(output.matches) >= 0
    error_message = "matches must be a list"
  }
}
`)
	require.NoError(t, cfg.RunPlan())

	blocks := golden.Blocks[*toolspec.ExternalToolBlock](cfg)
	require.Len(t, blocks, 1)
	b := blocks[0]
	assert.Equal(t, "", b.WorkingDir)
	assert.Equal(t, []string{"nonexistent-python-in-container", "--json"}, b.Program)

	schema, err := b.InputConstraint().JSONSchema()
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{"type": "number"},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"tags"},
		"additionalProperties": false,
	}, schema)
}

func TestExternalToolBlockValidation(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr string
	}{
		{
			name: "non-object input_type rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool"]
  input_type  = string
  output_type = string
}`,
			wantErr: "external tool input_type must be an object type",
		},
		{
			name: "any input_type rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool"]
  input_type  = any
  output_type = string
}`,
			wantErr: "dynamic type",
		},
		{
			name: "any output_type rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool"]
  input_type  = object({})
  output_type = any
}`,
			wantErr: "dynamic type",
		},
		{
			name: "missing program rejected",
			source: `external_tool "bad" {
  description = "Bad"
  input_type  = object({})
  output_type = string
}`,
			wantErr: "external tool program is required",
		},
		{
			name: "blank program executable rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["  "]
  input_type  = object({})
  output_type = string
}`,
			wantErr: "external tool program must contain an executable",
		},
		{
			name: "null program element rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool", null]
  input_type  = object({})
  output_type = string
}`,
			wantErr: "external tool program must not contain null elements",
		},
		{
			name: "invalid working_dir type rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool"]
  working_dir = 123
  input_type  = object({})
  output_type = string
}`,
			wantErr: "external tool working_dir must be a known string or null",
		},
		{
			name: "postcondition typo rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool"]
  input_type  = object({ query = string })
  output_type = string
  postcondition {
    condition     = input.typo == ""
    error_message = "bad"
  }
}`,
			wantErr: "Unsupported attribute",
		},
		{
			name: "postcondition known false rejected",
			source: `external_tool "bad" {
  description = "Bad"
  program     = ["tool"]
  input_type  = object({})
  output_type = string
  postcondition {
    condition     = false
    error_message = "always fails"
  }
}`,
			wantErr: "always fails",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := parseToolConfig(t, tt.source)
			err := cfg.RunPlan()
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestToolBlocksPreserveGoldenMetaSyntax(t *testing.T) {
	cfg := parseToolConfig(t, `
external_tool "dep" {
  description = "Dependency"
  program     = ["dep"]
  input_type  = object({})
  output_type = string
}

external_tool "target" {
  description = "Target"
  program     = ["target"]
  input_type  = object({ query = string })
  output_type = string
  depends_on  = [external_tool.dep]

  precondition {
    condition     = true
    error_message = "precondition ok"
  }

  postcondition {
    condition     = input.query != ""
    error_message = "query required"
  }
}
`)
	require.NoError(t, cfg.RunPlan())
	ancestors, err := cfg.GetAncestors("external_tool.target")
	require.NoError(t, err)
	assert.Contains(t, ancestors, "external_tool.dep")
	blocks := golden.Blocks[*toolspec.ExternalToolBlock](cfg)
	var target *toolspec.ExternalToolBlock
	for _, b := range blocks {
		if b.Name() == "target" {
			target = b
		}
	}
	require.NotNil(t, target)
	require.Len(t, target.Postconditions, 1)
	assert.Equal(t, "query required", target.Postconditions[0].ErrorMessage)
}

func TestToolBlocksSupportForEachAndDynamicPostcondition(t *testing.T) {
	t.Run("for_each on go_tool and external_tool", func(t *testing.T) {
		cfg := parseToolConfig(t, `
go_tool "multi_go" {
  for_each    = toset(["a", "b"])
  description = "Go ${each.key}"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

external_tool "multi_ext" {
  for_each    = { first = "/tmp/first", second = "/tmp/second" }
  description = "Ext ${each.key}"
  program     = ["tool", each.key]
  working_dir = each.value
  input_type  = object({ q = string })
  output_type = string
}
`)
		require.NoError(t, cfg.RunPlan())

		goBlocks := golden.Blocks[*toolspec.GoToolBlock](cfg)
		require.Len(t, goBlocks, 2)
		extBlocks := golden.Blocks[*toolspec.ExternalToolBlock](cfg)
		require.Len(t, extBlocks, 2)
	})

	t.Run("dynamic postcondition expands multiple postconditions", func(t *testing.T) {
		cfg := parseToolConfig(t, `
external_tool "checked" {
  description = "Checked"
  program     = ["checked"]
  input_type  = object({ query = string })
  output_type = string

  dynamic "postcondition" {
    for_each = { non_empty = "query must be present", bounded = "query must fit" }
    content {
      condition     = input.query != ""
      error_message = "${postcondition.key}: ${postcondition.value}"
    }
  }
}
`)
		require.NoError(t, cfg.RunPlan())
		blocks := golden.Blocks[*toolspec.ExternalToolBlock](cfg)
		require.Len(t, blocks, 1)
		require.Len(t, blocks[0].Postconditions, 2)
		assert.Equal(t, "bounded: query must fit", blocks[0].Postconditions[0].ErrorMessage)
		assert.Equal(t, "non_empty: query must be present", blocks[0].Postconditions[1].ErrorMessage)
	})

	t.Run("dynamic postcondition catches invalid field in content", func(t *testing.T) {
		cfg := parseToolConfig(t, `
go_tool "bad_dynamic" {
  description = "Bad dynamic"
  source = <<-GO
    import "context"
    type Input struct { Query string `+"`json:\"query\"`"+` }
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO

  dynamic "postcondition" {
    for_each = ["a"]
    content {
      condition     = input.typo != ""
      error_message = "invalid ${postcondition.value}"
    }
  }
}
`)
		err := cfg.RunPlan()
		require.Error(t, err)
		assert.ErrorContains(t, err, "Unsupported attribute")
	})

	t.Run("dynamic non-postcondition block rejected", func(t *testing.T) {
		cfg := parseToolConfig(t, `
external_tool "bad_dynamic_block" {
  description = "Bad dynamic"
  program     = ["tool"]
  input_type  = object({})
  output_type = string

  dynamic "retry" {
    for_each = ["a"]
    content {}
  }
}
`)
		err := cfg.RunPlan()
		require.Error(t, err)
		assert.ErrorContains(t, err, `Blocks of type "retry" are not expected here`)
	})

	t.Run("dynamic postcondition custom iterator valid", func(t *testing.T) {
		cfg := parseToolConfig(t, `
external_tool "custom_iter" {
  description = "Custom iterator"
  program     = ["tool"]
  input_type  = object({ query = string })
  output_type = string

  dynamic "postcondition" {
    for_each = { q = "must be set" }
    iterator = rule
    content {
      condition     = input.query != ""
      error_message = "${rule.key}: ${rule.value}"
    }
  }
}
`)
		require.NoError(t, cfg.RunPlan())
		blocks := golden.Blocks[*toolspec.ExternalToolBlock](cfg)
		require.Len(t, blocks, 1)
		require.Len(t, blocks[0].Postconditions, 1)
		assert.Equal(t, "q: must be set", blocks[0].Postconditions[0].ErrorMessage)
	})

	t.Run("dynamic postcondition invalid iterator and labeled content rejected", func(t *testing.T) {
		cases := []struct {
			name    string
			dynamic string
			wantErr string
		}{
			{
				name: "string literal iterator",
				dynamic: `
  dynamic "postcondition" {
    for_each = ["a"]
    iterator = "str"
    content {
      condition     = input.query != ""
      error_message = "bad"
    }
  }`,
				wantErr: "iterator must be a single identifier",
			},
			{
				name: "traversal iterator",
				dynamic: `
  dynamic "postcondition" {
    for_each = ["a"]
    iterator = a.b
    content {
      condition     = input.query != ""
      error_message = "bad"
    }
  }`,
				wantErr: "iterator must be a single identifier",
			},
			{
				name: "reserved iterator input",
				dynamic: `
  dynamic "postcondition" {
    for_each = ["a"]
    iterator = input
    content {
      condition     = input.query != ""
      error_message = "bad"
    }
  }`,
				wantErr: "reserved",
			},
			{
				name: "labeled content block",
				dynamic: `
  dynamic "postcondition" {
    for_each = ["a"]
    content "unexpected_label" {
      condition     = input.query != ""
      error_message = "bad"
    }
  }`,
				wantErr: "content block must not have labels",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg := parseToolConfig(t, `
external_tool "bad_iter" {
  description = "Bad"
  program     = ["tool"]
  input_type  = object({ query = string })
  output_type = string
`+tc.dynamic+"\n}")
				err := cfg.RunPlan()
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErr)
			})
		}
	})
}

func TestToolBlocksPreserveSensitivityPrePlanDynamicValResetAndDeepCopy(t *testing.T) {
	t.Run("pre-plan Value returns DynamicVal", func(t *testing.T) {
		g := &toolspec.GoToolBlock{}
		e := &toolspec.ExternalToolBlock{}
		assert.True(t, g.Value().RawEquals(cty.DynamicVal))
		assert.True(t, e.Value().RawEquals(cty.DynamicVal))
	})

	t.Run("Decode preserves sensitive marks and resets state across re-decode", func(t *testing.T) {
		registerToolBlocks.Do(func() {
			defaults.SetDefaults(&struct{}{})
			golden.RegisterBlock(new(toolspec.GoToolBlock))
			golden.RegisterBlock(new(toolspec.ExternalToolBlock))
		})

		parseSingle := func(src string) *golden.HclBlock {
			sf, diags := hclsyntax.ParseConfig([]byte(src), "s.sofa.hcl", hcl.InitialPos)
			require.False(t, diags.HasErrors(), diags.Error())
			wf, diags := hclwrite.ParseConfig([]byte(src), "s.sofa.hcl", hcl.InitialPos)
			require.False(t, diags.HasErrors(), diags.Error())
			body := sf.Body.(*hclsyntax.Body)
			blocks := golden.AsHclBlocks(body.Blocks, wf.Body().Blocks())
			require.Len(t, blocks, 1)
			return blocks[0]
		}

		evalCtx := &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"secret": cty.ObjectVal(map[string]cty.Value{
					"desc":   corespec.MarkSensitive(cty.StringVal("secret-desc")),
					"source": corespec.MarkSensitive(cty.StringVal("import \"context\"\ntype Quote string\ntype Input struct{ Q Quote `json:\"q\"` }\ntype Output string\nfunc Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }")),
					"dep":    corespec.MarkSensitive(cty.StringVal("github.com/google/uuid@v1.6.0")),
					"arg":    corespec.MarkSensitive(cty.StringVal("--secret")),
					"dir":    corespec.MarkSensitive(cty.StringVal("/secret/dir")),
				}),
			},
		}

		goHcl := parseSingle(`
go_tool "s" {
  description  = secret.desc
  dependencies = [secret.dep]
  source       = secret.source
}
`)
		goBlock := &toolspec.GoToolBlock{BaseBlock: golden.NewBaseBlock(nil, goHcl)}
		assert.True(t, goBlock.Value().RawEquals(cty.DynamicVal))
		require.NoError(t, goBlock.Decode(goHcl, evalCtx))
		require.NoError(t, goBlock.ExecuteDuringPlan())

		goVal := goBlock.Value()
		assert.True(t, goVal.GetAttr("description").HasMark(golden.SensitiveMark))
		assert.True(t, goVal.GetAttr("source").HasMark(golden.SensitiveMark))
		assert.True(t, goVal.GetAttr("dependencies").Index(cty.NumberIntVal(0)).HasMark(golden.SensitiveMark))
		assert.Equal(t, "<sensitive>", golden.CtyValueToString(goVal.GetAttr("description")))

		// Deep-copy checks for QuotePaths and GoToolConfig
		qp := goBlock.QuotePaths()
		require.Equal(t, [][]string{{"q"}}, qp)
		qp[0][0] = "mutated"
		assert.Equal(t, [][]string{{"q"}}, goBlock.QuotePaths())

		goCfg := goBlock.GoToolConfig()
		goCfg.Dependencies[0] = "mutated"
		goCfg.QuotePaths[0][0] = "mutated"
		assert.Equal(t, []string{"github.com/google/uuid@v1.6.0"}, goBlock.GoToolConfig().Dependencies)
		assert.Equal(t, [][]string{{"q"}}, goBlock.GoToolConfig().QuotePaths)

		// Re-decoding without dependencies resets old dependencies and planned value
		goHcl2 := parseSingle(`
go_tool "s" {
  description = "plain"
  source      = "import \"context\"\ntype Input struct{}\ntype Output string\nfunc Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }"
}
`)
		require.NoError(t, goBlock.Decode(goHcl2, evalCtx))
		assert.True(t, goBlock.Value().RawEquals(cty.DynamicVal))
		require.NoError(t, goBlock.ExecuteDuringPlan())
		assert.Empty(t, goBlock.GoToolConfig().Dependencies)
		assert.False(t, goBlock.Value().GetAttr("description").HasMark(golden.SensitiveMark))

		extHcl := parseSingle(`
external_tool "e" {
  description = secret.desc
  program     = ["cli", secret.arg]
  working_dir = secret.dir
  input_type  = object({ q = string })
  output_type = string
}
`)
		extBlock := &toolspec.ExternalToolBlock{BaseBlock: golden.NewBaseBlock(nil, extHcl)}
		assert.True(t, extBlock.Value().RawEquals(cty.DynamicVal))
		require.NoError(t, extBlock.Decode(extHcl, evalCtx))
		require.NoError(t, extBlock.ExecuteDuringPlan())

		extVal := extBlock.Value()
		assert.True(t, extVal.GetAttr("description").HasMark(golden.SensitiveMark))
		assert.True(t, extVal.GetAttr("working_dir").HasMark(golden.SensitiveMark))
		assert.False(t, extVal.GetAttr("program").Index(cty.NumberIntVal(0)).HasMark(golden.SensitiveMark))
		assert.True(t, extVal.GetAttr("program").Index(cty.NumberIntVal(1)).HasMark(golden.SensitiveMark))

		extCfg := extBlock.ExternalToolConfig()
		extCfg.Program[0] = "mutated"
		assert.Equal(t, []string{"cli", "--secret"}, extBlock.ExternalToolConfig().Program)

		// Re-decoding without working_dir resets old working_dir
		extHcl2 := parseSingle(`
external_tool "e" {
  description = "plain"
  program     = ["cli"]
  input_type  = object({})
  output_type = string
}
`)
		require.NoError(t, extBlock.Decode(extHcl2, evalCtx))
		assert.True(t, extBlock.Value().RawEquals(cty.DynamicVal))
		require.NoError(t, extBlock.ExecuteDuringPlan())
		assert.Equal(t, "", extBlock.ExternalToolConfig().WorkingDir)
	})
}

func parseToolConfig(t *testing.T, source string) *toolTestConfig {
	t.Helper()
	return parseToolConfigWithPrefix(t, "module.parent", source)
}

func parseToolConfigWithPrefix(t *testing.T, canonicalPrefix, source string) *toolTestConfig {
	t.Helper()

	registerToolBlocks.Do(func() {
		defaults.SetDefaults(&struct{}{})
		golden.RegisterBlock(new(toolspec.GoToolBlock))
		golden.RegisterBlock(new(toolspec.ExternalToolBlock))
	})

	syntaxFile, diagnostics := hclsyntax.ParseConfig([]byte(source), "tool.sofa.hcl", hcl.InitialPos)
	require.False(t, diagnostics.HasErrors(), diagnostics.Error())
	writeFile, diagnostics := hclwrite.ParseConfig([]byte(source), "tool.sofa.hcl", hcl.InitialPos)
	require.False(t, diagnostics.HasErrors(), diagnostics.Error())
	body, ok := syntaxFile.Body.(*hclsyntax.Body)
	require.True(t, ok)

	cfg := config.NewBaseConfig(golden.NewBaseConfigArgs{
		DslAbbreviation: "sofa",
		DslFullName:     "sofa",
	})
	wrapped := &toolTestConfig{
		BaseConfig:      cfg,
		canonicalPrefix: canonicalPrefix,
	}
	err := golden.InitConfig(wrapped, golden.AsHclBlocks(body.Blocks, writeFile.Body().Blocks()))
	require.NoError(t, err)
	return wrapped
}

var _ = cty.NilVal
