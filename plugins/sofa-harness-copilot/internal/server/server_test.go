package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func startTestGRPCServer(t *testing.T, srv *Server) harnessv1.AgentHarnessClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := grpc.NewServer()
	harnessv1.RegisterAgentHarnessServer(grpcSrv, srv)
	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	t.Cleanup(func() {
		grpcSrv.Stop()
		_ = lis.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return harnessv1.NewAgentHarnessClient(conn)
}

func TestGetCapabilities(t *testing.T) {
	t.Parallel()
	srv := New(WithVersion("0.19.0"))
	caps, err := srv.GetCapabilities(t.Context(), &harnessv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Equal(t, "copilot", caps.GetHarnessName())
	assert.Equal(t, "0.19.0", caps.GetHarnessVersion())
	assert.Equal(t, uint32(1), caps.GetProtocolVersion())
	assert.Equal(t, []string{"openai", "azure", "anthropic"}, caps.GetSupportedProviderTypes())
	assert.True(t, caps.GetSupportsSessionResume())
	assert.True(t, caps.GetSupportsMcpServers())
	assert.True(t, caps.GetSupportsMcpResources())
	assert.True(t, caps.GetSupportsSkills())
}

func TestCLIPathDiscoveryPrecedenceAndHarnessEnvIsolation(t *testing.T) {
	// Not parallel because we check process env is NOT mutated.
	const sentinelKey = "SOFA_TEST_COPILOT_HARNESS_TOKEN"
	t.Setenv(sentinelKey, "")
	_ = os.Unsetenv(sentinelKey)

	t.Run("prefers bundled bin/copilot next to executable", func(t *testing.T) {
		tmp := t.TempDir()
		exePath := filepath.Join(tmp, "sofa-harness-copilot")
		bundledCLI := filepath.Join(tmp, "bin", "copilot")
		require.NoError(t, os.MkdirAll(filepath.Dir(bundledCLI), 0o755))
		require.NoError(t, os.WriteFile(exePath, []byte("exe"), 0o755))
		require.NoError(t, os.WriteFile(bundledCLI, []byte("cli"), 0o755))

		var gotPath string
		var gotEnv []string
		fc := &fakeSDKClient{}
		srv := New(
			WithExecutablePath(func() (string, error) { return exePath, nil }),
			WithLookPath(func(string) (string, error) { return "/usr/bin/copilot", nil }),
			WithClientFactory(func(cliPath string, env []string) (sdkClient, error) {
				gotPath = cliPath
				gotEnv = slices.Clone(env)
				return fc, nil
			}),
		)

		resp, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId:          "s-cli-1",
			Model:              "gpt-5",
			HarnessEnv:         map[string]string{sentinelKey: "secret-gh-token"},
			HarnessOptionsJson: []byte(`{"cli_path":"/opt/override/copilot"}`),
		})
		require.NoError(t, err)
		require.Nil(t, resp.GetError())
		assert.Equal(t, bundledCLI, gotPath)
		assert.Contains(t, gotEnv, sentinelKey+"=secret-gh-token")
		_, leaked := os.LookupEnv(sentinelKey)
		assert.False(t, leaked, "HarnessEnv must NEVER be written to plugin process env via os.Setenv")
	})

	t.Run("falls back to options cli_path then LookPath", func(t *testing.T) {
		tmp := t.TempDir()
		exePath := filepath.Join(tmp, "sofa-harness-copilot")

		var gotPath string
		fc := &fakeSDKClient{}
		srv := New(
			WithExecutablePath(func() (string, error) { return exePath, nil }),
			WithLookPath(func(string) (string, error) { return "/usr/local/bin/copilot", nil }),
			WithClientFactory(func(cliPath string, env []string) (sdkClient, error) {
				gotPath = cliPath
				return fc, nil
			}),
		)

		resp, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId:          "s-cli-2",
			Model:              "gpt-5",
			HarnessOptionsJson: []byte(`{"cli_path":"/opt/custom/copilot"}`),
		})
		require.NoError(t, err)
		require.Nil(t, resp.GetError())
		assert.Equal(t, "/opt/custom/copilot", gotPath)

		resp2, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId: "s-cli-3",
			Model:     "gpt-5",
		})
		require.NoError(t, err)
		require.Nil(t, resp2.GetError())
		assert.Equal(t, "/usr/local/bin/copilot", gotPath)
	})
}

