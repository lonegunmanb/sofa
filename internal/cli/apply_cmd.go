package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	"github.com/lonegunmanb/sofa/internal/project"
	"github.com/lonegunmanb/sofa/internal/server"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/spf13/cobra"
)

func newApplyCommand(opts Options) *cobra.Command {
	var (
		cf         clientFlags
		namespace  string
		envPairs   []string
		secretEnvs []string
		secretKVs  []string
		bindings   []varBinding
		detach     bool
	)

	cmd := &cobra.Command{
		Use:   "apply [path]",
		Short: "Compile a local configuration or upload a .sofaplan archive and execute it on sofa server",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			ctx := command.Context()
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}

			envs, err := parseKeyValuePairs(envPairs, "--env")
			if err != nil {
				return err
			}
			secrets, err := parseKeyValuePairs(secretKVs, "--secret")
			if err != nil {
				return err
			}
			if len(secretEnvs) > 0 {
				if secrets == nil {
					secrets = make(map[string]string, len(secretEnvs))
				}
				lookup := lookupEnvFunc(opts)
				for _, key := range secretEnvs {
					trimmed := strings.TrimSpace(key)
					if trimmed == "" {
						return &usageError{err: errors.New("--secret-env key must not be empty")}
					}
					val, ok := lookup(trimmed)
					if !ok {
						return fmt.Errorf("environment variable %q for --secret-env is not set", trimmed)
					}
					secrets[trimmed] = val
				}
			}

			target := "."
			if len(args) == 1 {
				target = args[0]
			}
			resolvedTarget, err := resolvePathFromWorkingDir(target, opts.WorkingDir)
			if err != nil {
				return err
			}
			info, err := os.Stat(resolvedTarget)
			if err != nil {
				return fmt.Errorf("inspect apply target %q: %w", resolvedTarget, err)
			}

			var planBytes []byte
			if info.IsDir() {
				planBytes, err = compileDirToSofaplanBytes(ctx, resolvedTarget, bindings, opts, command.ErrOrStderr())
				if err != nil {
					return err
				}
			} else {
				if len(bindings) > 0 {
					return &usageError{err: errors.New("-var and -var-file cannot be used when applying a prebuilt .sofaplan file")}
				}
				planBytes, err = os.ReadFile(resolvedTarget)
				if err != nil {
					return fmt.Errorf("read plan file %q: %w", resolvedTarget, err)
				}
			}

			runID, effNS, status, err := client.postMultipartRun(ctx, namespace, envs, secrets, planBytes)
			if err != nil {
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == 422 && len(apiErr.MissingKeys) > 0 {
					return formatMissingKeysError(apiErr.MissingKeys, namespace)
				}
				return err
			}

			_, _ = fmt.Fprintf(command.OutOrStdout(), "Run created: %s (namespace=%s, status=%s)\n", runID, effNS, status)
			if detach {
				return nil
			}

			return followRunInForeground(ctx, client, runID, command.OutOrStdout(), command.ErrOrStderr())
		},
	}

	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&namespace, "namespace", "default", "target configuration namespace")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "set run-scoped env variable (KEY=VALUE)")
	cmd.Flags().StringArrayVar(&secretEnvs, "secret-env", nil, "import run-scoped secret from client environment variable (KEY)")
	cmd.Flags().StringArrayVar(&secretKVs, "secret", nil, "set run-scoped secret directly (KEY=VALUE)")
	cmd.Flags().Var(&orderedVarFlag{kind: varBindingLiteral, bindings: &bindings}, "var", "set a sofa input variable (name=value)")
	cmd.Flags().Var(&orderedVarFlag{kind: varBindingFile, bindings: &bindings}, "var-file", "load sofa input variables from a file")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "submit run and exit immediately without streaming logs")
	return cmd
}

