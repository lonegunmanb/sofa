package session

import (
	"context"
	"fmt"
	"strings"
)

// DiaryInput captures the structured checkpoint diary submitted by the Agent via sofa_checkpoint.
type DiaryInput struct {
	Completed     []string `json:"completed"`
	Hypotheses    []string `json:"hypotheses,omitempty"`
	Decisions     []string `json:"decisions,omitempty"`
	TestEvidence  []string `json:"test_evidence,omitempty"`
	OpenQuestions []string `json:"open_questions,omitempty"`
	NextSteps     []string `json:"next_steps"`
}

// Validate checks that DiaryInput satisfies protocol invariants.
func (d DiaryInput) Validate() error {
	if err := validateStringSlice("completed", d.Completed); err != nil {
		return err
	}
	if err := validateStringSlice("hypotheses", d.Hypotheses); err != nil {
		return err
	}
	if err := validateStringSlice("decisions", d.Decisions); err != nil {
		return err
	}
	if err := validateStringSlice("test_evidence", d.TestEvidence); err != nil {
		return err
	}
	if err := validateStringSlice("open_questions", d.OpenQuestions); err != nil {
		return err
	}
	if err := validateStringSlice("next_steps", d.NextSteps); err != nil {
		return err
	}
	if len(d.Completed) == 0 && len(d.NextSteps) == 0 {
		return fmt.Errorf("diary must include at least one item in completed or next_steps")
	}
	return nil
}

// HumanInterjection represents a live operator message injected into an active session.
type HumanInterjection struct {
	ID                  string `json:"id"`
	Message             string `json:"message"`
	WaitForConfirmation bool   `json:"wait_for_confirmation"`
}

// DecisionRequest captures the payload submitted via sofa_request_decision.
type DecisionRequest struct {
	Question    string   `json:"question"`
	Context     string   `json:"context,omitempty"`
	Options     []string `json:"options,omitempty"`
	Recommended string   `json:"recommended,omitempty"`
}

// Validate checks that DecisionRequest has a non-blank question and non-blank options.
func (r DecisionRequest) Validate() error {
	if strings.TrimSpace(r.Question) == "" {
		return fmt.Errorf("decision question is required")
	}
	if err := validateStringSlice("options", r.Options); err != nil {
		return err
	}
	return nil
}

// EscalationRequest captures the payload submitted via sofa_escalate.
type EscalationRequest struct {
	FallbackName string `json:"fallback,omitempty"`
	Summary      string `json:"summary"`
}

// Validate checks that EscalationRequest includes a non-blank summary.
func (r EscalationRequest) Validate() error {
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("escalation summary is required")
	}
	return nil
}

// ControlHooks decouples session gates from host checkpoint persistence, Git workspace checks,
// and operator interjection queues (implemented in Slices 017/018 and Stage 3 NodeActor).
type ControlHooks interface {
	DrainInterjections(ctx context.Context) ([]HumanInterjection, error)
	OnCheckpointRequested(ctx context.Context, diary DiaryInput) error
	ValidateWorkspaceForHandoff(ctx context.Context) ([]string, error)
}

// NoopControlHooks provides default no-op implementations when hooks are omitted.
type NoopControlHooks struct{}

func (NoopControlHooks) DrainInterjections(context.Context) ([]HumanInterjection, error) {
	return nil, nil
}

func (NoopControlHooks) OnCheckpointRequested(context.Context, DiaryInput) error {
	return nil
}

func (NoopControlHooks) ValidateWorkspaceForHandoff(context.Context) ([]string, error) {
	return nil, nil
}

// FormatDiarySummary formats historical structured diary entries into prompt context
// for session recovery fallback when SESSION_NOT_FOUND occurs.
func FormatDiarySummary(diaries []DiaryInput) string {
	if len(diaries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Historical Checkpoint Diary Summary\n")
	for i, d := range diaries {
		fmt.Fprintf(&b, "\n### Checkpoint %d\n", i+1)
		writeDiarySection(&b, "Completed", d.Completed)
		writeDiarySection(&b, "Decisions", d.Decisions)
		writeDiarySection(&b, "Hypotheses", d.Hypotheses)
		writeDiarySection(&b, "Test Evidence", d.TestEvidence)
		writeDiarySection(&b, "Open Questions", d.OpenQuestions)
		writeDiarySection(&b, "Next Steps", d.NextSteps)
	}
	return strings.TrimSpace(b.String())
}

func writeDiarySection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "- %s:\n", title)
	for _, item := range items {
		fmt.Fprintf(b, "  - %s\n", strings.TrimSpace(item))
	}
}

func validateStringSlice(field string, items []string) error {
	for i, item := range items {
		if strings.TrimSpace(item) == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
	}
	return nil
}
