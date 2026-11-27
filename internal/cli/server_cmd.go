package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	actorcfg "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/actor/merge"
	"github.com/lonegunmanb/sofa/internal/actor/node"
	"github.com/lonegunmanb/sofa/internal/actor/registry"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	s3blob "github.com/lonegunmanb/sofa/internal/blob/s3"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/lonegunmanb/sofa/internal/server"
	"github.com/lonegunmanb/sofa/internal/webui"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	"github.com/spf13/cobra"
)

type blobStoreFlags struct {
	BlobDir          string
	S3Bucket         string
	S3Region         string
	S3Endpoint       string
	S3Prefix         string
	S3ForcePathStyle bool
}

func bindBlobStoreFlags(cmd *cobra.Command, bf *blobStoreFlags) {
	cmd.Flags().StringVar(&bf.BlobDir, "blob-dir", "", "local BlobStore directory (defaults to <state-dir>/blobs when S3 is not configured)")
	cmd.Flags().StringVar(&bf.S3Bucket, "s3-bucket", "", "S3/MinIO/OSS bucket name (enables S3 BlobStore)")
	cmd.Flags().StringVar(&bf.S3Region, "s3-region", "", "S3 region (default us-east-1)")
	cmd.Flags().StringVar(&bf.S3Endpoint, "s3-endpoint", "", "custom S3-compatible endpoint URL")
	cmd.Flags().StringVar(&bf.S3Prefix, "s3-prefix", "", "optional S3 key prefix")
	cmd.Flags().BoolVar(&bf.S3ForcePathStyle, "s3-force-path-style", false, "force path-style S3 addressing")
}

func openBlobStore(ctx context.Context, bf blobStoreFlags, defaultDir string) (blob.Store, error) {
	if strings.TrimSpace(bf.S3Bucket) != "" {
		return s3blob.NewStore(ctx, s3blob.Config{
			Bucket:       bf.S3Bucket,
			Region:       bf.S3Region,
			Endpoint:     bf.S3Endpoint,
			KeyPrefix:    bf.S3Prefix,
			UsePathStyle: bf.S3ForcePathStyle,
		})
	}
	dir := strings.TrimSpace(bf.BlobDir)
	if dir == "" {
		dir = defaultDir
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("either --blob-dir or --s3-bucket must be configured")
	}
	return blob.NewLocalStore(dir)
}

func newServerCommand(opts Options) *cobra.Command {
	var (
		dev            bool
		listen         string
		internalListen string
		stateDir       string
		maxConcurrency int
		pluginDir      string
		runtimeBin     string
		apiToken       string
		appAPIToken    string
		daprGRPCPort   string
		bf             blobStoreFlags
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the sofa control-plane API server (or full single-node stack with --dev)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			lookup := lookupEnvFunc(opts)

			resolvedToken := strings.TrimSpace(apiToken)
			if resolvedToken == "" {
				resolvedToken = envOrDefault(lookup, "SOFA_API_TOKEN", "")
			}

			resolvedPublic := strings.TrimSpace(listen)
			if resolvedPublic == "" {
				resolvedPublic = "127.0.0.1:8080"
			}
			resolvedInternal := strings.TrimSpace(internalListen)
			if resolvedInternal == "" {
				if dev {
					resolvedInternal = "127.0.0.1:0"
				} else {
					resolvedInternal = "127.0.0.1:50050"
				}
			}

			if err := server.ValidateListenSecurity(resolvedPublic, resolvedInternal, resolvedToken, dev); err != nil {
				return err
			}

			if dev {
				return runDevServer(ctx, command, opts, devServerConfig{
					PublicAddr:     resolvedPublic,
					InternalAddr:   resolvedInternal,
					APIToken:       resolvedToken,
					StateDir:       stateDir,
					MaxConcurrency: maxConcurrency,
					PluginDir:      pluginDir,
					RuntimeBin:     runtimeBin,
					BlobFlags:      bf,
				})
			}

			resolvedAppToken := strings.TrimSpace(appAPIToken)
			if resolvedAppToken == "" {
				resolvedAppToken = envOrDefault(lookup, "APP_API_TOKEN", "")
			}
			resolvedGRPCPort := strings.TrimSpace(daprGRPCPort)
			if resolvedGRPCPort == "" {
				resolvedGRPCPort = envOrDefault(lookup, "DAPR_GRPC_PORT", "")
			}

			return runDistributedServer(ctx, command, opts, distributedServerConfig{
				PublicAddr:   resolvedPublic,
				InternalAddr: resolvedInternal,
				APIToken:     resolvedToken,
				AppAPIToken:  resolvedAppToken,
				DaprGRPCPort: resolvedGRPCPort,
				StateDir:     stateDir,
				PluginDir:    pluginDir,
				RuntimeBin:   runtimeBin,
				BlobFlags:    bf,
			})
		},
	}

	cmd.Flags().BoolVar(&dev, "dev", false, "run all-in-one local mode (embedded Dapr scheduler/daprd + local WorkerDaemon)")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8080", "public HTTP/WebSocket listen address")
	cmd.Flags().StringVar(&internalListen, "internal-listen", "", "internal Dapr callback listen address (loopback)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "persistent state directory (defaults to ~/.sofa/dev-state in --dev mode)")
	cmd.Flags().IntVar(&maxConcurrency, "max-concurrency", 2, "maximum concurrent node tasks for co-located --dev worker")
	cmd.Flags().StringVar(&pluginDir, "plugin-dir", "", "local harness plugin discovery directory")
	cmd.Flags().StringVar(&runtimeBin, "runtime-bin", "podman", "container runtime CLI binary (podman, docker, nerdctl)")
	cmd.Flags().StringVar(&apiToken, "api-token", "", "API Bearer token (env: SOFA_API_TOKEN)")
	cmd.Flags().StringVar(&appAPIToken, "app-api-token", "", "Dapr internal callback token (env: APP_API_TOKEN)")
	cmd.Flags().StringVar(&daprGRPCPort, "dapr-grpc-port", "", "external Dapr sidecar gRPC port (env: DAPR_GRPC_PORT)")
	bindBlobStoreFlags(cmd, &bf)
	return cmd
}

