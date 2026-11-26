package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/lonegunmanb/golden"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const (
	sensitivePlaceholder = "<sensitive>"
	unknownPlaceholder   = "<known after apply>"
)

// Display renders a human-reviewable redacted JSON representation of planned,
// replacing sensitive values with "<sensitive>" and unknown values with "<known after apply>".
func Display(planned *Plan) (string, error) {
	if planned == nil {
		return "", fmt.Errorf("plan is nil")
	}
	view, err := displayPlan(planned)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(view); err != nil {
		return "", fmt.Errorf("encode plan display: %w", err)
	}
	return buffer.String(), nil
}

// DisplayValues formats a map of cty values with sensitivity redaction and unknown placeholders.
func DisplayValues(values map[string]cty.Value) (string, error) {
	view := make(map[string]any, len(values))
	for name, value := range values {
		displayed, err := displayValue(value)
		if err != nil {
			return "", fmt.Errorf("display value %q: %w", name, err)
		}
		view[name] = displayed
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(view); err != nil {
		return "", fmt.Errorf("encode values display: %w", err)
	}
	return buffer.String(), nil
}

// ContainsSensitiveValues reports whether planned or any nested child module
// contains any value marked sensitive or any sensitive output.
func ContainsSensitiveValues(planned *Plan) bool {
	if planned == nil {
		return false
	}
	for _, node := range planned.nodes {
		if corespec.IsSensitive(node.Config) {
			return true
		}
		if node.Module != nil {
			for _, input := range node.Module.Inputs {
				if corespec.IsSensitive(input) {
					return true
				}
			}
			if ContainsSensitiveValues(node.Module.Plan) {
				return true
			}
		}
	}
	for _, output := range planned.outputs {
		if output.Sensitive || corespec.IsSensitive(output.Value) {
			return true
		}
	}
	for _, val := range planned.context {
		if corespec.IsSensitive(val) {
			return true
		}
	}
	return false
}

func displayPlan(planned *Plan) (map[string]any, error) {
	nodes := make([]any, len(planned.nodes))
	for i, node := range planned.nodes {
		cfg := node.Config
		if cfg == cty.NilVal {
			cfg = cty.EmptyObjectVal
		}
		configView, err := displayValue(cfg)
		if err != nil {
			return nil, fmt.Errorf("display node %q config: %w", node.Address, err)
		}
		stripInternalExprFields(node.Kind, configView)
		nodeView := map[string]any{
			"address":      node.Address,
			"kind":         node.Kind,
			"dependencies": node.Dependencies,
			"config":       configView,
			"origin":       displayOrigin(node.Origin),
		}
		if node.CanonicalAddress != "" {
			nodeView["canonical_address"] = node.CanonicalAddress
		}
		if node.Module != nil {
			childView, err := displayPlan(node.Module.Plan)
			if err != nil {
				return nil, fmt.Errorf("display module %q: %w", node.Address, err)
			}
			modView := map[string]any{
				"source":            node.Module.Source,
				"canonical_address": node.Module.CanonicalAddress,
				"bundle_path":       node.Module.BundlePath,
				"plan":              childView,
			}
			if node.Module.Parallelism != nil {
				modView["parallelism"] = *node.Module.Parallelism
			}
			if node.Module.Timeout != nil {
				modView["timeout"] = node.Module.Timeout.String()
			}
			if len(node.Module.Inputs) > 0 {
				inputsView := make(map[string]any, len(node.Module.Inputs))
				for k, v := range node.Module.Inputs {
					iv, err := displayValue(v)
					if err != nil {
						return nil, fmt.Errorf("display module %q input %q: %w", node.Address, k, err)
					}
					inputsView[k] = iv
				}
				modView["inputs"] = inputsView
			}
			nodeView["module"] = modView
		}
		nodes[i] = nodeView
	}

	outputs := make(map[string]any, len(planned.outputs))
	for name, output := range planned.outputs {
		var valView any
		if output.Sensitive {
			valView = sensitivePlaceholder
		} else {
			var err error
			valView, err = displayValue(output.Value)
			if err != nil {
				return nil, fmt.Errorf("display output %q: %w", name, err)
			}
		}
		outView := map[string]any{
			"value":     valView,
			"sensitive": output.Sensitive,
		}
		if output.Description != nil {
			outView["description"] = *output.Description
		}
		outputs[name] = outView
	}

	result := map[string]any{
		"format_version": FormatVersion,
		"directory":      planned.directory,
		"nodes":          nodes,
		"outputs":        outputs,
	}
	if planned.bundlePath != "" {
		result["bundle_path"] = planned.bundlePath
	}
	if len(planned.deferredValidations) > 0 {
		result["deferred_validations"] = planned.deferredValidations
	}
	if len(planned.tools) > 0 {
		toolsView := make(map[string]any, len(planned.tools))
		for id, tool := range planned.tools {
			t := tool
			t.Origin.Source = ""
			toolsView[id] = t
		}
		result["tools"] = toolsView
	}
	if planned.pluginLock != nil && len(planned.pluginLock.Plugins) > 0 {
		result["plugin_lock"] = planned.pluginLock
	}
	return result, nil
}

func displayOrigin(origin Origin) Origin {
	origin.Source = ""
	return origin
}

func stripInternalExprFields(kind string, view any) {
	cfgMap, ok := view.(map[string]any)
	if !ok {
		return
	}
	switch kind {
	case "worker":
		delete(cfgMap, "deferred_expression")
	case "dynamic_worker":
		delete(cfgMap, "content_template")
		delete(cfgMap, "for_each_expr")
		if content, ok := cfgMap["content"].(map[string]any); ok {
			delete(content, "deferred_expression")
		}
	case "module":
		delete(cfgMap, "input_exprs")
	case "preserve":
		delete(cfgMap, "branch_expr")
	case "merge":
		delete(cfgMap, "based_source_expr")
		delete(cfgMap, "sources_expr")
	}
}

func displayValue(value cty.Value) (any, error) {
	if value == cty.NilVal {
		return nil, nil
	}
	unmarked, marks := value.Unmark()
	if _, hasSensitive := marks[golden.SensitiveMark]; hasSensitive || len(marks) > 0 {
		return sensitivePlaceholder, nil
	}
	if !unmarked.IsKnown() {
		return unknownPlaceholder, nil
	}
	if unmarked.IsNull() {
		return nil, nil
	}
	typeValue := unmarked.Type()
	switch {
	case typeValue.IsObjectType(), typeValue.IsMapType():
		result := make(map[string]any, unmarked.LengthInt())
		for name, element := range unmarked.AsValueMap() {
			view, err := displayValue(element)
			if err != nil {
				return nil, err
			}
			result[name] = view
		}
		return result, nil
	case typeValue.IsTupleType(), typeValue.IsListType(), typeValue.IsSetType():
		result := make([]any, 0, unmarked.LengthInt())
		iterator := unmarked.ElementIterator()
		for iterator.Next() {
			_, element := iterator.Element()
			view, err := displayValue(element)
			if err != nil {
				return nil, err
			}
			result = append(result, view)
		}
		if typeValue.IsSetType() {
			sort.Slice(result, func(i, j int) bool {
				return fmt.Sprint(result[i]) < fmt.Sprint(result[j])
			})
		}
		return result, nil
	case typeValue.IsPrimitiveType():
		encoded, err := ctyjson.Marshal(unmarked, typeValue)
		if err != nil {
			return nil, fmt.Errorf("encode primitive display value: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var result any
		if err = decoder.Decode(&result); err != nil {
			return nil, fmt.Errorf("decode primitive display value: %w", err)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported display type %s", typeValue.FriendlyName())
	}
}
