package planner

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/harness"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcpspec "github.com/lonegunmanb/sofa/internal/mcp/spec"
	mergespec "github.com/lonegunmanb/sofa/internal/merge/spec"
	modulespec "github.com/lonegunmanb/sofa/internal/module/spec"
	"github.com/lonegunmanb/sofa/internal/plan"
	providerspec "github.com/lonegunmanb/sofa/internal/provider"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	sourcespec "github.com/lonegunmanb/sofa/internal/source"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// ExportOptions configures conversion of a planned *SofaConfig into an immutable *plan.Plan.
type ExportOptions struct {
	PluginLock        *harness.PluginLockFile
	RequirePluginLock bool
}

// ExportPlan converts a planned root SofaConfig (and recursively all child modules)
// into an immutable *plan.Plan preserving topology, sensitivity marks, unknowns,
// tool metadata, and plugin lockfile verification.
func (c *SofaConfig) ExportPlan(opts ExportOptions) (*plan.Plan, error) {
	if c == nil {
		return nil, fmt.Errorf("sofa config is nil")
	}
	return c.exportPlan(opts, nil)
}

func (c *SofaConfig) exportPlan(opts ExportOptions, deferredValidations map[string][]corespec.Condition) (*plan.Plan, error) {
	var lockFile harness.PluginLockFile
	if opts.PluginLock != nil {
		lockFile = *opts.PluginLock
	} else {
		lockFile = harness.PluginLockFile{FormatVersion: harness.LockFormatVersion}
	}

	vertices := c.GetVertices()
	exportedAddrs := make(map[string]struct{})
	nodeBlocks := make([]golden.Block, 0, len(vertices))
	tools := make(map[string]plan.ToolSpec)

	for addr, vertex := range vertices {
		switch b := vertex.(type) {
		case *sourcespec.SourceBlock,
			*sourcespec.PreserveBlock,
			*sandboxspec.SandboxBlock,
			*providerspec.ModelProviderBlock,
			*mcpspec.ServerBlock,
			*workerspec.WorkerBlock,
			*workerspec.DynamicWorkerBlock,
			*mergespec.MergeBlock,
			*modulespec.ModuleBlock:
			exportedAddrs[addr] = struct{}{}
			nodeBlocks = append(nodeBlocks, b.(golden.Block))
		case *harnessspec.HarnessBlock:
			if opts.RequirePluginLock {
				if err := harness.CanSatisfyHarnessLock(b.HarnessConfig(), lockFile); err != nil {
					return nil, err
				}
			}
			exportedAddrs[addr] = struct{}{}
			nodeBlocks = append(nodeBlocks, b)
		case *toolspec.GoToolBlock:
			exportedAddrs[addr] = struct{}{}
			nodeBlocks = append(nodeBlocks, b)
			ts, err := encodeGoToolSpec(b, c.directory)
			if err != nil {
				return nil, fmt.Errorf("export %s: %w", addr, err)
			}
			tools[ts.ID] = ts
		case *toolspec.ExternalToolBlock:
			exportedAddrs[addr] = struct{}{}
			nodeBlocks = append(nodeBlocks, b)
			ts, err := encodeExternalToolSpec(b, c.directory)
			if err != nil {
				return nil, fmt.Errorf("export %s: %w", addr, err)
			}
			tools[ts.ID] = ts
		}
	}

	// Collect intra-plan transitive dependencies (including across locals/vars).
	depsByAddr := make(map[string][]string, len(exportedAddrs))
	for addr := range exportedAddrs {
		ancestors, err := c.GetAncestors(addr)
		if err != nil {
			return nil, fmt.Errorf("read ancestors for %s: %w", addr, err)
		}
		deps := make([]string, 0)
		for anc := range ancestors {
			if anc == addr {
				continue
			}
			if _, ok := exportedAddrs[anc]; ok {
				deps = append(deps, anc)
			}
		}
		sort.Strings(deps)
		depsByAddr[addr] = deps
	}

	nodes := make([]plan.NodeSpec, 0, len(nodeBlocks))
	for _, block := range nodeBlocks {
		addr := block.Address()
		node, err := c.exportNode(block, depsByAddr[addr], opts, tools)
		if err != nil {
			return nil, fmt.Errorf("export node %s: %w", addr, err)
		}
		nodes = append(nodes, node)
	}

	depths := computeTopologicalDepths(nodes)
	sort.Slice(nodes, func(i, j int) bool {
		di, dj := depths[nodes[i].Address], depths[nodes[j].Address]
		if di != dj {
			return di < dj
		}
		return nodes[i].Address < nodes[j].Address
	})

	outputsList := c.Outputs()
	outputs := make(map[string]plan.OutputSpec, len(outputsList))
	outputBlocks := make(map[string]*golden.OutputBlock)
	for _, ob := range golden.Blocks[*golden.OutputBlock](c) {
		outputBlocks[ob.Name()] = ob
	}
	for _, out := range outputsList {
		var sourceURLs []string
		if ob := outputBlocks[out.Name]; ob != nil {
			if valAttr, ok := ob.HclBlock().Attributes()["value"]; ok {
				sourceURLs = c.resolveExpressionSourceURLs(valAttr.Expr, nil)
			}
		}
		outputs[out.Name] = plan.OutputSpec{
			Value:       out.Value,
			Description: out.Description,
			Sensitive:   out.Sensitive,
			Expression:  out.Expression,
			SourceURLs:  sourceURLs,
		}
	}

	contextValues := make(map[string]cty.Value)
	if evalCtx := c.EvalContext(); evalCtx != nil {
		for _, key := range []string{"var", "local", "path"} {
			if val, ok := evalCtx.Variables[key]; ok && val != cty.NilVal {
				contextValues[key] = val
			}
		}
	}

	localExprs := make(map[string]string)
	for _, lb := range golden.Blocks[*golden.LocalBlock](c) {
		if attr, ok := lb.HclBlock().Attributes()["value"]; ok {
			localExprs[lb.Name()] = savedExpressionSource(attr)
		}
	}

	var rootLock *harness.PluginLockFile
	if !c.isChildModule && opts.PluginLock != nil {
		rootLock = opts.PluginLock
	}

	return plan.New(plan.Args{
		Directory:           c.directory,
		BundlePath:          c.bundlePath,
		Nodes:               nodes,
		Outputs:             outputs,
		Context:             contextValues,
		LocalExpressions:    localExprs,
		DeferredValidations: deferredValidations,
		Tools:               tools,
		PluginLock:          rootLock,
	})
}

