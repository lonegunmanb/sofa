package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	"github.com/lonegunmanb/golden"
	configactor "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

// memoryStateManager is a pure in-memory actor.StateManagerContext for Tier 1 unit tests.
type memoryStateManager struct {
	mu        sync.Mutex
	persisted map[string][]byte
	staged    map[string]stagedEntry
}

type stagedEntry struct {
	remove bool
	data   []byte
}

func newMemoryStateManager() *memoryStateManager {
	return &memoryStateManager{
		persisted: make(map[string][]byte),
		staged:    make(map[string]stagedEntry),
	}
}

func (m *memoryStateManager) Add(_ context.Context, stateName string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.staged[stateName]; ok {
		return fmt.Errorf("duplicate staged key %q", stateName)
	}
	if _, ok := m.persisted[stateName]; ok {
		return fmt.Errorf("duplicate persisted key %q", stateName)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.staged[stateName] = stagedEntry{data: raw}
	return nil
}

func (m *memoryStateManager) Get(_ context.Context, stateName string, reply any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.staged[stateName]; ok {
		if entry.remove {
			return fmt.Errorf("state %q marked for removal", stateName)
		}
		return json.Unmarshal(entry.data, reply)
	}
	raw, ok := m.persisted[stateName]
	if !ok {
		return fmt.Errorf("state %q not found", stateName)
	}
	return json.Unmarshal(raw, reply)
}

func (m *memoryStateManager) Set(_ context.Context, stateName string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.staged[stateName] = stagedEntry{data: raw}
	return nil
}

func (m *memoryStateManager) SetWithTTL(ctx context.Context, stateName string, value any, _ time.Duration) error {
	return m.Set(ctx, stateName, value)
}

func (m *memoryStateManager) Remove(_ context.Context, stateName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staged[stateName] = stagedEntry{remove: true}
	return nil
}

func (m *memoryStateManager) Contains(_ context.Context, stateName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.staged[stateName]; ok {
		return !entry.remove, nil
	}
	_, ok := m.persisted[stateName]
	return ok, nil
}

func (m *memoryStateManager) Save(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, entry := range m.staged {
		if entry.remove {
			delete(m.persisted, k)
		} else {
			m.persisted[k] = append([]byte(nil), entry.data...)
		}
	}
	m.staged = make(map[string]stagedEntry)
	return nil
}

func (m *memoryStateManager) Flush(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staged = make(map[string]stagedEntry)
}

var _ actor.StateManagerContext = (*memoryStateManager)(nil)

func newTestConfigActor() *configactor.ConfigActor {
	a := configactor.NewConfigActor(nil)
	a.SetID(configactor.GlobalActorID)
	a.SetStateManager(newMemoryStateManager())
	return a
}

func TestConfigActorThreeTierPrecedenceAndNamespaceIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := newTestConfigActor()

	require.Equal(t, "ConfigActor", a.Type())
	require.Equal(t, "global", a.ID())
	require.Equal(t, "default", configactor.NormalizeNamespace(""))
	require.Equal(t, "default", configactor.NormalizeNamespace("   "))
	require.Equal(t, "proj-a", configactor.NormalizeNamespace(" proj-a "))

	// 1. Populate Global tier
	_, err := a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "GLOBAL_ONLY", Value: "g-only", IsSecret: false},
			{Name: "NS_OVERRIDDEN", Value: "g-ns", IsSecret: false},
			{Name: "RUN_OVERRIDDEN", Value: "g-run", IsSecret: false},
			{Name: "SECRET_SHARED", Value: "sec-global-99", IsSecret: true},
		},
	})
	require.NoError(t, err)

	// 2. Populate Namespace "proj-a" and default namespace (empty string -> "default")
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope:     configactor.ScopeNamespace,
		Namespace: "proj-a",
		Entries: []configactor.EntryInput{
			{Name: "NS_OVERRIDDEN", Value: "proj-a-val", IsSecret: false},
			{Name: "PROJ_A_ONLY", Value: "only-a", IsSecret: true},
			{Name: "SECRET_SHARED", Value: "sec-proj-a-88", IsSecret: false}, // cross-scope override allowed even with different kind
		},
	})
	require.NoError(t, err)

	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope:     configactor.ScopeNamespace,
		Namespace: "", // normalizes to "default"
		Entries: []configactor.EntryInput{
			{Name: "DEFAULT_NS_VAR", Value: "from-default", IsSecret: false},
		},
	})
	require.NoError(t, err)

	// 3. Populate Run "run-1"
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeRun,
		RunID: "run-1",
		Entries: []configactor.EntryInput{
			{Name: "RUN_OVERRIDDEN", Value: "run-1-val", IsSecret: false},
			{Name: "RUN_ONLY", Value: "only-run-1", IsSecret: true},
		},
	})
	require.NoError(t, err)

	// Verify RunOverrides > Run > Namespace("proj-a") > Global
	resolved, err := a.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		Namespace: "proj-a",
		RunID:     "run-1",
		RunOverrides: map[string]string{
			"RUN_OVERRIDDEN": "override-val",
			"OVERRIDE_ONLY":  "ephemeral-val",
		},
		Keys: []string{"GLOBAL_ONLY", "NS_OVERRIDDEN", "RUN_OVERRIDDEN", "PROJ_A_ONLY", "RUN_ONLY", "SECRET_SHARED", "OVERRIDE_ONLY"},
	})
	require.NoError(t, err)
	assert.Empty(t, resolved.MissingKeys)
	assert.Equal(t, map[string]string{
		"GLOBAL_ONLY":    "g-only",
		"NS_OVERRIDDEN":  "proj-a-val",
		"RUN_OVERRIDDEN": "override-val",
		"PROJ_A_ONLY":    "only-a",
		"RUN_ONLY":       "only-run-1",
		"SECRET_SHARED":  "sec-proj-a-88",
		"OVERRIDE_ONLY":  "ephemeral-val",
	}, resolved.Values)

	// Verify ResolveForNode strictly returns ONLY requested keys
	subset, err := a.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		Namespace: "proj-a",
		RunID:     "run-1",
		Keys:      []string{"GLOBAL_ONLY", "RUN_ONLY"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"GLOBAL_ONLY": "g-only",
		"RUN_ONLY":    "only-run-1",
	}, subset.Values)

	// Verify Namespace("proj-b") is completely isolated from "proj-a" and "default"
	pfB, err := a.PreflightCheck(ctx, &configactor.PreflightCheckReq{
		Namespace:    "proj-b",
		RequiredKeys: []string{"GLOBAL_ONLY", "PROJ_A_ONLY", "DEFAULT_NS_VAR", "RUN_ONLY", "Z_MISSING"},
	})
	require.NoError(t, err)
	assert.False(t, pfB.OK)
	assert.Equal(t, []string{"DEFAULT_NS_VAR", "PROJ_A_ONLY", "RUN_ONLY", "Z_MISSING"}, pfB.MissingKeys)

	resB, err := a.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		Namespace: "proj-b",
		Keys:      []string{"NS_OVERRIDDEN", "SECRET_SHARED", "PROJ_A_ONLY"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"PROJ_A_ONLY"}, resB.MissingKeys)
	assert.Equal(t, map[string]string{
		"NS_OVERRIDDEN": "g-ns",
		"SECRET_SHARED": "sec-global-99",
	}, resB.Values)

	// Verify default namespace reads DEFAULT_NS_VAR and Global
	pfDef, err := a.PreflightCheck(ctx, &configactor.PreflightCheckReq{
		Namespace:    "",
		RunID:        "run-1",
		RunOverrides: map[string]string{"EPHEMERAL": "ok"},
		RequiredKeys: []string{"GLOBAL_ONLY", "DEFAULT_NS_VAR", "RUN_ONLY", "EPHEMERAL"},
	})
	require.NoError(t, err)
	assert.True(t, pfDef.OK)
	assert.Empty(t, pfDef.MissingKeys)
}

