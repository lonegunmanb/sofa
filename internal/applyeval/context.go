package applyeval

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// SourceRuntimeState captures the runtime resolution of a source block.
type SourceRuntimeState struct {
	Address   string
	Name      string
	URL       string
	Branch    string
	CommitSHA string
}

// ArtifactRuntimeState captures a published runtime artifact produced by a worker.
type ArtifactRuntimeState struct {
	ID            string
	Name          string
	Type          string
	RelPath       string
	ContainerPath string
	Description   string
	Required      bool
	NonEmpty      bool
}

// WorkerRuntimeResult captures the completed runtime outputs of a worker node.
type WorkerRuntimeResult struct {
	Address    string
	Name       string
	WorkBranch string
	CommitSHA  string
	Result     *string
	Artifacts  map[string]ArtifactRuntimeState
}

// DynamicWorkerRuntimeResult captures the completed runtime outputs of a dynamic_worker node.
type DynamicWorkerRuntimeResult struct {
	Address   string
	Name      string
	Keys      []string
	Instances map[string]WorkerRuntimeResult
}

// MergeRuntimeResult captures the completed runtime outputs of a merge node.
type MergeRuntimeResult struct {
	Address    string
	Name       string
	WorkBranch string
	CommitSHA  string
}

// ModuleRuntimeResult captures the evaluated outputs of a completed child module node.
type ModuleRuntimeResult struct {
	Address string
	Outputs map[string]cty.Value
}

// RuntimeState holds all completed node results injected into an Apply-time EvalContext.
type RuntimeState struct {
	Sources        map[string]SourceRuntimeState
	Workers        map[string]WorkerRuntimeResult
	DynamicWorkers map[string]DynamicWorkerRuntimeResult
	Merges         map[string]MergeRuntimeResult
	Modules        map[string]ModuleRuntimeResult
}

type evalOptions struct {
	baseDir     string
	plan        *plan.Plan
	parentPlans []*plan.Plan
}

// EvalOption configures Apply-time context building and node evaluation.
type EvalOption func(*evalOptions)

// WithBaseDir overrides the basedir passed to config.Functions(basedir).
func WithBaseDir(dir string) EvalOption {
	return func(o *evalOptions) {
		o.baseDir = dir
	}
}

// WithPlan provides the owning Plan for SourceURL and local traversal resolution.
func WithPlan(p *plan.Plan) EvalOption {
	return func(o *evalOptions) {
		o.plan = p
	}
}

// WithParentPlans supplies ancestor module Plans (outermost to immediate parent)
// so inherited singleton sandbox, harness, and model_provider values are visible.
func WithParentPlans(parents ...*plan.Plan) EvalOption {
	return func(o *evalOptions) {
		for _, p := range parents {
			if p != nil {
				o.parentPlans = append(o.parentPlans, p)
			}
		}
	}
}

func resolveEvalOptions(planned *plan.Plan, opts []EvalOption) evalOptions {
	cfg := evalOptions{plan: planned}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.baseDir == "" && cfg.plan != nil {
		cfg.baseDir = cfg.plan.Directory()
	}
	return cfg
}

var artifactValueType = cty.Object(map[string]cty.Type{
	"id":          cty.String,
	"name":        cty.String,
	"kind":        cty.String,
	"type":        cty.String,
	"path":        cty.String,
	"rel_path":    cty.String,
	"description": cty.String,
	"required":    cty.Bool,
	"non_empty":   cty.Bool,
})

