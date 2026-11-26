package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/provider"
	"github.com/lonegunmanb/sofa/internal/session"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Scripted Test Doubles ---

type mockHooks struct {
	mu            sync.Mutex
	interjections [][]session.HumanInterjection
	drainCalls    int
	ckptErrs      []error
	ckptCalls     []session.DiaryInput
	unauthQueue   [][]string
}

func (h *mockHooks) DrainInterjections(_ context.Context) ([]session.HumanInterjection, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drainCalls++
	if len(h.interjections) == 0 {
		return nil, nil
	}
	next := h.interjections[0]
	h.interjections = h.interjections[1:]
	return next, nil
}

func (h *mockHooks) OnCheckpointRequested(_ context.Context, diary session.DiaryInput) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ckptCalls = append(h.ckptCalls, diary)
	if len(h.ckptErrs) > 0 {
		err := h.ckptErrs[0]
		h.ckptErrs = h.ckptErrs[1:]
		return err
	}
	return nil
}

func (h *mockHooks) ValidateWorkspaceForHandoff(_ context.Context) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.unauthQueue) == 0 {
		return nil, nil
	}
	next := h.unauthQueue[0]
	h.unauthQueue = h.unauthQueue[1:]
	return next, nil
}

type scriptedTurnAction func(ctx context.Context, s *scriptedSession, prompt string, onEvent func(runtime.Event)) (runtime.TurnSummary, error)

type scriptedSession struct {
	id         string
	cfg        runtime.SessionConfig
	turns      []scriptedTurnAction
	prompts    []string
	turnIdx    int
	abortCount atomic.Int32
	resumeCnt  atomic.Int32
	closed     atomic.Bool
	abortedCh  chan struct{}
	mu         sync.Mutex
}

func newScriptedSession(id string, cfg runtime.SessionConfig, turns ...scriptedTurnAction) *scriptedSession {
	return &scriptedSession{
		id:        id,
		cfg:       cfg,
		turns:     turns,
		abortedCh: make(chan struct{}, 8),
	}
}

func (s *scriptedSession) ID() string { return s.id }

func (s *scriptedSession) RunTurn(ctx context.Context, prompt string, onEvent func(runtime.Event)) (runtime.TurnSummary, error) {
	s.mu.Lock()
	idx := s.turnIdx
	s.turnIdx++
	s.prompts = append(s.prompts, prompt)
	var action scriptedTurnAction
	if idx < len(s.turns) {
		action = s.turns[idx]
	}
	s.mu.Unlock()

	if action == nil {
		return runtime.TurnSummary{TurnID: fmt.Sprintf("turn-%d", idx+1), FinalContent: "done"}, nil
	}
	return action(ctx, s, prompt, onEvent)
}

func (s *scriptedSession) Abort(_ context.Context) error {
	s.abortCount.Add(1)
	select {
	case s.abortedCh <- struct{}{}:
	default:
	}
	return nil
}

func (s *scriptedSession) Resume(_ context.Context) error {
	s.resumeCnt.Add(1)
	return nil
}

func (s *scriptedSession) ReadMCPResource(_ context.Context, serverName, uri string) ([]runtime.MCPResourceContent, error) {
	return []runtime.MCPResourceContent{{
		URI:      uri,
		MIMEType: "text/plain",
		Text:     "resource:" + serverName + ":" + uri,
	}}, nil
}

func (s *scriptedSession) Close(_ context.Context) error {
	s.closed.Store(true)
	return nil
}

type scriptedOpener struct {
	mu       sync.Mutex
	openFn   func(ctx context.Context, cfg runtime.SessionConfig, attempt int) (runtime.Session, error)
	opened   []runtime.SessionConfig
	attempts int
}

func (o *scriptedOpener) Capabilities(_ context.Context) (runtime.Capabilities, error) {
	return runtime.Capabilities{HarnessName: "scripted", SupportsSessionResume: true, SupportsMCPResources: true}, nil
}

func (o *scriptedOpener) Open(ctx context.Context, cfg runtime.SessionConfig) (runtime.Session, error) {
	o.mu.Lock()
	attempt := o.attempts
	o.attempts++
	o.opened = append(o.opened, cfg)
	fn := o.openFn
	o.mu.Unlock()
	return fn(ctx, cfg, attempt)
}

func callBuiltInTool(ctx context.Context, cfg runtime.SessionConfig, callID, name string, args any, succeeded bool) (runtime.PreToolUseOutput, error) {
	raw, _ := json.Marshal(args)
	out, err := cfg.PreToolUseHook(ctx, runtime.PreToolUseInput{
		HookCallID:    "hook-pre-" + callID,
		ToolCallID:    callID,
		ToolName:      name,
		ArgumentsJSON: raw,
	})
	if err != nil || !out.Allow {
		return out, err
	}
	if cfg.PostToolUseHook != nil {
		postErr := cfg.PostToolUseHook(ctx, runtime.PostToolUseInput{
			HookCallID: "hook-post-" + callID,
			ToolCallID: callID,
			ToolName:   name,
			Succeeded:  succeeded,
		})
		if postErr != nil {
			return out, postErr
		}
	}
	return out, nil
}

func callHostTool(ctx context.Context, cfg runtime.SessionConfig, callID, name string, args any) (runtime.ToolOutput, error) {
	raw, _ := json.Marshal(args)
	h, ok := cfg.ToolHandlers[name]
	if !ok {
		return runtime.ToolOutput{}, fmt.Errorf("missing host tool %q", name)
	}
	return h(ctx, runtime.ToolInvocation{
		ToolCallID:    callID,
		ToolName:      name,
		ArgumentsJSON: raw,
	})
}

