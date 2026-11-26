package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/pkg/harnessplugin"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
)

const (
	customAgentName = "sofa_worker"
	defaultVersion  = "0.1.0"
)

// Option configures a Server instance for production or testing.
type Option func(*Server)

// WithClientFactory overrides how Copilot SDK clients are constructed.
func WithClientFactory(fn sdkClientFactory) Option {
	return func(s *Server) {
		if fn != nil {
			s.clientFactory = fn
		}
	}
}

// WithExecutablePath overrides os.Executable for CLI path discovery tests.
func WithExecutablePath(fn func() (string, error)) Option {
	return func(s *Server) {
		if fn != nil {
			s.execPath = fn
		}
	}
}

// WithLookPath overrides exec.LookPath for CLI path discovery tests.
func WithLookPath(fn func(string) (string, error)) Option {
	return func(s *Server) {
		if fn != nil {
			s.lookPath = fn
		}
	}
}

// WithVersion sets the reported harness version in GetCapabilities.
func WithVersion(version string) Option {
	return func(s *Server) {
		if strings.TrimSpace(version) != "" {
			s.version = strings.TrimSpace(version)
		}
	}
}

// Server implements the 7-RPC AgentHarness gRPC service backed by copilot-sdk/go.
type Server struct {
	harnessv1.UnimplementedAgentHarnessServer

	version       string
	clientFactory sdkClientFactory
	execPath      func() (string, error)
	lookPath      func(string) (string, error)

	mu       sync.RWMutex
	sessions map[string]*managedSession
}