func TestOpenSessionProviderAndConfigMapping(t *testing.T) {
	t.Parallel()

	t.Run("orthogonal provider, tool filters, skills custom agent, mcp servers", func(t *testing.T) {
		t.Parallel()
		fc := &fakeSDKClient{}
		srv := New(
			WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
			WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
		)

		resp, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId:             "sess-map-1",
			Model:                 "gpt-5.4",
			Profile:               "coder-profile",
			ReasoningEffort:       "high",
			SystemPrompt:          "follow sofa protocol",
			WorkingDirectory:      "/workspace",
			SessionStoreDirectory: "/.sofa/session-store",
			Provider: &harnessv1.ProviderConfig{
				Type:      "openai",
				BaseUrl:   "https://models.example.test/v1",
				WireApi:   "responses",
				Transport: "websockets",
				Headers:   map[string]string{"X-Test": "1"},
				ModelId:   "coder-profile",
				WireModel: "gpt-5.4",
				Auth:      &harnessv1.ProviderConfig_ApiKey{ApiKey: "sk-123"},
			},
			AvailableTools: &harnessv1.ToolFilter{Configured: true, Names: []string{"bash", "sofa_checkpoint"}},
			ExcludedTools:  &harnessv1.ToolFilter{Configured: true, Names: nil},
			SkillDirectories: []string{
				"/.sofa/bundle/skills",
			},
			Skills:         []string{"go-tdd"},
			DisabledSkills: []string{"deprecated"},
			McpServers: []*harnessv1.MCPServerConfig{
				{
					Name:      "docs",
					Tools:     []string{"search"},
					TimeoutMs: 15000,
					Transport: &harnessv1.MCPServerConfig_Http{
						Http: &harnessv1.MCPHTTPTransport{
							Url:     "https://mcp.example.test",
							Headers: map[string]string{"Authorization": "Bearer abc"},
						},
					},
				},
				{
					Name:      "local",
					Tools:     []string{},
					TimeoutMs: 5000,
					Transport: &harnessv1.MCPServerConfig_Stdio{
						Stdio: &harnessv1.MCPStdioTransport{
							Command:          "uvx",
							Args:             []string{"mcp-tool"},
							Env:              map[string]string{"TOKEN": "xyz"},
							WorkingDirectory: "/workspace",
						},
					},
				},
			},
		})
		require.NoError(t, err)
		require.Nil(t, resp.GetError())
		assert.Equal(t, "sess-map-1", resp.GetSessionId())
		assert.False(t, resp.GetResumed())

		require.Len(t, fc.createConfigs, 1)
		cfg := fc.createConfigs[0]
		assert.Equal(t, "sess-map-1", cfg.SessionID)
		assert.Equal(t, "gpt-5.4", cfg.Model)
		assert.Equal(t, "high", cfg.ReasoningEffort)
		assert.Equal(t, "/workspace", cfg.WorkingDirectory)
		assert.Equal(t, "/.sofa/session-store", cfg.ConfigDirectory)
		require.NotNil(t, cfg.SystemMessage)
		assert.Equal(t, "append", cfg.SystemMessage.Mode)
		assert.Equal(t, "follow sofa protocol", cfg.SystemMessage.Content)
		assert.Equal(t, []string{"bash", "sofa_checkpoint"}, cfg.AvailableTools)
		assert.NotNil(t, cfg.ExcludedTools)
		assert.Empty(t, cfg.ExcludedTools)

		require.NotNil(t, cfg.Provider)
		assert.Equal(t, "openai", cfg.Provider.Type)
		assert.Equal(t, "https://models.example.test/v1", cfg.Provider.BaseURL)
		assert.Equal(t, "responses", cfg.Provider.WireAPI)
		assert.Equal(t, "websockets", cfg.Provider.Transport)
		assert.Equal(t, map[string]string{"X-Test": "1"}, cfg.Provider.Headers)
		assert.Equal(t, "coder-profile", cfg.Provider.ModelID)
		assert.Equal(t, "gpt-5.4", cfg.Provider.WireModel)
		assert.Equal(t, "sk-123", cfg.Provider.APIKey)

		assert.Equal(t, sdk.Bool(true), cfg.EnableSkills)
		assert.Equal(t, []string{"/.sofa/bundle/skills"}, cfg.SkillDirectories)
		assert.Equal(t, []string{"deprecated"}, cfg.DisabledSkills)
		assert.Equal(t, "sofa_worker", cfg.Agent)
		require.Len(t, cfg.CustomAgents, 1)
		assert.Equal(t, sdk.CustomAgentConfig{
			Name:            "sofa_worker",
			Prompt:          "follow sofa protocol",
			Skills:          []string{"go-tdd"},
			Model:           "coder-profile",
			ReasoningEffort: "high",
		}, cfg.CustomAgents[0])

		httpSrv, ok := cfg.MCPServers["docs"].(sdk.MCPHTTPServerConfig)
		require.True(t, ok)
		assert.Equal(t, "https://mcp.example.test", httpSrv.URL)
		assert.Equal(t, 15000, httpSrv.Timeout)
		stdioSrv, ok := cfg.MCPServers["local"].(sdk.MCPStdioServerConfig)
		require.True(t, ok)
		assert.Equal(t, "uvx", stdioSrv.Command)
		assert.Equal(t, []string{"mcp-tool"}, stdioSrv.Args)
		assert.Equal(t, map[string]string{"TOKEN": "xyz"}, stdioSrv.Env)
	})

	t.Run("nil provider uses native Copilot auth and unconfigured ToolFilter stays nil", func(t *testing.T) {
		t.Parallel()
		fc := &fakeSDKClient{}
		srv := New(
			WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
			WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
		)

		resp, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId: "sess-native",
			Model:     "claude-sonnet-4",
		})
		require.NoError(t, err)
		require.Nil(t, resp.GetError())
		require.Len(t, fc.createConfigs, 1)
		assert.Nil(t, fc.createConfigs[0].Provider)
		assert.Nil(t, fc.createConfigs[0].AvailableTools)
		assert.Nil(t, fc.createConfigs[0].ExcludedTools)
		assert.Empty(t, fc.createConfigs[0].CustomAgents)
		assert.Empty(t, fc.createConfigs[0].Agent)
	})

	t.Run("unsupported provider type returns ERROR_CATEGORY_UNSUPPORTED_CAPABILITY", func(t *testing.T) {
		t.Parallel()
		fc := &fakeSDKClient{}
		srv := New(
			WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
			WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
		)

		resp, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId: "sess-bad-provider",
			Model:     "gemini-2.5-pro",
			Provider: &harnessv1.ProviderConfig{
				Type:    "vertex",
				BaseUrl: "https://example.invalid",
			},
		})
		require.NoError(t, err)
		require.NotNil(t, resp.GetError())
		assert.Equal(t, harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY, resp.GetError().GetCategory())
		assert.Empty(t, fc.createConfigs)
	})

	t.Run("ResumeSessionId Session not found returns ERROR_CATEGORY_SESSION_NOT_FOUND without creating new session", func(t *testing.T) {
		t.Parallel()
		fc := &fakeSDKClient{
			resumeErr: errors.New("JSON-RPC Error -32603: Session not found: stale-id"),
		}
		srv := New(
			WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
			WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
		)

		resp, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
			SessionId:       "new-id",
			ResumeSessionId: "stale-id",
			Model:           "gpt-5",
		})
		require.NoError(t, err)
		require.NotNil(t, resp.GetError())
		assert.Equal(t, harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND, resp.GetError().GetCategory())
		assert.Equal(t, []string{"stale-id"}, fc.resumeIDs)
		assert.Empty(t, fc.createConfigs, "must not silently fall back to CreateSession inside plugin")
		assert.Equal(t, 1, fc.stopCalls)
	})
}

