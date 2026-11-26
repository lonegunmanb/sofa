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

func TestToolNameFunction(t *testing.T) {
	functions := Functions(t.TempDir())
	toolNameFn := functions["tool_name"]

	t.Run("go tool returns deterministic id", func(t *testing.T) {
		got, err := toolNameFn.Call([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"id":      cty.StringVal("tool_go_tool_finish_12345678-1234-8234-8234-1234567890ab"),
				"address": cty.StringVal("go_tool.finish"),
				"kind":    cty.StringVal("go"),
			}),
		})
		require.NoError(t, err)
		require.Equal(t, "tool_go_tool_finish_12345678-1234-8234-8234-1234567890ab", got.AsString())
	})

	t.Run("external tool returns deterministic id", func(t *testing.T) {
		got, err := toolNameFn.Call([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"id":      cty.StringVal("tool_external_tool_lookup_12345678-1234-8234-8234-1234567890ab"),
				"address": cty.StringVal("external_tool.lookup"),
				"kind":    cty.StringVal("external"),
			}),
		})
		require.NoError(t, err)
		require.Equal(t, "tool_external_tool_lookup_12345678-1234-8234-8234-1234567890ab", got.AsString())
	})

	t.Run("builtin without id normalizes address", func(t *testing.T) {
		got, err := toolNameFn.Call([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("builtin.web.search"),
				"kind":    cty.StringVal("builtin"),
			}),
		})
		require.NoError(t, err)
		require.Equal(t, "builtin_web_search", got.AsString())
	})

	t.Run("preserves outer mark", func(t *testing.T) {
		input := cty.ObjectVal(map[string]cty.Value{
			"id":      cty.StringVal("tool_go_tool_finish_12345678-1234-8234-8234-1234567890ab"),
			"address": cty.StringVal("go_tool.finish"),
			"kind":    cty.StringVal("go"),
		}).Mark(golden.SensitiveMark)
		got, err := toolNameFn.Call([]cty.Value{input})
		require.NoError(t, err)
		require.True(t, got.HasMark(golden.SensitiveMark))
	})

	t.Run("succeeds when sibling attributes are sensitive", func(t *testing.T) {
		got, err := toolNameFn.Call([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"id":          cty.StringVal("tool_external_tool_lookup_12345678-1234-8234-8234-1234567890ab"),
				"address":     cty.StringVal("external_tool.lookup"),
				"kind":        cty.StringVal("external"),
				"description": cty.StringVal("secret").Mark(golden.SensitiveMark),
				"program":     cty.ListVal([]cty.Value{cty.StringVal("cli").Mark(golden.SensitiveMark)}),
			}),
		})
		require.NoError(t, err)
		require.Equal(t, "tool_external_tool_lookup_12345678-1234-8234-8234-1234567890ab", got.AsString())
		require.False(t, got.HasMark(golden.SensitiveMark))
	})

	t.Run("propagates marks from address kind or id", func(t *testing.T) {
		got, err := toolNameFn.Call([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"id":      cty.StringVal("tool_go_tool_finish_12345678-1234-8234-8234-1234567890ab").Mark(golden.SensitiveMark),
				"address": cty.StringVal("go_tool.finish"),
				"kind":    cty.StringVal("go"),
			}),
		})
		require.NoError(t, err)
		require.True(t, got.HasMark(golden.SensitiveMark))
		unmarked, _ := got.Unmark()
		require.Equal(t, "tool_go_tool_finish_12345678-1234-8234-8234-1234567890ab", unmarked.AsString())
	})

	t.Run("accepts module prefixed and for_each addresses", func(t *testing.T) {
		for _, tc := range []struct {
			kind    string
			address string
			want    string
		}{
			{kind: "go", address: "module.sub.go_tool.finish", want: "module_sub_go_tool_finish"},
			{kind: "external", address: "module.a.module.b.external_tool.lookup", want: "module_a_module_b_external_tool_lookup"},
			{kind: "go", address: "go_tool.multi_go[a]", want: "go_tool_multi_go[a]"},
			{kind: "external", address: "external_tool.multi_ext[a.b]", want: "external_tool_multi_ext[a_b]"},
			{kind: "go", address: "module.sub.go_tool.multi_go[a.b]", want: "module_sub_go_tool_multi_go[a_b]"},
		} {
			got, err := toolNameFn.Call([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{
					"address": cty.StringVal(tc.address),
					"kind":    cty.StringVal(tc.kind),
				}),
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, got.AsString())
		}
	})

	t.Run("returns unknown string for dynamic or unknown tool object", func(t *testing.T) {
		got, err := toolNameFn.Call([]cty.Value{cty.DynamicVal.Mark(golden.SensitiveMark)})
		require.NoError(t, err)
		require.False(t, got.IsKnown())
		require.True(t, got.Type().Equals(cty.String))
		require.True(t, got.HasMark(golden.SensitiveMark))

		gotObj, err := toolNameFn.Call([]cty.Value{
			cty.UnknownVal(cty.Object(map[string]cty.Type{"address": cty.String, "kind": cty.String})),
		})
		require.NoError(t, err)
		require.False(t, gotObj.IsKnown())
		require.True(t, gotObj.Type().Equals(cty.String))
	})

	t.Run("rejects invalid inputs", func(t *testing.T) {
		invalid := []cty.Value{
			cty.StringVal("go_tool.finish"),
			cty.UnknownVal(cty.String),
			cty.NullVal(cty.DynamicPseudoType),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("go_tool.finish"),
				"kind":    cty.StringVal(""),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("go_tool."),
				"kind":    cty.StringVal("go"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("module..go_tool.finish"),
				"kind":    cty.StringVal("go"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("go_tool..bad"),
				"kind":    cty.StringVal("go"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("go_tool.a.b"),
				"kind":    cty.StringVal("go"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("module.sub.external_tool.a.b"),
				"kind":    cty.StringVal("external"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("go_tool.[a]"),
				"kind":    cty.StringVal("go"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("go_tool.multi[]"),
				"kind":    cty.StringVal("go"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"address": cty.StringVal("mcp_server.search"),
				"kind":    cty.StringVal("mcp_server"),
			}),
		}
		for _, candidate := range invalid {
			_, err := toolNameFn.Call([]cty.Value{candidate})
			require.ErrorContains(t, err, "tool_name requires a typed tool address")
		}
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
		"tool_name",
	} {
		require.Contains(t, functions, name)
	}
}