func TestConfigActorValidationMutualExclusionAndWriteOnlySecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := newTestConfigActor()

	// Structurally guarantee SecretMeta has NO Value field.
	secretMetaType := reflect.TypeOf(configactor.SecretMeta{})
	_, hasValueField := secretMetaType.FieldByName("Value")
	assert.False(t, hasValueField, "SecretMeta must never contain a Value field")

	// Reject invalid key names
	for _, badKey := range []string{"", "1ABC", "FOO-BAR", "FOO BAR", "A.B", "$HOME"} {
		_, err := a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
			Scope: configactor.ScopeGlobal,
			Entries: []configactor.EntryInput{
				{Name: badKey, Value: "v", IsSecret: false},
			},
		})
		require.Error(t, err, "key %q must be rejected", badKey)
	}

	// Reject invalid scopes / missing RunID
	_, err := a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeRun,
		RunID: "   ",
		Entries: []configactor.EntryInput{
			{Name: "GOOD_KEY", Value: "v"},
		},
	})
	require.Error(t, err)

	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.Scope("invalid"),
		Entries: []configactor.EntryInput{
			{Name: "GOOD_KEY", Value: "v"},
		},
	})
	require.Error(t, err)

	// Set Secret("FOO") and Env("BAR") in Global
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "FOO", Value: "ultra-secret-foo-value", IsSecret: true},
			{Name: "BAR", Value: "plain-bar-value", IsSecret: false},
		},
	})
	require.NoError(t, err)

	// Same-scope Secret -> Env conflict MUST fail
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "NEW_OK", Value: "should-not-partially-commit", IsSecret: false},
			{Name: "FOO", Value: "downgrade-attempt", IsSecret: false},
		},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, configactor.ErrKindConflict))

	// Same-scope Env -> Secret conflict MUST fail
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "BAR", Value: "upgrade-attempt", IsSecret: true},
		},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, configactor.ErrKindConflict))

	// Same-batch conflict MUST fail
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "DUP", Value: "1", IsSecret: false},
			{Name: "DUP", Value: "2", IsSecret: true},
		},
	})
	require.Error(t, err)

	// Verify NEW_OK was NOT partially committed when batch failed
	meta, err := a.ListMetadata(ctx, &configactor.ListMetadataReq{Scope: configactor.ScopeGlobal})
	require.NoError(t, err)
	require.Len(t, meta.Envs, 1)
	assert.Equal(t, "BAR", meta.Envs[0].Name)
	assert.Equal(t, "plain-bar-value", meta.Envs[0].Value)
	require.Len(t, meta.Secrets, 1)
	assert.Equal(t, "FOO", meta.Secrets[0].Name)
	assert.Equal(t, configactor.ScopeGlobal, meta.Secrets[0].Scope)
	assert.False(t, meta.Secrets[0].UpdatedAt.IsZero())

	// Serialized ListMetadata and PreflightCheck responses MUST NOT leak secret plaintext
	rawMeta, err := json.Marshal(meta)
	require.NoError(t, err)
	assert.NotContains(t, string(rawMeta), "ultra-secret-foo-value")

	pf, err := a.PreflightCheck(ctx, &configactor.PreflightCheckReq{
		RequiredKeys: []string{"FOO", "BAR", "MISSING_KEY"},
	})
	require.NoError(t, err)
	rawPF, err := json.Marshal(pf)
	require.NoError(t, err)
	assert.NotContains(t, string(rawPF), "ultra-secret-foo-value")
	assert.NotContains(t, string(rawPF), "plain-bar-value")
	assert.Equal(t, []string{"MISSING_KEY"}, pf.MissingKeys)

	// Same-kind overwrite succeeds
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "FOO", Value: "rotated-secret-foo", IsSecret: true},
		},
	})
	require.NoError(t, err)
	res, err := a.ResolveForNode(ctx, &configactor.ResolveForNodeReq{Keys: []string{"FOO"}})
	require.NoError(t, err)
	assert.Equal(t, "rotated-secret-foo", res.Values["FOO"])

	// DeleteEntry allows subsequent re-creation as Env
	delResp, err := a.DeleteEntry(ctx, &configactor.DeleteEntryReq{
		Scope: configactor.ScopeGlobal,
		Name:  "FOO",
	})
	require.NoError(t, err)
	assert.True(t, delResp.Deleted)

	delAgain, err := a.DeleteEntry(ctx, &configactor.DeleteEntryReq{
		Scope: configactor.ScopeGlobal,
		Name:  "FOO",
	})
	require.NoError(t, err)
	assert.False(t, delAgain.Deleted)

	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "FOO", Value: "now-plain-env", IsSecret: false},
		},
	})
	require.NoError(t, err)

	// DeleteRunScope wipes all run entries cleanly
	_, err = a.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeRun,
		RunID: "run-clean",
		Entries: []configactor.EntryInput{
			{Name: "RUN_ENV", Value: "v1", IsSecret: false},
			{Name: "RUN_SEC", Value: "s1", IsSecret: true},
		},
	})
	require.NoError(t, err)
	delRun, err := a.DeleteRunScope(ctx, &configactor.DeleteRunScopeReq{RunID: "run-clean"})
	require.NoError(t, err)
	assert.Equal(t, 2, delRun.DeletedCount)

	runMeta, err := a.ListMetadata(ctx, &configactor.ListMetadataReq{
		Scope: configactor.ScopeRun,
		RunID: "run-clean",
	})
	require.NoError(t, err)
	assert.Empty(t, runMeta.Envs)
	assert.Empty(t, runMeta.Secrets)
}

