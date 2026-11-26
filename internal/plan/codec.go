package plan

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/harness"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
)

// FormatVersion is the schema version of serialized plan.json documents.
const FormatVersion = 1

type wirePlan struct {
	FormatVersion       int                             `json:"format_version"`
	Directory           string                          `json:"directory"`
	BundlePath          string                          `json:"bundle_path,omitempty"`
	Nodes               []wireNode                      `json:"nodes"`
	Outputs             map[string]wireOutput           `json:"outputs"`
	Context             map[string]wireValue            `json:"context,omitempty"`
	LocalExpressions    map[string]string               `json:"local_expressions,omitempty"`
	DeferredValidations map[string][]corespec.Condition `json:"deferred_validations,omitempty"`
	Tools               map[string]ToolSpec             `json:"tools,omitempty"`
	PluginLock          *harness.PluginLockFile         `json:"plugin_lock,omitempty"`
}

type wireNode struct {
	Address          string      `json:"address"`
	CanonicalAddress string      `json:"canonical_address,omitempty"`
	Kind             string      `json:"kind"`
	Dependencies     []string    `json:"dependencies,omitempty"`
	Config           wireValue   `json:"config"`
	Module           *wireModule `json:"module,omitempty"`
	Origin           Origin      `json:"origin,omitzero"`
}

type wireModule struct {
	Plan                wirePlan                        `json:"plan"`
	Source              string                          `json:"source,omitempty"`
	CanonicalAddress    string                          `json:"canonical_address,omitempty"`
	BundlePath          string                          `json:"bundle_path,omitempty"`
	Parallelism         *int                            `json:"parallelism,omitempty"`
	TimeoutNanos        *int64                          `json:"timeout_nanoseconds,omitempty"`
	TimeoutRaw          string                          `json:"timeout_raw,omitempty"`
	Inputs              map[string]wireValue            `json:"inputs,omitempty"`
	InputExprs          map[string]string               `json:"input_exprs,omitempty"`
	DeferredValidations map[string][]corespec.Condition `json:"deferred_validations,omitempty"`
}

type wireOutput struct {
	Value       wireValue `json:"value"`
	Description *string   `json:"description,omitempty"`
	Sensitive   bool      `json:"sensitive,omitempty"`
	Expression  string    `json:"expression,omitempty"`
	SourceURLs  []string  `json:"source_urls,omitempty"`
}

type wireValue struct {
	Type           json.RawMessage `json:"type"`
	Value          []byte          `json:"value"`
	SensitivePaths []wirePath      `json:"sensitive_paths,omitempty"`
}

type wirePath []wirePathStep

type wirePathStep struct {
	Attribute *string    `json:"attribute,omitempty"`
	Index     *wireIndex `json:"index,omitempty"`
}

type wireIndex struct {
	Type  json.RawMessage `json:"type"`
	Value json.RawMessage `json:"value"`
}

// Marshal encodes an immutable Plan into deterministic JSON bytes, preserving
// typed unknowns and golden.SensitiveMark paths.
func Marshal(planned *Plan) ([]byte, error) {
	if planned == nil {
		return nil, fmt.Errorf("plan is nil")
	}
	wire, err := encodePlan(planned)
	if err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode plan: %w", err)
	}
	return append(encoded, '\n'), nil
}

// Unmarshal decodes serialized plan JSON back into a validated immutable Plan.
func Unmarshal(encoded []byte) (*Plan, error) {
	var wire wirePlan
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if wire.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("decode plan: unsupported format_version %d", wire.FormatVersion)
	}
	planned, err := decodePlan(wire)
	if err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	return planned, nil
}

