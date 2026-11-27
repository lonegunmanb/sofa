package spec

import (
	"encoding/json"
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

type wireCtyValue struct {
	Type    json.RawMessage `json:"type,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Unknown bool            `json:"unknown,omitempty"`
}

// MarshalJSON preserves Options (cty.Value) across Dapr Actor/Service JSON RPCs.
func (c HarnessConfig) MarshalJSON() ([]byte, error) {
	type alias HarnessConfig
	var opts *wireCtyValue
	if c.Options != cty.NilVal {
		unmarked, _ := c.Options.UnmarkDeep()
		typeJSON, err := ctyjson.MarshalType(unmarked.Type())
		if err != nil {
			return nil, fmt.Errorf("encode harness options type: %w", err)
		}
		if !unmarked.IsWhollyKnown() {
			opts = &wireCtyValue{Type: typeJSON, Unknown: true}
		} else {
			valJSON, err := ctyjson.Marshal(unmarked, unmarked.Type())
			if err != nil {
				return nil, fmt.Errorf("encode harness options value: %w", err)
			}
			opts = &wireCtyValue{Type: typeJSON, Value: valJSON}
		}
	}
	return json.Marshal(struct {
		alias
		Options *wireCtyValue `json:"Options,omitempty"`
	}{
		alias:   alias(c),
		Options: opts,
	})
}

// UnmarshalJSON restores Options (cty.Value) from JSON.
func (c *HarnessConfig) UnmarshalJSON(data []byte) error {
	type alias HarnessConfig
	var wire struct {
		alias
		Options *wireCtyValue `json:"Options,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = HarnessConfig(wire.alias)
	if wire.Options == nil || len(wire.Options.Type) == 0 {
		c.Options = cty.NilVal
		return nil
	}
	ty, err := ctyjson.UnmarshalType(wire.Options.Type)
	if err != nil {
		return fmt.Errorf("decode harness options type: %w", err)
	}
	if wire.Options.Unknown {
		c.Options = cty.UnknownVal(ty)
		return nil
	}
	val, err := ctyjson.Unmarshal(wire.Options.Value, ty)
	if err != nil {
		return fmt.Errorf("decode harness options value: %w", err)
	}
	c.Options = val
	return nil
}
