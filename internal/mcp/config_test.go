package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMCPConfigValidate(t *testing.T) {
	base := Config{
		Name:      "search",
		Transport: TransportHTTP,
		Tools:     []string{"search"},
		Timeout:   30 * time.Second,
		HTTP:      &HTTPConfig{URL: "https://mcp.example.com/mcp"},
	}
	require.NoError(t, base.ValidateSelection())

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty selection", func(c *Config) { c.Tools, c.Resources = nil, nil }, "tool or resource"},
		{"duplicate tool", func(c *Config) { c.Tools = []string{"search", "search"} }, "more than once"},
		{"empty tool", func(c *Config) { c.Tools = []string{""} }, "empty"},
		{"timeout too long", func(c *Config) { c.Timeout = 10 * time.Minute }, "between 1s and 5m"},
		{"two transports", func(c *Config) { c.Stdio = &StdioConfig{Command: "server"} }, "exactly one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			require.ErrorContains(t, cfg.ValidateSelection(), tt.want)
		})
	}
}

func TestMCPHTTPAndStdioValidation(t *testing.T) {
	require.NoError(t, (HTTPConfig{URL: "https://mcp.example.com"}).Validate())
	require.ErrorContains(t, (HTTPConfig{URL: "ftp://mcp.example.com"}).Validate(), "http")
	require.ErrorContains(t, (HTTPConfig{URL: "https://example.com", Headers: map[string]string{"Authorization": "secret"}}).Validate(), "authorization")

	require.NoError(t, (StdioConfig{Command: "server"}).Validate())
	require.ErrorContains(t, (StdioConfig{}).Validate(), "command")
	require.ErrorContains(t, (StdioConfig{
		Command: "server",
		Env:     map[string]string{"TOKEN": "a"},
		EnvRefs: map[string]string{"TOKEN": "TOKEN_REF"},
	}).Validate(), "both")
}