func encodePlan(planned *Plan) (wirePlan, error) {
	result := wirePlan{
		FormatVersion:       FormatVersion,
		Directory:           planned.directory,
		BundlePath:          planned.bundlePath,
		Nodes:               make([]wireNode, len(planned.nodes)),
		Outputs:             make(map[string]wireOutput, len(planned.outputs)),
		Context:             make(map[string]wireValue, len(planned.context)),
		LocalExpressions:    planned.LocalExpressions(),
		DeferredValidations: planned.DeferredValidations(),
		Tools:               planned.Tools(),
		PluginLock:          planned.PluginLock(),
	}
	for i, node := range planned.nodes {
		cfgVal := node.Config
		if cfgVal == cty.NilVal {
			cfgVal = cty.EmptyObjectVal
		}
		config, err := encodeValue(cfgVal)
		if err != nil {
			return wirePlan{}, fmt.Errorf("node %q config: %w", node.Address, err)
		}
		result.Nodes[i] = wireNode{
			Address:          node.Address,
			CanonicalAddress: node.CanonicalAddress,
			Kind:             node.Kind,
			Dependencies:     node.Dependencies,
			Config:           config,
			Origin:           node.Origin,
		}
		if node.Module != nil {
			child, err := encodePlan(node.Module.Plan)
			if err != nil {
				return wirePlan{}, fmt.Errorf("module %q: %w", node.Address, err)
			}
			inputs := make(map[string]wireValue, len(node.Module.Inputs))
			for k, v := range node.Module.Inputs {
				encodedInput, err := encodeValue(v)
				if err != nil {
					return wirePlan{}, fmt.Errorf("module %q input %q: %w", node.Address, k, err)
				}
				inputs[k] = encodedInput
			}
			var timeoutNanos *int64
			if node.Module.Timeout != nil {
				ns := int64(*node.Module.Timeout)
				timeoutNanos = &ns
			}
			result.Nodes[i].Module = &wireModule{
				Plan:                child,
				Source:              node.Module.Source,
				CanonicalAddress:    node.Module.CanonicalAddress,
				BundlePath:          node.Module.BundlePath,
				Parallelism:         node.Module.Parallelism,
				TimeoutNanos:        timeoutNanos,
				TimeoutRaw:          node.Module.TimeoutRaw,
				Inputs:              inputs,
				InputExprs:          node.Module.InputExprs,
				DeferredValidations: node.Module.DeferredValidations,
			}
		}
	}
	for name, output := range planned.outputs {
		value, err := encodeValue(output.Value)
		if err != nil {
			return wirePlan{}, fmt.Errorf("output %q: %w", name, err)
		}
		result.Outputs[name] = wireOutput{
			Value:       value,
			Description: output.Description,
			Sensitive:   output.Sensitive,
			Expression:  output.Expression,
			SourceURLs:  output.SourceURLs,
		}
	}
	for name, value := range planned.context {
		encoded, err := encodeValue(value)
		if err != nil {
			return wirePlan{}, fmt.Errorf("context %q: %w", name, err)
		}
		result.Context[name] = encoded
	}
	return result, nil
}

func decodePlan(wire wirePlan) (*Plan, error) {
	if wire.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported nested plan format_version %d", wire.FormatVersion)
	}
	nodes := make([]NodeSpec, len(wire.Nodes))
	for i, node := range wire.Nodes {
		config, err := decodeValue(node.Config)
		if err != nil {
			return nil, fmt.Errorf("node %q config: %w", node.Address, err)
		}
		nodes[i] = NodeSpec{
			Address:          node.Address,
			CanonicalAddress: node.CanonicalAddress,
			Kind:             node.Kind,
			Dependencies:     node.Dependencies,
			Config:           config,
			Origin:           node.Origin,
		}
		if node.Module != nil {
			child, err := decodePlan(node.Module.Plan)
			if err != nil {
				return nil, fmt.Errorf("module %q: %w", node.Address, err)
			}
			var inputs map[string]cty.Value
			if len(node.Module.Inputs) > 0 {
				inputs = make(map[string]cty.Value, len(node.Module.Inputs))
				for k, v := range node.Module.Inputs {
					decodedInput, err := decodeValue(v)
					if err != nil {
						return nil, fmt.Errorf("module %q input %q: %w", node.Address, k, err)
					}
					inputs[k] = decodedInput
				}
			}
			var timeout *time.Duration
			if node.Module.TimeoutNanos != nil {
				d := time.Duration(*node.Module.TimeoutNanos)
				timeout = &d
			}
			nodes[i].Module = &ModuleSpec{
				Plan:                child,
				Source:              node.Module.Source,
				CanonicalAddress:    node.Module.CanonicalAddress,
				BundlePath:          node.Module.BundlePath,
				Parallelism:         node.Module.Parallelism,
				Timeout:             timeout,
				TimeoutRaw:          node.Module.TimeoutRaw,
				Inputs:              inputs,
				InputExprs:          node.Module.InputExprs,
				DeferredValidations: node.Module.DeferredValidations,
			}
		}
	}

	outputs := make(map[string]OutputSpec, len(wire.Outputs))
	for name, output := range wire.Outputs {
		value, err := decodeValue(output.Value)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", name, err)
		}
		outputs[name] = OutputSpec{
			Value:       value,
			Description: output.Description,
			Sensitive:   output.Sensitive,
			Expression:  output.Expression,
			SourceURLs:  output.SourceURLs,
		}
	}

	var contextValues map[string]cty.Value
	if len(wire.Context) > 0 {
		contextValues = make(map[string]cty.Value, len(wire.Context))
		for name, encoded := range wire.Context {
			value, err := decodeValue(encoded)
			if err != nil {
				return nil, fmt.Errorf("context %q: %w", name, err)
			}
			contextValues[name] = value
		}
	}

	return New(Args{
		Directory:           wire.Directory,
		BundlePath:          wire.BundlePath,
		Nodes:               nodes,
		Outputs:             outputs,
		Context:             contextValues,
		LocalExpressions:    wire.LocalExpressions,
		DeferredValidations: wire.DeferredValidations,
		Tools:               wire.Tools,
		PluginLock:          wire.PluginLock,
	})
}