func (c *SofaConfig) exportNode(
	block golden.Block,
	deps []string,
	opts ExportOptions,
	tools map[string]plan.ToolSpec,
) (plan.NodeSpec, error) {
	addr := block.Address()
	canonical := c.CanonicalAddress(addr)
	origin := blockOrigin(block.HclBlock(), c.directory)

	switch b := block.(type) {
	case *sourcespec.SourceBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "source",
			Dependencies:     deps,
			Config:           encodeSourceConfig(b.SourceConfig()),
			Origin:           origin,
		}, nil
	case *sourcespec.PreserveBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "preserve",
			Dependencies:     deps,
			Config:           encodePreserveConfig(b.PreserveConfig()),
			Origin:           origin,
		}, nil
	case *sandboxspec.SandboxBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "sandbox",
			Dependencies:     deps,
			Config:           encodeSandboxConfig(b.SandboxConfig()),
			Origin:           origin,
		}, nil
	case *harnessspec.HarnessBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "harness",
			Dependencies:     deps,
			Config:           encodeHarnessConfig(b),
			Origin:           origin,
		}, nil
	case *providerspec.ModelProviderBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "model_provider",
			Dependencies:     deps,
			Config:           encodeModelProviderConfig(b),
			Origin:           origin,
		}, nil
	case *mcpspec.ServerBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "mcp_server",
			Dependencies:     deps,
			Config:           encodeMCPServerConfig(b),
			Origin:           origin,
		}, nil
	case *toolspec.GoToolBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "go_tool",
			Dependencies:     deps,
			Config:           encodeGoToolConfig(b.GoToolConfig()),
			Origin:           origin,
		}, nil
	case *toolspec.ExternalToolBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "external_tool",
			Dependencies:     deps,
			Config:           encodeExternalToolConfig(b.ExternalToolConfig()),
			Origin:           origin,
		}, nil
	case *workerspec.WorkerBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "worker",
			Dependencies:     deps,
			Config:           encodeWorkerConfig(b.WorkerConfig(), blockEachValue(b)),
			Origin:           origin,
		}, nil
	case *workerspec.DynamicWorkerBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "dynamic_worker",
			Dependencies:     deps,
			Config:           encodeDynamicWorkerConfig(b.DynamicWorkerConfig()),
			Origin:           origin,
		}, nil
	case *mergespec.MergeBlock:
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "merge",
			Dependencies:     deps,
			Config:           encodeMergeConfig(b.MergeConfig(), blockEachValue(b)),
			Origin:           origin,
		}, nil
	case *modulespec.ModuleBlock:
		mp := b.PlannedModule()
		childCfg, ok := mp.ChildConfig.(*SofaConfig)
		if !ok || childCfg == nil {
			return plan.NodeSpec{}, fmt.Errorf("module %s has no planned SofaConfig", addr)
		}
		childPlan, err := childCfg.exportPlan(opts, mp.DeferredValidations)
		if err != nil {
			return plan.NodeSpec{}, err
		}
		for id, ts := range childPlan.Tools() {
			tools[id] = ts
		}
		return plan.NodeSpec{
			Address:          addr,
			CanonicalAddress: canonical,
			Kind:             "module",
			Dependencies:     deps,
			Config:           encodeModuleNodeConfig(mp, blockEachValue(b)),
			Module: &plan.ModuleSpec{
				Plan:                childPlan,
				Source:              mp.Source,
				CanonicalAddress:    mp.CanonicalAddress,
				BundlePath:          mp.BundlePath,
				Parallelism:         mp.Parallelism,
				Timeout:             mp.Timeout,
				TimeoutRaw:          mp.TimeoutRaw,
				Inputs:              maps.Clone(mp.Inputs),
				InputExprs:          maps.Clone(mp.InputExprs),
				DeferredValidations: mp.DeferredValidations,
			},
			Origin: origin,
		}, nil
	default:
		return plan.NodeSpec{}, fmt.Errorf("unsupported block type %T", block)
	}
}

