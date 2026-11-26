package spec_test

import (
	"testing"

	"github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func TestConditionEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		condition     spec.Condition
		variables     map[string]cty.Value
		functions     map[string]function.Function
		expectedKnown bool
		expectedError string
	}{
		{
			name: "known pass",
			condition: spec.Condition{
				Expression:   `input.name != ""`,
				ErrorMessage: "name is required",
			},
			variables: map[string]cty.Value{
				"input": cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("sofa")}),
			},
			expectedKnown: true,
		},
		{
			name: "known failure returns error_message",
			condition: spec.Condition{
				Expression:   `input.name != ""`,
				ErrorMessage: "name is required",
			},
			variables: map[string]cty.Value{
				"input": cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("")}),
			},
			expectedKnown: true,
			expectedError: "name is required",
		},
		{
			name: "unknown defers evaluation",
			condition: spec.Condition{
				Expression:   `input.name != "" && output.ok`,
				ErrorMessage: "must succeed",
			},
			variables: map[string]cty.Value{
				"input":  cty.UnknownVal(cty.Object(map[string]cty.Type{"name": cty.String})),
				"output": cty.UnknownVal(cty.Object(map[string]cty.Type{"ok": cty.Bool})),
			},
			expectedKnown: false,
		},
		{
			name: "sensitive marked bool preserves evaluation",
			condition: spec.Condition{
				Expression:   `input.secret != ""`,
				ErrorMessage: "secret required",
			},
			variables: map[string]cty.Value{
				"input": cty.ObjectVal(map[string]cty.Value{
					"secret": spec.MarkSensitive(cty.StringVal("token")),
				}),
			},
			expectedKnown: true,
		},
		{
			name: "undeclared variable root rejected",
			condition: spec.Condition{
				Expression:   `var.enabled`,
				ErrorMessage: "must be enabled",
			},
			variables: map[string]cty.Value{
				"input":  cty.EmptyObjectVal,
				"output": cty.EmptyObjectVal,
			},
			expectedError: `condition may only reference input, output; found "var"`,
		},
		{
			name: "non-bool result rejected",
			condition: spec.Condition{
				Expression:   `input.query`,
				ErrorMessage: "must be bool",
			},
			variables: map[string]cty.Value{
				"input": cty.UnknownVal(cty.Object(map[string]cty.Type{"query": cty.String})),
			},
			expectedError: "condition must return bool",
		},
		{
			name: "null bool result rejected",
			condition: spec.Condition{
				Expression:   `input.flag`,
				ErrorMessage: "must not be null",
			},
			variables: map[string]cty.Value{
				"input": cty.ObjectVal(map[string]cty.Value{"flag": cty.NullVal(cty.Bool)}),
			},
			expectedError: "condition must not return null",
		},
		{
			name: "blank expression rejected",
			condition: spec.Condition{
				Expression:   "   ",
				ErrorMessage: "err",
			},
			variables:     map[string]cty.Value{"input": cty.EmptyObjectVal},
			expectedError: "condition expression is required",
		},
		{
			name: "blank error_message rejected",
			condition: spec.Condition{
				Expression:   "true",
				ErrorMessage: " ",
			},
			variables:     map[string]cty.Value{"input": cty.EmptyObjectVal},
			expectedError: "condition error_message is required",
		},
		{
			name: "invalid HCL expression rejected",
			condition: spec.Condition{
				Expression:   "input.name ==",
				ErrorMessage: "err",
			},
			variables:     map[string]cty.Value{"input": cty.EmptyObjectVal},
			expectedError: "parse condition",
		},
		{
			name: "unknown attribute on typed object rejected",
			condition: spec.Condition{
				Expression:   `input.typo == ""`,
				ErrorMessage: "err",
			},
			variables: map[string]cty.Value{
				"input": cty.UnknownVal(cty.Object(map[string]cty.Type{"query": cty.String})),
			},
			expectedError: "evaluate condition",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			known, err := tt.condition.Evaluate(tt.variables, tt.functions)
			assert.Equal(t, tt.expectedKnown, known)
			if tt.expectedError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.expectedError)
		})
	}
}
