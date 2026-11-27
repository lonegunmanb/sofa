package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lonegunmanb/sofa/internal/config"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/plan"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	toolspec "github.com/lonegunmanb/sofa/internal/tool/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// Issue aliases sofa's canonical structured validation issue.
type Issue = corespec.Issue

// ContainerExecFunc executes a command inside the active sandbox container with stdin JSON
// and returns stdout, stderr, and the process exit code.
type ContainerExecFunc func(ctx context.Context, workDir string, command []string, env map[string]string, stdin []byte) (stdout string, stderr string, exitCode int, err error)

// NewPodmanContainerExec adapts sandboxruntime.Engine.Exec into a ContainerExecFunc
// targeting an already-running container.
func NewPodmanContainerExec(engine sandboxruntime.Engine, containerName string) ContainerExecFunc {
	return func(ctx context.Context, workDir string, command []string, env map[string]string, stdin []byte) (string, string, int, error) {
		if engine == nil {
			return "", "", -1, errors.New("sandbox engine is nil")
		}
		var stdinReader io.Reader
		if len(stdin) > 0 {
			stdinReader = bytes.NewReader(stdin)
		}
		res, err := engine.Exec(ctx, containerName, workDir, command, env, stdinReader)
		return res.Stdout, res.Stderr, res.ExitCode, err
	}
}

// ToolBinding binds a planned ToolSpec with optional tool_use alias/preset input,
// terminate semantics, external_tool env/env_refs, and execution timeout.
type ToolBinding struct {
	Spec            plan.ToolSpec
	ExposedName     string
	PresetInput     cty.Value
	PresetInputJSON json.RawMessage
	Terminate       bool
	Env             map[string]string
	EnvRefs         map[string]string
	Timeout         time.Duration
}

// EffectiveName returns ExposedName when set (tool_use alias) or Spec.ID otherwise.
func (b ToolBinding) EffectiveName() string {
	if strings.TrimSpace(b.ExposedName) != "" {
		return strings.TrimSpace(b.ExposedName)
	}
	return b.Spec.ID
}

// InvocationResult captures both the harness ToolOutput and structured host metadata
// for downstream session gates (016) and worker result extraction (018).
type InvocationResult struct {
	ToolOutput      harnessruntime.ToolOutput
	Accepted        bool
	Terminate       bool
	ValidatedInput  cty.Value
	ValidatedOutput cty.Value
	OutputJSON      json.RawMessage
	Issues          []Issue
}

// ExecutorOptions configures an Executor instance for a worker or QC session.
type ExecutorOptions struct {
	WorkspaceDir string
	ImportsDir   string
	BundleDir    string
	Exec         ContainerExecFunc
	LookupEnv    func(string) (string, bool)
	Functions    map[string]function.Function
}

// Executor validates inputs, executes go_tool/external_tool inside containers,
// verifies QuotePaths against host files, and evaluates HCL postconditions.
type Executor struct {
	workspaceDir string
	importsDir   string
	bundleDir    string
	execFn       ContainerExecFunc
	lookupEnv    func(string) (string, bool)
	functions    map[string]function.Function
}

// NewExecutor constructs an Executor with defaults for LookupEnv and config.Functions.
func NewExecutor(opts ExecutorOptions) *Executor {
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	funcs := opts.Functions
	if funcs == nil {
		funcs = config.Functions(opts.WorkspaceDir)
	}
	return &Executor{
		workspaceDir: opts.WorkspaceDir,
		importsDir:   opts.ImportsDir,
		bundleDir:    opts.BundleDir,
		execFn:       opts.Exec,
		lookupEnv:    lookupEnv,
		functions:    funcs,
	}
}

