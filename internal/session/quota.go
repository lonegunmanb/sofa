package session

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
)

// ErrQuotaExhausted is returned when a tool's configured tool_call_quota reaches zero.
var ErrQuotaExhausted = errors.New("tool call quota exhausted")

// QuotaTracker enforces per-tool call quotas with thread-safe reservation and clamped rollback.
type QuotaTracker struct {
	mu        sync.Mutex
	limits    map[string]int
	remaining map[string]int
}

// NewQuotaTracker initializes a QuotaTracker from a map of tool call limits.
// Tools absent from limits are unlimited.
func NewQuotaTracker(limits map[string]int) (*QuotaTracker, error) {
	lim := make(map[string]int, len(limits))
	rem := make(map[string]int, len(limits))
	for k, v := range limits {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("tool_call_quota must not contain an empty tool name")
		}
		if v < 0 {
			return nil, fmt.Errorf("tool_call_quota for %q must be non-negative, got %d", k, v)
		}
		lim[k] = v
		rem[k] = v
	}
	return &QuotaTracker{
		limits:    lim,
		remaining: rem,
	}, nil
}

// Reserve decrements remaining quota for toolName if it is configured in tool_call_quota.
func (q *QuotaTracker) Reserve(toolName string) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	limit, ok := q.limits[toolName]
	if !ok {
		return nil
	}
	rem := q.remaining[toolName]
	if rem <= 0 {
		return fmt.Errorf("%w for %q (limit=%d)", ErrQuotaExhausted, toolName, limit)
	}
	q.remaining[toolName] = rem - 1
	return nil
}

// Rollback restores one reserved call for toolName, clamped to its initial configured limit.
func (q *QuotaTracker) Rollback(toolName string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	limit, ok := q.limits[toolName]
	if !ok {
		return
	}
	if q.remaining[toolName] < limit {
		q.remaining[toolName]++
	}
}

// Remaining reports the remaining quota for toolName and whether it is quota-limited.
func (q *QuotaTracker) Remaining(toolName string) (int, bool) {
	if q == nil {
		return 0, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	rem, ok := q.remaining[toolName]
	return rem, ok
}

// Snapshot returns a copy of remaining quotas for all configured tools.
func (q *QuotaTracker) Snapshot() map[string]int {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return maps.Clone(q.remaining)
}
