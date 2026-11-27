package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/server"
	"github.com/spf13/cobra"
)

func newRunsCommand(opts Options) *cobra.Command {
	var (
		cf        clientFlags
		namespace string
		status    string
	)
	runsCmd := &cobra.Command{
		Use:   "runs",
		Short: "List or inspect workflow runs",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			return listRuns(command, opts, cf, namespace, status)
		},
	}
	bindClientFlags(runsCmd, &cf)
	runsCmd.Flags().StringVar(&namespace, "namespace", "", "filter by namespace")
	runsCmd.Flags().StringVar(&status, "status", "", "filter by run status")

	var listCF clientFlags
	var listNS, listStatus string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List workflow runs",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			return listRuns(command, opts, listCF, listNS, listStatus)
		},
	}
	bindClientFlags(listCmd, &listCF)
	listCmd.Flags().StringVar(&listNS, "namespace", "", "filter by namespace")
	listCmd.Flags().StringVar(&listStatus, "status", "", "filter by run status")

	var getCF clientFlags
	var reveal bool
	getCmd := &cobra.Command{
		Use:   "get <run_id>",
		Short: "Inspect a run's status, nodes, and outputs",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(getCF, opts)
			if err != nil {
				return err
			}
			path := "/api/v1/runs/" + url.PathEscape(args[0])
			if reveal {
				path += "?reveal_sensitive=true"
			}
			var detail server.RunDetailResponse
			if err := client.doJSON(command.Context(), http.MethodGet, path, nil, &detail); err != nil {
				return err
			}
			w := command.OutOrStdout()
			_, _ = fmt.Fprintf(w, "Run: %s\nNamespace: %s\nStatus: %s\n", detail.RunID, detail.Namespace, detail.Status)
			if detail.ErrorMessage != "" {
				_, _ = fmt.Fprintf(w, "Error: %s\n", detail.ErrorMessage)
			}
			for _, n := range detail.Nodes {
				_, _ = fmt.Fprintf(w, "  node %s\tstatus=%s\tepoch=%d\tbranch=%s\tsha=%s\n",
					n.CanonicalAddress, n.Status, n.CurrentEpoch, n.WorkBranch, n.CommitSHA)
			}
			printRunOutputsSummary(w, detail.Outputs)
			return nil
		},
	}
	bindClientFlags(getCmd, &getCF)
	getCmd.Flags().BoolVar(&reveal, "reveal-sensitive", false, "reveal sensitive output values")

	runsCmd.AddCommand(listCmd, getCmd)
	return runsCmd
}

func listRuns(command *cobra.Command, opts Options, cf clientFlags, namespace, status string) error {
	client, err := newAPIClient(cf, opts)
	if err != nil {
		return err
	}
	q := url.Values{}
	if strings.TrimSpace(namespace) != "" {
		q.Set("namespace", strings.TrimSpace(namespace))
	}
	if strings.TrimSpace(status) != "" {
		q.Set("status", strings.TrimSpace(status))
	}
	path := "/api/v1/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp run.ListRunsResp
	if err := client.doJSON(command.Context(), http.MethodGet, path, nil, &resp); err != nil {
		return err
	}
	for _, r := range resp.Runs {
		_, _ = fmt.Fprintf(command.OutOrStdout(), "%s\tnamespace=%s\tstatus=%s\tupdated_at=%s\n",
			r.RunID, r.Namespace, r.Status, r.UpdatedAt.Format("2006-01-02T15:04:05Z"))
	}
	return nil
}