func computeTopologicalDepths(nodes []plan.NodeSpec) map[string]int {
	deps := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		deps[n.Address] = n.Dependencies
	}
	depths := make(map[string]int, len(nodes))
	var visit func(string) int
	visit = func(addr string) int {
		if d, ok := depths[addr]; ok {
			return d
		}
		maxDep := -1
		for _, dep := range deps[addr] {
			if d := visit(dep); d > maxDep {
				maxDep = d
			}
		}
		d := maxDep + 1
		depths[addr] = d
		return d
	}
	for _, n := range nodes {
		visit(n.Address)
	}
	return depths
}

func encodeGoToolSpec(b *toolspec.GoToolBlock, moduleDir string) (plan.ToolSpec, error) {
	cfg := b.GoToolConfig()
	schemaMap, err := cfg.InputConstraint.JSONSchema()
	if err != nil {
		return plan.ToolSpec{}, err
	}
	schemaBytes, err := json.Marshal(schemaMap)
	if err != nil {
		return plan.ToolSpec{}, err
	}
	inputType, err := ctyjson.MarshalType(cfg.InputConstraint.Type())
	if err != nil {
		return plan.ToolSpec{}, err
	}
	outputType, err := ctyjson.MarshalType(cfg.OutputConstraint.Type())
	if err != nil {
		return plan.ToolSpec{}, err
	}
	return plan.ToolSpec{
		ID:               cfg.ID,
		Name:             cfg.Name,
		Address:          cfg.Address,
		CanonicalAddress: cfg.CanonicalAddress,
		Kind:             "go",
		Description:      cfg.Description,
		Source:           cfg.Source,
		Dependencies:     slices.Clone(cfg.Dependencies),
		InputSchema:      schemaBytes,
		InputType:        inputType,
		OutputType:       outputType,
		QuotePaths:       cfg.QuotePaths,
		Postconditions:   slices.Clone(cfg.Postconditions),
		Origin:           blockOrigin(b.HclBlock(), moduleDir),
	}, nil
}

func encodeExternalToolSpec(b *toolspec.ExternalToolBlock, moduleDir string) (plan.ToolSpec, error) {
	cfg := b.ExternalToolConfig()
	schemaMap, err := cfg.InputConstraint.JSONSchema()
	if err != nil {
		return plan.ToolSpec{}, err
	}
	schemaBytes, err := json.Marshal(schemaMap)
	if err != nil {
		return plan.ToolSpec{}, err
	}
	inputType, err := ctyjson.MarshalType(cfg.InputConstraint.Type())
	if err != nil {
		return plan.ToolSpec{}, err
	}
	outputType, err := ctyjson.MarshalType(cfg.OutputConstraint.Type())
	if err != nil {
		return plan.ToolSpec{}, err
	}
	return plan.ToolSpec{
		ID:               cfg.ID,
		Name:             cfg.Name,
		Address:          cfg.Address,
		CanonicalAddress: cfg.CanonicalAddress,
		Kind:             "external",
		Description:      cfg.Description,
		Program:          slices.Clone(cfg.Program),
		WorkingDir:       cfg.WorkingDir,
		InputSchema:      schemaBytes,
		InputType:        inputType,
		OutputType:       outputType,
		Postconditions:   slices.Clone(cfg.Postconditions),
		Origin:           blockOrigin(b.HclBlock(), moduleDir),
	}, nil
}

func encodeSourceConfig(cfg sourcespec.SourceConfig) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"url":         cty.StringVal(cfg.URL),
		"branch":      cty.StringVal(cfg.Branch),
		"token_ref":   optionalStringPtrVal(cfg.TokenRef),
		"ssh_key_ref": optionalStringPtrVal(cfg.SSHKeyRef),
	})
}

func encodePreserveConfig(cfg sourcespec.PreserveConfig) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"url":         cty.StringVal(cfg.URL),
		"branch":      cfg.Branch.Value,
		"branch_expr": cty.StringVal(cfg.BranchExpr),
	})
}

