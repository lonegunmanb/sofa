package spec_test

import (
	"encoding/json"
	"testing"

	"github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssueValidateAndJSON(t *testing.T) {
	t.Parallel()

	path := "query"
	hint := "provide a non-empty query"

	tests := []struct {
		name          string
		issue         spec.Issue
		expectedError string
		expectedJSON  string
	}{
		{
			name:         "required fields",
			issue:        spec.Issue{Code: "invalid_query", Message: "query must not be empty"},
			expectedJSON: `{"code":"invalid_query","message":"query must not be empty"}`,
		},
		{
			name: "optional fields",
			issue: spec.Issue{
				ID:         "iss-1",
				Code:       "invalid_query",
				Message:    "query must not be empty",
				Path:       &path,
				RepairHint: &hint,
			},
			expectedJSON: `{"id":"iss-1","code":"invalid_query","message":"query must not be empty","path":"query","repair_hint":"provide a non-empty query"}`,
		},
		{
			name:          "missing code",
			issue:         spec.Issue{Message: "query must not be empty"},
			expectedError: "issue code is required",
		},
		{
			name:          "blank code",
			issue:         spec.Issue{Code: " \t", Message: "query must not be empty"},
			expectedError: "issue code is required",
		},
		{
			name:          "missing message",
			issue:         spec.Issue{Code: "invalid_query"},
			expectedError: "issue message is required",
		},
		{
			name:          "blank message",
			issue:         spec.Issue{Code: "invalid_query", Message: " \n"},
			expectedError: "issue message is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.issue.Validate()
			if tt.expectedError != "" {
				assert.EqualError(t, err, tt.expectedError)
				return
			}
			require.NoError(t, err)
			encoded, err := json.Marshal(tt.issue)
			require.NoError(t, err)
			assert.JSONEq(t, tt.expectedJSON, string(encoded))
		})
	}
}

func TestToolResponseValidate(t *testing.T) {
	t.Parallel()

	output := "finished"
	validIssue := spec.Issue{Code: "invalid_query", Message: "query must not be empty"}
	invalidIssue := spec.Issue{Code: "invalid_query"}

	tests := []struct {
		name          string
		response      spec.ToolResponse[string]
		expectedError string
	}{
		{
			name:     "accepted without output",
			response: spec.ToolResponse[string]{Accepted: true},
		},
		{
			name:     "accepted with output",
			response: spec.ToolResponse[string]{Accepted: true, Output: &output},
		},
		{
			name:          "accepted with issues",
			response:      spec.ToolResponse[string]{Accepted: true, Issues: []spec.Issue{validIssue}},
			expectedError: "accepted response must not contain issues",
		},
		{
			name:     "rejected with issue",
			response: spec.ToolResponse[string]{Issues: []spec.Issue{validIssue}},
		},
		{
			name:          "rejected without issues",
			response:      spec.ToolResponse[string]{},
			expectedError: "rejected response must contain at least one issue",
		},
		{
			name:          "rejected with output",
			response:      spec.ToolResponse[string]{Output: &output, Issues: []spec.Issue{validIssue}},
			expectedError: "rejected response must not contain output",
		},
		{
			name:          "invalid issue",
			response:      spec.ToolResponse[string]{Issues: []spec.Issue{invalidIssue}},
			expectedError: "issue 0: issue message is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.response.Validate()
			if tt.expectedError == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, tt.expectedError)
		})
	}
}