// New constructs a Server with production defaults and optional overrides.
func New(opts ...Option) *Server {
	s := &Server{
		version:       defaultVersion,
		clientFactory: defaultClientFactory,
		execPath:      os.Executable,
		lookPath:      exec.LookPath,
		sessions:      make(map[string]*managedSession),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

type managedSession struct {
	id            string
	client        sdkClient
	sdkMu         sync.RWMutex
	sdk           sdkSession
	resumeCfg     *sdk.ResumeSessionConfig
	hostToolNames map[string]struct{}

	turnMu   sync.Mutex
	activeMu sync.RWMutex
	active   *activeTurn
}

func (m *managedSession) currentSDK() sdkSession {
	m.sdkMu.RLock()
	defer m.sdkMu.RUnlock()
	return m.sdk
}

func (m *managedSession) replaceSDK(next sdkSession) {
	m.sdkMu.Lock()
	m.sdk = next
	m.sdkMu.Unlock()
}

func (m *managedSession) currentTurn() *activeTurn {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	return m.active
}

func (m *managedSession) setActiveTurn(t *activeTurn) {
	m.activeMu.Lock()
	m.active = t
	m.activeMu.Unlock()
}

func (m *managedSession) clearActiveTurn(t *activeTurn) {
	m.activeMu.Lock()
	if m.active == t {
		m.active = nil
	}
	m.activeMu.Unlock()
}

type activeTurn struct {
	turnID string
	ctx    context.Context
	cancel context.CancelFunc
	stream harnessv1.AgentHarness_RunTurnServer

	sendMu sync.Mutex

	pendingMu          sync.Mutex
	pendingHooks       map[string]chan *harnessv1.HostTurnFrame
	pendingTools       map[string]chan *harnessv1.InvokeHostToolResponse
	hookToolCallQueues map[string][]string
	toolNames          map[string]string
}

func newActiveTurn(ctx context.Context, turnID string, stream harnessv1.AgentHarness_RunTurnServer) *activeTurn {
	turnCtx, cancel := context.WithCancel(ctx)
	return &activeTurn{
		turnID:             turnID,
		ctx:                turnCtx,
		cancel:             cancel,
		stream:             stream,
		pendingHooks:       make(map[string]chan *harnessv1.HostTurnFrame),
		pendingTools:       make(map[string]chan *harnessv1.InvokeHostToolResponse),
		hookToolCallQueues: make(map[string][]string),
		toolNames:          make(map[string]string),
	}
}

func (t *activeTurn) sendFrame(frame *harnessv1.PluginTurnFrame) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	return t.stream.Send(frame)
}

func (t *activeTurn) registerHook(hookCallID string) chan *harnessv1.HostTurnFrame {
	ch := make(chan *harnessv1.HostTurnFrame, 1)
	t.pendingMu.Lock()
	t.pendingHooks[hookCallID] = ch
	t.pendingMu.Unlock()
	return ch
}

func (t *activeTurn) unregisterHook(hookCallID string) {
	t.pendingMu.Lock()
	delete(t.pendingHooks, hookCallID)
	t.pendingMu.Unlock()
}

func (t *activeTurn) deliverHook(hookCallID string, frame *harnessv1.HostTurnFrame) {
	t.pendingMu.Lock()
	ch := t.pendingHooks[hookCallID]
	delete(t.pendingHooks, hookCallID)
	t.pendingMu.Unlock()
	if ch != nil {
		ch <- frame
	}
}

func (t *activeTurn) registerTool(toolCallID string) chan *harnessv1.InvokeHostToolResponse {
	ch := make(chan *harnessv1.InvokeHostToolResponse, 1)
	t.pendingMu.Lock()
	t.pendingTools[toolCallID] = ch
	t.pendingMu.Unlock()
	return ch
}

func (t *activeTurn) unregisterTool(toolCallID string) {
	t.pendingMu.Lock()
	delete(t.pendingTools, toolCallID)
	t.pendingMu.Unlock()
}

func (t *activeTurn) deliverTool(resp *harnessv1.InvokeHostToolResponse) {
	if resp == nil {
		return
	}
	t.pendingMu.Lock()
	ch := t.pendingTools[resp.GetToolCallId()]
	delete(t.pendingTools, resp.GetToolCallId())
	t.pendingMu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

func (t *activeTurn) recordToolName(toolCallID, toolName string) {
	if toolCallID == "" || toolName == "" {
		return
	}
	t.pendingMu.Lock()
	t.toolNames[toolCallID] = toolName
	t.pendingMu.Unlock()
}

func (t *activeTurn) lookupToolName(toolCallID string) string {
	if toolCallID == "" {
		return ""
	}
	t.pendingMu.Lock()
	name := t.toolNames[toolCallID]
	t.pendingMu.Unlock()
	return name
}

func (t *activeTurn) enqueueHookToolCall(toolName, toolCallID string) {
	if toolName == "" || toolCallID == "" {
		return
	}
	t.pendingMu.Lock()
	t.hookToolCallQueues[toolName] = append(t.hookToolCallQueues[toolName], toolCallID)
	t.pendingMu.Unlock()
}

func (t *activeTurn) dequeueHookToolCall(toolName string) string {
	if toolName == "" {
		return ""
	}
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	q := t.hookToolCallQueues[toolName]
	if len(q) == 0 {
		return ""
	}
	id := q[0]
	if len(q) == 1 {
		delete(t.hookToolCallQueues, toolName)
	} else {
		t.hookToolCallQueues[toolName] = q[1:]
	}
	return id
}

// GetCapabilities returns Copilot harness metadata and supported feature matrix.
func (s *Server) GetCapabilities(_ context.Context, _ *harnessv1.GetCapabilitiesRequest) (*harnessv1.GetCapabilitiesResponse, error) {
	return &harnessv1.GetCapabilitiesResponse{
		HarnessName:            "copilot",
		HarnessVersion:         s.version,
		ProtocolVersion:        1,
		SupportedProviderTypes: []string{"openai", "azure", "anthropic"},
		SupportsSessionResume:  true,
		SupportsMcpServers:     true,
		SupportsMcpResources:   true,
		SupportsSkills:         true,
	}, nil
}

// OpenSession creates a new Copilot SDK session or resumes an existing one by ID.
func (s *Server) OpenSession(ctx context.Context, req *harnessv1.OpenSessionRequest) (*harnessv1.OpenSessionResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return &harnessv1.OpenSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL,
				HttpStatusCode: 400,
				Message:        "open session request is required",
			},
		}, nil
	}

	profile := req.GetProfile()
	if profile == "" {
		profile = req.GetModel()
	}

	providerCfg, errDetail := buildProviderConfig(req.GetProvider(), profile, req.GetModel())
	if errDetail != nil {
		return &harnessv1.OpenSessionResponse{Error: errDetail}, nil
	}

	mcpServers, err := buildMCPServers(req.GetMcpServers())
	if err != nil {
		return &harnessv1.OpenSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL,
				HttpStatusCode: 400,
				Message:        err.Error(),
			},
		}, nil
	}

	cliPath, err := s.resolveCLIPath(req.GetHarnessOptionsJson())
	if err != nil {
		return &harnessv1.OpenSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category: harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL,
				Message:  err.Error(),
			},
		}, nil
	}

	childEnv := buildChildEnv(os.Environ(), req.GetHarnessEnv())
	client, err := s.clientFactory(cliPath, childEnv)
	if err != nil {
		return &harnessv1.OpenSessionResponse{Error: classifyError(err)}, nil
	}
	if err := client.Start(ctx); err != nil {
		_ = client.Stop()
		return &harnessv1.OpenSessionResponse{Error: classifyError(err)}, nil
	}

	hostToolNames := make(map[string]struct{}, len(req.GetHostTools()))
	for _, ht := range req.GetHostTools() {
		if ht != nil && ht.GetName() != "" {
			hostToolNames[ht.GetName()] = struct{}{}
		}
	}

	sessionID := req.GetSessionId()
	if req.GetResumeSessionId() != "" {
		sessionID = req.GetResumeSessionId()
	} else if sessionID == "" {
		sessionID = uuid.NewString()
	}

	managed := &managedSession{
		id:            sessionID,
		client:        client,
		hostToolNames: hostToolNames,
	}

	sdkTools, err := buildHostSDKTools(req.GetHostTools(), managed)
	if err != nil {
		_ = client.Stop()
		return &harnessv1.OpenSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL,
				HttpStatusCode: 400,
				Message:        err.Error(),
			},
		}, nil
	}

	sdkCfg := &sdk.SessionConfig{
		SessionID:              sessionID,
		Model:                  req.GetModel(),
		ReasoningEffort:        req.GetReasoningEffort(),
		SystemMessage:          &sdk.SystemMessageConfig{Mode: "append", Content: req.GetSystemPrompt()},
		WorkingDirectory:       req.GetWorkingDirectory(),
		ConfigDirectory:        req.GetSessionStoreDirectory(),
		Streaming:              sdk.Bool(true),
		Provider:               providerCfg,
		Tools:                  sdkTools,
		AvailableTools:         buildToolFilter(req.GetAvailableTools()),
		ExcludedTools:          buildToolFilter(req.GetExcludedTools()),
		OnPermissionRequest:    sdk.PermissionHandler.ApproveAll,
		EnableSkills:           sdk.Bool(true),
		SkillDirectories:       slices.Clone(req.GetSkillDirectories()),
		DisabledSkills:         slices.Clone(req.GetDisabledSkills()),
		EnableSessionStore:     sdk.Bool(false),
		SkipEmbeddingRetrieval: sdk.Bool(true),
		LargeOutput:            &sdk.LargeToolOutputConfig{Enabled: sdk.Bool(false)},
		Hooks:                  managed.buildSessionHooks(),
		MCPServers:             mcpServers,
	}
	if len(req.GetSkills()) > 0 {
		sdkCfg.CustomAgents = []sdk.CustomAgentConfig{{
			Name:            customAgentName,
			Prompt:          req.GetSystemPrompt(),
			Skills:          slices.Clone(req.GetSkills()),
			Model:           profile,
			ReasoningEffort: req.GetReasoningEffort(),
		}}
		sdkCfg.Agent = customAgentName
	}

	resumeCfg := buildResumeSessionConfig(sdkCfg)
	managed.resumeCfg = resumeCfg

	if req.GetResumeSessionId() != "" {
		sess, resumeErr := client.ResumeSession(ctx, req.GetResumeSessionId(), cloneResumeSessionConfig(resumeCfg))
		if resumeErr != nil {
			_ = client.Stop()
			return &harnessv1.OpenSessionResponse{Error: classifyError(resumeErr)}, nil
		}
		managed.sdk = sess
		s.storeSession(managed)
		return &harnessv1.OpenSessionResponse{
			SessionId: req.GetResumeSessionId(),
			Resumed:   true,
		}, nil
	}

	sess, createErr := client.CreateSession(ctx, sdkCfg)
	if createErr != nil {
		_ = client.Stop()
		return &harnessv1.OpenSessionResponse{Error: classifyError(createErr)}, nil
	}
	managed.sdk = sess
	s.storeSession(managed)
	return &harnessv1.OpenSessionResponse{
		SessionId: sessionID,
		Resumed:   false,
	}, nil
}