func TestExtractPlanAndNodeEnvRefsFromPlannerAndNestedModules(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "child")
	deepDir := filepath.Join(childDir, "modules", "deep")
	require.NoError(t, os.MkdirAll(deepDir, 0o700))

	writeHCL(t, filepath.Join(deepDir, "main.sofa.hcl"), `
variable "secret_ref_var" {
  type      = string
  sensitive = true
}

source "deep_src" {
  url       = "https://github.com/example/deep.git"
  branch    = "main"
  token_ref = "HOST_DEEP_SRC_TOKEN"
}

harness "deep_h" {
  env_refs = {
    CONTAINER_DEEP_KEY = var.secret_ref_var
  }
}

worker "leaf" {
  based_source   = source.deep_src.branch
  harness        = harness.deep_h
  model_provider = null
  model          = "gpt-5"
  system_prompt  = "deep leaf"
}
`)

	writeHCL(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "in_branch" {
  type = string
}

model_provider "child_mp" {
  type        = "openai"
  endpoint    = "https://api.example.com/v1"
  api_key_ref = "HOST_CHILD_MP_KEY"
}

harness "child_h" {
  env_refs = {
    CONTAINER_CHILD_KEY = "HOST_CHILD_HARNESS_KEY"
  }
}

mcp_server "child_mcp" {
  tools = ["lookup"]
  http {
    url              = "https://mcp.child.example.com"
    bearer_token_ref = "HOST_CHILD_MCP_TOKEN"
  }
}

worker "child_work" {
  based_source   = var.in_branch
  harness        = harness.child_h
  model_provider = model_provider.child_mp
  model          = "gpt-5"
  system_prompt  = "child work"
  mcp_tool_ids   = [mcp_server.child_mcp.tool_ids["lookup"]]
}

module "deep" {
  source         = "./modules/deep"
  secret_ref_var = "HOST_DEEP_HARNESS_VAR"
}
`)

	writeHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "primary" {
  url       = "https://github.com/example/primary.git"
  branch    = "main"
  token_ref = "HOST_PRIMARY_GIT_TOKEN"
}

source "secondary" {
  url         = "https://github.com/example/secondary.git"
  branch      = "main"
  ssh_key_ref = "HOST_SECONDARY_SSH_KEY"
}

sandbox "s" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "work_h" {
  env_refs = {
    CONTAINER_API_KEY = "HOST_WORK_HARNESS_KEY"
    CONTAINER_REGION  = "HOST_WORK_HARNESS_REGION"
  }
}

harness "qc_h" {
  env_refs = {
    CONTAINER_QC_KEY = "HOST_QC_HARNESS_KEY"
  }
}

model_provider "work_mp" {
  type        = "openai"
  endpoint    = "https://api.openai.com/v1"
  api_key_ref = "HOST_WORK_OPENAI_KEY"
}

model_provider "qc_mp" {
  type             = "anthropic"
  endpoint         = "https://api.anthropic.com"
  bearer_token_ref = "HOST_QC_BEARER_TOKEN"
}

mcp_server "docs_http" {
  tools = ["search"]
  http {
    url              = "https://mcp.example.com"
    bearer_token_ref = "HOST_MCP_HTTP_TOKEN"
  }
}

mcp_server "tools_stdio" {
  tools = ["lint"]
  stdio {
    command = "mcp-lint"
    env_refs = {
      CONTAINER_MCP_PASS = "HOST_MCP_STDIO_PASS"
    }
  }
}

go_tool "discover" {
  description = "Discover"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Checks map[string]string `+"`json:\"checks\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "step1" {
  based_source      = source.primary.branch
  harness           = harness.work_h
  model_provider    = model_provider.work_mp
  model             = "gpt-5"
  system_prompt     = "step 1"
  mcp_tool_ids      = [mcp_server.docs_http.tool_ids["search"]]
  terminate_tool_id = go_tool.discover.id

  qc {
    harness        = harness.qc_h
    model_provider = model_provider.qc_mp
    model          = "claude-opus"
    criteria       = { ok = "verify" }
  }
}