func encodeValue(value cty.Value) (wireValue, error) {
	if value == cty.NilVal {
		return wireValue{}, fmt.Errorf("cannot encode nil cty.Value")
	}
	unmarked, pathMarks := value.UnmarkDeepWithPaths()
	typeJSON, err := ctyjson.MarshalType(unmarked.Type())
	if err != nil {
		return wireValue{}, fmt.Errorf("encode type: %w", err)
	}
	valueMessagePack, err := ctymsgpack.Marshal(unmarked, unmarked.Type())
	if err != nil {
		return wireValue{}, fmt.Errorf("encode value: %w", err)
	}
	result := wireValue{Type: typeJSON, Value: valueMessagePack}
	for _, pvm := range pathMarks {
		if _, isSensitive := pvm.Marks[golden.SensitiveMark]; !isSensitive {
			continue
		}
		encodedPath, err := encodePath(pvm.Path)
		if err != nil {
			return wireValue{}, err
		}
		result.SensitivePaths = append(result.SensitivePaths, encodedPath)
	}
	return result, nil
}

func decodeValue(wire wireValue) (cty.Value, error) {
	typeValue, err := ctyjson.UnmarshalType(wire.Type)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode type: %w", err)
	}
	value, err := ctymsgpack.Unmarshal(wire.Value, typeValue)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode value: %w", err)
	}
	if len(wire.SensitivePaths) == 0 {
		return value, nil
	}
	marks := make([]cty.PathValueMarks, len(wire.SensitivePaths))
	for i, wp := range wire.SensitivePaths {
		path, err := decodePath(wp)
		if err != nil {
			return cty.NilVal, err
		}
		if _, err = path.Apply(value); err != nil {
			return cty.NilVal, fmt.Errorf("sensitive path does not apply: %w", err)
		}
		marks[i] = cty.PathValueMarks{Path: path, Marks: sensitiveMarks()}
	}
	return value.MarkWithPaths(marks), nil
}

func encodePath(path cty.Path) (wirePath, error) {
	result := make(wirePath, len(path))
	for i, step := range path {
		switch s := step.(type) {
		case cty.GetAttrStep:
			name := s.Name
			result[i].Attribute = &name
		case cty.IndexStep:
			key := s.Key
			typeJSON, err := ctyjson.MarshalType(key.Type())
			if err != nil {
				return nil, fmt.Errorf("encode sensitive path index type: %w", err)
			}
			valueJSON, err := ctyjson.Marshal(key, key.Type())
			if err != nil {
				return nil, fmt.Errorf("encode sensitive path index: %w", err)
			}
			result[i].Index = &wireIndex{Type: typeJSON, Value: valueJSON}
		default:
			return nil, fmt.Errorf("unsupported sensitive path step %T", step)
		}
	}
	return result, nil
}

func decodePath(wire wirePath) (cty.Path, error) {
	result := make(cty.Path, len(wire))
	for i, step := range wire {
		switch {
		case step.Attribute != nil && step.Index == nil:
			result[i] = cty.GetAttrStep{Name: *step.Attribute}
		case step.Attribute == nil && step.Index != nil:
			typeValue, err := ctyjson.UnmarshalType(step.Index.Type)
			if err != nil {
				return nil, fmt.Errorf("decode sensitive path index type: %w", err)
			}
			key, err := ctyjson.Unmarshal(step.Index.Value, typeValue)
			if err != nil {
				return nil, fmt.Errorf("decode sensitive path index: %w", err)
			}
			result[i] = cty.IndexStep{Key: key}
		default:
			return nil, fmt.Errorf("decode sensitive path: each step must contain one selector")
		}
	}
	return result, nil
}

func sensitiveMarks() cty.ValueMarks {
	_, marks := corespec.MarkSensitive(cty.StringVal("")).Unmark()
	return marks
}
