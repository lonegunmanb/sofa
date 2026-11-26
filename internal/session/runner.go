package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/provider"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

const defaultMaxTurnsSafetyLimit = 1000

// WorkSessionConfig configures a single Work session execution loop.
type WorkSessionConfig struct {
	Opener                runtime.Opener
	BaseSession           runtime.SessionConfig
	Prompt                string
	DiarySummary          string
	ToolExecutor          *toolruntime.Executor
	ToolBindings          []toolruntime.ToolBinding
	MCPResources          []toolruntime.MCPResourceSpec
	WritablePaths         []string
	CheckpointEveryRounds int
	MaxProtocolAttempts   int
	MaxTurns              int
	ToolCallQuota         map[string]int
	Fallbacks             []workerspec.FallbackConfig
	RetryPolicy           provider.RetryPolicy
	StallTimeout          time.Duration
	WatchdogCheckInterval time.Duration
	Hooks                 ControlHooks
	OnEvent               func(runtime.Event)
	Sleep                 func(ctx context.Context, d time.Duration) error
	Jitter                func(d time.Duration) time.Duration
}

// WorkResult summarizes the completed Work phase outcome, diaries, and quota state.
type WorkResult struct {
	SessionID      string
	Outcome        PhaseOutcome
	Diaries        []DiaryInput
	QuotaRemaining map[string]int
}

// QCSessionConfig configures a single QC audit session execution loop.
type QCSessionConfig struct {
	Opener                runtime.Opener
	BaseSession           runtime.SessionConfig
	Prompt                string
	Ledger                *QCLedger
	Criteria              map[string]string
	ToolExecutor          *toolruntime.Executor
	ToolBindings          []toolruntime.ToolBinding
	MCPResources          []toolruntime.MCPResourceSpec
	MaxProtocolAttempts   int
	MaxTurns              int
	ToolCallQuota         map[string]int
	RetryPolicy           provider.RetryPolicy
	StallTimeout          time.Duration
	WatchdogCheckInterval time.Duration
	Hooks                 ControlHooks
	OnEvent               func(runtime.Event)
	Sleep                 func(ctx context.Context, d time.Duration) error
	Jitter                func(d time.Duration) time.Duration
}

// QCResult summarizes the completed QC phase verdict and ledger state.
type QCResult struct {
	SessionID      string
	Verdict        QCVerdict
	OpenIssues     []QCIssue
	AllIssues      []QCIssue
	Outcome        PhaseOutcome
	QuotaRemaining map[string]int
}