// BuildEvalContext constructs an *hcl.EvalContext strictly from an immutable *plan.Plan
// and completed RuntimeState without reading any host .sofa.hcl files.
func BuildEvalContext(planned *plan.Plan, state RuntimeState, opts ...EvalOption) (*hcl.EvalContext, error) {
	if planned == nil {
		return nil, fmt.Errorf("applyeval: plan is nil")
	}
	cfg := resolveEvalOptions(planned, opts)

	vars := make(map[string]cty.Value)
	for k, v := range planned.Context() {
		if v != cty.NilVal {
			vars[k] = v
		}
	}
	if _, ok := vars["var"]; !ok {
		vars["var"] = cty.EmptyObjectVal
	}
	if _, ok := vars["local"]; !ok {
		vars["local"] = cty.EmptyObjectVal
	}
	if _, ok := vars["path"]; !ok {
		vars["path"] = cty.ObjectVal(map[string]cty.Value{
			"module": cty.StringVal(planned.Directory()),
		})
	}

	byKind := make(map[string]map[string]cty.Value)
	indexedByKind := make(map[string]map[string]map[string]cty.Value)
	completedNodes := make(map[string]bool)

	for _, node := range planned.Nodes() {
		kind, name, key, hasKey := parseNodeAddress(node.Address)
		if kind == "" || name == "" {
			continue
		}
		var val cty.Value
		var completed bool
		switch node.Kind {
		case "source":
			sState, ok := state.Sources[node.Address]
			completed = ok
			val = buildSourceValue(node, sState, ok)
		case "preserve":
			val = buildPreserveValue(node)
			completed = true
		case "sandbox":
			val = buildSandboxValue(node)
			completed = true
		case "harness":
			val = buildHarnessValue(node)
			completed = true
		case "model_provider":
			val = buildModelProviderValue(node)
			completed = true
		case "mcp_server":
			val = buildMCPServerValue(node)
			completed = true
		case "go_tool":
			val = buildGoToolValue(node)
			completed = true
		case "external_tool":
			val = buildExternalToolValue(node)
			completed = true
		case "worker":
			wRes, ok := state.Workers[node.Address]
			completed = ok
			val = buildWorkerValue(node, wRes, ok)
		case "dynamic_worker":
			dRes, ok := state.DynamicWorkers[node.Address]
			completed = ok
			val = buildDynamicWorkerValue(node, dRes, ok)
		case "merge":
			mRes, ok := state.Merges[node.Address]
			completed = ok
			val = buildMergeValue(node, mRes, ok)
		case "module":
			modRes, ok := state.Modules[node.Address]
			completed = ok
			val = buildModuleValue(node, modRes, ok)
		default:
			continue
		}
		if completed {
			completedNodes[node.Address] = true
		}
		insertBlockValue(byKind, indexedByKind, kind, name, key, hasKey, val)
	}

	injectParentInheritedSingletons(planned, cfg.parentPlans, byKind)

	for _, kind := range []string{
		"source", "preserve", "sandbox", "harness", "model_provider",
		"mcp_server", "go_tool", "external_tool", "worker", "dynamic_worker",
		"merge", "module",
	} {
		merged := mergeKindValues(byKind[kind], indexedByKind[kind])
		if len(merged) == 0 {
			vars[kind] = cty.EmptyObjectVal
		} else {
			vars[kind] = cty.ObjectVal(merged)
		}
	}

	ctx := &hcl.EvalContext{
		Variables: vars,
		Functions: config.Functions(cfg.baseDir),
	}

	if err := evaluateLocalsTopologically(ctx, planned, completedNodes); err != nil {
		return nil, err
	}
	return ctx, nil
}

// SourceRuntimeValue constructs the runtime cty.Object for a source node.
func SourceRuntimeValue(node plan.NodeSpec, state SourceRuntimeState) cty.Value {
	return buildSourceValue(node, state, true)
}

// WorkerRuntimeValue constructs the runtime cty.Object for a completed worker node.
func WorkerRuntimeValue(node plan.NodeSpec, res WorkerRuntimeResult) cty.Value {
	return buildWorkerValue(node, res, true)
}

// DynamicWorkerRuntimeValue constructs the runtime cty.Object for a completed dynamic_worker node.
func DynamicWorkerRuntimeValue(node plan.NodeSpec, res DynamicWorkerRuntimeResult) cty.Value {
	return buildDynamicWorkerValue(node, res, true)
}

// MergeRuntimeValue constructs the runtime cty.Object for a completed merge node.
func MergeRuntimeValue(node plan.NodeSpec, res MergeRuntimeResult) cty.Value {
	return buildMergeValue(node, res, true)
}

// ModuleRuntimeValue constructs the runtime cty.Object for a completed module's evaluated outputs.
func ModuleRuntimeValue(outputs map[string]cty.Value) cty.Value {
	if len(outputs) == 0 {
		return cty.EmptyObjectVal
	}
	copied := make(map[string]cty.Value, len(outputs))
	for k, v := range outputs {
		if v != cty.NilVal {
			copied[k] = v
		}
	}
	if len(copied) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(copied)
}

func parseNodeAddress(addr string) (kind, name, key string, hasKey bool) {
	dot := strings.IndexByte(addr, '.')
	if dot <= 0 || dot+1 >= len(addr) {
		return "", "", "", false
	}
	kind = addr[:dot]
	rest := addr[dot+1:]
	if strings.HasSuffix(rest, "]") {
		if idx := strings.IndexByte(rest, '['); idx > 0 {
			return kind, rest[:idx], rest[idx+1 : len(rest)-1], true
		}
	}
	return kind, rest, "", false
}

func insertBlockValue(
	byKind map[string]map[string]cty.Value,
	indexedByKind map[string]map[string]map[string]cty.Value,
	kind, name, key string,
	hasKey bool,
	val cty.Value,
) {
	if !hasKey {
		if byKind[kind] == nil {
			byKind[kind] = make(map[string]cty.Value)
		}
		byKind[kind][name] = val
		return
	}
	if indexedByKind[kind] == nil {
		indexedByKind[kind] = make(map[string]map[string]cty.Value)
	}
	if indexedByKind[kind][name] == nil {
		indexedByKind[kind][name] = make(map[string]cty.Value)
	}
	indexedByKind[kind][name][key] = val
}

