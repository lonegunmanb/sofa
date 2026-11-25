package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestCWDFunctionUsesForwardSlashes(t *testing.T) {
	expected, err := os.Getwd()
	require.NoError(t, err)

	got, err := cwdFunction.Call(nil)
	require.NoError(t, err)
	require.Equal(t, filepath.ToSlash(expected), got.AsString())
}

func TestOneFunction(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := oneFunction.Call([]cty.Value{cty.ListValEmpty(cty.String)})
		require.NoError(t, err)
		require.True(t, got.IsNull())
		require.True(t, got.Type().Equals(cty.String))
	})

	t.Run("singleton", func(t *testing.T) {
		got, err := oneFunction.Call([]cty.Value{
			cty.ListVal([]cty.Value{cty.StringVal("only")}),
		})
		require.NoError(t, err)
		require.Equal(t, "only", got.AsString())
	})

	t.Run("multiple", func(t *testing.T) {
		_, err := oneFunction.Call([]cty.Value{
			cty.ListVal([]cty.Value{cty.StringVal("first"), cty.StringVal("second")}),
		})
		require.ErrorContains(t, err, "at most one")
	})

	t.Run("null", func(t *testing.T) {
		_, err := oneFunction.Call([]cty.Value{
			cty.NullVal(cty.List(cty.String)),
		})
		require.ErrorContains(t, err, "null collection")
	})

	t.Run("non collection", func(t *testing.T) {
		_, err := oneFunction.Call([]cty.Value{cty.NumberIntVal(1)})
		require.ErrorContains(t, err, "collection is required")
	})

	t.Run("unknown", func(t *testing.T) {
		got, err := oneFunction.Call([]cty.Value{
			cty.UnknownVal(cty.List(cty.String)),
		})
		require.NoError(t, err)
		require.False(t, got.IsKnown())
		require.True(t, got.Type().Equals(cty.String))
	})

	t.Run("sensitive collection", func(t *testing.T) {
		input := cty.ListVal([]cty.Value{cty.StringVal("hidden")}).Mark(golden.SensitiveMark)
		got, err := oneFunction.Call([]cty.Value{input})
		require.NoError(t, err)
		require.True(t, got.HasMarkDeep(golden.SensitiveMark))
		require.Equal(t, "<sensitive>", golden.CtyValueToString(got))
	})

	t.Run("sensitive element", func(t *testing.T) {
		input := cty.TupleVal([]cty.Value{
			cty.StringVal("hidden").Mark(golden.SensitiveMark),
		})
		got, err := oneFunction.Call([]cty.Value{input})
		require.NoError(t, err)
		require.True(t, got.HasMarkDeep(golden.SensitiveMark))
		require.Equal(t, "<sensitive>", golden.CtyValueToString(got))
	})
}

func TestLocalTimestampFunction(t *testing.T) {
	got, err := localTimestampFunction.Call(nil)
	require.NoError(t, err)
	parsed, err := time.Parse(time.RFC3339Nano, got.AsString())
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), parsed, 10*time.Second)
}

func TestJSONDecodeWithTypeFunction(t *testing.T) {
	targetType := cty.Object(map[string]cty.Type{
		"checks": cty.Map(cty.String),
	})

	t.Run("known exact type", func(t *testing.T) {
		got, err := jsonDecodeWithTypeFunction.Call([]cty.Value{
			cty.StringVal(`{"checks":{"a":"b"}}`),
			typeexpr.TypeConstraintVal(targetType),
		})
		require.NoError(t, err)
		require.True(t, got.Type().Equals(targetType))
		require.Equal(t, "b", got.GetAttr("checks").Index(cty.StringVal("a")).AsString())
	})

	t.Run("unknown preserves declared type", func(t *testing.T) {
		got, err := jsonDecodeWithTypeFunction.Call([]cty.Value{
			cty.UnknownVal(cty.String),
			typeexpr.TypeConstraintVal(targetType),
		})
		require.NoError(t, err)
		require.False(t, got.IsKnown())
		require.True(t, got.Type().Equals(targetType))
	})

	t.Run("invalid json", func(t *testing.T) {
		_, err := jsonDecodeWithTypeFunction.Call([]cty.Value{
			cty.StringVal("{"),
			typeexpr.TypeConstraintVal(targetType),
		})
		require.Error(t, err)
	})
}

func TestFunctionsPreserveUpstreamFunctionsAndOverrideHelpers(t *testing.T) {
	functions := Functions(t.TempDir())
	for _, name := range []string{
		"abspath",
		"jsondecode",
		"cwd",
		"local_timestamp",
		"one",
		"jsondecodewithtype",
	} {
		require.Contains(t, functions, name)
	}
}
