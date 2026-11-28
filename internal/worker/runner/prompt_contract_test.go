package runner

import (
	"strings"
	"testing"

	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
)

func TestBuildWorkRuntimeContractPromptContainerMode(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "podman",
		WorkingDir:     "/workspace",
		WritablePaths:  []string{"internal/**", "specs/**"},
		Artifacts: map[string]workerspec.Artifact{
			"summary": {Name: "summary", Type: "file", Path: "summary.md", Description: "Change summary.", Required: true, NonEmpty: true},
		},
		Imports: map[string]workerspec.ImportArtifact{
			"spec_bundle": {Name: "spec_bundle", Desc: "Spec bundle", Path: "bundle"},
		},
		TerminateTool: "sofa_complete_work",
		Fallbacks: []workerspec.FallbackConfig{
			{Name: "replan", On: []string{"agent_escalated"}, Target: "worker.plan"},
			{Name: "unrelated_fallback", On: []string{"qc_exhausted"}, Target: "worker.impl"},
		},
	})

	assert.Contains(t, prompt, "Working directory: /workspace")
	assert.Contains(t, prompt, "git add")
	assert.Contains(t, prompt, "git commit")
	assert.Contains(t, prompt, "git push")
	assert.Contains(t, prompt, "git checkout")
	assert.Contains(t, prompt, "git reset")
	assert.Contains(t, prompt, "git clean")
	assert.Contains(t, prompt, "Allowed writable paths")
	assert.Contains(t, prompt, "internal/**")
	assert.Contains(t, prompt, "specs/**")
	assert.Contains(t, prompt, ".git/")
	assert.Contains(t, prompt, ".sofa/")
	assert.NotContains(t, prompt, "`internal/**` ->")
	assert.Contains(t, prompt, "/.sofa/artifacts/summary.md")
	assert.Contains(t, prompt, "- spec_bundle: /.sofa/imports/bundle")
	assert.NotContains(t, prompt, "`spec_bundle` ->")
	assert.Contains(t, prompt, "Change summary.")
	assert.Contains(t, prompt, "sofa_checkpoint")
	assert.Contains(t, prompt, "sofa_complete_work")
	assert.Contains(t, prompt, "sofa_request_decision")
	assert.Contains(t, prompt, "sofa_escalate")
	assert.Contains(t, prompt, "replan")
	assert.NotContains(t, prompt, "unrelated_fallback")
}

func TestBuildWorkRuntimeContractPromptWithoutWritablePaths(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "docker",
		WorkingDir:     "/workspace",
		TerminateTool:  "sofa_complete_work",
	})

	assert.Contains(t, prompt, "ordinary workspace files")
	assert.Contains(t, prompt, ".git/")
	assert.NotContains(t, prompt, "Allowed writable paths")
	assert.NotContains(t, prompt, "Imported artifacts")
	assert.NotContains(t, prompt, "Declared output artifacts")
	assert.NotContains(t, prompt, "sofa_escalate")
}

func TestBuildWorkRuntimeContractPromptExplicitEmptyWritablePaths(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "podman",
		WorkingDir:     "/workspace",
		WritablePaths:  []string{},
		TerminateTool:  "sofa_complete_work",
	})

	assert.Contains(t, prompt, "ordinary workspace files")
	assert.Contains(t, prompt, ".git/")
	assert.NotContains(t, prompt, "Allowed writable paths")
	assert.NotContains(t, prompt, "`internal/**`")
}

func TestBuildWorkRuntimeContractPromptArtifactBooleanFlags(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "podman",
		WorkingDir:     "/workspace",
		Artifacts: map[string]workerspec.Artifact{
			"alpha":   {Name: "alpha", Type: "file", Path: "alpha.txt", Required: true, NonEmpty: true, Description: "Alpha output."},
			"bravo":   {Name: "bravo", Type: "file", Path: "bravo.txt", Required: true, NonEmpty: false},
			"charlie": {Name: "charlie", Type: "directory", Path: "charlie", Required: false, NonEmpty: true},
			"delta":   {Name: "delta", Type: "file", Path: "delta.txt", Required: false, NonEmpty: false},
		},
		TerminateTool: "sofa_complete_work",
	})

	assert.Contains(t, prompt,
		"- alpha (file, required=true, non_empty=true): /.sofa/artifacts/alpha.txt — Alpha output.")
	assert.Contains(t, prompt,
		"- bravo (file, required=true, non_empty=false): /.sofa/artifacts/bravo.txt")
	assert.Contains(t, prompt,
		"- charlie (directory, required=false, non_empty=true): /.sofa/artifacts/charlie")
	assert.Contains(t, prompt,
		"- delta (file, required=false, non_empty=false): /.sofa/artifacts/delta.txt")

	assert.Less(t,
		strings.Index(prompt, "- alpha "),
		strings.Index(prompt, "- bravo "),
		"artifacts must be listed sorted by name",
	)
}

func TestBuildWorkRuntimeContractPromptCustomTerminateTool(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "nerdctl",
		WorkingDir:     "/workspace",
		TerminateTool:  "submit_result",
	})

	assert.Contains(t, prompt, "submit_result")
	assert.NotContains(t, prompt, "sofa_complete_work")
}

