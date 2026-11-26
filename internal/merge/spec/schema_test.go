package spec

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

type stubMergeRegistry struct {
	sourceURLs     map[string]string
	upstreamURLs   map[string][]string
	locals         map[string]hcl.Expression
	sourceBranches []SourceBranchInfo
	preserved      []PreservedBranchInfo
}

func (s *stubMergeRegistry) LookupSourceURL(name string) (string, bool) {
	u, ok := s.sourceURLs[name]
	return u, ok
}

func (s *stubMergeRegistry) SingleRootSourceURL() (string, int) {
	if len(s.sourceURLs) == 1 {
		for _, u := range s.sourceURLs {
			return u, 1
		}
	}
	return "", len(s.sourceURLs)
}

func (s *stubMergeRegistry) LookupUpstreamSourceURLs(address string) ([]string, bool) {
	u, ok := s.upstreamURLs[address]
	return u, ok
}

func (s *stubMergeRegistry) LookupVariableSourceURLs(name string) ([]string, bool) {
	return nil, false
}

func (s *stubMergeRegistry) LookupModuleOutputSourceURLs(moduleAddress, outputName string) ([]string, bool) {
	return nil, false
}

func (s *stubMergeRegistry) ResolveLocalExpr(name string) (hcl.Expression, bool) {
	e, ok := s.locals[name]
	return e, ok
}

func (s *stubMergeRegistry) SourceBranches() []SourceBranchInfo {
	return s.sourceBranches
}

func (s *stubMergeRegistry) PreservedKnownBranches() []PreservedBranchInfo {
	return s.preserved
}

type mergeTestConfig struct {
	*golden.BaseConfig
	*stubMergeRegistry
}

func decodeMergeHCL(t *testing.T, source string, variables map[string]cty.Value, reg *stubMergeRegistry) (*MergeBlock, error) {
	t.Helper()
	syntaxFile, diags := hclsyntax.ParseConfig([]byte(source), "merge.sofa.hcl", hcl.InitialPos)
	require.False(t, diags.HasErrors(), diags.Error())
	writeFile, diags := hclwrite.ParseConfig([]byte(source), "merge.sofa.hcl", hcl.InitialPos)
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
	var cfg golden.Config = base
	if reg != nil {
		cfg = &mergeTestConfig{BaseConfig: base, stubMergeRegistry: reg}
	}
	m := &MergeBlock{
		BaseBlock: golden.NewBaseBlock(cfg, hb),
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
	ctx.Variables["worker"] = cty.ObjectVal(map[string]cty.Value{
		"discover": cty.ObjectVal(map[string]cty.Value{
			"work_branch": cty.UnknownVal(cty.String),
		}),
		"a": cty.ObjectVal(map[string]cty.Value{
			"work_branch": cty.UnknownVal(cty.String),
		}),
		"b": cty.ObjectVal(map[string]cty.Value{
			"work_branch": cty.UnknownVal(cty.String),
		}),
	})
	ctx.Variables["dynamic_worker"] = cty.ObjectVal(map[string]cty.Value{
		"verify": cty.ObjectVal(map[string]cty.Value{
			"work_branches": cty.UnknownVal(cty.List(cty.String)),
		}),
	})
	for k, v := range variables {
		ctx.Variables[k] = v
	}
	err := m.Decode(hb, ctx)
	return m, err
}

func TestMergeDefaultsAndValidSources(t *testing.T) {
	// Without gatekeeper, sandbox may be omitted; defaults apply.
	src := `
merge "integrate" {
  based_source = worker.discover.work_branch
  sources      = dynamic_worker.verify.work_branches
}
`
	m, err := decodeMergeHCL(t, src, nil, nil)
	require.NoError(t, err)
	require.True(t, m.Value().RawEquals(cty.DynamicVal))
	require.NoError(t, m.ExecuteDuringPlan())

	cfg := m.MergeConfig()
	require.Equal(t, "integrate", cfg.Name)
	require.Nil(t, cfg.TargetBranch)
	require.Equal(t, "rebase", cfg.Strategy)
	require.Equal(t, 2, cfg.MaxReworks)
	require.Equal(t, "optional", cfg.HumanApproval)
	require.True(t, cfg.Sandbox.IsNull())

	val := m.Value()
	require.Equal(t, "merge.integrate", val.GetAttr("address").AsString())
	require.Equal(t, "merge", val.GetAttr("kind").AsString())
	require.Equal(t, "integrate", val.GetAttr("name").AsString())
	require.True(t, val.Type().HasAttribute("work_branch"))
	require.True(t, val.Type().HasAttribute("commit_sha"))

	// Static non-empty sources, concat(), and max_reworks = 0 all pass.
	for _, sourcesExpr := range []string{
		`[worker.a.work_branch, worker.b.work_branch]`,
		`toset(["feat/a", "feat/b"])`,
		`concat(dynamic_worker.verify.work_branches, [worker.a.work_branch])`,
	} {
		t.Run(sourcesExpr, func(t *testing.T) {
			s := fmt.Sprintf(`
merge "integrate" {
  based_source   = "main"
  target_branch  = "release/v1"
  sources        = %s
  strategy       = "rebase"
  max_reworks    = 0
  human_approval = "required"
  sandbox        = sandbox.go_env

  gatekeeper "regression" {
    command = ["go", "test", "./...", "-count=1"]
  }
}
`, sourcesExpr)
			m2, err := decodeMergeHCL(t, s, nil, nil)
			require.NoError(t, err)
			require.NoError(t, m2.ExecuteDuringPlan())
			c2 := m2.MergeConfig()
			require.NotNil(t, c2.TargetBranch)
			require.Equal(t, "release/v1", *c2.TargetBranch)
			require.Equal(t, 0, c2.MaxReworks)
			require.Equal(t, "required", c2.HumanApproval)
			require.Len(t, c2.Gatekeepers, 1)

			// Deep copy isolation.
			*c2.TargetBranch = "mutated"
			c2.Gatekeepers[0].Command[0] = "mutated"
			c3 := m2.MergeConfig()
			require.Equal(t, "release/v1", *c3.TargetBranch)
			require.Equal(t, "go", c3.Gatekeepers[0].Command[0])
		})
	}
}

func TestMergeRejectsInvalidSourcesAndUnsupportedLLMFields(t *testing.T) {
	invalidSources := []string{
		`[]`,
		`null`,
		`"feat/a"`,
		`[1, 2]`,
		`[true]`,
		`[null]`,
		`["bad..branch"]`,
		`jsondecode(worker.discover.result)`,
		`concat(dynamic_worker.verify.work_branches, [123])`,
	}
	for _, expr := range invalidSources {
		t.Run("sources_"+expr, func(t *testing.T) {
			src := fmt.Sprintf(`
merge "integrate" {
  based_source = "main"
  sources      = %s
}
`, expr)
			_, err := decodeMergeHCL(t, src, map[string]cty.Value{
				"worker": cty.ObjectVal(map[string]cty.Value{
					"discover": cty.ObjectVal(map[string]cty.Value{
						"work_branch": cty.UnknownVal(cty.String),
						"result":      cty.UnknownVal(cty.String),
					}),
					"a": cty.ObjectVal(map[string]cty.Value{
						"work_branch": cty.UnknownVal(cty.String),
					}),
				}),
			}, nil)
			require.Error(t, err)
		})
	}

	unsupported := []struct {
		name string
		body string
	}{
		{name: "model", body: `model = "gpt-5"`},
		{name: "prompt", body: `prompt = "fix conflicts"`},
		{name: "system_prompt", body: `system_prompt = "fix conflicts"`},
		{name: "harness", body: `harness = "local"`},
		{name: "model_provider", body: `model_provider = null`},
		{name: "conflict_resolution_attr", body: `conflict_resolution = "agent"`},
		{name: "conflict_resolution_block", body: `conflict_resolution {}`},
		{name: "qc_block", body: `qc { criteria = { a = "b" } }`},
		{name: "fallback_block", body: "fallback \"f\" {\n  on = [\"gatekeeper_exhausted\"]\n  target = worker.a\n}"},
		{name: "retry_block", body: `retry { lifecycle_retries = 1 }`},
	}
	for _, tc := range unsupported {
		t.Run("unsupported_"+tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`
merge "integrate" {
  based_source = "main"
  sources      = [worker.a.work_branch]
  %s
}
`, tc.body)
			_, err := decodeMergeHCL(t, src, nil, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "unsupported")
		})
	}
}