// HostToolSpec builds the HostToolSpec exposed to the LLM, stripping any top-level
// properties already locked by tool_use.input.
func HostToolSpec(binding ToolBinding) (harnessruntime.HostToolSpec, error) {
	name := binding.EffectiveName()
	if strings.TrimSpace(name) == "" {
		return harnessruntime.HostToolSpec{}, errors.New("tool exposed name or id is required")
	}

	var schemaObj map[string]any
	if len(bytes.TrimSpace(binding.Spec.InputSchema)) > 0 {
		if err := json.Unmarshal(binding.Spec.InputSchema, &schemaObj); err != nil {
			return harnessruntime.HostToolSpec{}, fmt.Errorf("parse input_schema for %q: %w", name, err)
		}
	} else if len(bytes.TrimSpace(binding.Spec.InputType)) > 0 {
		inputType, err := ctyjson.UnmarshalType(binding.Spec.InputType)
		if err != nil {
			return harnessruntime.HostToolSpec{}, fmt.Errorf("unmarshal input_type for %q: %w", name, err)
		}
		schemaObj, err = toolspec.JSONSchema(inputType)
		if err != nil {
			return harnessruntime.HostToolSpec{}, fmt.Errorf("derive input_schema for %q: %w", name, err)
		}
	} else {
		schemaObj = map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		}
	}

	presetMap, err := resolvePresetInputMap(binding)
	if err != nil {
		return harnessruntime.HostToolSpec{}, fmt.Errorf("resolve preset input for %q: %w", name, err)
	}
	if len(presetMap) > 0 {
		stripPresetFieldsFromSchema(schemaObj, presetMap)
	}

	schemaBytes, err := json.Marshal(schemaObj)
	if err != nil {
		return harnessruntime.HostToolSpec{}, fmt.Errorf("marshal input_schema for %q: %w", name, err)
	}

	return harnessruntime.HostToolSpec{
		Name:                 name,
		Description:          binding.Spec.Description,
		ParametersJSONSchema: schemaBytes,
	}, nil
}

func stripPresetFieldsFromSchema(schemaObj map[string]any, preset map[string]any) {
	props, _ := schemaObj["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	} else {
		props = maps.Clone(props)
		for k := range preset {
			delete(props, k)
		}
	}
	schemaObj["properties"] = props
	schemaObj["additionalProperties"] = false

	if rawReq, ok := schemaObj["required"]; ok {
		filtered := make([]string, 0)
		switch items := rawReq.(type) {
		case []any:
			for _, item := range items {
				s, ok := item.(string)
				if ok {
					if _, locked := preset[s]; !locked {
						filtered = append(filtered, s)
					}
				}
			}
		case []string:
			for _, s := range items {
				if _, locked := preset[s]; !locked {
					filtered = append(filtered, s)
				}
			}
		}
		if len(filtered) == 0 {
			delete(schemaObj, "required")
		} else {
			schemaObj["required"] = filtered
		}
	}
}

// Handler returns a harnessruntime.ToolHandler bound to binding.
func (e *Executor) Handler(binding ToolBinding) harnessruntime.ToolHandler {
	return func(ctx context.Context, call harnessruntime.ToolInvocation) (harnessruntime.ToolOutput, error) {
		res, err := e.Invoke(ctx, binding, call.ArgumentsJSON)
		if err != nil {
			return res.ToolOutput, err
		}
		return res.ToolOutput, nil
	}
}

