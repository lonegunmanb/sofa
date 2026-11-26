package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/lonegunmanb/hclfuncs"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// Functions returns the standard planning function set with sofa's additions.
func Functions(basedir string) map[string]function.Function {
	result := hclfuncs.Functions(basedir)
	result["cwd"] = cwdFunction
	result["local_timestamp"] = localTimestampFunction
	result["one"] = oneFunction
	result["jsondecodewithtype"] = jsonDecodeWithTypeFunction
	result["tool_name"] = toolNameFunction
	return result
}

var cwdFunction = function.New(&function.Spec{
	Description: "Returns the absolute current working directory with forward slashes.",
	Params:      []function.Parameter{},
	Type:        function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		dir, err := os.Getwd()
		if err != nil {
			return cty.NilVal, err
		}
		return cty.StringVal(filepath.ToSlash(dir)), nil
	},
})

var localTimestampFunction = function.New(&function.Spec{
	Description: "Returns the current local time in RFC3339Nano form.",
	Params:      []function.Parameter{},
	Type:        function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(time.Now().Format(time.RFC3339Nano)), nil
	},
})

var oneFunction = function.New(&function.Spec{
	Description: "Returns the only element of a collection, or null for an empty collection.",
	Params: []function.Parameter{
		{
			Name:         "collection",
			Type:         cty.DynamicPseudoType,
			AllowUnknown: true,
			AllowNull:    true,
		},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		value := args[0]
		if value.IsNull() {
			return cty.NilType, fmt.Errorf("cannot use null collection with one")
		}
		valueType := value.Type()
		switch {
		case valueType.IsListType(), valueType.IsSetType():
			return valueType.ElementType(), nil
		case valueType.IsMapType():
			return valueType.ElementType(), nil
		case valueType.IsTupleType():
			return unifyElementTypes(valueType.TupleElementTypes())
		case valueType.IsObjectType():
			attributeTypes := valueType.AttributeTypes()
			types := make([]cty.Type, 0, len(attributeTypes))
			for _, attributeType := range attributeTypes {
				types = append(types, attributeType)
			}
			return unifyElementTypes(types)
		default:
			return cty.NilType, fmt.Errorf("cannot use %s with one; a collection is required", valueType.FriendlyName())
		}
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		value := args[0]
		if value.IsNull() {
			return cty.NilVal, fmt.Errorf("cannot use null collection with one")
		}
		if !value.IsKnown() {
			return cty.UnknownVal(retType), nil
		}
		if value.LengthInt() == 0 {
			return cty.NullVal(retType), nil
		}
		if value.LengthInt() > 1 {
			return cty.NilVal, fmt.Errorf("one() requires a collection with at most one element, got %d", value.LengthInt())
		}

		iterator := value.ElementIterator()
		if !iterator.Next() {
			return cty.NullVal(retType), nil
		}
		_, element := iterator.Element()
		element, err := convert.Convert(element, retType)
		if err != nil {
			return cty.NilVal, err
		}
		return element, nil
	},
})

func unifyElementTypes(types []cty.Type) (cty.Type, error) {
	if len(types) == 0 {
		return cty.NilType, fmt.Errorf("cannot infer element type from an empty collection")
	}
	first := types[0]
	allEqual := true
	for _, candidate := range types[1:] {
		if !candidate.Equals(first) {
			allEqual = false
			break
		}
	}
	if allEqual {
		return first, nil
	}
	unified, err := convert.UnifyUnsafe(types)
	if err != nil || unified == cty.NilType {
		return cty.NilType, fmt.Errorf("collection elements must have a compatible type")
	}
	return unified, nil
}

var jsonDecodeWithTypeFunction = function.New(&function.Spec{
	Description: "Decodes JSON into the explicitly supplied type constraint.",
	Params: []function.Parameter{
		{
			Name: "json",
			Type: cty.String,
		},
		{
			Name: "type",
			Type: typeexpr.TypeConstraintType,
		},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		return typeexpr.TypeConstraintFromVal(args[1]), nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return ctyjson.Unmarshal([]byte(args[0].AsString()), retType)
	},
})