// RunWorkPhase executes the full Work session lifecycle with protocol tools, four host gates,
// quota tracking/rollback, retry policy, and dual-barrier stall watchdog.
func RunWorkPhase(ctx context.Context, cfg WorkSessionConfig) (WorkResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Opener == nil {
		return WorkResult{}, errors.New("work session opener is required")
	}

	quota, err := NewQuotaTracker(cfg.ToolCallQuota)
	if err != nil {
		return WorkResult{}, err
	}
	gates := NewGateController(GateControllerConfig{
		Hooks:                 cfg.Hooks,
		Quota:                 quota,
		CheckpointEveryRounds: cfg.CheckpointEveryRounds,
		WritablePaths:         cfg.WritablePaths,
	})

	retryExec, err := buildRetryExecutor(cfg.RetryPolicy, cfg.Sleep, cfg.Jitter)
	if err != nil {
		return WorkResult{}, err
	}

	var (
		sessionMu     sync.RWMutex
		activeSession runtime.Session
	)
	readMCP := func(rctx context.Context, serverName, uri string) ([]runtime.MCPResourceContent, error) {
		sessionMu.RLock()
		s := activeSession
		sessionMu.RUnlock()
		if s == nil {
			return nil, errors.New("session is not open")
		}
		return s.ReadMCPResource(rctx, serverName, uri)
	}

	hasTerminate, customSpecs, customHandlers, err := buildCustomAndMCPTools(gates, cfg.ToolExecutor, cfg.ToolBindings, cfg.MCPResources, cfg.ToolCallQuota, cfg.WritablePaths, readMCP)
	if err != nil {
		return WorkResult{}, err
	}

	protoSpecs, protoHandlers := WorkProtocolTools(gates, WorkProtocolOptions{
		HasCustomTerminate:      hasTerminate,
		AgentEscalatedFallbacks: collectAgentEscalatedFallbacks(cfg.Fallbacks),
		WritablePaths:           cfg.WritablePaths,
	})

	sessionCfg := assembleSessionConfig(cfg.BaseSession, gates, protoSpecs, protoHandlers, customSpecs, customHandlers)

	sess, _, err := retryExec.OpenWithRetry(ctx, cfg.Opener, sessionCfg, cfg.DiarySummary)
	if err != nil {
		return WorkResult{}, err
	}
	sessionMu.Lock()
	activeSession = sess
	sessionMu.Unlock()
	defer func() {
		_ = sess.Close(context.Background())
	}()

	maxProto := cfg.MaxProtocolAttempts
	if maxProto <= 0 {
		maxProto = workerspec.DefaultMaxProtocolAttempts
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurnsSafetyLimit
	}

	outcome, err := driveTurnLoop(ctx, turnLoopParams{
		Session:             sess,
		Gates:               gates,
		RetryExec:           retryExec,
		InitialPrompt:       cfg.Prompt,
		MaxProtocolAttempts: maxProto,
		MaxTurns:            maxTurns,
		StallTimeout:        cfg.StallTimeout,
		CheckInterval:       cfg.WatchdogCheckInterval,
		OnEvent:             cfg.OnEvent,
		AllowHumanPause:     true,
		HasCustomTerminate:  hasTerminate,
		IsQC:                false,
	})
	if err != nil {
		return WorkResult{
			SessionID:      sess.ID(),
			Diaries:        gates.Diaries(),
			QuotaRemaining: quota.Snapshot(),
		}, err
	}

	return WorkResult{
		SessionID:      sess.ID(),
		Outcome:        outcome,
		Diaries:        gates.Diaries(),
		QuotaRemaining: quota.Snapshot(),
	}, nil
}

// RunQCPhase executes the QC audit session lifecycle backed by the host-owned QCLedger.
func RunQCPhase(ctx context.Context, cfg QCSessionConfig) (QCResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Opener == nil {
		return QCResult{}, errors.New("qc session opener is required")
	}

	ledger := cfg.Ledger
	if ledger == nil {
		var err error
		ledger, err = NewQCLedger(cfg.Criteria)
		if err != nil {
			return QCResult{}, err
		}
	}

	quota, err := NewQuotaTracker(cfg.ToolCallQuota)
	if err != nil {
		return QCResult{}, err
	}
	gates := NewGateController(GateControllerConfig{
		Hooks: cfg.Hooks,
		Quota: quota,
	})

	retryExec, err := buildRetryExecutor(cfg.RetryPolicy, cfg.Sleep, cfg.Jitter)
	if err != nil {
		return QCResult{}, err
	}

	var (
		sessionMu     sync.RWMutex
		activeSession runtime.Session
	)
	readMCP := func(rctx context.Context, serverName, uri string) ([]runtime.MCPResourceContent, error) {
		sessionMu.RLock()
		s := activeSession
		sessionMu.RUnlock()
		if s == nil {
			return nil, errors.New("session is not open")
		}
		return s.ReadMCPResource(rctx, serverName, uri)
	}

	_, customSpecs, customHandlers, err := buildCustomAndMCPTools(gates, cfg.ToolExecutor, cfg.ToolBindings, cfg.MCPResources, cfg.ToolCallQuota, nil, readMCP)
	if err != nil {
		return QCResult{}, err
	}

	protoSpecs, protoHandlers := QCProtocolTools(gates, ledger)
	sessionCfg := assembleSessionConfig(cfg.BaseSession, gates, protoSpecs, protoHandlers, customSpecs, customHandlers)

	sess, _, err := retryExec.OpenWithRetry(ctx, cfg.Opener, sessionCfg, "")
	if err != nil {
		return QCResult{}, err
	}
	sessionMu.Lock()
	activeSession = sess
	sessionMu.Unlock()
	defer func() {
		_ = sess.Close(context.Background())
	}()

	maxProto := cfg.MaxProtocolAttempts
	if maxProto <= 0 {
		maxProto = workerspec.DefaultMaxProtocolAttempts
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurnsSafetyLimit
	}

	outcome, err := driveTurnLoop(ctx, turnLoopParams{
		Session:             sess,
		Gates:               gates,
		RetryExec:           retryExec,
		InitialPrompt:       cfg.Prompt,
		MaxProtocolAttempts: maxProto,
		MaxTurns:            maxTurns,
		StallTimeout:        cfg.StallTimeout,
		CheckInterval:       cfg.WatchdogCheckInterval,
		OnEvent:             cfg.OnEvent,
		AllowHumanPause:     false,
		IsQC:                true,
	})
	if err != nil {
		return QCResult{
			SessionID:      sess.ID(),
			OpenIssues:     ledger.OpenIssuesSnapshot(),
			AllIssues:      ledger.AllIssues(),
			QuotaRemaining: quota.Snapshot(),
		}, err
	}

	var verdict QCVerdict
	if outcome.QCVerdict != nil {
		verdict = *outcome.QCVerdict
	}

	return QCResult{
		SessionID:      sess.ID(),
		Verdict:        verdict,
		OpenIssues:     ledger.OpenIssuesSnapshot(),
		AllIssues:      ledger.AllIssues(),
		Outcome:        outcome,
		QuotaRemaining: quota.Snapshot(),
	}, nil
}

