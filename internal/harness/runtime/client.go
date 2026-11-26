package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/lonegunmanb/sofa/pkg/harnessplugin"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// OpenerOptions tunes plugin startup and logging behavior for GRPCClientOpener.
type OpenerOptions struct {
	Logger       hclog.Logger
	StartTimeout time.Duration
	RuntimeBin   string
}

// GRPCClientOpener implements Opener over a hashicorp/go-plugin AutoMTLS gRPC client.
type GRPCClientOpener struct {
	pluginClient *plugin.Client
	addr         net.Addr
	rpcClient    harnessv1.AgentHarnessClient
}

var _ Opener = (*GRPCClientOpener)(nil)

// NewGRPCClientOpener wraps an already-connected AgentHarnessClient as an Opener.
func NewGRPCClientOpener(client harnessv1.AgentHarnessClient) *GRPCClientOpener {
	return &GRPCClientOpener{rpcClient: client}
}

// NewLocalSubprocessOpener starts a local plugin executable with SkipHostEnv and AutoMTLS.
func NewLocalSubprocessOpener(binPath string, args ...string) (*GRPCClientOpener, error) {
	return NewLocalSubprocessOpenerWithOptions(binPath, args, OpenerOptions{})
}

// NewLocalSubprocessOpenerWithOptions starts a local plugin executable using explicit options.
func NewLocalSubprocessOpenerWithOptions(binPath string, args []string, opts OpenerOptions) (*GRPCClientOpener, error) {
	if strings.TrimSpace(binPath) == "" {
		return nil, errors.New("plugin executable path is required")
	}
	cfg := baseClientConfig(opts)
	cfg.Cmd = exec.Command(binPath, args...)
	return NewOpenerFromClientConfig(cfg)
}

// NewContainerOpener starts a containerized plugin using ContainerRunner with SkipHostEnv and AutoMTLS.
func NewContainerOpener(layout sandboxruntime.SessionMountLayout, opts OpenerOptions) (*GRPCClientOpener, error) {
	cfg := baseClientConfig(opts)
	cfg.RunnerFunc = NewContainerRunnerFunc(layout, opts.RuntimeBin)
	return NewOpenerFromClientConfig(cfg)
}

func baseClientConfig(opts OpenerOptions) *plugin.ClientConfig {
	logger := opts.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	startTimeout := opts.StartTimeout
	if startTimeout <= 0 {
		startTimeout = 1 * time.Minute
	}
	return &plugin.ClientConfig{
		HandshakeConfig: harnessplugin.Handshake,
		Plugins: plugin.PluginSet{
			harnessplugin.PluginName: &harnessplugin.GRPCPlugin{},
		},
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SkipHostEnv:      true,
		AutoMTLS:         true,
		Logger:           logger,
		StartTimeout:     startTimeout,
	}
}

// NewOpenerFromClientConfig starts and connects a plugin.Client using the given configuration.
func NewOpenerFromClientConfig(cfg *plugin.ClientConfig) (*GRPCClientOpener, error) {
	if cfg == nil {
		return nil, errors.New("plugin client config is required")
	}
	client := plugin.NewClient(cfg)
	addr, err := client.Start()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("start harness plugin: %w", err)
	}
	rpcClientProtocol, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("connect harness plugin client: %w", err)
	}
	if err := waitForPluginPing(client, rpcClientProtocol, cfg.StartTimeout); err != nil {
		client.Kill()
		return nil, fmt.Errorf("wait for harness plugin health: %w", err)
	}
	raw, err := rpcClientProtocol.Dispense(harnessplugin.PluginName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("dispense %q plugin: %w", harnessplugin.PluginName, err)
	}
	grpcClient, ok := raw.(harnessv1.AgentHarnessClient)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("dispensed plugin %T does not implement AgentHarnessClient", raw)
	}
	return &GRPCClientOpener{
		pluginClient: client,
		addr:         addr,
		rpcClient:    grpcClient,
	}, nil
}

