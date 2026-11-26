package session

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// QCIssueInput describes a single issue opened by the QC agent without a model-chosen ID.
type QCIssueInput struct {
	Criterion  string `json:"criterion"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Path       string `json:"path,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	RepairHint string `json:"repair_hint,omitempty"`
}

// QCIssue represents a host-tracked QC issue with a deterministic monotonic ID (QC-001, ...).
type QCIssue struct {
	ID                 string `json:"id"`
	Criterion          string `json:"criterion"`
	Code               string `json:"code"`
	Message            string `json:"message"`
	Path               string `json:"path,omitempty"`
	Evidence           string `json:"evidence,omitempty"`
	RepairHint         string `json:"repair_hint,omitempty"`
	Resolved           bool   `json:"resolved"`
	ResolutionEvidence string `json:"resolution_evidence,omitempty"`
}

// QCVerdict records the validated outcome of sofa_qc_submit_verdict.
type QCVerdict struct {
	Decision string `json:"decision"`
	Summary  string `json:"summary"`
}

type qcLedgerSnapshot struct {
	Criteria map[string]string `json:"criteria"`
	NextSeq  int               `json:"next_seq"`
	History  []QCIssue         `json:"history"`
}

// QCLedger is a thread-safe host-owned cross-round QC issue ledger.
type QCLedger struct {
	mu       sync.Mutex
	criteria map[string]string
	nextSeq  int
	history  []QCIssue
	byID     map[string]int
	openIDs  []string
}

// NewQCLedger constructs a QCLedger initialized with valid qc.criteria keys.
func NewQCLedger(criteria map[string]string) (*QCLedger, error) {
	if len(criteria) == 0 {
		return nil, fmt.Errorf("qc criteria must not be empty")
	}
	copied := make(map[string]string, len(criteria))
	for k, v := range criteria {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("qc criteria keys and descriptions must not be empty")
		}
		copied[k] = v
	}
	return &QCLedger{
		criteria: copied,
		nextSeq:  1,
		byID:     make(map[string]int),
	}, nil
}

// Criteria returns a defensive copy of the configured criteria map.
func (l *QCLedger) Criteria() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return maps.Clone(l.criteria)
}

// OpenIssues validates all inputs atomically, assigns sequential QC-001.. IDs, and records them.
func (l *QCLedger) OpenIssues(inputs []QCIssueInput) ([]QCIssue, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("at least one issue is required")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureInit()

	if len(l.criteria) == 0 {
		return nil, fmt.Errorf("qc ledger has no configured criteria")
	}

	for i, in := range inputs {
		if _, ok := l.criteria[in.Criterion]; !ok {
			return nil, fmt.Errorf("issues[%d]: unknown criterion %q (allowed: %s)", i, in.Criterion, strings.Join(l.sortedCriteriaKeysLocked(), ", "))
		}
		if strings.TrimSpace(in.Code) == "" {
			return nil, fmt.Errorf("issues[%d]: code is required", i)
		}
		if strings.TrimSpace(in.Message) == "" {
			return nil, fmt.Errorf("issues[%d]: message is required", i)
		}
	}

	opened := make([]QCIssue, 0, len(inputs))
	for _, in := range inputs {
		id := fmt.Sprintf("QC-%03d", l.nextSeq)
		l.nextSeq++
		issue := QCIssue{
			ID:         id,
			Criterion:  in.Criterion,
			Code:       strings.TrimSpace(in.Code),
			Message:    strings.TrimSpace(in.Message),
			Path:       strings.TrimSpace(in.Path),
			Evidence:   strings.TrimSpace(in.Evidence),
			RepairHint: strings.TrimSpace(in.RepairHint),
		}
		l.byID[id] = len(l.history)
		l.history = append(l.history, issue)
		l.openIDs = append(l.openIDs, id)
		opened = append(opened, issue)
	}
	return opened, nil
}

