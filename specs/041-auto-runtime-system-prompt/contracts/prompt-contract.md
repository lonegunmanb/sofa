# Contract: Runtime & Protocol System Prompt Rendering (`041`)

Internal Go contract (unexported, package `runner`). No network or serialized wire format.

## `buildWorkRuntimeContractPrompt(in workRuntimeContractInput) string`

Must return a non-empty Markdown-ish plain-text section that:

1. Names the working directory exactly once (`Working directory: <in.WorkingDir>`).
2. Contains the literal prohibition covering `git add`, `git commit`, `git push`, `git checkout`, `git reset`, `git clean`, and states that the sofa host manages checkpoint commits/branch pushes and that `.git/` and `.sofa/` control files must not be modified.
3. When `len(in.WritablePaths) > 0`, lists each glob verbatim; otherwise states ordinary workspace files may be edited but `.git/` and `.sofa/` control files are forbidden.
4. When imports exist, lists `- <name>: <importsRoot>/<relPath>` (containers `/.sofa/imports/...`, host `.sofa/imports/...`).
5. When artifacts exist (sorted by name), lists `- <name> (<type>, required=<bool>, non_empty=<bool>): <artifactsRoot>/<path> — <description>`.
6. Requires `sofa_checkpoint` milestones, requires ending with the terminate tool named by `in.TerminateTool` (default `sofa_complete_work`), and restricts `sofa_request_decision` to blocking ambiguity.
7. Only when `agent_escalated` fallbacks exist: lists the sorted, de-duplicated fallback **names** (the value `sofa_escalate` validates against `allowedFallbacks`) and requires `sofa_escalate`; non-`agent_escalated` fallbacks MUST NOT appear.

## `buildQCRuntimeContractPrompt(in qcRuntimeContractInput) string`

Must return a non-empty section that:

1. States the audit working directory (`in.WorkingDir`) and artifacts root (`<layout.ArtifactsRoot>`), read-only, forbidding any workspace/artifact modification and any Git write command.
2. When artifacts exist (sorted by name), lists each artifact path and description and points at `git status` / `git diff` for verification.
3. Requires `sofa_qc_open_issues` to open problems, `sofa_qc_resolve_issues` to close verified-fixed issues, and mandates ending with `sofa_qc_submit_verdict`; states `pass` requires zero open issues and `revise` requires at least one open issue.

## Path layout mapping

| Sandbox runtime | Workdir (caller-supplied) | Artifacts root | Imports root |
|-----------------|---------------------------|----------------|--------------|
| `host` (trimmed) | host path | `.sofa/artifacts` | `.sofa/imports` |
| anything else | `/workspace` | `/.sofa/artifacts` | `/.sofa/imports` |

## Integration contract

- `WorkerRunner` Work loop: `effectiveSysPrompt = appendPromptSection(effectiveSysPrompt, buildWorkRuntimeContractPrompt(...))` after the diary append, before constructing `workBaseSession`.
- `runQCRound`: `SystemPrompt: appendPromptSection(qcAuditSentence, buildQCRuntimeContractPrompt(...))`.