// Invoke executes one typed tool call end-to-end.
func (e *Executor) Invoke(ctx context.Context, binding ToolBinding, argumentsJSON []byte) (InvocationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	inputType, err := ctyjson.UnmarshalType(binding.Spec.InputType)
	if err != nil {
		return failureResult(fmt.Sprintf("invalid tool input_type %q: %v", binding.EffectiveName(), err)), nil
	}
	outputType, err := ctyjson.UnmarshalType(binding.Spec.OutputType)
	if err != nil {
		return failureResult(fmt.Sprintf("invalid tool output_type %q: %v", binding.EffectiveName(), err)), nil
	}

	llmArgs, err := parseStrictJSONObject(argumentsJSON)
	if err != nil {
		return modelRejectionResult(binding.Terminate, cty.NilVal, []Issue{{
			Code:    "invalid_input",
			Message: err.Error(),
		}}), nil
	}

	presetMap, err := resolvePresetInputMap(binding)
	if err != nil {
		return failureResult(fmt.Sprintf("invalid preset input for %q: %v", binding.EffectiveName(), err)), nil
	}
	for k, v := range presetMap {
		llmArgs[k] = v
	}

	mergedJSON, err := json.Marshal(llmArgs)
	if err != nil {
		return failureResult(fmt.Sprintf("marshal merged input for %q: %v", binding.EffectiveName(), err)), nil
	}
	rawInput, err := ctyjson.Unmarshal(mergedJSON, inputType)
	if err != nil {
		return modelRejectionResult(binding.Terminate, cty.NilVal, []Issue{{
			Code:    "invalid_input",
			Message: fmt.Sprintf("input does not match schema: %v", err),
		}}), nil
	}
	validatedInput, err := toolspec.NewConstraint(inputType).Apply(rawInput)
	if err != nil {
		return modelRejectionResult(binding.Terminate, cty.NilVal, []Issue{{
			Code:    "invalid_input",
			Message: fmt.Sprintf("input constraint validation failed: %v", err),
		}}), nil
	}
	canonicalInputJSON, err := ctyjson.Marshal(validatedInput, inputType)
	if err != nil {
		return failureResult(fmt.Sprintf("marshal validated input for %q: %v", binding.EffectiveName(), err)), nil
	}

	if e.execFn == nil {
		return failureResult(fmt.Sprintf("container exec function is not configured for tool %q", binding.EffectiveName())), nil
	}

	execCtx := ctx
	var cancel context.CancelFunc
	if binding.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, binding.Timeout)
		defer cancel()
	}

	var validatedOutput cty.Value
	var outputJSON json.RawMessage
	var secrets []string

	switch binding.Spec.Kind {
	case "go":
		cmd := []string{containerToolsDir + "/" + binding.Spec.ID}
		stdout, stderr, exitCode, execErr := e.execFn(execCtx, sandboxruntime.DefaultContainerWorkingDir, cmd, nil, canonicalInputJSON)
		if execErr != nil {
			return failureResult(fmt.Sprintf("go_tool %s execution error: %v", binding.EffectiveName(), execErr)), nil
		}
		if exitCode != 0 {
			errMsg := strings.TrimSpace(stderr)
			if errMsg == "" {
				errMsg = strings.TrimSpace(stdout)
			}
			return failureResult(fmt.Sprintf("go_tool %s exited with code %d: %s", binding.EffectiveName(), exitCode, errMsg)), nil
		}

		rawResp, err := parseStrictGoToolResponse(stdout)
		if err != nil {
			return failureResult(fmt.Sprintf("go_tool %s invalid response: %v", binding.EffectiveName(), err)), nil
		}
		if !*rawResp.Accepted {
			return modelRejectionResult(binding.Terminate, validatedInput, rawResp.Issues), nil
		}

		rawOut, err := ctyjson.Unmarshal(rawResp.Output, outputType)
		if err != nil {
			return failureResult(fmt.Sprintf("go_tool %s output type mismatch: %v", binding.EffectiveName(), err)), nil
		}
		validatedOutput, err = toolspec.NewConstraint(outputType).Apply(rawOut)
		if err != nil {
			return failureResult(fmt.Sprintf("go_tool %s output constraint failed: %v", binding.EffectiveName(), err)), nil
		}
		outputJSON, err = ctyjson.Marshal(validatedOutput, outputType)
		if err != nil {
			return failureResult(fmt.Sprintf("go_tool %s marshal output: %v", binding.EffectiveName(), err)), nil
		}

	case "external":
		var env map[string]string
		env, secrets, err = materializeExternalToolEnv(binding.Env, binding.EnvRefs, e.lookupEnv)
		if err != nil {
			return failureResult(redactSecrets(err.Error(), secrets)), nil
		}
		workDir, err := sandboxruntime.ResolveContainerWorkDir(binding.Spec.WorkingDir)
		if err != nil {
			return failureResult(fmt.Sprintf("external_tool %s working_dir: %v", binding.EffectiveName(), err)), nil
		}
		if len(binding.Spec.Program) == 0 {
			return failureResult(fmt.Sprintf("external_tool %s program is empty", binding.EffectiveName())), nil
		}

		cmd := sandboxruntime.RelocateBundleCommandArgs(binding.Spec.Program, e.bundleDir, e.workspaceDir)
		stdout, stderr, exitCode, execErr := e.execFn(execCtx, workDir, cmd, env, canonicalInputJSON)
		if execErr != nil {
			return failureResult(redactSecrets(fmt.Sprintf("external_tool %s execution error: %v (%s)", binding.EffectiveName(), execErr, strings.TrimSpace(stderr)), secrets)), nil
		}
		if exitCode != 0 {
			errMsg := strings.TrimSpace(stderr)
			if errMsg == "" {
				errMsg = strings.TrimSpace(stdout)
			}
			return failureResult(redactSecrets(fmt.Sprintf("external_tool %s exited with code %d: %s", binding.EffectiveName(), exitCode, errMsg), secrets)), nil
		}

		rawOutJSON, err := parseSingleJSONValue(stdout)
		if err != nil {
			return failureResult(redactSecrets(fmt.Sprintf("external_tool %s invalid JSON stdout: %v", binding.EffectiveName(), err), secrets)), nil
		}
		rawOut, err := ctyjson.Unmarshal(rawOutJSON, outputType)
		if err != nil {
			return failureResult(redactSecrets(fmt.Sprintf("external_tool %s output type mismatch: %v", binding.EffectiveName(), err), secrets)), nil
		}
		validatedOutput, err = toolspec.NewConstraint(outputType).Apply(rawOut)
		if err != nil {
			return failureResult(redactSecrets(fmt.Sprintf("external_tool %s output constraint failed: %v", binding.EffectiveName(), err), secrets)), nil
		}
		outputJSON, err = ctyjson.Marshal(validatedOutput, outputType)
		if err != nil {
			return failureResult(redactSecrets(fmt.Sprintf("external_tool %s marshal output: %v", binding.EffectiveName(), err), secrets)), nil
		}

	default:
		return failureResult(fmt.Sprintf("unsupported tool kind %q", binding.Spec.Kind)), nil
	}

	if binding.Spec.Kind == "go" && len(binding.Spec.QuotePaths) > 0 {
		if issues := e.verifyQuotePaths(validatedOutput, binding.Spec.QuotePaths); len(issues) > 0 {
			return modelRejectionResult(binding.Terminate, validatedInput, issues), nil
		}
	}

	if len(binding.Spec.Postconditions) > 0 {
		vars := map[string]cty.Value{
			"input":  validatedInput,
			"output": validatedOutput,
		}
		for _, cond := range binding.Spec.Postconditions {
			known, evalErr := cond.Evaluate(vars, e.functions)
			if !known || evalErr != nil {
				msg := strings.TrimSpace(cond.ErrorMessage)
				if msg == "" && evalErr != nil {
					msg = evalErr.Error()
				}
				return modelRejectionResult(binding.Terminate, validatedInput, []Issue{{
					Code:    "postcondition_failed",
					Message: msg,
				}}), nil
			}
		}
	}

	var llmText string
	if binding.Spec.Kind == "go" {
		envelope, err := json.Marshal(struct {
			Accepted bool            `json:"accepted"`
			Output   json.RawMessage `json:"output"`
		}{
			Accepted: true,
			Output:   outputJSON,
		})
		if err != nil {
			return failureResult(fmt.Sprintf("marshal accepted response for %q: %v", binding.EffectiveName(), err)), nil
		}
		llmText = string(envelope)
	} else {
		llmText = string(outputJSON)
	}

	return InvocationResult{
		ToolOutput: harnessruntime.ToolOutput{
			ResultType:       harnessruntime.HostToolResultSuccess,
			TextResultForLLM: llmText,
		},
		Accepted:        true,
		Terminate:       binding.Terminate,
		ValidatedInput:  validatedInput,
		ValidatedOutput: validatedOutput,
		OutputJSON:      outputJSON,
	}, nil
}

