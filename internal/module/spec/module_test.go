package spec

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

type fakeModuleConfig struct {
	*golden.BaseConfig
	planFn func(ModulePlanRequest) (ModulePlan, error)
}

func (f *fakeModuleConfig) PlanChildModule(req ModulePlanRequest) (ModulePlan, error) {
	if f.planFn == nil {
		return ModulePlan{}, nil
	}
	return f.planFn(req)
}

func TestModuleBlockMetadataAndInterfaces(t *testing.T) {
	var b ModuleBlock
	var _ golden.CustomDecode = &b
	var _ golden.PlanBlock = &b
	var _ golden.SingleValueBlock = &b

	require.Equal(t, "", b.Type())
	require.Equal(t, "module", b.BlockType())
	require.Equal(t, 2, b.AddressLength())
	require.False(t, b.CanExecutePrePlan())
	require.Equal(t, cty.DynamicVal, b.Value())
}

func TestModuleBlockDecodeAndPlanSuccess(t *testing.T) {
	hclText := `
module "audit" {
  source      = "./modules/audit"
  parallelism = 4
  timeout     = "15m"
  depends_on  = [worker.specify]

  branch   = worker.specify.work_branch
  secret   = var.api_token
  artifact = {
    path = worker.specify.artifact.spec.path
    tags = ["a", "b"]
  }

  precondition {
    condition     = true
    error_message = "ok"
  }
}
`
	hb := parseSingleModuleHclBlock(t, hclText)
	var capturedReq ModulePlanRequest
	cfg := &fakeModuleConfig{
		BaseConfig: golden.NewBasicConfig(t.TempDir(), "sofa", "sofa", nil, nil, context.Background()),
		planFn: func(req ModulePlanRequest) (ModulePlan, error) {
			capturedReq = req
			return ModulePlan{
				Plan: Plan{
					Directory:  "/tmp/audit",
					BundlePath: "modules/audit",
					Outputs: map[string]Output{
						"verified_branch": {
							Name:       "verified_branch",
							Value:      cty.UnknownVal(cty.String),
							Expression: "worker.review.work_branch",
						},
						"token_echo": {
							Name:      "token_echo",
							Value:     corespec.MarkSensitive(cty.StringVal("ok")),
							Sensitive: true,
						},
					},
				},
				Address:          req.Address,
				Name:             req.Name,
				CanonicalAddress: req.Address,
				Source:           req.Source,
				Parallelism:      req.Parallelism,
				Timeout:          req.Timeout,
				TimeoutRaw:       req.TimeoutRaw,
				Inputs:           req.Inputs,
				InputExprs:       req.InputExprs,
			}, nil
		},
	}

	mb := &ModuleBlock{BaseBlock: golden.NewBaseBlock(cfg, hb)}
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"worker": cty.ObjectVal(map[string]cty.Value{
				"specify": cty.ObjectVal(map[string]cty.Value{
					"work_branch": cty.UnknownVal(cty.String),
					"artifact": cty.ObjectVal(map[string]cty.Value{
						"spec": cty.ObjectVal(map[string]cty.Value{
							"path": cty.StringVal("spec.md"),
						}),
					}),
				}),
			}),
			"var": cty.ObjectVal(map[string]cty.Value{
				"api_token": corespec.MarkSensitive(cty.StringVal("top-secret")),
			}),
		},
	}

	require.NoError(t, mb.Decode(hb, ctx))
	require.Equal(t, "./modules/audit", mb.Source)
	require.NotNil(t, mb.Parallelism)
	require.Equal(t, 4, *mb.Parallelism)
	require.NotNil(t, mb.Timeout)
	require.Equal(t, 15*time.Minute, *mb.Timeout)
	require.Equal(t, "15m", mb.TimeoutRaw)
	require.Equal(t, cty.UnknownVal(cty.String), mb.Inputs["branch"])
	require.True(t, corespec.IsSensitive(mb.Inputs["secret"]))
	require.Equal(t, "worker.specify.work_branch", mb.InputExprs["branch"])
	require.Equal(t, "var.api_token", mb.InputExprs["secret"])
	require.NotContains(t, mb.Inputs, "source")
	require.NotContains(t, mb.Inputs, "parallelism")
	require.NotContains(t, mb.Inputs, "timeout")
	require.NotContains(t, mb.Inputs, "depends_on")
	require.Equal(t, cty.DynamicVal, mb.Value())

	require.NoError(t, mb.ExecuteDuringPlan())
	require.Equal(t, "module.audit", capturedReq.Address)
	require.Equal(t, "audit", capturedReq.Name)
	require.NotNil(t, capturedReq.InputAttrs["branch"])

	val := mb.Value()
	require.True(t, val.Type().IsObjectType())
	require.False(t, val.GetAttr("verified_branch").IsKnown())
	require.True(t, val.GetAttr("verified_branch").Type().Equals(cty.String))
	require.True(t, corespec.IsSensitive(val.GetAttr("token_echo")))
}