func mergeKindValues(direct map[string]cty.Value, indexed map[string]map[string]cty.Value) map[string]cty.Value {
	if len(direct) == 0 && len(indexed) == 0 {
		return nil
	}
	out := make(map[string]cty.Value, len(direct)+len(indexed))
	maps.Copy(out, direct)
	for name, instances := range indexed {
		if len(instances) == 0 {
			out[name] = cty.EmptyObjectVal
		} else {
			out[name] = cty.ObjectVal(instances)
		}
	}
	return out
}

func injectParentInheritedSingletons(
	planned *plan.Plan,
	parentPlans []*plan.Plan,
	byKind map[string]map[string]cty.Value,
) {
	if len(parentPlans) == 0 {
		return
	}
	localCounts := map[string]int{}
	for _, node := range planned.Nodes() {
		switch node.Kind {
		case "sandbox", "harness", "model_provider":
			localCounts[node.Kind]++
		}
	}

	type inheritedEntry struct {
		status int // 0=none, 1=unique, 2=ambiguous
		name   string
		val    cty.Value
	}
	dims := []string{"sandbox", "harness", "model_provider"}
	inherited := make(map[string]inheritedEntry, len(dims))

	for _, p := range parentPlans {
		if p == nil {
			continue
		}
		grouped := map[string][]plan.NodeSpec{}
		for _, n := range p.Nodes() {
			switch n.Kind {
			case "sandbox", "harness", "model_provider":
				grouped[n.Kind] = append(grouped[n.Kind], n)
			}
		}
		for _, dim := range dims {
			nodes := grouped[dim]
			switch len(nodes) {
			case 0:
				// Preserve inherited singleton from outer ancestor.
			case 1:
				n := nodes[0]
				_, name, _, hasKey := parseNodeAddress(n.Address)
				if hasKey || name == "" {
					inherited[dim] = inheritedEntry{status: 2}
					continue
				}
				var val cty.Value
				switch dim {
				case "sandbox":
					val = buildSandboxValue(n)
				case "harness":
					val = buildHarnessValue(n)
				case "model_provider":
					val = buildModelProviderValue(n)
				}
				inherited[dim] = inheritedEntry{status: 1, name: name, val: val}
			default:
				inherited[dim] = inheritedEntry{status: 2}
			}
		}
	}

	for _, dim := range dims {
		if localCounts[dim] != 0 {
			continue
		}
		entry := inherited[dim]
		if entry.status != 1 || entry.name == "" || entry.val == cty.NilVal {
			continue
		}
		if byKind[dim] == nil {
			byKind[dim] = make(map[string]cty.Value)
		}
		if _, exists := byKind[dim][entry.name]; !exists {
			byKind[dim][entry.name] = entry.val
		}
	}
}

func nodeCanonicalOrAddress(node plan.NodeSpec) string {
	if strings.TrimSpace(node.CanonicalAddress) != "" {
		return node.CanonicalAddress
	}
	return node.Address
}

func nodeSimpleName(node plan.NodeSpec, fallback string) string {
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	_, name, _, _ := parseNodeAddress(node.Address)
	return name
}

func buildSourceValue(node plan.NodeSpec, state SourceRuntimeState, completed bool) cty.Value {
	addr := state.Address
	if addr == "" {
		addr = node.Address
	}
	name := nodeSimpleName(node, state.Name)
	url := state.URL
	if url == "" {
		url = readStringAttr(node.Config, "url")
	}
	branch := state.Branch
	if branch == "" {
		branch = readStringAttr(node.Config, "branch")
	}
	commitVal := cty.UnknownVal(cty.String)
	if completed {
		commitVal = cty.StringVal(state.CommitSHA)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address":    cty.StringVal(addr),
		"kind":       cty.StringVal("source"),
		"name":       cty.StringVal(name),
		"url":        cty.StringVal(url),
		"branch":     cty.StringVal(branch),
		"commit_sha": commitVal,
	})
}

func buildPreserveValue(node plan.NodeSpec) cty.Value {
	name := nodeSimpleName(node, "")
	branchVal := cty.UnknownVal(cty.String)
	if hasAttr(node.Config, "branch") {
		branchVal = node.Config.GetAttr("branch")
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address":     cty.StringVal(node.Address),
		"kind":        cty.StringVal("preserve"),
		"name":        cty.StringVal(name),
		"url":         cty.StringVal(readStringAttr(node.Config, "url")),
		"branch":      branchVal,
		"branch_expr": cty.StringVal(readStringAttr(node.Config, "branch_expr")),
	})
}

func buildSandboxValue(node plan.NodeSpec) cty.Value {
	name := nodeSimpleName(node, "")
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(nodeCanonicalOrAddress(node)),
		"kind":    cty.StringVal("sandbox"),
		"name":    cty.StringVal(name),
		"runtime": cty.StringVal(readStringAttr(node.Config, "runtime")),
		"image":   cty.StringVal(readStringAttr(node.Config, "image")),
		"network": cty.StringVal(readStringAttr(node.Config, "network")),
	})
}

