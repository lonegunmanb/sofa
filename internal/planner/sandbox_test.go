package planner

import (
	"path/filepath"
	"testing"

	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/stretchr/testify/require"
)

func TestSandboxPlansWithSafeDefaults(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"

  mount {
    source = "/cache"
    target = "/go/pkg/mod"
  }

  resources {
    cpus   = "0.5"
    memory = "2g"
  }
}

output "network" {
  value = sandbox.go_env.network
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())
	require.Equal(t, "none", cfg.Outputs()[0].Value.AsString())
}

func TestSandboxRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "mutable mount",
			body: `sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
  mount {
    source    = "/cache"
    target    = "/go/pkg/mod"
    read_only = false
  }
}`,
			want: "read_only",
		},
		{
			name: "reserved target",
			body: `sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
  mount {
    source = "/cache"
    target = "/workspace/cache"
  }
}`,
			want: "reserved",
		},
		{
			name: "multiple resources",
			body: `sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
  resources {}
  resources {}
}`,
			want: "at most one",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.body)
			cfg := newTestPlanner(t, dir, nil)
			initPlanner(t, cfg, dir)
			require.ErrorContains(t, cfg.RunPlan(), tt.want)
		})
	}
}

func TestSandboxAllowsNullEntrypoint(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime    = "podman"
  image      = "ghcr.io/example/go:1.27"
  entrypoint = null
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())
}

func TestSandboxRejectsExplicitEmptyEntrypoint(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime    = "podman"
  image      = "ghcr.io/example/go:1.27"
  entrypoint = []
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.ErrorContains(t, cfg.RunPlan(), "empty list")
}

func TestSandboxConfigReturnsDeepCopy(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
  mount {
    source = "/cache"
    target = "/go/pkg/mod"
  }
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	var block *sandboxspec.SandboxBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*sandboxspec.SandboxBlock); ok {
			block = candidate
		}
	}
	require.NotNil(t, block)
	snapshot := block.SandboxConfig()
	snapshot.Mounts[0].Target = "/mutated"
	require.Equal(t, "/go/pkg/mod", block.SandboxConfig().Mounts[0].Target)
}