func waitForPluginPing(client *plugin.Client, proto plugin.ClientProtocol, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := proto.Ping(); err == nil {
			return nil
		} else if client.Exited() || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Addr returns the host-reachable network address of the connected plugin server.
func (o *GRPCClientOpener) Addr() net.Addr {
	if o == nil {
		return nil
	}
	return o.addr
}

// ContainerID returns the running container or process ID reported by go-plugin's runner.
func (o *GRPCClientOpener) ContainerID() string {
	if o == nil || o.pluginClient == nil {
		return ""
	}
	return o.pluginClient.ID()
}

// Close shuts down the underlying go-plugin client and removes any spawned container.
func (o *GRPCClientOpener) Close() error {
	if o == nil || o.pluginClient == nil {
		return nil
	}
	o.pluginClient.Kill()
	return nil
}

// Capabilities queries the plugin's GetCapabilities RPC.
func (o *GRPCClientOpener) Capabilities(ctx context.Context) (Capabilities, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := o.rpcClient.GetCapabilities(ctx, &harnessv1.GetCapabilitiesRequest{})
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		HarnessName:            resp.GetHarnessName(),
		HarnessVersion:         resp.GetHarnessVersion(),
		ProtocolVersion:        resp.GetProtocolVersion(),
		SupportedProviderTypes: append([]string(nil), resp.GetSupportedProviderTypes()...),
		SupportsSessionResume:  resp.GetSupportsSessionResume(),
		SupportsMCPServers:     resp.GetSupportsMcpServers(),
		SupportsMCPResources:   resp.GetSupportsMcpResources(),
		SupportsSkills:         resp.GetSupportsSkills(),
	}, nil
}

// Open invokes OpenSession over the AutoMTLS gRPC channel and returns a Session handle.
func (o *GRPCClientOpener) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req := toProtoOpenSessionRequest(cfg)
	resp, err := o.rpcClient.OpenSession(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.GetError() != nil {
		return nil, fromProtoError(resp.GetError())
	}
	sessionID := resp.GetSessionId()
	if sessionID == "" {
		sessionID = cfg.SessionID
	}

	handlers := make(map[string]ToolHandler, len(cfg.ToolHandlers))
	for name, h := range cfg.ToolHandlers {
		handlers[name] = h
	}

	return &GRPCSession{
		id:              sessionID,
		client:          o.rpcClient,
		preToolUseHook:  cfg.PreToolUseHook,
		postToolUseHook: cfg.PostToolUseHook,
		toolHandlers:    handlers,
	}, nil
}

// GRPCSession implements Session over AgentHarness gRPC.
type GRPCSession struct {
	id              string
	client          harnessv1.AgentHarnessClient
	preToolUseHook  PreToolUseHook
	postToolUseHook PostToolUseHook
	toolHandlers    map[string]ToolHandler

	turnMu       sync.Mutex
	activeTurnMu sync.Mutex
	activeTurnID string
}

var _ Session = (*GRPCSession)(nil)

// ID returns the effective session identifier.
func (s *GRPCSession) ID() string {
	return s.id
}