func encodeSandboxConfig(cfg sandboxspec.SandboxConfig) cty.Value {
	mounts := make([]cty.Value, len(cfg.Mounts))
	for i, m := range cfg.Mounts {
		mounts[i] = cty.ObjectVal(map[string]cty.Value{
			"source":    cty.StringVal(m.Source),
			"target":    cty.StringVal(m.Target),
			"read_only": cty.BoolVal(m.ReadOnly),
		})
	}
	mountsVal := cty.ListValEmpty(cty.Object(map[string]cty.Type{
		"source": cty.String, "target": cty.String, "read_only": cty.Bool,
	}))
	if len(mounts) > 0 {
		mountsVal = cty.ListVal(mounts)
	}
	resourcesVal := cty.NullVal(cty.Object(map[string]cty.Type{"cpus": cty.String, "memory": cty.String}))
	if cfg.Resources != nil {
		resourcesVal = cty.ObjectVal(map[string]cty.Value{
			"cpus":   cty.StringVal(cfg.Resources.CPUs),
			"memory": cty.StringVal(cfg.Resources.Memory),
		})
	}
	return cty.ObjectVal(map[string]cty.Value{
		"runtime":   cty.StringVal(cfg.Runtime),
		"image":     cty.StringVal(cfg.Image),
		"network":   cty.StringVal(cfg.Network),
		"mounts":    mountsVal,
		"resources": resourcesVal,
	})
}

func encodeHarnessConfig(b *harnessspec.HarnessBlock) cty.Value {
	cfg := b.HarnessConfig()
	optionsVal := cfg.Options
	if optionsVal == cty.NilVal {
		optionsVal = cty.NullVal(cty.EmptyObject)
	}
	val := b.Value()
	sourceVal := cty.NullVal(cty.String)
	versionVal := cty.NullVal(cty.String)
	if val != cty.NilVal && val.IsKnown() && !val.IsNull() && val.Type().IsObjectType() {
		if val.Type().HasAttribute("source") {
			sourceVal = val.GetAttr("source")
		}
		if val.Type().HasAttribute("version") {
			versionVal = val.GetAttr("version")
		}
	}
	return cty.ObjectVal(map[string]cty.Value{
		"name":         cty.StringVal(cfg.Name),
		"source":       sourceVal,
		"source_host":  cty.StringVal(cfg.SourceHost),
		"source_owner": cty.StringVal(cfg.SourceOwner),
		"source_repo":  cty.StringVal(cfg.SourceRepo),
		"version":      versionVal,
		"signature":    cty.StringVal(string(cfg.Signature)),
		"signing_key":  b.SigningKeyValue(),
		"env_refs":     stringMapVal(cfg.EnvRefs),
		"options":      optionsVal,
	})
}

func encodeModelProviderConfig(b *providerspec.ModelProviderBlock) cty.Value {
	cfg := b.ProviderConfig()
	headers := cfg.Headers
	if headers == cty.NilVal {
		headers = cty.NullVal(cty.Map(cty.String))
	}
	apiKey := b.APIKeyValue()
	if apiKey == cty.NilVal {
		apiKey = cty.NullVal(cty.String)
	}
	bearer := b.BearerTokenValue()
	if bearer == cty.NilVal {
		bearer = cty.NullVal(cty.String)
	}
	var wireAPI *string
	if cfg.WireAPI != nil {
		s := string(*cfg.WireAPI)
		wireAPI = &s
	}
	var transport *string
	if cfg.Transport != nil {
		s := string(*cfg.Transport)
		transport = &s
	}
	return cty.ObjectVal(map[string]cty.Value{
		"type":             cty.StringVal(string(cfg.Type)),
		"endpoint":         cty.StringVal(cfg.Endpoint),
		"wire_api":         optionalStringPtrVal(wireAPI),
		"transport":        optionalStringPtrVal(transport),
		"headers":          headers,
		"api_key":          apiKey,
		"api_key_ref":      optionalStringPtrVal(cfg.APIKeyRef),
		"bearer_token":     bearer,
		"bearer_token_ref": optionalStringPtrVal(cfg.BearerTokenRef),
		"retry":            encodeRetryOverride(cfg.Retry),
	})
}

