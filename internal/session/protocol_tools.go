package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
)

const (
	ToolSofaCheckpoint      = "sofa_checkpoint"
	ToolSofaCompleteWork    = "sofa_complete_work"
	ToolSofaRequestDecision = "sofa_request_decision"
	ToolSofaPauseForHuman   = "sofa_pause_for_human"
	ToolSofaEscalate        = "sofa_escalate"

	ToolSofaQCOpenIssues    = "sofa_qc_open_issues"
	ToolSofaQCResolveIssues = "sofa_qc_resolve_issues"
	ToolSofaQCSubmitVerdict = "sofa_qc_submit_verdict"
)

// WorkProtocolOptions configures which Work protocol tools are mounted and how they validate.
type WorkProtocolOptions struct {
	HasCustomTerminate      bool
	AgentEscalatedFallbacks []string
	WritablePaths           []string
}

// WorkProtocolTools builds HostToolSpecs and gated ToolHandlers for a Work session.
func WorkProtocolTools(gates *GateController, opts WorkProtocolOptions) ([]runtime.HostToolSpec, map[string]runtime.ToolHandler) {
	specs := make([]runtime.HostToolSpec, 0, 5)
	handlers := make(map[string]runtime.ToolHandler, 5)

	specs = append(specs, runtime.HostToolSpec{
		Name:                 ToolSofaCheckpoint,
		Description:          "Persist a structured progress diary and workspace checkpoint before continuing or handing off.",
		ParametersJSONSchema: checkpointSchemaJSON(),
	})
	handlers[ToolSofaCheckpoint] = gates.WrapHostTool(ToolSofaCheckpoint, HostToolCheckpoint, "", checkpointHandler(gates, opts.WritablePaths))

	if !opts.HasCustomTerminate {
		specs = append(specs, runtime.HostToolSpec{
			Name:                 ToolSofaCompleteWork,
			Description:          "Signal that all requested work is complete and ready for gatekeeper/QC verification.",
			ParametersJSONSchema: completeWorkSchemaJSON(),
		})
		handlers[ToolSofaCompleteWork] = gates.WrapHostTool(ToolSofaCompleteWork, HostToolCompleteWork, "", completeWorkHandler(gates, opts.WritablePaths))
	}

	specs = append(specs, runtime.HostToolSpec{
		Name:                 ToolSofaRequestDecision,
		Description:          "Pause execution to ask the human operator a structured decision question.",
		ParametersJSONSchema: requestDecisionSchemaJSON(),
	})
	handlers[ToolSofaRequestDecision] = gates.WrapHostTool(ToolSofaRequestDecision, HostToolHandoff, "", requestDecisionHandler(gates))

	specs = append(specs, runtime.HostToolSpec{
		Name:                 ToolSofaPauseForHuman,
		Description:          "Pause execution and hand control back to the human operator after addressing their interjection.",
		ParametersJSONSchema: pauseForHumanSchemaJSON(),
	})
	handlers[ToolSofaPauseForHuman] = gates.WrapHostTool(ToolSofaPauseForHuman, HostToolHandoff, "", pauseForHumanHandler(gates))

	if len(opts.AgentEscalatedFallbacks) > 0 {
		specs = append(specs, runtime.HostToolSpec{
			Name:                 ToolSofaEscalate,
			Description:          "Escalate to a declared upstream fallback worker when the current task cannot proceed without upstream changes.",
			ParametersJSONSchema: escalateSchemaJSON(),
		})
		handlers[ToolSofaEscalate] = gates.WrapHostTool(ToolSofaEscalate, HostToolHandoff, "", escalateHandler(gates, opts.AgentEscalatedFallbacks))
	}

	return specs, handlers
}

