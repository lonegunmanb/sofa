package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/spf13/cobra"
)

func newEnvCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage global and namespace environment variables",
	}
	cmd.AddCommand(
		newConfigSetCmd(opts, false),
		newConfigListCmd(opts, false),
		newConfigDeleteCmd(opts, false),
	)
	return cmd
}

func newSecretCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage global and namespace write-only secrets",
	}
	cmd.AddCommand(
		newConfigSetCmd(opts, true),
		newConfigListCmd(opts, true),
		newConfigDeleteCmd(opts, true),
	)
	return cmd
}

func scopeForNamespace(ns string) (string, string) {
	trimmed := strings.TrimSpace(ns)
	if trimmed == "" {
		return string(config.ScopeGlobal), ""
	}
	return string(config.ScopeNamespace), trimmed
}

func newConfigSetCmd(opts Options, isSecret bool) *cobra.Command {
	var (
		cf        clientFlags
		namespace string
		fromEnv   bool
	)
	noun := "env"
	endpoint := "/api/v1/envs"
	use := "set [--namespace <ns>] KEY=VALUE [KEY2=VALUE2...]"
	short := "Set environment variables"
	if isSecret {
		noun = "secret"
		endpoint = "/api/v1/secrets"
		use = "set [--namespace <ns>] [--from-env KEY... | KEY=VALUE...]"
		short = "Set write-only secrets (use --from-env to avoid shell history exposure)"
	}

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			type entryItem struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}
			entries := make([]entryItem, 0, len(args))
			if fromEnv {
				if !isSecret {
					return &usageError{err: errors.New("--from-env is only supported for sofa secret set")}
				}
				lookup := lookupEnvFunc(opts)
				for _, rawKey := range args {
					key := strings.TrimSpace(rawKey)
					if key == "" {
						return &usageError{err: errors.New("secret key name must not be empty")}
					}
					val, ok := lookup(key)
					if !ok {
						return fmt.Errorf("environment variable %q is not set in current process environment", key)
					}
					entries = append(entries, entryItem{Name: key, Value: val})
				}
			} else {
				for _, raw := range args {
					k, v, ok := strings.Cut(raw, "=")
					k = strings.TrimSpace(k)
					if !ok || k == "" {
						return &usageError{err: fmt.Errorf("invalid %s assignment %q: expected KEY=VALUE", noun, raw)}
					}
					entries = append(entries, entryItem{Name: k, Value: v})
				}
			}

			scope, ns := scopeForNamespace(namespace)
			payload := map[string]any{
				"scope":   scope,
				"entries": entries,
			}
			if ns != "" {
				payload["namespace"] = ns
			}

			var resp config.UpsertEntriesResp
			if err := client.doJSON(command.Context(), http.MethodPut, endpoint, payload, &resp); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Updated %s keys (%s): %s\n", noun, scope, strings.Join(resp.Updated, ", "))
			return nil
		},
	}

	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&namespace, "namespace", "", "target namespace (defaults to global scope when omitted)")
	if isSecret {
		cmd.Flags().BoolVar(&fromEnv, "from-env", false, "read secret values from current environment variables by key name")
	}
	return cmd
}

func newConfigListCmd(opts Options, isSecret bool) *cobra.Command {
	var (
		cf        clientFlags
		namespace string
		runID     string
	)
	endpoint := "/api/v1/envs"
	short := "List environment variables"
	if isSecret {
		endpoint = "/api/v1/secrets"
		short = "List secret metadata (names and timestamps only; plaintext is never returned)"
	}

	cmd := &cobra.Command{
		Use:   "list",
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			q := url.Values{}
			scope, ns := scopeForNamespace(namespace)
			if strings.TrimSpace(runID) != "" {
				q.Set("scope", string(config.ScopeRun))
				q.Set("run_id", strings.TrimSpace(runID))
			} else {
				q.Set("scope", scope)
				if ns != "" {
					q.Set("namespace", ns)
				}
			}

			if isSecret {
				var resp struct {
					Scope     string              `json:"scope"`
					Namespace string              `json:"namespace"`
					Secrets   []config.SecretMeta `json:"secrets"`
				}
				if err := client.doJSON(command.Context(), http.MethodGet, endpoint+"?"+q.Encode(), nil, &resp); err != nil {
					return err
				}
				for _, s := range resp.Secrets {
					_, _ = fmt.Fprintf(command.OutOrStdout(), "%s\tscope=%s\tnamespace=%s\tupdated_at=%s\n",
						s.Name, s.Scope, s.Namespace, s.UpdatedAt.Format("2006-01-02T15:04:05Z"))
				}
				return nil
			}

			var resp struct {
				Scope     string            `json:"scope"`
				Namespace string            `json:"namespace"`
				Envs      []config.EnvEntry `json:"envs"`
			}
			if err := client.doJSON(command.Context(), http.MethodGet, endpoint+"?"+q.Encode(), nil, &resp); err != nil {
				return err
			}
			for _, e := range resp.Envs {
				_, _ = fmt.Fprintf(command.OutOrStdout(), "%s=%s\tscope=%s\tnamespace=%s\n",
					e.Name, e.Value, e.Scope, e.Namespace)
			}
			return nil
		},
	}

	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&namespace, "namespace", "", "target namespace (defaults to global scope when omitted)")
	cmd.Flags().StringVar(&runID, "run", "", "optional run ID filter")
	return cmd
}

func newConfigDeleteCmd(opts Options, isSecret bool) *cobra.Command {
	var (
		cf        clientFlags
		namespace string
	)
	noun := "env"
	endpoint := "/api/v1/envs"
	if isSecret {
		noun = "secret"
		endpoint = "/api/v1/secrets"
	}

	cmd := &cobra.Command{
		Use:   "delete [--namespace <ns>] KEY [KEY2...]",
		Short: fmt.Sprintf("Delete %s entries", noun),
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := newAPIClient(cf, opts)
			if err != nil {
				return err
			}
			scope, ns := scopeForNamespace(namespace)
			q := url.Values{}
			q.Set("scope", scope)
			if ns != "" {
				q.Set("namespace", ns)
			}
			for _, rawKey := range args {
				key := strings.TrimSpace(rawKey)
				if key == "" {
					return &usageError{err: errors.New("key name must not be empty")}
				}
				var resp config.DeleteEntryResp
				if err := client.doJSON(
					command.Context(),
					http.MethodDelete,
					fmt.Sprintf("%s/%s?%s", endpoint, url.PathEscape(key), q.Encode()),
					nil,
					&resp,
				); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(command.OutOrStdout(), "Deleted %s %q (deleted=%v)\n", noun, key, resp.Deleted)
			}
			return nil
		},
	}

	bindClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&namespace, "namespace", "", "target namespace (defaults to global scope when omitted)")
	return cmd
}