// RunTurn drives a single conversation turn over the bidirectional HostTurnFrame/PluginTurnFrame stream.
func (s *Server) RunTurn(stream harnessv1.AgentHarness_RunTurnServer) error {
	firstFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	startReq := firstFrame.GetStartTurn()
	if startReq == nil {
		return errors.New("expected StartTurnRequest as first RunTurn frame")
	}

	managed, ok := s.getSession(startReq.GetSessionId())
	if !ok {
		return stream.Send(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_TurnResult{
				TurnResult: &harnessv1.TurnResult{
					TurnId: startReq.GetTurnId(),
					Error: &harnessv1.ErrorDetail{
						Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
						HttpStatusCode: 404,
						Message:        fmt.Sprintf("session %q not found", startReq.GetSessionId()),
					},
				},
			},
		})
	}

	managed.turnMu.Lock()
	defer managed.turnMu.Unlock()

	sdkSess := managed.currentSDK()
	if sdkSess == nil {
		return stream.Send(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_TurnResult{
				TurnResult: &harnessv1.TurnResult{
					TurnId: startReq.GetTurnId(),
					Error: &harnessv1.ErrorDetail{
						Category: harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT,
						Message:  "session is unavailable",
					},
				},
			},
		})
	}

	turn := newActiveTurn(stream.Context(), startReq.GetTurnId(), stream)
	managed.setActiveTurn(turn)
	defer func() {
		turn.cancel()
		managed.clearActiveTurn(turn)
	}()

	// Background stream reader routes hook decisions/acks and host tool responses
	// concurrently while SendAndWait drives the turn.
	go func() {
		for {
			frame, recvErr := stream.Recv()
			if recvErr != nil {
				turn.cancel()
				return
			}
			switch p := frame.GetPayload().(type) {
			case *harnessv1.HostTurnFrame_PreToolUseDecision:
				if p.PreToolUseDecision != nil {
					turn.deliverHook(p.PreToolUseDecision.GetHookCallId(), frame)
				}
			case *harnessv1.HostTurnFrame_PostToolUseAck:
				if p.PostToolUseAck != nil {
					turn.deliverHook(p.PostToolUseAck.GetHookCallId(), frame)
				}
			case *harnessv1.HostTurnFrame_InvokeHostToolResponse:
				if p.InvokeHostToolResponse != nil {
					turn.deliverTool(p.InvokeHostToolResponse)
				}
			}
		}
	}()

	var (
		stateMu          sync.Mutex
		lastMessageID    string
		lastContent      string
		lastSessionError *sdk.SessionErrorData
	)

	unsubscribe := sdkSess.On(func(ev sdk.SessionEvent) {
		switch d := ev.Data.(type) {
		case *sdk.AssistantMessageData:
			if d != nil {
				stateMu.Lock()
				if d.MessageID != "" {
					lastMessageID = d.MessageID
				}
				lastContent = d.Content
				stateMu.Unlock()
			}
		case *sdk.SessionErrorData:
			if d != nil {
				cp := *d
				stateMu.Lock()
				lastSessionError = &cp
				stateMu.Unlock()
			}
		}

		protoEv := toProtoSessionEvent(ev, turn)
		if protoEv == nil {
			return
		}
		if sendErr := turn.sendFrame(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_Event{Event: protoEv},
		}); sendErr != nil {
			turn.cancel()
		}
	})

	finalEv, sendErr := sdkSess.SendAndWait(turn.ctx, sdk.MessageOptions{Prompt: startReq.GetPrompt()})
	unsubscribe()

	if sendErr != nil {
		var errDetail *harnessv1.ErrorDetail
		var modelErr *modelCallError
		if errors.As(sendErr, &modelErr) {
			errDetail = classifyError(sendErr)
		} else {
			stateMu.Lock()
			sessErrData := lastSessionError
			stateMu.Unlock()
			if sessErrData != nil {
				errDetail = classifySessionErrorData(sessErrData)
			} else {
				errDetail = classifyError(sendErr)
			}
		}
		return turn.sendFrame(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_TurnResult{
				TurnResult: &harnessv1.TurnResult{
					TurnId: startReq.GetTurnId(),
					Error:  errDetail,
				},
			},
		})
	}

	stateMu.Lock()
	msgID := lastMessageID
	content := lastContent
	stateMu.Unlock()

	if finalEv != nil {
		if d, ok := finalEv.Data.(*sdk.AssistantMessageData); ok && d != nil {
			if d.MessageID != "" {
				msgID = d.MessageID
			}
			content = d.Content
		}
	}

	return turn.sendFrame(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_TurnResult{
			TurnResult: &harnessv1.TurnResult{
				TurnId:             startReq.GetTurnId(),
				AssistantMessageId: msgID,
				FinalContent:       content,
			},
		},
	})
}