func TestRunTurnHooksAntiDoubleInterceptionAndStreaming(t *testing.T) {
	t.Parallel()

	var (
		mu             sync.Mutex
		bashHookErr    string
		bashModified   any
		bashAddCtx     string
		ckptToolResult sdk.ToolResult
	)
	fs := &fakeSDKSession{
		id: "sess-turn-1",
		onSend: func(ctx context.Context, cfg *sdk.SessionConfig, emit func(sdk.SessionEvent), _ sdk.MessageOptions) (*sdk.SessionEvent, error) {
			// Emit streaming events covering reasoning, message delta, usage (5 counters), tool events, subagent, warning, idle.
			emit(sdk.SessionEvent{ID: "e1", Data: &sdk.AssistantReasoningDeltaData{ReasoningID: "r-1", DeltaContent: "thinking..."}})
			emit(sdk.SessionEvent{ID: "e2", Data: &sdk.AssistantMessageDeltaData{MessageID: "m-1", DeltaContent: "working "}})
			inTok, outTok, reasTok, crTok, cwTok := int64(100), int64(25), int64(10), int64(40), int64(5)
			apiCall := "call-api-1"
			emit(sdk.SessionEvent{ID: "e3", Data: &sdk.AssistantUsageData{
				Model: "gpt-5", APICallID: &apiCall,
				InputTokens: &inTok, OutputTokens: &outTok, ReasoningTokens: &reasTok,
				CacheReadTokens: &crTok, CacheWriteTokens: &cwTok,
			}})

			// 1. Non-host built-in tool "bash": MUST trigger PreToolUse -> PostToolUse(succeeded=false).
			preOut, err := cfg.Hooks.OnPreToolUse(sdk.PreToolUseHookInput{
				SessionID: "sess-turn-1",
				ToolName:  "bash",
				ToolArgs:  map[string]any{"command": "ls"},
			}, sdk.HookInvocation{SessionID: "sess-turn-1"})
			if err != nil {
				return nil, err
			}
			mu.Lock()
			bashModified = preOut.ModifiedArgs
			bashAddCtx = preOut.AdditionalContext
			mu.Unlock()

			emit(sdk.SessionEvent{ID: "e4", Data: &sdk.ToolExecutionStartData{ToolCallID: "tc-bash", ToolName: "bash", Arguments: preOut.ModifiedArgs}})
			emit(sdk.SessionEvent{ID: "e5", Data: &sdk.ToolShellOutputData{ToolCallID: "tc-bash", Text: "exit status 1\n"}})
			emit(sdk.SessionEvent{ID: "e6", Data: &sdk.ToolExecutionCompleteData{
				ToolCallID: "tc-bash",
				Success:    false,
				Error:      &sdk.ToolExecutionCompleteError{Message: "command exited 1"},
			}})

			_, err = cfg.Hooks.OnPostToolUseFailure(sdk.PostToolUseFailureHookInput{
				SessionID: "sess-turn-1",
				ToolName:  "bash",
				ToolArgs:  preOut.ModifiedArgs,
				Error:     "command exited 1",
			}, sdk.HookInvocation{SessionID: "sess-turn-1"})
			if err != nil {
				mu.Lock()
				bashHookErr = err.Error()
				mu.Unlock()
			}

			// 2. Host tool "sofa_checkpoint": MUST NOT trigger PreToolUse/PostToolUse, even if SDK calls hook!
			hostPreOut, err := cfg.Hooks.OnPreToolUse(sdk.PreToolUseHookInput{
				SessionID: "sess-turn-1",
				ToolName:  "sofa_checkpoint",
				ToolArgs:  map[string]any{"completed": []string{"step1"}},
			}, sdk.HookInvocation{SessionID: "sess-turn-1"})
			if err != nil {
				return nil, err
			}
			if hostPreOut == nil || hostPreOut.PermissionDecision != "allow" {
				return nil, fmt.Errorf("expected host tool PreToolUse short-circuit allow")
			}
			// Invoke actual sdk.Tool handler for sofa_checkpoint.
			var ckptTool sdk.Tool
			for _, tl := range cfg.Tools {
				if tl.Name == "sofa_checkpoint" {
					ckptTool = tl
				}
			}
			res, err := ckptTool.Handler(sdk.ToolInvocation{
				SessionID:    "sess-turn-1",
				ToolCallID:   "tc-ckpt",
				ToolName:     "sofa_checkpoint",
				Arguments:    map[string]any{"completed": []string{"step1"}},
				TraceContext: ctx,
			})
			if err != nil {
				return nil, err
			}
			mu.Lock()
			ckptToolResult = res
			mu.Unlock()
			_, err = cfg.Hooks.OnPostToolUse(sdk.PostToolUseHookInput{
				SessionID: "sess-turn-1",
				ToolName:  "sofa_checkpoint",
			}, sdk.HookInvocation{SessionID: "sess-turn-1"})
			if err != nil {
				return nil, err
			}

			emit(sdk.SessionEvent{ID: "e7", Data: &sdk.SubagentStartedData{ToolCallID: "tc-sub", AgentName: "helper"}})
			emit(sdk.SessionEvent{ID: "e8", Data: &sdk.SubagentCompletedData{ToolCallID: "tc-sub", AgentName: "helper"}})
			emit(sdk.SessionEvent{ID: "e9", Data: &sdk.SessionWarningData{WarningType: "policy", Message: "headsup"}})
			emit(sdk.SessionEvent{ID: "e10", Data: &sdk.SessionIdleData{}})

			return &sdk.SessionEvent{
				ID: "final",
				Data: &sdk.AssistantMessageData{
					MessageID: "m-1",
					Content:   "turn finished",
				},
			}, nil
		},
	}
	fc := &fakeSDKClient{session: fs}
	srv := New(
		WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
		WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
	)
	client := startTestGRPCServer(t, srv)

	openResp, err := client.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
		SessionId: "sess-turn-1",
		Model:     "gpt-5",
		HostTools: []*harnessv1.HostToolSpec{
			{
				Name:                 "sofa_checkpoint",
				Description:          "checkpoint",
				ParametersJsonSchema: []byte(`{"type":"object"}`),
			},
		},
	})
	require.NoError(t, err)
	require.Nil(t, openResp.GetError())

	stream, err := client.RunTurn(t.Context())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&harnessv1.HostTurnFrame{
		Payload: &harnessv1.HostTurnFrame_StartTurn{
			StartTurn: &harnessv1.StartTurnRequest{
				SessionId: "sess-turn-1",
				TurnId:    "turn-1",
				Prompt:    "run task",
			},
		},
	}))

	var (
		events     []*harnessv1.SessionEvent
		preReqs    []*harnessv1.PreToolUseRequest
		postReqs   []*harnessv1.PostToolUseRequest
		hostTools  []string
		turnResult *harnessv1.TurnResult
	)
	for {
		frame, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		require.NoError(t, recvErr)

		switch p := frame.GetPayload().(type) {
		case *harnessv1.PluginTurnFrame_Event:
			events = append(events, p.Event)
		case *harnessv1.PluginTurnFrame_PreToolUse:
			preReqs = append(preReqs, p.PreToolUse)
			require.NoError(t, stream.Send(&harnessv1.HostTurnFrame{
				Payload: &harnessv1.HostTurnFrame_PreToolUseDecision{
					PreToolUseDecision: &harnessv1.PreToolUseDecision{
						HookCallId:            p.PreToolUse.GetHookCallId(),
						Allow:                 true,
						ModifiedArgumentsJson: []byte(`{"command":"ls -la"}`),
						AdditionalContext:     "human guidance",
					},
				},
			}))
		case *harnessv1.PluginTurnFrame_PostToolUse:
			postReqs = append(postReqs, p.PostToolUse)
			require.NoError(t, stream.Send(&harnessv1.HostTurnFrame{
				Payload: &harnessv1.HostTurnFrame_PostToolUseAck{
					PostToolUseAck: &harnessv1.PostToolUseAck{
						HookCallId: p.PostToolUse.GetHookCallId(),
					},
				},
			}))
		case *harnessv1.PluginTurnFrame_InvokeHostTool:
			hostTools = append(hostTools, p.InvokeHostTool.GetToolName())
			require.NoError(t, stream.Send(&harnessv1.HostTurnFrame{
				Payload: &harnessv1.HostTurnFrame_InvokeHostToolResponse{
					InvokeHostToolResponse: &harnessv1.InvokeHostToolResponse{
						ToolCallId:       p.InvokeHostTool.GetToolCallId(),
						ResultType:       harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_SUCCESS,
						TextResultForLlm: `{"accepted":true}`,
					},
				},
			}))
		case *harnessv1.PluginTurnFrame_TurnResult:
			turnResult = p.TurnResult
		}
	}

	require.NotNil(t, turnResult)
	assert.Nil(t, turnResult.GetError())
	assert.Equal(t, "m-1", turnResult.GetAssistantMessageId())
	assert.Equal(t, "turn finished", turnResult.GetFinalContent())

	require.Len(t, preReqs, 1, "host tool sofa_checkpoint MUST NEVER trigger PreToolUse")
	assert.Equal(t, "bash", preReqs[0].GetToolName())
	assert.NotEmpty(t, preReqs[0].GetToolCallId())
	require.Len(t, postReqs, 1, "host tool sofa_checkpoint MUST NEVER trigger PostToolUse")
	assert.Equal(t, "bash", postReqs[0].GetToolName())
	assert.Equal(t, preReqs[0].GetToolCallId(), postReqs[0].GetToolCallId())
	assert.False(t, postReqs[0].GetSucceeded())
	assert.Equal(t, "command exited 1", postReqs[0].GetErrorMessage())
	assert.Equal(t, []string{"sofa_checkpoint"}, hostTools)

	mu.Lock()
	assert.Empty(t, bashHookErr)
	assert.Equal(t, map[string]any{"command": "ls -la"}, bashModified)
	assert.Equal(t, "human guidance", bashAddCtx)
	assert.Equal(t, "success", ckptToolResult.ResultType)
	assert.Equal(t, `{"accepted":true}`, ckptToolResult.TextResultForLLM)
	mu.Unlock()

	require.Len(t, events, 10)
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA, events[0].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA, events[1].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_USAGE, events[2].GetKind())
	assert.Equal(t, int64(100), events[2].GetUsage().GetInputTokens())
	assert.Equal(t, int64(25), events[2].GetUsage().GetOutputTokens())
	assert.Equal(t, int64(10), events[2].GetUsage().GetReasoningTokens())
	assert.Equal(t, int64(40), events[2].GetUsage().GetCacheReadTokens())
	assert.Equal(t, int64(5), events[2].GetUsage().GetCacheWriteTokens())
	assert.Equal(t, "call-api-1", events[2].GetUsage().GetApiCallId())
	assert.NotEmpty(t, events[2].GetRawSdkEventJson())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_START, events[3].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_SHELL_OUTPUT, events[4].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_COMPLETE, events[5].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED, events[6].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_COMPLETED, events[7].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_WARNING, events[8].GetKind())
	assert.Equal(t, harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_IDLE, events[9].GetKind())
}