func hasHostTool(specs []runtime.HostToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// --- Tests ---

func TestGate1_HumanInterjections_NormalAndWaitForConfirmation(t *testing.T) {
	ctx := context.Background()

	t.Run("normal interjection injects AdditionalContext without locking mutating tools", func(t *testing.T) {
		hooks := &mockHooks{
			interjections: [][]session.HumanInterjection{
				nil, // pre-turn drain
				{{ID: "ij-1", Message: "please keep comments concise", WaitForConfirmation: false}},
			},
		}
		var preOut runtime.PreToolUseOutput
		var bashOut runtime.PreToolUseOutput
		sess := newScriptedSession("s1", runtime.SessionConfig{}, func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			var err error
			preOut, err = callBuiltInTool(ctx, s.cfg, "tc-1", "view", map[string]any{"path": "README.md"}, true)
			require.NoError(t, err)
			bashOut, err = callBuiltInTool(ctx, s.cfg, "tc-2", "bash", map[string]any{"command": "go test ./..."}, true)
			require.NoError(t, err)
			out, err := callHostTool(ctx, s.cfg, "tc-3", "sofa_complete_work", map[string]any{"summary": "all done"})
			require.NoError(t, err)
			require.Equal(t, runtime.HostToolResultSuccess, out.ResultType)
			return runtime.TurnSummary{FinalContent: "done"}, nil
		})
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			sess.cfg = cfg
			return sess, nil
		}}

		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener:                opener,
			Prompt:                "implement feature",
			Hooks:                 hooks,
			CheckpointEveryRounds: 5,
		})
		require.NoError(t, err)
		assert.Equal(t, session.OutcomeSucceeded, res.Outcome.Status)
		assert.True(t, preOut.Allow)
		assert.Contains(t, preOut.AdditionalContext, "please keep comments concise")
		assert.True(t, bashOut.Allow)
	})

	t.Run("wait_for_confirmation in PreToolUse locks mutating tools and allows read-only and pause", func(t *testing.T) {
		hooks := &mockHooks{
			interjections: [][]session.HumanInterjection{
				nil, // pre-turn drain
				{{ID: "ij-2", Message: "Should we use PostgreSQL or SQLite?", WaitForConfirmation: true}},
			},
		}
		var bashDenied, editDenied, viewAllowed runtime.PreToolUseOutput
		var mcpAllowed runtime.ToolOutput
		sess := newScriptedSession("s2", runtime.SessionConfig{}, func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			var err error
			// Drains WaitForConfirmation interjection on bash -> immediately blocked!
			bashDenied, err = callBuiltInTool(ctx, s.cfg, "tc-1", "bash", map[string]any{"command": "rm -rf /"}, true)
			require.NoError(t, err)
			editDenied, err = callBuiltInTool(ctx, s.cfg, "tc-2", "edit", map[string]any{"file": "main.go"}, true)
			require.NoError(t, err)
			viewAllowed, err = callBuiltInTool(ctx, s.cfg, "tc-3", "view", map[string]any{"path": "go.mod"}, true)
			require.NoError(t, err)
			mcpAllowed, err = callHostTool(ctx, s.cfg, "tc-4", "sofa_read_mcp_resource", map[string]any{"server_name": "docs", "uri": "docs://arch"})
			require.NoError(t, err)
			// Trying to complete work while mustPauseForHuman is set must be rejected
			completeDenied, err := callHostTool(ctx, s.cfg, "tc-5", "sofa_complete_work", map[string]any{"summary": "skip"})
			require.NoError(t, err)
			assert.Equal(t, runtime.HostToolResultFailure, completeDenied.ResultType)

			pauseOut, err := callHostTool(ctx, s.cfg, "tc-6", "sofa_pause_for_human", map[string]any{"reason": "Answered PostgreSQL vs SQLite; awaiting confirmation"})
			require.NoError(t, err)
			require.Equal(t, runtime.HostToolResultSuccess, pauseOut.ResultType)
			return runtime.TurnSummary{FinalContent: "waiting for human"}, nil
		})
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			sess.cfg = cfg
			return sess, nil
		}}

		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener:                opener,
			Prompt:                "implement storage",
			Hooks:                 hooks,
			CheckpointEveryRounds: 5,
			MCPResources: []toolruntime.MCPResourceSpec{
				{ID: "mcp_resource_docs_1", ServerName: "docs", URI: "docs://arch"},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, session.OutcomeWaitingHuman, res.Outcome.Status)
		assert.False(t, bashDenied.Allow)
		assert.Contains(t, bashDenied.AdditionalContext, "Should we use PostgreSQL or SQLite?")
		assert.False(t, editDenied.Allow)
		assert.True(t, viewAllowed.Allow)
		assert.Equal(t, runtime.HostToolResultSuccess, mcpAllowed.ResultType)
	})

	t.Run("pre-turn wait_for_confirmation holds original prompt and pauses cleanly", func(t *testing.T) {
		hooks := &mockHooks{
			interjections: [][]session.HumanInterjection{
				{{ID: "ij-3", Message: "Confirm target API version before editing", WaitForConfirmation: true}},
			},
		}
		sess := newScriptedSession("s3", runtime.SessionConfig{}, func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			out, err := callHostTool(ctx, s.cfg, "tc-1", "sofa_pause_for_human", map[string]any{"reason": "Waiting on API version confirmation"})
			require.NoError(t, err)
			require.Equal(t, runtime.HostToolResultSuccess, out.ResultType)
			return runtime.TurnSummary{FinalContent: "v2 recommended; awaiting confirmation"}, nil
		})
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			sess.cfg = cfg
			return sess, nil
		}}

		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener:                opener,
			Prompt:                "ORIGINAL_SCHEDULED_PROMPT",
			Hooks:                 hooks,
			CheckpointEveryRounds: 5,
		})
		require.NoError(t, err)
		assert.Equal(t, session.OutcomeWaitingHuman, res.Outcome.Status)
		require.Len(t, sess.prompts, 1)
		assert.Contains(t, sess.prompts[0], "Confirm target API version before editing")
		assert.NotContains(t, sess.prompts[0], "ORIGINAL_SCHEDULED_PROMPT")
	})
}

func TestGate2_CheckpointPendingAndOnCheckpointRequested(t *testing.T) {
	ctx := context.Background()
	hooks := &mockHooks{
		ckptErrs: []error{errors.New("transient git push failed"), nil},
	}

	var bashBlockedOut, bashResumedOut runtime.PreToolUseOutput
	var ckptFailedOut, ckptOKOut runtime.ToolOutput

	sess := newScriptedSession("s-ckpt", runtime.SessionConfig{},
		// Round 1: 1 business tool call -> roundsSinceCheckpoint = 1
		func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			out, err := callBuiltInTool(ctx, s.cfg, "tc-1", "bash", map[string]any{"command": "echo r1"}, true)
			require.NoError(t, err)
			require.True(t, out.Allow)
			return runtime.TurnSummary{FinalContent: "r1 done"}, nil
		},
		// Round 2: 1 business tool call -> roundsSinceCheckpoint = 2 -> checkpointPending becomes true!
		func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			out, err := callBuiltInTool(ctx, s.cfg, "tc-2", "bash", map[string]any{"command": "echo r2"}, true)
			require.NoError(t, err)
			require.True(t, out.Allow)
			return runtime.TurnSummary{FinalContent: "r2 done"}, nil
		},
		// Round 3: runner prompts for checkpoint; bash is blocked; first sofa_checkpoint fails (stays locked!); second succeeds and unlocks bash!
		func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			var err error
			bashBlockedOut, err = callBuiltInTool(ctx, s.cfg, "tc-3", "bash", map[string]any{"command": "echo blocked"}, true)
			require.NoError(t, err)

			diary := map[string]any{
				"completed":  []string{"implemented step 1 and 2"},
				"next_steps": []string{"finish tests"},
			}
			ckptFailedOut, err = callHostTool(ctx, s.cfg, "tc-4", "sofa_checkpoint", diary)
			require.NoError(t, err)

			// Still locked after failed OnCheckpointRequested!
			stillBlocked, err := callBuiltInTool(ctx, s.cfg, "tc-5", "bash", map[string]any{"command": "echo still-blocked"}, true)
			require.NoError(t, err)
			assert.False(t, stillBlocked.Allow)

			ckptOKOut, err = callHostTool(ctx, s.cfg, "tc-6", "sofa_checkpoint", diary)
			require.NoError(t, err)

			bashResumedOut, err = callBuiltInTool(ctx, s.cfg, "tc-7", "bash", map[string]any{"command": "echo resumed"}, true)
			require.NoError(t, err)

			completeOut, err := callHostTool(ctx, s.cfg, "tc-8", "sofa_complete_work", map[string]any{"summary": "all finished"})
			require.NoError(t, err)
			require.Equal(t, runtime.HostToolResultSuccess, completeOut.ResultType)
			return runtime.TurnSummary{FinalContent: "done"}, nil
		},
	)
	opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
		sess.cfg = cfg
		return sess, nil
	}}

	res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
		Opener:                opener,
		Prompt:                "do work",
		Hooks:                 hooks,
		CheckpointEveryRounds: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, session.OutcomeSucceeded, res.Outcome.Status)
	assert.False(t, bashBlockedOut.Allow)
	assert.Contains(t, bashBlockedOut.DenyReason, "sofa_checkpoint")
	assert.Equal(t, runtime.HostToolResultFailure, ckptFailedOut.ResultType)
	assert.NotEmpty(t, ckptFailedOut.HandlerError)
	assert.Equal(t, runtime.HostToolResultSuccess, ckptOKOut.ResultType)
	assert.True(t, bashResumedOut.Allow)
	assert.Len(t, res.Diaries, 1)
}

