package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/module"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	"github.com/lonegunmanb/sofa/internal/project"
	"github.com/lonegunmanb/sofa/internal/variableschema"
	"github.com/spf13/cobra"
)

const (
	ExitSuccess   = 0
	ExitFailure   = 1
	ExitUsage     = 2
	ExitInterrupt = 130
)

// Options injects working directory, plugin fetchers, UI handler, and environment lookups for offline testing.
type Options struct {
	WorkingDir          string
	Fetch               module.FetchFunc
	ReleaseFetcher      harness.ReleaseFetcher
	AttestationVerifier harness.AttestationVerifier
	LookupEnv           func(string) (string, bool)
	UIHandler           http.Handler
}

type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// ExitCode maps CLI errors to standard process exit codes.
func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	return ExitFailure
}

// NormalizeArgs rewrites single-dash -var and -var-file flags to double-dash
// flags prior to the -- end-of-flags delimiter.
func NormalizeArgs(args []string) []string {
	normalized := append([]string(nil), args...)
	for i, arg := range normalized {
		if arg == "--" {
			break
		}
		if arg == "-var" || arg == "-var-file" ||
			strings.HasPrefix(arg, "-var=") || strings.HasPrefix(arg, "-var-file=") {
			normalized[i] = "-" + arg
		}
	}
	return normalized
}

// Run executes the sofa CLI with normalized arguments and returns the exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options) int {
	if ctx == nil {
		ctx = context.Background()
	}
	command := NewCommand(opts)
	command.SetArgs(NormalizeArgs(args))
	if stdin != nil {
		command.SetIn(stdin)
	}
	if stdout != nil {
		command.SetOut(stdout)
	}
	if stderr != nil {
		command.SetErr(stderr)
	}

	err := command.ExecuteContext(ctx)
	if err == nil {
		return ExitSuccess
	}
	if stderr != nil {
		writeError(stderr, err)
	}
	if errors.Is(ctx.Err(), context.Canceled) && errors.Is(err, context.Canceled) {
		return ExitInterrupt
	}
	return ExitCode(err)
}

// NewCommand constructs the root sofa Cobra command tree.
func NewCommand(opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "sofa",
		Short:         "Plan and package software factory workflows",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(command *cobra.Command, _ []string) error {
			return command.Context().Err()
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})
	root.AddCommand(
		newInitCommand(opts),
		newSchemaCommand(opts),
		newPlanCommand(opts),
		newApplyCommand(opts),
		newServerCommand(opts),
		newWorkerCommand(opts),
		newEnvCommand(opts),
		newSecretCommand(opts),
		newRunsCommand(opts),
		newLogsCommand(opts),
		newPauseCommand(opts),
		newResumeCommand(opts),
		newRerunCommand(opts),
		newDeleteCommand(opts),
		newInterjectCommand(opts),
		newDecisionsCommand(opts),
	)
	return root
}

func newInitCommand(opts Options) *cobra.Command {
	stateDir := ".sofa"
	var upgrade bool
	command := &cobra.Command{
		Use:   "init [source]",
		Short: "Initialize the active configuration, child modules, and harness plugin lockfile",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			source := "."
			if len(args) == 1 {
				source = args[0]
			}
			resolvedSource := resolveInitSource(source, opts.WorkingDir)
			resolvedStateDir, err := resolvePathFromWorkingDir(stateDir, opts.WorkingDir)
			if err != nil {
				return err
			}
			if err := project.Init(command.Context(), resolvedSource, resolvedStateDir, project.InitOptions{
				Upgrade:             upgrade,
				Fetch:               opts.Fetch,
				ReleaseFetcher:      opts.ReleaseFetcher,
				AttestationVerifier: opts.AttestationVerifier,
				LookupEnv:           opts.LookupEnv,
			}); err != nil {
				return fmt.Errorf("initialize project: %w", err)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Initialized sofa workspace in %s\n", resolvedStateDir)
			return err
		},
	}
	command.Flags().StringVar(&stateDir, "state-dir", stateDir, "path to the sofa state directory")
	command.Flags().BoolVar(&upgrade, "upgrade", false, "re-fetch modules and re-verify remote harness plugins")
	return command
}