type turnLoopParams struct {
	Session             runtime.Session
	Gates               *GateController
	RetryExec           *RetryExecutor
	InitialPrompt       string
	MaxProtocolAttempts int
	MaxTurns            int
	StallTimeout        time.Duration
	CheckInterval       time.Duration
	OnEvent             func(runtime.Event)
	AllowHumanPause     bool
	HasCustomTerminate  bool
	IsQC                bool
}

func driveTurnLoop(ctx context.Context, p turnLoopParams) (PhaseOutcome, error) {
	scheduledSent := false
	nextPrompt := p.InitialPrompt
	protocolAttempts := 0
	turns := 0

	for turns < p.MaxTurns {
		// Pre-turn interjection check: deliver a dedicated interjection turn before sending scheduled prompt.
		if !scheduledSent {
			drained, err := p.Gates.DrainTurnInterjections(ctx)
			if err != nil {
				return PhaseOutcome{}, fmt.Errorf("drain pre-turn interjections: %w", err)
			}
			if len(drained) > 0 {
				interjectionPrompt := formatPreTurnInterjectionPrompt(drained, p.Gates.MustPauseForHuman())
				if err := runOneTurn(ctx, p, interjectionPrompt); err != nil {
					return PhaseOutcome{}, err
				}
				turns++
				if out, ok := p.Gates.Outcome(); ok {
					return out, nil
				}
				_ = p.Gates.EndTurn()
				if p.Gates.CheckpointPending() {
					if err := driveCheckpointBeforeScheduled(ctx, p, &turns, &protocolAttempts); err != nil {
						return PhaseOutcome{}, err
					}
					if out, ok := p.Gates.Outcome(); ok {
						return out, nil
					}
				}
				if p.Gates.MustPauseForHuman() {
					// Hold original scheduled prompt and drive Agent to call sofa_pause_for_human.
					scheduledSent = true
					nextPrompt = humanPauseReminderPrompt()
					continue
				}
				// Normal pre-turn interjection handled; proceed to send scheduled prompt next.
				continue
			}
		}

		if !scheduledSent {
			scheduledSent = true
		}

		wasCheckpointPending := p.Gates.CheckpointPending()
		wasMustPause := p.Gates.MustPauseForHuman()

		if err := runOneTurn(ctx, p, nextPrompt); err != nil {
			return PhaseOutcome{}, err
		}
		turns++

		if out, ok := p.Gates.Outcome(); ok {
			return out, nil
		}

		acc := p.Gates.EndTurn()

		switch {
		case p.Gates.CheckpointPending():
			if wasCheckpointPending && acc.CheckpointCalls == 0 {
				protocolAttempts++
			}
			nextPrompt = checkpointReminderPrompt()
		case p.Gates.MustPauseForHuman():
			if wasMustPause && acc.BusinessToolCalls == 0 && acc.CheckpointCalls == 0 {
				protocolAttempts++
			}
			nextPrompt = humanPauseReminderPrompt()
		default:
			if acc.RejectedHandoffs > 0 || (acc.BusinessToolCalls == 0 && acc.CheckpointCalls == 0) {
				protocolAttempts++
			}
			if p.IsQC {
				nextPrompt = qcProtocolReminderPrompt()
			} else {
				nextPrompt = workContinuationPrompt(p.HasCustomTerminate, acc.RejectedHandoffs > 0)
			}
		}

		if protocolAttempts > p.MaxProtocolAttempts {
			return PhaseOutcome{}, fmt.Errorf("%w (attempts=%d, max=%d)", ErrProtocolAttemptsExhausted, protocolAttempts, p.MaxProtocolAttempts)
		}
	}

	return PhaseOutcome{}, fmt.Errorf("%w: exceeded max turn safety ceiling (%d)", ErrProtocolAttemptsExhausted, p.MaxTurns)
}

