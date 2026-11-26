package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
)

// OutcomeStatus classifies the completed state of a Work or QC session phase.
type OutcomeStatus string

const (
	OutcomeSucceeded      OutcomeStatus = "succeeded"
	OutcomeWaitingHuman   OutcomeStatus = "waiting_human"
	OutcomeAgentEscalated OutcomeStatus = "agent_escalated"
	OutcomeQCCompleted    OutcomeStatus = "qc_completed"
)

// PhaseOutcome records the structured handoff/completion result emitted by a protocol or terminate tool.
type PhaseOutcome struct {
	Status          OutcomeStatus      `json:"status"`
	Summary         string             `json:"summary,omitempty"`
	ResultJSON      json.RawMessage    `json:"result_json,omitempty"`
	DecisionRequest *DecisionRequest   `json:"decision_request,omitempty"`
	PauseReason     string             `json:"pause_reason,omitempty"`
	Escalation      *EscalationRequest `json:"escalation,omitempty"`
	QCVerdict       *QCVerdict         `json:"qc_verdict,omitempty"`
}

// HostToolKind classifies how GateController gates and accounts for a host tool.
type HostToolKind int

const (
	HostToolBusiness HostToolKind = iota
	HostToolReadOnly
	HostToolCheckpoint
	HostToolHandoff
	HostToolCompleteWork
	HostToolCustomTerminate
)

// TurnAccounting summarizes tool activity observed during a single RunTurn.
type TurnAccounting struct {
	BusinessToolCalls int
	CheckpointCalls   int
	RejectedHandoffs  int
}

// GateControllerConfig configures a GateController for a Work or QC session.
type GateControllerConfig struct {
	Hooks                 ControlHooks
	Quota                 *QuotaTracker
	CheckpointEveryRounds int
	WritablePaths         []string
	OnActivity            func()
}

type nonHostReservation struct {
	toolName      string
	quotaReserved bool
}

// GateController enforces the four host safety gates across PreToolUse, PostToolUse, and InvokeHostTool.
type GateController struct {
	hooks                 ControlHooks
	quota                 *QuotaTracker
	checkpointEveryRounds int
	writablePaths         []string
	onActivity            func()

	mu                    sync.Mutex
	activeHostTools       int
	hostIdleCh            chan struct{}
	lastActivityUnixNano  int64
	mustPauseForHuman     bool
	checkpointPending     bool
	roundsSinceCheckpoint int
	roundComplete         bool
	hasOutcome            bool
	outcome               PhaseOutcome
	diaries               []DiaryInput

	turnBusinessTools    int
	turnCheckpointCalls  int
	turnRejectedHandoffs int

	inFlightNonHost map[string]nonHostReservation
	anonByTool      map[string]int
}

// NewGateController constructs a GateController with closed idle barrier and initialized activity timestamp.
func NewGateController(cfg GateControllerConfig) *GateController {
	hooks := cfg.Hooks
	if hooks == nil {
		hooks = NoopControlHooks{}
	}
	idle := make(chan struct{})
	close(idle)
	return &GateController{
		hooks:                 hooks,
		quota:                 cfg.Quota,
		checkpointEveryRounds: cfg.CheckpointEveryRounds,
		writablePaths:         slices.Clone(cfg.WritablePaths),
		onActivity:            cfg.OnActivity,
		hostIdleCh:            idle,
		lastActivityUnixNano:  time.Now().UnixNano(),
		inFlightNonHost:       make(map[string]nonHostReservation),
		anonByTool:            make(map[string]int),
	}
}

