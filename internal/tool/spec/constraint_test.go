package spec_test

import (
	"reflect"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestJSONSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		typeValue cty.Type
		expected  map[string]any
	}{
		{name: "string", typeValue: cty.String, expected: map[string]any{"type": "string"}},
		{name: "number", typeValue: cty.Number, expected: map[string]any{"type": "number"}},
		{name: "bool", typeValue: cty.Bool, expected: map[string]any{"type": "boolean"}},
		{
			name:      "list",
			typeValue: cty.List(cty.String),
			expected:  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		{
			name:      "set",
			typeValue: cty.Set(cty.Number),
			expected: map[string]any{
				"type": "array", "items": map[string]any{"type": "number"}, "uniqueItems": true,
			},
		},
		{
			name:      "map",
			typeValue: cty.Map(cty.Bool),
			expected: map[string]any{
				"type": "object", "additionalProperties": map[string]any{"type": "boolean"},
			},
		},
		{
			name:      "tuple",
			typeValue: cty.Tuple([]cty.Type{cty.String, cty.Number}),
			expected: map[string]any{
				"type": "array",
				"prefixItems": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "number"},
				},
				"items": false, "minItems": 2, "maxItems": 2,
			},
		},
		{
			name: "object with optional attribute",
			typeValue: cty.ObjectWithOptionalAttrs(
				map[string]cty.Type{"query": cty.String, "limit": cty.Number},
				[]string{"limit"},
			),
			expected: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "number"},
					"query": map[string]any{"type": "string"},
				},
				"required": []string{"query"}, "additionalProperties": false,
			},
		},
		{
			name:      "empty object",
			typeValue: cty.EmptyObject,
			expected: map[string]any{
				"type": "object", "properties": map[string]any{}, "additionalProperties": false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			actual, err := toolspec.JSONSchema(tt.typeValue)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestJSONSchemaRejectsUnsupportedTypes(t *testing.T) {
	t.Parallel()

	capsule := cty.Capsule("reader", reflect.TypeFor[interface{ Read([]byte) (int, error) }]())
	tests := []struct {
		name          string
		typeValue     cty.Type
		expectedError string
	}{
		{name: "nil", typeValue: cty.NilType, expectedError: "type is required and cannot be null"},
		{name: "dynamic", typeValue: cty.DynamicPseudoType, expectedError: "dynamic type"},
		{name: "nested any", typeValue: cty.List(cty.DynamicPseudoType), expectedError: "dynamic type"},
		{name: "capsule", typeValue: capsule, expectedError: "capsule type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := toolspec.JSONSchema(tt.typeValue)
			require.ErrorContains(t, err, tt.expectedError)
		})
	}
}

func TestConstraintApply(t *testing.T) {
	t.Parallel()

	constraint := parseConstraintForTest(t, `object({
  query = string
  limit = optional(number, 20)
  note  = optional(string)
})`)

	actual, err := constraint.Apply(cty.ObjectVal(map[string]cty.Value{
		"query": cty.StringVal("energy"),
	}))
	require.NoError(t, err)
	assert.Equal(t, "energy", actual.GetAttr("query").AsString())
	assert.True(t, cty.NumberIntVal(20).RawEquals(actual.GetAttr("limit")))
	assert.True(t, actual.GetAttr("note").IsNull())

	nestedConstraint := parseConstraintForTest(t, `object({
  query = string
  filters = optional(object({
    limit = optional(number, 20)
  }), {})
})`)
	nested, err := nestedConstraint.Apply(cty.ObjectVal(map[string]cty.Value{
		"query": cty.StringVal("energy"),
	}))
	require.NoError(t, err)
	assert.True(t, cty.NumberIntVal(20).RawEquals(nested.GetAttr("filters").GetAttr("limit")))
}

func TestConstraintApplyRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		constraint    toolspec.Constraint
		value         cty.Value
		expectedError string
	}{
		{
			name:          "unknown",
			constraint:    toolspec.NewConstraint(cty.Object(map[string]cty.Type{"query": cty.String})),
			value:         cty.ObjectVal(map[string]cty.Value{"query": cty.UnknownVal(cty.String)}),
			expectedError: "value must be wholly known",
		},
		{
			name:          "unsupported dynamic constraint",
			constraint:    toolspec.NewConstraint(cty.DynamicPseudoType),
			value:         cty.StringVal("x"),
			expectedError: "dynamic type",
		},
		{
			name:          "type conversion mismatch",
			constraint:    toolspec.NewConstraint(cty.Object(map[string]cty.Type{"query": cty.String})),
			value:         cty.NumberIntVal(42),
			expectedError: "value does not match type constraint",
		},
		{
			name:       "required attribute is null",
			constraint: toolspec.NewConstraint(cty.Object(map[string]cty.Type{"query": cty.String})),
			value: cty.ObjectVal(map[string]cty.Value{
				"query": cty.NullVal(cty.String),
			}),
			expectedError: "null is not allowed at value.query",
		},
		{
			name:       "undeclared attribute",
			constraint: toolspec.NewConstraint(cty.Object(map[string]cty.Type{"query": cty.String})),
			value: cty.ObjectVal(map[string]cty.Value{
				"query": cty.StringVal("energy"),
				"extra": cty.True,
			}),
			expectedError: "undeclared attribute at value.extra",
		},
		{
			name:          "collection element is null",
			constraint:    toolspec.NewConstraint(cty.List(cty.String)),
			value:         cty.TupleVal([]cty.Value{cty.NullVal(cty.String)}),
			expectedError: "null is not allowed at value[0]",
		},
		{
			name:          "top-level null",
			constraint:    toolspec.NewConstraint(cty.String),
			value:         cty.NullVal(cty.String),
			expectedError: "null is not allowed at value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tt.constraint.Apply(tt.value)
			require.ErrorContains(t, err, tt.expectedError)
		})
	}
}

func TestConstraintJSONSchemaMethod(t *testing.T) {
	t.Parallel()

	constraint := toolspec.NewConstraint(cty.String)
	actual, err := constraint.JSONSchema()
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"type": "string"}, actual)
}

func parseConstraintForTest(t *testing.T, source string) toolspec.Constraint {
	t.Helper()

	expression, diagnostics := hclsyntax.ParseExpression([]byte(source), "type.sofa.hcl", hcl.InitialPos)
	require.False(t, diagnostics.HasErrors(), diagnostics.Error())
	typeValue, defaults, diagnostics := typeexpr.TypeConstraintWithDefaults(expression)
	require.False(t, diagnostics.HasErrors(), diagnostics.Error())
	return toolspec.NewConstraintWithDefaults(typeValue, defaults)
}
