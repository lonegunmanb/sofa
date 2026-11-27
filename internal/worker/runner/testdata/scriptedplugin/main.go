package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lonegunmanb/sofa/pkg/harnessplugin"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
)

type scriptedServer struct {
	harnessv1.UnimplementedAgentHarnessServer

	mu       sync.Mutex
	sessions map[string]*harnessv1.OpenSessionRequest
}

func newScriptedServer() *scriptedServer {
	return &scriptedServer{
		sessions: make(map[string]*harnessv1.OpenSessionRequest),
	}
}

func (s *scriptedServer) GetCapabilities(_ context.Context, _ *harnessv1.GetCapabilitiesRequest) (*harnessv1.GetCapabilitiesResponse, error) {
	return &harnessv1.GetCapabilitiesResponse{
		HarnessName:            "scripted",
		HarnessVersion:         "0.1.0",
		ProtocolVersion:        1,
		SupportedProviderTypes: []string{"openai", "azure", "anthropic"},
		SupportsSessionResume:  true,
		SupportsMcpServers:     true,
		SupportsMcpResources:   true,
	}, nil
}

func (s *scriptedServer) OpenSession(_ context.Context, req *harnessv1.OpenSessionRequest) (*harnessv1.OpenSessionResponse, error) {
	sessionID := req.SessionId
	if sessionID == "" {
		sessionID = "sess-scripted-" + req.Model
	}
	if req.SessionStoreDirectory != "" {
		if err := os.MkdirAll(req.SessionStoreDirectory, 0o755); err != nil {
			return nil, err
		}
		_ = os.WriteFile(filepath.Join(req.SessionStoreDirectory, sessionID+".marker"), []byte("ok\n"), 0o644)
	}
	s.mu.Lock()
	s.sessions[sessionID] = req
	s.mu.Unlock()
	return &harnessv1.OpenSessionResponse{
		SessionId: sessionID,
		Resumed:   req.ResumeSessionId != "",
	}, nil
}

func (s *scriptedServer) RunTurn(stream harnessv1.AgentHarness_RunTurnServer) error {
	firstFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	startReq := firstFrame.GetStartTurn()
	if startReq == nil {
		return fmt.Errorf("expected StartTurnRequest")
	}

	s.mu.Lock()
	sessReq := s.sessions[startReq.SessionId]
	s.mu.Unlock()
	if sessReq == nil {
		return fmt.Errorf("unknown session %q", startReq.SessionId)
	}

	if sessReq.Model == "qc-model" {
		return s.runQC(stream, startReq)
	}
	if sessReq.Model == "work-skills-model" {
		if err := verifyContainerSkills(sessReq); err != nil {
			return err
		}
	}
	return s.runWork(stream, startReq)
}

func verifyContainerSkills(req *harnessv1.OpenSessionRequest) error {
	disabled := make(map[string]struct{}, len(req.DisabledSkills))
	for _, d := range req.DisabledSkills {
		disabled[d] = struct{}{}
	}
	for _, s := range req.Skills {
		if _, bad := disabled[s]; bad {
			return fmt.Errorf("disabled skill %q leaked into OpenSessionRequest.Skills", s)
		}
	}
	for _, dir := range req.SkillDirectories {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read container skill dir %q: %w", dir, err)
		}
		for _, ent := range entries {
			if _, bad := disabled[ent.Name()]; bad {
				return fmt.Errorf("disabled skill directory %q still present under %q", ent.Name(), dir)
			}
			skillMD := filepath.Join(dir, ent.Name(), "SKILL.md")
			if _, err := os.ReadFile(skillMD); err != nil {
				return fmt.Errorf("read SKILL.md at %q: %w", skillMD, err)
			}
			helperSh := filepath.Join(dir, ent.Name(), "scripts", "helper.sh")
			if info, statErr := os.Stat(helperSh); statErr == nil {
				if info.Mode().Perm()&0o111 == 0 {
					return fmt.Errorf("expected helper script %q to have +x mode, got %o", helperSh, info.Mode().Perm())
				}
				if out, execErr := exec.Command(helperSh).CombinedOutput(); execErr != nil {
					return fmt.Errorf("execute %q: %v (%s)", helperSh, execErr, string(out))
				}
			}
		}
	}
	return nil
}

func invokeHostTool(stream harnessv1.AgentHarness_RunTurnServer, callID, toolName string, payload any) (*harnessv1.InvokeHostToolResponse, error) {
	argsBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_InvokeHostTool{
			InvokeHostTool: &harnessv1.InvokeHostToolRequest{
				ToolCallId:    callID,
				ToolName:      toolName,
				ArgumentsJson: argsBytes,
			},
		},
	}); err != nil {
		return nil, err
	}
	respFrame, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	resp := respFrame.GetInvokeHostToolResponse()
	if resp == nil {
		return nil, fmt.Errorf("expected InvokeHostToolResponse for %s", toolName)
	}
	return resp, nil
}