type rawGoToolResponse struct {
	Accepted *bool           `json:"accepted"`
	Output   json.RawMessage `json:"output"`
	Issues   []Issue         `json:"issues"`
}

func parseStrictGoToolResponse(stdout string) (rawGoToolResponse, error) {
	rawVal, err := parseSingleJSONValue(stdout)
	if err != nil {
		return rawGoToolResponse{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(rawVal))
	dec.DisallowUnknownFields()
	var resp rawGoToolResponse
	if err := dec.Decode(&resp); err != nil {
		return rawGoToolResponse{}, err
	}
	if resp.Accepted == nil {
		return rawGoToolResponse{}, errors.New("missing required 'accepted' field")
	}

	trimmedOut := bytes.TrimSpace(resp.Output)
	hasOutput := len(trimmedOut) > 0 && !bytes.Equal(trimmedOut, []byte("null"))
	if *resp.Accepted {
		if len(resp.Issues) != 0 {
			return rawGoToolResponse{}, errors.New("accepted response must not contain issues")
		}
		if !hasOutput {
			return rawGoToolResponse{}, errors.New("accepted response must contain non-null output")
		}
	} else {
		if hasOutput {
			return rawGoToolResponse{}, errors.New("rejected response must not contain output")
		}
		if len(resp.Issues) == 0 {
			return rawGoToolResponse{}, errors.New("rejected response must contain at least one issue")
		}
		for i, issue := range resp.Issues {
			if err := issue.Validate(); err != nil {
				return rawGoToolResponse{}, fmt.Errorf("issue %d: %w", i, err)
			}
		}
	}
	return resp, nil
}

func parseSingleJSONValue(rawText string) (json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(rawText))
	dec.UseNumber()
	var val json.RawMessage
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("unexpected trailing tokens after JSON value")
	}
	return bytes.TrimSpace(val), nil
}

