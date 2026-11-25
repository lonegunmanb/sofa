package config

import (
	"fmt"
	"os"
	"path/filepath"
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
