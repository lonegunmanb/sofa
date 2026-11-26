package planner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/golden"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestPlannerTypedToolsDeterministicIDsAndToolName(t *testing.T) {
	const hclSource = `
go_tool "submit_checks" {
  description  = "校验并提交受影响的邮件类型映射"
  dependencies = ["github.com/google/uuid@v1.6.0"]
  source = <<-GO
    import (
      "context"
      "github.com/google/uuid"
    )

    type Input struct {
      Checks map[string]string ` + "`json:\"checks\"`" + `
    }
    type Output string

    func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
      out := Output(uuid.NewString())
      return ToolResponse[Output]{Accepted: true, Output: &out}, nil
    }
  GO

  postcondition {
    condition     = length(input.checks) >= 0
    error_message = "checks must be valid"
  }
}

external_tool "search_catalog_with_an_extremely_long_name_that_must_be_capped_at_sixty_four_chars" {
  description = "Search the catalog"
  program     = ["nonexistent-container-cli", "--json"]
  input_type = object({
    limit = optional(number, 10)
    tags  = list(string)
  })
  output_type = object({
    matches = list(string)
  })
}

output "go_id" {
  value = go_tool.submit_checks.id
}

output "go_tool_name" {
  value = tool_name(go_tool.submit_checks)
}

output "external_tool_name" {
  value = tool_name(external_tool.search_catalog_with_an_extremely_long_name_that_must_be_capped_at_sixty_four_chars)
}
`

	dir1 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir1, "workflow.sofa.hcl"), hclSource)
	cfg1 := newTestPlanner(t, dir1, nil)
	initPlanner(t, cfg1, dir1)
	require.NoError(t, cfg1.RunPlan())

	dir2 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir2, "workflow.sofa.hcl"), hclSource)
	cfg2 := newTestPlanner(t, dir2, nil)
	initPlanner(t, cfg2, dir2)
	require.NoError(t, cfg2.RunPlan())

	outputs1 := cfg1.Outputs()
	outputs2 := cfg2.Outputs()
	require.Len(t, outputs1, 3)
	require.Len(t, outputs2, 3)

	// Outputs are sorted by name: external_tool_name, go_id, go_tool_name
	extName := outputs1[0].Value.AsString()
	goID := outputs1[1].Value.AsString()
	goName := outputs1[2].Value.AsString()

	require.True(t, strings.HasPrefix(goID, "tool_go_tool_submit_checks_"))
	require.Equal(t, goID, goName)
	require.LessOrEqual(t, len(goID), 64)
	require.Equal(t, goID, outputs2[1].Value.AsString())

	require.True(t, strings.HasPrefix(extName, "tool_external_tool_"))
	require.LessOrEqual(t, len(extName), 64)
	require.Equal(t, extName, outputs2[0].Value.AsString())

	extBlock := golden.Blocks[*toolspec.ExternalToolBlock](cfg1)[0]
	schema, err := extBlock.InputConstraint().JSONSchema()
	require.NoError(t, err)
	require.Equal(t, []string{"tags"}, schema["required"])
}

func TestPlannerTypedToolsRejectInvalidContracts(t *testing.T) {
	tests := []struct {
		name    string
		hcl     string
		wantErr string
	}{
		{
			name: "go_tool invalid dependency",
			hcl: `
go_tool "bad" {
  description  = "Bad"
  dependencies = ["-modfile=x"]
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}
`,
			wantErr: "invalid dependency",
		},
		{
			name: "go_tool non-struct Input without postcondition",
			hcl: `
go_tool "bad" {
  description = "Bad"
  source = <<-GO
    import "context"
    type Input string
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}
`,
			wantErr: "Input must be a struct",
		},
		{
			name: "external_tool non-object input_type",
			hcl: `
external_tool "bad" {
  description = "Bad"
  program     = ["run"]
  input_type  = string
  output_type = string
}
`,
			wantErr: "external tool input_type must be an object type",
		},
		{
			name: "postcondition typo",
			hcl: `
external_tool "bad" {
  description = "Bad"
  program     = ["run"]
  input_type  = object({ query = string })
  output_type = string
  postcondition {
    condition     = input.typo == ""
    error_message = "bad"
  }
}
`,
			wantErr: "Unsupported attribute",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.hcl)
			cfg := newTestPlanner(t, dir, nil)
			initPlanner(t, cfg, dir)
			err := cfg.RunPlan()
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPlannerToolNameSucceedsWithSensitiveToolAttributes(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "secret_desc" {
  type      = string
  default   = "sensitive description"
  sensitive = true
}

variable "secret_arg" {
  type      = string
  default   = "--secret-flag"
  sensitive = true
}

go_tool "sensitive_go" {
  description = var.secret_desc
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

external_tool "sensitive_ext" {
  description = var.secret_desc
  program     = ["cli", var.secret_arg]
  input_type  = object({ q = string })
  output_type = string
}

output "go_name" {
  value = tool_name(go_tool.sensitive_go)
}

output "ext_name" {
  value = tool_name(external_tool.sensitive_ext)
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	outputs := cfg.Outputs()
	require.Len(t, outputs, 2)
	require.Contains(t, outputs[0].Value.AsString(), "tool_external_tool_sensitiv_")
	require.Contains(t, outputs[1].Value.AsString(), "tool_go_tool_sensitive_go_")
	require.True(t, cfg.EvalContext().Variables["go_tool"].GetAttr("sensitive_go").GetAttr("description").HasMark(golden.SensitiveMark))
	require.True(t, cfg.EvalContext().Variables["external_tool"].GetAttr("sensitive_ext").GetAttr("program").Index(cty.NumberIntVal(1)).HasMark(golden.SensitiveMark))
}