// RunTurn executes a single turn over the bidirectional RunTurn stream.
// At most one RunTurn stream is active per session at any time.
func (s *GRPCSession) RunTurn(ctx context.Context, prompt string, onEvent func(Event)) (TurnSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	turnID := "turn-" + uuid.NewString()
	s.setActiveTurnID(turnID)
	defer s.setActiveTurnID("")

	turnCtx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()

	stream, err := s.client.RunTurn(turnCtx)
	if err != nil {
		return TurnSummary{}, err
	}

	var sendMu sync.Mutex
	sendFrame := func(frame *harnessv1.HostTurnFrame) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(frame)
	}

	if err := sendFrame(&harnessv1.HostTurnFrame{
		Payload: &harnessv1.HostTurnFrame_StartTurn{
			StartTurn: &harnessv1.StartTurnRequest{
				SessionId: s.id,
				TurnId:    turnID,
				Prompt:    prompt,
			},
		},
	}); err != nil {
		return TurnSummary{}, err
	}

	var wg sync.WaitGroup
	var sendErrMu sync.Mutex
	var firstSendErr error
	recordSendErr := func(err error) {
		if err == nil {
			return
		}
		sendErrMu.Lock()
		if firstSendErr == nil {
			firstSendErr = err
		}
		sendErrMu.Unlock()
		cancelTurn()
	}

	for {
		frame, recvErr := stream.Recv()
		if recvErr != nil {
			cancelTurn()
			wg.Wait()
			sendErrMu.Lock()
			sErr := firstSendErr
			sendErrMu.Unlock()
			if sErr != nil && ctx.Err() == nil {
				return TurnSummary{}, sErr
			}
			return TurnSummary{}, recvErr
		}

		switch p := frame.GetPayload().(type) {
		case *harnessv1.PluginTurnFrame_Event:
			if onEvent != nil && p.Event != nil {
				onEvent(fromProtoEvent(p.Event))
			}

		case *harnessv1.PluginTurnFrame_PreToolUse:
			req := p.PreToolUse
			if req == nil {
				continue
			}
			wg.Add(1)
			go func(r *harnessv1.PreToolUseRequest) {
				defer wg.Done()
				out := PreToolUseOutput{Allow: true}
				if s.preToolUseHook != nil {
					hookOut, hookErr := s.preToolUseHook(turnCtx, PreToolUseInput{
						HookCallID:    r.GetHookCallId(),
						ToolCallID:    r.GetToolCallId(),
						ToolName:      r.GetToolName(),
						ArgumentsJSON: append([]byte(nil), r.GetArgumentsJson()...),
					})
					if hookErr != nil {
						out = PreToolUseOutput{
							Allow:      false,
							DenyReason: hookErr.Error(),
						}
					} else {
						out = hookOut
					}
				}
				err := sendFrame(&harnessv1.HostTurnFrame{
					Payload: &harnessv1.HostTurnFrame_PreToolUseDecision{
						PreToolUseDecision: &harnessv1.PreToolUseDecision{
							HookCallId:            r.GetHookCallId(),
							Allow:                 out.Allow,
							DenyReason:            out.DenyReason,
							ModifiedArgumentsJson: append([]byte(nil), out.ModifiedArgumentsJSON...),
							AdditionalContext:     out.AdditionalContext,
						},
					},
				})
				recordSendErr(err)
			}(req)

		case *harnessv1.PluginTurnFrame_PostToolUse:
			req := p.PostToolUse
			if req == nil {
				continue
			}
			wg.Add(1)
			go func(r *harnessv1.PostToolUseRequest) {
				defer wg.Done()
				if s.postToolUseHook != nil {
					_ = s.postToolUseHook(turnCtx, PostToolUseInput{
						HookCallID:   r.GetHookCallId(),
						ToolCallID:   r.GetToolCallId(),
						ToolName:     r.GetToolName(),
						Succeeded:    r.GetSucceeded(),
						ErrorMessage: r.GetErrorMessage(),
					})
				}
				err := sendFrame(&harnessv1.HostTurnFrame{
					Payload: &harnessv1.HostTurnFrame_PostToolUseAck{
						PostToolUseAck: &harnessv1.PostToolUseAck{
							HookCallId: r.GetHookCallId(),
						},
					},
				})
				recordSendErr(err)
			}(req)

		case *harnessv1.PluginTurnFrame_InvokeHostTool:
			req := p.InvokeHostTool
			if req == nil {
				continue
			}
			wg.Add(1)
			go func(r *harnessv1.InvokeHostToolRequest) {
				defer wg.Done()
				resp := s.invokeHostTool(turnCtx, r)
				err := sendFrame(&harnessv1.HostTurnFrame{
					Payload: &harnessv1.HostTurnFrame_InvokeHostToolResponse{
						InvokeHostToolResponse: resp,
					},
				})
				recordSendErr(err)
			}(req)

		case *harnessv1.PluginTurnFrame_TurnResult:
			wg.Wait()
			sendMu.Lock()
			_ = stream.CloseSend()
			sendMu.Unlock()

			sendErrMu.Lock()
			sErr := firstSendErr
			sendErrMu.Unlock()
			if sErr != nil {
				return TurnSummary{}, sErr
			}

			tr := p.TurnResult
			if tr == nil {
				return TurnSummary{}, errors.New("plugin returned empty turn_result")
			}
			if tr.GetError() != nil {
				return TurnSummary{}, fromProtoError(tr.GetError())
			}
			return TurnSummary{
				TurnID:             tr.GetTurnId(),
				AssistantMessageID: tr.GetAssistantMessageId(),
				FinalContent:       tr.GetFinalContent(),
			}, nil
		}
	}
}