func buildHarnessValue(node plan.NodeSpec) cty.Value {
	name := nodeSimpleName(node, readStringAttr(node.Config, "name"))
	sourceVal := cty.NullVal(cty.String)
	if hasAttr(node.Config, "source") {
		sourceVal = node.Config.GetAttr("source")
	}
	versionVal := cty.NullVal(cty.String)
	if hasAttr(node.Config, "version") {
		versionVal = node.Config.GetAttr("version")
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(nodeCanonicalOrAddress(node)),
		"kind":    cty.StringVal("harness"),
		"name":    cty.StringVal(name),
		"source":  sourceVal,
		"version": versionVal,
	})
}

func buildModelProviderValue(node plan.NodeSpec) cty.Value {
	name := nodeSimpleName(node, "")
	policy := provider.DefaultRetryPolicy()
	if hasAttr(node.Config, "retry") {
		override := decodeRetryOverrideFromPlanConfig(node.Config.GetAttr("retry"))
		if merged, err := provider.MergeRetry(policy, override); err == nil {
			policy = merged
		}
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(nodeCanonicalOrAddress(node)),
		"kind":    cty.StringVal("model_provider"),
		"name":    cty.StringVal(name),
		"retry": cty.ObjectVal(map[string]cty.Value{
			"lifecycle_retries":    cty.NumberIntVal(int64(policy.LifecycleRetries)),
			"model_call_retries":   cty.NumberIntVal(int64(policy.ModelCallRetries)),
			"interval_seconds":     cty.NumberIntVal(int64(policy.Interval / time.Second)),
			"max_interval_seconds": cty.NumberIntVal(int64(policy.MaxInterval / time.Second)),
			"error_message_regex":  stringListCty(policy.ErrorMessageRegex),
		}),
	})
}

func buildMCPServerValue(node plan.NodeSpec) cty.Value {
	name := nodeSimpleName(node, readStringAttr(node.Config, "name"))
	runtimeName := readStringAttr(node.Config, "runtime_name")
	if runtimeName == "" {
		runtimeName = nodeCanonicalOrAddress(node)
	}
	tools := readStringSliceAttr(node.Config, "tools")
	resources := readStringSliceAttr(node.Config, "resources")

	toolIDs := make(map[string]cty.Value, len(tools))
	for _, tool := range tools {
		toolIDs[tool] = cty.StringVal(mcpToolIDFor(runtimeName, name, tool))
	}
	resourceIDs := make(map[string]cty.Value, len(resources))
	for _, res := range resources {
		resourceIDs[res] = cty.StringVal(mcpResourceIDFor(runtimeName, name, res))
	}

	toolMapVal := cty.MapValEmpty(cty.String)
	if len(toolIDs) > 0 {
		toolMapVal = cty.MapVal(toolIDs)
	}
	resourceMapVal := cty.MapValEmpty(cty.String)
	if len(resourceIDs) > 0 {
		resourceMapVal = cty.MapVal(resourceIDs)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address":      cty.StringVal(node.Address),
		"kind":         cty.StringVal("mcp_server"),
		"name":         cty.StringVal(name),
		"tool_ids":     toolMapVal,
		"resource_ids": resourceMapVal,
	})
}

func buildGoToolValue(node plan.NodeSpec) cty.Value {
	desc := cty.StringVal("")
	if hasAttr(node.Config, "description") {
		desc = node.Config.GetAttr("description")
	}
	src := cty.StringVal("")
	if hasAttr(node.Config, "source") {
		src = node.Config.GetAttr("source")
	}
	deps := cty.ListValEmpty(cty.String)
	if hasAttr(node.Config, "dependencies") {
		deps = node.Config.GetAttr("dependencies")
	}
	return cty.ObjectVal(map[string]cty.Value{
		"id":           cty.StringVal(readStringAttr(node.Config, "id")),
		"address":      cty.StringVal(node.Address),
		"kind":         cty.StringVal("go"),
		"description":  desc,
		"source":       src,
		"dependencies": deps,
	})
}

func buildExternalToolValue(node plan.NodeSpec) cty.Value {
	desc := cty.StringVal("")
	if hasAttr(node.Config, "description") {
		desc = node.Config.GetAttr("description")
	}
	prog := cty.ListValEmpty(cty.String)
	if hasAttr(node.Config, "program") {
		prog = node.Config.GetAttr("program")
	}
	wd := cty.StringVal("")
	if hasAttr(node.Config, "working_dir") {
		wd = node.Config.GetAttr("working_dir")
	}
	return cty.ObjectVal(map[string]cty.Value{
		"id":          cty.StringVal(readStringAttr(node.Config, "id")),
		"address":     cty.StringVal(node.Address),
		"kind":        cty.StringVal("external"),
		"description": desc,
		"program":     prog,
		"working_dir": wd,
	})
}