// AbortTurn immediately aborts the active turn without acquiring RunTurn's turnMu.
func (s *Server) AbortTurn(ctx context.Context, req *harnessv1.AbortTurnRequest) (*harnessv1.AbortTurnResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	managed, ok := s.getSession(req.GetSessionId())
	if !ok {
		return &harnessv1.AbortTurnResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
				HttpStatusCode: 404,
				Message:        fmt.Sprintf("session %q not found", req.GetSessionId()),
			},
		}, nil
	}

	turn := managed.currentTurn()
	if req.GetTurnId() != "" {
		if turn == nil || turn.turnID != req.GetTurnId() {
			return &harnessv1.AbortTurnResponse{}, nil
		}
	}
	if turn != nil {
		turn.cancel()
	}

	sdkSess := managed.currentSDK()
	if sdkSess == nil {
		return &harnessv1.AbortTurnResponse{}, nil
	}
	if err := sdkSess.Abort(ctx); err != nil {
		return &harnessv1.AbortTurnResponse{Error: classifyError(err)}, nil
	}
	return &harnessv1.AbortTurnResponse{}, nil
}

// ResumeSession disconnects the current SDK session handle and reconnects the same session ID.
func (s *Server) ResumeSession(ctx context.Context, req *harnessv1.ResumeSessionRequest) (*harnessv1.ResumeSessionResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	managed, ok := s.getSession(req.GetSessionId())
	if !ok {
		return &harnessv1.ResumeSessionResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
				HttpStatusCode: 404,
				Message:        fmt.Sprintf("session %q not found", req.GetSessionId()),
			},
		}, nil
	}

	if cur := managed.currentSDK(); cur != nil {
		if err := cur.Disconnect(); err != nil {
			return &harnessv1.ResumeSessionResponse{Error: classifyError(err)}, nil
		}
		managed.replaceSDK(nil)
	}

	resumed, err := managed.client.ResumeSession(ctx, managed.id, cloneResumeSessionConfig(managed.resumeCfg))
	if err != nil {
		return &harnessv1.ResumeSessionResponse{Error: classifyError(err)}, nil
	}
	managed.replaceSDK(resumed)
	return &harnessv1.ResumeSessionResponse{}, nil
}

// ReadMCPResource reads a declared resource from an attached MCP server.
func (s *Server) ReadMCPResource(ctx context.Context, req *harnessv1.ReadMCPResourceRequest) (*harnessv1.ReadMCPResourceResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	managed, ok := s.getSession(req.GetSessionId())
	if !ok {
		return &harnessv1.ReadMCPResourceResponse{
			Error: &harnessv1.ErrorDetail{
				Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
				HttpStatusCode: 404,
				Message:        fmt.Sprintf("session %q not found", req.GetSessionId()),
			},
		}, nil
	}
	sdkSess := managed.currentSDK()
	if sdkSess == nil {
		return &harnessv1.ReadMCPResourceResponse{
			Error: &harnessv1.ErrorDetail{
				Category: harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT,
				Message:  "session is unavailable",
			},
		}, nil
	}

	contents, err := sdkSess.ReadMCPResource(ctx, req.GetServerName(), req.GetUri())
	if err != nil {
		return &harnessv1.ReadMCPResourceResponse{Error: classifyError(err)}, nil
	}
	return &harnessv1.ReadMCPResourceResponse{Contents: contents}, nil
}

// CloseSession disconnects the Copilot SDK session and stops the underlying client process.
func (s *Server) CloseSession(_ context.Context, req *harnessv1.CloseSessionRequest) (*harnessv1.CloseSessionResponse, error) {
	managed, ok := s.deleteSession(req.GetSessionId())
	if !ok {
		return &harnessv1.CloseSessionResponse{}, nil
	}

	if err := cleanupManagedSession(managed); err != nil {
		return &harnessv1.CloseSessionResponse{Error: classifyError(err)}, nil
	}
	return &harnessv1.CloseSessionResponse{}, nil
}