func TestAbortTurnCancelsBlockedSendAndWaitConcurrently(t *testing.T) {
	t.Parallel()

	startedCh := make(chan struct{})
	abortCalledCh := make(chan struct{})
	fs := &fakeSDKSession{
		id: "sess-abort-1",
		onSend: func(ctx context.Context, _ *sdk.SessionConfig, _ func(sdk.SessionEvent), _ sdk.MessageOptions) (*sdk.SessionEvent, error) {
			close(startedCh)
			select {
			case <-abortCalledCh:
				return nil, errors.New("aborted")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		onAbort: func(context.Context) error {
			close(abortCalledCh)
			return nil
		},
	}
	fc := &fakeSDKClient{session: fs}
	srv := New(
		WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
		WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
	)
	client := startTestGRPCServer(t, srv)

	_, err := client.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{SessionId: "sess-abort-1", Model: "gpt-5"})
	require.NoError(t, err)

	stream, err := client.RunTurn(t.Context())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&harnessv1.HostTurnFrame{
		Payload: &harnessv1.HostTurnFrame_StartTurn{
			StartTurn: &harnessv1.StartTurnRequest{SessionId: "sess-abort-1", TurnId: "turn-abort", Prompt: "hang"},
		},
	}))

	select {
	case <-startedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SendAndWait to block")
	}

	// Stale TurnId must NOT cancel or abort the active turn.
	staleResp, err := client.AbortTurn(t.Context(), &harnessv1.AbortTurnRequest{SessionId: "sess-abort-1", TurnId: "stale-turn-id"})
	require.NoError(t, err)
	require.Nil(t, staleResp.GetError())
	select {
	case <-abortCalledCh:
		t.Fatal("stale TurnId must not abort active turn")
	case <-time.After(30 * time.Millisecond):
	}

	abortResp, err := client.AbortTurn(t.Context(), &harnessv1.AbortTurnRequest{SessionId: "sess-abort-1", TurnId: "turn-abort"})
	require.NoError(t, err)
	require.Nil(t, abortResp.GetError())

	var tr *harnessv1.TurnResult
	for {
		frame, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		require.NoError(t, recvErr)
		if p, ok := frame.GetPayload().(*harnessv1.PluginTurnFrame_TurnResult); ok {
			tr = p.TurnResult
		}
	}
	require.NotNil(t, tr)
	require.NotNil(t, tr.GetError())
	assert.Equal(t, harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL, tr.GetError().GetCategory())
}