func newLogsCommand(opts Options) *cobra.Command {
	var (
		cf    clientFlags
		epoch uint64
	)
	cmd := &cobra.Command{
		Use:   "logs <run_id> <node_id>",
		Short: "Fetch archived JSONL logs for a node",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/v1/runs/%s/nodes/%s/logs", url.PathEscape(args[0]), url.PathEscape(args[1]))
			if epoch > 0 {
				path += fmt.Sprintf("?epoch=%d", epoch)
			}
			raw, err := client.getBytes(command.Context(), path)
			if err != nil {
				return err
			}
			_, err = command.OutOrStdout().Write(raw)
			return err
		},
	}
	bindClientFlags(cmd, &cf)
	cmd.Flags().Uint64Var(&epoch, "epoch", 0, "specific execution epoch (defaults to latest)")
	return cmd
}

func newPauseCommand(opts Options) *cobra.Command {
	var (
		cf   clientFlags
		mode string
	)
	cmd := &cobra.Command{
		Use:   "pause <run_id>",
		Short: "Pause an active run (planned or immediate)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			m := strings.ToLower(strings.TrimSpace(mode))
			if m != "planned" && m != "immediate" {
				return &usageError{err: fmt.Errorf("invalid --mode %q (expected planned or immediate)", mode)}
			}
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			var resp run.PauseRunResp
			if err := client.doJSON(
				command.Context(),
				http.MethodPost,
				fmt.Sprintf("/api/v1/runs/%s/pause?mode=%s", url.PathEscape(args[0]), url.QueryEscape(m)),
				map[string]string{"mode": m},
				&resp,
			); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Run %s status=%s (paused=%v)\n", resp.RunID, resp.Status, resp.Paused)
			return nil
		},
	}
	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&mode, "mode", "planned", "pause mode: planned or immediate")
	return cmd
}

func newResumeCommand(opts Options) *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "resume <run_id>",
		Short: "Resume a paused or failed run from its latest complete checkpoint",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			var resp run.ResumeRunResp
			if err := client.doJSON(command.Context(), http.MethodPost, "/api/v1/runs/"+url.PathEscape(args[0])+"/resume", nil, &resp); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Run %s status=%s (resumed=%v)\n", resp.RunID, resp.Status, resp.Resumed)
			return nil
		},
	}
	bindClientFlags(cmd, &cf)
	return cmd
}

func newRerunCommand(opts Options) *cobra.Command {
	var (
		cf     clientFlags
		reason string
	)
	cmd := &cobra.Command{
		Use:   "rerun <run_id> <node_id>",
		Short: "Reset and rerun a node and its transitive downstream nodes",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			var body any
			if strings.TrimSpace(reason) != "" {
				body = map[string]string{"reason": reason}
			}
			var resp run.RerunNodeResp
			if err := client.doJSON(
				command.Context(),
				http.MethodPost,
				fmt.Sprintf("/api/v1/runs/%s/nodes/%s/rerun", url.PathEscape(args[0]), url.PathEscape(args[1])),
				body,
				&resp,
			); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Rerun accepted for %s (run_status=%s)\n", args[1], resp.RunStatus)
			return nil
		},
	}
	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&reason, "reason", "", "optional rerun reason")
	return cmd
}

func newDeleteCommand(opts Options) *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "delete <run_id>",
		Short: "Stop and delete a run, cleaning up intermediate Git branches and blobs",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			var resp run.DeleteRunResp
			if err := client.doJSON(command.Context(), http.MethodDelete, "/api/v1/runs/"+url.PathEscape(args[0]), nil, &resp); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Deleted run %s (deleted=%v, status=%s)\n", args[0], resp.Deleted, resp.Status)
			return nil
		},
	}
	bindClientFlags(cmd, &cf)
	return cmd
}

