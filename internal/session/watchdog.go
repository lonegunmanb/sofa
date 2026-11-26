package session

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/provider"
)

var (
	// ErrStallRecoveryExhausted is returned when StallWatchdog exhausts LifecycleRetries.
	ErrStallRecoveryExhausted = errors.New("session stall recovery retries exhausted")
	// ErrProtocolAttemptsExhausted is returned when the model fails to satisfy protocol gates within max_protocol_attempts.
	ErrProtocolAttemptsExhausted = errors.New("max protocol attempts exhausted")
)

const defaultContinuationPrompt = "Your previous turn stalled and the session was resumed. Inspect current workspace and continue from where you left off."

// WatchdogTurnOptions configures RunTurnWithRetryAndWatchdog.
type WatchdogTurnOptions struct {
	Gates              *GateController
	StallTimeout       time.Duration
	CheckInterval      time.Duration
	ContinuationPrompt string
	OnEvent            func(runtime.Event)
}

// RetryExecutor applies provider.RetryPolicy to Opener.Open and Session.RunTurn,
// including SESSION_NOT_FOUND fresh-session diary replay fallback and StallWatchdog recovery.
type RetryExecutor struct {
	Policy  provider.RetryPolicy
	Sleep   func(ctx context.Context, d time.Duration) error
	Jitter  func(d time.Duration) time.Duration
	regexes []*regexp.Regexp
}

// NewRetryExecutor compiles ErrorMessageRegex patterns and constructs a RetryExecutor.
func NewRetryExecutor(policy provider.RetryPolicy) (*RetryExecutor, error) {
	regexes := make([]*regexp.Regexp, 0, len(policy.ErrorMessageRegex))
	for _, expr := range policy.ErrorMessageRegex {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile error_message_regex %q: %w", expr, err)
		}
		regexes = append(regexes, re)
	}
	return &RetryExecutor{
		Policy:  policy,
		Sleep:   defaultSleep,
		Jitter:  defaultJitter,
		regexes: regexes,
	}, nil
}

// OpenWithRetry opens or resumes a session with LifecycleRetries backoff, falling back to a fresh
// session with injected historical diary summary when ResumeSessionID yields ErrorCategorySessionNotFound.
func (r *RetryExecutor) OpenWithRetry(
	ctx context.Context,
	opener harnessruntimeOpener,
	cfg runtime.SessionConfig,
	diarySummary string,
) (runtime.Session, runtime.SessionConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opener == nil {
		return nil, cfg, errors.New("session opener is nil")
	}

	effectiveCfg := cfg
	resumeFallbackUsed := false
	attempt := 0

	for {
		sess, err := opener.Open(ctx, effectiveCfg)
		if err == nil {
			return sess, effectiveCfg, nil
		}
		if ctx.Err() != nil {
			return nil, effectiveCfg, ctx.Err()
		}

		if effectiveCfg.ResumeSessionID != "" && !resumeFallbackUsed && isCategory(err, runtime.ErrorCategorySessionNotFound) {
			resumeFallbackUsed = true
			effectiveCfg.ResumeSessionID = ""
			if strings.TrimSpace(diarySummary) != "" {
				effectiveCfg.SystemPrompt = appendContext(effectiveCfg.SystemPrompt, diarySummary)
			}
			continue
		}

		if isPermanentError(err) || !r.isOpenRetryable(err) || attempt >= r.Policy.LifecycleRetries {
			return nil, effectiveCfg, err
		}

		if sleepErr := r.sleepBackoff(ctx, attempt); sleepErr != nil {
			return nil, effectiveCfg, sleepErr
		}
		attempt++
	}
}

type harnessruntimeOpener = runtime.Opener

// RunTurnWithRetryAndWatchdog executes a session turn protected by StallWatchdog and RetryPolicy.
func (r *RetryExecutor) RunTurnWithRetryAndWatchdog(
	ctx context.Context,
	sess runtime.Session,
	prompt string,
	opts WatchdogTurnOptions,
) (runtime.TurnSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess == nil {
		return runtime.TurnSummary{}, errors.New("session is nil")
	}

	contPrompt := strings.TrimSpace(opts.ContinuationPrompt)
	if contPrompt == "" {
		contPrompt = defaultContinuationPrompt
	}

	currentPrompt := prompt
	modelAttempt := 0
	stallRecoveries := 0

	for {
		summary, stalled, err := r.runSingleTurnWithWatchdog(ctx, sess, currentPrompt, opts)
		if opts.Gates != nil {
			opts.Gates.AbortTurnCleanup(stalled || err != nil)
		}
		if ctx.Err() != nil {
			return runtime.TurnSummary{}, ctx.Err()
		}

		if stalled {
			// Dual barrier: RunTurn has returned AND we wait for all active host tool callbacks to reach 0.
			if opts.Gates != nil {
				if waitErr := opts.Gates.WaitForZeroActiveTools(ctx); waitErr != nil {
					return runtime.TurnSummary{}, waitErr
				}
			}
			if stallRecoveries >= r.Policy.LifecycleRetries {
				return runtime.TurnSummary{}, fmt.Errorf("%w (%d/%d): %v", ErrStallRecoveryExhausted, stallRecoveries, r.Policy.LifecycleRetries, err)
			}
			stallRecoveries++
			if resumeErr := sess.Resume(ctx); resumeErr != nil {
				return runtime.TurnSummary{}, fmt.Errorf("resume stalled session: %w", resumeErr)
			}
			currentPrompt = contPrompt
			continue
		}

		if err == nil {
			return summary, nil
		}

		if isPermanentError(err) || !r.isModelCallRetryable(err) || modelAttempt >= r.Policy.ModelCallRetries {
			return runtime.TurnSummary{}, err
		}
		if sleepErr := r.sleepBackoff(ctx, modelAttempt); sleepErr != nil {
			return runtime.TurnSummary{}, sleepErr
		}
		modelAttempt++
	}
}