func buildWorkerValue(node plan.NodeSpec, res WorkerRuntimeResult, completed bool) cty.Value {
	addr := res.Address
	if addr == "" {
		addr = node.Address
	}
	name := nodeSimpleName(node, res.Name)

	workBranchVal := cty.UnknownVal(cty.String)
	commitSHAVal := cty.UnknownVal(cty.String)
	if completed {
		workBranchVal = cty.StringVal(res.WorkBranch)
		commitSHAVal = cty.StringVal(res.CommitSHA)
	}

	artifactsObj := buildWorkerArtifactsObject(node.Config, res.Artifacts, completed)

	attrs := map[string]cty.Value{
		"address":     cty.StringVal(addr),
		"kind":        cty.StringVal("worker"),
		"name":        cty.StringVal(name),
		"work_branch": workBranchVal,
		"commit_sha":  commitSHAVal,
		"artifact":    artifactsObj,
	}

	hasTerm := workerConfigHasTerminatingTool(node.Config)
	if hasTerm || res.Result != nil {
		if completed && res.Result != nil {
			attrs["result"] = cty.StringVal(*res.Result)
		} else if completed {
			attrs["result"] = cty.StringVal("")
		} else {
			attrs["result"] = cty.UnknownVal(cty.String)
		}
	}
	return cty.ObjectVal(attrs)
}

func buildDynamicWorkerValue(node plan.NodeSpec, res DynamicWorkerRuntimeResult, completed bool) cty.Value {
	addr := res.Address
	if addr == "" {
		addr = node.Address
	}
	name := nodeSimpleName(node, res.Name)

	var contentCfg cty.Value
	if hasAttr(node.Config, "content") {
		contentCfg = node.Config.GetAttr("content")
	}
	itemType, hasResult := dynamicWorkerItemType(contentCfg)

	if !completed {
		return cty.ObjectVal(map[string]cty.Value{
			"address":       cty.StringVal(addr),
			"kind":          cty.StringVal("dynamic_worker"),
			"name":          cty.StringVal(name),
			"workers":       cty.UnknownVal(cty.Map(itemType)),
			"work_branches": cty.UnknownVal(cty.List(cty.String)),
		})
	}

	keys := slices.Clone(res.Keys)
	if keys == nil && len(res.Instances) > 0 {
		keys = make([]string, 0, len(res.Instances))
		for k := range res.Instances {
			keys = append(keys, k)
		}
		slices.Sort(keys)
	}

	if len(keys) == 0 {
		return cty.ObjectVal(map[string]cty.Value{
			"address":       cty.StringVal(addr),
			"kind":          cty.StringVal("dynamic_worker"),
			"name":          cty.StringVal(name),
			"workers":       cty.MapValEmpty(itemType),
			"work_branches": cty.ListValEmpty(cty.String),
		})
	}

	workersMap := make(map[string]cty.Value, len(keys))
	branches := make([]cty.Value, 0, len(keys))
	for _, k := range keys {
		inst := res.Instances[k]
		branches = append(branches, cty.StringVal(inst.WorkBranch))

		itemAttrs := map[string]cty.Value{
			"work_branch": cty.StringVal(inst.WorkBranch),
			"commit_sha":  cty.StringVal(inst.CommitSHA),
			"artifact":    buildWorkerArtifactsObject(contentCfg, inst.Artifacts, true),
		}
		if hasResult {
			if inst.Result != nil {
				itemAttrs["result"] = cty.StringVal(*inst.Result)
			} else {
				itemAttrs["result"] = cty.StringVal("")
			}
		}
		workersMap[k] = cty.ObjectVal(itemAttrs)
	}

	return cty.ObjectVal(map[string]cty.Value{
		"address":       cty.StringVal(addr),
		"kind":          cty.StringVal("dynamic_worker"),
		"name":          cty.StringVal(name),
		"workers":       cty.MapVal(workersMap),
		"work_branches": cty.ListVal(branches),
	})
}

func buildMergeValue(node plan.NodeSpec, res MergeRuntimeResult, completed bool) cty.Value {
	addr := res.Address
	if addr == "" {
		addr = node.Address
	}
	name := nodeSimpleName(node, res.Name)
	workBranch := cty.UnknownVal(cty.String)
	commitSHA := cty.UnknownVal(cty.String)
	if completed {
		workBranch = cty.StringVal(res.WorkBranch)
		commitSHA = cty.StringVal(res.CommitSHA)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address":     cty.StringVal(addr),
		"kind":        cty.StringVal("merge"),
		"name":        cty.StringVal(name),
		"work_branch": workBranch,
		"commit_sha":  commitSHA,
	})
}

func buildModuleValue(node plan.NodeSpec, res ModuleRuntimeResult, completed bool) cty.Value {
	if completed {
		return ModuleRuntimeValue(res.Outputs)
	}
	if node.Module == nil || node.Module.Plan == nil {
		return cty.EmptyObjectVal
	}
	planOutputs := node.Module.Plan.Outputs()
	if len(planOutputs) == 0 {
		return cty.EmptyObjectVal
	}
	outMap := make(map[string]cty.Value, len(planOutputs))
	for k, out := range planOutputs {
		val := out.Value
		if val == cty.NilVal {
			val = cty.DynamicVal
		}
		outMap[k] = val
	}
	return cty.ObjectVal(outMap)
}

