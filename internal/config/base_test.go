package config

import (
	"context"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
)

func TestNewBaseConfigUsesSofaDSLAndFunctions(t *testing.T) {
	cfg := NewBaseConfig(golden.NewBaseConfigArgs{
		Basedir:         t.TempDir(),
		Ctx:             context.Background(),
		DslFullName:     "sofa",
		DslAbbreviation: "sofa",
	})

	require.Equal(t, "sofa", cfg.DslFullName())
	require.Equal(t, "sofa", cfg.DslAbbreviation())

	ctx := cfg.EmptyEvalContext()
	require.NotNil(t, ctx)
	for _, name := range []string{
		"abspath",
		"cwd",
		"jsondecode",
		"jsondecodewithtype",
		"local_timestamp",
		"one",
	} {
		require.Contains(t, ctx.Functions, name)
	}
}