func newInterjectCommand(opts Options) *cobra.Command {
	var (
		cf      clientFlags
		message string
		wait    bool
	)
	cmd := &cobra.Command{
		Use:   "interject <run_id> <node_id>",
		Short: "Send a live human diary interjection to a running node",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(command *cobra.Command, args []string) error {
			if strings.TrimSpace(message) == "" {
				return &usageError{err: errors.New("-m / --message is required")}
			}
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			var resp node.EnqueueInterjectionResp
			if err := client.doJSON(
				command.Context(),
				http.MethodPost,
				fmt.Sprintf("/api/v1/runs/%s/nodes/%s/diary-messages", url.PathEscape(args[0]), url.PathEscape(args[1])),
				node.EnqueueInterjectionReq{
					Message:             message,
					WaitForConfirmation: wait,
				},
				&resp,
			); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Enqueued interjection %s (status=%s)\n", resp.ID, resp.Status)
			return nil
		},
	}
	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVarP(&message, "message", "m", "", "interjection message")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for human confirmation after agent responds")
	return cmd
}

func newDecisionsCommand(opts Options) *cobra.Command {
	var (
		cf     clientFlags
		runID  string
		status string
	)
	decisionsCmd := &cobra.Command{
		Use:   "decisions",
		Short: "List or respond to human decision cards",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			return listDecisions(command, opts, cf, runID, status)
		},
	}
	bindClientFlags(decisionsCmd, &cf)
	decisionsCmd.Flags().StringVar(&runID, "run", "", "filter decisions by run_id")
	decisionsCmd.Flags().StringVar(&status, "status", "", "filter decisions by status (PENDING, ANSWERED, INVALIDATED)")

	var listCF clientFlags
	var listRunID, listStatus string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List human decision cards",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			return listDecisions(command, opts, listCF, listRunID, listStatus)
		},
	}
	bindClientFlags(listCmd, &listCF)
	listCmd.Flags().StringVar(&listRunID, "run", "", "filter decisions by run_id")
	listCmd.Flags().StringVar(&listStatus, "status", "", "filter decisions by status (PENDING, ANSWERED, INVALIDATED)")

	var (
		respCF         clientFlags
		respRunID      string
		respNodeAddr   string
		choice         string
		approve        bool
		reject         bool
		returnFallback string
		comment        string
		commitSHA      string
		artifactIDs    []string
	)
	respondCmd := &cobra.Command{
		Use:   "respond <decision_id>",
		Short: "Respond to a pending decision or human_approval card",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			ctx := command.Context()
			client, err := newAPIClient(respCF, opts)
			if err != nil {
				return err
			}
			cardID := args[0]

			if approve && reject {
				return &usageError{err: errors.New("--approve and --reject cannot be used together")}
			}
			if approve && strings.TrimSpace(returnFallback) != "" {
				return &usageError{err: errors.New("--approve and --return-fallback cannot be used together")}
			}

			// Look up card metadata from GET /api/v1/decisions when run/node/commit_sha are omitted.
			var matchedCard *node.DecisionCard
			if strings.TrimSpace(respRunID) == "" || strings.TrimSpace(respNodeAddr) == "" || (approve && strings.TrimSpace(commitSHA) == "") {
				q := url.Values{}
				if strings.TrimSpace(respRunID) != "" {
					q.Set("run_id", strings.TrimSpace(respRunID))
				}
				path := "/api/v1/decisions"
				if len(q) > 0 {
					path += "?" + q.Encode()
				}
				var decResp struct {
					Decisions []node.DecisionCard `json:"decisions"`
				}
				if err := client.doJSON(ctx, http.MethodGet, path, nil, &decResp); err == nil {
					for i := range decResp.Decisions {
						c := decResp.Decisions[i]
						if c.CardID == cardID {
							if matchedCard == nil || c.Status == node.DecisionStatusPending {
								cp := c
								matchedCard = &cp
							}
						}
					}
				}
			}

			if matchedCard != nil {
				if strings.TrimSpace(respRunID) == "" {
					respRunID = matchedCard.RunID
				}
				if strings.TrimSpace(respNodeAddr) == "" {
					respNodeAddr = matchedCard.NodeAddr
				}
				if strings.TrimSpace(commitSHA) == "" {
					commitSHA = matchedCard.CommitSHA
				}
				if len(artifactIDs) == 0 && len(matchedCard.ArtifactIDs) > 0 {
					artifactIDs = append([]string(nil), matchedCard.ArtifactIDs...)
				}
			}

			body := map[string]any{}
			if strings.TrimSpace(respRunID) != "" {
				body["run_id"] = strings.TrimSpace(respRunID)
			}
			if strings.TrimSpace(respNodeAddr) != "" {
				body["node_addr"] = strings.TrimSpace(respNodeAddr)
			}
			if strings.TrimSpace(choice) != "" {
				body["selected_option"] = strings.TrimSpace(choice)
			}
			if strings.TrimSpace(comment) != "" {
				body["answer_text"] = comment
			}
			if approve {
				body["approved"] = true
			} else if reject {
				body["approved"] = false
			}
			if strings.TrimSpace(returnFallback) != "" {
				body["returned_upstream"] = true
				body["return_fallback"] = strings.TrimSpace(returnFallback)
			}
			if strings.TrimSpace(commitSHA) != "" {
				body["commit_sha"] = strings.TrimSpace(commitSHA)
			}
			if len(artifactIDs) > 0 {
				body["artifact_ids"] = artifactIDs
			}

			var resp node.SubmitDecisionResp
			if err := client.doJSON(
				ctx,
				http.MethodPost,
				fmt.Sprintf("/api/v1/decisions/%s/respond", url.PathEscape(cardID)),
				body,
				&resp,
			); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Decision %s accepted (status=%s, auto_resumed=%v)\n", cardID, resp.Status, resp.AutoResumed)
			return nil
		},
	}
	bindClientFlags(respondCmd, &respCF)
	respondCmd.Flags().StringVar(&respRunID, "run", "", "owning run_id (auto-detected if omitted)")
	respondCmd.Flags().StringVar(&respNodeAddr, "node", "", "owning node_addr (auto-detected if omitted)")
	respondCmd.Flags().StringVar(&choice, "choice", "", "selected option for request_decision")
	respondCmd.Flags().BoolVar(&approve, "approve", false, "approve a human_approval card")
	respondCmd.Flags().BoolVar(&reject, "reject", false, "reject a human_approval card")
	respondCmd.Flags().StringVar(&returnFallback, "return-fallback", "", "trigger upstream fallback name")
	respondCmd.Flags().StringVar(&comment, "comment", "", "free-text answer or approval comment")
	respondCmd.Flags().StringVar(&commitSHA, "commit-sha", "", "expected commit SHA for human_approval (auto-detected if omitted)")
	respondCmd.Flags().StringArrayVar(&artifactIDs, "artifact-id", nil, "expected artifact IDs for human_approval (auto-detected if omitted)")

	decisionsCmd.AddCommand(listCmd, respondCmd)
	return decisionsCmd
}

func listDecisions(command *cobra.Command, opts Options, cf clientFlags, runID, status string) error {
	client, err := newAPIClient(cf, opts)
	if err != nil {
		return err
	}
	q := url.Values{}
	if strings.TrimSpace(runID) != "" {
		q.Set("run_id", strings.TrimSpace(runID))
	}
	if strings.TrimSpace(status) != "" {
		q.Set("status", strings.TrimSpace(status))
	}
	path := "/api/v1/decisions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Decisions []node.DecisionCard `json:"decisions"`
	}
	if err := client.doJSON(command.Context(), http.MethodGet, path, nil, &resp); err != nil {
		return err
	}
	printDecisions(command.OutOrStdout(), resp.Decisions)
	return nil
}

func printDecisions(w io.Writer, cards []node.DecisionCard) {
	for _, c := range cards {
		encodedOpts, _ := json.Marshal(c.Options)
		_, _ = fmt.Fprintf(w, "%s\trun=%s\tnode=%s\tkind=%s\tstatus=%s\tcommit=%s\toptions=%s\n",
			c.CardID, c.RunID, c.NodeAddr, c.Kind, c.Status, c.CommitSHA, string(encodedOpts))
	}
}