type devServerConfig struct {
	PublicAddr     string
	InternalAddr   string
	APIToken       string
	StateDir       string
	MaxConcurrency int
	PluginDir      string
	RuntimeBin     string
	BlobFlags      blobStoreFlags
}

func runDevServer(ctx context.Context, cmd *cobra.Command, opts Options, cfg devServerConfig) error {
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve user home dir for default --state-dir: %w", err)
		}
		stateDir = filepath.Join(home, ".sofa", "dev-state")
	}
	absStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("resolve state-dir: %w", err)
	}
	if err := os.MkdirAll(absStateDir, 0o700); err != nil {
		return fmt.Errorf("create state-dir %q: %w", absStateDir, err)
	}

	blobStore, err := openBlobStore(ctx, cfg.BlobFlags, filepath.Join(absStateDir, "blobs"))
	if err != nil {
		return fmt.Errorf("initialize BlobStore: %w", err)
	}

	maxConc := cfg.MaxConcurrency
	if maxConc <= 0 {
		maxConc = registry.DefaultDevMaxConcurrency
	}
	runtimeBin := strings.TrimSpace(cfg.RuntimeBin)
	if runtimeBin == "" {
		runtimeBin = "podman"
	}

	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{RuntimeBin: runtimeBin})
	platform := detectTargetPlatformBestEffort(ctx, eng)

	var cpClientRef atomic.Pointer[daprclient.Client]
	cpClientProvider := func() daprclient.Client {
		c := cpClientRef.Load()
		if c == nil {
			return nil
		}
		return *c
	}

	adapter := &daprControlPlaneAdapter{provider: cpClientProvider}
	wd, err := daemon.New(daemon.Options{
		AppID:          "sofa-dev",
		FixedAppID:     "sofa-dev",
		DevMode:        true,
		MaxConcurrency: maxConc,
		Arch:           platform.Key(),
		Registry:       adapter,
		PubSub:         adapter,
		BlobStore:      blobStore,
		SandboxEngine:  eng,
		NodeReporter:   adapter,
		MergeReporter:  adapter,
		TempDir:        filepath.Join(absStateDir, "worker-tmp"),
		PluginDir:      strings.TrimSpace(cfg.PluginDir),
		NodeRunnerDepsProvider: func(_ context.Context, req runner.NodeExecutionRequest) (runner.WorkerRunnerDeps, error) {
			c := cpClientProvider()
			remoteCP, cpErr := node.NewRemoteNodeControlPlaneForNode(c, req.RunID, req.NodeAddr, req.Epoch)
			if cpErr != nil {
				return runner.WorkerRunnerDeps{}, cpErr
			}
			gitAdapter := git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: req.LookupEnv})
			nodeEng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{
				RuntimeBin: runtimeBin,
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
					ToolCacheDir: filepath.Join(absStateDir, "tool-cache"),
					StartTimeout: 60 * time.Second,
				},
			}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("create co-located WorkerDaemon: %w", err)
	}

	srv, err := server.New(server.Config{
		PublicAddr:   cfg.PublicAddr,
		InternalAddr: cfg.InternalAddr,
		APIToken:     cfg.APIToken,
		DevMode:      true,
		Backend:      server.NewDaprBackend(cpClientProvider),
		BlobStore:    blobStore,
		UIHandler:    resolveUIHandler(opts.UIHandler),
		RunDefaults: server.RunStartDefaults{
			RequiredArch:   platform.Key(),
			TargetPlatform: &platform,
		},
		RegisterInternal: func(svc common.Service) error {
			registerControlPlaneActors(svc, cpClientProvider, blobStore, filepath.Join(absStateDir, "bundle-cache"))
			return wd.RegisterHandlers(svc)
		},
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	_, portStr, err := net.SplitHostPort(srv.InternalAddr())
	if err != nil {
		return fmt.Errorf("parse internal callback address %q: %w", srv.InternalAddr(), err)
	}
	internalPort, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("parse internal callback port %q: %w", portStr, err)
	}

	mgr, err := daprhost.NewManager(daprhost.Options{
		StateDir:             filepath.Join(absStateDir, "dapr"),
		AppID:                "sofa-dev",
		AppPort:              internalPort,
		NameResolutionDBPath: filepath.Join(absStateDir, "nameresolution.db"),
	})
	if err != nil {
		return fmt.Errorf("create daprhost manager: %w", err)
	}
	srv.SetAppAPIToken(mgr.AppAPIToken())

	if err := srv.StartInternal(ctx); err != nil {
		return fmt.Errorf("start internal server: %w", err)
	}
	cleanupSrv := true
	defer func() {
		if cleanupSrv {
			_ = srv.Stop(context.Background())
		}
	}()

	cpClient, err := mgr.Start(ctx)
	if err != nil {
		return fmt.Errorf("start daprhost: %w", err)
	}
	cpClientRef.Store(&cpClient)
	cleanupMgr := true
	defer func() {
		if cleanupMgr {
			_ = mgr.Stop(context.Background())
		}
	}()

	if err := wd.Start(ctx); err != nil {
		return fmt.Errorf("start co-located WorkerDaemon: %w", err)
	}
	cleanupWD := true
	defer func() {
		if cleanupWD {
			_ = wd.Stop(context.Background())
		}
	}()

	recCtx, recCancel := context.WithTimeout(ctx, 15*time.Second)
	_, recErr := run.NewOperatorClientStub(cpClient).RecoverActiveRuns(recCtx, &run.RecoverActiveRunsReq{Reason: "server-startup"})
	recCancel()
	if recErr != nil && cmd.ErrOrStderr() != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: RecoverActiveRuns on startup failed: %v\n", recErr)
	}

	if err := srv.StartPublic(ctx); err != nil {
		return fmt.Errorf("start public server: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sofa server --dev listening on http://%s\n", srv.PublicAddr())

	<-ctx.Done()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()

	cleanupWD = false
	cleanupSrv = false
	cleanupMgr = false
	return errors.Join(
		wd.Stop(shutCtx),
		srv.Stop(shutCtx),
		mgr.Stop(shutCtx),
	)
}