func TestResumeSessionReadMCPResourceAndCloseSession(t *testing.T) {
	t.Parallel()

	text := "hello resource"
	mime := "text/plain"
	resumedSess := &fakeSDKSession{
		id: "sess-res-1",
		mcpContents: []rpc.MCPResourceContent{
			{
				URI:      "doc://readme",
				MIMEType: &mime,
				Text:     &text,
				Meta:     map[string]any{"source": "docs"},
			},
		},
	}
	initialSess := &fakeSDKSession{id: "sess-res-1"}
	fc := &fakeSDKClient{session: initialSess, resumedSession: resumedSess}
	srv := New(
		WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
		WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
	)
	client := startTestGRPCServer(t, srv)

	openResp, err := client.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{SessionId: "sess-res-1", Model: "gpt-5"})
	require.NoError(t, err)
	require.Nil(t, openResp.GetError())

	resResp, err := client.ResumeSession(t.Context(), &harnessv1.ResumeSessionRequest{SessionId: "sess-res-1"})
	require.NoError(t, err)
	require.Nil(t, resResp.GetError())
	assert.Equal(t, 1, initialSess.disconnectCalls)
	assert.Equal(t, []string{"sess-res-1"}, fc.resumeIDs)

	readResp, err := client.ReadMCPResource(t.Context(), &harnessv1.ReadMCPResourceRequest{
		SessionId:  "sess-res-1",
		ServerName: "docs",
		Uri:        "doc://readme",
	})
	require.NoError(t, err)
	require.Nil(t, readResp.GetError())
	require.Len(t, readResp.GetContents(), 1)
	assert.Equal(t, "doc://readme", readResp.GetContents()[0].GetUri())
	assert.Equal(t, "text/plain", readResp.GetContents()[0].GetMimeType())
	assert.Equal(t, "hello resource", readResp.GetContents()[0].GetText())
	assert.Equal(t, map[string]string{"source": "docs"}, readResp.GetContents()[0].GetMeta())

	closeResp, err := client.CloseSession(t.Context(), &harnessv1.CloseSessionRequest{SessionId: "sess-res-1"})
	require.NoError(t, err)
	require.Nil(t, closeResp.GetError())
	assert.Equal(t, 1, resumedSess.disconnectCalls)
	assert.Equal(t, 1, fc.stopCalls)
}