func TestGate3_HandoffValidation_ConcurrentBarrier_TrailingTool_AndMaxAttempts(t *testing.T) {
	ctx := context.Background()

	t.Run("sofa_complete_work rejects unauthorized paths then succeeds after fix and blocks trailing tool", func(t *testing.T) {
		hooks := &mockHooks{
			unauthQueue: [][]string{
				{"outside/secret.txt"}, // turn 1 complete attempt -> rejected
				nil,                    // turn 2 complete attempt -> clean
			},
		}
		var firstComplete, secondComplete, trailingBash runtime.ToolOutput
		var trailingPre runtime.PreToolUseOutput
		sess := newScriptedSession("s-handoff", runtime.SessionConfig{},
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				var err error
				firstComplete, err = callHostTool(ctx, s.cfg, "tc-1", "sofa_complete_work", map[string]any{"summary": "try 1"})
				require.NoError(t, err)
				return runtime.TurnSummary{FinalContent: "oops"}, nil
			},
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				_, err := callBuiltInTool(ctx, s.cfg, "tc-2", "bash", map[string]any{"command": "rm outside/secret.txt"}, true)
				require.NoError(t, err)
				secondComplete, err = callHostTool(ctx, s.cfg, "tc-3", "sofa_complete_work", map[string]any{"summary": "try 2"})
				require.NoError(t, err)
				// Trailing built-in and host tools after roundComplete must be rejected
				trailingPre, err = callBuiltInTool(ctx, s.cfg, "tc-4", "bash", map[string]any{"command": "echo trailing"}, true)
				require.NoError(t, err)
				trailingBash, err = callHostTool(ctx, s.cfg, "tc-5", "sofa_checkpoint", map[string]any{
					"completed": []string{"x"}, "next_steps": []string{"y"},
				})
				require.NoError(t, err)
				return runtime.TurnSummary{FinalContent: "done"}, nil
			},
		)
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			sess.cfg = cfg
			return sess, nil
		}}

		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener:                opener,
			Prompt:                "work",
			Hooks:                 hooks,
			WritablePaths:         []string{"internal/**"},
			CheckpointEveryRounds: 5,
			MaxProtocolAttempts:   3,
		})
		require.NoError(t, err)
		assert.Equal(t, session.OutcomeSucceeded, res.Outcome.Status)
		assert.Equal(t, runtime.HostToolResultSuccess, firstComplete.ResultType)
		assert.Contains(t, firstComplete.TextResultForLLM, `"accepted":false`)
		assert.Contains(t, firstComplete.TextResultForLLM, "outside/secret.txt")
		assert.Contains(t, firstComplete.TextResultForLLM, "internal/**")
		assert.Equal(t, runtime.HostToolResultSuccess, secondComplete.ResultType)
		assert.Contains(t, secondComplete.TextResultForLLM, `"accepted":true`)
		assert.False(t, trailingPre.Allow)
		assert.Equal(t, runtime.HostToolResultFailure, trailingBash.ResultType)
	})

	t.Run("custom terminate tool replaces sofa_complete_work and validates workspace after Accepted=true", func(t *testing.T) {
		hooks := &mockHooks{
			unauthQueue: [][]string{
				{"bad/path.go"},
				nil,
			},
		}
		exec := toolruntime.NewExecutor(toolruntime.ExecutorOptions{
			Exec: func(_ context.Context, _ string, _ []string, _ map[string]string, _ []byte) (string, string, int, error) {
				return `{"accepted":true,"output":{"verdict":"ok"}}`, "", 0, nil
			},
		})
		binding := toolruntime.ToolBinding{
			Spec: plan.ToolSpec{
				ID:         "tool_finish_11111111-1111-1111-1111-111111111111",
				Kind:       "go",
				InputType:  []byte(`["object",{"note":"string"}]`),
				OutputType: []byte(`["object",{"verdict":"string"}]`),
			},
			ExposedName: "submit_result",
			Terminate:   true,
		}

		var firstOut, secondOut runtime.ToolOutput
		var mountedSpecs []runtime.HostToolSpec
		sess := newScriptedSession("s-term", runtime.SessionConfig{},
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				var err error
				firstOut, err = callHostTool(ctx, s.cfg, "tc-1", "submit_result", map[string]any{"note": "v1"})
				require.NoError(t, err)
				return runtime.TurnSummary{}, nil
			},
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				var err error
				secondOut, err = callHostTool(ctx, s.cfg, "tc-2", "submit_result", map[string]any{"note": "v2"})
				require.NoError(t, err)
				return runtime.TurnSummary{}, nil
			},
		)
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			mountedSpecs = cfg.HostTools
			sess.cfg = cfg
			return sess, nil
		}}

		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener:                opener,
			Prompt:                "work",
			Hooks:                 hooks,
			WritablePaths:         []string{"pkg/**"},
			ToolExecutor:          exec,
			ToolBindings:          []toolruntime.ToolBinding{binding},
			CheckpointEveryRounds: 5,
		})
		require.NoError(t, err)
		assert.False(t, hasHostTool(mountedSpecs, "sofa_complete_work"))
		assert.False(t, hasHostTool(mountedSpecs, "sofa_escalate"))
		assert.True(t, hasHostTool(mountedSpecs, "submit_result"))
		assert.Contains(t, firstOut.TextResultForLLM, "bad/path.go")
		assert.Contains(t, secondOut.TextResultForLLM, `"verdict":"ok"`)
		assert.Equal(t, session.OutcomeSucceeded, res.Outcome.Status)
		assert.JSONEq(t, `{"verdict":"ok"}`, string(res.Outcome.ResultJSON))
	})

	t.Run("concurrent in-flight tool rejects handoff and exhausting max_protocol_attempts errors", func(t *testing.T) {
		var concurrentOut runtime.ToolOutput
		sess := newScriptedSession("s-conc", runtime.SessionConfig{},
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				// Start a non-host bash tool without PostToolUse yet -> activeToolCalls == 1
				raw, _ := json.Marshal(map[string]any{"command": "sleep 1"})
				pre, err := s.cfg.PreToolUseHook(ctx, runtime.PreToolUseInput{
					HookCallID: "h1", ToolCallID: "tc-bg", ToolName: "bash", ArgumentsJSON: raw,
				})
				require.NoError(t, err)
				require.True(t, pre.Allow)
				// Now attempt handoff concurrently -> activeToolCalls == 2 (>1) -> rejected!
				concurrentOut, err = callHostTool(ctx, s.cfg, "tc-end", "sofa_complete_work", map[string]any{"summary": "early"})
				require.NoError(t, err)
				// Finish bg tool
				require.NoError(t, s.cfg.PostToolUseHook(ctx, runtime.PostToolUseInput{
					HookCallID: "h2", ToolCallID: "tc-bg", ToolName: "bash", Succeeded: true,
				}))
				return runtime.TurnSummary{FinalContent: "no handoff"}, nil
			},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				return runtime.TurnSummary{FinalContent: "still no handoff"}, nil
			},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				return runtime.TurnSummary{FinalContent: "still no handoff 2"}, nil
			},
		)
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			sess.cfg = cfg
			return sess, nil
		}}

		_, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener:                opener,
			Prompt:                "work",
			MaxProtocolAttempts:   2,
			CheckpointEveryRounds: 5,
		})
		require.ErrorIs(t, err, session.ErrProtocolAttemptsExhausted)
		assert.Contains(t, concurrentOut.TextResultForLLM, `"accepted":false`)
		assert.Contains(t, concurrentOut.TextResultForLLM, "in flight")
	})

	t.Run("sofa_escalate mounts only when agent_escalated fallback exists and validates name", func(t *testing.T) {
		var mountedSpecs []runtime.HostToolSpec
		var badEscOut, okEscOut runtime.ToolOutput
		sess := newScriptedSession("s-esc", runtime.SessionConfig{},
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				var err error
				badEscOut, err = callHostTool(ctx, s.cfg, "tc-1", "sofa_escalate", map[string]any{"fallback": "nonexistent", "summary": "blocked"})
				require.NoError(t, err)
				okEscOut, err = callHostTool(ctx, s.cfg, "tc-2", "sofa_escalate", map[string]any{"fallback": "replan", "summary": "missing requirement"})
				require.NoError(t, err)
				return runtime.TurnSummary{}, nil
			},
		)
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			mountedSpecs = cfg.HostTools
			sess.cfg = cfg
			return sess, nil
		}}

		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
			Opener: opener,
			Prompt: "work",
			Fallbacks: []workerspec.FallbackConfig{
				{Name: "replan", On: []string{"agent_escalated"}, Target: "worker.plan"},
			},
		})
		require.NoError(t, err)
		assert.True(t, hasHostTool(mountedSpecs, "sofa_escalate"))
		assert.Contains(t, badEscOut.TextResultForLLM, `"accepted":false`)
		assert.Contains(t, okEscOut.TextResultForLLM, `"accepted":true`)
		assert.Equal(t, session.OutcomeAgentEscalated, res.Outcome.Status)
		require.NotNil(t, res.Outcome.Escalation)
		assert.Equal(t, "replan", res.Outcome.Escalation.FallbackName)
		assert.Equal(t, "missing requirement", res.Outcome.Escalation.Summary)
	})
}