func compileDirToSofaplanBytes(
	ctx context.Context,
	configDir string,
	bindings []varBinding,
	opts Options,
	stderr io.Writer,
) ([]byte, error) {
	tmpStateDir, err := os.MkdirTemp("", "sofa-apply-state-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary state directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpStateDir) }()

	// Preserve existing plugins.lock.json when present in <dir>/.sofa or <workingDir>/.sofa.
	seedExistingPluginLock(tmpStateDir, configDir, opts.WorkingDir)

	if err := project.Init(ctx, configDir, tmpStateDir, project.InitOptions{
		Fetch:               opts.Fetch,
		ReleaseFetcher:      opts.ReleaseFetcher,
		AttestationVerifier: opts.AttestationVerifier,
		LookupEnv:           opts.LookupEnv,
	}); err != nil {
		return nil, fmt.Errorf("initialize configuration %q: %w", configDir, err)
	}

	proj, err := project.OpenProject(tmpStateDir)
	if err != nil {
		return nil, err
	}

	lockFile, lockErr := harness.ReadLockFile(proj.StateDirectory)
	if lockErr != nil {
		if !errors.Is(lockErr, os.ErrNotExist) {
			return nil, lockErr
		}
		lockFile = harness.PluginLockFile{FormatVersion: harness.LockFormatVersion}
	}

	cliVars, err := buildCliFlagVariables(bindings, opts.WorkingDir, configDir)
	if err != nil {
		return nil, err
	}

	loaded, diags, err := config.LoadDirectoryContext(ctx, proj.ConfigDirectory)
	if err != nil {
		return nil, fmt.Errorf("load configuration %q: %w", configDir, err)
	}
	if diags.HasErrors() {
		return nil, diags
	}

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:                  ctx,
		Directory:                proj.ConfigDirectory,
		ModulesDirectory:         proj.ModulesDirectory,
		BundlePath:               "config",
		CliFlagAssignedVariables: cliVars,
	})
	if err := cfg.Init(loaded.Blocks); err != nil {
		return nil, err
	}
	if err := cfg.RunPlan(); err != nil {
		return nil, err
	}
	planned, err := cfg.ExportPlan(planner.ExportOptions{
		PluginLock:        &lockFile,
		RequirePluginLock: true,
	})
	if err != nil {
		return nil, err
	}

	tmpArchiveDir, err := os.MkdirTemp("", "sofa-apply-plan-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary plan directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpArchiveDir) }()

	archivePath := filepath.Join(tmpArchiveDir, "apply.sofaplan")
	if err := plan.SaveArchive(archivePath, planned, plan.ArchiveSource{
		ConfigDir:  proj.ConfigDirectory,
		ModulesDir: proj.ModulesDirectory,
		PluginLock: &lockFile,
	}); err != nil {
		return nil, fmt.Errorf("package .sofaplan archive: %w", err)
	}
	if plan.ContainsSensitiveValues(planned) && stderr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: compiled plan contains sensitive values\n")
	}
	return os.ReadFile(archivePath)
}

func seedExistingPluginLock(dstStateDir, configDir, workingDir string) {
	candidates := []string{filepath.Join(configDir, ".sofa", harness.LockFileName)}
	if strings.TrimSpace(workingDir) != "" {
		candidates = append(candidates, filepath.Join(workingDir, ".sofa", harness.LockFileName))
	}
	for _, cand := range candidates {
		if data, err := os.ReadFile(cand); err == nil && len(data) > 0 {
			_ = os.WriteFile(filepath.Join(dstStateDir, harness.LockFileName), data, 0o600)
			return
		}
	}
}

func formatMissingKeysError(missingKeys []string, namespace string) error {
	keysArg := strings.Join(missingKeys, " ")
	nsSuffix := ""
	if ns := strings.TrimSpace(namespace); ns != "" && ns != "default" {
		nsSuffix = fmt.Sprintf(" --namespace %s", ns)
	}
	return fmt.Errorf(
		"preflight check failed: missing required managed environment variables or secrets: %s\n"+
			"Set them before running sofa apply:\n"+
			"  sofa secret set --from-env %s%s\n"+
			"  (or for non-secret config: sofa env set%s KEY=VALUE)",
		strings.Join(missingKeys, ", "),
		keysArg, nsSuffix,
		nsSuffix,
	)
}