worker "step2" {
  based_source   = worker.step1.work_branch
  harness        = harness.work_h
  model_provider = null
  model          = "gpt-5"
  system_prompt  = "step 2"
}

dynamic_worker "dyn" {
  dynamic "worker" {
    for_each = jsondecodewithtype(worker.step1.result, object({ checks = map(string) })).checks
    iterator = item
    content {
      based_source   = worker.step1.work_branch
      harness        = harness.work_h
      model_provider = model_provider.work_mp
      model          = "gpt-5"
      system_prompt  = "verify ${item.key}"
      mcp_tool_ids   = [mcp_server.tools_stdio.tool_ids["lint"]]
    }
  }
}

merge "integrate" {
  based_source = source.primary.branch
  sources      = dynamic_worker.dyn.work_branches
}

module "child" {
  source    = "./modules/child"
  in_branch = worker.step2.work_branch
}
`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), rootDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:    context.Background(),
		Directory:  rootDir,
		BundlePath: "config",
	})
	require.NoError(t, cfg.Init(loaded.Blocks))
	require.NoError(t, cfg.RunPlan())

	planned, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	// ExtractPlanEnvRefs must collect all host refs across root + child + deep modules,
	// and NEVER include container-side env_refs keys!
	allRefs := configactor.ExtractPlanEnvRefs(planned)
	expectedAll := []string{
		"HOST_CHILD_HARNESS_KEY",
		"HOST_CHILD_MCP_TOKEN",
		"HOST_CHILD_MP_KEY",
		"HOST_DEEP_HARNESS_VAR",
		"HOST_DEEP_SRC_TOKEN",
		"HOST_MCP_HTTP_TOKEN",
		"HOST_MCP_STDIO_PASS",
		"HOST_PRIMARY_GIT_TOKEN",
		"HOST_QC_BEARER_TOKEN",
		"HOST_QC_HARNESS_KEY",
		"HOST_SECONDARY_SSH_KEY",
		"HOST_WORK_HARNESS_KEY",
		"HOST_WORK_HARNESS_REGION",
		"HOST_WORK_OPENAI_KEY",
	}
	assert.Equal(t, expectedAll, allRefs)
	for _, containerKey := range []string{"CONTAINER_API_KEY", "CONTAINER_REGION", "CONTAINER_QC_KEY", "CONTAINER_MCP_PASS", "CONTAINER_DEEP_KEY", "CONTAINER_CHILD_KEY"} {
		assert.NotContains(t, allRefs, containerKey)
	}

	// Nil plan returns empty slice safely
	assert.Empty(t, configactor.ExtractPlanEnvRefs(nil))

	// Per-node minimal extraction: worker.step1
	step1Refs, err := configactor.ExtractNodeEnvRefs(planned, "worker.step1")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"HOST_MCP_HTTP_TOKEN",
		"HOST_PRIMARY_GIT_TOKEN",
		"HOST_QC_BEARER_TOKEN",
		"HOST_QC_HARNESS_KEY",
		"HOST_WORK_HARNESS_KEY",
		"HOST_WORK_HARNESS_REGION",
		"HOST_WORK_OPENAI_KEY",
	}, step1Refs)

	// worker.step2 (model_provider = null, no MCP, depends on worker.step1) MUST NOT inherit step1's MCP or QC or provider refs!
	step2Refs, err := configactor.ExtractNodeEnvRefs(planned, "worker.step2")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"HOST_PRIMARY_GIT_TOKEN",
		"HOST_WORK_HARNESS_KEY",
		"HOST_WORK_HARNESS_REGION",
	}, step2Refs)

	// dynamic_worker.dyn (both canonical base and expanded instance address)
	dynRefs, err := configactor.ExtractNodeEnvRefs(planned, `dynamic_worker.dyn["check-1"]`)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"HOST_MCP_STDIO_PASS",
		"HOST_PRIMARY_GIT_TOKEN",
		"HOST_WORK_HARNESS_KEY",
		"HOST_WORK_HARNESS_REGION",
		"HOST_WORK_OPENAI_KEY",
	}, dynRefs)

	// merge.integrate only needs source.primary git token
	mergeRefs, err := configactor.ExtractNodeEnvRefs(planned, "merge.integrate")
	require.NoError(t, err)
	assert.Equal(t, []string{"HOST_PRIMARY_GIT_TOKEN"}, mergeRefs)

	// Child module worker uses child_h + child_mp + child_mcp + primary repo source
	childWorkRefs, err := configactor.ExtractNodeEnvRefs(planned, "module.child.worker.child_work")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"HOST_CHILD_HARNESS_KEY",
		"HOST_CHILD_MCP_TOKEN",
		"HOST_CHILD_MP_KEY",
		"HOST_PRIMARY_GIT_TOKEN",
	}, childWorkRefs)

	// Deep nested module worker uses deep_h + deep_src
	deepLeafRefs, err := configactor.ExtractNodeEnvRefs(planned, "module.child.module.deep.worker.leaf")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"HOST_DEEP_HARNESS_VAR",
		"HOST_DEEP_SRC_TOKEN",
	}, deepLeafRefs)

	// Unknown node returns error
	_, err = configactor.ExtractNodeEnvRefs(planned, "worker.does_not_exist")
	require.Error(t, err)
}

func TestExtractNodeEnvRefsRootSingletonInheritanceIntoChildModule(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "child")
	require.NoError(t, os.MkdirAll(childDir, 0o700))

	writeHCL(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "in_branch" {
  type = string
}

worker "inherited" {
  based_source  = var.in_branch
  model         = "gpt-5"
  system_prompt = "uses root singletons"
}
`)

	writeHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "repo" {
  url       = "https://github.com/example/repo.git"
  branch    = "main"
  token_ref = "HOST_ROOT_GIT_TOKEN"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}

