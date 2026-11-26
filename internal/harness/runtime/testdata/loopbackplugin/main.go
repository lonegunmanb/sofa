package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/lonegunmanb/sofa/pkg/harnessplugin"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
)

type activeTurn struct {
	turnID   string
	cancelCh chan struct{}
	once     sync.Once
}

func (a *activeTurn) cancel() {
	a.once.Do(func() {
		close(a.cancelCh)
	})
}

type loopbackServer struct {
	harnessv1.UnimplementedAgentHarnessServer

	mu       sync.Mutex
	sessions map[string]*harnessv1.OpenSessionRequest
	turns    map[string]*activeTurn
}

func newLoopbackServer() *loopbackServer {
	return &loopbackServer{
		sessions: make(map[string]*harnessv1.OpenSessionRequest),
		turns:    make(map[string]*activeTurn),
	}
}

func (s *loopbackServer) GetCapabilities(_ context.Context, _ *harnessv1.GetCapabilitiesRequest) (*harnessv1.GetCapabilitiesResponse, error) {
	return &harnessv1.GetCapabilitiesResponse{
		HarnessName:            "loopback",
		HarnessVersion:         "0.1.0",
		ProtocolVersion:        1,
		SupportedProviderTypes: []string{"openai", "azure", "anthropic"},
		SupportsSessionResume:  true,
		SupportsMcpServers:     true,
		SupportsMcpResources:   true,
		SupportsSkills:         true,
	}, nil
}

func (s *loopbackServer) OpenSession(_ context.Context, req *harnessv1.OpenSessionRequest) (*harnessv1.OpenSessionResponse, error) {
	if req.ResumeSessionId == "missing-session" {
		return &harnessv1.OpenSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
				HttpStatusCode: 404,
				Message:        "session not found",
			},
		}, nil
	}
	if req.Provider != nil && req.Provider.Type == "unsupported" {
		return &harnessv1.OpenSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY,
				HttpStatusCode: 400,
				Message:        "unsupported provider",
			},
		}, nil
	}

	sessionID := req.SessionId
	if sessionID == "" {
		sessionID = "sess-loopback-1"
	}
	s.mu.Lock()
	s.sessions[sessionID] = req
	s.mu.Unlock()

	return &harnessv1.OpenSessionResponse{
		SessionId: sessionID,
		Resumed:   req.ResumeSessionId != "",
	}, nil
}

func (s *loopbackServer) RunTurn(stream harnessv1.AgentHarness_RunTurnServer) error {
	firstFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	startReq := firstFrame.GetStartTurn()
	if startReq == nil {
		return fmt.Errorf("expected StartTurnRequest as first frame")
	}

	turn := &activeTurn{
		turnID:   startReq.TurnId,
		cancelCh: make(chan struct{}),
	}
	s.mu.Lock()
	s.turns[startReq.SessionId] = turn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if cur, ok := s.turns[startReq.SessionId]; ok && cur == turn {
			delete(s.turns, startReq.SessionId)
		}
		s.mu.Unlock()
	}()

	switch startReq.Prompt {
	case "prompt:hooks-and-parallel":
		return s.runHooksAndParallel(stream, startReq)
	case "prompt:block-until-abort":
		return s.runBlockUntilAbort(stream, startReq, turn)
	case "prompt:rate-limit":
		return stream.Send(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_TurnResult{
				TurnResult: &harnessv1.TurnResult{
					TurnId: startReq.TurnId,
					Error: &harnessv1.ErrorDetail{
						Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT,
						HttpStatusCode: 429,
						Message:        "rate limit exceeded",
					},
				},
			},
		})
	default:
		return s.runSimple(stream, startReq)
	}
}