func TestMergeGatekeeperSandboxTargetBranchAndPreserveGuard(t *testing.T) {
	// Gatekeeper without sandbox fails; dynamic gatekeeper without sandbox fails.
	for _, gkBlock := range []string{
		`gatekeeper "test" { command = ["go", "test"] }`,
		`dynamic "gatekeeper" {
           for_each = toset(["test"])
           content "test" { command = ["go", "test"] }
         }`,
	} {
		src := fmt.Sprintf(`
merge "integrate" {
  based_source = "main"
  sources      = [worker.a.work_branch]
  %s
}
`, gkBlock)
		_, err := decodeMergeHCL(t, src, nil, nil)
		require.Error(t, err)
	}

	// Unknown or invalid target_branch fails.
	for _, tb := range []string{`worker.a.work_branch`, `"bad..branch"`, `""`} {
		src := fmt.Sprintf(`
merge "integrate" {
  based_source  = "main"
  target_branch = %s
  sources       = [worker.a.work_branch]
}
`, tb)
		_, err := decodeMergeHCL(t, src, nil, nil)
		require.Error(t, err)
	}

	// Unprotected source branch fails during ExecuteDuringPlan().
	unprotectedReg := &stubMergeRegistry{
		sourceURLs: map[string]string{"project": "https://github.com/example/repo.git"},
		sourceBranches: []SourceBranchInfo{
			{URL: "https://github.com/example/repo.git", Branch: "main"},
		},
	}
	srcTargetMain := `
merge "integrate" {
  based_source  = "dev"
  target_branch = "main"
  sources       = [worker.a.work_branch]
}
`
	m, err := decodeMergeHCL(t, srcTargetMain, nil, unprotectedReg)
	require.NoError(t, err)
	err = m.ExecuteDuringPlan()
	require.Error(t, err)
	require.Contains(t, err.Error(), "preserve")

	// Protected by matching preserve block passes.
	protectedReg := &stubMergeRegistry{
		sourceURLs: map[string]string{"project": "https://github.com/example/repo.git"},
		sourceBranches: []SourceBranchInfo{
			{URL: "https://github.com/example/repo.git", Branch: "main"},
		},
		preserved: []PreservedBranchInfo{
			{URL: "https://github.com/example/repo.git", Branch: "main"},
		},
	}
	m2, err := decodeMergeHCL(t, srcTargetMain, nil, protectedReg)
	require.NoError(t, err)
	require.NoError(t, m2.ExecuteDuringPlan())

	// Distinct target_branch also passes.
	srcTargetRelease := `
merge "integrate" {
  based_source  = "main"
  target_branch = "integration/next"
  sources       = [worker.a.work_branch]
}
`
	m3, err := decodeMergeHCL(t, srcTargetRelease, nil, unprotectedReg)
	require.NoError(t, err)
	require.NoError(t, m3.ExecuteDuringPlan())
}