harness "only_h" {
  env_refs = {
    K = "HOST_ROOT_HARNESS_KEY"
  }
}

model_provider "only_mp" {
  type        = "openai"
  endpoint    = "https://api.example.com/v1"
  api_key_ref = "HOST_ROOT_MP_KEY"
}

module "child" {
  source    = "./modules/child"
  in_branch = source.repo.branch
}
`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), rootDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:   context.Background(),
		Directory: rootDir,
	})
	require.NoError(t, cfg.Init(loaded.Blocks))
	require.NoError(t, cfg.RunPlan())

	planned, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	refs, err := configactor.ExtractNodeEnvRefs(planned, "module.child.worker.inherited")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"HOST_ROOT_GIT_TOKEN",
		"HOST_ROOT_HARNESS_KEY",
		"HOST_ROOT_MP_KEY",
	}, refs)
}

func TestExtractPlanAndNodeEnvRefsExternalToolAndEdgeValues(t *testing.T) {
	t.Parallel()

	extToolID := "tool_external_tool_db_12345678-1234-8234-9234-123456789abc"
	p, err := plan.New(plan.Args{
		Nodes: []plan.NodeSpec{
			{
				Address:          "external_tool.db",
				CanonicalAddress: "external_tool.db",
				Kind:             "external_tool",
				Config: cty.ObjectVal(map[string]cty.Value{
					"id": cty.StringVal(extToolID),
					"env_refs": cty.MapVal(map[string]cty.Value{
						"DB_PASSWORD": cty.StringVal("HOST_DB_PASS").Mark(golden.SensitiveMark),
					}),
				}),
			},
			{
				Address:          "source.unknown_src",
				CanonicalAddress: "source.unknown_src",
				Kind:             "source",
				Config: cty.ObjectVal(map[string]cty.Value{
					"url":         cty.StringVal("https://example.com/repo.git"),
					"token_ref":   cty.NullVal(cty.String),
					"ssh_key_ref": cty.UnknownVal(cty.String),
				}),
			},
			{
				Address:          "harness.nil_cfg",
				CanonicalAddress: "harness.nil_cfg",
				Kind:             "harness",
				Config:           cty.NilVal,
			},
			{
				Address:          "worker.use_ext",
				CanonicalAddress: "worker.use_ext",
				Kind:             "worker",
				Dependencies:     []string{"external_tool.db"},
				Config: cty.ObjectVal(map[string]cty.Value{
					"tool_ids": cty.ListVal([]cty.Value{cty.StringVal(extToolID)}),
				}),
			},
		},
		Tools: map[string]plan.ToolSpec{
			extToolID: {
				ID:               extToolID,
				Name:             "db",
				Address:          "external_tool.db",
				CanonicalAddress: "external_tool.db",
				Kind:             "external",
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"HOST_DB_PASS"}, configactor.ExtractPlanEnvRefs(p))

	nodeRefs, err := configactor.ExtractNodeEnvRefs(p, "worker.use_ext")
	require.NoError(t, err)
	assert.Equal(t, []string{"HOST_DB_PASS"}, nodeRefs)
}

func TestExtractNodeEnvRefsQCToolIDsAndUnknownBasedSource(t *testing.T) {
	t.Parallel()

	qcExtToolID := "tool_external_tool_qc_audit_12345678-1234-8234-9234-123456789abc"

	t.Run("qc_tool_ids_on_worker_and_dynamic_worker", func(t *testing.T) {
		p, err := plan.New(plan.Args{
			Nodes: []plan.NodeSpec{
				{
					Address:          "external_tool.qc_audit",
					CanonicalAddress: "external_tool.qc_audit",
					Kind:             "external_tool",
					Config: cty.ObjectVal(map[string]cty.Value{
						"id": cty.StringVal(qcExtToolID),
						"env_refs": cty.MapVal(map[string]cty.Value{
							"CONTAINER_QC_TOOL": cty.StringVal("HOST_QC_TOOL_SECRET"),
						}),
					}),
				},
				{
					Address:          "worker.with_qc_tool",
					CanonicalAddress: "worker.with_qc_tool",
					Kind:             "worker",
					Dependencies:     []string{"external_tool.qc_audit"},
					Config: cty.ObjectVal(map[string]cty.Value{
						"based_source": cty.StringVal("main"),
						"qc": cty.ObjectVal(map[string]cty.Value{
							"tool_ids": cty.ListVal([]cty.Value{cty.StringVal(qcExtToolID)}),
						}),
					}),
				},
			},
			Tools: map[string]plan.ToolSpec{
				qcExtToolID: {
					ID:               qcExtToolID,
					Name:             "qc_audit",
					Address:          "external_tool.qc_audit",
					CanonicalAddress: "external_tool.qc_audit",
					Kind:             "external",
				},
			},
		})
		require.NoError(t, err)

		refs, err := configactor.ExtractNodeEnvRefs(p, "worker.with_qc_tool")
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_QC_TOOL_SECRET"}, refs)
		assert.NotContains(t, refs, "CONTAINER_QC_TOOL")
	})

	t.Run("unknown_based_source_on_dynamic_worker_child_worker_and_merge", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "modules", "child")
		require.NoError(t, os.MkdirAll(childDir, 0o700))

		writeHCL(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "unknown_branch" {
  type = string
}

worker "child_unknown" {
  based_source  = var.unknown_branch
  model         = "gpt-5"
  system_prompt = "child unknown based_source"
}

merge "child_merge_unknown" {
  based_source = var.unknown_branch
  sources      = [worker.child_unknown.work_branch]
}
`)

		writeHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "repo" {
  url       = "https://github.com/example/repo.git"
  branch    = "main"
  token_ref = "HOST_UNKNOWN_SRC_TOKEN"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}

