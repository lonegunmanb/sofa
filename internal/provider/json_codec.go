package provider

import (
	"encoding/json"
	"fmt"

	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

type wireHeaderIndex struct {
	Type  json.RawMessage `json:"type"`
	Value json.RawMessage `json:"value"`
}

type wireHeaderPathStep struct {
	Attribute *string          `json:"attribute,omitempty"`
	Index     *wireHeaderIndex `json:"index,omitempty"`
}

type wireHeadersValue struct {
	Type           json.RawMessage        `json:"type,omitempty"`
	Value          json.RawMessage        `json:"value,omitempty"`
	Unknown        bool                   `json:"unknown,omitempty"`
	SensitivePaths [][]wireHeaderPathStep `json:"sensitive_paths,omitempty"`
}

func encodeHeadersValue(val cty.Value) (*wireHeadersValue, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, pathMarks := val.UnmarkDeepWithPaths()
	typeJSON, err := ctyjson.MarshalType(unmarked.Type())
	if err != nil {
		return nil, fmt.Errorf("encode provider headers type: %w", err)
	}

	var sensitivePaths [][]wireHeaderPathStep
	for _, pvm := range pathMarks {
		if _, ok := pvm.Marks[golden.SensitiveMark]; !ok {
			continue
		}
		steps, err := encodeHeaderPath(pvm.Path)
		if err != nil {
			return nil, err
		}
		sensitivePaths = append(sensitivePaths, steps)
	}

	if !unmarked.IsWhollyKnown() {
		return &wireHeadersValue{
			Type:           typeJSON,
			Unknown:        true,
			SensitivePaths: sensitivePaths,
		}, nil
	}
	valJSON, err := ctyjson.Marshal(unmarked, unmarked.Type())
	if err != nil {
		return nil, fmt.Errorf("encode provider headers value: %w", err)
	}
	return &wireHeadersValue{
		Type:           typeJSON,
		Value:          valJSON,
		SensitivePaths: sensitivePaths,
	}, nil
}

func decodeHeadersValue(w *wireHeadersValue) (cty.Value, error) {
	if w == nil || len(w.Type) == 0 {
		return cty.NilVal, nil
	}
	ty, err := ctyjson.UnmarshalType(w.Type)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode provider headers type: %w", err)
	}
	var val cty.Value
	if w.Unknown {
		val = cty.UnknownVal(ty)
	} else {
		val, err = ctyjson.Unmarshal(w.Value, ty)
		if err != nil {
			return cty.NilVal, fmt.Errorf("decode provider headers value: %w", err)
		}
	}
	if len(w.SensitivePaths) == 0 {
		return val, nil
	}
	marks := make([]cty.PathValueMarks, len(w.SensitivePaths))
	for i, steps := range w.SensitivePaths {
		p, err := decodeHeaderPath(steps)
		if err != nil {
			return cty.NilVal, err
		}
		if _, err := p.Apply(val); err != nil {
			return cty.NilVal, fmt.Errorf("apply sensitive path to provider headers: %w", err)
		}
		marks[i] = cty.PathValueMarks{
			Path:  p,
			Marks: cty.NewValueMarks(golden.SensitiveMark),
		}
	}
	return val.MarkWithPaths(marks), nil
}

func encodeHeaderPath(p cty.Path) ([]wireHeaderPathStep, error) {
	out := make([]wireHeaderPathStep, len(p))
	for i, step := range p {
		switch s := step.(type) {
		case cty.GetAttrStep:
			name := s.Name
			out[i].Attribute = &name
		case cty.IndexStep:
			typeJSON, err := ctyjson.MarshalType(s.Key.Type())
			if err != nil {
				return nil, fmt.Errorf("encode sensitive path index type: %w", err)
			}
			valJSON, err := ctyjson.Marshal(s.Key, s.Key.Type())
			if err != nil {
				return nil, fmt.Errorf("encode sensitive path index: %w", err)
			}
			out[i].Index = &wireHeaderIndex{Type: typeJSON, Value: valJSON}
		default:
			return nil, fmt.Errorf("unsupported sensitive path step %T", step)
		}
	}
	return out, nil
}

func decodeHeaderPath(steps []wireHeaderPathStep) (cty.Path, error) {
	out := make(cty.Path, len(steps))
	for i, step := range steps {
		switch {
		case step.Attribute != nil && step.Index == nil:
			out[i] = cty.GetAttrStep{Name: *step.Attribute}
		case step.Attribute == nil && step.Index != nil:
			ty, err := ctyjson.UnmarshalType(step.Index.Type)
			if err != nil {
				return nil, fmt.Errorf("decode sensitive path index type: %w", err)
			}
			key, err := ctyjson.Unmarshal(step.Index.Value, ty)
			if err != nil {
				return nil, fmt.Errorf("decode sensitive path index: %w", err)
			}
			out[i] = cty.IndexStep{Key: key}
		default:
			return nil, fmt.Errorf("invalid sensitive path step")
		}
	}
	return out, nil
}

// MarshalJSON preserves Headers (cty.Value) and any golden.SensitiveMark paths across Dapr JSON RPCs.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config
	hdr, err := encodeHeadersValue(c.Headers)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		alias
		Headers *wireHeadersValue `json:"Headers,omitempty"`
	}{
		alias:   alias(c),
		Headers: hdr,
	})
}

// UnmarshalJSON restores Headers (cty.Value) and golden.SensitiveMark paths from JSON.
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config
	var wire struct {
		alias
		Headers *wireHeadersValue `json:"Headers,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = Config(wire.alias)
	hdr, err := decodeHeadersValue(wire.Headers)
	if err != nil {
		return err
	}
	c.Headers = hdr
	return nil
}