func TestBuildWorkRuntimeContractPromptHostMode(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "host",
		WorkingDir:     "/tmp/sofa/run/workspace",
		WritablePaths:  []string{"internal/**"},
		Artifacts: map[string]workerspec.Artifact{
			"summary": {Name: "summary", Type: "file", Path: "summary.md", Description: "summary", Required: true},
		},
		Imports: map[string]workerspec.ImportArtifact{
			"bundle": {Name: "bundle", Path: "bundle"},
		},
		TerminateTool: "sofa_complete_work",
	})

	assert.Contains(t, prompt, "Working directory: /tmp/sofa/run/workspace")
	assert.Contains(t, prompt, ".sofa/artifacts/summary.md")
	assert.Contains(t, prompt, "- bundle: .sofa/imports/bundle")
	assert.NotContains(t, prompt, "`bundle` ->")
	assert.NotContains(t, prompt, "/.sofa/artifacts")
	assert.NotContains(t, prompt, "/.sofa/imports")
}

func TestBuildQCRuntimeContractPromptContainerMode(t *testing.T) {
	prompt := buildQCRuntimeContractPrompt(qcRuntimeContractInput{
		SandboxRuntime: "podman",
		WorkingDir:     "/workspace",
		Artifacts: map[string]workerspec.Artifact{
			"summary": {Name: "summary", Type: "file", Path: "summary.md", Description: "Change summary.", Required: true, NonEmpty: true},
			"report":  {Name: "report", Type: "file", Path: "report.json", Description: "Report.", Required: true},
		},
	})

	assert.Contains(t, prompt, "/workspace")
	assert.Contains(t, prompt, "/.sofa/artifacts/summary.md")
	assert.Contains(t, prompt, "/.sofa/artifacts/report.json")
	assert.Contains(t, prompt, "read-only")
	assert.Contains(t, prompt, "git status")
	assert.Contains(t, prompt, "git diff")
	assert.Contains(t, prompt, "sofa_qc_open_issues")
	assert.Contains(t, prompt, "sofa_qc_resolve_issues")
	assert.Contains(t, prompt, "sofa_qc_submit_verdict")
	assert.Contains(t, prompt, "pass")
	assert.Contains(t, prompt, "revise")
	assert.Less(t,
		strings.Index(prompt, "/.sofa/artifacts/report.json"),
		strings.Index(prompt, "/.sofa/artifacts/summary.md"),
		"artifacts must be listed sorted by name",
	)
}

func TestBuildQCRuntimeContractPromptHostMode(t *testing.T) {
	prompt := buildQCRuntimeContractPrompt(qcRuntimeContractInput{
		SandboxRuntime: "host",
		WorkingDir:     "/tmp/sofa/run/qc-workspace-r1",
		Artifacts: map[string]workerspec.Artifact{
			"summary": {Name: "summary", Type: "file", Path: "summary.md", Description: "summary"},
		},
	})

	assert.Contains(t, prompt, "/tmp/sofa/run/qc-workspace-r1")
	assert.Contains(t, prompt, ".sofa/artifacts/summary.md")
	assert.NotContains(t, prompt, "/.sofa/artifacts")
	assert.Contains(t, prompt, "sofa_qc_submit_verdict")
}

func TestBuildWorkRuntimeContractPromptEscalationNamesSortedAndDeduped(t *testing.T) {
	prompt := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "podman",
		WorkingDir:     "/workspace",
		TerminateTool:  "sofa_complete_work",
		Fallbacks: []workerspec.FallbackConfig{
			{Name: "zeta", On: []string{"agent_escalated"}, Target: "worker.zeta"},
			{Name: "alpha", On: []string{"agent_escalated"}, Target: "worker.alpha"},
			{Name: "alpha", On: []string{"agent_escalated"}, Target: "worker.alpha2"},
			{Name: "skipped", On: []string{"qc_exhausted"}, Target: "worker.skip"},
			{Name: "  ", On: []string{"agent_escalated"}, Target: "worker.blank"},
		},
	})

	assert.Contains(t, prompt, "Escalation fallbacks: `alpha`, `zeta`.")
	assert.NotContains(t, prompt, "`skipped`")
	assert.Less(t,
		strings.Index(prompt, "`alpha`"),
		strings.Index(prompt, "`zeta`"),
		"escalation fallbacks must be sorted by name",
	)
	// The duplicated alpha name must appear exactly once.
	assert.Equal(t, 1, strings.Count(prompt, "`alpha`"))
}

func TestComposeWorkSystemPromptOrdering(t *testing.T) {
	userPrompt := "USER DOMAIN PROMPT"
	diary := "RECOVERY DIARY CONTEXT"
	contract := buildWorkRuntimeContractPrompt(workRuntimeContractInput{
		SandboxRuntime: "podman",
		WorkingDir:     "/workspace",
		TerminateTool:  "sofa_complete_work",
	})

	composed := composeWorkSystemPrompt(userPrompt, diary, contract)
	assert.True(t, strings.HasPrefix(composed, userPrompt), "user prompt must lead")
	assert.Less(t, strings.Index(composed, userPrompt), strings.Index(composed, diary))
	assert.Less(t, strings.Index(composed, diary), strings.Index(composed, contract),
		"recovery diary must precede the host runtime contract")

	noDiary := composeWorkSystemPrompt(userPrompt, "", contract)
	assert.True(t, strings.HasPrefix(noDiary, userPrompt))
	assert.Less(t, strings.Index(noDiary, userPrompt), strings.Index(noDiary, contract))
	assert.NotContains(t, noDiary, "\n\n\n", "blank diary must not leave an empty section")
}