func cleanupManagedSession(managed *managedSession) error {
	if managed == nil {
		return nil
	}
	if turn := managed.currentTurn(); turn != nil {
		turn.cancel()
	}

	var errs []error
	if cur := managed.currentSDK(); cur != nil {
		if err := cur.Disconnect(); err != nil {
			errs = append(errs, err)
		}
		managed.replaceSDK(nil)
	}
	if managed.client != nil {
		if err := managed.client.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) storeSession(m *managedSession) {
	s.mu.Lock()
	prev := s.sessions[m.id]
	s.sessions[m.id] = m
	s.mu.Unlock()
	if prev != nil && prev != m {
		_ = cleanupManagedSession(prev)
	}
}

func (s *Server) getSession(id string) (*managedSession, bool) {
	s.mu.RLock()
	m, ok := s.sessions[id]
	s.mu.RUnlock()
	return m, ok
}

func (s *Server) deleteSession(id string) (*managedSession, bool) {
	s.mu.Lock()
	m, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	return m, ok
}

func (s *Server) resolveCLIPath(optionsJSON []byte) (string, error) {
	if s.execPath != nil {
		if exePath, err := s.execPath(); err == nil && strings.TrimSpace(exePath) != "" {
			bundled := filepath.Join(filepath.Dir(exePath), "bin", "copilot")
			if info, statErr := os.Stat(bundled); statErr == nil && !info.IsDir() {
				return bundled, nil
			}
		}
	}

	if len(optionsJSON) > 0 {
		var opts map[string]any
		if err := json.Unmarshal(optionsJSON, &opts); err == nil {
			if raw, ok := opts["cli_path"].(string); ok && strings.TrimSpace(raw) != "" {
				return strings.TrimSpace(raw), nil
			}
		}
	}

	if s.lookPath != nil {
		if found, err := s.lookPath("copilot"); err == nil && strings.TrimSpace(found) != "" {
			return found, nil
		}
	}

	return "", errors.New("copilot CLI binary not found at <execDir>/bin/copilot, options.cli_path, or PATH")
}

func buildChildEnv(baseEnv []string, harnessEnv map[string]string) []string {
	merged := make(map[string]string, len(baseEnv)+len(harnessEnv))
	order := make([]string, 0, len(baseEnv)+len(harnessEnv))

	for _, entry := range baseEnv {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" {
			continue
		}
		if k == harnessplugin.Handshake.MagicCookieKey || strings.HasPrefix(k, "PLUGIN_") {
			continue
		}
		if _, exists := merged[k]; !exists {
			order = append(order, k)
		}
		merged[k] = v
	}

	if len(harnessEnv) > 0 {
		keys := make([]string, 0, len(harnessEnv))
		for k := range harnessEnv {
			if k != "" {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			if _, exists := merged[k]; !exists {
				order = append(order, k)
			}
			merged[k] = harnessEnv[k]
		}
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func buildProviderConfig(p *harnessv1.ProviderConfig, profile, model string) (*sdk.ProviderConfig, *harnessv1.ErrorDetail) {
	if p == nil {
		return nil, nil
	}
	switch p.GetType() {
	case "openai", "azure", "anthropic":
	default:
		return nil, &harnessv1.ErrorDetail{
			Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY,
			HttpStatusCode: 400,
			Message:        fmt.Sprintf("unsupported provider type %q", p.GetType()),
		}
	}

	modelID := p.GetModelId()
	if modelID == "" {
		modelID = profile
	}
	wireModel := p.GetWireModel()
	if wireModel == "" {
		wireModel = model
	}

	out := &sdk.ProviderConfig{
		Type:        p.GetType(),
		BaseURL:     p.GetBaseUrl(),
		WireAPI:     p.GetWireApi(),
		Transport:   p.GetTransport(),
		Headers:     maps.Clone(p.GetHeaders()),
		ModelID:     modelID,
		WireModel:   wireModel,
		APIKey:      p.GetApiKey(),
		BearerToken: p.GetBearerToken(),
	}
	return out, nil
}

func buildToolFilter(f *harnessv1.ToolFilter) []string {
	if f == nil || !f.GetConfigured() {
		return nil
	}
	if len(f.GetNames()) == 0 {
		return []string{}
	}
	return slices.Clone(f.GetNames())
}

func buildMCPServers(servers []*harnessv1.MCPServerConfig) (map[string]sdk.MCPServerConfig, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	result := make(map[string]sdk.MCPServerConfig, len(servers))
	for _, srv := range servers {
		if srv == nil || srv.GetName() == "" {
			return nil, errors.New("mcp server name is required")
		}
		if _, exists := result[srv.GetName()]; exists {
			return nil, fmt.Errorf("duplicate mcp server %q", srv.GetName())
		}
		tools := slices.Clone(srv.GetTools())
		if tools == nil {
			tools = []string{}
		}
		timeout := int(srv.GetTimeoutMs())

		switch t := srv.GetTransport().(type) {
		case *harnessv1.MCPServerConfig_Http:
			if t.Http == nil {
				return nil, fmt.Errorf("mcp server %q http transport is nil", srv.GetName())
			}
			result[srv.GetName()] = sdk.MCPHTTPServerConfig{
				URL:     t.Http.GetUrl(),
				Headers: maps.Clone(t.Http.GetHeaders()),
				Tools:   tools,
				Timeout: timeout,
			}
		case *harnessv1.MCPServerConfig_Stdio:
			if t.Stdio == nil {
				return nil, fmt.Errorf("mcp server %q stdio transport is nil", srv.GetName())
			}
			result[srv.GetName()] = sdk.MCPStdioServerConfig{
				Command:          t.Stdio.GetCommand(),
				Args:             slices.Clone(t.Stdio.GetArgs()),
				Env:              maps.Clone(t.Stdio.GetEnv()),
				WorkingDirectory: t.Stdio.GetWorkingDirectory(),
				Tools:            tools,
				Timeout:          timeout,
			}
		default:
			return nil, fmt.Errorf("mcp server %q has unsupported transport", srv.GetName())
		}
	}
	return result, nil
}

func buildHostSDKTools(specs []*harnessv1.HostToolSpec, managed *managedSession) ([]sdk.Tool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]sdk.Tool, 0, len(specs))
	for _, spec := range specs {
		if spec == nil || spec.GetName() == "" {
			return nil, errors.New("host tool name is required")
		}
		var params map[string]any
		if len(spec.GetParametersJsonSchema()) > 0 {
			if err := json.Unmarshal(spec.GetParametersJsonSchema(), &params); err != nil {
				return nil, fmt.Errorf("unmarshal host tool %q parameters schema: %w", spec.GetName(), err)
			}
		}
		toolName := spec.GetName()
		tools = append(tools, sdk.Tool{
			Name:        toolName,
			Description: spec.GetDescription(),
			Parameters:  params,
			Handler:     managed.makeHostToolHandler(toolName),
		})
	}
	return tools, nil
}

func (m *managedSession) makeHostToolHandler(toolName string) sdk.ToolHandler {
	return func(invocation sdk.ToolInvocation) (sdk.ToolResult, error) {
		turn := m.currentTurn()
		if turn == nil {
			return sdk.ToolResult{}, fmt.Errorf("no active RunTurn for host tool %q", toolName)
		}

		toolCallID := invocation.ToolCallID
		if toolCallID == "" {
			toolCallID = "host-" + uuid.NewString()
		}

		var argsBytes []byte
		if invocation.Arguments != nil {
			encoded, err := json.Marshal(invocation.Arguments)
			if err != nil {
				return sdk.ToolResult{}, fmt.Errorf("marshal arguments for host tool %q: %w", toolName, err)
			}
			argsBytes = encoded
		}

		respCh := turn.registerTool(toolCallID)
		defer turn.unregisterTool(toolCallID)

		if err := turn.sendFrame(&harnessv1.PluginTurnFrame{
			Payload: &harnessv1.PluginTurnFrame_InvokeHostTool{
				InvokeHostTool: &harnessv1.InvokeHostToolRequest{
					ToolCallId:    toolCallID,
					ToolName:      toolName,
					ArgumentsJson: argsBytes,
				},
			},
		}); err != nil {
			return sdk.ToolResult{}, err
		}

		ctx := invocation.TraceContext
		if ctx == nil {
			ctx = turn.ctx
		}

		select {
		case resp := <-respCh:
			if resp == nil {
				return sdk.ToolResult{}, errors.New("received empty host tool response")
			}
			resultType := "success"
			if resp.GetResultType() == harnessv1.HostToolResultType_HOST_TOOL_RESULT_TYPE_FAILURE {
				resultType = "failure"
			}
			out := sdk.ToolResult{
				TextResultForLLM: resp.GetTextResultForLlm(),
				ResultType:       resultType,
			}
			if resp.GetHandlerError() != "" {
				out.Error = resp.GetHandlerError()
			}
			return out, nil
		case <-turn.ctx.Done():
			return sdk.ToolResult{}, turn.ctx.Err()
		case <-ctx.Done():
			return sdk.ToolResult{}, ctx.Err()
		}
	}
}

func (m *managedSession) buildSessionHooks() *sdk.SessionHooks {
	return &sdk.SessionHooks{
		OnPreToolUse: func(input sdk.PreToolUseHookInput, _ sdk.HookInvocation) (*sdk.PreToolUseHookOutput, error) {
			if _, isHost := m.hostToolNames[input.ToolName]; isHost {
				return &sdk.PreToolUseHookOutput{PermissionDecision: "allow"}, nil
			}
			turn := m.currentTurn()
			if turn == nil {
				return &sdk.PreToolUseHookOutput{PermissionDecision: "allow"}, nil
			}

			hookCallID := "pre-" + uuid.NewString()
			toolCallID := "hook-tc-" + uuid.NewString()
			var argsBytes []byte
			if input.ToolArgs != nil {
				encoded, err := json.Marshal(input.ToolArgs)
				if err != nil {
					return nil, fmt.Errorf("marshal preToolUse args for %q: %w", input.ToolName, err)
				}
				argsBytes = encoded
			}

			respCh := turn.registerHook(hookCallID)
			defer turn.unregisterHook(hookCallID)

			if err := turn.sendFrame(&harnessv1.PluginTurnFrame{
				Payload: &harnessv1.PluginTurnFrame_PreToolUse{
					PreToolUse: &harnessv1.PreToolUseRequest{
						HookCallId:    hookCallID,
						ToolCallId:    toolCallID,
						ToolName:      input.ToolName,
						ArgumentsJson: argsBytes,
					},
				},
			}); err != nil {
				return nil, err
			}

			select {
			case frame := <-respCh:
				dec := frame.GetPreToolUseDecision()
				if dec == nil {
					return nil, errors.New("expected PreToolUseDecision frame")
				}
				if !dec.GetAllow() {
					return &sdk.PreToolUseHookOutput{
						PermissionDecision:       "deny",
						PermissionDecisionReason: dec.GetDenyReason(),
					}, nil
				}
				turn.enqueueHookToolCall(input.ToolName, toolCallID)
				out := &sdk.PreToolUseHookOutput{
					PermissionDecision: "allow",
					AdditionalContext:  dec.GetAdditionalContext(),
				}
				if len(dec.GetModifiedArgumentsJson()) > 0 {
					var mod any
					if err := json.Unmarshal(dec.GetModifiedArgumentsJson(), &mod); err != nil {
						return nil, fmt.Errorf("unmarshal modified_arguments_json: %w", err)
					}
					out.ModifiedArgs = mod
				}
				return out, nil
			case <-turn.ctx.Done():
				return nil, turn.ctx.Err()
			}
		},

		OnPostToolUse: func(input sdk.PostToolUseHookInput, _ sdk.HookInvocation) (*sdk.PostToolUseHookOutput, error) {
			if _, isHost := m.hostToolNames[input.ToolName]; isHost {
				return &sdk.PostToolUseHookOutput{}, nil
			}
			if err := m.sendPostToolUse(input.ToolName, true, ""); err != nil {
				return nil, err
			}
			return &sdk.PostToolUseHookOutput{}, nil
		},

		OnPostToolUseFailure: func(input sdk.PostToolUseFailureHookInput, _ sdk.HookInvocation) (*sdk.PostToolUseFailureHookOutput, error) {
			if _, isHost := m.hostToolNames[input.ToolName]; isHost {
				return &sdk.PostToolUseFailureHookOutput{}, nil
			}
			if err := m.sendPostToolUse(input.ToolName, false, input.Error); err != nil {
				return nil, err
			}
			return &sdk.PostToolUseFailureHookOutput{}, nil
		},
	}
}

func (m *managedSession) sendPostToolUse(toolName string, succeeded bool, errorMsg string) error {
	turn := m.currentTurn()
	if turn == nil {
		return nil
	}
	hookCallID := "post-" + uuid.NewString()
	toolCallID := turn.dequeueHookToolCall(toolName)
	respCh := turn.registerHook(hookCallID)
	defer turn.unregisterHook(hookCallID)

	if err := turn.sendFrame(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_PostToolUse{
			PostToolUse: &harnessv1.PostToolUseRequest{
				HookCallId:   hookCallID,
				ToolCallId:   toolCallID,
				ToolName:     toolName,
				Succeeded:    succeeded,
				ErrorMessage: errorMsg,
			},
		},
	}); err != nil {
		return err
	}

	select {
	case frame := <-respCh:
		if frame.GetPostToolUseAck() == nil {
			return errors.New("expected PostToolUseAck frame")
		}
		return nil
	case <-turn.ctx.Done():
		return turn.ctx.Err()
	}
}