func parseStrictJSONObject(raw []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}
	val, err := parseSingleJSONValue(string(trimmed))
	if err != nil {
		return nil, fmt.Errorf("invalid JSON arguments: %w", err)
	}
	if len(val) == 0 || val[0] != '{' {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(val))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("invalid JSON object arguments: %w", err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

func resolvePresetInputMap(binding ToolBinding) (map[string]any, error) {
	if len(bytes.TrimSpace(binding.PresetInputJSON)) > 0 {
		return parseStrictJSONObject(binding.PresetInputJSON)
	}
	if binding.PresetInput == cty.NilVal {
		return nil, nil
	}
	unmarked, _ := binding.PresetInput.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, errors.New("preset input must be wholly known")
	}
	raw, err := ctyjson.Marshal(unmarked, unmarked.Type())
	if err != nil {
		return nil, err
	}
	return parseStrictJSONObject(raw)
}

func materializeExternalToolEnv(staticEnv, envRefs map[string]string, lookupEnv func(string) (string, bool)) (map[string]string, []string, error) {
	out := make(map[string]string, len(staticEnv)+len(envRefs))
	for k, v := range staticEnv {
		out[k] = v
	}
	secrets := make([]string, 0, len(envRefs))
	for dstKey, srcEnv := range envRefs {
		if strings.TrimSpace(dstKey) == "" || strings.TrimSpace(srcEnv) == "" {
			return nil, secrets, fmt.Errorf("external_tool env_refs[%q] (%q) must not be empty", dstKey, srcEnv)
		}
		val, ok := lookupEnv(srcEnv)
		if !ok || strings.TrimSpace(val) == "" {
			return nil, secrets, fmt.Errorf("external_tool env_refs[%q] (%q) is not set or empty", dstKey, srcEnv)
		}
		out[dstKey] = val
		secrets = append(secrets, val)
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	return out, secrets, nil
}

func redactSecrets(text string, secrets []string) string {
	redacted := text
	for _, secret := range secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	return redacted
}

type extractedQuote struct {
	Path       string
	ExactQuote string
}

func (e *Executor) verifyQuotePaths(outputVal cty.Value, quotePaths [][]string) []Issue {
	var quotes []extractedQuote
	for _, qp := range quotePaths {
		extracted, issue := collectQuotesAlongPath(outputVal, qp, strings.Join(qp, "."))
		if issue != nil {
			return []Issue{*issue}
		}
		quotes = append(quotes, extracted...)
	}

	for _, q := range quotes {
		if err := e.verifySingleQuote(q); err != nil {
			p := q.Path
			return []Issue{{
				Code:    "quote_verification_failed",
				Message: err.Error(),
				Path:    &p,
			}}
		}
	}
	return nil
}

func collectQuotesAlongPath(val cty.Value, steps []string, label string) ([]extractedQuote, *Issue) {
	if val == cty.NilVal || val.IsNull() {
		return nil, nil
	}
	if len(steps) == 0 {
		vt := val.Type()
		if !vt.IsObjectType() && !vt.IsMapType() {
			return nil, quoteIssue(label, "quote entry must be an object with path and exact_quote")
		}
		m := val.AsValueMap()
		pathVal, okP := m["path"]
		quoteVal, okQ := m["exact_quote"]
		if !okP || !okQ || pathVal.IsNull() || quoteVal.IsNull() ||
			!pathVal.Type().Equals(cty.String) || !quoteVal.Type().Equals(cty.String) {
			return nil, quoteIssue(label, "quote entry must contain string 'path' and 'exact_quote'")
		}
		return []extractedQuote{{
			Path:       pathVal.AsString(),
			ExactQuote: quoteVal.AsString(),
		}}, nil
	}

	step := steps[0]
	rest := steps[1:]
	vt := val.Type()

	switch step {
	case "[]":
		if !vt.IsListType() && !vt.IsTupleType() && !vt.IsSetType() {
			return nil, quoteIssue(label, fmt.Sprintf("expected list/tuple/set at %s", label))
		}
		var out []extractedQuote
		for it := val.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			sub, issue := collectQuotesAlongPath(elem, rest, label)
			if issue != nil {
				return nil, issue
			}
			out = append(out, sub...)
		}
		return out, nil

	case "*":
		if !vt.IsMapType() && !vt.IsObjectType() {
			return nil, quoteIssue(label, fmt.Sprintf("expected map/object at %s", label))
		}
		var out []extractedQuote
		m := val.AsValueMap()
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, issue := collectQuotesAlongPath(m[k], rest, label)
			if issue != nil {
				return nil, issue
			}
			out = append(out, sub...)
		}
		return out, nil

	default:
		if !vt.IsObjectType() && !vt.IsMapType() {
			return nil, quoteIssue(label, fmt.Sprintf("expected object/map at %s", label))
		}
		m := val.AsValueMap()
		child, exists := m[step]
		if !exists || child.IsNull() {
			return nil, nil
		}
		return collectQuotesAlongPath(child, rest, label)
	}
}