type distributedServerConfig struct {
	PublicAddr   string
	InternalAddr string
	APIToken     string
	AppAPIToken  string
	DaprGRPCPort string
	StateDir     string
	PluginDir    string
	RuntimeBin   string
	BlobFlags    blobStoreFlags
}

func runDistributedServer(ctx context.Context, cmd *cobra.Command, opts Options, cfg distributedServerConfig) error {
	defaultBlobDir := ""
	bundleCacheRoot := ""
	if strings.TrimSpace(cfg.StateDir) != "" {
		absState, err := filepath.Abs(cfg.StateDir)
		if err != nil {
			return fmt.Errorf("resolve state-dir: %w", err)
		}
		if err := os.MkdirAll(absState, 0o700); err != nil {
			return fmt.Errorf("create state-dir %q: %w", absState, err)
		}
		defaultBlobDir = filepath.Join(absState, "blobs")
		bundleCacheRoot = filepath.Join(absState, "bundle-cache")
	}

	blobStore, err := openBlobStore(ctx, cfg.BlobFlags, defaultBlobDir)
	if err != nil {
		return fmt.Errorf("initialize BlobStore: %w", err)
	}

	daprClient, err := connectExternalDaprClient(cfg.DaprGRPCPort)
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

	runtimeBin := strings.TrimSpace(cfg.RuntimeBin)
	if runtimeBin == "" {
		runtimeBin = "podman"
	}
	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{RuntimeBin: runtimeBin})
	platform := detectTargetPlatformBestEffort(ctx, eng)

	srv, err := server.New(server.Config{
		PublicAddr:   cfg.PublicAddr,
		InternalAddr: cfg.InternalAddr,
		APIToken:     cfg.APIToken,
		AppAPIToken:  cfg.AppAPIToken,
		DevMode:      false,
		Backend:      server.NewDaprBackend(cpClientProvider),
		BlobStore:    blobStore,
		UIHandler:    resolveUIHandler(opts.UIHandler),
		RunDefaults: server.RunStartDefaults{
			RequiredArch:   platform.Key(),
			TargetPlatform: &platform,
		},
		RegisterInternal: func(svc common.Service) error {
			registerControlPlaneActors(svc, cpClientProvider, blobStore, bundleCacheRoot)
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	if err := srv.StartInternal(ctx); err != nil {
		return fmt.Errorf("start internal server: %w", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		_ = srv.Stop(shutCtx)
	}()

	recCtx, recCancel := context.WithTimeout(ctx, 15*time.Second)
	_, recErr := run.NewOperatorClientStub(daprClient).RecoverActiveRuns(recCtx, &run.RecoverActiveRunsReq{Reason: "server-startup"})
	recCancel()
	if recErr != nil && cmd.ErrOrStderr() != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: RecoverActiveRuns on startup failed: %v\n", recErr)
	}

	if err := srv.StartPublic(ctx); err != nil {
		return fmt.Errorf("start public server: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sofa server listening on http://%s\n", srv.PublicAddr())

	<-ctx.Done()
	return nil
}

func registerControlPlaneActors(
	svc common.Service,
	cpClientProvider func() daprclient.Client,
	blobStore blob.Store,
	bundleCacheRoot string,
) {
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return actorcfg.NewConfigActor(cpClientProvider)
	})
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return registry.NewProcessRegistryActor(registry.Options{
			ClientProvider: cpClientProvider,
		})
	})
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return node.NewNodeActor(node.Options{
			ClientProvider: cpClientProvider,
		})
	})
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return merge.NewMergeActor(merge.Options{
			ClientProvider: cpClientProvider,
		})
	})
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return run.NewOperatorActor(run.OperatorOptions{
			ClientProvider: cpClientProvider,
		})
	})
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return run.NewRunActor(run.RunOptions{
			ClientProvider:  cpClientProvider,
			BlobStore:       blobStore,
			BundleCacheRoot: bundleCacheRoot,
			GitAdapter:      git.NewCLIAdapter(git.CLIAdapterOptions{}),
		})
	})
}