func TestModuleBlockZeroOutputsReturnsEmptyObjectAfterPlan(t *testing.T) {
	hb := parseSingleModuleHclBlock(t, `
module "empty" {
  source      = "./empty"
  parallelism = null
  timeout     = null
}
`)
	cfg := &fakeModuleConfig{
		BaseConfig: golden.NewBasicConfig(t.TempDir(), "sofa", "sofa", nil, nil, context.Background()),
		planFn: func(req ModulePlanRequest) (ModulePlan, error) {
			return ModulePlan{
				Plan: Plan{Directory: "/tmp/empty"},
			}, nil
		},
	}
	mb := &ModuleBlock{BaseBlock: golden.NewBaseBlock(cfg, hb)}
	require.NoError(t, mb.Decode(hb, &hcl.EvalContext{}))
	require.Nil(t, mb.Parallelism)
	require.Nil(t, mb.Timeout)
	require.Equal(t, cty.DynamicVal, mb.Value())
	require.NoError(t, mb.ExecuteDuringPlan())
	require.True(t, mb.Value().RawEquals(cty.EmptyObjectVal))
}

func TestModuleBlockDynamicPreconditionEvaluated(t *testing.T) {
	hb := parseSingleModuleHclBlock(t, `
module "checked" {
  source = "./checked"

  dynamic "precondition" {
    for_each = ["must be non-empty"]
    content {
      condition     = var.flag
      error_message = precondition.value
    }
  }
}
`)
	mb := &ModuleBlock{BaseBlock: golden.NewBaseBlock(nil, hb)}
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				"flag": cty.BoolVal(false),
			}),
		},
	}
	require.NoError(t, mb.Decode(hb, ctx))
	failed, err := mb.PreConditionCheck(ctx)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	require.Equal(t, "must be non-empty", failed[0].ErrorMessage)
}