func TestGate4_QuotaTrackerAndAutomaticRollback(t *testing.T) {
	ctx := context.Background()

	t.Run("tool_call_quota bash=2 reserves, rolls back on PostToolUse failure, and rejects 4th", func(t *testing.T) {
		qt, err := session.NewQuotaTracker(map[string]int{"bash": 2, "zero_tool": 0})
		require.NoError(t, err)
		gates := session.NewGateController(session.GateControllerConfig{Quota: qt})

		// Zero limit rejects immediately
		zeroOut, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "z1", ToolCallID: "z1", ToolName: "zero_tool"})
		require.NoError(t, err)
		assert.False(t, zeroOut.Allow)

		// Unlisted tool is unlimited
		viewOut, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "v1", ToolCallID: "v1", ToolName: "view"})
		require.NoError(t, err)
		assert.True(t, viewOut.Allow)
		require.NoError(t, gates.PostToolUse(ctx, runtime.PostToolUseInput{HookCallID: "v2", ToolCallID: "v1", ToolName: "view", Succeeded: true}))
		_, limited := qt.Remaining("view")
		assert.False(t, limited)

		// 1st bash succeeds -> remaining 1
		out1, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "h1", ToolCallID: "b1", ToolName: "bash"})
		require.NoError(t, err)
		require.True(t, out1.Allow)
		rem, ok := qt.Remaining("bash")
		require.True(t, ok)
		assert.Equal(t, 1, rem)
		require.NoError(t, gates.PostToolUse(ctx, runtime.PostToolUseInput{HookCallID: "h1p", ToolCallID: "b1", ToolName: "bash", Succeeded: true}))
		rem, _ = qt.Remaining("bash")
		assert.Equal(t, 1, rem)

		// 2nd bash fails in PostToolUse -> rolls back to 1
		out2, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "h2", ToolCallID: "b2", ToolName: "bash"})
		require.NoError(t, err)
		require.True(t, out2.Allow)
		rem, _ = qt.Remaining("bash")
		assert.Equal(t, 0, rem)
		require.NoError(t, gates.PostToolUse(ctx, runtime.PostToolUseInput{HookCallID: "h2p", ToolCallID: "b2", ToolName: "bash", Succeeded: false, ErrorMessage: "exit 1"}))
		rem, _ = qt.Remaining("bash")
		assert.Equal(t, 1, rem)

		// 3rd bash succeeds -> remaining 0
		out3, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "h3", ToolCallID: "b3", ToolName: "bash"})
		require.NoError(t, err)
		require.True(t, out3.Allow)
		require.NoError(t, gates.PostToolUse(ctx, runtime.PostToolUseInput{HookCallID: "h3p", ToolCallID: "b3", ToolName: "bash", Succeeded: true}))
		rem, _ = qt.Remaining("bash")
		assert.Equal(t, 0, rem)

		// 4th bash rejected in PreToolUse!
		out4, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "h4", ToolCallID: "b4", ToolName: "bash"})
		require.NoError(t, err)
		assert.False(t, out4.Allow)
		assert.Contains(t, out4.DenyReason, "quota")

		// Spurious PostToolUse failure for denied call must NOT rollback
		require.NoError(t, gates.PostToolUse(ctx, runtime.PostToolUseInput{HookCallID: "h4p", ToolCallID: "b4", ToolName: "bash", Succeeded: false}))
		rem, _ = qt.Remaining("bash")
		assert.Equal(t, 0, rem)
	})

	t.Run("host tool HandlerError rolls back quota while Accepted=false consumes quota", func(t *testing.T) {
		qt, err := session.NewQuotaTracker(map[string]int{"custom_go": 2})
		require.NoError(t, err)
		gates := session.NewGateController(session.GateControllerConfig{Quota: qt})

		callNo := 0
		wrapped := gates.WrapHostTool("custom_go", session.HostToolBusiness, "custom_go", func(_ context.Context, _ runtime.ToolInvocation) (runtime.ToolOutput, error) {
			callNo++
			if callNo == 1 {
				// Model-repairable business rejection (HandlerError == "")
				return runtime.ToolOutput{
					ResultType:       runtime.HostToolResultSuccess,
					TextResultForLLM: `{"accepted":false,"issues":[{"code":"bad_arg","message":"fix it"}]}`,
				}, nil
			}
			// Infrastructure crash (HandlerError != "")
			return runtime.ToolOutput{
				ResultType:       runtime.HostToolResultFailure,
				TextResultForLLM: "container crash",
				HandlerError:     "exit code 137",
			}, nil
		})

		// Call 1: Accepted=false -> consumes quota (1 left)
		out1, err := wrapped(ctx, runtime.ToolInvocation{ToolCallID: "c1", ToolName: "custom_go"})
		require.NoError(t, err)
		assert.Equal(t, runtime.HostToolResultSuccess, out1.ResultType)
		rem, _ := qt.Remaining("custom_go")
		assert.Equal(t, 1, rem)

		// Call 2: HandlerError != "" -> rolls back quota (still 1 left)
		out2, err := wrapped(ctx, runtime.ToolInvocation{ToolCallID: "c2", ToolName: "custom_go"})
		require.NoError(t, err)
		assert.NotEmpty(t, out2.HandlerError)
		rem, _ = qt.Remaining("custom_go")
		assert.Equal(t, 1, rem)
	})
}

