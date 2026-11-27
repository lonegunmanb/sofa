package cli

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/spf13/cobra"
)

func newWorkerCommand(opts Options) *cobra.Command {
	var (
		listen         string
		appID          string
		appAPIToken    string
		daprGRPCPort   string
		maxConcurrency int
		pluginDir      string
		stateDir       string
		tempDir        string
		runtimeBin     string
		bf             blobStoreFlags
	)

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a standalone sofa compute worker daemon",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			lookup := lookupEnvFunc(opts)

			resolvedListen := strings.TrimSpace(listen)
			if resolvedListen == "" {
				if port := envOrDefault(lookup, "APP_PORT", ""); port != "" {
					resolvedListen = "127.0.0.1:" + port
				} else {
					resolvedListen = "127.0.0.1:50051"
				}
			}
			resolvedAppID := strings.TrimSpace(appID)
			if resolvedAppID == "" {
				resolvedAppID = envOrDefault(lookup, "APP_ID", "")
			}
			resolvedAppToken := strings.TrimSpace(appAPIToken)
			if resolvedAppToken == "" {
				resolvedAppToken = envOrDefault(lookup, "APP_API_TOKEN", "")
			}
			resolvedGRPCPort := strings.TrimSpace(daprGRPCPort)
			if resolvedGRPCPort == "" {
				resolvedGRPCPort = envOrDefault(lookup, "DAPR_GRPC_PORT", "")
			}

			defaultBlobDir := ""
			toolCacheDir := ""
			resolvedTempDir := strings.TrimSpace(tempDir)
			if strings.TrimSpace(stateDir) != "" {
				absState, err := filepath.Abs(stateDir)
				if err != nil {
					return fmt.Errorf("resolve state-dir: %w", err)
				}
				if err := os.MkdirAll(absState, 0o700); err != nil {
					return fmt.Errorf("create state-dir %q: %w", absState, err)
				}
				defaultBlobDir = filepath.Join(absState, "blobs")
				toolCacheDir = filepath.Join(absState, "tool-cache")
				if resolvedTempDir == "" {
					resolvedTempDir = filepath.Join(absState, "worker-tmp")
				}
			}

			blobStore, err := openBlobStore(ctx, bf, defaultBlobDir)
			if err != nil {
				return fmt.Errorf("initialize BlobStore: %w", err)
			}

			daprClient, err := connectExternalDaprClient(resolvedGRPCPort)
			if err != nil {
				return fmt.Errorf("connect external Dapr sidecar: %w", err)
			}
			defer daprClient.Close()

			var cpClientRef atomic.Pointer[daprclient.Client]
			cpClientRef.Store(&daprClient)
			cpClientProvider := func() daprclient.Client {
				c := cpClientRef.Load()
				if c == nil {
					return nil
				}
				return *c
			}

			if maxConcurrency <= 0 {
				maxConcurrency = registry.DefaultWorkerMaxConcurrency
			}
			rtBin := strings.TrimSpace(runtimeBin)
			if rtBin == "" {
				rtBin = "podman"
			}
			eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{RuntimeBin: rtBin})
			platform := detectTargetPlatformBestEffort(ctx, eng)

			adapter := &daprControlPlaneAdapter{provider: cpClientProvider}
			wd, err := daemon.New(daemon.Options{
				AppID:          resolvedAppID,
				DevMode:        false,
				MaxConcurrency: maxConcurrency,
				Arch:           platform.Key(),
				Registry:       adapter,
				PubSub:         adapter,
				BlobStore:      blobStore,
				SandboxEngine:  eng,
				NodeReporter:   adapter,
				MergeReporter:  adapter,
				TempDir:        resolvedTempDir,
				PluginDir:      strings.TrimSpace(pluginDir),
				NodeRunnerDepsProvider: func(_ context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error) {
					c := cpClientProvider()
					remoteCP, cpErr := node.NewRemoteNodeControlPlaneForNode(c, req.RunID, req.NodeAddr, req.Epoch)
					if cpErr != nil {
						return runner.WorkerRunnerDeps{}, cpErr
					}
					gitAdapter := git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: req.LookupEnv})
					nodeEng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{
						RuntimeBin: rtBin,
						GitAdapter: gitAdapter,
					})
					return runner.WorkerRunnerDeps{
						ControlPlane:  remoteCP,
						GitAdapter:    gitAdapter,
						BlobStore:     blobStore,
						ArtifactSvc:   artifact.NewService(blobStore, artifact.ServiceOptions{}),
						SandboxEngine: nodeEng,
						SessionLauncher: &runner.ContainerSessionLauncher{
							Installer:    harnessinstaller.NewInstaller(),
							ToolCacheDir: toolCacheDir,
							StartTimeout: 60 * time.Second,
						},
					}, nil
				},
			})
			if err != nil {
				return fmt.Errorf("create WorkerDaemon: %w", err)
			}

			mux := chi.NewRouter()
			if resolvedAppToken != "" {
				mux.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						got := r.Header.Get(common.APITokenKey)
						if subtle.ConstantTimeCompare([]byte(got), []byte(resolvedAppToken)) != 1 {
							http.Error(w, "unauthorized dapr-api-token", http.StatusUnauthorized)
							return
						}
						next.ServeHTTP(w, r)
					})
				})
			}

			svc := daprhttp.NewServiceWithMux(resolvedListen, mux)
			if err := wd.RegisterHandlers(svc); err != nil {
				return err
			}

			svcErrCh := make(chan error, 1)
			go func() {
				err := svc.Start()
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					svcErrCh <- err
				}
				close(svcErrCh)
			}()

			if err := wd.Start(ctx); err != nil {
				_ = svc.Stop()
				return fmt.Errorf("start WorkerDaemon: %w", err)
			}

			_, _ = fmt.Fprintf(command.OutOrStdout(), "sofa worker listening on %s (process_id=%s, app_id=%s, max_concurrency=%d)\n",
				resolvedListen, wd.ProcessID(), wd.AppID(), maxConcurrency)

			var runErr error
			select {
			case <-ctx.Done():
			case err := <-svcErrCh:
				if err != nil {
					runErr = fmt.Errorf("worker callback server exited: %w", err)
				}
			}

			shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer shutCancel()
			return errors.Join(runErr, wd.Stop(shutCtx), svc.Stop())
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "", "internal Dapr callback listen address (default 127.0.0.1:50051 or 127.0.0.1:$APP_PORT)")
	cmd.Flags().StringVar(&appID, "app-id", "", "Dapr App ID (env: APP_ID; defaults to sofa-worker-<process_id>)")
	cmd.Flags().StringVar(&appAPIToken, "app-api-token", "", "Dapr internal callback token (env: APP_API_TOKEN)")
	cmd.Flags().StringVar(&daprGRPCPort, "dapr-grpc-port", "", "external Dapr sidecar gRPC port (env: DAPR_GRPC_PORT)")
	cmd.Flags().IntVar(&maxConcurrency, "max-concurrency", registry.DefaultWorkerMaxConcurrency, "maximum concurrent node tasks")
	cmd.Flags().StringVar(&pluginDir, "plugin-dir", "", "local harness plugin discovery directory")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "optional worker state/cache directory")
	cmd.Flags().StringVar(&tempDir, "temp-dir", "", "temporary workspace root")
	cmd.Flags().StringVar(&runtimeBin, "runtime-bin", "podman", "container runtime CLI binary (podman, docker, nerdctl)")
	bindBlobStoreFlags(cmd, &bf)
	return cmd
}

// Compile-time unused import guard for merge/node if needed in worker tests.
var (
	_ = merge.ActorType
	_ = node.ActorType
)