func (s *loopbackServer) runSimple(stream harnessv1.AgentHarness_RunTurnServer, startReq *harnessv1.StartTurnRequest) error {
	for _, ev := range []*harnessv1.SessionEvent{
		{
			EventId: "ev-1",
			Kind:    harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA,
			Text:    "thinking...",
		},
		{
			EventId: "ev-2",
			Kind:    harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA,
			Text:    "hello ",
		},
		{
			EventId: "ev-3",
			Kind:    harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_USAGE,
			Usage: &harnessv1.TokenUsage{
				Model:        "gpt-5",
				InputTokens:  10,
				OutputTokens: 5,
			},
		},
	} {
		if err := stream.Send(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_Event{Event: ev},
		}); err != nil {
			return err
		}
	}

	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_InvokeHostTool{
			InvokeHostTool: &harnessv1.InvokeHostToolRequest{
				ToolCallId:    "call-echo-1",
				ToolName:      "echo_host",
				ArgumentsJson: []byte(`{"prompt":"` + startReq.Prompt + `"}`),
			},
		},
	}); err != nil {
		return err
	}

	respFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	toolResp := respFrame.GetInvokeHostToolResponse()
	if toolResp == nil {
		return fmt.Errorf("expected InvokeHostToolResponse")
	}

	return stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_TurnResult{
			TurnResult: &harnessv1.TurnResult{
				TurnId:             startReq.TurnId,
				AssistantMessageId: "msg-simple",
				FinalContent:       "done:" + toolResp.TextResultForLlm,
			},
		},
	})
}

func (s *loopbackServer) runHooksAndParallel(stream harnessv1.AgentHarness_RunTurnServer, startReq *harnessv1.StartTurnRequest) error {
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_Event{
			Event: &harnessv1.SessionEvent{
				EventId: "ev-hp-1",
				Kind:    harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA,
				Text:    "running hooks",
			},
		},
	}); err != nil {
		return err
	}

	// 1. PreToolUse allow -> PostToolUse failure
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_PreToolUse{
			PreToolUse: &harnessv1.PreToolUseRequest{
				HookCallId:    "hook-allow-1",
				ToolCallId:    "tc-bash-1",
				ToolName:      "bash",
				ArgumentsJson: []byte(`{"cmd":"ls"}`),
			},
		},
	}); err != nil {
		return err
	}
	allowFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	allowDec := allowFrame.GetPreToolUseDecision()
	if allowDec == nil || !allowDec.Allow {
		return fmt.Errorf("expected allow decision for bash")
	}

	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_PostToolUse{
			PostToolUse: &harnessv1.PostToolUseRequest{
				HookCallId:   "hook-post-1",
				ToolCallId:   "tc-bash-1",
				ToolName:     "bash",
				Succeeded:    false,
				ErrorMessage: "command exited 1",
			},
		},
	}); err != nil {
		return err
	}
	postFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	if postFrame.GetPostToolUseAck() == nil {
		return fmt.Errorf("expected PostToolUseAck")
	}

	// 2. PreToolUse deny
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_PreToolUse{
			PreToolUse: &harnessv1.PreToolUseRequest{
				HookCallId:    "hook-deny-1",
				ToolCallId:    "tc-edit-1",
				ToolName:      "edit",
				ArgumentsJson: []byte(`{"path":"forbidden.txt"}`),
			},
		},
	}); err != nil {
		return err
	}
	denyFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	denyDec := denyFrame.GetPreToolUseDecision()
	if denyDec == nil || denyDec.Allow {
		return fmt.Errorf("expected deny decision for edit")
	}

	// 3. Parallel InvokeHostTool (send two before receiving either)
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_InvokeHostTool{
			InvokeHostTool: &harnessv1.InvokeHostToolRequest{
				ToolCallId:    "call-par-a",
				ToolName:      "tool_a",
				ArgumentsJson: []byte(`{"side":"a"}`),
			},
		},
	}); err != nil {
		return err
	}
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_InvokeHostTool{
			InvokeHostTool: &harnessv1.InvokeHostToolRequest{
				ToolCallId:    "call-par-b",
				ToolName:      "tool_b",
				ArgumentsJson: []byte(`{"side":"b"}`),
			},
		},
	}); err != nil {
		return err
	}

	toolResults := make(map[string]string, 2)
	for len(toolResults) < 2 {
		f, err := stream.Recv()
		if err != nil {
			return err
		}
		resp := f.GetInvokeHostToolResponse()
		if resp == nil {
			return fmt.Errorf("expected InvokeHostToolResponse")
		}
		toolResults[resp.ToolCallId] = resp.TextResultForLlm
	}

	summary := map[string]any{
		"modified_args":      string(allowDec.ModifiedArgumentsJson),
		"additional_context": allowDec.AdditionalContext,
		"deny_reason":        denyDec.DenyReason,
		"tool_a":             toolResults["call-par-a"],
		"tool_b":             toolResults["call-par-b"],
	}
	summaryBytes, _ := json.Marshal(summary)
	return stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_TurnResult{
			TurnResult: &harnessv1.TurnResult{
				TurnId:             startReq.TurnId,
				AssistantMessageId: "msg-hp",
				FinalContent:       string(summaryBytes),
			},
		},
	})
}