// TouchActivity refreshes the activity timestamp and triggers optional watchdog heartbeat callback.
func (g *GateController) TouchActivity() {
	g.mu.Lock()
	g.lastActivityUnixNano = time.Now().UnixNano()
	cb := g.onActivity
	g.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// LastActivity returns the latest recorded tool or session activity timestamp.
func (g *GateController) LastActivity() time.Time {
	g.mu.Lock()
	ns := g.lastActivityUnixNano
	g.mu.Unlock()
	return time.Unix(0, ns)
}

// ActiveHostToolCalls returns the current number of in-flight host tool invocations.
func (g *GateController) ActiveHostToolCalls() int {
	g.mu.Lock()
	n := g.activeHostTools
	g.mu.Unlock()
	return n
}

// ActiveToolCalls returns the total number of in-flight host and non-host tool invocations.
func (g *GateController) ActiveToolCalls() int {
	g.mu.Lock()
	n := g.activeHostTools + g.nonHostInFlightLocked()
	g.mu.Unlock()
	return n
}

// IdleCh returns a channel that is closed when ActiveHostToolCalls reaches zero.
func (g *GateController) IdleCh() <-chan struct{} {
	g.mu.Lock()
	ch := g.hostIdleCh
	g.mu.Unlock()
	return ch
}

// WaitForZeroActiveTools blocks until all in-flight host tool calls have completed or ctx is cancelled.
func (g *GateController) WaitForZeroActiveTools(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		g.mu.Lock()
		if g.activeHostTools == 0 {
			g.mu.Unlock()
			return nil
		}
		ch := g.hostIdleCh
		g.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *GateController) nonHostInFlightLocked() int {
	n := len(g.inFlightNonHost)
	for _, cnt := range g.anonByTool {
		n += cnt
	}
	return n
}

func (g *GateController) enterHostToolCallLocked() {
	if g.activeHostTools == 0 {
		g.hostIdleCh = make(chan struct{})
	}
	g.activeHostTools++
	g.lastActivityUnixNano = time.Now().UnixNano()
}

func (g *GateController) leaveHostToolCallLocked() {
	if g.activeHostTools <= 0 {
		return
	}
	g.activeHostTools--
	g.lastActivityUnixNano = time.Now().UnixNano()
	if g.activeHostTools == 0 {
		close(g.hostIdleCh)
	}
}

// AbortTurnCleanup clears any unclosed non-host tool reservations from an ended or aborted turn
// and rolls back their reserved quotas when turnFailed is true.
func (g *GateController) AbortTurnCleanup(turnFailed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleanupNonHostLocked(turnFailed)
}

func (g *GateController) cleanupNonHostLocked(rollbackQuota bool) {
	if rollbackQuota && g.quota != nil {
		for _, res := range g.inFlightNonHost {
			if res.quotaReserved {
				g.quota.Rollback(res.toolName)
			}
		}
		for toolName, cnt := range g.anonByTool {
			if _, limited := g.quota.Remaining(toolName); limited {
				for i := 0; i < cnt; i++ {
					g.quota.Rollback(toolName)
				}
			}
		}
	}
	if len(g.inFlightNonHost) > 0 {
		clear(g.inFlightNonHost)
	}
	if len(g.anonByTool) > 0 {
		clear(g.anonByTool)
	}
}

// BeginTurn resets per-turn counters before issuing a RunTurn.
func (g *GateController) BeginTurn() {
	g.mu.Lock()
	g.turnBusinessTools = 0
	g.turnCheckpointCalls = 0
	g.turnRejectedHandoffs = 0
	g.lastActivityUnixNano = time.Now().UnixNano()
	g.mu.Unlock()
}

// EndTurn finalizes round accounting after a RunTurn returns.
func (g *GateController) EndTurn() TurnAccounting {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleanupNonHostLocked(false)
	acc := TurnAccounting{
		BusinessToolCalls: g.turnBusinessTools,
		CheckpointCalls:   g.turnCheckpointCalls,
		RejectedHandoffs:  g.turnRejectedHandoffs,
	}
	if !g.roundComplete && g.turnBusinessTools > 0 {
		g.roundsSinceCheckpoint++
		if g.checkpointEveryRounds > 0 && g.roundsSinceCheckpoint >= g.checkpointEveryRounds {
			g.checkpointPending = true
		}
	}
	return acc
}

// CheckpointPending reports whether a periodic checkpoint is required before continuing.
func (g *GateController) CheckpointPending() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checkpointPending
}

// MustPauseForHuman reports whether a WaitForConfirmation human interjection requires pausing.
func (g *GateController) MustPauseForHuman() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mustPauseForHuman
}

// RoundComplete reports whether an accepted handoff or termination tool completed the phase.
func (g *GateController) RoundComplete() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.roundComplete
}

// Outcome returns the recorded PhaseOutcome if RoundComplete is true.
func (g *GateController) Outcome() (PhaseOutcome, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.outcome, g.hasOutcome
}

// Diaries returns a copy of all DiaryInput entries committed during this session.
func (g *GateController) Diaries() []DiaryInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.diaries)
}

// DrainTurnInterjections drains human interjections prior to issuing a RunTurn and updates Gate 1 state.
func (g *GateController) DrainTurnInterjections(ctx context.Context) ([]HumanInterjection, error) {
	items, err := g.hooks.DrainInterjections(ctx)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	g.mu.Lock()
	for _, item := range items {
		if item.WaitForConfirmation {
			g.mustPauseForHuman = true
		}
	}
	g.mu.Unlock()
	return items, nil
}

