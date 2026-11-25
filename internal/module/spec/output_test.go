package spec

import (
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestOutputBlockGoldenContract(t *testing.T) {
	block := newTestOutputBlock("sample")
	require.Equal(t, "", block.Type())
	require.Equal(t, "output", block.BlockType())
	require.Equal(t, 2, block.AddressLength())
	require.False(t, block.CanExecutePrePlan())
	_, isPlan := any(block).(golden.PlanBlock)
	_, isSingleValue := any(block).(golden.SingleValueBlock)
	require.True(t, isPlan)
	require.True(t, isSingleValue)
}

func TestOutputBlockPublishesKnownValue(t *testing.T) {
	description := "generated path"
	block := newTestOutputBlock("artifact_path")
	block.Expression = cty.StringVal(".sofa/report.json")
	block.Description = &description

	require.NoError(t, block.ExecuteDuringPlan())
	snapshot := block.Output()
	require.Equal(t, "artifact_path", snapshot.Name)
	require.Equal(t, ".sofa/report.json", snapshot.Value.AsString())
	require.Equal(t, description, *snapshot.Description)
	require.False(t, snapshot.Sensitive)
	require.Equal(t, ".sofa/report.json", golden.CtyValueToString(block.Value()))
}

func TestOutputBlockMarksSensitiveValue(t *testing.T) {
	block := newTestOutputBlock("secret")
	block.Expression = cty.ListVal([]cty.Value{
		cty.StringVal("hidden-value"),
	})
	block.Sensitive = true

	require.NoError(t, block.ExecuteDuringPlan())
	require.True(t, block.Value().HasMarkDeep(golden.SensitiveMark))
	require.Equal(t, "<sensitive>", golden.CtyValueToString(block.Value()))
	require.True(t, block.Output().Sensitive)
}

func TestOutputBlockPreservesUnknownAndNull(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		block := newTestOutputBlock("unknown")
		block.Expression = cty.UnknownVal(cty.Map(cty.String))

		require.NoError(t, block.ExecuteDuringPlan())
		require.False(t, block.Value().IsKnown())
		require.True(t, block.Value().Type().Equals(cty.Map(cty.String)))
	})

	t.Run("null", func(t *testing.T) {
		block := newTestOutputBlock("null_value")
		block.Expression = cty.NullVal(cty.String)

		require.NoError(t, block.ExecuteDuringPlan())
		require.True(t, block.Value().IsNull())
	})
}

func TestOutputBlockValueBeforePlanIsDynamic(t *testing.T) {
	block := newTestOutputBlock("pending")
	require.True(t, block.Value().RawEquals(cty.DynamicVal))
}

func TestOutputSnapshotReportsDerivedSensitivity(t *testing.T) {
	block := newTestOutputBlock("derived_secret")
	block.Expression = cty.StringVal("hidden").Mark(golden.SensitiveMark)

	require.NoError(t, block.ExecuteDuringPlan())
	snapshot := block.Output()
	require.False(t, block.Sensitive)
	require.True(t, snapshot.Sensitive)
	require.Equal(t, "<sensitive>", golden.CtyValueToString(snapshot.Value))
}

func newTestOutputBlock(name string) *OutputBlock {
	hclBlock := &golden.HclBlock{
		Block: &hclsyntax.Block{
			Type:   "output",
			Labels: []string{"", name},
		},
	}
	return &OutputBlock{
		BaseBlock: golden.NewBaseBlock(nil, hclBlock),
	}
}