func newSchemaCommand(opts Options) *cobra.Command {
	stateDir := ".sofa"
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "schema",
		Short: "Print the initialized root variable schema",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			resolvedStateDir, err := resolvePathFromWorkingDir(stateDir, opts.WorkingDir)
			if err != nil {
				return err
			}
			proj, err := project.OpenProject(resolvedStateDir)
			if err != nil {
				return err
			}
			doc, err := variableschema.Inspect(command.Context(), proj.ConfigDirectory)
			if err != nil {
				return fmt.Errorf("inspect root variable schema: %w", err)
			}
			if jsonOutput {
				encoded, err := variableschema.Marshal(doc)
				if err != nil {
					return err
				}
				if _, err := command.OutOrStdout().Write(encoded); err != nil {
					return fmt.Errorf("write root variable schema: %w", err)
				}
				return nil
			}
			if _, err := io.WriteString(command.OutOrStdout(), variableschema.FormatText(doc)); err != nil {
				return fmt.Errorf("write root variable schema: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&stateDir, "state-dir", stateDir, "path to the sofa state directory")
	command.Flags().BoolVar(&jsonOutput, "json", false, "output the versioned JSON schema protocol")
	return command
}

func newPlanCommand(opts Options) *cobra.Command {
	stateDir := ".sofa"
	var outPath string
	var bindings []varBinding
	command := &cobra.Command{
		Use:   "plan",
		Short: "Create an immutable workflow plan and optional .sofaplan archive",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			resolvedStateDir, err := resolvePathFromWorkingDir(stateDir, opts.WorkingDir)
			if err != nil {
				return err
			}
			proj, err := project.OpenProject(resolvedStateDir)
			if err != nil {
				return err
			}

			lockFile, lockErr := harness.ReadLockFile(proj.StateDirectory)
			if lockErr != nil {
				if !errors.Is(lockErr, os.ErrNotExist) {
					return lockErr
				}
				lockFile = harness.PluginLockFile{FormatVersion: harness.LockFormatVersion}
			}

			cliVars, err := buildCliFlagVariables(bindings, opts.WorkingDir, proj.ConfigDirectory)
			if err != nil {
				return err
			}

			loaded, diags, err := config.LoadDirectoryContext(ctx, proj.ConfigDirectory)
			if err != nil {
				return fmt.Errorf("load configuration %q: %w", proj.ConfigDirectory, err)
			}
			if diags.HasErrors() {
				return diags
			}

			cfg := planner.NewSofaConfig(planner.ConfigArgs{
				Context:                  ctx,
				Directory:                proj.ConfigDirectory,
				ModulesDirectory:         proj.ModulesDirectory,
				BundlePath:               "config",
				CliFlagAssignedVariables: cliVars,
			})
			if err := cfg.Init(loaded.Blocks); err != nil {
				return err
			}
			if err := cfg.RunPlan(); err != nil {
				return err
			}
			planned, err := cfg.ExportPlan(planner.ExportOptions{
				PluginLock:        &lockFile,
				RequirePluginLock: true,
			})
			if err != nil {
				return err
			}

			displayText, err := plan.Display(planned)
			if err != nil {
				return fmt.Errorf("display plan: %w", err)
			}
			if _, err := io.WriteString(command.OutOrStdout(), displayText); err != nil {
				return fmt.Errorf("write plan output: %w", err)
			}

			if strings.TrimSpace(outPath) != "" {
				resolvedOut, err := resolvePathFromWorkingDir(outPath, opts.WorkingDir)
				if err != nil {
					return err
				}
				if err := plan.SaveArchive(resolvedOut, planned, plan.ArchiveSource{
					ConfigDir:  proj.ConfigDirectory,
					ModulesDir: proj.ModulesDirectory,
					PluginLock: &lockFile,
				}); err != nil {
					return fmt.Errorf("save plan archive %q: %w", resolvedOut, err)
				}
				if plan.ContainsSensitiveValues(planned) {
					_, _ = fmt.Fprintf(
						command.ErrOrStderr(),
						"warning: plan archive %q is unencrypted and contains sensitive values\n",
						resolvedOut,
					)
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&stateDir, "state-dir", stateDir, "path to the sofa state directory")
	command.Flags().StringVar(&outPath, "out", "", "write self-contained .sofaplan archive to path")
	command.Flags().Var(&orderedVarFlag{kind: varBindingLiteral, bindings: &bindings}, "var", "set a sofa input variable (name=value)")
	command.Flags().Var(&orderedVarFlag{kind: varBindingFile, bindings: &bindings}, "var-file", "load sofa input variables from a file")
	return command
}

type varBindingKind uint8

const (
	varBindingLiteral varBindingKind = iota
	varBindingFile
)

type varBinding struct {
	kind varBindingKind
	name string
	val  string
}

type orderedVarFlag struct {
	kind     varBindingKind
	bindings *[]varBinding
}

func (f *orderedVarFlag) String() string { return "" }

func (f *orderedVarFlag) Type() string {
	if f.kind == varBindingFile {
		return "path"
	}
	return "name=value"
}

func (f *orderedVarFlag) Set(raw string) error {
	switch f.kind {
	case varBindingLiteral:
		name, value, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("invalid -var %q: expected NAME=VALUE", raw)
		}
		*f.bindings = append(*f.bindings, varBinding{
			kind: varBindingLiteral,
			name: name,
			val:  value,
		})
	case varBindingFile:
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("-var-file path must not be empty")
		}
		*f.bindings = append(*f.bindings, varBinding{
			kind: varBindingFile,
			val:  raw,
		})
	}
	return nil
}

func buildCliFlagVariables(
	bindings []varBinding,
	workingDir string,
	configDir string,
) ([]golden.CliFlagAssignedVariables, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	result := make([]golden.CliFlagAssignedVariables, 0, len(bindings))
	for _, b := range bindings {
		switch b.kind {
		case varBindingLiteral:
			result = append(result, golden.NewCliFlagAssignedVariable(b.name, b.val))
		case varBindingFile:
			resolved, err := resolveVarFilePath(b.val, workingDir, configDir)
			if err != nil {
				return nil, err
			}
			result = append(result, golden.NewCliFlagAssignedVariableFile(resolved))
		}
	}
	return result, nil
}

func resolveVarFilePath(rawPath, workingDir, configDir string) (string, error) {
	if filepath.IsAbs(rawPath) {
		info, err := os.Stat(rawPath)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("var-file %q does not exist or is a directory", rawPath)
		}
		return rawPath, nil
	}
	baseDir := workingDir
	if baseDir == "" {
		var err error
		baseDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
	}
	workCandidate := filepath.Join(baseDir, rawPath)
	if info, err := os.Stat(workCandidate); err == nil && !info.IsDir() {
		return workCandidate, nil
	}
	cfgCandidate := filepath.Join(configDir, rawPath)
	if info, err := os.Stat(cfgCandidate); err == nil && !info.IsDir() {
		return cfgCandidate, nil
	}
	return "", fmt.Errorf("var-file %q not found in working directory or configuration directory", rawPath)
}

func resolveInitSource(source, workingDir string) string {
	if workingDir == "" || filepath.IsAbs(source) {
		return source
	}
	if module.IsLocalSource(source) {
		return filepath.Join(workingDir, source)
	}
	candidate := filepath.Join(workingDir, source)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return source
}

func resolvePathFromWorkingDir(path, workingDir string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	base := workingDir
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get current working directory: %w", err)
		}
	}
	return filepath.Clean(filepath.Join(base, path)), nil
}

func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(command *cobra.Command, args []string) error {
		if err := validate(command, args); err != nil {
			return &usageError{err: err}
		}
		return nil
	}
}

func writeError(writer io.Writer, err error) {
	var diagnostics hcl.Diagnostics
	if !errors.As(err, &diagnostics) {
		_, _ = fmt.Fprintf(writer, "error: %v\n", err)
		return
	}
	prefix := strings.TrimSuffix(err.Error(), diagnostics.Error())
	for _, diagnostic := range diagnostics {
		_, _ = fmt.Fprintf(writer, "error: %s%s\n", prefix, diagnostic)
	}
}