func dynamicWorkerItemType(contentCfg cty.Value) (cty.Type, bool) {
	artType := cty.EmptyObject
	if hasAttr(contentCfg, "artifacts") {
		artsVal := contentCfg.GetAttr("artifacts")
		unmarked, _ := artsVal.UnmarkDeep()
		if unmarked != cty.NilVal && unmarked.IsKnown() && !unmarked.IsNull() &&
			(unmarked.Type().IsObjectType() || unmarked.Type().IsMapType()) && unmarked.LengthInt() > 0 {
			artAttrs := make(map[string]cty.Type, unmarked.LengthInt())
			for name := range unmarked.AsValueMap() {
				artAttrs[name] = artifactValueType
			}
			artType = cty.Object(artAttrs)
		}
	}
	itemAttrs := map[string]cty.Type{
		"work_branch": cty.String,
		"commit_sha":  cty.String,
		"artifact":    artType,
	}
	hasResult := workerConfigHasTerminatingTool(contentCfg)
	if hasResult {
		itemAttrs["result"] = cty.String
	}
	return cty.Object(itemAttrs), hasResult
}

func buildWorkerArtifactsObject(workerCfg cty.Value, runtimeArtifacts map[string]ArtifactRuntimeState, completed bool) cty.Value {
	declared := extractPlannedArtifacts(workerCfg)
	if len(declared) == 0 && len(runtimeArtifacts) == 0 {
		return cty.EmptyObjectVal
	}
	out := make(map[string]cty.Value, len(declared)+len(runtimeArtifacts))
	for name, art := range declared {
		idVal := cty.UnknownVal(cty.String)
		if completed {
			idVal = cty.StringVal("")
		}
		rt, hasRT := runtimeArtifacts[name]
		if hasRT {
			if rt.ID != "" {
				idVal = cty.StringVal(rt.ID)
			}
			if rt.Type != "" {
				art.Type = rt.Type
			}
			if rt.RelPath != "" {
				art.Path = rt.RelPath
			}
			if rt.Description != "" {
				art.Description = rt.Description
			}
		}
		containerPath := art.ContainerPath()
		if hasRT && rt.ContainerPath != "" {
			containerPath = rt.ContainerPath
		}
		out[name] = cty.ObjectVal(map[string]cty.Value{
			"id":          idVal,
			"name":        cty.StringVal(art.Name),
			"kind":        cty.StringVal("artifact"),
			"type":        cty.StringVal(art.Type),
			"path":        cty.StringVal(containerPath),
			"rel_path":    cty.StringVal(art.Path),
			"description": cty.StringVal(art.Description),
			"required":    cty.BoolVal(art.Required),
			"non_empty":   cty.BoolVal(art.NonEmpty),
		})
	}
	for name, rt := range runtimeArtifacts {
		if _, exists := out[name]; exists {
			continue
		}
		containerPath := rt.ContainerPath
		if containerPath == "" {
			containerPath = workerspec.Artifact{Name: name, Path: rt.RelPath}.ContainerPath()
		}
		out[name] = cty.ObjectVal(map[string]cty.Value{
			"id":          cty.StringVal(rt.ID),
			"name":        cty.StringVal(name),
			"kind":        cty.StringVal("artifact"),
			"type":        cty.StringVal(rt.Type),
			"path":        cty.StringVal(containerPath),
			"rel_path":    cty.StringVal(rt.RelPath),
			"description": cty.StringVal(rt.Description),
			"required":    cty.BoolVal(rt.Required),
			"non_empty":   cty.BoolVal(rt.NonEmpty),
		})
	}
	if len(out) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(out)
}

func extractPlannedArtifacts(workerCfg cty.Value) map[string]workerspec.Artifact {
	if !hasAttr(workerCfg, "artifacts") {
		return nil
	}
	artsVal := workerCfg.GetAttr("artifacts")
	unmarked, _ := artsVal.UnmarkDeep()
	if unmarked == cty.NilVal || unmarked.IsNull() || !unmarked.IsKnown() ||
		(!unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType()) {
		return nil
	}
	out := make(map[string]workerspec.Artifact, unmarked.LengthInt())
	for name, val := range unmarked.AsValueMap() {
		artName := readStringAttr(val, "name")
		if artName == "" {
			artName = name
		}
		out[name] = workerspec.Artifact{
			Name:        artName,
			Type:        readStringAttr(val, "type"),
			Path:        readStringAttr(val, "path"),
			Description: readStringAttr(val, "description"),
			Required:    readBoolAttr(val, "required", false),
			NonEmpty:    readBoolAttr(val, "non_empty", false),
		}
	}
	return out
}