func encodeMCPServerConfig(b *mcpspec.ServerBlock) cty.Value {
	cfg := b.ServerConfig()
	httpType := cty.Object(map[string]cty.Type{
		"url":              cty.String,
		"headers":          cty.Map(cty.String),
		"bearer_token":     cty.String,
		"bearer_token_ref": cty.String,
	})
	stdioType := cty.Object(map[string]cty.Type{
		"command":           cty.String,
		"args":              cty.List(cty.String),
		"env":               cty.Map(cty.String),
		"env_refs":          cty.Map(cty.String),
		"working_directory": cty.String,
	})
	httpVal := cty.NullVal(httpType)
	if cfg.HTTP != nil {
		bearer := cty.NullVal(cty.String)
		if cfg.HTTP.BearerToken != nil {
			bearer = corespec.MarkSensitive(cty.StringVal(*cfg.HTTP.BearerToken))
		}
		httpVal = cty.ObjectVal(map[string]cty.Value{
			"url":              cty.StringVal(cfg.HTTP.URL),
			"headers":          b.HTTPHeadersValue(),
			"bearer_token":     bearer,
			"bearer_token_ref": optionalStringPtrVal(cfg.HTTP.BearerTokenRef),
		})
	}
	stdioVal := cty.NullVal(stdioType)
	if cfg.Stdio != nil {
		stdioVal = cty.ObjectVal(map[string]cty.Value{
			"command":           cty.StringVal(cfg.Stdio.Command),
			"args":              stringListVal(cfg.Stdio.Args),
			"env":               b.StdioEnvValue(),
			"env_refs":          stringMapVal(cfg.Stdio.EnvRefs),
			"working_directory": cty.StringVal(cfg.Stdio.WorkingDirectory),
		})
	}
	return cty.ObjectVal(map[string]cty.Value{
		"name":                cty.StringVal(cfg.Name),
		"runtime_name":        cty.StringVal(cfg.RuntimeName),
		"transport":           cty.StringVal(string(cfg.Transport)),
		"tools":               stringListVal(cfg.Tools),
		"resources":           stringListVal(cfg.Resources),
		"timeout_nanoseconds": cty.NumberIntVal(int64(cfg.Timeout)),
		"http":                httpVal,
		"stdio":               stdioVal,
	})
}

func encodeGoToolConfig(cfg toolspec.GoToolConfig) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"id":                cty.StringVal(cfg.ID),
		"name":              cty.StringVal(cfg.Name),
		"address":           cty.StringVal(cfg.Address),
		"canonical_address": cty.StringVal(cfg.CanonicalAddress),
		"description":       cty.StringVal(cfg.Description),
		"source":            cty.StringVal(cfg.Source),
		"dependencies":      stringListVal(cfg.Dependencies),
		"postconditions":    encodeConditions(cfg.Postconditions),
	})
}

func encodeExternalToolConfig(cfg toolspec.ExternalToolConfig) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"id":                cty.StringVal(cfg.ID),
		"name":              cty.StringVal(cfg.Name),
		"address":           cty.StringVal(cfg.Address),
		"canonical_address": cty.StringVal(cfg.CanonicalAddress),
		"description":       cty.StringVal(cfg.Description),
		"program":           stringListVal(cfg.Program),
		"working_dir":       cty.StringVal(cfg.WorkingDir),
		"postconditions":    encodeConditions(cfg.Postconditions),
	})
}

func blockEachValue(block golden.Block) cty.Value {
	if block == nil {
		return cty.NullVal(cty.EmptyObject)
	}
	for ctx := block.EvalContext(); ctx != nil; ctx = ctx.Parent() {
		if ctx.Variables != nil {
			if eachVal, ok := ctx.Variables["each"]; ok && eachVal != cty.NilVal {
				return eachVal
			}
		}
	}
	return cty.NullVal(cty.EmptyObject)
}