func (s *loopbackServer) runBlockUntilAbort(stream harnessv1.AgentHarness_RunTurnServer, startReq *harnessv1.StartTurnRequest, turn *activeTurn) error {
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_Event{
			Event: &harnessv1.SessionEvent{
				EventId: "ev-block-1",
				Kind:    harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_WARNING,
				Text:    "blocking",
			},
		},
	}); err != nil {
		return err
	}

	select {
	case <-turn.cancelCh:
		return stream.Send(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_TurnResult{
				TurnResult: &harnessv1.TurnResult{
					TurnId: startReq.TurnId,
					Error: &harnessv1.ErrorDetail{
						Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT,
						HttpStatusCode: 499,
						Message:        "turn aborted",
					},
				},
			},
		})
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

func (s *loopbackServer) AbortTurn(_ context.Context, req *harnessv1.AbortTurnRequest) (*harnessv1.AbortTurnResponse, error) {
	s.mu.Lock()
	turn := s.turns[req.SessionId]
	s.mu.Unlock()
	if turn != nil {
		turn.cancel()
	}
	return &harnessv1.AbortTurnResponse{}, nil
}

func (s *loopbackServer) ResumeSession(_ context.Context, req *harnessv1.ResumeSessionRequest) (*harnessv1.ResumeSessionResponse, error) {
	s.mu.Lock()
	_, ok := s.sessions[req.SessionId]
	s.mu.Unlock()
	if !ok {
		return &harnessv1.ResumeSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
				HttpStatusCode: 404,
				Message:        "session not found",
			},
		}, nil
	}
	return &harnessv1.ResumeSessionResponse{}, nil
}

func (s *loopbackServer) ReadMCPResource(_ context.Context, req *harnessv1.ReadMCPResourceRequest) (*harnessv1.ReadMCPResourceResponse, error) {
	if req.ServerName == "loopback" && req.Uri == "loopback://env" {
		raw, _ := json.Marshal(os.Environ())
		return &harnessv1.ReadMCPResourceResponse{
			Contents: []*harnessv1.MCPResourceContent{{
				Uri:      req.Uri,
				MimeType: "application/json",
				Text:     string(raw),
			}},
		}, nil
	}
	if req.ServerName == "loopback" && req.Uri == "loopback://session" {
		s.mu.Lock()
		sess := s.sessions[req.SessionId]
		s.mu.Unlock()
		if sess == nil {
			return &harnessv1.ReadMCPResourceResponse{
				Error: &harnessv1.ErrorDetail{
					Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
					HttpStatusCode: 404,
					Message:        "session not found",
				},
			}, nil
		}
		snapshot := map[string]any{
			"session_id":           sess.SessionId,
			"model":                sess.Model,
			"profile":              sess.Profile,
			"harness_env":          sess.HarnessEnv,
			"harness_options_json": string(sess.HarnessOptionsJson),
			"available_tools":      sess.AvailableTools,
			"excluded_tools":       sess.ExcludedTools,
			"provider":             sess.Provider,
			"mcp_servers":          sess.McpServers,
		}
		raw, _ := json.Marshal(snapshot)
		return &harnessv1.ReadMCPResourceResponse{
			Contents: []*harnessv1.MCPResourceContent{{
				Uri:      req.Uri,
				MimeType: "application/json",
				Text:     string(raw),
			}},
		}, nil
	}
	return &harnessv1.ReadMCPResourceResponse{
		Contents: []*harnessv1.MCPResourceContent{{
			Uri:      req.Uri,
			MimeType: "text/plain",
			Text:     "resource:" + req.ServerName + ":" + req.Uri,
		}},
	}, nil
}

func (s *loopbackServer) CloseSession(_ context.Context, req *harnessv1.CloseSessionRequest) (*harnessv1.CloseSessionResponse, error) {
	s.mu.Lock()
	delete(s.sessions, req.SessionId)
	s.mu.Unlock()
	return &harnessv1.CloseSessionResponse{}, nil
}

func main() {
	harnessplugin.Serve(newLoopbackServer())
}