func buildResumeSessionConfig(cfg *sdk.SessionConfig) *sdk.ResumeSessionConfig {
	return &sdk.ResumeSessionConfig{
		Model:                  cfg.Model,
		Tools:                  slices.Clone(cfg.Tools),
		SystemMessage:          cfg.SystemMessage,
		AvailableTools:         cloneToolFilterSlice(cfg.AvailableTools),
		ExcludedTools:          cloneToolFilterSlice(cfg.ExcludedTools),
		Provider:               cfg.Provider,
		ReasoningEffort:        cfg.ReasoningEffort,
		OnPermissionRequest:    cfg.OnPermissionRequest,
		Hooks:                  cfg.Hooks,
		MCPServers:             cloneMCPServers(cfg.MCPServers),
		WorkingDirectory:       cfg.WorkingDirectory,
		ConfigDirectory:        cfg.ConfigDirectory,
		EnableSkills:           cfg.EnableSkills,
		Streaming:              cfg.Streaming,
		CustomAgents:           slices.Clone(cfg.CustomAgents),
		Agent:                  cfg.Agent,
		SkillDirectories:       slices.Clone(cfg.SkillDirectories),
		DisabledSkills:         slices.Clone(cfg.DisabledSkills),
		ContinuePendingWork:    sdk.Bool(false),
		EnableSessionStore:     sdk.Bool(false),
		SkipEmbeddingRetrieval: sdk.Bool(true),
		LargeOutput:            &sdk.LargeToolOutputConfig{Enabled: sdk.Bool(false)},
	}
}

