package provider

import (
	"fmt"
	"regexp"
	"time"
)

type RetryOverride struct {
	LifecycleRetries  *int
	ModelCallRetries  *int
	Interval          *time.Duration
	MaxInterval       *time.Duration
	ErrorMessageRegex []string
}

type RetryPolicy struct {
	LifecycleRetries  int
	ModelCallRetries  int
	Interval          time.Duration
	MaxInterval       time.Duration
	ErrorMessageRegex []string
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		LifecycleRetries:  10,
		ModelCallRetries:  5,
		Interval:          10 * time.Second,
		MaxInterval:       180 * time.Second,
		ErrorMessageRegex: []string{},
	}
}

func (o RetryOverride) Validate() error {
	if o.LifecycleRetries != nil && *o.LifecycleRetries < 0 {
		return fmt.Errorf("lifecycle retries must not be negative")
	}
	if o.ModelCallRetries != nil && *o.ModelCallRetries < 0 {
		return fmt.Errorf("model call retries must not be negative")
	}
	if o.Interval != nil && *o.Interval <= 0 {
		return fmt.Errorf("retry interval must be positive")
	}
	if o.MaxInterval != nil && *o.MaxInterval < 0 {
		return fmt.Errorf("maximum retry interval must not be negative")
	}
	if o.Interval != nil && o.MaxInterval != nil && *o.MaxInterval < *o.Interval {
		return fmt.Errorf("maximum retry interval must not be less than retry interval")
	}
	for _, expression := range o.ErrorMessageRegex {
		if _, err := regexp.Compile(expression); err != nil {
			return fmt.Errorf("compile error message regex %q: %w", expression, err)
		}
	}
	return nil
}

func MergeRetry(base RetryPolicy, override RetryOverride) (RetryPolicy, error) {
	if err := override.Validate(); err != nil {
		return RetryPolicy{}, err
	}
	result := base
	result.ErrorMessageRegex = append(append([]string{}, base.ErrorMessageRegex...), override.ErrorMessageRegex...)
	if override.LifecycleRetries != nil {
		result.LifecycleRetries = *override.LifecycleRetries
	}
	if override.ModelCallRetries != nil {
		result.ModelCallRetries = *override.ModelCallRetries
	}
	if override.Interval != nil {
		result.Interval = *override.Interval
	}
	if override.MaxInterval != nil {
		result.MaxInterval = *override.MaxInterval
	}
	if result.MaxInterval < result.Interval {
		return RetryPolicy{}, fmt.Errorf("maximum retry interval must not be less than retry interval")
	}
	return result, nil
}