func driveCheckpointBeforeScheduled(ctx context.Context, p turnLoopParams, turns, protocolAttempts *int) error {
	for p.Gates.CheckpointPending() && *turns < p.MaxTurns {
		if err := runOneTurn(ctx, p, checkpointReminderPrompt()); err != nil {
			return err
		}
		*turns++
		if p.Gates.RoundComplete() {
			return nil
		}
		acc := p.Gates.EndTurn()
		if p.Gates.CheckpointPending() && acc.CheckpointCalls == 0 {
			*protocolAttempts++
			if *protocolAttempts > p.MaxProtocolAttempts {
				return fmt.Errorf("%w (attempts=%d, max=%d)", ErrProtocolAttemptsExhausted, *protocolAttempts, p.MaxProtocolAttempts)
			}
		}
	}
	return nil
}

func runOneTurn(ctx context.Context, p turnLoopParams, prompt string) error {
	p.Gates.BeginTurn()
	_, err := p.RetryExec.RunTurnWithRetryAndWatchdog(ctx, p.Session, prompt, WatchdogTurnOptions{
		Gates:         p.Gates,
		StallTimeout:  p.StallTimeout,
		CheckInterval: p.CheckInterval,
		OnEvent:       p.OnEvent,
	})
	return err
}

func buildRetryExecutor(policy provider.RetryPolicy, sleep func(context.Context, time.Duration) error, jitter func(time.Duration) time.Duration) (*RetryExecutor, error) {
	if policy.Interval == 0 && policy.MaxInterval == 0 && policy.LifecycleRetries == 0 && policy.ModelCallRetries == 0 && len(policy.ErrorMessageRegex) == 0 {
		policy = provider.DefaultRetryPolicy()
	}
	exec, err := NewRetryExecutor(policy)
	if err != nil {
		return nil, err
	}
	if sleep != nil {
		exec.Sleep = sleep
	}
	if jitter != nil {
		exec.Jitter = jitter
	}
	return exec, nil
}

func buildCustomAndMCPTools(
	gates *GateController,
	exec *toolruntime.Executor,
	bindings []toolruntime.ToolBinding,
	mcpResources []toolruntime.MCPResourceSpec,
	quotaLimits map[string]int,
	writablePaths []string,
	readMCP toolruntime.MCPResourceReadFunc,
) (bool, []runtime.HostToolSpec, map[string]runtime.ToolHandler, error) {
	specs := make([]runtime.HostToolSpec, 0, len(bindings)+1)
	handlers := make(map[string]runtime.ToolHandler, len(bindings)+1)
	hasTerminate := false

	for _, b := range bindings {
		spec, err := toolruntime.HostToolSpec(b)
		if err != nil {
			return false, nil, nil, err
		}
		specs = append(specs, spec)
		quotaKey := resolveQuotaKey(b, quotaLimits)
		if b.Terminate {
			hasTerminate = true
			handlers[spec.Name] = WrapCustomTerminateHandler(gates, exec, b, quotaKey, writablePaths)
		} else {
			bindingCopy := b
			handlers[spec.Name] = gates.WrapHostTool(spec.Name, HostToolBusiness, quotaKey, func(ctx context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
				if exec == nil {
					msg := fmt.Sprintf("tool executor is not configured for tool %q", bindingCopy.EffectiveName())
					return runtime.ToolOutput{ResultType: runtime.HostToolResultFailure, TextResultForLLM: msg, HandlerError: msg}, nil
				}
				return exec.Handler(bindingCopy)(ctx, call)
			})
		}
	}

	if mcpTool, ok := toolruntime.NewMCPResourceReader(mcpResources, readMCP); ok {
		spec := mcpTool.HostToolSpec()
		specs = append(specs, spec)
		handlers[spec.Name] = gates.WrapHostTool(spec.Name, HostToolReadOnly, spec.Name, mcpTool.Handler())
	}

	return hasTerminate, specs, handlers, nil
}

