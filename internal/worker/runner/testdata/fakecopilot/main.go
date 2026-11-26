// Command fakecopilot is a lightweight stdlib-only Copilot CLI JSON-RPC 2.0 stdio stub
// used by Tier 2 Podman integration tests to drive the real github.com/github/copilot-sdk/go
// client inside sofa-harness-copilot without external network calls or tokens.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type server struct {
	writeMu sync.Mutex
	out     io.Writer

	nextReqID atomic.Int64

	pendingMu      sync.Mutex
	pendingReplies map[string]chan rpcMessage
	pendingTools   map[string]chan string

	sessMu   sync.Mutex
	sessions map[string]string // sessionID -> model
}

func newServer(out io.Writer) *server {
	return &server{
		out:            out,
		pendingReplies: make(map[string]chan rpcMessage),
		pendingTools:   make(map[string]chan string),
		sessions:       make(map[string]string),
	}
}

func (s *server) sendRaw(payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = s.out.Write(b)
	return err
}

func (s *server) callClient(method string, params any) (rpcMessage, error) {
	id := s.nextReqID.Add(1)
	idKey := strconv.FormatInt(id, 10)
	ch := make(chan rpcMessage, 1)
	s.pendingMu.Lock()
	s.pendingReplies[idKey] = ch
	s.pendingMu.Unlock()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return rpcMessage{}, err
	}
	rawID := json.RawMessage(idKey)
	if err := s.sendRaw(rpcMessage{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		return rpcMessage{}, err
	}
	resp := <-ch
	return resp, nil
}

func (s *server) notify(method string, params any) error {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.sendRaw(rpcMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	})
}

func (s *server) reply(id json.RawMessage, result any) error {
	rawResult, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.sendRaw(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  rawResult,
	})
}

func (s *server) invokeExternalTool(sessionID, reqID, callID, toolName string, args map[string]any) error {
	ch := make(chan string, 1)
	s.pendingMu.Lock()
	s.pendingTools[reqID] = ch
	s.pendingMu.Unlock()

	if err := s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-" + reqID,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"type":      "external_tool.requested",
			"data": map[string]any{
				"requestId":  reqID,
				"sessionId":  sessionID,
				"toolCallId": callID,
				"toolName":   toolName,
				"arguments":  args,
			},
		},
	}); err != nil {
		return err
	}

	<-ch
	return nil
}

