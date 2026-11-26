package runtime_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func buildLocalLoopbackPlugin(t *testing.T) string {
	t.Helper()
	binName := "sofa-harness-loopback"
	if goruntime.GOOS == "windows" {
		binName += ".exe"
	}
	outPath := filepath.Join(t.TempDir(), binName)
	cmd := exec.Command("go", "build", "-o", outPath, "./testdata/loopbackplugin")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build loopback plugin: %s", string(out))
	return outPath
}

func TestLocalLoopbackBridgeAllRPCsAndSecurity(t *testing.T) {
	t.Setenv("SOFA_HOST_SECRET", "topsecret-must-not-leak")

	pluginBin := buildLocalLoopbackPlugin(t)
	opener, err := runtime.NewLocalSubprocessOpener(pluginBin)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = opener.Close()
	})

	// 1. GetCapabilities
	caps, err := opener.Capabilities(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "loopback", caps.HarnessName)
	assert.Equal(t, uint32(1), caps.ProtocolVersion)
	assert.Equal(t, []string{"openai", "azure", "anthropic"}, caps.SupportedProviderTypes)
	assert.True(t, caps.SupportsSessionResume)
	assert.True(t, caps.SupportsMCPServers)
	assert.True(t, caps.SupportsMCPResources)
	assert.True(t, caps.SupportsSkills)

	// 2. Rogue plain or self-signed gRPC client dialing listening address MUST be rejected at mTLS handshake.
	addr := opener.Addr()
	require.NotNil(t, addr)
	assertRogueClientsRejected(t, addr)

	// 3. In-memory credential materialization (Provider, MCPServer, HarnessEnv, Options).
	lookupEnv := func(key string) (string, bool) {
		switch key {
		case "OPENAI_KEY":
			return "sk-live-secret", true
		case "MCP_BEARER":
			return "mcp-token-secret", true
		case "MCP_STDIO_SECRET":
			return "stdio-secret-val", true
		case "HARNESS_SECRET":
			return "harness-token-val", true
		default:
			return "", false
		}
	}
	wireAPI := provider.WireAPIResponses
	transport := provider.TransportHTTP
	apiKeyRef := "OPENAI_KEY"
	matProvider, err := runtime.MaterializeProviderConfig(provider.Config{
		Type:      provider.TypeOpenAI,
		Endpoint:  "https://api.example.invalid/v1",
		WireAPI:   &wireAPI,
		Transport: &transport,
		Headers: cty.MapVal(map[string]cty.Value{
			"X-Custom": cty.StringVal("header-v1"),
		}),
		APIKeyRef: &apiKeyRef,
	}, "gpt-5", "coder", lookupEnv)
	require.NoError(t, err)

	bearerRef := "MCP_BEARER"
	matHTTPMCP, err := runtime.MaterializeMCPServerConfig(mcppkg.Config{
		Name:      "docs-http",
		Tools:     []string{"search"},
		Timeout:   15 * time.Second,
		Transport: mcppkg.TransportHTTP,
		HTTP: &mcppkg.HTTPConfig{
			URL:            "https://mcp.example.invalid",
			Headers:        map[string]string{"X-Org": "sofa"},
			BearerTokenRef: &bearerRef,
		},
	}, lookupEnv)
	require.NoError(t, err)

	matStdioMCP, err := runtime.MaterializeMCPServerConfig(mcppkg.Config{
		Name:      "local-stdio",
		Tools:     []string{"read"},
		Timeout:   10 * time.Second,
		Transport: mcppkg.TransportStdio,
		Stdio: &mcppkg.StdioConfig{
			Command:          "mcp-bin",
			Args:             []string{"--stdio"},
			Env:              map[string]string{"PLAIN": "ok"},
			EnvRefs:          map[string]string{"SECRET": "MCP_STDIO_SECRET"},
			WorkingDirectory: "/workspace",
		},
	}, lookupEnv)
	require.NoError(t, err)

	matHarnessEnv, err := runtime.MaterializeHarnessEnv(map[string]string{"H_TOKEN": "HARNESS_SECRET"}, lookupEnv)
	require.NoError(t, err)

	optsJSON, err := runtime.MarshalHarnessOptions(cty.ObjectVal(map[string]cty.Value{
		"max_steps": cty.NumberIntVal(12),
	}))
	require.NoError(t, err)

	// 4. OpenSession error propagation (missing resume session & unsupported provider).
	_, err = opener.Open(t.Context(), runtime.SessionConfig{
		SessionID:       "s-err-1",
		ResumeSessionID: "missing-session",
		Model:           "gpt-5",
	})
	var hErr *runtime.HarnessError
	require.ErrorAs(t, err, &hErr)
	assert.Equal(t, runtime.ErrorCategorySessionNotFound, hErr.Category)
	assert.Equal(t, int32(404), hErr.HTTPStatusCode)

	// 5. OpenSession success with materialized credentials + hooks + parallel tool handlers.
	var (
		mu             sync.Mutex
		postHookInputs []runtime.PostToolUseInput
	)
	barrierCh := make(chan struct{})
	var barrierCount int
	var barrierMu sync.Mutex

	sess, err := opener.Open(t.Context(), runtime.SessionConfig{
		SessionID:             "sess-1",
		Model:                 "gpt-5",
		Profile:               "coder",
		SystemPrompt:          "follow protocol",
		WorkingDirectory:      "/workspace",
		SessionStoreDirectory: "/.sofa/session-store",
		PermissionMode:        "approve_all",
		Provider:              &matProvider,
		HarnessEnv:            matHarnessEnv,
		HarnessOptionsJSON:    optsJSON,
		HostTools: []runtime.HostToolSpec{
			{Name: "echo_host", Description: "echo", ParametersJSONSchema: []byte(`{"type":"object"}`)},
			{Name: "tool_a", Description: "a", ParametersJSONSchema: []byte(`{"type":"object"}`)},
			{Name: "tool_b", Description: "b", ParametersJSONSchema: []byte(`{"type":"object"}`)},
		},
		AvailableTools: runtime.ToolFilter{Configured: true, Names: []string{"bash", "echo_host", "tool_a", "tool_b"}},
		ExcludedTools:  runtime.ToolFilter{Configured: true, Names: []string{}},
		MCPServers:     []runtime.MCPServerConfig{matHTTPMCP, matStdioMCP},
		PreToolUseHook: func(_ context.Context, input runtime.PreToolUseInput) (runtime.PreToolUseOutput, error) {
			if input.ToolName == "bash" {
				return runtime.PreToolUseOutput{
					Allow:                 true,
					ModifiedArgumentsJSON: []byte(`{"cmd":"ls -la"}`),
					AdditionalContext:     "human says check hidden files",
				}, nil
			}
			return runtime.PreToolUseOutput{
				Allow:      false,
				DenyReason: "edit forbidden by gate",
			}, nil
		},
		PostToolUseHook: func(_ context.Context, input runtime.PostToolUseInput) error {
			mu.Lock()
			postHookInputs = append(postHookInputs, input)
			mu.Unlock()
			return nil
		},
		ToolHandlers: map[string]runtime.ToolHandler{
			"echo_host": func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
				return runtime.ToolOutput{
					ResultType:       runtime.HostToolResultSuccess,
					TextResultForLLM: "echoed:" + string(call.ArgumentsJSON),
				}, nil
			},
			"tool_a": func(ctx context.Context, _ runtime.ToolInvocation) (runtime.ToolOutput, error) {
				barrierMu.Lock()
				barrierCount++
				if barrierCount == 2 {
					close(barrierCh)
				}
				barrierMu.Unlock()
				select {
				case <-barrierCh:
				case <-ctx.Done():
					return runtime.ToolOutput{}, ctx.Err()
				}
				return runtime.ToolOutput{ResultType: runtime.HostToolResultSuccess, TextResultForLLM: "res-a"}, nil
			},
			"tool_b": func(ctx context.Context, _ runtime.ToolInvocation) (runtime.ToolOutput, error) {
				barrierMu.Lock()
				barrierCount++
				if barrierCount == 2 {
					close(barrierCh)
				}
				barrierMu.Unlock()
				select {
				case <-barrierCh:
				case <-ctx.Done():
					return runtime.ToolOutput{}, ctx.Err()
				}
				return runtime.ToolOutput{ResultType: runtime.HostToolResultSuccess, TextResultForLLM: "res-b"}, nil
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-1", sess.ID())

	// 6. Verify PLUGIN_* and SOFA_HARNESS_PLUGIN are scrubbed inside plugin process AND SOFA_HOST_SECRET did not leak!
	envRes, err := sess.ReadMCPResource(t.Context(), "loopback", "loopback://env")
	require.NoError(t, err)
	require.Len(t, envRes, 1)
	var pluginEnv []string
	require.NoError(t, json.Unmarshal([]byte(envRes[0].Text), &pluginEnv))
	for _, entry := range pluginEnv {
		key, _, _ := strings.Cut(entry, "=")
		assert.False(t, strings.HasPrefix(key, "PLUGIN_"), "PLUGIN_* var %q must be scrubbed inside plugin", key)
		assert.NotEqual(t, "SOFA_HARNESS_PLUGIN", key, "SOFA_HARNESS_PLUGIN must be scrubbed inside plugin")
		assert.NotEqual(t, "SOFA_HOST_SECRET", key, "host secret must not leak via SkipHostEnv: true")
	}

	// 7. Verify materialized ProviderConfig, HarnessEnv, MCPServers, and ToolFilter arrived intact over AutoMTLS.
	sessRes, err := sess.ReadMCPResource(t.Context(), "loopback", "loopback://session")
	require.NoError(t, err)
	require.Len(t, sessRes, 1)
	var sessSnapshot map[string]any
	require.NoError(t, json.Unmarshal([]byte(sessRes[0].Text), &sessSnapshot))
	assert.Equal(t, `{"max_steps":12}`, sessSnapshot["harness_options_json"])
	assert.Contains(t, sessRes[0].Text, "sk-live-secret")
	assert.Contains(t, sessRes[0].Text, "Bearer mcp-token-secret")
	assert.Contains(t, sessRes[0].Text, "stdio-secret-val")
	assert.Contains(t, sessRes[0].Text, "harness-token-val")

	// 8. RunTurn: simple streaming events + host tool invocation.
	var events []runtime.Event
	sum1, err := sess.RunTurn(t.Context(), "prompt:simple", func(ev runtime.Event) {
		events = append(events, ev)
	})
	require.NoError(t, err)
	assert.Equal(t, "msg-simple", sum1.AssistantMessageID)
	assert.Contains(t, sum1.FinalContent, "echoed:")
	require.Len(t, events, 3)
	assert.Equal(t, runtime.EventAssistantReasoningDelta, events[0].Kind)
	assert.Equal(t, runtime.EventAssistantMessageDelta, events[1].Kind)
	assert.Equal(t, runtime.EventAssistantUsage, events[2].Kind)
	require.NotNil(t, events[2].Usage)
	assert.Equal(t, int64(10), events[2].Usage.InputTokens)

	// 9. RunTurn: PreToolUse allow+deny, PostToolUse failure, and 2 concurrent InvokeHostTool calls synchronized via barrier.
	sum2, err := sess.RunTurn(t.Context(), "prompt:hooks-and-parallel", nil)
	require.NoError(t, err)
	var hpSummary map[string]string
	require.NoError(t, json.Unmarshal([]byte(sum2.FinalContent), &hpSummary))
	assert.Equal(t, `{"cmd":"ls -la"}`, hpSummary["modified_args"])
	assert.Equal(t, "human says check hidden files", hpSummary["additional_context"])
	assert.Equal(t, "edit forbidden by gate", hpSummary["deny_reason"])
	assert.Equal(t, "res-a", hpSummary["tool_a"])
	assert.Equal(t, "res-b", hpSummary["tool_b"])

	mu.Lock()
	require.Len(t, postHookInputs, 1)
	assert.Equal(t, "bash", postHookInputs[0].ToolName)
	assert.False(t, postHookInputs[0].Succeeded)
	assert.Equal(t, "command exited 1", postHookInputs[0].ErrorMessage)
	mu.Unlock()

	// 10. RunTurn: Rate-limit error mapped to *HarnessError.
	_, err = sess.RunTurn(t.Context(), "prompt:rate-limit", nil)
	require.ErrorAs(t, err, &hErr)
	assert.Equal(t, runtime.ErrorCategoryRateLimit, hErr.Category)
	assert.Equal(t, int32(429), hErr.HTTPStatusCode)

	// 11. AbortTurn interrupts a blocked RunTurn without blocking on the turn mutex.
	startedCh := make(chan struct{})
	var startedOnce sync.Once
	turnDoneCh := make(chan error, 1)
	go func() {
		_, runErr := sess.RunTurn(t.Context(), "prompt:block-until-abort", func(ev runtime.Event) {
			if ev.Kind == runtime.EventSessionWarning && ev.Text == "blocking" {
				startedOnce.Do(func() { close(startedCh) })
			}
		})
		turnDoneCh <- runErr
	}()

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for blocked RunTurn to start")
	}

	require.NoError(t, sess.Abort(t.Context()))

	select {
	case abortErr := <-turnDoneCh:
		var abortHErr *runtime.HarnessError
		require.ErrorAs(t, abortErr, &abortHErr)
		assert.Equal(t, runtime.ErrorCategoryTransient, abortHErr.Category)
	case <-time.After(5 * time.Second):
		t.Fatal("RunTurn did not exit promptly after Abort")
	}

	// 12. ResumeSession & generic ReadMCPResource & CloseSession.
	require.NoError(t, sess.Resume(t.Context()))
	resContents, err := sess.ReadMCPResource(t.Context(), "docs-http", "doc://readme")
	require.NoError(t, err)
	require.Len(t, resContents, 1)
	assert.Equal(t, "resource:docs-http:doc://readme", resContents[0].Text)
	require.NoError(t, sess.Close(t.Context()))

	// Resume after CloseSession returns SessionNotFound.
	err = sess.Resume(t.Context())
	require.ErrorAs(t, err, &hErr)
	assert.Equal(t, runtime.ErrorCategorySessionNotFound, hErr.Category)
}

func assertRogueClientsRejected(t *testing.T, addr net.Addr) {
	t.Helper()

	dialOpt := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, addr.Network(), addr.String())
	})

	// 1. Plaintext gRPC client without TLS must fail.
	plainConn, err := grpc.NewClient("passthrough:///rogue", dialOpt, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = plainConn.Close() }()
	plainClient := harnessv1.NewAgentHarnessClient(plainConn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = plainClient.GetCapabilities(ctx, &harnessv1.GetCapabilitiesRequest{})
	require.Error(t, err, "plaintext client must be rejected by AutoMTLS server")

	// 2. TLS client presenting a rogue self-signed client certificate must fail mTLS verification.
	rogueCert := generateRogueClientCert(t)
	rogueTLS := &tls.Config{
		Certificates:       []tls.Certificate{rogueCert},
		InsecureSkipVerify: true,
		ServerName:         "localhost",
		MinVersion:         tls.VersionTLS12,
	}
	tlsConn, err := grpc.NewClient("passthrough:///rogue-tls", dialOpt, grpc.WithTransportCredentials(credentials.NewTLS(rogueTLS)))
	require.NoError(t, err)
	defer func() { _ = tlsConn.Close() }()
	tlsClient := harnessv1.NewAgentHarnessClient(tlsConn)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_, err = tlsClient.GetCapabilities(ctx2, &harnessv1.GetCapabilitiesRequest{})
	require.Error(t, err, "rogue self-signed client cert must be rejected at mTLS handshake")
}

func generateRogueClientCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(999),
		Subject:      pkix.Name{CommonName: "rogue"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
}

// Keep unused import check happy if harnessspec is referenced in helper tests.
var _ = harnessspec.HarnessConfig{}

func TestHarnessErrorFormatting(t *testing.T) {
	t.Parallel()
	var nilErr *runtime.HarnessError
	assert.Equal(t, "", nilErr.Error())

	noStatus := &runtime.HarnessError{Category: runtime.ErrorCategorySessionNotFound, Message: "missing"}
	assert.Equal(t, "harness error (category=7): missing", noStatus.Error())

	withStatus := &runtime.HarnessError{Category: runtime.ErrorCategoryRateLimit, HTTPStatusCode: 429, Message: "slow down"}
	assert.Equal(t, "harness error (category=2 status=429): slow down", withStatus.Error())
}

func TestContainerRunnerLinuxUDSAndWindowsTCPConfig(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	harnessDir := t.TempDir()
	tmpDir := t.TempDir()

	// Linux host mode: mounts tmpDir at /.sofa/plugin, rewrites PLUGIN_UNIX_SOCKET_DIR, translates UDS path.
	linuxRunnerFn := runtime.NewContainerRunnerFunc(
		sandboxConfigForTest(ws, harnessDir, "linux"),
		"podman",
	)
	linuxCmd := exec.Command("ignored")
	linuxCmd.Env = []string{
		"SOFA_HARNESS_PLUGIN=sofa-harness-v1",
		"PLUGIN_UNIX_SOCKET_DIR=" + tmpDir,
		"PLUGIN_CLIENT_CERT=cert-pem",
		"HOST_SECRET=must-be-dropped",
	}
	rLinux, err := linuxRunnerFn(nil, linuxCmd, tmpDir)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rLinux.ID(), "sofa-harness-"))

	netType, hostAddr, err := rLinux.PluginToHost("unix", "/.sofa/plugin/plugin123")
	require.NoError(t, err)
	assert.Equal(t, "unix", netType)
	assert.Equal(t, filepath.Join(tmpDir, "plugin123"), hostAddr)

	_, _, err = rLinux.PluginToHost("unix", "/tmp/outside")
	require.Error(t, err)

	hNet, hAddr, err := rLinux.HostToPlugin("tcp", "127.0.0.1:8080")
	require.NoError(t, err)
	assert.Equal(t, "tcp", hNet)
	assert.Equal(t, "127.0.0.1:8080", hAddr)

	// Windows host mode: rejects network="none" for agent sessions.
	badCfg := sandboxConfigForTest(ws, harnessDir, "windows")
	badCfg.Sandbox.Network = "none"
	_, err = runtime.NewContainerRunnerFunc(badCfg, "podman")(nil, exec.Command("ignored"), tmpDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none")
}