func resolveUIHandler(override http.Handler) http.Handler {
	if override != nil {
		return override
	}
	return webui.NewHandler()
}

func detectTargetPlatformBestEffort(ctx context.Context, eng *sandboxruntime.PodmanEngine) harnessinstaller.Platform {
	if eng != nil {
		detectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		p, err := eng.DetectPlatform(detectCtx)
		cancel()
		if err == nil && p.Validate() == nil {
			return p
		}
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		arch = "amd64"
	}
	return harnessinstaller.Platform{OS: "linux", Arch: arch}
}

func connectExternalDaprClient(grpcPort string) (daprclient.Client, error) {
	if p := strings.TrimSpace(grpcPort); p != "" {
		return daprclient.NewClientWithPort(p)
	}
	return daprclient.NewClient()
}

type daprControlPlaneAdapter struct {
	provider func() daprclient.Client
}

func (a *daprControlPlaneAdapter) RegisterProcess(ctx context.Context, req *registry.RegisterProcessReq) (*registry.RegisterProcessResp, error) {
	return registry.NewClientStub(a.provider()).RegisterProcess(ctx, req)
}

func (a *daprControlPlaneAdapter) Heartbeat(ctx context.Context, req *registry.HeartbeatReq) (*registry.HeartbeatResp, error) {
	return registry.NewClientStub(a.provider()).Heartbeat(ctx, req)
}

func (a *daprControlPlaneAdapter) ReleaseSlot(ctx context.Context, req *registry.ReleaseSlotReq) (*registry.ReleaseSlotResp, error) {
	return registry.NewClientStub(a.provider()).ReleaseSlot(ctx, req)
}

func (a *daprControlPlaneAdapter) PublishEvent(ctx context.Context, pubsubName, topicName string, data any) error {
	return a.provider().PublishEvent(ctx, pubsubName, topicName, data)
}

func (a *daprControlPlaneAdapter) ReportNodeOutcome(ctx context.Context, report daemon.NodeOutcomeReport) error {
	stub, err := node.NewClientStubForNode(report.RunID, report.NodeAddr, a.provider())
	if err != nil {
		return err
	}
	_, err = stub.ReportNodeOutcome(ctx, &report)
	return err
}

func (a *daprControlPlaneAdapter) ReportCandidateResult(ctx context.Context, report daemon.MergeOutcomeReport) error {
	stub := merge.NewClientStub(report.MergeActorID, a.provider())
	_, err := stub.ReportCandidateResult(ctx, &report)
	return err
}