// PreToolUse enforces Gates 1-4 on non-host built-in and MCP tools.
func (g *GateController) PreToolUse(ctx context.Context, input runtime.PreToolUseInput) (runtime.PreToolUseOutput, error) {
	g.TouchActivity()

	items, err := g.hooks.DrainInterjections(ctx)
	if err != nil {
		return runtime.PreToolUseOutput{
			Allow:      false,
			DenyReason: fmt.Sprintf("drain human interjections: %v", err),
		}, nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	for _, item := range items {
		if item.WaitForConfirmation {
			g.mustPauseForHuman = true
		}
	}
	additionalCtx := formatInterjectionsContext(items)

	// Gate 3: roundComplete blocks trailing tool calls.
	if g.roundComplete {
		return runtime.PreToolUseOutput{
			Allow:             false,
			DenyReason:        "round already complete: finish your final response without calling additional tools",
			AdditionalContext: additionalCtx,
		}, nil
	}

	// Gate 2: checkpointPending locks all non-checkpoint tools.
	if g.checkpointPending {
		return runtime.PreToolUseOutput{
			Allow:             false,
			DenyReason:        "checkpoint required: call sofa_checkpoint with a structured diary entry before continuing",
			AdditionalContext: additionalCtx,
		}, nil
	}

	// Gate 1: mustPauseForHuman locks mutating tools until sofa_pause_for_human is called.
	if g.mustPauseForHuman && !isReadOnlyBuiltInTool(input.ToolName) {
		return runtime.PreToolUseOutput{
			Allow:             false,
			DenyReason:        "human confirmation required: answer the human's question and call sofa_pause_for_human before making changes",
			AdditionalContext: additionalCtx,
		}, nil
	}

	// Gate 4: Quota enforcement.
	quotaReserved := false
	if g.quota != nil {
		if err := g.quota.Reserve(input.ToolName); err != nil {
			return runtime.PreToolUseOutput{
				Allow:             false,
				DenyReason:        err.Error(),
				AdditionalContext: additionalCtx,
			}, nil
		}
		_, quotaReserved = g.quota.Remaining(input.ToolName)
	}

	key := correlationKey(input.ToolCallID, input.HookCallID)
	if key != "" {
		g.inFlightNonHost[key] = nonHostReservation{
			toolName:      input.ToolName,
			quotaReserved: quotaReserved,
		}
	} else {
		g.anonByTool[input.ToolName]++
	}
	g.lastActivityUnixNano = time.Now().UnixNano()
	g.turnBusinessTools++
	if g.checkpointEveryRounds > 0 && g.turnBusinessTools > g.checkpointEveryRounds*4 {
		g.checkpointPending = true
	}

	return runtime.PreToolUseOutput{
		Allow:             true,
		AdditionalContext: additionalCtx,
	}, nil
}

// PostToolUse completes active tool tracking and rolls back quota on tool execution failure.
func (g *GateController) PostToolUse(_ context.Context, input runtime.PostToolUseInput) error {
	g.TouchActivity()

	g.mu.Lock()
	defer g.mu.Unlock()

	key := correlationKey(input.ToolCallID, input.HookCallID)
	var res nonHostReservation
	found := false
	if key != "" {
		res, found = g.inFlightNonHost[key]
		if found {
			delete(g.inFlightNonHost, key)
		}
	} else if cnt := g.anonByTool[input.ToolName]; cnt > 0 {
		found = true
		if cnt == 1 {
			delete(g.anonByTool, input.ToolName)
		} else {
			g.anonByTool[input.ToolName] = cnt - 1
		}
		_, limited := g.quota.Remaining(input.ToolName)
		res = nonHostReservation{toolName: input.ToolName, quotaReserved: limited}
	}

	if !found {
		return nil
	}

	if !input.Succeeded && res.quotaReserved && g.quota != nil {
		g.quota.Rollback(res.toolName)
	}
	return nil
}

// WrapHostTool wraps any host-managed tool (protocol, typed tool, or MCP resource reader)
// with active tool accounting and Gates 1-4.
func (g *GateController) WrapHostTool(toolName string, kind HostToolKind, quotaKey string, handler runtime.ToolHandler) runtime.ToolHandler {
	return func(ctx context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		g.mu.Lock()
		g.enterHostToolCallLocked()
		g.mu.Unlock()
		if g.onActivity != nil {
			g.onActivity()
		}

		defer func() {
			g.mu.Lock()
			g.leaveHostToolCallLocked()
			g.mu.Unlock()
			if g.onActivity != nil {
				g.onActivity()
			}
		}()

		items, err := g.hooks.DrainInterjections(ctx)
		if err != nil {
			return gateDenyFailure(fmt.Sprintf("drain human interjections: %v", err)), nil
		}

		g.mu.Lock()
		for _, item := range items {
			if item.WaitForConfirmation {
				g.mustPauseForHuman = true
			}
		}
		interjectionCtx := formatInterjectionsContext(items)

		// Gate 3: roundComplete blocks trailing tool calls.
		if g.roundComplete {
			g.mu.Unlock()
			return gateDenyFailure(appendContext("round already complete: finish your final response without calling additional tools", interjectionCtx)), nil
		}

		// Gate 2: checkpointPending blocks all host tools except sofa_checkpoint.
		if g.checkpointPending && kind != HostToolCheckpoint {
			g.mu.Unlock()
			return gateDenyFailure(appendContext("checkpoint required: call sofa_checkpoint with a structured diary entry before continuing", interjectionCtx)), nil
		}

		// Gate 1: mustPauseForHuman locks mutating/completion/decision/escalate tools.
		if g.mustPauseForHuman && !isAllowedDuringHumanPause(toolName, kind) {
			g.mu.Unlock()
			return gateDenyFailure(appendContext("human confirmation required: answer the human's question and call sofa_pause_for_human before making changes", interjectionCtx)), nil
		}

		// Gate 3 concurrent barrier for handoff/termination tools (activeHostTools + nonHostInFlightLocked includes this call).
		if isHandoffKind(kind) && (g.activeHostTools+g.nonHostInFlightLocked()) > 1 {
			g.turnRejectedHandoffs++
			g.mu.Unlock()
			return modelProtocolRejection(appendContext("cannot complete or hand off round while other tool calls are still in flight; wait for concurrent tools to finish first", interjectionCtx)), nil
		}

		// Gate 4: Quota reservation.
		quotaReserved := false
		if quotaKey != "" && g.quota != nil {
			if err := g.quota.Reserve(quotaKey); err != nil {
				g.mu.Unlock()
				return gateDenyFailure(appendContext(err.Error(), interjectionCtx)), nil
			}
			_, quotaReserved = g.quota.Remaining(quotaKey)
		}
		g.mu.Unlock()

		out, callErr := handler(ctx, call)
		if callErr != nil || out.HandlerError != "" {
			if quotaReserved && g.quota != nil {
				g.quota.Rollback(quotaKey)
			}
			if interjectionCtx != "" {
				out.TextResultForLLM = appendContext(out.TextResultForLLM, interjectionCtx)
			}
			return out, callErr
		}

		if kind == HostToolBusiness || kind == HostToolReadOnly {
			g.mu.Lock()
			g.turnBusinessTools++
			if g.checkpointEveryRounds > 0 && g.turnBusinessTools > g.checkpointEveryRounds*4 {
				g.checkpointPending = true
			}
			g.mu.Unlock()
		}

		if interjectionCtx != "" {
			out.TextResultForLLM = appendContext(out.TextResultForLLM, interjectionCtx)
		}
		return out, nil
	}
}

func (g *GateController) recordRejectedHandoff() {
	g.mu.Lock()
	g.turnRejectedHandoffs++
	g.mu.Unlock()
}

func (g *GateController) completeCheckpoint(diary DiaryInput) {
	g.mu.Lock()
	g.checkpointPending = false
	g.roundsSinceCheckpoint = 0
	g.turnBusinessTools = 0
	g.turnCheckpointCalls++
	g.diaries = append(g.diaries, diary)
	g.mu.Unlock()
}

func (g *GateController) completeRound(outcome PhaseOutcome) {
	g.mu.Lock()
	if outcome.Status == OutcomeWaitingHuman {
		g.mustPauseForHuman = false
	}
	g.roundComplete = true
	g.hasOutcome = true
	g.outcome = outcome
	g.mu.Unlock()
}

func isHandoffKind(kind HostToolKind) bool {
	return kind == HostToolHandoff || kind == HostToolCompleteWork || kind == HostToolCustomTerminate
}

func isAllowedDuringHumanPause(toolName string, kind HostToolKind) bool {
	if kind == HostToolCheckpoint || kind == HostToolReadOnly {
		return true
	}
	return toolName == ToolSofaPauseForHuman || toolName == ToolSofaCheckpoint || toolName == "sofa_read_mcp_resource"
}

func isReadOnlyBuiltInTool(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "view", "read_file", "grep", "grep_search", "glob", "glob_files", "list_dir", "find_by_name", "ls", "search":
		return true
	default:
		return false
	}
}

func correlationKey(toolCallID, hookCallID string) string {
	if strings.TrimSpace(toolCallID) != "" {
		return "tc:" + toolCallID
	}
	if strings.TrimSpace(hookCallID) != "" {
		return "hc:" + hookCallID
	}
	return ""
}

func formatInterjectionsContext(items []HumanInterjection) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Human Interjection]")
	for _, item := range items {
		b.WriteString("\n- ")
		if item.ID != "" {
			fmt.Fprintf(&b, "(%s) ", item.ID)
		}
		b.WriteString(item.Message)
		if item.WaitForConfirmation {
			b.WriteString(" [WAIT_FOR_CONFIRMATION: answer the human and call sofa_pause_for_human]")
		}
	}
	return b.String()
}

func appendContext(base, extra string) string {
	if extra == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + "\n\n" + extra
}

func gateDenyFailure(reason string) runtime.ToolOutput {
	return runtime.ToolOutput{
		ResultType:       runtime.HostToolResultFailure,
		TextResultForLLM: reason,
	}
}