func TestModuleBlockDecodeRejections(t *testing.T) {
	tests := []struct {
		name    string
		hclText string
		ctx     *hcl.EvalContext
		wantErr string
	}{
		{
			name:    "missing source",
			hclText: `module "bad" {}`,
			wantErr: "source",
		},
		{
			name:    "empty source",
			hclText: `module "bad" { source = "   " }`,
			wantErr: "source",
		},
		{
			name:    "null source",
			hclText: `module "bad" { source = null }`,
			wantErr: "source",
		},
		{
			name: "source referencing variable",
			hclText: `module "bad" {
  source = var.mod_path
}`,
			ctx: &hcl.EvalContext{
				Variables: map[string]cty.Value{
					"var": cty.ObjectVal(map[string]cty.Value{"mod_path": cty.StringVal("./m")}),
				},
			},
			wantErr: "literal",
		},
		{
			name:    "zero parallelism",
			hclText: "module \"bad\" {\n  source      = \"./m\"\n  parallelism = 0\n}",
			wantErr: "parallelism",
		},
		{
			name:    "negative parallelism",
			hclText: "module \"bad\" {\n  source      = \"./m\"\n  parallelism = -2\n}",
			wantErr: "parallelism",
		},
		{
			name:    "fractional parallelism",
			hclText: "module \"bad\" {\n  source      = \"./m\"\n  parallelism = 1.5\n}",
			wantErr: "parallelism",
		},
		{
			name: "unknown parallelism",
			hclText: `module "bad" {
  source      = "./m"
  parallelism = var.p
}`,
			ctx: &hcl.EvalContext{
				Variables: map[string]cty.Value{
					"var": cty.ObjectVal(map[string]cty.Value{"p": cty.UnknownVal(cty.Number)}),
				},
			},
			wantErr: "parallelism",
		},
		{
			name:    "zero timeout",
			hclText: "module \"bad\" {\n  source  = \"./m\"\n  timeout = \"0s\"\n}",
			wantErr: "timeout",
		},
		{
			name:    "negative timeout",
			hclText: "module \"bad\" {\n  source  = \"./m\"\n  timeout = \"-5m\"\n}",
			wantErr: "timeout",
		},
		{
			name:    "invalid timeout",
			hclText: "module \"bad\" {\n  source  = \"./m\"\n  timeout = \"not-a-duration\"\n}",
			wantErr: "timeout",
		},
		{
			name: "unknown timeout",
			hclText: `module "bad" {
  source  = "./m"
  timeout = var.t
}`,
			ctx: &hcl.EvalContext{
				Variables: map[string]cty.Value{
					"var": cty.ObjectVal(map[string]cty.Value{"t": cty.UnknownVal(cty.String)}),
				},
			},
			wantErr: "timeout",
		},
		{
			name: "unsupported nested block",
			hclText: `module "bad" {
  source = "./m"
  worker "inner" {}
}`,
			wantErr: "unsupported",
		},
		{
			name: "unsupported dynamic block",
			hclText: `module "bad" {
  source = "./m"
  dynamic "worker" {
    for_each = ["a"]
    content {}
  }
}`,
			wantErr: "precondition",
		},
		{
			name:    "unsafe path label",
			hclText: `module "a/b" { source = "./m" }`,
			wantErr: "module",
		},
		{
			name:    "dotdot label",
			hclText: `module ".." { source = "./m" }`,
			wantErr: "module",
		},
		{
			name: "capsule input",
			hclText: `module "bad" {
  source = "./m"
  cap    = var.cap
}`,
			ctx: &hcl.EvalContext{
				Variables: map[string]cty.Value{
					"var": cty.ObjectVal(map[string]cty.Value{
						"cap": cty.CapsuleVal(cty.Capsule("cap", reflect.TypeOf(struct{}{})), &struct{}{}),
					}),
				},
			},
			wantErr: "capsule",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hb := parseSingleModuleHclBlock(t, tt.hclText)
			mb := &ModuleBlock{BaseBlock: golden.NewBaseBlock(nil, hb)}
			ctx := tt.ctx
			if ctx == nil {
				ctx = &hcl.EvalContext{}
			}
			err := mb.Decode(hb, ctx)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestModuleBlockExecuteErrors(t *testing.T) {
	hb := parseSingleModuleHclBlock(t, `module "audit" { source = "./m" }`)

	mbNoPlanner := &ModuleBlock{BaseBlock: golden.NewBaseBlock(nil, hb)}
	require.NoError(t, mbNoPlanner.Decode(hb, &hcl.EvalContext{}))
	require.ErrorContains(t, mbNoPlanner.ExecuteDuringPlan(), "ModulePlanner")

	cfgErr := &fakeModuleConfig{
		BaseConfig: golden.NewBasicConfig(t.TempDir(), "sofa", "sofa", nil, nil, context.Background()),
		planFn: func(ModulePlanRequest) (ModulePlan, error) {
			return ModulePlan{}, errors.New("boom")
		},
	}
	mbErr := &ModuleBlock{BaseBlock: golden.NewBaseBlock(cfgErr, hb)}
	require.NoError(t, mbErr.Decode(hb, &hcl.EvalContext{}))
	require.ErrorContains(t, mbErr.ExecuteDuringPlan(), "boom")
}

func TestPlanAndModulePlanCloneIsolation(t *testing.T) {
	p := 3
	d := 20 * time.Minute
	desc := "branch"
	orig := ModulePlan{
		Plan: Plan{
			Directory:  "/orig",
			BundlePath: "modules/audit",
			Outputs: map[string]Output{
				"out": {
					Name:        "out",
					Value:       cty.StringVal("v"),
					Description: &desc,
					Sensitive:   true,
					Expression:  "local.v",
					SourceURLs:  []string{"https://example.com/repo.git"},
				},
			},
			Modules: map[string]ModulePlan{
				"module.nested": {
					Plan: Plan{Directory: "/nested"},
					Name: "nested",
				},
			},
			DeferredValidations: map[string][]corespec.Condition{
				"branch": {{Expression: `var.branch != ""`, ErrorMessage: "required"}},
			},
		},
		Address:          "module.audit",
		Name:             "audit",
		CanonicalAddress: "module.audit",
		Source:           "./audit",
		Parallelism:      &p,
		Timeout:          &d,
		TimeoutRaw:       "20m",
		Inputs:           map[string]cty.Value{"branch": cty.UnknownVal(cty.String)},
		InputExprs:       map[string]string{"branch": "worker.a.work_branch"},
	}

	cloned := orig.Clone()
	*cloned.Parallelism = 99
	*cloned.Timeout = time.Hour
	*cloned.Outputs["out"].Description = "mutated"
	cloned.Outputs["out"].SourceURLs[0] = "mutated"
	cloned.DeferredValidations["branch"][0].ErrorMessage = "mutated"
	cloned.Inputs["extra"] = cty.StringVal("x")
	cloned.InputExprs["extra"] = `"x"`
	delete(cloned.Modules, "module.nested")

	require.Equal(t, 3, *orig.Parallelism)
	require.Equal(t, 20*time.Minute, *orig.Timeout)
	require.Equal(t, "branch", *orig.Outputs["out"].Description)
	require.Equal(t, "https://example.com/repo.git", orig.Outputs["out"].SourceURLs[0])
	require.Equal(t, "required", orig.DeferredValidations["branch"][0].ErrorMessage)
	require.NotContains(t, orig.Inputs, "extra")
	require.NotContains(t, orig.InputExprs, "extra")
	require.Contains(t, orig.Modules, "module.nested")
}

func parseSingleModuleHclBlock(t *testing.T, src string) *golden.HclBlock {
	t.Helper()
	sf, sDiags := hclsyntax.ParseConfig([]byte(src), "test.sofa.hcl", hcl.InitialPos)
	require.False(t, sDiags.HasErrors(), sDiags.Error())
	wf, wDiags := hclwrite.ParseConfig([]byte(src), "test.sofa.hcl", hcl.InitialPos)
	require.False(t, wDiags.HasErrors(), wDiags.Error())
	sb := sf.Body.(*hclsyntax.Body)
	require.Len(t, sb.Blocks, 1)
	b := sb.Blocks[0]
	wrapped := &hclsyntax.Block{
		Type:            b.Type,
		Labels:          append([]string{""}, b.Labels...),
		Body:            b.Body,
		TypeRange:       b.TypeRange,
		LabelRanges:     b.LabelRanges,
		OpenBraceRange:  b.OpenBraceRange,
		CloseBraceRange: b.CloseBraceRange,
	}
	return golden.NewHclBlock(wrapped, wf.Body().Blocks()[0], nil)
}