func cloneResumeSessionConfig(cfg *sdk.ResumeSessionConfig) *sdk.ResumeSessionConfig {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.Tools = slices.Clone(cfg.Tools)
	clone.AvailableTools = cloneToolFilterSlice(cfg.AvailableTools)
	clone.ExcludedTools = cloneToolFilterSlice(cfg.ExcludedTools)
	clone.CustomAgents = slices.Clone(cfg.CustomAgents)
	clone.SkillDirectories = slices.Clone(cfg.SkillDirectories)
	clone.DisabledSkills = slices.Clone(cfg.DisabledSkills)
	clone.MCPServers = cloneMCPServers(cfg.MCPServers)
	return &clone
}

func cloneToolFilterSlice(in []string) []string {
	if in == nil {
		return nil
	}
	if len(in) == 0 {
		return []string{}
	}
	return slices.Clone(in)
}

func cloneMCPServers(src map[string]sdk.MCPServerConfig) map[string]sdk.MCPServerConfig {
	if src == nil {
		return nil
	}
	out := make(map[string]sdk.MCPServerConfig, len(src))
	for name, srv := range src {
		switch cfg := srv.(type) {
		case sdk.MCPHTTPServerConfig:
			cfg.Tools = slices.Clone(cfg.Tools)
			cfg.Headers = maps.Clone(cfg.Headers)
			out[name] = cfg
		case sdk.MCPStdioServerConfig:
			cfg.Tools = slices.Clone(cfg.Tools)
			cfg.Args = slices.Clone(cfg.Args)
			cfg.Env = maps.Clone(cfg.Env)
			out[name] = cfg
		default:
			out[name] = srv
		}
	}
	return out
}