func resolveQuotaKey(b toolruntime.ToolBinding, limits map[string]int) string {
	eff := b.EffectiveName()
	if _, ok := limits[eff]; ok {
		return eff
	}
	if b.Spec.ID != "" {
		if _, ok := limits[b.Spec.ID]; ok {
			return b.Spec.ID
		}
	}
	return eff
}

func assembleSessionConfig(
	base runtime.SessionConfig,
	gates *GateController,
	protoSpecs []runtime.HostToolSpec,
	protoHandlers map[string]runtime.ToolHandler,
	customSpecs []runtime.HostToolSpec,
	customHandlers map[string]runtime.ToolHandler,
) runtime.SessionConfig {
	cfg := base
	cfg.HostTools = make([]runtime.HostToolSpec, 0, len(base.HostTools)+len(protoSpecs)+len(customSpecs))
	cfg.HostTools = append(cfg.HostTools, base.HostTools...)
	cfg.HostTools = append(cfg.HostTools, customSpecs...)
	cfg.HostTools = append(cfg.HostTools, protoSpecs...)

	cfg.ToolHandlers = make(map[string]runtime.ToolHandler, len(base.ToolHandlers)+len(protoHandlers)+len(customHandlers))
	for k, h := range base.ToolHandlers {
		cfg.ToolHandlers[k] = h
	}
	for k, h := range customHandlers {
		cfg.ToolHandlers[k] = h
	}
	for k, h := range protoHandlers {
		cfg.ToolHandlers[k] = h
	}

	cfg.PreToolUseHook = gates.PreToolUse
	cfg.PostToolUseHook = gates.PostToolUse
	return cfg
}

func collectAgentEscalatedFallbacks(fallbacks []workerspec.FallbackConfig) []string {
	var names []string
	for _, fb := range fallbacks {
		if slices.Contains(fb.On, "agent_escalated") {
			names = append(names, fb.Name)
		}
	}
	return names
}

func formatPreTurnInterjectionPrompt(items []HumanInterjection, waitForConfirmation bool) string {
	var b strings.Builder
	b.WriteString("A human operator sent the following interjection before the next scheduled step:\n")
	for _, item := range items {
		fmt.Fprintf(&b, "- %s\n", item.Message)
	}
	if waitForConfirmation {
		b.WriteString("\nConfirmation is required: answer the human's question clearly (using read-only inspection if needed) and then call sofa_pause_for_human. Do not modify files or complete work yet.")
	} else {
		b.WriteString("\nAcknowledge and incorporate this guidance.")
	}
	return b.String()
}

func checkpointReminderPrompt() string {
	return "A periodic checkpoint is required before any further tool calls. Call sofa_checkpoint with a structured diary entry (completed, next_steps, and relevant evidence) now."
}

func humanPauseReminderPrompt() string {
	return "Human confirmation is required before proceeding. Provide your answer to the operator and call sofa_pause_for_human with a clear reason."
}

func workContinuationPrompt(hasCustomTerminate, rejectedHandoff bool) string {
	completeTool := ToolSofaCompleteWork
	if hasCustomTerminate {
		completeTool = "your configured terminate tool"
	}
	if rejectedHandoff {
		return fmt.Sprintf("Your previous handoff attempt was rejected. Resolve all reported issues and call %s (or sofa_request_decision / sofa_pause_for_human / sofa_escalate if applicable).", completeTool)
	}
	return fmt.Sprintf("Continue working on the task. When all work is verified and ready, call %s (or sofa_request_decision / sofa_pause_for_human if human input is required).", completeTool)
}

func qcProtocolReminderPrompt() string {
	return "Continue your QC audit. Record any defects with sofa_qc_open_issues, verify and close fixed issues with sofa_qc_resolve_issues, and conclude by calling sofa_qc_submit_verdict."
}