var toolNameFunction = function.New(&function.Spec{
	Description: "Returns the runtime tool name/ID for a typed tool reference.",
	Params: []function.Parameter{
		{
			Name:             "tool_ref",
			Description:      "A typed tool reference object.",
			Type:             cty.DynamicPseudoType,
			AllowNull:        true,
			AllowMarked:      true,
			AllowUnknown:     true,
			AllowDynamicType: true,
		},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		unmarked, outerMarks := args[0].Unmark()
		if unmarked == cty.NilVal || unmarked.IsNull() {
			return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
		}
		if !unmarked.IsKnown() {
			argType := unmarked.Type()
			if argType.Equals(cty.DynamicPseudoType) {
				return cty.UnknownVal(cty.String).WithMarks(outerMarks), nil
			}
			if argType.IsObjectType() && validUnknownToolObjectTypes(argType) {
				return cty.UnknownVal(cty.String).WithMarks(outerMarks), nil
			}
			return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
		}

		valueType := unmarked.Type()
		if !valueType.IsObjectType() || !valueType.HasAttribute("address") || !valueType.HasAttribute("kind") {
			return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
		}
		if !valueType.AttributeType("address").Equals(cty.String) || !valueType.AttributeType("kind").Equals(cty.String) {
			return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
		}

		addressVal, addressMarks := unmarked.GetAttr("address").Unmark()
		kindVal, kindMarks := unmarked.GetAttr("kind").Unmark()
		if addressVal.IsNull() || kindVal.IsNull() {
			return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
		}

		mergedMarks := mergeValueMarks(outerMarks, addressMarks, kindMarks)
		var idVal cty.Value
		hasID := valueType.HasAttribute("id")
		if hasID {
			if !valueType.AttributeType("id").Equals(cty.String) {
				return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
			}
			var idMarks cty.ValueMarks
			idVal, idMarks = unmarked.GetAttr("id").Unmark()
			mergedMarks = mergeValueMarks(mergedMarks, idMarks)
		}

		if !addressVal.IsKnown() || !kindVal.IsKnown() || (hasID && !idVal.IsKnown()) {
			return cty.UnknownVal(cty.String).WithMarks(mergedMarks), nil
		}

		address := addressVal.AsString()
		kind := kindVal.AsString()
		if !isValidTypedToolAddress(kind, address) {
			return cty.NilVal, function.NewArgError(0, errors.New("tool_name requires a typed tool address"))
		}

		if hasID && !idVal.IsNull() && idVal.AsString() != "" {
			return cty.StringVal(idVal.AsString()).WithMarks(mergedMarks), nil
		}
		return cty.StringVal(strings.ReplaceAll(address, ".", "_")).WithMarks(mergedMarks), nil
	},
})

func validUnknownToolObjectTypes(objectType cty.Type) bool {
	for _, attrName := range []string{"address", "kind", "id"} {
		if objectType.HasAttribute(attrName) {
			attrType := objectType.AttributeType(attrName)
			if !attrType.Equals(cty.String) && !attrType.Equals(cty.DynamicPseudoType) {
				return false
			}
		}
	}
	return true
}

func mergeValueMarks(groups ...cty.ValueMarks) cty.ValueMarks {
	merged := make(cty.ValueMarks)
	for _, group := range groups {
		for mark := range group {
			merged[mark] = struct{}{}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func isValidTypedToolAddress(kind, address string) bool {
	switch kind {
	case "go":
		return hasTypedToolSegment(address, "go_tool")
	case "external":
		return hasTypedToolSegment(address, "external_tool")
	case "builtin":
		return address != ""
	default:
		return false
	}
}

func hasTypedToolSegment(address, blockType string) bool {
	directPrefix := blockType + "."
	if strings.HasPrefix(address, directPrefix) {
		return validToolInstanceSegment(address[len(directPrefix):])
	}
	if !strings.HasPrefix(address, "module.") {
		return false
	}
	marker := "." + blockType + "."
	idx := strings.LastIndex(address, marker)
	if idx <= len("module.") || idx+len(marker) >= len(address) {
		return false
	}
	modulePrefix := address[len("module."):idx]
	for _, segment := range strings.Split(modulePrefix, ".") {
		if segment == "" {
			return false
		}
	}
	return validToolInstanceSegment(address[idx+len(marker):])
}

func validToolInstanceSegment(segment string) bool {
	if segment == "" {
		return false
	}
	name, rest, hasBracket := strings.Cut(segment, "[")
	if !hasBracket {
		return !strings.ContainsAny(segment, ".]")
	}
	if name == "" || strings.ContainsAny(name, ".]") {
		return false
	}
	return strings.HasSuffix(rest, "]") && len(rest) > 1
}
