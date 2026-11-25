package spec

import (
	"fmt"

	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

// MarkSensitive marks a value using golden's canonical sensitivity marker.
func MarkSensitive(value cty.Value) cty.Value {
	if value == cty.NilVal {
		return value
	}
	return value.Mark(golden.SensitiveMark)
}

// IsSensitive reports whether a value or any nested value uses golden's
// canonical sensitivity marker.
func IsSensitive(value cty.Value) bool {
	if value == cty.NilVal {
		return false
	}
	return value.HasMarkDeep(golden.SensitiveMark)
}

// ValidateType rejects types that cannot provide a deterministic sofa contract:
// absent types, dynamic pseudo-types, and capsule types.
func ValidateType(valueType cty.Type) error {
	if valueType == cty.NilType {
		return fmt.Errorf("type is required and cannot be null")
	}
	if valueType == cty.DynamicPseudoType || valueType.HasDynamicTypes() {
		return fmt.Errorf("dynamic type %q is not allowed; declare a concrete type", valueType.FriendlyName())
	}
	if valueType.IsCapsuleType() {
		return fmt.Errorf("capsule type %q is not allowed", valueType.FriendlyName())
	}

	switch {
	case valueType.IsPrimitiveType():
		return nil
	case valueType.IsListType(), valueType.IsSetType(), valueType.IsMapType():
		return ValidateType(valueType.ElementType())
	case valueType.IsTupleType():
		for i, elementType := range valueType.TupleElementTypes() {
			if err := ValidateType(elementType); err != nil {
				return fmt.Errorf("tuple element %d: %w", i, err)
			}
		}
		return nil
	case valueType.IsObjectType():
		for name, attributeType := range valueType.AttributeTypes() {
			if err := ValidateType(attributeType); err != nil {
				return fmt.Errorf("object attribute %q: %w", name, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported type %q", valueType.FriendlyName())
	}
}
