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

func encodeCtyValue(val cty.Value) (*wireCtyValue, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	typeJSON, err := ctyjson.MarshalType(unmarked.Type())
	if err != nil {
		return nil, fmt.Errorf("encode cty type: %w", err)
	}
	if !unmarked.IsWhollyKnown() {
		return &wireCtyValue{
			Type:    typeJSON,
			Unknown: true,
		}, nil
	}
	valJSON, err := ctyjson.Marshal(unmarked, unmarked.Type())
	if err != nil {
		return nil, fmt.Errorf("encode cty value: %w", err)
	}
	return &wireCtyValue{
		Type:  typeJSON,
		Value: valJSON,
	}, nil
}

func decodeCtyValue(w *wireCtyValue) (cty.Value, error) {
	if w == nil || len(w.Type) == 0 {
		return cty.NilVal, nil
	}
	ty, err := ctyjson.UnmarshalType(w.Type)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode cty type: %w", err)
	}
	if w.Unknown {
		return cty.UnknownVal(ty), nil
	}
	val, err := ctyjson.Unmarshal(w.Value, ty)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode cty value: %w", err)
	}
	return val, nil
}

// MarshalJSON preserves cty.Value fields across Dapr Actor/Service JSON RPCs.
func (u ToolUse) MarshalJSON() ([]byte, error) {
	type alias ToolUse
	in, err := encodeCtyValue(u.Input)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		alias
		Input *wireCtyValue `json:"Input,omitempty"`
	}{
		alias: alias(u),
		Input: in,
	})
}

// UnmarshalJSON restores cty.Value fields for ToolUse from JSON.
func (u *ToolUse) UnmarshalJSON(data []byte) error {
	type alias ToolUse
	var wire struct {
		alias
		Input *wireCtyValue `json:"Input,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*u = ToolUse(wire.alias)
	in, err := decodeCtyValue(wire.Input)
	if err != nil {
		return err
	}
	u.Input = in
	return nil
}

// MarshalJSON preserves Sources (cty.Value) on ImportArtifact across JSON RPCs.
func (i ImportArtifact) MarshalJSON() ([]byte, error) {
	type alias ImportArtifact
	src, err := encodeCtyValue(i.Sources)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		alias
		Sources *wireCtyValue `json:"Sources,omitempty"`
	}{
		alias:   alias(i),
		Sources: src,
	})
}

// UnmarshalJSON restores Sources (cty.Value) on ImportArtifact from JSON.
func (i *ImportArtifact) UnmarshalJSON(data []byte) error {
	type alias ImportArtifact
	var wire struct {
		alias
		Sources *wireCtyValue `json:"Sources,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*i = ImportArtifact(wire.alias)
	src, err := decodeCtyValue(wire.Sources)
	if err != nil {
		return err
	}
	i.Sources = src
	return nil
}

// MarshalJSON preserves cty.Value fields on QCConfig across JSON RPCs.
func (q QCConfig) MarshalJSON() ([]byte, error) {
	type alias QCConfig
	crit, err := encodeCtyValue(q.Criteria)
	if err != nil {
		return nil, err
	}
	sb, err := encodeCtyValue(q.Sandbox)
	if err != nil {
		return nil, err
	}
	har, err := encodeCtyValue(q.Harness)
	if err != nil {
		return nil, err
	}
	mp, err := encodeCtyValue(q.ModelProvider)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		alias
		Criteria      *wireCtyValue `json:"Criteria,omitempty"`
		Sandbox       *wireCtyValue `json:"Sandbox,omitempty"`
		Harness       *wireCtyValue `json:"Harness,omitempty"`
		ModelProvider *wireCtyValue `json:"ModelProvider,omitempty"`
	}{
		alias:         alias(q),
		Criteria:      crit,
		Sandbox:       sb,
		Harness:       har,
		ModelProvider: mp,
	})
}

// UnmarshalJSON restores cty.Value fields on QCConfig from JSON.
func (q *QCConfig) UnmarshalJSON(data []byte) error {
	type alias QCConfig
	var wire struct {
		alias
		Criteria      *wireCtyValue `json:"Criteria,omitempty"`
		Sandbox       *wireCtyValue `json:"Sandbox,omitempty"`
		Harness       *wireCtyValue `json:"Harness,omitempty"`
		ModelProvider *wireCtyValue `json:"ModelProvider,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*q = QCConfig(wire.alias)
	var err error
	if q.Criteria, err = decodeCtyValue(wire.Criteria); err != nil {
		return err
	}
	if q.Sandbox, err = decodeCtyValue(wire.Sandbox); err != nil {
		return err
	}
	if q.Harness, err = decodeCtyValue(wire.Harness); err != nil {
		return err
	}
	if q.ModelProvider, err = decodeCtyValue(wire.ModelProvider); err != nil {
		return err
	}
	return nil
}

// MarshalJSON preserves cty.Value fields on Config across Dapr State Store and Service Invocation JSON RPCs.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config
	bs, err := encodeCtyValue(c.BasedSource)
	if err != nil {
		return nil, err
	}
	sb, err := encodeCtyValue(c.Sandbox)
	if err != nil {
		return nil, err
	}
	har, err := encodeCtyValue(c.Harness)
	if err != nil {
		return nil, err
	}
	mp, err := encodeCtyValue(c.ModelProvider)
	if err != nil {
		return nil, err
	}
	sp, err := encodeCtyValue(c.SystemPrompt)
	if err != nil {
		return nil, err
	}
	pr, err := encodeCtyValue(c.Prompt)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		alias
		BasedSource   *wireCtyValue `json:"BasedSource,omitempty"`
		Sandbox       *wireCtyValue `json:"Sandbox,omitempty"`
		Harness       *wireCtyValue `json:"Harness,omitempty"`
		ModelProvider *wireCtyValue `json:"ModelProvider,omitempty"`
		SystemPrompt  *wireCtyValue `json:"SystemPrompt,omitempty"`
		Prompt        *wireCtyValue `json:"Prompt,omitempty"`
	}{
		alias:         alias(c),
		BasedSource:   bs,
		Sandbox:       sb,
		Harness:       har,
		ModelProvider: mp,
		SystemPrompt:  sp,
		Prompt:        pr,
	})
}

// UnmarshalJSON restores cty.Value fields on Config from JSON.
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config
	var wire struct {
		alias
		BasedSource   *wireCtyValue `json:"BasedSource,omitempty"`
		Sandbox       *wireCtyValue `json:"Sandbox,omitempty"`
		Harness       *wireCtyValue `json:"Harness,omitempty"`
		ModelProvider *wireCtyValue `json:"ModelProvider,omitempty"`
		SystemPrompt  *wireCtyValue `json:"SystemPrompt,omitempty"`
		Prompt        *wireCtyValue `json:"Prompt,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = Config(wire.alias)
	var err error
	if c.BasedSource, err = decodeCtyValue(wire.BasedSource); err != nil {
		return err
	}
	if c.Sandbox, err = decodeCtyValue(wire.Sandbox); err != nil {
		return err
	}
	if c.Harness, err = decodeCtyValue(wire.Harness); err != nil {
		return err
	}
	if c.ModelProvider, err = decodeCtyValue(wire.ModelProvider); err != nil {
		return err
	}
	if c.SystemPrompt, err = decodeCtyValue(wire.SystemPrompt); err != nil {
		return err
	}
	if c.Prompt, err = decodeCtyValue(wire.Prompt); err != nil {
		return err
	}
	return nil
}