func (s *server) invokeHook(sessionID, hookType string, input map[string]any) (map[string]any, error) {
	resp, err := s.callClient("hooks.invoke", map[string]any{
		"sessionId": sessionID,
		"hookType":  hookType,
		"input":     input,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("hook %s failed: %s", hookType, resp.Error.Message)
	}
	var out struct {
		Output map[string]any `json:"output"`
	}
	if len(resp.Result) > 0 {
		_ = json.Unmarshal(resp.Result, &out)
	}
	return out.Output, nil
}

func (s *server) runWorkTurn(sessionID string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-delta-1",
			"timestamp": now,
			"type":      "assistant.message_delta",
			"data": map[string]any{
				"messageId":    "msg-work",
				"deltaContent": "implementing feature",
			},
		},
	})
	_ = s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-usage-1",
			"timestamp": now,
			"type":      "assistant.usage",
			"data": map[string]any{
				"model":        "work-model",
				"inputTokens":  42,
				"outputTokens": 18,
			},
		},
	})

	// Verify HarnessEnv was injected exclusively into copilot CLI's env and PLUGIN_* was scrubbed.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "SOFA_HARNESS_PLUGIN" || strings.HasPrefix(key, "PLUGIN_") {
			_ = s.notify("session.event", map[string]any{
				"sessionId": sessionID,
				"event": map[string]any{
					"id":        "ev-err-env",
					"timestamp": now,
					"type":      "session.error",
					"data": map[string]any{
						"errorType": "internal",
						"message":   "leaked plugin handshake env var: " + key,
					},
				},
			})
			return
		}
	}
	if os.Getenv("SOFA_HARNESS_COPILOT_SECRET") != "copilot-in-mem-secret" {
		_ = s.notify("session.event", map[string]any{
			"sessionId": sessionID,
			"event": map[string]any{
				"id":        "ev-err-secret",
				"timestamp": now,
				"type":      "session.error",
				"data": map[string]any{
					"errorType": "authentication",
					"message":   "missing SOFA_HARNESS_COPILOT_SECRET in copilot CLI env",
				},
			},
		})
		return
	}

	// Trigger non-host built-in tool PreToolUse -> PostToolUse hooks.
	preOut, err := s.invokeHook(sessionID, "preToolUse", map[string]any{
		"sessionId": sessionID,
		"timestamp": time.Now().UnixMilli(),
		"cwd":       "/workspace",
		"toolName":  "bash",
		"toolArgs":  map[string]any{"command": "write feature"},
	})
	if err != nil {
		return
	}
	if dec, _ := preOut["permissionDecision"].(string); dec == "deny" {
		return
	}

	_ = os.MkdirAll("/workspace/allowed", 0o755)
	_ = os.WriteFile("/workspace/allowed/feature.txt", []byte("feature-v1\n"), 0o644)
	_ = os.MkdirAll("/.sofa/artifacts", 0o755)
	_ = os.WriteFile("/.sofa/artifacts/report.json", []byte(`{"status":"ok","harness":"copilot"}`+"\n"), 0o644)

	_, _ = s.invokeHook(sessionID, "postToolUse", map[string]any{
		"sessionId":  sessionID,
		"timestamp":  time.Now().UnixMilli(),
		"cwd":        "/workspace",
		"toolName":   "bash",
		"toolArgs":   map[string]any{"command": "write feature"},
		"toolResult": "ok",
	})

	_ = s.invokeExternalTool(sessionID, "req-ckpt-1", "tc-ckpt-1", "sofa_checkpoint", map[string]any{
		"completed":  []string{"wrote allowed/feature.txt and report.json"},
		"next_steps": []string{"complete work"},
	})
	_ = s.invokeExternalTool(sessionID, "req-complete-1", "tc-complete-1", "sofa_complete_work", map[string]any{
		"summary": "feature.txt and report.json ready via copilot plugin",
	})

	_ = s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-msg-1",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"type":      "assistant.message",
			"data": map[string]any{
				"messageId": "msg-work",
				"content":   "work completed",
			},
		},
	})
	_ = s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-idle-1",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"type":      "session.idle",
			"data":      map[string]any{},
		},
	})
}

func (s *server) runQCTurn(sessionID string) {
	var issues []map[string]any

	if err := os.WriteFile("/workspace/allowed/qc_tamper.txt", []byte("bad"), 0o644); err == nil {
		issues = append(issues, map[string]any{
			"criterion": "read_only_isolation",
			"code":      "workspace_writable",
			"message":   "QC container unexpectedly wrote to /workspace",
		})
	}
	if err := os.WriteFile("/.sofa/artifacts/report.json", []byte("tampered"), 0o644); err == nil {
		issues = append(issues, map[string]any{
			"criterion": "read_only_isolation",
			"code":      "artifacts_writable",
			"message":   "QC container unexpectedly wrote to /.sofa/artifacts",
		})
	}
	featBytes, err := os.ReadFile("/workspace/allowed/feature.txt")
	if err != nil || strings.TrimSpace(string(featBytes)) != "feature-v1" {
		issues = append(issues, map[string]any{
			"criterion": "feature_content",
			"code":      "missing_feature",
			"message":   fmt.Sprintf("unexpected feature.txt: err=%v content=%q", err, string(featBytes)),
		})
	}
	repBytes, err := os.ReadFile("/.sofa/artifacts/report.json")
	if err != nil || !strings.Contains(string(repBytes), `"harness":"copilot"`) {
		issues = append(issues, map[string]any{
			"criterion": "feature_content",
			"code":      "missing_report",
			"message":   fmt.Sprintf("unexpected report.json: err=%v content=%q", err, string(repBytes)),
		})
	}

	if len(issues) > 0 {
		_ = s.invokeExternalTool(sessionID, "req-qc-issues", "tc-qc-issues", "sofa_qc_open_issues", map[string]any{
			"issues": issues,
		})
		_ = s.invokeExternalTool(sessionID, "req-qc-revise", "tc-qc-revise", "sofa_qc_submit_verdict", map[string]any{
			"decision": "revise",
			"summary":  "qc found issues",
		})
	} else {
		_ = s.invokeExternalTool(sessionID, "req-qc-pass", "tc-qc-pass", "sofa_qc_submit_verdict", map[string]any{
			"decision": "pass",
			"summary":  "verified read-only isolation and copilot output",
		})
	}

	_ = s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-qc-msg",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"type":      "assistant.message",
			"data": map[string]any{
				"messageId": "msg-qc",
				"content":   "qc completed",
			},
		},
	})
	_ = s.notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        "ev-qc-idle",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"type":      "session.idle",
			"data":      map[string]any{},
		},
	})
}

