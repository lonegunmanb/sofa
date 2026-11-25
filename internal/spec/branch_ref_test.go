package spec

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestNewBranchRefAcceptsKnownAndUnknownStrings(t *testing.T) {
	known, err := NewBranchRef(cty.StringVal("release/v1"), `"release/v1"`)
	require.NoError(t, err)
	require.Equal(t, "release/v1", known.Value.AsString())
	require.Equal(t, `"release/v1"`, known.Expression)

	unknown, err := NewBranchRef(cty.UnknownVal(cty.String), "worker.implement.work_branch")
	require.NoError(t, err)
	require.False(t, unknown.Value.IsKnown())
	require.True(t, unknown.Value.Type().Equals(cty.String))
	require.Equal(t, "worker.implement.work_branch", unknown.Expression)
}

func TestNewBranchRefRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		value cty.Value
		want  string
	}{
		{name: "null", value: cty.NullVal(cty.String), want: "must not be null"},
		{name: "number", value: cty.NumberIntVal(1), want: "must be a string"},
		{name: "unknown number", value: cty.UnknownVal(cty.Number), want: "must be a string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewBranchRef(tt.value, "value")
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestValidateBranchName(t *testing.T) {
	valid := []string{
		"main",
		"release/v1",
		"feature/source-preserve",
		"a.b",
		"a_b",
	}
	for _, branch := range valid {
		require.NoError(t, ValidateBranchName(branch), branch)
	}

	invalid := []string{
		"",
		"-main",
		"/main",
		"main/",
		"main..next",
		"main next",
		"main~1",
		"main^",
		"main:child",
		"main@{1}",
		"main\tnext",
	}
	for _, branch := range invalid {
		require.Error(t, ValidateBranchName(branch), branch)
	}
}

func TestNewBranchRefValidatesKnownBranchName(t *testing.T) {
	_, err := NewBranchRef(cty.StringVal("bad..branch"), `"bad..branch"`)
	require.ErrorContains(t, err, "branch")
}
