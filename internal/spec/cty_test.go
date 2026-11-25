package spec

import (
	"reflect"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestMarkAndDetectSensitive(t *testing.T) {
	value := cty.ObjectVal(map[string]cty.Value{
		"nested": cty.ListVal([]cty.Value{
			cty.StringVal("secret"),
		}),
	})

	require.False(t, IsSensitive(value))

	marked := MarkSensitive(value)
	require.True(t, IsSensitive(marked))
	require.True(t, marked.HasMarkDeep(golden.SensitiveMark))
}

func TestIsSensitiveDetectsDeepMarks(t *testing.T) {
	value := cty.ObjectVal(map[string]cty.Value{
		"visible": cty.StringVal("safe"),
		"secret":  cty.StringVal("hidden").Mark(golden.SensitiveMark),
	})

	require.True(t, IsSensitive(value))
}

func TestIsSensitiveHandlesNilValue(t *testing.T) {
	require.False(t, IsSensitive(cty.NilVal))
}

func TestValidateType(t *testing.T) {
	tests := []struct {
		name    string
		value   cty.Type
		wantErr bool
	}{
		{name: "string", value: cty.String},
		{name: "number", value: cty.Number},
		{name: "bool", value: cty.Bool},
		{name: "list", value: cty.List(cty.String)},
		{name: "set", value: cty.Set(cty.Number)},
		{name: "map", value: cty.Map(cty.Bool)},
		{name: "tuple", value: cty.Tuple([]cty.Type{cty.String, cty.Number})},
		{
			name: "object",
			value: cty.Object(map[string]cty.Type{
				"checks": cty.Map(cty.String),
			}),
		},
		{name: "nil", value: cty.NilType, wantErr: true},
		{name: "dynamic", value: cty.DynamicPseudoType, wantErr: true},
		{name: "capsule", value: cty.Capsule("token", reflect.TypeOf("")), wantErr: true},
		{name: "nested dynamic", value: cty.List(cty.DynamicPseudoType), wantErr: true},
		{
			name: "nested capsule",
			value: cty.Object(map[string]cty.Type{
				"token": cty.Capsule("token", reflect.TypeOf("")),
			}),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateType(tt.value)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