// QCProtocolTools builds HostToolSpecs and gated ToolHandlers for a QC audit session.
func QCProtocolTools(gates *GateController, ledger *QCLedger) ([]runtime.HostToolSpec, map[string]runtime.ToolHandler) {
	specs := []runtime.HostToolSpec{
		{
			Name:                 ToolSofaQCOpenIssues,
			Description:          "Open one or more concrete QC issues tied to declared criteria. The host assigns sequential QC-xxx IDs.",
			ParametersJSONSchema: qcOpenIssuesSchemaJSON(),
		},
		{
			Name:                 ToolSofaQCResolveIssues,
			Description:          "Resolve previously opened QC issues after verifying fixes in the frozen workspace/artifacts.",
			ParametersJSONSchema: qcResolveIssuesSchemaJSON(),
		},
		{
			Name:                 ToolSofaQCSubmitVerdict,
			Description:          "Submit final QC verdict ('pass' when 0 open issues remain, or 'revise' when >=1 open issues exist).",
			ParametersJSONSchema: qcSubmitVerdictSchemaJSON(),
		},
	}
	handlers := map[string]runtime.ToolHandler{
		ToolSofaQCOpenIssues:    gates.WrapHostTool(ToolSofaQCOpenIssues, HostToolBusiness, "", qcOpenIssuesHandler(ledger)),
		ToolSofaQCResolveIssues: gates.WrapHostTool(ToolSofaQCResolveIssues, HostToolBusiness, "", qcResolveIssuesHandler(ledger)),
		ToolSofaQCSubmitVerdict: gates.WrapHostTool(ToolSofaQCSubmitVerdict, HostToolHandoff, "", qcSubmitVerdictHandler(gates, ledger)),
	}
	return specs, handlers
}

// WrapCustomTerminateHandler wraps a custom terminate tool binding so Executor.Invoke's
// Accepted flag and ValidateWorkspaceForHandoff are checked before setting roundComplete.
func WrapCustomTerminateHandler(gates *GateController, exec *toolruntime.Executor, binding toolruntime.ToolBinding, quotaKey string, writablePaths []string) runtime.ToolHandler {
	return gates.WrapHostTool(binding.EffectiveName(), HostToolCustomTerminate, quotaKey, func(ctx context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		if exec == nil {
			msg := fmt.Sprintf("tool executor is not configured for terminate tool %q", binding.EffectiveName())
			return runtime.ToolOutput{
				ResultType:       runtime.HostToolResultFailure,
				TextResultForLLM: msg,
				HandlerError:     msg,
			}, nil
		}
		res, err := exec.Invoke(ctx, binding, call.ArgumentsJSON)
		if err != nil {
			return res.ToolOutput, err
		}
		if res.ToolOutput.HandlerError != "" {
			return res.ToolOutput, nil
		}
		if !res.Accepted {
			gates.recordRejectedHandoff()
			return res.ToolOutput, nil
		}

		unauth, valErr := gates.hooks.ValidateWorkspaceForHandoff(ctx)
		if valErr != nil {
			return runtime.ToolOutput{
				ResultType:       runtime.HostToolResultFailure,
				TextResultForLLM: valErr.Error(),
				HandlerError:     valErr.Error(),
			}, nil
		}
		if len(unauth) > 0 {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(formatUnauthorizedPathsMsg(unauth, writablePaths)), nil
		}

		gates.completeRound(PhaseOutcome{
			Status:     OutcomeSucceeded,
			Summary:    string(res.OutputJSON),
			ResultJSON: append([]byte(nil), res.OutputJSON...),
		})
		return res.ToolOutput, nil
	})
}

func checkpointHandler(gates *GateController, writablePaths []string) runtime.ToolHandler {
	return func(ctx context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		var diary DiaryInput
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &diary); err != nil {
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaCheckpoint, err)), nil
		}
		if diary.Completed == nil || diary.NextSteps == nil {
			return modelProtocolRejection(fmt.Sprintf("%s requires both completed and next_steps arrays", ToolSofaCheckpoint)), nil
		}
		if err := diary.Validate(); err != nil {
			return modelProtocolRejection(err.Error()), nil
		}

		unauth, err := gates.hooks.ValidateWorkspaceForHandoff(ctx)
		if err != nil {
			return runtime.ToolOutput{
				ResultType:       runtime.HostToolResultFailure,
				TextResultForLLM: fmt.Sprintf("workspace validation error: %v", err),
				HandlerError:     err.Error(),
			}, nil
		}
		if len(unauth) > 0 {
			return modelProtocolRejection(formatUnauthorizedPathsMsg(unauth, writablePaths)), nil
		}

		if err := gates.hooks.OnCheckpointRequested(ctx, diary); err != nil {
			return runtime.ToolOutput{
				ResultType:       runtime.HostToolResultFailure,
				TextResultForLLM: fmt.Sprintf("checkpoint failed: %v", err),
				HandlerError:     err.Error(),
			}, nil
		}

		gates.completeCheckpoint(diary)
		return modelAcceptedJSON(map[string]any{"accepted": true}), nil
	}
}

