package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lonegunmanb/sofa/internal/cli"
	"github.com/stretchr/testify/assert"
)

func TestMainRunHelpAndInterrupt(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--help"}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, cli.ExitSuccess, code)
	assert.Contains(t, stdout.String(), "sofa")
	assert.Contains(t, stdout.String(), "init")
	assert.Contains(t, stdout.String(), "schema")
	assert.Contains(t, stdout.String(), "plan")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"init"}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, 130, code)
}