// ResolveIssues validates all issue IDs atomically and marks them resolved with evidence.
func (l *QCLedger) ResolveIssues(ids []string, resolutionEvidence string) ([]QCIssue, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("at least one issue id is required")
	}
	evidence := strings.TrimSpace(resolutionEvidence)
	if evidence == "" {
		return nil, fmt.Errorf("resolution_evidence is required")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureInit()

	seen := make(map[string]struct{}, len(ids))
	indices := make([]int, 0, len(ids))
	for i, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return nil, fmt.Errorf("ids[%d]: issue id must not be empty", i)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("duplicate issue id %q in resolve request", id)
		}
		seen[id] = struct{}{}

		idx, exists := l.byID[id]
		if !exists {
			return nil, fmt.Errorf("unknown issue id %q", id)
		}
		if l.history[idx].Resolved {
			return nil, fmt.Errorf("issue %q is already resolved", id)
		}
		indices = append(indices, idx)
	}

	resolved := make([]QCIssue, 0, len(indices))
	for _, idx := range indices {
		l.history[idx].Resolved = true
		l.history[idx].ResolutionEvidence = evidence
		resolved = append(resolved, l.history[idx])
	}

	remaining := make([]string, 0, len(l.openIDs))
	for _, openID := range l.openIDs {
		if _, wasResolved := seen[openID]; !wasResolved {
			remaining = append(remaining, openID)
		}
	}
	l.openIDs = remaining
	return resolved, nil
}

// SubmitVerdict enforces cross-round pass/revise ledger invariants before accepting a verdict.
func (l *QCLedger) SubmitVerdict(decision, summary string) (QCVerdict, error) {
	trimmedSummary := strings.TrimSpace(summary)
	if trimmedSummary == "" {
		return QCVerdict{}, fmt.Errorf("verdict summary is required")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureInit()

	switch decision {
	case "pass":
		if len(l.openIDs) > 0 {
			return QCVerdict{}, fmt.Errorf("cannot submit pass verdict while %d open QC issues remain (%s); verify and resolve them via sofa_qc_resolve_issues first", len(l.openIDs), strings.Join(l.openIDs, ", "))
		}
	case "revise":
		if len(l.openIDs) == 0 {
			return QCVerdict{}, fmt.Errorf("cannot submit revise verdict with 0 open QC issues; record at least one issue via sofa_qc_open_issues first")
		}
	default:
		return QCVerdict{}, fmt.Errorf("decision must be \"pass\" or \"revise\", got %q", decision)
	}

	return QCVerdict{
		Decision: decision,
		Summary:  trimmedSummary,
	}, nil
}

// OpenIssuesSnapshot returns a copy of currently open QC issues in open order.
func (l *QCLedger) OpenIssuesSnapshot() []QCIssue {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureInit()

	out := make([]QCIssue, 0, len(l.openIDs))
	for _, id := range l.openIDs {
		if idx, ok := l.byID[id]; ok {
			out = append(out, l.history[idx])
		}
	}
	return out
}

// AllIssues returns a defensive copy of all opened issues (both open and resolved).
func (l *QCLedger) AllIssues() []QCIssue {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureInit()
	return slices.Clone(l.history)
}

// Snapshot serializes QCLedger state to JSON for checkpointing across rounds.
func (l *QCLedger) Snapshot() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureInit()

	snap := qcLedgerSnapshot{
		Criteria: maps.Clone(l.criteria),
		NextSeq:  l.nextSeq,
		History:  slices.Clone(l.history),
	}
	return json.Marshal(snap)
}

// Restore replaces QCLedger state from a serialized Snapshot.
func (l *QCLedger) Restore(data []byte) error {
	var snap qcLedgerSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("unmarshal qc ledger snapshot: %w", err)
	}
	if len(snap.Criteria) == 0 {
		return fmt.Errorf("qc ledger snapshot missing criteria")
	}
	for k, v := range snap.Criteria {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("qc ledger snapshot has invalid criteria entry")
		}
	}
	if snap.NextSeq < 1 {
		snap.NextSeq = len(snap.History) + 1
	}

	byID := make(map[string]int, len(snap.History))
	openIDs := make([]string, 0)
	for i, issue := range snap.History {
		if strings.TrimSpace(issue.ID) == "" {
			return fmt.Errorf("qc ledger snapshot history[%d] missing id", i)
		}
		if _, dup := byID[issue.ID]; dup {
			return fmt.Errorf("qc ledger snapshot has duplicate issue id %q", issue.ID)
		}
		byID[issue.ID] = i
		if !issue.Resolved {
			openIDs = append(openIDs, issue.ID)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.criteria = maps.Clone(snap.Criteria)
	l.nextSeq = snap.NextSeq
	l.history = slices.Clone(snap.History)
	l.byID = byID
	l.openIDs = openIDs
	return nil
}

func (l *QCLedger) ensureInit() {
	if l.byID == nil {
		l.byID = make(map[string]int)
	}
	if l.nextSeq < 1 {
		l.nextSeq = 1
	}
}

func (l *QCLedger) sortedCriteriaKeysLocked() []string {
	keys := make([]string, 0, len(l.criteria))
	for k := range l.criteria {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
