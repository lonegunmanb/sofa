package spec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandboxConfigValidate(t *testing.T) {
	valid := SandboxConfig{
		Runtime: "podman",
		Image:   "ghcr.io/example/go:1.27",
		Network: "none",
		Mounts: []MountSpec{{
			Source:   "/cache",
			Target:   "/go/pkg/mod",
			ReadOnly: true,
		}},
		Resources: &ResourcesSpec{CPUs: "0.5", Memory: "2g"},
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*SandboxConfig)
		want   string
	}{
		{"runtime", func(c *SandboxConfig) { c.Runtime = "qemu" }, "runtime"},
		{"image whitespace", func(c *SandboxConfig) { c.Image = "bad image" }, "whitespace"},
		{"network", func(c *SandboxConfig) { c.Network = "private" }, "network"},
		{"relative target", func(c *SandboxConfig) { c.Mounts[0].Target = "cache" }, "absolute"},
		{"root target", func(c *SandboxConfig) { c.Mounts[0].Target = "/" }, "root"},
		{"workspace target", func(c *SandboxConfig) { c.Mounts[0].Target = "/workspace/cache" }, "reserved"},
		{"sofa target", func(c *SandboxConfig) { c.Mounts[0].Target = "/.sofa/cache" }, "reserved"},
		{
			"duplicate target",
			func(c *SandboxConfig) {
				c.Mounts = append(c.Mounts, MountSpec{Source: "/other", Target: "/go/pkg/mod/", ReadOnly: true})
			},
			"duplicate",
		},
		{"mutable mount", func(c *SandboxConfig) { c.Mounts[0].ReadOnly = false }, "read_only"},
		{"bad cpu", func(c *SandboxConfig) { c.Resources.CPUs = "0" }, "cpus"},
		{"infinite cpu", func(c *SandboxConfig) { c.Resources.CPUs = "+Inf" }, "cpus"},
		{"bad memory", func(c *SandboxConfig) { c.Resources.Memory = "2x" }, "memory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cloneSandboxConfig(valid)
			tt.mutate(&cfg)
			require.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}