func toProtoSessionEvent(ev sdk.SessionEvent, turn *activeTurn) *harnessv1.SessionEvent {
	if ev.Data == nil {
		return nil
	}

	ts := ev.Timestamp.UnixNano()
	if ev.Timestamp.IsZero() {
		ts = time.Now().UnixNano()
	}

	out := &harnessv1.SessionEvent{
		EventId:           ev.ID,
		TimestampUnixNano: ts,
	}
	if raw, err := json.Marshal(ev); err == nil {
		out.RawSdkEventJson = raw
	}

	switch d := ev.Data.(type) {
	case *sdk.AssistantMessageDeltaData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA
		out.MessageId = d.MessageID
		out.Text = d.DeltaContent
	case *sdk.AssistantMessageData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_MESSAGE
		out.MessageId = d.MessageID
		out.Text = d.Content
	case *sdk.AssistantReasoningDeltaData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA
		out.MessageId = d.ReasoningID
		out.Text = d.DeltaContent
	case *sdk.AssistantReasoningData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_REASONING
		out.MessageId = d.ReasoningID
		out.Text = d.Content
	case *sdk.AssistantUsageData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_USAGE
		out.Usage = &harnessv1.TokenUsage{
			Model:            d.Model,
			InputTokens:      int64Val(d.InputTokens),
			OutputTokens:     int64Val(d.OutputTokens),
			ReasoningTokens:  int64Val(d.ReasoningTokens),
			CacheReadTokens:  int64Val(d.CacheReadTokens),
			CacheWriteTokens: int64Val(d.CacheWriteTokens),
		}
		if d.APICallID != nil {
			out.Usage.ApiCallId = *d.APICallID
		}
	case *sdk.AssistantToolCallDeltaData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_ASSISTANT_TOOL_CALL_DELTA
		out.ToolCallId = d.ToolCallID
		out.Text = d.InputDelta
		if d.ToolName != nil {
			out.ToolName = *d.ToolName
			turn.recordToolName(d.ToolCallID, *d.ToolName)
		}
	case *sdk.ToolExecutionStartData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_START
		out.ToolCallId = d.ToolCallID
		out.ToolName = d.ToolName
		turn.recordToolName(d.ToolCallID, d.ToolName)
		if d.Arguments != nil {
			out.ArgumentsJson, _ = json.Marshal(d.Arguments)
		}
	case *sdk.ToolExecutionProgressData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_PROGRESS
		out.ToolCallId = d.ToolCallID
		out.ToolName = turn.lookupToolName(d.ToolCallID)
		out.Text = d.ProgressMessage
	case *sdk.ToolShellOutputData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_SHELL_OUTPUT
		out.ToolCallId = d.ToolCallID
		out.ToolName = turn.lookupToolName(d.ToolCallID)
		out.Text = d.Text
	case *sdk.ToolExecutionPartialResultData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_SHELL_OUTPUT
		out.ToolCallId = d.ToolCallID
		out.ToolName = turn.lookupToolName(d.ToolCallID)
		out.Text = d.PartialOutput
	case *sdk.ToolExecutionCompleteData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_TOOL_EXECUTION_COMPLETE
		out.ToolCallId = d.ToolCallID
		out.ToolName = turn.lookupToolName(d.ToolCallID)
		out.ResultJson, _ = json.Marshal(d)
		if d.Error != nil {
			out.ErrorMessage = d.Error.Message
		}
	case *sdk.SubagentStartedData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED
		out.ToolCallId = d.ToolCallID
		out.SubagentName = firstNonEmpty(d.AgentName, d.AgentDisplayName)
	case *sdk.SubagentCompletedData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_COMPLETED
		out.ToolCallId = d.ToolCallID
		out.SubagentName = firstNonEmpty(d.AgentName, d.AgentDisplayName)
	case *sdk.SubagentFailedData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_FAILED
		out.ToolCallId = d.ToolCallID
		out.SubagentName = firstNonEmpty(d.AgentName, d.AgentDisplayName)
		out.ErrorMessage = d.Error
	case *sdk.SessionWarningData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_WARNING
		out.Text = d.Message
	case *sdk.SessionIdleData:
		out.Kind = harnessv1.SessionEventKind_SESSION_EVENT_KIND_SESSION_IDLE
	default:
		return nil
	}
	return out
}

func int64Val(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