func quoteIssue(quotePath, msg string) *Issue {
	return &Issue{
		Code:    "quote_verification_failed",
		Message: msg,
		Path:    &quotePath,
	}
}

func (e *Executor) verifySingleQuote(q extractedQuote) error {
	if q.ExactQuote == "" {
		return fmt.Errorf("exact_quote for %q must not be empty", q.Path)
	}
	rootDir, relPath, err := e.resolveQuoteRootAndRel(q.Path)
	if err != nil {
		return err
	}
	if rootDir == "" {
		return fmt.Errorf("host root directory is not configured for quote path %q", q.Path)
	}

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return fmt.Errorf("open root %q for quote %q: %w", rootDir, q.Path, err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(relPath)
	if err != nil {
		return fmt.Errorf("quote file %q not accessible: %w", q.Path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("quote path %q must not be a symlink", q.Path)
	}
	if info.IsDir() {
		return fmt.Errorf("quote path %q must be a regular file", q.Path)
	}

	f, err := root.Open(relPath)
	if err != nil {
		return fmt.Errorf("open quote file %q: %w", q.Path, err)
	}
	defer func() { _ = f.Close() }()

	content, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read quote file %q: %w", q.Path, err)
	}
	if !utf8.Valid(content) {
		return fmt.Errorf("quote file %q is not valid UTF-8", q.Path)
	}
	if !strings.Contains(string(content), q.ExactQuote) {
		return fmt.Errorf("exact_quote not found in %q", q.Path)
	}
	return nil
}

func (e *Executor) resolveQuoteRootAndRel(rawPath string) (string, string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", "", errors.New("quote path must not be empty")
	}
	if strings.Contains(trimmed, `\`) {
		return "", "", fmt.Errorf("quote path %q must use '/' separators", rawPath)
	}

	var hostRoot string
	var relCandidate string
	switch {
	case strings.HasPrefix(trimmed, "/.sofa/imports/"):
		hostRoot = e.importsDir
		relCandidate = strings.TrimPrefix(trimmed, "/.sofa/imports/")
	case strings.HasPrefix(trimmed, "/.sofa/bundle/"):
		hostRoot = e.bundleDir
		relCandidate = strings.TrimPrefix(trimmed, "/.sofa/bundle/")
	case strings.HasPrefix(trimmed, "/workspace/"):
		hostRoot = e.workspaceDir
		relCandidate = strings.TrimPrefix(trimmed, "/workspace/")
	case strings.HasPrefix(trimmed, "/"):
		return "", "", fmt.Errorf("quote path %q is outside /workspace, /.sofa/imports, and /.sofa/bundle", rawPath)
	default:
		hostRoot = e.workspaceDir
		relCandidate = trimmed
	}

	cleaned := path.Clean(relCandidate)
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", "", fmt.Errorf("invalid quote relative path %q", rawPath)
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".git" || seg == ".." {
			return "", "", fmt.Errorf("quote path %q references forbidden segment %q", rawPath, seg)
		}
	}
	return hostRoot, cleaned, nil
}

func modelRejectionResult(terminate bool, validatedInput cty.Value, issues []Issue) InvocationResult {
	envelope, _ := json.Marshal(struct {
		Accepted bool    `json:"accepted"`
		Issues   []Issue `json:"issues"`
	}{
		Accepted: false,
		Issues:   issues,
	})
	return InvocationResult{
		ToolOutput: harnessruntime.ToolOutput{
			ResultType:       harnessruntime.HostToolResultSuccess,
			TextResultForLLM: string(envelope),
		},
		Accepted:       false,
		Terminate:      terminate,
		ValidatedInput: validatedInput,
		Issues:         issues,
	}
}

func failureResult(errMsg string) InvocationResult {
	return InvocationResult{
		ToolOutput: harnessruntime.ToolOutput{
			ResultType:       harnessruntime.HostToolResultFailure,
			TextResultForLLM: errMsg,
			HandlerError:     errMsg,
		},
		Accepted: false,
	}
}
