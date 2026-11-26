package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
)

var (
	toolIDPattern        = regexp.MustCompile(`^tool_.+_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	mcpToolIDPattern     = regexp.MustCompile(`^mcp_tool_.+_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	mcpResourceIDPattern = regexp.MustCompile(`^mcp_resource_.+_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	validNodeKinds = map[string]struct{}{
		"source":         {},
		"preserve":       {},
		"sandbox":        {},
		"harness":        {},
		"model_provider": {},
		"mcp_server":     {},
		"go_tool":        {},
		"external_tool":  {},
		"worker":         {},
		"dynamic_worker": {},
		"merge":          {},
		"module":         {},
	}
)

// Args provides the inputs required to construct an immutable Plan.
type Args struct {
	Directory           string
	BundlePath          string
	Nodes               []NodeSpec
	Outputs             map[string]OutputSpec
	Context             map[string]cty.Value
	LocalExpressions    map[string]string
	DeferredValidations map[string][]corespec.Condition
	Tools               map[string]ToolSpec
	PluginLock          *harness.PluginLockFile
}

// NodeSpec describes one planned node in a module DAG.
type NodeSpec struct {
	Address          string
	CanonicalAddress string
	Kind             string
	Dependencies     []string
	Config           cty.Value
	Module           *ModuleSpec
	Origin           Origin
}

// Origin identifies the immutable HCL source captured for one planned block.
type Origin struct {
	Filename    string `json:"filename"`
	StartLine   int    `json:"start_line"`
	StartColumn int    `json:"start_column"`
	EndLine     int    `json:"end_line"`
	EndColumn   int    `json:"end_column"`
	Source      string `json:"source"`
}

// ModuleSpec captures a child module call and its nested immutable Plan.
type ModuleSpec struct {
	Plan                *Plan
	Source              string
	CanonicalAddress    string
	BundlePath          string
	Parallelism         *int
	Timeout             *time.Duration
	TimeoutRaw          string
	Inputs              map[string]cty.Value
	InputExprs          map[string]string
	DeferredValidations map[string][]corespec.Condition
}

// OutputSpec captures a planned module output value and metadata.
type OutputSpec struct {
	Value       cty.Value
	Description *string
	Sensitive   bool
	Expression  string
	SourceURLs  []string
}

// ToolSpec captures immutable metadata for a registered go_tool or external_tool.
type ToolSpec struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	Address          string               `json:"address"`
	CanonicalAddress string               `json:"canonical_address"`
	Kind             string               `json:"kind"`
	Description      string               `json:"description"`
	Source           string               `json:"source,omitempty"`
	Dependencies     []string             `json:"dependencies,omitempty"`
	Program          []string             `json:"program,omitempty"`
	WorkingDir       string               `json:"working_dir,omitempty"`
	InputSchema      json.RawMessage      `json:"input_schema,omitempty"`
	InputType        json.RawMessage      `json:"input_type,omitempty"`
	OutputType       json.RawMessage      `json:"output_type,omitempty"`
	QuotePaths       [][]string           `json:"quote_paths,omitempty"`
	Postconditions   []corespec.Condition `json:"postconditions,omitempty"`
	Origin           Origin               `json:"origin,omitzero"`
}

// Plan is an immutable offline planning snapshot of a root or child module.
type Plan struct {
	directory           string
	bundlePath          string
	nodes               []NodeSpec
	outputs             map[string]OutputSpec
	context             map[string]cty.Value
	localExpressions    map[string]string
	deferredValidations map[string][]corespec.Condition
	tools               map[string]ToolSpec
	pluginLock          *harness.PluginLockFile
}

// New validates invariants and constructs an immutable deep-copied Plan.
func New(args Args) (*Plan, error) {
	if err := validateNodes(args.Nodes); err != nil {
		return nil, err
	}
	if err := validateTools(args.Tools); err != nil {
		return nil, err
	}
	if args.PluginLock != nil {
		if err := harness.ValidateLockFile(*args.PluginLock); err != nil {
			return nil, fmt.Errorf("invalid plugin lockfile: %w", err)
		}
	}
	return &Plan{
		directory:           args.Directory,
		bundlePath:          args.BundlePath,
		nodes:               cloneNodes(args.Nodes),
		outputs:             cloneOutputs(args.Outputs),
		context:             cloneContext(args.Context),
		localExpressions:    maps.Clone(args.LocalExpressions),
		deferredValidations: cloneDeferredValidations(args.DeferredValidations),
		tools:               cloneTools(args.Tools),
		pluginLock:          clonePluginLock(args.PluginLock),
	}, nil
}

// Directory returns the planned module directory path.
func (p *Plan) Directory() string { return p.directory }

// BundlePath returns the bundle-relative module path ("config" or "modules/...").
func (p *Plan) BundlePath() string { return p.bundlePath }

// Nodes returns a deep copy of the planned nodes.
func (p *Plan) Nodes() []NodeSpec { return cloneNodes(p.nodes) }

// Outputs returns a deep copy of the planned outputs.
func (p *Plan) Outputs() map[string]OutputSpec { return cloneOutputs(p.outputs) }

// Context returns a deep copy of the planned evaluation context variables.
func (p *Plan) Context() map[string]cty.Value { return cloneContext(p.context) }

// LocalExpressions returns a copy of raw local HCL expressions.
func (p *Plan) LocalExpressions() map[string]string { return maps.Clone(p.localExpressions) }

// DeferredValidations returns a deep copy of deferred variable validations.
func (p *Plan) DeferredValidations() map[string][]corespec.Condition {
	return cloneDeferredValidations(p.deferredValidations)
}

// Tools returns a deep copy of the registered tool specifications.
func (p *Plan) Tools() map[string]ToolSpec { return cloneTools(p.tools) }

// PluginLock returns a deep copy of the locked remote harness plugin snapshot, if any.
func (p *Plan) PluginLock() *harness.PluginLockFile { return clonePluginLock(p.pluginLock) }

// IsToolID reports whether value matches sofa's deterministic typed tool ID format.
func IsToolID(value string) bool {
	return toolIDPattern.MatchString(value)
}

// IsMCPToolID reports whether value matches sofa's deterministic MCP tool ID format.
func IsMCPToolID(value string) bool {
	return mcpToolIDPattern.MatchString(value)
}

// IsMCPResourceID reports whether value matches sofa's deterministic MCP resource ID format.
func IsMCPResourceID(value string) bool {
	return mcpResourceIDPattern.MatchString(value)
}

func validateNodes(nodes []NodeSpec) error {
	addresses := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node.Address == "" {
			return fmt.Errorf("node address is required")
		}
		if _, exists := validNodeKinds[node.Kind]; !exists {
			return fmt.Errorf("node %q has unsupported node kind %q", node.Address, node.Kind)
		}
		if _, exists := addresses[node.Address]; exists {
			return fmt.Errorf("node %q is declared more than once", node.Address)
		}
		addresses[node.Address] = struct{}{}
		if node.Kind == "module" && (node.Module == nil || node.Module.Plan == nil) {
			return fmt.Errorf("module node %q must contain a child plan", node.Address)
		}
	}
	for _, node := range nodes {
		for _, dependency := range node.Dependencies {
			if _, exists := addresses[dependency]; !exists {
				return fmt.Errorf("node %q depends on unknown node %q", node.Address, dependency)
			}
		}
	}
	return validateAcyclic(nodes)
}

func validateAcyclic(nodes []NodeSpec) error {
	dependencies := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		dependencies[node.Address] = node.Dependencies
	}
	state := make(map[string]uint8, len(nodes))
	stack := make([]string, 0, len(nodes))
	var visit func(string) error
	visit = func(address string) error {
		switch state[address] {
		case 1:
			return fmt.Errorf("plan dependency cycle: %v -> %s", stack, address)
		case 2:
			return nil
		}
		state[address] = 1
		stack = append(stack, address)
		for _, dependency := range dependencies[address] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[address] = 2
		return nil
	}
	for _, node := range nodes {
		if err := visit(node.Address); err != nil {
			return err
		}
	}
	return nil
}

func validateTools(tools map[string]ToolSpec) error {
	for id, tool := range tools {
		if id == "" || tool.ID == "" {
			return fmt.Errorf("tool id is required")
		}
		if id != tool.ID {
			return fmt.Errorf("tool registry key %q must match tool id %q", id, tool.ID)
		}
		if !IsToolID(id) {
			return fmt.Errorf("tool id %q is invalid", id)
		}
		if tool.Address == "" {
			return fmt.Errorf("tool %s address is required", id)
		}
		if tool.Kind != "go" && tool.Kind != "external" {
			return fmt.Errorf("tool %s has invalid kind %q", id, tool.Kind)
		}
	}
	return nil
}

func clonePlan(source *Plan) *Plan {
	if source == nil {
		return nil
	}
	return &Plan{
		directory:           source.directory,
		bundlePath:          source.bundlePath,
		nodes:               cloneNodes(source.nodes),
		outputs:             cloneOutputs(source.outputs),
		context:             cloneContext(source.context),
		localExpressions:    maps.Clone(source.localExpressions),
		deferredValidations: cloneDeferredValidations(source.deferredValidations),
		tools:               cloneTools(source.tools),
		pluginLock:          clonePluginLock(source.pluginLock),
	}
}

func cloneNodes(source []NodeSpec) []NodeSpec {
	if source == nil {
		return nil
	}
	result := make([]NodeSpec, len(source))
	for i, node := range source {
		result[i] = node
		result[i].Dependencies = slices.Clone(node.Dependencies)
		if node.Module != nil {
			mod := *node.Module
			mod.Plan = clonePlan(node.Module.Plan)
			if node.Module.Parallelism != nil {
				p := *node.Module.Parallelism
				mod.Parallelism = &p
			}
			if node.Module.Timeout != nil {
				d := *node.Module.Timeout
				mod.Timeout = &d
			}
			mod.Inputs = maps.Clone(node.Module.Inputs)
			mod.InputExprs = maps.Clone(node.Module.InputExprs)
			mod.DeferredValidations = cloneDeferredValidations(node.Module.DeferredValidations)
			result[i].Module = &mod
		}
	}
	return result
}

func cloneOutputs(source map[string]OutputSpec) map[string]OutputSpec {
	if source == nil {
		return nil
	}
	result := make(map[string]OutputSpec, len(source))
	for k, out := range source {
		if out.Description != nil {
			desc := *out.Description
			out.Description = &desc
		}
		out.SourceURLs = slices.Clone(out.SourceURLs)
		result[k] = out
	}
	return result
}

func cloneContext(source map[string]cty.Value) map[string]cty.Value {
	if source == nil {
		return nil
	}
	result := make(map[string]cty.Value, len(source))
	maps.Copy(result, source)
	return result
}

func cloneDeferredValidations(source map[string][]corespec.Condition) map[string][]corespec.Condition {
	if source == nil {
		return nil
	}
	result := make(map[string][]corespec.Condition, len(source))
	for k, conds := range source {
		result[k] = slices.Clone(conds)
	}
	return result
}

func cloneTools(source map[string]ToolSpec) map[string]ToolSpec {
	if source == nil {
		return nil
	}
	result := make(map[string]ToolSpec, len(source))
	for id, tool := range source {
		tool.Dependencies = slices.Clone(tool.Dependencies)
		tool.Program = slices.Clone(tool.Program)
		tool.InputSchema = compactJSON(tool.InputSchema)
		tool.InputType = compactJSON(tool.InputType)
		tool.OutputType = compactJSON(tool.OutputType)
		if tool.QuotePaths != nil {
			qp := make([][]string, len(tool.QuotePaths))
			for i, path := range tool.QuotePaths {
				qp[i] = slices.Clone(path)
			}
			tool.QuotePaths = qp
		}
		tool.Postconditions = slices.Clone(tool.Postconditions)
		result[id] = tool
	}
	return result
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return slices.Clone(raw)
	}
	return buf.Bytes()
}

func clonePluginLock(source *harness.PluginLockFile) *harness.PluginLockFile {
	if source == nil {
		return nil
	}
	cloned := &harness.PluginLockFile{
		FormatVersion: source.FormatVersion,
		Plugins:       make([]harness.PluginLockEntry, len(source.Plugins)),
	}
	for i, entry := range source.Plugins {
		entry.Assets = maps.Clone(entry.Assets)
		cloned.Plugins[i] = entry
	}
	return cloned
}