func workerConfigHasTerminatingTool(workerCfg cty.Value) bool {
	if readStringAttr(workerCfg, "terminate_tool_id") != "" {
		return true
	}
	if hasAttr(workerCfg, "tool_uses") {
		tuVal := workerCfg.GetAttr("tool_uses")
		unmarked, _ := tuVal.UnmarkDeep()
		if unmarked != cty.NilVal && !unmarked.IsNull() && unmarked.IsKnown() && unmarked.CanIterateElements() {
			for it := unmarked.ElementIterator(); it.Next(); {
				_, elem := it.Element()
				if readBoolAttr(elem, "terminate", false) {
					return true
				}
			}
		}
	}
	return false
}

func evaluateLocalsTopologically(ctx *hcl.EvalContext, planned *plan.Plan, completedNodes map[string]bool) error {
	localExprs := planned.LocalExpressions()
	if len(localExprs) == 0 {
		return nil
	}

	localMap := map[string]cty.Value{}
	if existing, ok := ctx.Variables["local"]; ok && existing != cty.NilVal {
		unmarked, _ := existing.UnmarkDeep()
		if !unmarked.IsNull() && unmarked.IsKnown() && (unmarked.Type().IsObjectType() || unmarked.Type().IsMapType()) {
			localMap = existing.AsValueMap()
		}
	}

	parsed := make(map[string]hcl.Expression, len(localExprs))
	deps := make(map[string][]string, len(localExprs))
	for name, raw := range localExprs {
		expr, diags := hclsyntax.ParseExpression([]byte(raw), "local."+name, hcl.InitialPos)
		if diags.HasErrors() {
			return fmt.Errorf("parse local.%s: %w", name, diags)
		}
		parsed[name] = expr
		var localDeps []string
		seen := make(map[string]struct{})
		for _, tr := range expr.Variables() {
			if tr.RootName() == "local" && len(tr) >= 2 {
				if attr, ok := tr[1].(hcl.TraverseAttr); ok {
					if _, exists := localExprs[attr.Name]; exists {
						if _, dup := seen[attr.Name]; !dup {
							seen[attr.Name] = struct{}{}
							localDeps = append(localDeps, attr.Name)
						}
					}
				}
			}
		}
		sort.Strings(localDeps)
		deps[name] = localDeps
	}

	order, err := topoSortLocals(localExprs, deps)
	if err != nil {
		return err
	}

	for _, name := range order {
		if len(localMap) == 0 {
			ctx.Variables["local"] = cty.EmptyObjectVal
		} else {
			ctx.Variables["local"] = cty.ObjectVal(localMap)
		}
		expr := parsed[name]
		val, diags := expr.Value(ctx)
		if diags.HasErrors() {
			if hasUncompletedNodeDependency(expr, parsed, completedNodes, map[string]bool{}) {
				if _, hasPlanVal := localMap[name]; hasPlanVal {
					continue
				}
			}
			return fmt.Errorf("evaluate local.%s: %w", name, diags)
		}
		localMap[name] = val
	}

	if len(localMap) == 0 {
		ctx.Variables["local"] = cty.EmptyObjectVal
	} else {
		ctx.Variables["local"] = cty.ObjectVal(localMap)
	}
	return nil
}