func TestQCLedger_CrossRoundAntiAmnesiaAndSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	criteria := map[string]string{
		"tests":    "All unit tests pass",
		"security": "No hardcoded secrets",
	}
	ledger, err := session.NewQCLedger(criteria)
	require.NoError(t, err)

	// Empty ledger calling revise must be hard rejected
	_, err = ledger.SubmitVerdict("revise", "nothing wrong")
	require.Error(t, err)

	// Unknown criterion must be rejected
	_, err = ledger.OpenIssues([]session.QCIssueInput{{
		Criterion: "nonexistent",
		Code:      "bad",
		Message:   "bad",
	}})
	require.Error(t, err)

	// Round 1 QC session: open QC-001 & QC-002 and submit revise
	var r1PassDenied runtime.ToolOutput
	var r1OpenWithIDRejected runtime.ToolOutput
	sess1 := newScriptedSession("qc-1", runtime.SessionConfig{}, func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
		var err error
		// Schema/decoder forbids passing id
		r1OpenWithIDRejected, err = callHostTool(ctx, s.cfg, "q1", "sofa_qc_open_issues", map[string]any{
			"issues": []map[string]any{
				{"id": "QC-999", "criterion": "tests", "code": "c1", "message": "m1"},
			},
		})
		require.NoError(t, err)

		openOut, err := callHostTool(ctx, s.cfg, "q2", "sofa_qc_open_issues", map[string]any{
			"issues": []map[string]any{
				{"criterion": "tests", "code": "missing_test", "message": "Add unit test for edge case"},
				{"criterion": "security", "code": "secret_leak", "message": "Remove API key literal"},
			},
		})
		require.NoError(t, err)
		assert.Contains(t, openOut.TextResultForLLM, "QC-001")
		assert.Contains(t, openOut.TextResultForLLM, "QC-002")

		// Trying pass while QC-001 & QC-002 are open -> hard rejected
		r1PassDenied, err = callHostTool(ctx, s.cfg, "q3", "sofa_qc_submit_verdict", map[string]any{
			"decision": "pass",
			"summary":  "looks fine",
		})
		require.NoError(t, err)

		revOut, err := callHostTool(ctx, s.cfg, "q4", "sofa_qc_submit_verdict", map[string]any{
			"decision": "revise",
			"summary":  "2 issues found",
		})
		require.NoError(t, err)
		assert.Contains(t, revOut.TextResultForLLM, `"accepted":true`)
		return runtime.TurnSummary{}, nil
	})
	opener1 := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
		sess1.cfg = cfg
		return sess1, nil
	}}

	qcRes1, err := session.RunQCPhase(ctx, session.QCSessionConfig{
		Opener: opener1,
		Prompt: "audit round 1",
		Ledger: ledger,
	})
	require.NoError(t, err)
	assert.Contains(t, r1OpenWithIDRejected.TextResultForLLM, `"accepted":false`)
	assert.Contains(t, r1PassDenied.TextResultForLLM, `"accepted":false`)
	assert.Contains(t, r1PassDenied.TextResultForLLM, "QC-001")
	assert.Contains(t, r1PassDenied.TextResultForLLM, "QC-002")
	assert.Equal(t, "revise", qcRes1.Verdict.Decision)
	assert.Len(t, qcRes1.OpenIssues, 2)

	// Snapshot & Restore into a fresh QCLedger (simulating checkpoint recovery / cross-round persistence)
	snap, err := ledger.Snapshot()
	require.NoError(t, err)
	restoredLedger := &session.QCLedger{}
	require.NoError(t, restoredLedger.Restore(snap))

	// Round 2 QC session: resolve only QC-001 -> pass rejected listing QC-002; resolve QC-002 -> pass succeeds!
	var r2PartialPassDenied, r2FinalPass runtime.ToolOutput
	sess2 := newScriptedSession("qc-2", runtime.SessionConfig{}, func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
		res1, err := callHostTool(ctx, s.cfg, "q5", "sofa_qc_resolve_issues", map[string]any{
			"ids":                 []string{"QC-001"},
			"resolution_evidence": "Verified unit test added and passes",
		})
		require.NoError(t, err)
		assert.Contains(t, res1.TextResultForLLM, `"accepted":true`)

		// Attempt pass with QC-002 still open
		r2PartialPassDenied, err = callHostTool(ctx, s.cfg, "q6", "sofa_qc_submit_verdict", map[string]any{
			"decision": "pass",
			"summary":  "almost done",
		})
		require.NoError(t, err)

		// Resolve QC-002
		res2, err := callHostTool(ctx, s.cfg, "q7", "sofa_qc_resolve_issues", map[string]any{
			"ids":                 []string{"QC-002"},
			"resolution_evidence": "Verified literal secret removed",
		})
		require.NoError(t, err)
		assert.Contains(t, res2.TextResultForLLM, `"accepted":true`)

		// Now pass succeeds
		r2FinalPass, err = callHostTool(ctx, s.cfg, "q8", "sofa_qc_submit_verdict", map[string]any{
			"decision": "pass",
			"summary":  "all issues verified and resolved",
		})
		require.NoError(t, err)
		return runtime.TurnSummary{}, nil
	})
	opener2 := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
		sess2.cfg = cfg
		return sess2, nil
	}}

	qcRes2, err := session.RunQCPhase(ctx, session.QCSessionConfig{
		Opener: opener2,
		Prompt: "audit round 2",
		Ledger: restoredLedger,
	})
	require.NoError(t, err)
	assert.Contains(t, r2PartialPassDenied.TextResultForLLM, `"accepted":false`)
	assert.Contains(t, r2PartialPassDenied.TextResultForLLM, "QC-002")
	assert.NotContains(t, r2PartialPassDenied.TextResultForLLM, "QC-001")
	assert.Contains(t, r2FinalPass.TextResultForLLM, `"accepted":true`)
	assert.Equal(t, "pass", qcRes2.Verdict.Decision)
	assert.Empty(t, qcRes2.OpenIssues)

	// Opening another issue on restored ledger continues monotonic ID sequence at QC-003
	nextOpened, err := restoredLedger.OpenIssues([]session.QCIssueInput{{
		Criterion: "tests",
		Code:      "follow_up",
		Message:   "extra check",
	}})
	require.NoError(t, err)
	require.Len(t, nextOpened, 1)
	assert.Equal(t, "QC-003", nextOpened[0].ID)
}