func completeWorkHandler(gates *GateController, writablePaths []string) runtime.ToolHandler {
	return func(ctx context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		var req struct {
			Summary string `json:"summary"`
		}
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaCompleteWork, err)), nil
		}
		if strings.TrimSpace(req.Summary) == "" {
			gates.recordRejectedHandoff()
			return modelProtocolRejection("summary is required"), nil
		}

		unauth, err := gates.hooks.ValidateWorkspaceForHandoff(ctx)
		if err != nil {
			return runtime.ToolOutput{
				ResultType:       runtime.HostToolResultFailure,
				TextResultForLLM: fmt.Sprintf("workspace validation error: %v", err),
				HandlerError:     err.Error(),
			}, nil
		}
		if len(unauth) > 0 {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(formatUnauthorizedPathsMsg(unauth, writablePaths)), nil
		}

		summary := strings.TrimSpace(req.Summary)
		gates.completeRound(PhaseOutcome{
			Status:  OutcomeSucceeded,
			Summary: summary,
		})
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"summary":  summary,
		}), nil
	}
}

func requestDecisionHandler(gates *GateController) runtime.ToolHandler {
	return func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		var req DecisionRequest
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaRequestDecision, err)), nil
		}
		if err := req.Validate(); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(err.Error()), nil
		}
		copied := req
		gates.completeRound(PhaseOutcome{
			Status:          OutcomeWaitingHuman,
			DecisionRequest: &copied,
		})
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"question": req.Question,
		}), nil
	}
}

func pauseForHumanHandler(gates *GateController) runtime.ToolHandler {
	return func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		var req struct {
			Reason string `json:"reason"`
		}
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaPauseForHuman, err)), nil
		}
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			gates.recordRejectedHandoff()
			return modelProtocolRejection("reason is required"), nil
		}
		gates.completeRound(PhaseOutcome{
			Status:      OutcomeWaitingHuman,
			PauseReason: reason,
		})
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"reason":   reason,
		}), nil
	}
}

func escalateHandler(gates *GateController, allowedFallbacks []string) runtime.ToolHandler {
	return func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		var req EscalationRequest
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaEscalate, err)), nil
		}
		if err := req.Validate(); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(err.Error()), nil
		}
		req.FallbackName = strings.TrimSpace(req.FallbackName)
		req.Summary = strings.TrimSpace(req.Summary)

		if req.FallbackName == "" {
			if len(allowedFallbacks) == 1 {
				req.FallbackName = allowedFallbacks[0]
			} else {
				gates.recordRejectedHandoff()
				return modelProtocolRejection(fmt.Sprintf("fallback name is required when multiple agent_escalated fallbacks exist (%s)", strings.Join(allowedFallbacks, ", "))), nil
			}
		} else if !slices.Contains(allowedFallbacks, req.FallbackName) {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(fmt.Sprintf("unknown agent_escalated fallback %q (allowed: %s)", req.FallbackName, strings.Join(allowedFallbacks, ", "))), nil
		}

		copied := req
		gates.completeRound(PhaseOutcome{
			Status:     OutcomeAgentEscalated,
			Summary:    req.Summary,
			Escalation: &copied,
		})
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"fallback": req.FallbackName,
			"summary":  req.Summary,
		}), nil
	}
}

func qcOpenIssuesHandler(ledger *QCLedger) runtime.ToolHandler {
	return func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		if ledger == nil {
			msg := "qc ledger is not configured"
			return runtime.ToolOutput{ResultType: runtime.HostToolResultFailure, TextResultForLLM: msg, HandlerError: msg}, nil
		}
		var req struct {
			Issues []QCIssueInput `json:"issues"`
		}
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaQCOpenIssues, err)), nil
		}
		opened, err := ledger.OpenIssues(req.Issues)
		if err != nil {
			return modelProtocolRejection(err.Error()), nil
		}
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"issues":   opened,
		}), nil
	}
}

func qcResolveIssuesHandler(ledger *QCLedger) runtime.ToolHandler {
	return func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		if ledger == nil {
			msg := "qc ledger is not configured"
			return runtime.ToolOutput{ResultType: runtime.HostToolResultFailure, TextResultForLLM: msg, HandlerError: msg}, nil
		}
		var req struct {
			IDs                []string `json:"ids"`
			ResolutionEvidence string   `json:"resolution_evidence"`
		}
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaQCResolveIssues, err)), nil
		}
		resolved, err := ledger.ResolveIssues(req.IDs, req.ResolutionEvidence)
		if err != nil {
			return modelProtocolRejection(err.Error()), nil
		}
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"resolved": resolved,
		}), nil
	}
}