func encodeWorkerConfig(cfg workerspec.Config, eachVal cty.Value) cty.Value {
	if eachVal == cty.NilVal {
		eachVal = cty.NullVal(cty.EmptyObject)
	}
	promptVal := cfg.Prompt
	if promptVal == cty.NilVal {
		promptVal = cty.NullVal(cty.String)
	}
	sandboxVal := cfg.Sandbox
	if sandboxVal == cty.NilVal {
		sandboxVal = cty.NullVal(cty.EmptyObject)
	}
	harnessVal := cfg.Harness
	if harnessVal == cty.NilVal {
		harnessVal = cty.NullVal(cty.EmptyObject)
	}
	providerVal := cfg.ModelProvider
	if providerVal == cty.NilVal {
		providerVal = cty.NullVal(cty.EmptyObject)
	}

	toolUses := make([]cty.Value, len(cfg.ToolUses))
	for i, tu := range cfg.ToolUses {
		input := tu.Input
		if input == cty.NilVal {
			input = cty.EmptyObjectVal
		}
		toolUses[i] = cty.ObjectVal(map[string]cty.Value{
			"name":      cty.StringVal(tu.Name),
			"tool_id":   cty.StringVal(tu.ToolID),
			"terminate": cty.BoolVal(tu.Terminate),
			"input":     input,
		})
	}
	toolUsesVal := cty.EmptyTupleVal
	if len(toolUses) > 0 {
		toolUsesVal = cty.TupleVal(toolUses)
	}

	artifacts := make(map[string]cty.Value, len(cfg.Artifacts))
	for name, art := range cfg.Artifacts {
		artifacts[name] = cty.ObjectVal(map[string]cty.Value{
			"name":           cty.StringVal(art.Name),
			"type":           cty.StringVal(art.Type),
			"path":           cty.StringVal(art.Path),
			"container_path": cty.StringVal(art.ContainerPath()),
			"description":    cty.StringVal(art.Description),
			"required":       cty.BoolVal(art.Required),
			"non_empty":      cty.BoolVal(art.NonEmpty),
		})
	}
	artifactsVal := cty.EmptyObjectVal
	if len(artifacts) > 0 {
		artifactsVal = cty.ObjectVal(artifacts)
	}

	imports := make(map[string]cty.Value, len(cfg.Imports))
	for name, imp := range cfg.Imports {
		srcs := imp.Sources
		if srcs == cty.NilVal {
			srcs = cty.EmptyTupleVal
		}
		imports[name] = cty.ObjectVal(map[string]cty.Value{
			"name":           cty.StringVal(imp.Name),
			"desc":           cty.StringVal(imp.Desc),
			"path":           cty.StringVal(workerspec.EffectiveImportRelPath(imp)),
			"container_path": cty.StringVal(imp.ContainerPath()),
			"sources":        srcs,
		})
	}
	importsVal := cty.EmptyObjectVal
	if len(imports) > 0 {
		importsVal = cty.ObjectVal(imports)
	}

	qcVal := cty.NullVal(cty.EmptyObject)
	if cfg.QC != nil {
		criteria := cfg.QC.Criteria
		if criteria == cty.NilVal {
			criteria = cty.MapValEmpty(cty.String)
		}
		qcS := cfg.QC.Sandbox
		if qcS == cty.NilVal {
			qcS = cty.NullVal(cty.EmptyObject)
		}
		qcH := cfg.QC.Harness
		if qcH == cty.NilVal {
			qcH = cty.NullVal(cty.EmptyObject)
		}
		qcMP := cfg.QC.ModelProvider
		if qcMP == cty.NilVal {
			qcMP = cty.NullVal(cty.EmptyObject)
		}
		qcVal = cty.ObjectVal(map[string]cty.Value{
			"criteria":                   criteria,
			"sandbox":                    qcS,
			"harness":                    qcH,
			"model_provider":             qcMP,
			"model_provider_set":         cty.BoolVal(cfg.QC.ModelProviderSet),
			"model":                      cty.StringVal(cfg.QC.Model),
			"profile":                    cty.StringVal(cfg.QC.Profile),
			"reasoning_effort":           cty.StringVal(cfg.QC.ReasoningEffort),
			"permission":                 cty.StringVal(cfg.QC.Permission),
			"max_qc_rounds":              cty.NumberIntVal(int64(cfg.QC.MaxQCRounds)),
			"max_gatekeeper_reworks":     cty.NumberIntVal(int64(cfg.QC.MaxGatekeeperReworks)),
			"max_gatekeeper_reworks_set": cty.BoolVal(cfg.QC.MaxGatekeeperReworksSet),
			"gatekeepers":                encodeGatekeepers(cfg.QC.Gatekeepers),
			"tool_ids":                   stringListVal(cfg.QC.ToolIDs),
			"tool_call_quota":            intMapVal(cfg.QC.ToolCallQuota),
			"allowed_tools":              optionalStringListVal(cfg.QC.AllowedTools),
			"disallowed_tools":           optionalStringListVal(cfg.QC.DisallowedTools),
			"skill_directories":          stringListVal(cfg.QC.SkillDirectories),
			"skills":                     stringListVal(cfg.QC.Skills),
			"disabled_skills":            stringListVal(cfg.QC.DisabledSkills),
			"retry":                      encodeRetryOverride(cfg.QC.Retry),
		})
	}

	fallbacks := make([]cty.Value, len(cfg.Fallbacks))
	for i, fb := range cfg.Fallbacks {
		fallbacks[i] = cty.ObjectVal(map[string]cty.Value{
			"name":        cty.StringVal(fb.Name),
			"on":          stringListVal(fb.On),
			"target":      cty.StringVal(fb.Target),
			"max_reworks": cty.NumberIntVal(int64(fb.MaxReworks)),
			"artifacts":   stringListVal(fb.Artifacts),
		})
	}
	fallbacksVal := cty.ListValEmpty(cty.Object(map[string]cty.Type{
		"name": cty.String, "on": cty.List(cty.String), "target": cty.String,
		"max_reworks": cty.Number, "artifacts": cty.List(cty.String),
	}))
	if len(fallbacks) > 0 {
		fallbacksVal = cty.ListVal(fallbacks)
	}

	return cty.ObjectVal(map[string]cty.Value{
		"based_source":            cfg.BasedSource,
		"source_url":              cty.StringVal(cfg.SourceURL),
		"sandbox":                 sandboxVal,
		"harness":                 harnessVal,
		"model_provider":          providerVal,
		"model":                   cty.StringVal(cfg.Model),
		"profile":                 cty.StringVal(cfg.Profile),
		"reasoning_effort":        cty.StringVal(cfg.ReasoningEffort),
		"system_prompt":           cfg.SystemPrompt,
		"prompt":                  promptVal,
		"writable_paths":          stringListVal(cfg.WritablePaths),
		"human_approval":          cty.StringVal(cfg.HumanApproval),
		"checkpoint_every_rounds": cty.NumberIntVal(int64(cfg.CheckpointRounds)),
		"permission":              cty.StringVal(cfg.Permission),
		"max_protocol_attempts":   cty.NumberIntVal(int64(cfg.MaxProtocolAttempts)),
		"timeout_nanoseconds":     optionalDurationPtrVal(cfg.Timeout),
		"timeout_raw":             cty.StringVal(cfg.TimeoutRaw),
		"retry":                   encodeRetryOverride(cfg.Retry),
		"tool_ids":                stringListVal(cfg.ToolIDs),
		"mcp_tool_ids":            stringListVal(cfg.MCPToolIDs),
		"mcp_resource_ids":        stringListVal(cfg.MCPResourceIDs),
		"terminate_tool_id":       cty.StringVal(cfg.TerminateToolID),
		"tool_call_quota":         intMapVal(cfg.ToolCallQuota),
		"allowed_tools":           optionalStringListVal(cfg.AllowedTools),
		"disallowed_tools":        optionalStringListVal(cfg.DisallowedTools),
		"skill_directories":       stringListVal(cfg.SkillDirectories),
		"skills":                  stringListVal(cfg.Skills),
		"disabled_skills":         stringListVal(cfg.DisabledSkills),
		"tool_uses":               toolUsesVal,
		"artifacts":               artifactsVal,
		"imports":                 importsVal,
		"qc":                      qcVal,
		"fallbacks":               fallbacksVal,
		"deferred_expression":     cty.StringVal(cfg.DeferredExpression),
		"each":                    eachVal,
	})
}