func TestRetryExecutor_And_StallWatchdog(t *testing.T) {
	ctx := context.Background()

	t.Run("RATE_LIMIT retries with backoff and succeeds; AUTHENTICATION fails with 0 retries", func(t *testing.T) {
		var sleeps []time.Duration
		policy := provider.RetryPolicy{
			LifecycleRetries: 2,
			ModelCallRetries: 3,
			Interval:         10 * time.Second,
			MaxInterval:      25 * time.Second,
		}
		exec, err := session.NewRetryExecutor(policy)
		require.NoError(t, err)
		exec.Sleep = func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		}
		exec.Jitter = func(_ time.Duration) time.Duration { return 0 }

		attempts := 0
		sess := newScriptedSession("s-rl", runtime.SessionConfig{},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				attempts++
				return runtime.TurnSummary{}, &runtime.HarnessError{Category: runtime.ErrorCategoryRateLimit, HTTPStatusCode: 429, Message: "slow down"}
			},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				attempts++
				return runtime.TurnSummary{}, &runtime.HarnessError{Category: runtime.ErrorCategoryRateLimit, HTTPStatusCode: 429, Message: "still busy"}
			},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				attempts++
				return runtime.TurnSummary{FinalContent: "recovered"}, nil
			},
		)

		summary, err := exec.RunTurnWithRetryAndWatchdog(ctx, sess, "go", session.WatchdogTurnOptions{})
		require.NoError(t, err)
		assert.Equal(t, "recovered", summary.FinalContent)
		assert.Equal(t, 3, attempts)
		assert.Equal(t, []time.Duration{10 * time.Second, 20 * time.Second}, sleeps)

		// Permanent AUTHENTICATION error fails immediately with 0 retries
		sleeps = nil
		authAttempts := 0
		authSess := newScriptedSession("s-auth", runtime.SessionConfig{},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				authAttempts++
				return runtime.TurnSummary{}, &runtime.HarnessError{Category: runtime.ErrorCategoryAuthentication, HTTPStatusCode: 401, Message: "bad key"}
			},
		)
		_, err = exec.RunTurnWithRetryAndWatchdog(ctx, authSess, "go", session.WatchdogTurnOptions{})
		require.Error(t, err)
		assert.Equal(t, 1, authAttempts)
		assert.Empty(t, sleeps)
	})

	t.Run("SESSION_NOT_FOUND falls back to fresh session with historical diary replay", func(t *testing.T) {
		exec, err := session.NewRetryExecutor(provider.DefaultRetryPolicy())
		require.NoError(t, err)
		exec.Sleep = func(_ context.Context, _ time.Duration) error { return nil }
		exec.Jitter = func(_ time.Duration) time.Duration { return 0 }

		opener := &scriptedOpener{
			openFn: func(_ context.Context, cfg runtime.SessionConfig, attempt int) (runtime.Session, error) {
				if attempt == 0 {
					return nil, &runtime.HarnessError{Category: runtime.ErrorCategorySessionNotFound, Message: "gone"}
				}
				return newScriptedSession("fresh-1", cfg), nil
			},
		}

		diarySummary := session.FormatDiarySummary([]session.DiaryInput{{
			Completed: []string{"wrote parser"},
			NextSteps: []string{"add tests"},
		}})
		sess, effectiveCfg, err := exec.OpenWithRetry(ctx, opener, runtime.SessionConfig{
			SessionID:       "new-id",
			ResumeSessionID: "old-missing-session",
			SystemPrompt:    "base system prompt",
		}, diarySummary)
		require.NoError(t, err)
		assert.Equal(t, "fresh-1", sess.ID())
		assert.Empty(t, effectiveCfg.ResumeSessionID)
		assert.Contains(t, effectiveCfg.SystemPrompt, "base system prompt")
		assert.Contains(t, effectiveCfg.SystemPrompt, "wrote parser")
		assert.Len(t, opener.opened, 2)
		assert.Equal(t, "old-missing-session", opener.opened[0].ResumeSessionID)
		assert.Empty(t, opener.opened[1].ResumeSessionID)
	})

	t.Run("StallWatchdog aborts stalled turn, waits for active tools to drain, resumes, and continues", func(t *testing.T) {
		exec, err := session.NewRetryExecutor(provider.RetryPolicy{
			LifecycleRetries: 2,
			ModelCallRetries: 1,
			Interval:         time.Millisecond,
			MaxInterval:      5 * time.Millisecond,
		})
		require.NoError(t, err)

		gates := session.NewGateController(session.GateControllerConfig{})
		toolFinished := make(chan struct{})
		resumeOrderOK := atomic.Bool{}

		sess := newScriptedSession("s-stall", runtime.SessionConfig{},
			// Turn 1: hangs until Abort fires, then starts a brief unwinding tool so WaitForZeroActiveTools is exercised!
			func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				handler := gates.WrapHostTool("slow_probe", session.HostToolReadOnly, "", func(_ context.Context, _ runtime.ToolInvocation) (runtime.ToolOutput, error) {
					time.Sleep(25 * time.Millisecond)
					close(toolFinished)
					return runtime.ToolOutput{ResultType: runtime.HostToolResultSuccess, TextResultForLLM: "ok"}, nil
				})

				<-s.abortedCh
				go func() {
					_, _ = handler(context.Background(), runtime.ToolInvocation{ToolCallID: "t-slow", ToolName: "slow_probe"})
				}()
				for gates.ActiveToolCalls() == 0 {
					time.Sleep(time.Millisecond)
				}
				return runtime.TurnSummary{}, errors.New("aborted due to stall")
			},
			// Turn 2: continuation prompt succeeds cleanly
			func(_ context.Context, _ *scriptedSession, prompt string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				select {
				case <-toolFinished:
					resumeOrderOK.Store(true)
				default:
				}
				assert.Contains(t, prompt, "stalled")
				return runtime.TurnSummary{FinalContent: "continued and completed"}, nil
			},
		)

		summary, err := exec.RunTurnWithRetryAndWatchdog(ctx, sess, "start", session.WatchdogTurnOptions{
			Gates:         gates,
			StallTimeout:  40 * time.Millisecond,
			CheckInterval: 5 * time.Millisecond,
		})
		require.NoError(t, err)
		assert.Equal(t, "continued and completed", summary.FinalContent)
		assert.Equal(t, int32(1), sess.abortCount.Load())
		assert.Equal(t, int32(1), sess.resumeCnt.Load())
		assert.True(t, resumeOrderOK.Load(), "Resume must not run before active tool calls reach zero")
	})

	t.Run("ErrorMessageRegex retries ERROR_CATEGORY_INTERNAL and StallWatchdog exhausts LifecycleRetries", func(t *testing.T) {
		exec, err := session.NewRetryExecutor(provider.RetryPolicy{
			LifecycleRetries:  1,
			ModelCallRetries:  2,
			Interval:          time.Millisecond,
			MaxInterval:       time.Millisecond,
			ErrorMessageRegex: []string{`temporary upstream reset`},
		})
		require.NoError(t, err)
		exec.Sleep = func(_ context.Context, _ time.Duration) error { return nil }
		exec.Jitter = func(_ time.Duration) time.Duration { return 0 }

		attempts := 0
		regexSess := newScriptedSession("s-regex", runtime.SessionConfig{},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				attempts++
				return runtime.TurnSummary{}, &runtime.HarnessError{Category: runtime.ErrorCategoryInternal, Message: "temporary upstream reset #1"}
			},
			func(_ context.Context, _ *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				attempts++
				return runtime.TurnSummary{FinalContent: "regex ok"}, nil
			},
		)
		sum, err := exec.RunTurnWithRetryAndWatchdog(ctx, regexSess, "hi", session.WatchdogTurnOptions{})
		require.NoError(t, err)
		assert.Equal(t, "regex ok", sum.FinalContent)
		assert.Equal(t, 2, attempts)

		// Exhaust stall retries (LifecycleRetries = 1 -> 1 recovery allowed, 2nd stall fails)
		alwaysStall := newScriptedSession("s-stall-exhaust", runtime.SessionConfig{},
			func(_ context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				<-s.abortedCh
				return runtime.TurnSummary{}, errors.New("aborted 1")
			},
			func(_ context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
				<-s.abortedCh
				return runtime.TurnSummary{}, errors.New("aborted 2")
			},
		)
		_, err = exec.RunTurnWithRetryAndWatchdog(ctx, alwaysStall, "start", session.WatchdogTurnOptions{
			StallTimeout:  15 * time.Millisecond,
			CheckInterval: 3 * time.Millisecond,
		})
		require.ErrorIs(t, err, session.ErrStallRecoveryExhausted)
		assert.Equal(t, int32(2), alwaysStall.abortCount.Load())
		assert.Equal(t, int32(1), alwaysStall.resumeCnt.Load())
	})
}