func (s *GRPCSession) invokeHostTool(ctx context.Context, req *harnessv1.InvokeHostToolRequest) *harnessv1.InvokeHostToolResponse {
	handler := s.toolHandlers[req.GetToolName()]
	if handler == nil {
		return &harnessv1.InvokeHostToolResponse{
			ToolCallId:       req.GetToolCallId(),
			ResultType:       harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_FAILURE,
			TextResultForLlm: fmt.Sprintf("host tool %q is not registered", req.GetToolName()),
			HandlerError:     fmt.Sprintf("unknown host tool %q", req.GetToolName()),
		}
	}

	out, err := handler(ctx, ToolInvocation{
		ToolCallID:    req.GetToolCallId(),
		ToolName:      req.GetToolName(),
		ArgumentsJSON: append([]byte(nil), req.GetArgumentsJson()...),
	})
	if err != nil {
		errMsg := out.HandlerError
		if errMsg == "" {
			errMsg = err.Error()
		}
		text := out.TextResultForLLM
		if text == "" {
			text = errMsg
		}
		return &harnessv1.InvokeHostToolResponse{
			ToolCallId:       req.GetToolCallId(),
			ResultType:       harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_FAILURE,
			TextResultForLlm: text,
			HandlerError:     errMsg,
		}
	}

	resultType := toProtoHostToolResultType(out.ResultType)
	if resultType == harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_UNSPECIFIED {
		resultType = harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_SUCCESS
	}
	return &harnessv1.InvokeHostToolResponse{
		ToolCallId:       req.GetToolCallId(),
		ResultType:       resultType,
		TextResultForLlm: out.TextResultForLLM,
		HandlerError:     out.HandlerError,
	}
}

func (s *GRPCSession) setActiveTurnID(turnID string) {
	s.activeTurnMu.Lock()
	s.activeTurnID = turnID
	s.activeTurnMu.Unlock()
}

func (s *GRPCSession) currentTurnID() string {
	s.activeTurnMu.Lock()
	defer s.activeTurnMu.Unlock()
	return s.activeTurnID
}

// Abort invokes unary AbortTurn without acquiring the RunTurn mutex so StallWatchdog
// or immediate human pause can interrupt a blocked turn immediately.
func (s *GRPCSession) Abort(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := s.client.AbortTurn(ctx, &harnessv1.AbortTurnRequest{
		SessionId: s.id,
		TurnId:    s.currentTurnID(),
	})
	if err != nil {
		return err
	}
	if resp.GetError() != nil {
		return fromProtoError(resp.GetError())
	}
	return nil
}

