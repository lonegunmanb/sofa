package spec

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
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

	for _, network := range []string{"bridge", "none", "host"} {
		cfg := cloneSandboxConfig(valid)
		cfg.Network = network
		require.NoError(t, cfg.Validate())
	}

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

func TestSandboxBlockDefaultsNetworkToBridge(t *testing.T) {
	block := &SandboxBlock{
		Runtime: "podman",
		Image:   "ghcr.io/example/go:1.27",
	}
	require.Equal(t, "bridge", block.SandboxConfig().Network)
}

func TestSandboxBlockRejectsEntrypointViaGoldenDecode(t *testing.T) {
	for _, src := range []string{
		`sandbox "go_env" {
  runtime    = "podman"
  image      = "ghcr.io/example/go:1.27"
  entrypoint = ["/bin/sh"]
}`,
		`sandbox "go_env" {
  runtime    = "podman"
  image      = "ghcr.io/example/go:1.27"
  entrypoint = null
}`,
	} {
		sf, sDiags := hclsyntax.ParseConfig([]byte(src), "sandbox.sofa.hcl", hcl.InitialPos)
		require.False(t, sDiags.HasErrors(), sDiags.Error())
		wf, wDiags := hclwrite.ParseConfig([]byte(src), "sandbox.sofa.hcl", hcl.InitialPos)
		require.False(t, wDiags.HasErrors(), wDiags.Error())

		sb := sf.Body.(*hclsyntax.Body).Blocks[0]
		wrapped := &hclsyntax.Block{
			Type:            sb.Type,
			Labels:          append([]string{""}, sb.Labels...),
			Body:            sb.Body,
			TypeRange:       sb.TypeRange,
			LabelRanges:     sb.LabelRanges,
			OpenBraceRange:  sb.OpenBraceRange,
			CloseBraceRange: sb.CloseBraceRange,
		}
		hb := golden.NewHclBlock(wrapped, wf.Body().Blocks()[0], nil)
		golden.RegisterBlock(&SandboxBlock{})
		base := golden.NewBasicConfig(t.TempDir(), "sofa", "sofa", nil, nil, nil)
		block := &SandboxBlock{BaseBlock: golden.NewBaseBlock(base, hb)}
		err := golden.Decode(block)
		require.Error(t, err)
		require.Contains(t, err.Error(), "entrypoint")
	}
}