func topoSortLocals(localExprs map[string]string, deps map[string][]string) ([]string, error) {
	names := make([]string, 0, len(localExprs))
	for name := range localExprs {
		names = append(names, name)
	}
	sort.Strings(names)

	state := make(map[string]uint8, len(names))
	order := make([]string, 0, len(names))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("applyeval: cyclic local dependency involving local.%s", name)
		case 2:
			return nil
		}
		state[name] = 1
		for _, dep := range deps[name] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		order = append(order, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func hasUncompletedNodeDependency(
	expr hcl.Expression,
	localExprs map[string]hcl.Expression,
	completedNodes map[string]bool,
	visitedLocals map[string]bool,
) bool {
	if expr == nil {
		return false
	}
	for _, tr := range expr.Variables() {
		root := tr.RootName()
		switch root {
		case "local":
			if len(tr) >= 2 {
				if attr, ok := tr[1].(hcl.TraverseAttr); ok && !visitedLocals[attr.Name] {
					visitedLocals[attr.Name] = true
					if subExpr, ok := localExprs[attr.Name]; ok {
						if hasUncompletedNodeDependency(subExpr, localExprs, completedNodes, visitedLocals) {
							return true
						}
					}
				}
			}
		case "source", "worker", "dynamic_worker", "merge", "module":
			if len(tr) >= 2 {
				if attr, ok := tr[1].(hcl.TraverseAttr); ok {
					addr := root + "." + attr.Name
					if len(tr) >= 3 {
						if idx, ok := tr[2].(hcl.TraverseIndex); ok {
							k, _ := idx.Key.UnmarkDeep()
							if k.IsKnown() && !k.IsNull() && k.Type().Equals(cty.String) {
								indexedAddr := addr + "[" + k.AsString() + "]"
								if !completedNodes[indexedAddr] {
									return true
								}
								continue
							}
						}
					}
					if !completedNodes[addr] {
						return true
					}
				}
			}
		}
	}
	return false
}

func decodeRetryOverrideFromPlanConfig(val cty.Value) provider.RetryOverride {
	unmarked, _ := val.UnmarkDeep()
	if unmarked == cty.NilVal || unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.Type().IsObjectType() {
		return provider.RetryOverride{}
	}
	var out provider.RetryOverride
	if v, ok := readOptionalIntAttr(unmarked, "lifecycle_retries"); ok {
		out.LifecycleRetries = &v
	}
	if v, ok := readOptionalIntAttr(unmarked, "model_call_retries"); ok {
		out.ModelCallRetries = &v
	}
	if v, ok := readOptionalInt64Attr(unmarked, "interval_nanoseconds"); ok {
		d := time.Duration(v)
		out.Interval = &d
	} else if v, ok := readOptionalInt64Attr(unmarked, "interval_seconds"); ok {
		d := time.Duration(v) * time.Second
		out.Interval = &d
	}
	if v, ok := readOptionalInt64Attr(unmarked, "max_interval_nanoseconds"); ok {
		d := time.Duration(v)
		out.MaxInterval = &d
	} else if v, ok := readOptionalInt64Attr(unmarked, "max_interval_seconds"); ok {
		d := time.Duration(v) * time.Second
		out.MaxInterval = &d
	}
	out.ErrorMessageRegex = readStringSliceAttr(unmarked, "error_message_regex")
	return out
}

func hasAttr(val cty.Value, attr string) bool {
	if val == cty.NilVal {
		return false
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked == cty.NilVal || unmarked.IsNull() || !unmarked.IsKnown() {
		return false
	}
	return unmarked.Type().IsObjectType() && unmarked.Type().HasAttribute(attr)
}

func readStringAttr(val cty.Value, attr string) string {
	if !hasAttr(val, attr) {
		return ""
	}
	raw, _ := val.GetAttr(attr).UnmarkDeep()
	if raw == cty.NilVal || raw.IsNull() || !raw.IsKnown() || !raw.Type().Equals(cty.String) {
		return ""
	}
	return raw.AsString()
}

func readBoolAttr(val cty.Value, attr string, fallback bool) bool {
	if !hasAttr(val, attr) {
		return fallback
	}
	raw, _ := val.GetAttr(attr).UnmarkDeep()
	if raw == cty.NilVal || raw.IsNull() || !raw.IsKnown() || !raw.Type().Equals(cty.Bool) {
		return fallback
	}
	return raw.True()
}

func readOptionalIntAttr(val cty.Value, attr string) (int, bool) {
	v, ok := readOptionalInt64Attr(val, attr)
	if !ok {
		return 0, false
	}
	return int(v), true
}

func readOptionalInt64Attr(val cty.Value, attr string) (int64, bool) {
	if !hasAttr(val, attr) {
		return 0, false
	}
	raw, _ := val.GetAttr(attr).UnmarkDeep()
	if raw == cty.NilVal || raw.IsNull() || !raw.IsKnown() || !raw.Type().Equals(cty.Number) {
		return 0, false
	}
	i64, _ := raw.AsBigFloat().Int64()
	return i64, true
}

func readStringSliceAttr(val cty.Value, attr string) []string {
	if !hasAttr(val, attr) {
		return nil
	}
	raw, _ := val.GetAttr(attr).UnmarkDeep()
	if raw == cty.NilVal || raw.IsNull() || !raw.IsKnown() || !raw.CanIterateElements() {
		return nil
	}
	out := make([]string, 0, raw.LengthInt())
	for it := raw.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if elem.IsNull() || !elem.IsKnown() || !elem.Type().Equals(cty.String) {
			continue
		}
		out = append(out, elem.AsString())
	}
	return out
}

func stringListCty(items []string) cty.Value {
	if len(items) == 0 {
		return cty.ListValEmpty(cty.String)
	}
	vals := make([]cty.Value, len(items))
	for i, s := range items {
		vals[i] = cty.StringVal(s)
	}
	return cty.ListVal(vals)
}

func convertPreservingMarks(val cty.Value, target cty.Type) (cty.Value, error) {
	if val == cty.NilVal || target == cty.NilType || target == cty.DynamicPseudoType {
		return val, nil
	}
	return convert.Convert(val, target)
}

func mcpToolIDFor(canonical, name, tool string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-tool-id/v1\x00" + canonical + "\x00" + tool))
	return "mcp_tool_" + mcpIDPart(name) + "__" + mcpIDPart(tool) + "_" + mcpUUIDFromDigest(digest)
}

func mcpResourceIDFor(canonical, name, resource string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-resource-id/v1\x00" + canonical + "\x00" + resource))
	return "mcp_resource_" + mcpIDPart(name) + "__" + mcpIDPart(resource) + "_" + mcpUUIDFromDigest(digest)
}

func mcpUUIDFromDigest(digest [sha256.Size]byte) string {
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func mcpIDPart(value string) string {
	var result strings.Builder
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '_', ch == '-':
			result.WriteRune(ch)
		default:
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "tool"
	}
	return result.String()
}