func TestAdditionalEdgeCases_RequestDecision_RunawayCheckpoint_And_QCErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("sofa_request_decision returns WaitingHuman with DecisionRequest", func(t *testing.T) {
		sess := newScriptedSession("s-dec", runtime.SessionConfig{}, func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			out, err := callHostTool(ctx, s.cfg, "d1", "sofa_request_decision", map[string]any{
				"question":    "Which auth provider?",
				"context":     "Both OAuth and SAML are supported",
				"options":     []string{"OAuth", "SAML"},
				"recommended": "OAuth",
			})
			require.NoError(t, err)
			require.Contains(t, out.TextResultForLLM, `"accepted":true`)
			return runtime.TurnSummary{}, nil
		})
		opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
			sess.cfg = cfg
			return sess, nil
		}}
		res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{Opener: opener, Prompt: "auth"})
		require.NoError(t, err)
		assert.Equal(t, session.OutcomeWaitingHuman, res.Outcome.Status)
		require.NotNil(t, res.Outcome.DecisionRequest)
		assert.Equal(t, "Which auth provider?", res.Outcome.DecisionRequest.Question)
		assert.Equal(t, "OAuth", res.Outcome.DecisionRequest.Recommended)
	})

	t.Run("single-turn runaway threshold checkpointEveryRounds*4 triggers checkpointPending mid-turn", func(t *testing.T) {
		gates := session.NewGateController(session.GateControllerConfig{CheckpointEveryRounds: 1})
		for i := 1; i <= 5; i++ {
			id := fmt.Sprintf("c%d", i)
			out, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: id, ToolCallID: id, ToolName: "bash"})
			require.NoError(t, err)
			require.True(t, out.Allow)
			require.NoError(t, gates.PostToolUse(ctx, runtime.PostToolUseInput{HookCallID: id, ToolCallID: id, ToolName: "bash", Succeeded: true}))
		}
		assert.True(t, gates.CheckpointPending())
		blocked, err := gates.PreToolUse(ctx, runtime.PreToolUseInput{HookCallID: "c6", ToolCallID: "c6", ToolName: "bash"})
		require.NoError(t, err)
		assert.False(t, blocked.Allow)
		assert.Contains(t, blocked.DenyReason, "sofa_checkpoint")
	})

	t.Run("QCLedger rejects duplicate, unknown, already-resolved IDs and blank evidence", func(t *testing.T) {
		ledger, err := session.NewQCLedger(map[string]string{"tests": "unit tests"})
		require.NoError(t, err)
		opened, err := ledger.OpenIssues([]session.QCIssueInput{{Criterion: "tests", Code: "c1", Message: "m1"}})
		require.NoError(t, err)
		require.Equal(t, "QC-001", opened[0].ID)

		_, err = ledger.ResolveIssues([]string{"QC-001"}, "   ")
		require.Error(t, err)
		_, err = ledger.ResolveIssues([]string{"QC-001", "QC-001"}, "ev")
		require.Error(t, err)
		_, err = ledger.ResolveIssues([]string{"QC-999"}, "ev")
		require.Error(t, err)
		_, err = ledger.ResolveIssues([]string{"QC-001"}, "fixed")
		require.NoError(t, err)
		_, err = ledger.ResolveIssues([]string{"QC-001"}, "fixed again")
		require.Error(t, err)
	})
}

