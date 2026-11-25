package provider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultRetryPolicy(t *testing.T) {
	policy := DefaultRetryPolicy()
	require.Equal(t, 10, policy.LifecycleRetries)
	require.Equal(t, 5, policy.ModelCallRetries)
	require.Equal(t, 10*time.Second, policy.Interval)
	require.Equal(t, 180*time.Second, policy.MaxInterval)
}

func TestRetryOverrideValidationAndMerge(t *testing.T) {
	lifecycle, modelCalls, interval, maxInterval := 3, 4, 2, 20
	policy, err := MergeRetry(DefaultRetryPolicy(), RetryOverride{
		LifecycleRetries:  &lifecycle,
		ModelCallRetries:  &modelCalls,
		Interval:          seconds(interval),
		MaxInterval:       seconds(maxInterval),
		ErrorMessageRegex: []string{"timeout"},
	})
	require.NoError(t, err)
	require.Equal(t, 3, policy.LifecycleRetries)
	require.Equal(t, 4, policy.ModelCallRetries)
	require.Equal(t, 2*time.Second, policy.Interval)
	require.Equal(t, 20*time.Second, policy.MaxInterval)
}

func TestRetryOverrideRejectsInvalidValues(t *testing.T) {
	negative := -1
	interval, maxInterval := 20, 10
	require.ErrorContains(t, RetryOverride{LifecycleRetries: &negative}.Validate(), "negative")
	require.ErrorContains(t, RetryOverride{Interval: seconds(interval), MaxInterval: seconds(maxInterval)}.Validate(), "less than")
	require.ErrorContains(t, RetryOverride{ErrorMessageRegex: []string{"["}}.Validate(), "compile")
}

func seconds(value int) *time.Duration {
	duration := time.Duration(value) * time.Second
	return &duration
}
