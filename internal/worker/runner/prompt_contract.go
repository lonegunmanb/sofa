package runner

import (
	"fmt"
	"path"
	"sort"
	"strings"

	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/lonegunmanb/sofa/internal/session"
	toolruntime "github.com/lonegunmanb/sofa/internal/tool/runtime"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

// HostSandboxRuntime is the sandbox runtime name that runs agent sessions directly
// on the host instead of inside a container.
const HostSandboxRuntime = "host"

const (
	hostArtifactsRoot = ".sofa/artifacts"
	hostImportsRoot   = ".sofa/imports"

	agentEscalatedTrigger = "agent_escalated"

	// qcAuditSystemPrompt is the domain-level QC instruction; the host-owned
	// read-only runtime contract is appended after it.
	qcAuditSystemPrompt = "Perform an independent, read-only quality control audit of /workspace and /.sofa/artifacts against all declared criteria."
)

// promptPathLayout resolves the workspace-relative layout an agent observes: the
// container mount layout (absolute /workspace, /.sofa/...) or the host layout
// (host workspace directory with .sofa/... relative to it).
type promptPathLayout struct {
	workingDir    string
	artifactsRoot string
	importsRoot   string
}

func newPromptPathLayout(sandboxRuntime, hostWorkingDir string) promptPathLayout {
	if strings.EqualFold(strings.TrimSpace(sandboxRuntime), HostSandboxRuntime) {
		workingDir := strings.TrimSpace(hostWorkingDir)
		if workingDir == "" {
			workingDir = "."
		}
		return promptPathLayout{
			workingDir:    workingDir,
			artifactsRoot: hostArtifactsRoot,
			importsRoot:   hostImportsRoot,
		}
	}
	return promptPathLayout{
		workingDir:    sandboxruntime.DefaultContainerWorkingDir,
		artifactsRoot: workerspec.ContainerArtifactsRoot,
		importsRoot:   workerspec.ContainerImportsRoot,
	}
}

// terminateToolName resolves the tool that ends a Work turn: the configured
// terminate binding when present, otherwise the default protocol tool.
func terminateToolName(bindings []toolruntime.ToolBinding) string {
	for _, b := range bindings {
		if !b.Terminate {
			continue
		}
		if name := strings.TrimSpace(b.EffectiveName()); name != "" {
			return name
		}
	}
	return session.ToolSofaCompleteWork
}

func (l promptPathLayout) artifactPath(art workerspec.Artifact) string {
	rel := strings.TrimSpace(art.Path)
	if rel == "" {
		rel = art.Name
	}
	return path.Join(l.artifactsRoot, rel)
}

func (l promptPathLayout) importPath(imported workerspec.ImportArtifact) string {
	return path.Join(l.importsRoot, workerspec.EffectiveImportRelPath(imported))
}

// workRuntimeContractInput captures the host-owned runtime facts rendered into
// each Work session's system prompt.
type workRuntimeContractInput struct {
	SandboxRuntime string
	WorkingDir     string
	WritablePaths  []string
	Artifacts      map[string]workerspec.Artifact
	Imports        map[string]workerspec.ImportArtifact
	TerminateTool  string
	Fallbacks      []workerspec.FallbackConfig
}

// qcRuntimeContractInput captures the host-owned runtime facts rendered into
// each QC session's system prompt.
type qcRuntimeContractInput struct {
	SandboxRuntime string
	WorkingDir     string
	Artifacts      map[string]workerspec.Artifact
}

// buildWorkRuntimeContractPrompt renders the host-owned runtime environment and
// protocol tool contract appended to the Work system prompt.
func buildWorkRuntimeContractPrompt(in workRuntimeContractInput) string {
	layout := newPromptPathLayout(in.SandboxRuntime, in.WorkingDir)
	terminateTool := strings.TrimSpace(in.TerminateTool)
	if terminateTool == "" {
		terminateTool = session.ToolSofaCompleteWork
	}

	var b strings.Builder
	b.WriteString("## Runtime & Protocol Constraints\n\n")
	b.WriteString("### Working directory and Git ownership\n")
	fmt.Fprintf(&b, "- Working directory: %s\n", layout.workingDir)
	b.WriteString("- NEVER run git add, git commit, git push, git checkout, git reset, or git clean. " +
		"The sofa host manages Git checkpoints, branch creation, and pushes automatically; " +
		"only write ordinary files and let the host capture them.\n")

	b.WriteString("\n### Write boundaries\n")
	if writable := compactStrings(in.WritablePaths); len(writable) > 0 {
		b.WriteString("- Allowed writable paths (workspace-relative globs; modify nothing else):\n")
		for _, p := range writable {
			fmt.Fprintf(&b, "  - `%s`\n", p)
		}
		b.WriteString("- You MUST NOT touch `.git/` or `.sofa/` control files even when they " +
			"appear to match an allowed glob.\n")
	} else {
		b.WriteString("- You may create or modify ordinary workspace files, but you MUST NOT touch " +
			"`.git/` or `.sofa/` internal control files.\n")
	}

	if imports := sortedImports(in.Imports); len(imports) > 0 {
		b.WriteString("\n### Imported artifacts (read-only)\n")
		for _, imported := range imports {
			line := fmt.Sprintf("- %s: %s", imported.Name, layout.importPath(imported))
			if desc := strings.TrimSpace(imported.Desc); desc != "" {
				line += " — " + desc
			}
			b.WriteString(line + "\n")
		}
	}

	if arts := sortedArtifacts(in.Artifacts); len(arts) > 0 {
		b.WriteString("\n### Declared output artifacts\n")
		for _, art := range arts {
			b.WriteString(renderArtifactLine(art, layout.artifactPath(art)))
		}
	}

	b.WriteString("\n### Protocol tools\n")
	fmt.Fprintf(&b, "- At key milestones call `%s` with a structured progress diary (`completed`, `next_steps`).\n",
		session.ToolSofaCheckpoint)
	fmt.Fprintf(&b, "- Once all work and declared artifacts are written and self-verified, you MUST call `%s` "+
		"to end the turn. Never stop with plain text only.\n", terminateTool)
	fmt.Fprintf(&b, "- Call `%s` only when blocked on requirement ambiguity or human sign-off.\n",
		session.ToolSofaRequestDecision)
	if fallbacks := agentEscalatedFallbackNames(in.Fallbacks); len(fallbacks) > 0 {
		fmt.Fprintf(&b, "- Escalation fallbacks: %s. Call `%s` with the chosen fallback name when the task exceeds your scope.\n",
			formatQuotedList(fallbacks), session.ToolSofaEscalate)
	}

	return strings.TrimRight(b.String(), "\n")
}

// buildQCRuntimeContractPrompt renders the host-owned read-only audit environment
// and QC protocol tool contract appended to the QC system prompt.
func buildQCRuntimeContractPrompt(in qcRuntimeContractInput) string {
	layout := newPromptPathLayout(in.SandboxRuntime, in.WorkingDir)

	var b strings.Builder
	b.WriteString("## Read-Only Audit Environment\n\n")
	fmt.Fprintf(&b, "- Working directory: %s (mounted read-only)\n", layout.workingDir)
	fmt.Fprintf(&b, "- Artifacts root: %s (read-only)\n", layout.artifactsRoot)
	b.WriteString("- Do not modify any workspace or artifact file and never run Git write commands. " +
		"Use `git status` and `git diff` to inspect the delivered changes.\n")

	if arts := sortedArtifacts(in.Artifacts); len(arts) > 0 {
		b.WriteString("\n### Artifacts under review\n")
		for _, art := range arts {
			b.WriteString(renderArtifactLine(art, layout.artifactPath(art)))
		}
	}

	b.WriteString("\n### QC protocol tools\n")
	fmt.Fprintf(&b, "- Record every problem you find with `%s`.\n", session.ToolSofaQCOpenIssues)
	fmt.Fprintf(&b, "- Re-check repaired problems with `%s`.\n", session.ToolSofaQCResolveIssues)
	fmt.Fprintf(&b, "- You MUST call `%s` to end the round: `pass` requires zero open issues; "+
		"`revise` requires at least one open issue.\n", session.ToolSofaQCSubmitVerdict)

	return strings.TrimRight(b.String(), "\n")
}

// composeWorkSystemPrompt assembles the final Work system prompt in the frozen
// order: user prompt, then the recovery diary (when present), then the
// host-owned runtime/protocol contract. Blank sections are skipped.
func composeWorkSystemPrompt(userPrompt, diary, contract string) string {
	return appendPromptSection(appendPromptSection(userPrompt, diary), contract)
}

// renderArtifactLine renders a single artifact declaration in the frozen
// contract shape: "- <name> (<type>, required=<bool>, non_empty=<bool>): <path>"
// with an optional " — <description>" suffix.
func renderArtifactLine(art workerspec.Artifact, artifactPath string) string {
	artType := strings.TrimSpace(art.Type)
	if artType == "" {
		artType = "file"
	}
	line := fmt.Sprintf("- %s (%s, required=%t, non_empty=%t): %s",
		art.Name, artType, art.Required, art.NonEmpty, artifactPath)
	if desc := strings.TrimSpace(art.Description); desc != "" {
		line += " — " + desc
	}
	return line + "\n"
}

func sortedArtifacts(arts map[string]workerspec.Artifact) []workerspec.Artifact {
	out := make([]workerspec.Artifact, 0, len(arts))
	for name, art := range arts {
		if strings.TrimSpace(art.Name) == "" {
			art.Name = name
		}
		out = append(out, art)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedImports(imports map[string]workerspec.ImportArtifact) []workerspec.ImportArtifact {
	out := make([]workerspec.ImportArtifact, 0, len(imports))
	for name, imported := range imports {
		if strings.TrimSpace(imported.Name) == "" {
			imported.Name = name
		}
		out = append(out, imported)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func agentEscalatedFallbackNames(fallbacks []workerspec.FallbackConfig) []string {
	var names []string
	seen := make(map[string]struct{}, len(fallbacks))
	for _, fb := range fallbacks {
		if !containsString(fb.On, agentEscalatedTrigger) {
			continue
		}
		name := strings.TrimSpace(fb.Name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatQuotedList(targets []string) string {
	quoted := make([]string, 0, len(targets))
	for _, t := range targets {
		quoted = append(quoted, fmt.Sprintf("`%s`", t))
	}
	return strings.Join(quoted, ", ")
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
