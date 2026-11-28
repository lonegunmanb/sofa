package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
)

const unboundedTurnDeadline = 365 * 24 * time.Hour

// ensureTurnDeadline attaches a far-future safety deadline when ctx carries no
// explicit deadline so github.com/github/copilot-sdk/go's Session.SendAndWait
// never falls back to its hardcoded 60-second default timeout. Turn-level stall
// detection and cancellation remain owned by the sofa host (StallWatchdog,
// AbortTurn, and gRPC stream teardown).
func ensureTurnDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, unboundedTurnDeadline)
}

type sdkClientFactory func(cliPath string, env []string) (sdkClient, error)

type sdkClient interface {
	Start(context.Context) error
	Stop() error
	CreateSession(context.Context, *sdk.SessionConfig) (sdkSession, error)
	ResumeSession(context.Context, string, *sdk.ResumeSessionConfig) (sdkSession, error)
}

type sdkSession interface {
	ID() string
	SendAndWait(context.Context, sdk.MessageOptions) (*sdk.SessionEvent, error)
	On(sdk.SessionEventHandler) func()
	Abort(context.Context) error
	Disconnect() error
	ReadMCPResource(ctx context.Context, serverName, uri string) ([]*harnessv1.MCPResourceContent, error)
}

func defaultClientFactory(cliPath string, env []string) (sdkClient, error) {
	client := sdk.NewClient(&sdk.ClientOptions{
		BaseDirectory: envValue(env, "COPILOT_HOME"),
		Connection: sdk.StdioConnection{
			Path: cliPath,
			Env:  env,
		},
	})
	return &officialClient{client: client}, nil
}

type officialClient struct {
	client *sdk.Client
}

func (c *officialClient) Start(ctx context.Context) error {
	return c.client.Start(ctx)
}

func (c *officialClient) Stop() error {
	return c.client.Stop()
}

func (c *officialClient) CreateSession(ctx context.Context, cfg *sdk.SessionConfig) (sdkSession, error) {
	sess, err := c.client.CreateSession(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &officialSession{sdk: sess}, nil
}

func (c *officialClient) ResumeSession(ctx context.Context, sessionID string, cfg *sdk.ResumeSessionConfig) (sdkSession, error) {
	sess, err := c.client.ResumeSession(ctx, sessionID, cfg)
	if err != nil {
		return nil, err
	}
	return &officialSession{sdk: sess}, nil
}

type officialSession struct {
	sdk *sdk.Session
}

func (s *officialSession) ID() string {
	if s == nil || s.sdk == nil {
		return ""
	}
	return s.sdk.SessionID
}

func (s *officialSession) On(handler sdk.SessionEventHandler) func() {
	if s == nil || s.sdk == nil {
		return func() {}
	}
	return s.sdk.On(handler)
}

func (s *officialSession) SendAndWait(ctx context.Context, options sdk.MessageOptions) (*sdk.SessionEvent, error) {
	waitCtx, cancelWait := ensureTurnDeadline(ctx)
	defer cancelWait()

	var (
		mu           sync.Mutex
		sessionError *sdk.SessionErrorData
	)
	unsubscribe := s.sdk.On(func(event sdk.SessionEvent) {
		data, ok := event.Data.(*sdk.SessionErrorData)
		if !ok || data == nil {
			return
		}
		cp := *data
		mu.Lock()
		sessionError = &cp
		mu.Unlock()
	})
	defer unsubscribe()

	event, err := s.sdk.SendAndWait(waitCtx, options)
	if err == nil {
		return event, nil
	}

	mu.Lock()
	data := sessionError
	mu.Unlock()
	if data == nil {
		return nil, err
	}
	return nil, &modelCallError{data: *data, err: err}
}

func (s *officialSession) Abort(ctx context.Context) error {
	return s.sdk.Abort(ctx)
}

func (s *officialSession) Disconnect() error {
	return s.sdk.Disconnect()
}

func (s *officialSession) ReadMCPResource(ctx context.Context, serverName, uri string) ([]*harnessv1.MCPResourceContent, error) {
	result, err := s.sdk.RPC.MCP.Resources().Read(ctx, &rpc.MCPResourcesReadRequest{
		ServerName: serverName,
		URI:        uri,
	})
	if err != nil {
		return nil, fmt.Errorf("read MCP resource %q from server %q: %w", uri, serverName, err)
	}
	contents := make([]*harnessv1.MCPResourceContent, 0, len(result.Contents))
	for _, item := range result.Contents {
		out := &harnessv1.MCPResourceContent{
			Uri: item.URI,
		}
		if item.MIMEType != nil {
			out.MimeType = *item.MIMEType
		}
		if item.Text != nil {
			out.Text = *item.Text
		}
		if item.Blob != nil && *item.Blob != "" {
			if decoded, decErr := base64.StdEncoding.DecodeString(*item.Blob); decErr == nil {
				out.Blob = decoded
			} else {
				out.Blob = []byte(*item.Blob)
			}
		}
		if len(item.Meta) > 0 {
			out.Meta = make(map[string]string, len(item.Meta))
			for k, v := range item.Meta {
				out.Meta[k] = fmt.Sprint(v)
			}
		}
		contents = append(contents, out)
	}
	return contents, nil
}

type modelCallError struct {
	data sdk.SessionErrorData
	err  error
}

func (e *modelCallError) Error() string {
	if e.data.Message != "" {
		return e.data.Message
	}
	return e.err.Error()
}

func (e *modelCallError) Unwrap() error {
	return e.err
}

func (e *modelCallError) HTTPStatusCode() int {
	if e.data.StatusCode == nil {
		return 0
	}
	return int(*e.data.StatusCode)
}
