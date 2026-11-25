package provider

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
)

type ModelProviderBlock struct {
	*golden.BaseBlock
	ProviderType         Type         `hcl:"type"`
	Endpoint             string       `hcl:"endpoint"`
	WireAPI              *WireAPI     `hcl:"wire_api,optional"`
	Transport            *Transport   `hcl:"transport,optional"`
	Headers              cty.Value    `hcl:"headers,optional"`
	APIKey               *string      `hcl:"api_key,optional"`
	APIKeyRef            *string      `hcl:"api_key_ref,optional"`
	BearerToken          *string      `hcl:"bearer_token,optional"`
	BearerTokenRef       *string      `hcl:"bearer_token_ref,optional"`
	RetryBlocks          []RetryBlock `hcl:"retry,block"`
	APIKeyAttribute      cty.Value
	BearerTokenAttribute cty.Value
	planned              Config
	plannedRetry         RetryPolicy
}

type RetryBlock struct {
	LifecycleRetries   *int     `hcl:"lifecycle_retries,optional"`
	ModelCallRetries   *int     `hcl:"model_call_retries,optional"`
	IntervalSeconds    *int     `hcl:"interval_seconds,optional"`
	MaxIntervalSeconds *int     `hcl:"max_interval_seconds,optional"`
	ErrorMessageRegex  []string `hcl:"error_message_regex,optional"`
}

func (b *ModelProviderBlock) Type() string            { return "" }
func (b *ModelProviderBlock) BlockType() string       { return "model_provider" }
func (b *ModelProviderBlock) AddressLength() int      { return 2 }
func (b *ModelProviderBlock) CanExecutePrePlan() bool { return false }

func (b *ModelProviderBlock) BaseDecode(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	for _, name := range []string{"type", "endpoint"} {
		attribute, exists := block.Body.Attributes[name]
		if !exists {
			continue
		}
		value, diags := attribute.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		unmarked, _ := value.UnmarkDeep()
		if unmarked.IsNull() || !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
			return fmt.Errorf("%s must be a known non-null string", name)
		}
	}
	for _, name := range []string{"wire_api", "transport", "api_key", "api_key_ref", "bearer_token", "bearer_token_ref"} {
		attribute, exists := block.Body.Attributes[name]
		if !exists {
			continue
		}
		value, diags := attribute.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		unmarked, _ := value.UnmarkDeep()
		if unmarked.IsNull() {
			continue
		}
		if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
			return fmt.Errorf("%s must be a known string or null", name)
		}
	}
	return nil
}

func (b *ModelProviderBlock) ExecuteDuringPlan() error {
	if len(b.RetryBlocks) > 1 {
		return fmt.Errorf("model provider must have at most one retry block")
	}
	config := Config{
		Type:           b.ProviderType,
		Endpoint:       b.Endpoint,
		WireAPI:        clonePointer(b.WireAPI),
		Transport:      clonePointer(b.Transport),
		Headers:        b.Headers,
		APIKey:         clonePointer(b.APIKey),
		APIKeyRef:      clonePointer(b.APIKeyRef),
		BearerToken:    clonePointer(b.BearerToken),
		BearerTokenRef: clonePointer(b.BearerTokenRef),
	}
	if len(b.RetryBlocks) == 1 {
		override, err := b.RetryBlocks[0].override()
		if err != nil {
			return err
		}
		config.Retry = override
	}
	headers, err := NormalizeHeaders(config.Headers)
	if err != nil {
		return err
	}
	config.Headers = headers
	if err := config.Validate(); err != nil {
		return err
	}
	policy, err := MergeRetry(DefaultRetryPolicy(), config.Retry)
	if err != nil {
		return err
	}
	b.planned = config.Clone()
	b.plannedRetry = policy
	b.APIKeyAttribute = sensitiveString(b.APIKey)
	b.BearerTokenAttribute = sensitiveString(b.BearerToken)
	return nil
}

func (b *ModelProviderBlock) Value() cty.Value {
	if b.plannedRetry.Interval == 0 {
		return cty.DynamicVal
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(b.Address()),
		"kind":    cty.StringVal("model_provider"),
		"name":    cty.StringVal(b.Name()),
		"retry": cty.ObjectVal(map[string]cty.Value{
			"lifecycle_retries":    cty.NumberIntVal(int64(b.plannedRetry.LifecycleRetries)),
			"model_call_retries":   cty.NumberIntVal(int64(b.plannedRetry.ModelCallRetries)),
			"interval_seconds":     cty.NumberIntVal(int64(b.plannedRetry.Interval / time.Second)),
			"max_interval_seconds": cty.NumberIntVal(int64(b.plannedRetry.MaxInterval / time.Second)),
			"error_message_regex":  stringListValue(b.plannedRetry.ErrorMessageRegex),
		}),
	})
}

func (b *ModelProviderBlock) APIKeyValue() cty.Value {
	return sensitiveString(b.APIKey)
}

func (b *ModelProviderBlock) BearerTokenValue() cty.Value {
	return sensitiveString(b.BearerToken)
}

func (b *ModelProviderBlock) RetryPolicy() RetryPolicy { return b.plannedRetry }
func (b *ModelProviderBlock) ProviderConfig() Config   { return b.planned.Clone() }

func (b RetryBlock) override() (RetryOverride, error) {
	interval, err := secondsDuration(b.IntervalSeconds, "interval_seconds")
	if err != nil {
		return RetryOverride{}, err
	}
	maxInterval, err := secondsDuration(b.MaxIntervalSeconds, "max_interval_seconds")
	if err != nil {
		return RetryOverride{}, err
	}
	return RetryOverride{
		LifecycleRetries:  clonePointer(b.LifecycleRetries),
		ModelCallRetries:  clonePointer(b.ModelCallRetries),
		Interval:          interval,
		MaxInterval:       maxInterval,
		ErrorMessageRegex: append([]string{}, b.ErrorMessageRegex...),
	}, nil
}

func secondsDuration(value *int, name string) (*time.Duration, error) {
	if value == nil {
		return nil, nil
	}
	if *value < 0 || int64(*value) > int64((1<<63-1)/time.Second) {
		return nil, fmt.Errorf("%s is out of range", name)
	}
	duration := time.Duration(*value) * time.Second
	return &duration, nil
}

func sensitiveString(value *string) cty.Value {
	if value == nil {
		return cty.NilVal
	}
	return specpkg.MarkSensitive(cty.StringVal(*value))
}

func stringListValue(values []string) cty.Value {
	if len(values) == 0 {
		return cty.ListValEmpty(cty.String)
	}
	result := make([]cty.Value, len(values))
	for index, value := range values {
		result[index] = cty.StringVal(value)
	}
	return cty.ListVal(result)
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

var _ golden.PlanBlock = (*ModelProviderBlock)(nil)
var _ golden.SingleValueBlock = (*ModelProviderBlock)(nil)
var _ golden.BaseDecode = (*ModelProviderBlock)(nil)