func qcSubmitVerdictHandler(gates *GateController, ledger *QCLedger) runtime.ToolHandler {
	return func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
		if ledger == nil {
			msg := "qc ledger is not configured"
			return runtime.ToolOutput{ResultType: runtime.HostToolResultFailure, TextResultForLLM: msg, HandlerError: msg}, nil
		}
		var req struct {
			Decision string `json:"decision"`
			Summary  string `json:"summary"`
		}
		if err := decodeStrictJSONObject(call.ArgumentsJSON, &req); err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(fmt.Sprintf("invalid %s arguments: %v", ToolSofaQCSubmitVerdict, err)), nil
		}
		verdict, err := ledger.SubmitVerdict(req.Decision, req.Summary)
		if err != nil {
			gates.recordRejectedHandoff()
			return modelProtocolRejection(err.Error()), nil
		}
		copied := verdict
		gates.completeRound(PhaseOutcome{
			Status:    OutcomeQCCompleted,
			Summary:   verdict.Summary,
			QCVerdict: &copied,
		})
		return modelAcceptedJSON(map[string]any{
			"accepted": true,
			"decision": verdict.Decision,
			"summary":  verdict.Summary,
		}), nil
	}
}

func decodeStrictJSONObject(raw []byte, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing tokens after JSON object")
	}
	return nil
}

func formatUnauthorizedPathsMsg(unauthorized, writablePaths []string) string {
	allowed := "(none)"
	if len(writablePaths) > 0 {
		allowed = strings.Join(writablePaths, ", ")
	}
	return fmt.Sprintf(
		"workspace contains modified or untracked files outside writable_paths: [%s]; allowed writable_paths: [%s]. Move or remove unauthorized files before retrying.",
		strings.Join(unauthorized, ", "),
		allowed,
	)
}

func modelProtocolRejection(message string) runtime.ToolOutput {
	raw, _ := json.Marshal(struct {
		Accepted bool     `json:"accepted"`
		Issues   []string `json:"issues"`
	}{
		Accepted: false,
		Issues:   []string{message},
	})
	return runtime.ToolOutput{
		ResultType:       runtime.HostToolResultSuccess,
		TextResultForLLM: string(raw),
	}
}

func modelAcceptedJSON(payload any) runtime.ToolOutput {
	raw, _ := json.Marshal(payload)
	return runtime.ToolOutput{
		ResultType:       runtime.HostToolResultSuccess,
		TextResultForLLM: string(raw),
	}
}

func mustMarshalSchema(schema map[string]any) []byte {
	b, _ := json.Marshal(schema)
	return b
}

func checkpointSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"completed":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"hypotheses":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"decisions":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"test_evidence":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"open_questions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"next_steps":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"completed", "next_steps"},
		"additionalProperties": false,
	})
}

func completeWorkSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
		},
		"required":             []string{"summary"},
		"additionalProperties": false,
	})
}

func requestDecisionSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question":    map[string]any{"type": "string"},
			"context":     map[string]any{"type": "string"},
			"options":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"recommended": map[string]any{"type": "string"},
		},
		"required":             []string{"question"},
		"additionalProperties": false,
	})
}

func pauseForHumanSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reason": map[string]any{"type": "string"},
		},
		"required":             []string{"reason"},
		"additionalProperties": false,
	})
}

func escalateSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"fallback": map[string]any{"type": "string"},
			"summary":  map[string]any{"type": "string"},
		},
		"required":             []string{"summary"},
		"additionalProperties": false,
	})
}

func qcOpenIssuesSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"issues": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"criterion":   map[string]any{"type": "string"},
						"code":        map[string]any{"type": "string"},
						"message":     map[string]any{"type": "string"},
						"path":        map[string]any{"type": "string"},
						"evidence":    map[string]any{"type": "string"},
						"repair_hint": map[string]any{"type": "string"},
					},
					"required":             []string{"criterion", "code", "message"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"issues"},
		"additionalProperties": false,
	})
}

func qcResolveIssuesSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ids":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"resolution_evidence": map[string]any{"type": "string"},
		},
		"required":             []string{"ids", "resolution_evidence"},
		"additionalProperties": false,
	})
}

func qcSubmitVerdictSchemaJSON() []byte {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []string{"pass", "revise"}},
			"summary":  map[string]any{"type": "string"},
		},
		"required":             []string{"decision", "summary"},
		"additionalProperties": false,
	})
}