func TestMaterializeMissingSecretErrors(t *testing.T) {
	t.Parallel()
	missingLookup := func(string) (string, bool) { return "", false }

	missingKey := "MISSING_OPENAI_KEY"
	_, err := runtime.MaterializeProviderConfig(provider.Config{
		Type:      provider.TypeOpenAI,
		Endpoint:  "https://api.example.invalid/v1",
		APIKeyRef: &missingKey,
	}, "gpt-5", "gpt-5", missingLookup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING_OPENAI_KEY")

	missingBearer := "MISSING_MCP_TOKEN"
	_, err = runtime.MaterializeMCPServerConfig(mcppkg.Config{
		Name:      "http-srv",
		Tools:     []string{"t"},
		Timeout:   5 * time.Second,
		Transport: mcppkg.TransportHTTP,
		HTTP: &mcppkg.HTTPConfig{
			URL:            "https://mcp.example.invalid",
			BearerTokenRef: &missingBearer,
		},
	}, missingLookup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING_MCP_TOKEN")

	_, err = runtime.MaterializeHarnessEnv(map[string]string{"GH": "MISSING_GH"}, missingLookup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING_GH")
}

type failingSendMockClient struct {
	harnessv1.AgentHarnessClient
	sendErr error
}

func (m *failingSendMockClient) OpenSession(_ context.Context, req *harnessv1.OpenSessionRequest, _ ...grpc.CallOption) (*harnessv1.OpenSessionResponse, error) {
	return &harnessv1.OpenSessionResponse{SessionId: req.GetSessionId()}, nil
}

func (m *failingSendMockClient) RunTurn(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[harnessv1.HostTurnFrame, harnessv1.PluginTurnFrame], error) {
	return &failingSendMockStream{ctx: ctx, sendErr: m.sendErr}, nil
}

type failingSendMockStream struct {
	ctx       context.Context
	sendErr   error
	recvCount int
}

func (s *failingSendMockStream) Send(frame *harnessv1.HostTurnFrame) error {
	// Allow initial StartTurnRequest; fail on InvokeHostToolResponse.
	if frame.GetInvokeHostToolResponse() != nil {
		return s.sendErr
	}
	return nil
}

func (s *failingSendMockStream) Recv() (*harnessv1.PluginTurnFrame, error) {
	if s.recvCount == 0 {
		s.recvCount++
		return &harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_InvokeHostTool{
				InvokeHostTool: &harnessv1.InvokeHostToolRequest{
					ToolCallId:    "tc-fail-send",
					ToolName:      "echo_host",
					ArgumentsJson: []byte(`{}`),
				},
			},
		}, nil
	}
	// Block until stream context is cancelled, simulating plugin waiting for InvokeHostToolResponse.
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (s *failingSendMockStream) Header() (metadata.MD, error) { return nil, nil }
func (s *failingSendMockStream) Trailer() metadata.MD         { return nil }
func (s *failingSendMockStream) CloseSend() error             { return nil }
func (s *failingSendMockStream) Context() context.Context     { return s.ctx }
func (s *failingSendMockStream) SendMsg(any) error            { return nil }
func (s *failingSendMockStream) RecvMsg(any) error            { return nil }

func TestRunTurnUnblocksAndReturnsSendErrorOnCallbackFailure(t *testing.T) {
	t.Parallel()

	expectedSendErr := errors.New("stream send broken pipe")
	opener := runtime.NewGRPCClientOpener(&failingSendMockClient{sendErr: expectedSendErr})
	sess, err := opener.Open(t.Context(), runtime.SessionConfig{
		SessionID: "sess-send-err",
		Model:     "gpt-5",
		ToolHandlers: map[string]runtime.ToolHandler{
			"echo_host": func(_ context.Context, _ runtime.ToolInvocation) (runtime.ToolOutput, error) {
				return runtime.ToolOutput{ResultType: runtime.HostToolResultSuccess, TextResultForLLM: "ok"}, nil
			},
		},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = sess.RunTurn(ctx, "trigger", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, expectedSendErr)
}

func sandboxConfigForTest(ws, harnessDir, hostOS string) sandboxruntime.SessionMountLayout {
	return sandboxruntime.SessionMountLayout{
		Sandbox: sandboxspec.SandboxConfig{
			Runtime: "podman",
			Image:   "alpine:latest",
			Network: "bridge",
		},
		HostOS:           hostOS,
		Entrypoint:       "/.sofa/harness/sofa-harness-loopback",
		HostWorkspaceDir: ws,
		HarnessDir:       harnessDir,
	}
}