func TestWatchdog_StallMidBuiltInTool_CleansUpNonHostAndRollsBackQuota(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var turn2BashOut runtime.PreToolUseOutput
	var turn2CompleteOut runtime.ToolOutput

	sess := newScriptedSession("s-mid-tool-stall", runtime.SessionConfig{},
		// Turn 1: built-in bash passes PreToolUse (consuming quota=1), then plugin stalls without PostToolUse!
		func(tctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			raw, _ := json.Marshal(map[string]any{"command": "hang-forever"})
			out, err := s.cfg.PreToolUseHook(tctx, runtime.PreToolUseInput{
				HookCallID: "h-stall", ToolCallID: "tc-stall", ToolName: "bash", ArgumentsJSON: raw,
			})
			require.NoError(t, err)
			require.True(t, out.Allow)

			select {
			case <-s.abortedCh:
				return runtime.TurnSummary{}, errors.New("aborted mid built-in tool")
			case <-tctx.Done():
				return runtime.TurnSummary{}, tctx.Err()
			}
		},
		// Turn 2: continuation after Abort -> Resume; quota must be rolled back to 1 and handoff barrier clear!
		func(tctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			var err error
			turn2BashOut, err = callBuiltInTool(tctx, s.cfg, "tc-retry", "bash", map[string]any{"command": "echo ok"}, true)
			require.NoError(t, err)
			turn2CompleteOut, err = callHostTool(tctx, s.cfg, "tc-done", "sofa_complete_work", map[string]any{"summary": "recovered"})
			require.NoError(t, err)
			return runtime.TurnSummary{FinalContent: "done"}, nil
		},
	)
	opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
		sess.cfg = cfg
		return sess, nil
	}}

	res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
		Opener:                opener,
		Prompt:                "run bash",
		ToolCallQuota:         map[string]int{"bash": 1},
		CheckpointEveryRounds: 5,
		RetryPolicy: provider.RetryPolicy{
			LifecycleRetries: 1,
			ModelCallRetries: 1,
			Interval:         time.Millisecond,
			MaxInterval:      5 * time.Millisecond,
		},
		StallTimeout:          35 * time.Millisecond,
		WatchdogCheckInterval: 5 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Equal(t, session.OutcomeSucceeded, res.Outcome.Status)
	assert.Equal(t, int32(1), sess.abortCount.Load())
	assert.Equal(t, int32(1), sess.resumeCnt.Load())
	assert.True(t, turn2BashOut.Allow, "aborted Turn 1 bash quota should have been rolled back")
	assert.Contains(t, turn2CompleteOut.TextResultForLLM, `"accepted":true`)
}

func TestPreTurnInterjection_CountsTowardCheckpointEveryRounds(t *testing.T) {
	ctx := context.Background()
	hooks := &mockHooks{
		interjections: [][]session.HumanInterjection{
			{{ID: "ij-pre", Message: "use strict error checks", WaitForConfirmation: false}},
		},
	}

	var turn3BashBlocked runtime.PreToolUseOutput
	sess := newScriptedSession("s-pre-ckpt", runtime.SessionConfig{},
		// Turn 1: pre-turn interjection turn executes 1 business tool -> roundsSinceCheckpoint = 1
		func(ctx context.Context, s *scriptedSession, prompt string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			assert.Contains(t, prompt, "use strict error checks")
			out, err := callBuiltInTool(ctx, s.cfg, "tc-1", "bash", map[string]any{"command": "echo r1"}, true)
			require.NoError(t, err)
			require.True(t, out.Allow)
			return runtime.TurnSummary{FinalContent: "acknowledged"}, nil
		},
		// Turn 2: scheduled prompt executes 1 business tool -> roundsSinceCheckpoint = 2 -> checkpointPending = true!
		func(ctx context.Context, s *scriptedSession, prompt string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			assert.Equal(t, "SCHEDULED_TASK", prompt)
			out, err := callBuiltInTool(ctx, s.cfg, "tc-2", "bash", map[string]any{"command": "echo r2"}, true)
			require.NoError(t, err)
			require.True(t, out.Allow)
			return runtime.TurnSummary{FinalContent: "r2 done"}, nil
		},
		// Turn 3: bash must be blocked by checkpointPending until sofa_checkpoint succeeds
		func(ctx context.Context, s *scriptedSession, _ string, _ func(runtime.Event)) (runtime.TurnSummary, error) {
			var err error
			turn3BashBlocked, err = callBuiltInTool(ctx, s.cfg, "tc-3", "bash", map[string]any{"command": "echo blocked"}, true)
			require.NoError(t, err)

			ckptOut, err := callHostTool(ctx, s.cfg, "tc-4", "sofa_checkpoint", map[string]any{
				"completed":  []string{"r1 and r2"},
				"next_steps": []string{"complete"},
			})
			require.NoError(t, err)
			require.Contains(t, ckptOut.TextResultForLLM, `"accepted":true`)

			doneOut, err := callHostTool(ctx, s.cfg, "tc-5", "sofa_complete_work", map[string]any{"summary": "all done"})
			require.NoError(t, err)
			require.Contains(t, doneOut.TextResultForLLM, `"accepted":true`)
			return runtime.TurnSummary{FinalContent: "done"}, nil
		},
	)
	opener := &scriptedOpener{openFn: func(_ context.Context, cfg runtime.SessionConfig, _ int) (runtime.Session, error) {
		sess.cfg = cfg
		return sess, nil
	}}

	res, err := session.RunWorkPhase(ctx, session.WorkSessionConfig{
		Opener:                opener,
		Prompt:                "SCHEDULED_TASK",
		Hooks:                 hooks,
		CheckpointEveryRounds: 2,
		MaxProtocolAttempts:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, session.OutcomeSucceeded, res.Outcome.Status)
	assert.False(t, turn3BashBlocked.Allow, "turn 3 bash must be blocked because pre-turn interjection turn counted as round 1")
	assert.Contains(t, turn3BashBlocked.DenyReason, "sofa_checkpoint")
	assert.Len(t, res.Diaries, 1)
}