func (s *scriptedServer) runWork(stream harnessv1.AgentHarness_RunTurnServer, startReq *harnessv1.StartTurnRequest) error {
	if err := os.MkdirAll("/workspace/allowed", 0o755); err != nil {
		return fmt.Errorf("mkdir /workspace/allowed: %w", err)
	}
	if err := os.WriteFile("/workspace/allowed/feature.txt", []byte("feature-v1\n"), 0o644); err != nil {
		return fmt.Errorf("write /workspace/allowed/feature.txt: %w", err)
	}
	if err := os.MkdirAll("/.sofa/artifacts", 0o755); err != nil {
		return fmt.Errorf("mkdir /.sofa/artifacts: %w", err)
	}
	if err := os.WriteFile("/.sofa/artifacts/report.json", []byte(`{"status":"ok","feature":"v1"}`+"\n"), 0o644); err != nil {
		return fmt.Errorf("write /.sofa/artifacts/report.json: %w", err)
	}

	if _, err := invokeHostTool(stream, "call-ckpt-1", "sofa_checkpoint", map[string]any{
		"completed":  []string{"implemented feature.txt and report.json"},
		"next_steps": []string{"complete work"},
	}); err != nil {
		return err
	}
	if _, err := invokeHostTool(stream, "call-complete-1", "sofa_complete_work", map[string]any{
		"summary": "feature.txt and report.json ready",
	}); err != nil {
		return err
	}

	return stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_TurnResult{
			TurnResult: &harnessv1.TurnResult{
				TurnId:             startReq.TurnId,
				AssistantMessageId: "msg-work",
				FinalContent:       "work completed",
			},
		},
	})
}

func (s *scriptedServer) runQC(stream harnessv1.AgentHarness_RunTurnServer, startReq *harnessv1.StartTurnRequest) error {
	var issues []map[string]any

	// 1. Verify /workspace is physically read-only.
	if err := os.WriteFile("/workspace/allowed/qc_tamper.txt", []byte("bad"), 0o644); err == nil {
		issues = append(issues, map[string]any{
			"criterion": "read_only_isolation",
			"code":      "workspace_writable",
			"message":   "QC container was unexpectedly able to write to /workspace",
		})
	} else if !strings.Contains(strings.ToLower(err.Error()), "read-only") && !strings.Contains(strings.ToLower(err.Error()), "permission") {
		issues = append(issues, map[string]any{
			"criterion": "read_only_isolation",
			"code":      "unexpected_workspace_error",
			"message":   err.Error(),
		})
	}

	// 2. Verify /.sofa/artifacts/report.json is physically read-only.
	if err := os.WriteFile("/.sofa/artifacts/report.json", []byte("tampered"), 0o644); err == nil {
		issues = append(issues, map[string]any{
			"criterion": "read_only_isolation",
			"code":      "artifacts_writable",
			"message":   "QC container was unexpectedly able to write to /.sofa/artifacts/report.json",
		})
	}

	// 3. Verify frozen workspace and artifact contents match Work output.
	featBytes, err := os.ReadFile("/workspace/allowed/feature.txt")
	if err != nil || strings.TrimSpace(string(featBytes)) != "feature-v1" {
		issues = append(issues, map[string]any{
			"criterion": "feature_content",
			"code":      "missing_feature",
			"message":   fmt.Sprintf("unexpected feature.txt: err=%v content=%q", err, string(featBytes)),
		})
	}
	repBytes, err := os.ReadFile("/.sofa/artifacts/report.json")
	if err != nil || !strings.Contains(string(repBytes), `"status":"ok"`) {
		issues = append(issues, map[string]any{
			"criterion": "feature_content",
			"code":      "missing_report",
			"message":   fmt.Sprintf("unexpected report.json: err=%v content=%q", err, string(repBytes)),
		})
	}

	if len(issues) > 0 {
		if _, err := invokeHostTool(stream, "call-open-issues", "sofa_qc_open_issues", map[string]any{
			"issues": issues,
		}); err != nil {
			return err
		}
		if _, err := invokeHostTool(stream, "call-verdict-revise", "sofa_qc_submit_verdict", map[string]any{
			"decision": "revise",
			"summary":  "qc verification failed",
		}); err != nil {
			return err
		}
	} else {
		if _, err := invokeHostTool(stream, "call-verdict-pass", "sofa_qc_submit_verdict", map[string]any{
			"decision": "pass",
			"summary":  "verified read-only workspace and report.json",
		}); err != nil {
			return err
		}
	}

	return stream.Send(&harnessv1.PluginTurnFrame{
		Payload: &harnessv1.PluginTurnFrame_TurnResult{
			TurnResult: &harnessv1.TurnResult{
				TurnId:             startReq.TurnId,
				AssistantMessageId: "msg-qc",
				FinalContent:       "qc completed",
			},
		},
	})
}

func (s *scriptedServer) AbortTurn(_ context.Context, _ *harnessv1.AbortTurnRequest) (*harnessv1.AbortTurnResponse, error) {
	return &harnessv1.AbortTurnResponse{}, nil
}

func (s *scriptedServer) ResumeSession(_ context.Context, _ *harnessv1.ResumeSessionRequest) (*harnessv1.ResumeSessionResponse, error) {
	return &harnessv1.ResumeSessionResponse{}, nil
}

func (s *scriptedServer) ReadMCPResource(_ context.Context, _ *harnessv1.ReadMCPResourceRequest) (*harnessv1.ReadMCPResourceResponse, error) {
	return &harnessv1.ReadMCPResourceResponse{}, nil
}

func (s *scriptedServer) CloseSession(_ context.Context, req *harnessv1.CloseSessionRequest) (*harnessv1.CloseSessionResponse, error) {
	s.mu.Lock()
	delete(s.sessions, req.SessionId)
	s.mu.Unlock()
	return &harnessv1.CloseSessionResponse{}, nil
}

func main() {
	harnessplugin.Serve(newScriptedServer())
}