func (s *server) handleRequest(msg rpcMessage) {
	switch msg.Method {
	case "connect":
		_ = s.reply(msg.ID, map[string]any{
			"ok":              true,
			"protocolVersion": 3,
			"version":         "1.0.19-fake",
		})

	case "session.create":
		var p struct {
			SessionID string `json:"sessionId"`
			Model     string `json:"model"`
			ConfigDir string `json:"configDir"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.ConfigDir != "" {
			_ = os.MkdirAll(p.ConfigDir, 0o755)
			_ = os.WriteFile(filepath.Join(p.ConfigDir, p.SessionID+".marker"), []byte("ok\n"), 0o644)
		}
		s.sessMu.Lock()
		s.sessions[p.SessionID] = p.Model
		s.sessMu.Unlock()
		_ = s.reply(msg.ID, map[string]any{
			"sessionId": p.SessionID,
		})

	case "session.send":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.sessMu.Lock()
		model := s.sessions[p.SessionID]
		s.sessMu.Unlock()

		_ = s.reply(msg.ID, map[string]any{
			"messageId": "msg-" + p.SessionID,
		})

		go func() {
			if model == "qc-model" {
				s.runQCTurn(p.SessionID)
			} else {
				s.runWorkTurn(p.SessionID)
			}
		}()

	case "session.tools.handlePendingToolCall":
		var p struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.pendingMu.Lock()
		ch := s.pendingTools[p.RequestID]
		delete(s.pendingTools, p.RequestID)
		s.pendingMu.Unlock()
		if ch != nil {
			ch <- p.RequestID
		}
		_ = s.reply(msg.ID, map[string]any{"success": true})

	case "session.detach":
		_ = s.reply(msg.ID, map[string]any{"success": true})

	case "session.options.update":
		_ = s.reply(msg.ID, map[string]any{})

	case "runtime.shutdown":
		_ = s.reply(msg.ID, map[string]any{})

	default:
		_ = s.reply(msg.ID, map[string]any{})
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	var contentLen int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, convErr := strconv.Atoi(strings.TrimSpace(v))
			if convErr == nil {
				contentLen = n
			}
		}
	}
	if contentLen <= 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func main() {
	srv := newServer(os.Stdout)
	reader := bufio.NewReader(os.Stdin)
	for {
		raw, err := readFrame(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			os.Exit(1)
		}
		var msg rpcMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Method == "" && len(msg.ID) > 0 {
			idKey := strings.TrimSpace(string(msg.ID))
			srv.pendingMu.Lock()
			ch := srv.pendingReplies[idKey]
			delete(srv.pendingReplies, idKey)
			srv.pendingMu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		if msg.Method != "" {
			srv.handleRequest(msg)
		}
	}
}