// Resume invokes unary ResumeSession for the session.
func (s *GRPCSession) Resume(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := s.client.ResumeSession(ctx, &harnessv1.ResumeSessionRequest{
		SessionId: s.id,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != nil {
		return fromProtoError(resp.GetError())
	}
	return nil
}

// ReadMCPResource invokes unary ReadMCPResource for the session.
func (s *GRPCSession) ReadMCPResource(ctx context.Context, serverName, uri string) ([]MCPResourceContent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := s.client.ReadMCPResource(ctx, &harnessv1.ReadMCPResourceRequest{
		SessionId:  s.id,
		ServerName: serverName,
		Uri:        uri,
	})
	if err != nil {
		return nil, err
	}
	if resp.GetError() != nil {
		return nil, fromProtoError(resp.GetError())
	}
	contents := make([]MCPResourceContent, 0, len(resp.GetContents()))
	for _, item := range resp.GetContents() {
		contents = append(contents, MCPResourceContent{
			URI:      item.GetUri(),
			MIMEType: item.GetMimeType(),
			Text:     item.GetText(),
			Blob:     append([]byte(nil), item.GetBlob()...),
			Meta:     maps.Clone(item.GetMeta()),
		})
	}
	return contents, nil
}

// Close destroys the plugin session via CloseSession.
func (s *GRPCSession) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := s.client.CloseSession(ctx, &harnessv1.CloseSessionRequest{
		SessionId: s.id,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != nil {
		return fromProtoError(resp.GetError())
	}
	return nil
}

// MaterializeProviderConfig resolves provider secret references in memory via lookupEnv
// for delivery solely over AutoMTLS OpenSessionRequest.
func MaterializeProviderConfig(
	cfg provider.Config,
	wireModel, modelID string,
	lookupEnv func(string) (string, bool),
) (ProviderConfig, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	headers, err := stringMapFromCty(cfg.Headers, "provider headers")
	if err != nil {
		return ProviderConfig{}, err
	}
	out := ProviderConfig{
		Type:      string(cfg.Type),
		BaseURL:   cfg.Endpoint,
		Headers:   headers,
		ModelID:   modelID,
		WireModel: wireModel,
	}
	if cfg.WireAPI != nil {
		out.WireAPI = string(*cfg.WireAPI)
	}
	if cfg.Transport != nil {
		out.Transport = string(*cfg.Transport)
	}

	switch {
	case cfg.APIKey != nil:
		out.APIKey = *cfg.APIKey
	case cfg.APIKeyRef != nil:
		val, ok := lookupEnv(*cfg.APIKeyRef)
		if !ok || strings.TrimSpace(val) == "" {
			return ProviderConfig{}, fmt.Errorf("provider api_key_ref environment variable %q is not set or empty", *cfg.APIKeyRef)
		}
		out.APIKey = val
	case cfg.BearerToken != nil:
		out.BearerToken = *cfg.BearerToken
	case cfg.BearerTokenRef != nil:
		val, ok := lookupEnv(*cfg.BearerTokenRef)
		if !ok || strings.TrimSpace(val) == "" {
			return ProviderConfig{}, fmt.Errorf("provider bearer_token_ref environment variable %q is not set or empty", *cfg.BearerTokenRef)
		}
		out.BearerToken = val
	}
	return out, nil
}

// MaterializeMCPServerConfig resolves MCP HTTP bearer token and stdio env_refs in memory.
func MaterializeMCPServerConfig(cfg mcppkg.Config, lookupEnv func(string) (string, bool)) (MCPServerConfig, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	name := cfg.RuntimeName
	if strings.TrimSpace(name) == "" {
		name = cfg.Name
	}
	out := MCPServerConfig{
		Name:    name,
		Tools:   append([]string(nil), cfg.Tools...),
		Timeout: cfg.Timeout,
	}

	switch cfg.Transport {
	case mcppkg.TransportHTTP:
		if cfg.HTTP == nil {
			return MCPServerConfig{}, fmt.Errorf("mcp server %q http config is nil", name)
		}
		headers := maps.Clone(cfg.HTTP.Headers)
		if headers == nil {
			headers = make(map[string]string)
		}
		if cfg.HTTP.BearerToken != nil {
			headers["Authorization"] = "Bearer " + *cfg.HTTP.BearerToken
		} else if cfg.HTTP.BearerTokenRef != nil {
			token, ok := lookupEnv(*cfg.HTTP.BearerTokenRef)
			if !ok || strings.TrimSpace(token) == "" {
				return MCPServerConfig{}, fmt.Errorf("mcp server %q bearer_token_ref %q is not set or empty", name, *cfg.HTTP.BearerTokenRef)
			}
			headers["Authorization"] = "Bearer " + token
		}
		out.HTTP = &MCPHTTPConfig{
			URL:     cfg.HTTP.URL,
			Headers: headers,
		}

	case mcppkg.TransportStdio:
		if cfg.Stdio == nil {
			return MCPServerConfig{}, fmt.Errorf("mcp server %q stdio config is nil", name)
		}
		env := maps.Clone(cfg.Stdio.Env)
		if env == nil {
			env = make(map[string]string)
		}
		for dstKey, srcEnv := range cfg.Stdio.EnvRefs {
			val, ok := lookupEnv(srcEnv)
			if !ok || strings.TrimSpace(val) == "" {
				return MCPServerConfig{}, fmt.Errorf("mcp server %q env_refs[%q] (%q) is not set or empty", name, dstKey, srcEnv)
			}
			env[dstKey] = val
		}
		out.Stdio = &MCPStdioConfig{
			Command:          cfg.Stdio.Command,
			Args:             append([]string(nil), cfg.Stdio.Args...),
			Env:              env,
			WorkingDirectory: cfg.Stdio.WorkingDirectory,
		}

	default:
		return MCPServerConfig{}, fmt.Errorf("mcp server %q has unsupported transport %q", name, cfg.Transport)
	}

	return out, nil
}

// MaterializeHarnessEnv resolves harness.env_refs from host environment into an in-memory map.
func MaterializeHarnessEnv(envRefs map[string]string, lookupEnv func(string) (string, bool)) (map[string]string, error) {
	if len(envRefs) == 0 {
		return map[string]string{}, nil
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	out := make(map[string]string, len(envRefs))
	for dstKey, srcEnv := range envRefs {
		val, ok := lookupEnv(srcEnv)
		if !ok || strings.TrimSpace(val) == "" {
			return nil, fmt.Errorf("harness env_refs[%q] (%q) is not set or empty", dstKey, srcEnv)
		}
		out[dstKey] = val
	}
	return out, nil
}

// MarshalHarnessOptions serializes a planned cty.Value options object/map to JSON bytes.
func MarshalHarnessOptions(options cty.Value) ([]byte, error) {
	if options == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := options.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, errors.New("harness options must be wholly known")
	}
	return ctyjson.Marshal(unmarked, unmarked.Type())
}

func stringMapFromCty(val cty.Value, field string) (map[string]string, error) {
	if val == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, fmt.Errorf("%s must be wholly known", field)
	}
	converted, err := convert.Convert(unmarked, cty.Map(cty.String))
	if err != nil {
		return nil, fmt.Errorf("%s must be map(string): %w", field, err)
	}
	out := make(map[string]string, converted.LengthInt())
	for it := converted.ElementIterator(); it.Next(); {
		k, v := it.Element()
		if v.IsNull() {
			return nil, fmt.Errorf("%s[%q] must not be null", field, k.AsString())
		}
		out[k.AsString()] = v.AsString()
	}
	return out, nil
}

func toProtoOpenSessionRequest(cfg SessionConfig) *harnessv1.OpenSessionRequest {
	req := &harnessv1.OpenSessionRequest{
		SessionId:             cfg.SessionID,
		ResumeSessionId:       cfg.ResumeSessionID,
		Model:                 cfg.Model,
		Profile:               cfg.Profile,
		ReasoningEffort:       cfg.ReasoningEffort,
		SystemPrompt:          cfg.SystemPrompt,
		WorkingDirectory:      cfg.WorkingDirectory,
		SessionStoreDirectory: cfg.SessionStoreDirectory,
		PermissionMode:        cfg.PermissionMode,
		HarnessEnv:            maps.Clone(cfg.HarnessEnv),
		HarnessOptionsJson:    append([]byte(nil), cfg.HarnessOptionsJSON...),
		SkillDirectories:      append([]string(nil), cfg.SkillDirectories...),
		Skills:                append([]string(nil), cfg.Skills...),
		DisabledSkills:        append([]string(nil), cfg.DisabledSkills...),
	}

	if cfg.Provider != nil {
		p := &harnessv1.ProviderConfig{
			Type:      cfg.Provider.Type,
			BaseUrl:   cfg.Provider.BaseURL,
			WireApi:   cfg.Provider.WireAPI,
			Transport: cfg.Provider.Transport,
			Headers:   maps.Clone(cfg.Provider.Headers),
			ModelId:   cfg.Provider.ModelID,
			WireModel: cfg.Provider.WireModel,
		}
		switch {
		case cfg.Provider.APIKey != "":
			p.Auth = &harnessv1.ProviderConfig_ApiKey{ApiKey: cfg.Provider.APIKey}
		case cfg.Provider.BearerToken != "":
			p.Auth = &harnessv1.ProviderConfig_BearerToken{BearerToken: cfg.Provider.BearerToken}
		}
		req.Provider = p
	}

	for _, ht := range cfg.HostTools {
		req.HostTools = append(req.HostTools, &harnessv1.HostToolSpec{
			Name:                 ht.Name,
			Description:          ht.Description,
			ParametersJsonSchema: append([]byte(nil), ht.ParametersJSONSchema...),
		})
	}

	if cfg.AvailableTools.Configured {
		req.AvailableTools = &harnessv1.ToolFilter{
			Configured: true,
			Names:      append([]string(nil), cfg.AvailableTools.Names...),
		}
	}
	if cfg.ExcludedTools.Configured {
		req.ExcludedTools = &harnessv1.ToolFilter{
			Configured: true,
			Names:      append([]string(nil), cfg.ExcludedTools.Names...),
		}
	}

	for _, srv := range cfg.MCPServers {
		ps := &harnessv1.MCPServerConfig{
			Name:      srv.Name,
			Tools:     append([]string(nil), srv.Tools...),
			TimeoutMs: int32(srv.Timeout / time.Millisecond),
		}
		if srv.HTTP != nil {
			ps.Transport = &harnessv1.MCPServerConfig_Http{
				Http: &harnessv1.MCPHTTPTransport{
					Url:     srv.HTTP.URL,
					Headers: maps.Clone(srv.HTTP.Headers),
				},
			}
		} else if srv.Stdio != nil {
			ps.Transport = &harnessv1.MCPServerConfig_Stdio{
				Stdio: &harnessv1.MCPStdioTransport{
					Command:          srv.Stdio.Command,
					Args:             append([]string(nil), srv.Stdio.Args...),
					Env:              maps.Clone(srv.Stdio.Env),
					WorkingDirectory: srv.Stdio.WorkingDirectory,
				},
			}
		}
		req.McpServers = append(req.McpServers, ps)
	}

	return req
}

func toProtoHostToolResultType(t HostToolResultType) harnessv1.HostToolResultType {
	switch t {
	case HostToolResultSuccess:
		return harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_SUCCESS
	case HostToolResultFailure:
		return harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_FAILURE
	default:
		return harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_UNSPECIFIED
	}
}

func fromProtoEvent(ev *harnessv1.SessionEvent) Event {
	out := Event{
		EventID:           ev.GetEventId(),
		TimestampUnixNano: ev.GetTimestampUnixNano(),
		Kind:              fromProtoEventKind(ev.GetKind()),
		MessageID:         ev.GetMessageId(),
		ToolCallID:        ev.GetToolCallId(),
		ToolName:          ev.GetToolName(),
		SubagentName:      ev.GetSubagentName(),
		Text:              ev.GetText(),
		ArgumentsJSON:     append([]byte(nil), ev.GetArgumentsJson()...),
		ResultJSON:        append([]byte(nil), ev.GetResultJson()...),
		ErrorMessage:      ev.GetErrorMessage(),
		RawSDKEventJSON:   append([]byte(nil), ev.GetRawSdkEventJson()...),
	}
	if u := ev.GetUsage(); u != nil {
		out.Usage = &TokenUsage{
			Model:            u.GetModel(),
			APICallID:        u.GetApiCallId(),
			InputTokens:      u.GetInputTokens(),
			OutputTokens:     u.GetOutputTokens(),
			ReasoningTokens:  u.GetReasoningTokens(),
			CacheReadTokens:  u.GetCacheReadTokens(),
			CacheWriteTokens: u.GetCacheWriteTokens(),
		}
	}
	return out
}

func fromProtoEventKind(k harnessv1.SessionEventKind) EventKind {
	switch k {
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA:
		return EventAssistantMessageDelta
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE:
		return EventAssistantMessage
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA:
		return EventAssistantReasoningDelta
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_REASONING:
		return EventAssistantReasoning
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_USAGE:
		return EventAssistantUsage
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_TOOL_CALL_DELTA:
		return EventAssistantToolCallDelta
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_START:
		return EventToolExecutionStart
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_PROGRESS:
		return EventToolExecutionProgress
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_SHELL_OUTPUT:
		return EventToolShellOutput
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_COMPLETE:
		return EventToolExecutionComplete
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED:
		return EventSubagentStarted
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_COMPLETED:
		return EventSubagentCompleted
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_FAILED:
		return EventSubagentFailed
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_WARNING:
		return EventSessionWarning
	case harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_IDLE:
		return EventSessionIdle
	default:
		return EventUnspecified
	}
}

func fromProtoError(ed *harnessv1.ErrorDetail) *HarnessError {
	if ed == nil {
		return nil
	}
	return &HarnessError{
		Category:       fromProtoErrorCategory(ed.GetCategory()),
		HTTPStatusCode: ed.GetHttpStatusCode(),
		Message:        ed.GetMessage(),
	}
}

func fromProtoErrorCategory(c harnessv1.ErrorCategory) ErrorCategory {
	switch c {
	case harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT:
		return ErrorCategoryTransient
	case harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT:
		return ErrorCategoryRateLimit
	case harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHENTICATION:
		return ErrorCategoryAuthentication
	case harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHORIZATION:
		return ErrorCategoryAuthorization
	case harnessv1.ErrorCategory_ERROR_CATEGORY_QUOTA:
		return ErrorCategoryQuota
	case harnessv1.ErrorCategory_ERROR_CATEGORY_CONTEXT_LIMIT:
		return ErrorCategoryContextLimit
	case harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND:
		return ErrorCategorySessionNotFound
	case harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY:
		return ErrorCategoryUnsupportedCapability
	case harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL:
		return ErrorCategoryInternal
	default:
		return ErrorCategoryUnspecified
	}
}