harness "h" {
  env_refs = {
    CONTAINER_H = "HOST_H_KEY"
  }
}

go_tool "discover" {
  description = "Discover"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Branches map[string]string `+"`json:\"branches\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "seed" {
  based_source      = source.repo.branch
  model             = "gpt-5"
  system_prompt     = "seed"
  terminate_tool_id = go_tool.discover.id
}

dynamic_worker "dyn_unknown_src" {
  dynamic "worker" {
    for_each = jsondecodewithtype(worker.seed.result, object({ branches = map(string) })).branches
    iterator = item
    content {
      based_source  = item.value
      model         = "gpt-5"
      system_prompt = "run on ${item.key}"
    }
  }
}

module "child" {
  source         = "./modules/child"
  unknown_branch = dynamic_worker.dyn_unknown_src.work_branches[0]
}
`)

		loaded, diags, err := config.LoadDirectoryContext(context.Background(), rootDir)
		require.NoError(t, err)
		require.False(t, diags.HasErrors(), diags.Error())

		cfg := planner.NewSofaConfig(planner.ConfigArgs{
			Context:   context.Background(),
			Directory: rootDir,
		})
		require.NoError(t, cfg.Init(loaded.Blocks))
		require.NoError(t, cfg.RunPlan())

		planned, err := cfg.ExportPlan(planner.ExportOptions{})
		require.NoError(t, err)

		dynRefs, err := configactor.ExtractNodeEnvRefs(planned, `dynamic_worker.dyn_unknown_src["a"]`)
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_H_KEY", "HOST_UNKNOWN_SRC_TOKEN"}, dynRefs)

		childWorkerRefs, err := configactor.ExtractNodeEnvRefs(planned, "module.child.worker.child_unknown")
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_H_KEY", "HOST_UNKNOWN_SRC_TOKEN"}, childWorkerRefs)

		childMergeRefs, err := configactor.ExtractNodeEnvRefs(planned, "module.child.merge.child_merge_unknown")
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_UNKNOWN_SRC_TOKEN"}, childMergeRefs)
	})

	t.Run("empty_source_url_with_unknown_based_source_fallback", func(t *testing.T) {
		childPlan, err := plan.New(plan.Args{
			Nodes: []plan.NodeSpec{
				{
					Address:          "worker.child_w",
					CanonicalAddress: "module.child.worker.child_w",
					Kind:             "worker",
					Config: cty.ObjectVal(map[string]cty.Value{
						"based_source": cty.UnknownVal(cty.String),
						"source_url":   cty.StringVal(""),
					}),
				},
			},
		})
		require.NoError(t, err)

		rootPlan, err := plan.New(plan.Args{
			Nodes: []plan.NodeSpec{
				{
					Address:          "source.repo",
					CanonicalAddress: "source.repo",
					Kind:             "source",
					Config: cty.ObjectVal(map[string]cty.Value{
						"url":       cty.StringVal("https://github.com/example/repo.git"),
						"token_ref": cty.StringVal("HOST_FALLBACK_TOKEN"),
					}),
				},
				{
					Address:          "dynamic_worker.dyn",
					CanonicalAddress: "dynamic_worker.dyn",
					Kind:             "dynamic_worker",
					Config: cty.ObjectVal(map[string]cty.Value{
						"content": cty.ObjectVal(map[string]cty.Value{
							"based_source": cty.UnknownVal(cty.String),
							"source_url":   cty.StringVal(""),
						}),
					}),
				},
				{
					Address:          "merge.m",
					CanonicalAddress: "merge.m",
					Kind:             "merge",
					Config: cty.ObjectVal(map[string]cty.Value{
						"based_source": cty.UnknownVal(cty.String),
						"source_url":   cty.StringVal(""),
					}),
				},
				{
					Address:          "module.child",
					CanonicalAddress: "module.child",
					Kind:             "module",
					Module: &plan.ModuleSpec{
						Plan:             childPlan,
						CanonicalAddress: "module.child",
					},
				},
			},
		})
		require.NoError(t, err)

		dynRefs, err := configactor.ExtractNodeEnvRefs(rootPlan, `dynamic_worker.dyn["k"]`)
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_FALLBACK_TOKEN"}, dynRefs)

		childRefs, err := configactor.ExtractNodeEnvRefs(rootPlan, "module.child.worker.child_w")
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_FALLBACK_TOKEN"}, childRefs)

		mergeRefs, err := configactor.ExtractNodeEnvRefs(rootPlan, "merge.m")
		require.NoError(t, err)
		assert.Equal(t, []string{"HOST_FALLBACK_TOKEN"}, mergeRefs)
	})
}

func writeHCL(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o600))
}