func encodeDynamicWorkerConfig(cfg workerspec.DynamicConfig) cty.Value {
	forEachVal := cfg.ForEach
	if forEachVal == cty.NilVal {
		forEachVal = cty.DynamicVal
	}
	return cty.ObjectVal(map[string]cty.Value{
		"name":             cty.StringVal(cfg.Name),
		"parallelism":      optionalIntPtrVal(cfg.Parallelism),
		"serial":           cty.BoolVal(cfg.Serial),
		"for_each":         forEachVal,
		"for_each_expr":    cty.StringVal(cfg.ForEachExpr),
		"iterator":         cty.StringVal(cfg.IteratorName),
		"content":          encodeWorkerConfig(cfg.Content, cty.NullVal(cty.EmptyObject)),
		"content_template": cty.StringVal(cfg.ContentTemplate),
	})
}

func encodeMergeConfig(cfg mergespec.Config, eachVal cty.Value) cty.Value {
	if eachVal == cty.NilVal {
		eachVal = cty.NullVal(cty.EmptyObject)
	}
	sandboxVal := cfg.Sandbox
	if sandboxVal == cty.NilVal {
		sandboxVal = cty.NullVal(cty.EmptyObject)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"name":              cty.StringVal(cfg.Name),
		"based_source":      cfg.BasedSource,
		"based_source_expr": cty.StringVal(cfg.BasedSourceExpr),
		"source_url":        cty.StringVal(cfg.SourceURL),
		"target_branch":     optionalStringPtrVal(cfg.TargetBranch),
		"sources":           cfg.Sources,
		"sources_expr":      cty.StringVal(cfg.SourcesExpr),
		"strategy":          cty.StringVal(cfg.Strategy),
		"max_reworks":       cty.NumberIntVal(int64(cfg.MaxReworks)),
		"human_approval":    cty.StringVal(cfg.HumanApproval),
		"sandbox":           sandboxVal,
		"gatekeepers":       encodeGatekeepers(cfg.Gatekeepers),
		"each":              eachVal,
	})
}

func encodeModuleNodeConfig(mp modulespec.ModulePlan, eachVal cty.Value) cty.Value {
	if eachVal == cty.NilVal {
		eachVal = cty.NullVal(cty.EmptyObject)
	}
	inputs := cty.EmptyObjectVal
	if len(mp.Inputs) > 0 {
		inputs = cty.ObjectVal(mp.Inputs)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"source":            cty.StringVal(mp.Source),
		"canonical_address": cty.StringVal(mp.CanonicalAddress),
		"bundle_path":       cty.StringVal(mp.BundlePath),
		"parallelism":       optionalIntPtrVal(mp.Parallelism),
		"timeout_raw":       cty.StringVal(mp.TimeoutRaw),
		"inputs":            inputs,
		"input_exprs":       stringMapVal(mp.InputExprs),
		"each":              eachVal,
	})
}