func TestOpenSessionOverGRPCSerializesEmptyMCPToolsAsNonNilSlice(t *testing.T) {
	t.Parallel()

	fc := &fakeSDKClient{}
	srv := New(
		WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
		WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
	)
	client := startTestGRPCServer(t, srv)

	resp, err := client.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{
		SessionId: "sess-mcp-empty-tools",
		Model:     "gpt-5",
		McpServers: []*harnessv1.MCPServerConfig{
			{
				Name:      "local",
				Tools:     []string{},
				TimeoutMs: 5000,
				Transport: &harnessv1.MCPServerConfig_Stdio{
					Stdio: &harnessv1.MCPStdioTransport{Command: "uvx"},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Nil(t, resp.GetError())
	require.Len(t, fc.createConfigs, 1)
	stdioSrv, ok := fc.createConfigs[0].MCPServers["local"].(sdk.MCPStdioServerConfig)
	require.True(t, ok)
	assert.NotNil(t, stdioSrv.Tools, "resource-only MCP server must preserve non-nil []string{} over gRPC")
	assert.Empty(t, stdioSrv.Tools)
}

func TestResumeSessionClearsSDKAfterDisconnectSoRetryDoesNotDoubleDisconnect(t *testing.T) {
	t.Parallel()

	initialSess := &fakeSDKSession{id: "sess-retry-1"}
	resumedSess := &fakeSDKSession{id: "sess-retry-1"}
	fc := &fakeSDKClient{
		session:        initialSess,
		resumedSession: resumedSess,
		resumeErrs:     []error{errors.New("connection reset by peer"), nil},
	}
	srv := New(
		WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
		WithClientFactory(func(string, []string) (sdkClient, error) { return fc, nil }),
	)
	client := startTestGRPCServer(t, srv)

	openResp, err := client.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{SessionId: "sess-retry-1", Model: "gpt-5"})
	require.NoError(t, err)
	require.Nil(t, openResp.GetError())

	firstRes, err := client.ResumeSession(t.Context(), &harnessv1.ResumeSessionRequest{SessionId: "sess-retry-1"})
	require.NoError(t, err)
	require.NotNil(t, firstRes.GetError())
	assert.Equal(t, harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT, firstRes.GetError().GetCategory())

	secondRes, err := client.ResumeSession(t.Context(), &harnessv1.ResumeSessionRequest{SessionId: "sess-retry-1"})
	require.NoError(t, err)
	require.Nil(t, secondRes.GetError())
	assert.Equal(t, 1, initialSess.disconnectCalls, "retry after transient ResumeSession error must not disconnect initial session twice")
}

func TestOpenSessionDuplicateSessionIDCleansUpDisplacedSession(t *testing.T) {
	t.Parallel()

	sess1 := &fakeSDKSession{id: "dup-sess"}
	client1 := &fakeSDKClient{session: sess1}
	sess2 := &fakeSDKSession{id: "dup-sess"}
	client2 := &fakeSDKClient{session: sess2}

	callIdx := 0
	srv := New(
		WithLookPath(func(string) (string, error) { return "/bin/copilot", nil }),
		WithClientFactory(func(string, []string) (sdkClient, error) {
			callIdx++
			if callIdx == 1 {
				return client1, nil
			}
			return client2, nil
		}),
	)

	resp1, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{SessionId: "dup-sess", Model: "gpt-5"})
	require.NoError(t, err)
	require.Nil(t, resp1.GetError())

	resp2, err := srv.OpenSession(t.Context(), &harnessv1.OpenSessionRequest{SessionId: "dup-sess", Model: "gpt-5"})
	require.NoError(t, err)
	require.Nil(t, resp2.GetError())

	assert.Equal(t, 1, sess1.disconnectCalls)
	assert.Equal(t, 1, client1.stopCalls)
	assert.Equal(t, 0, sess2.disconnectCalls)
	assert.Equal(t, 0, client2.stopCalls)
}

func TestClassifyErrorAllCategories(t *testing.T) {
	t.Parallel()

	code429 := int32(429)
	code401 := int32(401)
	code403 := int32(403)
	code503 := int32(503)

	tests := []struct {
		name     string
		err      error
		wantCat  harnessv1.ErrorCategory
		wantCode int32
	}{
		{
			name:     "session error rate_limit 429",
			err:      &modelCallError{data: sdk.SessionErrorData{ErrorType: "rate_limit", StatusCode: &code429, Message: "too many requests"}, err: errors.New("rate limit")},
			wantCat:  harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT,
			wantCode: 429,
		},
		{
			name:     "session error authentication 401",
			err:      &modelCallError{data: sdk.SessionErrorData{ErrorType: "authentication", StatusCode: &code401, Message: "bad token"}, err: errors.New("auth")},
			wantCat:  harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHENTICATION,
			wantCode: 401,
		},
		{
			name:     "session error authorization 403",
			err:      &modelCallError{data: sdk.SessionErrorData{ErrorType: "authorization", StatusCode: &code403, Message: "forbidden"}, err: errors.New("forbidden")},
			wantCat:  harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHORIZATION,
			wantCode: 403,
		},
		{
			name:    "session error quota",
			err:     &modelCallError{data: sdk.SessionErrorData{ErrorType: "quota", Message: "quota exceeded"}, err: errors.New("quota")},
			wantCat: harnessv1.ErrorCategory_ERROR_CATEGORY_QUOTA,
		},
		{
			name:    "session error context_limit",
			err:     &modelCallError{data: sdk.SessionErrorData{ErrorType: "context_limit", Message: "prompt too long"}, err: errors.New("context limit")},
			wantCat: harnessv1.ErrorCategory_ERROR_CATEGORY_CONTEXT_LIMIT,
		},
		{
			name:     "http 503 transient",
			err:      &modelCallError{data: sdk.SessionErrorData{ErrorType: "query", StatusCode: &code503, Message: "upstream unavailable"}, err: errors.New("503")},
			wantCat:  harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT,
			wantCode: 503,
		},
		{
			name:    "network timeout transient",
			err:     errors.New("dial tcp: connection reset by peer"),
			wantCat: harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT,
		},
		{
			name:     "session not found",
			err:      errors.New("JSON-RPC Error -32603: Session not found: abc"),
			wantCat:  harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
			wantCode: 404,
		},
		{
			name:    "unknown falls back to internal",
			err:     errors.New("unexpected json parse failure"),
			wantCat: harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyError(tc.err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantCat, got.GetCategory())
			if tc.wantCode != 0 {
				assert.Equal(t, tc.wantCode, got.GetHttpStatusCode())
			}
		})
	}
}

type fakeSDKClient struct {
	mu             sync.Mutex
	createConfigs  []*sdk.SessionConfig
	resumeIDs      []string
	resumeConfigs  []*sdk.ResumeSessionConfig
	session        sdkSession
	resumedSession sdkSession
	createErr      error
	resumeErr      error
	resumeErrs     []error
	stopCalls      int
}

func (c *fakeSDKClient) Start(context.Context) error { return nil }

func (c *fakeSDKClient) Stop() error {
	c.mu.Lock()
	c.stopCalls++
	c.mu.Unlock()
	return nil
}

func (c *fakeSDKClient) CreateSession(_ context.Context, cfg *sdk.SessionConfig) (sdkSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createConfigs = append(c.createConfigs, cfg)
	if c.createErr != nil {
		return nil, c.createErr
	}
	if c.session == nil {
		c.session = &fakeSDKSession{id: cfg.SessionID}
	}
	if fs, ok := c.session.(*fakeSDKSession); ok {
		fs.cfg = cfg
	}
	return c.session, nil
}

func (c *fakeSDKClient) ResumeSession(_ context.Context, sessionID string, cfg *sdk.ResumeSessionConfig) (sdkSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resumeIDs = append(c.resumeIDs, sessionID)
	c.resumeConfigs = append(c.resumeConfigs, cfg)
	if len(c.resumeErrs) > 0 {
		err := c.resumeErrs[0]
		c.resumeErrs = c.resumeErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if c.resumeErr != nil {
		return nil, c.resumeErr
	}
	if c.resumedSession != nil {
		return c.resumedSession, nil
	}
	if c.session == nil {
		c.session = &fakeSDKSession{id: sessionID}
	}
	return c.session, nil
}

type fakeSDKSession struct {
	mu              sync.Mutex
	id              string
	cfg             *sdk.SessionConfig
	handlers        []sdk.SessionEventHandler
	onSend          func(context.Context, *sdk.SessionConfig, func(sdk.SessionEvent), sdk.MessageOptions) (*sdk.SessionEvent, error)
	onAbort         func(context.Context) error
	mcpContents     []rpc.MCPResourceContent
	disconnectCalls int
}

func (s *fakeSDKSession) ID() string { return s.id }

func (s *fakeSDKSession) On(handler sdk.SessionEventHandler) func() {
	s.mu.Lock()
	idx := len(s.handlers)
	s.handlers = append(s.handlers, handler)
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if idx < len(s.handlers) {
			s.handlers[idx] = nil
		}
		s.mu.Unlock()
	}
}

func (s *fakeSDKSession) emit(ev sdk.SessionEvent) {
	s.mu.Lock()
	handlers := slices.Clone(s.handlers)
	s.mu.Unlock()
	for _, h := range handlers {
		if h != nil {
			h(ev)
		}
	}
}

func (s *fakeSDKSession) SendAndWait(ctx context.Context, opts sdk.MessageOptions) (*sdk.SessionEvent, error) {
	if s.onSend != nil {
		return s.onSend(ctx, s.cfg, s.emit, opts)
	}
	return &sdk.SessionEvent{Data: &sdk.AssistantMessageData{MessageID: "default-msg", Content: "ok"}}, nil
}

func (s *fakeSDKSession) Abort(ctx context.Context) error {
	if s.onAbort != nil {
		return s.onAbort(ctx)
	}
	return nil
}

func (s *fakeSDKSession) Disconnect() error {
	s.mu.Lock()
	s.disconnectCalls++
	s.mu.Unlock()
	return nil
}

func (s *fakeSDKSession) ReadMCPResource(_ context.Context, _ string, _ string) ([]*harnessv1.MCPResourceContent, error) {
	out := make([]*harnessv1.MCPResourceContent, 0, len(s.mcpContents))
	for _, c := range s.mcpContents {
		item := &harnessv1.MCPResourceContent{Uri: c.URI}
		if c.MIMEType != nil {
			item.MimeType = *c.MIMEType
		}
		if c.Text != nil {
			item.Text = *c.Text
		}
		if len(c.Meta) > 0 {
			item.Meta = make(map[string]string, len(c.Meta))
			for k, v := range c.Meta {
				item.Meta[k] = fmt.Sprint(v)
			}
		}
		out = append(out, item)
	}
	return out, nil
}