func (r *RetryExecutor) runSingleTurnWithWatchdog(
	ctx context.Context,
	sess runtime.Session,
	prompt string,
	opts WatchdogTurnOptions,
) (runtime.TurnSummary, bool, error) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	wrappedEvent := func(ev runtime.Event) {
		lastActivity.Store(time.Now().UnixNano())
		if opts.Gates != nil {
			opts.Gates.TouchActivity()
		}
		if opts.OnEvent != nil {
			opts.OnEvent(ev)
		}
	}

	var stalled atomic.Bool
	var turnFinished atomic.Bool
	var abortMu sync.Mutex
	stopWatch := make(chan struct{})
	var watchWg sync.WaitGroup

	if opts.StallTimeout > 0 {
		checkInterval := opts.CheckInterval
		if checkInterval <= 0 {
			checkInterval = opts.StallTimeout / 4
			if checkInterval < 5*time.Millisecond {
				checkInterval = 5 * time.Millisecond
			}
			if checkInterval > 100*time.Millisecond {
				checkInterval = 100 * time.Millisecond
			}
		}

		watchWg.Add(1)
		go func() {
			defer watchWg.Done()
			ticker := time.NewTicker(checkInterval)
			defer ticker.Stop()

			for {
				select {
				case <-stopWatch:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if opts.Gates != nil && opts.Gates.ActiveHostToolCalls() > 0 {
						lastActivity.Store(time.Now().UnixNano())
						continue
					}
					lastNs := lastActivity.Load()
					if opts.Gates != nil {
						if gateNs := opts.Gates.LastActivity().UnixNano(); gateNs > lastNs {
							lastNs = gateNs
						}
					}
					if time.Since(time.Unix(0, lastNs)) >= opts.StallTimeout {
						abortMu.Lock()
						if turnFinished.Load() {
							abortMu.Unlock()
							return
						}
						stalled.Store(true)
						abortCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
						_ = sess.Abort(abortCtx)
						cancel()
						abortMu.Unlock()
						return
					}
				}
			}
		}()
	}

	type turnResult struct {
		summary runtime.TurnSummary
		err     error
	}
	doneCh := make(chan turnResult, 1)
	go func() {
		sum, err := sess.RunTurn(ctx, prompt, wrappedEvent)
		abortMu.Lock()
		turnFinished.Store(true)
		abortMu.Unlock()
		doneCh <- turnResult{summary: sum, err: err}
	}()

	res := <-doneCh
	close(stopWatch)
	watchWg.Wait()

	return res.summary, stalled.Load(), res.err
}

func (r *RetryExecutor) sleepBackoff(ctx context.Context, attempt int) error {
	delay := r.backoffDelay(attempt)
	if delay <= 0 {
		return nil
	}
	sleepFn := r.Sleep
	if sleepFn == nil {
		sleepFn = defaultSleep
	}
	return sleepFn(ctx, delay)
}

func (r *RetryExecutor) backoffDelay(attempt int) time.Duration {
	if r.Policy.Interval <= 0 {
		return 0
	}
	delay := r.Policy.Interval
	for i := 0; i < attempt; i++ {
		if delay >= r.Policy.MaxInterval && r.Policy.MaxInterval > 0 {
			delay = r.Policy.MaxInterval
			break
		}
		if delay > (1<<62)/2 {
			delay = r.Policy.MaxInterval
			break
		}
		delay *= 2
	}
	if r.Policy.MaxInterval > 0 && delay > r.Policy.MaxInterval {
		delay = r.Policy.MaxInterval
	}
	jitterFn := r.Jitter
	if jitterFn == nil {
		jitterFn = defaultJitter
	}
	if j := jitterFn(delay); j > 0 {
		delay += j
		if r.Policy.MaxInterval > 0 && delay > r.Policy.MaxInterval {
			delay = r.Policy.MaxInterval
		}
	}
	return delay
}

func (r *RetryExecutor) isModelCallRetryable(err error) bool {
	if isCategory(err, runtime.ErrorCategoryTransient) || isCategory(err, runtime.ErrorCategoryRateLimit) {
		return true
	}
	return r.matchesRegex(err.Error())
}

func (r *RetryExecutor) isOpenRetryable(err error) bool {
	if isCategory(err, runtime.ErrorCategoryTransient) || isCategory(err, runtime.ErrorCategoryRateLimit) {
		return true
	}
	if r.matchesRegex(err.Error()) {
		return true
	}
	var he *runtime.HarnessError
	return !errors.As(err, &he)
}

func (r *RetryExecutor) matchesRegex(msg string) bool {
	for _, re := range r.regexes {
		if re.MatchString(msg) {
			return true
		}
	}
	return false
}

func isCategory(err error, cat runtime.ErrorCategory) bool {
	var he *runtime.HarnessError
	return errors.As(err, &he) && he.Category == cat
}

func isPermanentError(err error) bool {
	var he *runtime.HarnessError
	if !errors.As(err, &he) {
		return false
	}
	switch he.Category {
	case runtime.ErrorCategoryAuthentication,
		runtime.ErrorCategoryAuthorization,
		runtime.ErrorCategoryQuota,
		runtime.ErrorCategoryContextLimit,
		runtime.ErrorCategoryUnsupportedCapability:
		return true
	default:
		return false
	}
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	quarter := d / 4
	if quarter <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(quarter)))
}