func followRunInForeground(ctx context.Context, client *apiClient, runID string, stdout, stderr io.Writer) error {
	wsCtx, wsCancel := context.WithCancel(ctx)
	defer wsCancel()

	type wsReadResult struct {
		ev  server.WSEvent
		err error
	}
	wsCh := make(chan wsReadResult, 32)
	conn, wsErr := client.dialWS(wsCtx, "/api/v1/ws?run_id="+url.QueryEscape(runID))
	if wsErr == nil {
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		go func() {
			defer close(wsCh)
			for {
				_, data, err := conn.Read(wsCtx)
				if err != nil {
					wsCh <- wsReadResult{err: err}
					return
				}
				var ev server.WSEvent
				if json.Unmarshal(data, &ev) == nil {
					wsCh <- wsReadResult{ev: ev}
				}
			}
		}()
	} else {
		close(wsCh)
	}

	printedNodeStatus := make(map[string]node.NodeStatus)
	printedDecisions := make(map[string]struct{})
	lastRunStatus := run.RunStatus("")

	checkState := func() (done bool, err error) {
		var detail server.RunDetailResponse
		if err := client.doJSON(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, &detail); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}

		for _, n := range detail.Nodes {
			if prev, seen := printedNodeStatus[n.CanonicalAddress]; !seen || prev != n.Status {
				printedNodeStatus[n.CanonicalAddress] = n.Status
				_, _ = fmt.Fprintf(stdout, "[node] %s -> %s (epoch %d)\n", n.CanonicalAddress, n.Status, n.CurrentEpoch)
			}
		}

		if detail.Status != lastRunStatus {
			lastRunStatus = detail.Status
			_, _ = fmt.Fprintf(stdout, "[run] %s -> %s\n", detail.RunID, detail.Status)
		}

		if detail.Status == run.RunStatusWaitingHuman {
			var decResp struct {
				Decisions []node.DecisionCard `json:"decisions"`
			}
			if dErr := client.doJSON(ctx, http.MethodGet, "/api/v1/decisions?status=PENDING&run_id="+url.QueryEscape(runID), nil, &decResp); dErr == nil {
				for _, card := range decResp.Decisions {
					if _, seen := printedDecisions[card.CardID]; seen {
						continue
					}
					printedDecisions[card.CardID] = struct{}{}
					printDecisionHint(stdout, card)
				}
			}
		}

		switch detail.Status {
		case run.RunStatusSucceeded:
			printRunOutputsSummary(stdout, detail.Outputs)
			return true, nil
		case run.RunStatusFailed:
			msg := detail.ErrorMessage
			if msg == "" {
				msg = "run failed"
			}
			return true, fmt.Errorf("run %s failed: %s", runID, msg)
		case run.RunStatusDeleted:
			return true, fmt.Errorf("run %s was deleted", runID)
		default:
			return false, nil
		}
	}

	if finished, err := checkState(); finished {
		return err
	}

	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	wsOpen := wsErr == nil
	for {
		select {
		case <-ctx.Done():
			pauseCtx, pauseCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer pauseCancel()
			var pauseResp run.PauseRunResp
			pErr := client.doJSON(
				pauseCtx,
				http.MethodPost,
				"/api/v1/runs/"+url.PathEscape(runID)+"/pause?mode=immediate",
				map[string]string{"mode": "immediate"},
				&pauseResp,
			)
			if pErr != nil {
				if stderr != nil {
					_, _ = fmt.Fprintf(stderr, "warning: failed to pause run %s on interrupt: %v\n", runID, pErr)
				}
			} else {
				_, _ = fmt.Fprintf(stdout, "\nInterrupted: paused run %s immediately (status=%s)\n", runID, pauseResp.Status)
			}
			return fmt.Errorf("interrupted run %s: %w", runID, context.Canceled)

		case res, ok := <-wsCh:
			if !ok {
				wsOpen = false
				wsCh = nil
				continue
			}
			if res.err != nil {
				wsOpen = false
				wsCh = nil
				continue
			}
			printWSEvent(stdout, res.ev)
			if res.ev.Topic == run.DefaultRunEventsTopic || strings.HasPrefix(res.ev.Type, "run_status:") {
				if finished, err := checkState(); finished {
					return err
				}
			}

		case <-ticker.C:
			_ = wsOpen
			if finished, err := checkState(); finished {
				return err
			}
		}
	}
}

func printWSEvent(w io.Writer, ev server.WSEvent) {
	if ev.Topic == daemon.EventsTopic {
		var le daemon.LogEvent
		if json.Unmarshal(ev.Payload, &le) == nil {
			switch {
			case le.Text != "":
				_, _ = fmt.Fprintf(w, "[%s] %s\n", le.NodeAddr, strings.TrimRight(le.Text, "\r\n"))
			case le.Error != "":
				_, _ = fmt.Fprintf(w, "[%s] error: %s\n", le.NodeAddr, le.Error)
			case le.ToolName != "":
				_, _ = fmt.Fprintf(w, "[%s] tool %s (%s)\n", le.NodeAddr, le.ToolName, le.Kind)
			}
		}
	}
}

func printDecisionHint(w io.Writer, card node.DecisionCard) {
	_, _ = fmt.Fprintf(w, "[decision] pending %s on %s (kind=%s)\n", card.CardID, card.NodeAddr, card.Kind)
	if card.Question != "" {
		_, _ = fmt.Fprintf(w, "  question: %s\n", card.Question)
	}
	if card.Kind == node.DecisionKindHumanApproval {
		_, _ = fmt.Fprintf(w, "  respond: sofa decisions respond %s --approve\n", card.CardID)
	} else {
		_, _ = fmt.Fprintf(w, "  respond: sofa decisions respond %s --choice <value> (or --comment \"...\")\n", card.CardID)
	}
}

func printRunOutputsSummary(w io.Writer, outputs map[string]server.RunOutputView) {
	if len(outputs) == 0 {
		return
	}
	names := make([]string, 0, len(outputs))
	for k := range outputs {
		names = append(names, k)
	}
	sort.Strings(names)
	_, _ = fmt.Fprintln(w, "Outputs:")
	for _, name := range names {
		out := outputs[name]
		val := out.Value
		if out.Unknown {
			val = "<unknown>"
		}
		encoded, err := json.Marshal(val)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  %s = %v\n", name, val)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s = %s\n", name, string(encoded))
	}
}