func encodeGatekeepers(gatekeepers []workerspec.GatekeeperConfig) cty.Value {
	gkType := cty.Object(map[string]cty.Type{
		"name":                   cty.String,
		"command":                cty.List(cty.String),
		"working_dir":            cty.String,
		"timeout":                cty.String,
		"expect_exit_code":       cty.Number,
		"require_clean_worktree": cty.Bool,
	})
	if len(gatekeepers) == 0 {
		return cty.ListValEmpty(gkType)
	}
	items := make([]cty.Value, len(gatekeepers))
	for i, gk := range gatekeepers {
		items[i] = cty.ObjectVal(map[string]cty.Value{
			"name":                   cty.StringVal(gk.Name),
			"command":                stringListVal(gk.Command),
			"working_dir":            cty.StringVal(gk.WorkingDir),
			"timeout":                cty.StringVal(gk.Timeout),
			"expect_exit_code":       cty.NumberIntVal(int64(gk.ExpectExitCode)),
			"require_clean_worktree": cty.BoolVal(gk.RequireCleanTree),
		})
	}
	return cty.ListVal(items)
}

func encodeRetryOverride(r providerspec.RetryOverride) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"lifecycle_retries":        optionalIntPtrVal(r.LifecycleRetries),
		"model_call_retries":       optionalIntPtrVal(r.ModelCallRetries),
		"interval_nanoseconds":     optionalDurationPtrVal(r.Interval),
		"max_interval_nanoseconds": optionalDurationPtrVal(r.MaxInterval),
		"error_message_regex":      stringListVal(r.ErrorMessageRegex),
	})
}

func encodeConditions(conds []corespec.Condition) cty.Value {
	condType := cty.Object(map[string]cty.Type{
		"expression":    cty.String,
		"error_message": cty.String,
	})
	if len(conds) == 0 {
		return cty.ListValEmpty(condType)
	}
	vals := make([]cty.Value, len(conds))
	for i, cond := range conds {
		vals[i] = cty.ObjectVal(map[string]cty.Value{
			"expression":    cty.StringVal(cond.Expression),
			"error_message": cty.StringVal(cond.ErrorMessage),
		})
	}
	return cty.ListVal(vals)
}

func optionalStringPtrVal(v *string) cty.Value {
	if v == nil {
		return cty.NullVal(cty.String)
	}
	return cty.StringVal(*v)
}

func optionalIntPtrVal(v *int) cty.Value {
	if v == nil {
		return cty.NullVal(cty.Number)
	}
	return cty.NumberIntVal(int64(*v))
}

func optionalDurationPtrVal(d *time.Duration) cty.Value {
	if d == nil {
		return cty.NullVal(cty.Number)
	}
	return cty.NumberIntVal(int64(*d))
}

func stringListVal(items []string) cty.Value {
	if len(items) == 0 {
		return cty.ListValEmpty(cty.String)
	}
	vals := make([]cty.Value, len(items))
	for i, s := range items {
		vals[i] = cty.StringVal(s)
	}
	return cty.ListVal(vals)
}

func optionalStringListVal(items []string) cty.Value {
	if items == nil {
		return cty.NullVal(cty.List(cty.String))
	}
	return stringListVal(items)
}

func stringMapVal(m map[string]string) cty.Value {
	if len(m) == 0 {
		return cty.MapValEmpty(cty.String)
	}
	vals := make(map[string]cty.Value, len(m))
	for k, v := range m {
		vals[k] = cty.StringVal(v)
	}
	return cty.MapVal(vals)
}

func intMapVal(m map[string]int) cty.Value {
	if len(m) == 0 {
		return cty.MapValEmpty(cty.Number)
	}
	vals := make(map[string]cty.Value, len(m))
	for k, v := range m {
		vals[k] = cty.NumberIntVal(int64(v))
	}
	return cty.MapVal(vals)
}

func blockOrigin(block *golden.HclBlock, moduleDir string) plan.Origin {
	if block == nil || block.Block == nil {
		return plan.Origin{}
	}
	rangeVal := block.Block.Range()
	filename := filepath.ToSlash(rangeVal.Filename)
	if moduleDir != "" {
		if rel, err := filepath.Rel(moduleDir, rangeVal.Filename); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			filename = filepath.ToSlash(rel)
		}
	}
	source := ""
	if raw, err := os.ReadFile(rangeVal.Filename); err == nil {
		start := max(0, rangeVal.Start.Byte)
		end := min(len(raw), rangeVal.End.Byte)
		if start < end {
			source = strings.TrimSpace(string(raw[start:end]))
		}
	}
	return plan.Origin{
		Filename:    filename,
		StartLine:   rangeVal.Start.Line,
		StartColumn: rangeVal.Start.Column,
		EndLine:     rangeVal.End.Line,
		EndColumn:   rangeVal.End.Column,
		Source:      source,
	}
}

func savedExpressionSource(attribute *golden.HclAttribute) string {
	source := attribute.ExprString()
	for _, token := range attribute.ExprTokens() {
		if token.Type == hclsyntax.TokenOHeredoc {
			return source + "\n"
		}
	}
	return source
}
