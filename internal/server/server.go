package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	"github.com/lonegunmanb/sofa/internal/blob"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
)

// RunStartDefaults provides server-level defaults applied when invoking RunActor.StartRun.
type RunStartDefaults struct {
	RequiredArch          string
	HostOS                string
	TargetPlatform        *harnessinstaller.Platform
	PluginResolve         daemon.PluginResolveConfig
	TempRoot              string
	MaxGatekeeperReworks  int
	MaxArtifactReworks    int
	StallTimeout          time.Duration
	WatchdogCheckInterval time.Duration
}

// Config configures the dual-port REST/WebSocket and Dapr internal callback Server.
type Config struct {
	PublicAddr          string
	InternalAddr        string
	APIToken            string
	AppAPIToken         string
	AppAPITokenProvider func() string
	DevMode             bool
	PreviewSecret       []byte
	PreviewTicketTTL    time.Duration
	MaxUploadBytes      int64
	Backend             Backend
	BlobStore           blob.Store
	UIHandler           http.Handler
	RegisterInternal    func(svc common.Service) error
	RunDefaults         RunStartDefaults
	Clock               func() time.Time
}

// Server manages physically isolated public REST/WebSocket and internal Dapr callback listeners.
type Server struct {
	cfg           Config
	hub           *WSHub
	ticketSigner  *previewTicketSigner
	publicRouter  *chi.Mux
	internalMux   *chi.Mux
	internalSvc   common.Service
	appTokenValue atomic.Value

	mu           sync.Mutex
	started      bool
	publicLn     net.Listener
	publicHTTP   *http.Server
	publicAddr   string
	internalAddr string
	internalErr  chan error
	publicErr    chan error
}

// New validates security invariants and builds the dual-port Server without binding sockets yet.
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.PublicAddr) == "" {
		cfg.PublicAddr = "127.0.0.1:8080"
	}
	if err := ValidateListenSecurity(cfg.PublicAddr, cfg.InternalAddr, cfg.APIToken, cfg.DevMode); err != nil {
		return nil, err
	}
	if cfg.PreviewTicketTTL <= 0 {
		cfg.PreviewTicketTTL = 5 * time.Minute
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultMaxUploadBytes
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}

	signer, err := newPreviewTicketSigner(cfg.PreviewSecret, cfg.APIToken, cfg.Clock)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:          cfg,
		hub:          NewWSHub(defaultWSClientBuffer),
		ticketSigner: signer,
		internalAddr: strings.TrimSpace(cfg.InternalAddr),
	}
	s.appTokenValue.Store(cfg.AppAPIToken)

	s.publicRouter = s.buildPublicRouter()

	if err := s.initInternalService(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) buildPublicRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealthz)

	// Explicitly block Dapr actor/runtime paths on the public listener even when UIHandler is set.
	r.HandleFunc("/actors/*", http.NotFound)
	r.HandleFunc("/dapr/*", http.NotFound)

	r.Route("/api/v1", func(api chi.Router) {
		api.Use(originAntiCSRFMiddleware())

		// WebSocket route authenticates via Bearer header or Sec-WebSocket-Protocol subprotocol.
		api.Get("/ws", s.handleWebSocket)

		// Artifact preview content route authenticates via Bearer header or scoped ?ticket=.
		api.Get("/runs/{run_id}/artifacts/{artifact_id}/content", s.handleArtifactContent)

		// All remaining /api/v1/* endpoints strictly enforce Bearer auth when APIToken is set.
		api.Group(func(authed chi.Router) {
			authed.Use(bearerAuthMiddleware(s.cfg.APIToken))

			authed.Post("/runs", s.handleCreateRun)
			authed.Get("/runs", s.handleListRuns)
			authed.Get("/runs/{run_id}", s.handleGetRun)
			authed.Post("/runs/{run_id}/pause", s.handlePauseRun)
			authed.Post("/runs/{run_id}/resume", s.handleResumeRun)
			authed.Delete("/runs/{run_id}", s.handleDeleteRun)

			authed.Get("/runs/{run_id}/nodes/{node_id}", s.handleGetNode)
			authed.Get("/runs/{run_id}/nodes/{node_id}/logs", s.handleGetNodeLogs)
			authed.Post("/runs/{run_id}/nodes/{node_id}/rerun", s.handleRerunNode)
			authed.Post("/runs/{run_id}/nodes/{node_id}/diary-messages", s.handlePostDiaryMessage)

			authed.Post("/runs/{run_id}/artifacts/{artifact_id}/preview-ticket", s.handleMintPreviewTicket)

			authed.Get("/decisions", s.handleListDecisions)
			authed.Post("/decisions/{id}/respond", s.handleRespondDecision)

			authed.Get("/envs", s.handleListEnvs)
			authed.Put("/envs", s.handleUpsertEnvs)
			authed.Put("/envs/{name}", s.handleUpsertEnvs)
			authed.Delete("/envs", s.handleDeleteEnvs)
			authed.Delete("/envs/{name}", s.handleDeleteEnvs)

			authed.Get("/secrets", s.handleListSecrets)
			authed.Put("/secrets", s.handleUpsertSecrets)
			authed.Put("/secrets/{name}", s.handleUpsertSecrets)
			authed.Delete("/secrets", s.handleDeleteSecrets)
			authed.Delete("/secrets/{name}", s.handleDeleteSecrets)
		})
	})

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		p := ""
		if req != nil && req.URL != nil {
			p = req.URL.Path
		}
		if isReservedPublicPrefix(p) {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		if s.cfg.UIHandler != nil {
			s.cfg.UIHandler.ServeHTTP(w, req)
			return
		}
		http.NotFound(w, req)
	})
	return r
}

func isReservedPublicPrefix(p string) bool {
	for _, prefix := range []string{"/api", "/actors", "/dapr"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func (s *Server) initInternalService() error {
	internalMux := chi.NewRouter()
	internalMux.Use(internalAppTokenMiddleware(func() string {
		if s.cfg.AppAPITokenProvider != nil {
			if v := s.cfg.AppAPITokenProvider(); v != "" {
				return v
			}
		}
		tok, _ := s.appTokenValue.Load().(string)
		return tok
	}))

	addr := s.internalAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if strings.HasSuffix(addr, ":0") {
		port, err := reserveLoopbackPort()
		if err != nil {
			return fmt.Errorf("server: reserve internal callback port: %w", err)
		}
		host, _, _ := net.SplitHostPort(addr)
		addr = net.JoinHostPort(host, fmt.Sprintf("%d", port))
		if s.internalAddr != "" {
			s.internalAddr = addr
		}
	}

	svc := daprhttp.NewServiceWithMux(addr, internalMux)
	if err := s.registerPubSubHandlers(svc); err != nil {
		return err
	}
	if s.cfg.RegisterInternal != nil {
		if err := s.cfg.RegisterInternal(svc); err != nil {
			return fmt.Errorf("server: register internal handlers: %w", err)
		}
	}
	s.internalMux = internalMux
	s.internalSvc = svc
	return nil
}

// SetAppAPIToken updates the expected dapr-api-token on the internal callback listener.
func (s *Server) SetAppAPIToken(token string) {
	s.appTokenValue.Store(token)
}

// PublicHandler returns the public HTTP/WebSocket router (without /actors/* or /dapr/*).
func (s *Server) PublicHandler() http.Handler {
	return s.publicRouter
}

// InternalHandler returns the APP_API_TOKEN-protected Dapr callback router.
func (s *Server) InternalHandler() http.Handler {
	return s.internalMux
}

// InternalService returns the underlying Dapr common.Service attached to the internal listener.
func (s *Server) InternalService() common.Service {
	return s.internalSvc
}

// Hub returns the WebSocket fan-out hub.
func (s *Server) Hub() *WSHub {
	return s.hub
}

// PublicAddr returns the bound public listener address after Start (or configured address before Start).
func (s *Server) PublicAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicAddr != "" {
		return s.publicAddr
	}
	return s.cfg.PublicAddr
}

// InternalAddr returns the bound internal Dapr callback address.
func (s *Server) InternalAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.internalAddr
}

// HandlePubSubEvent injects a raw PubSub event payload into the WebSocket hub.
func (s *Server) HandlePubSubEvent(ctx context.Context, topic string, rawData []byte) error {
	return s.hub.HandlePubSubEvent(ctx, topic, rawData)
}

// MintPreviewTicket creates a short-lived HMAC-SHA256 preview ticket scoped strictly to (runID, artifactID).
func (s *Server) MintPreviewTicket(runID, artifactID string, ttl time.Duration) (string, time.Time, error) {
	return s.ticketSigner.mint(runID, artifactID, ttl)
}

// VerifyPreviewTicket validates a preview ticket's signature, expiry, and (runID, artifactID) scope.
func (s *Server) VerifyPreviewTicket(ticket, runID, artifactID string) error {
	return s.ticketSigner.verify(ticket, runID, artifactID)
}

// Start begins serving the public HTTP/WebSocket listener and, when configured, the internal Dapr callback server.
func (s *Server) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("server: already started")
	}

	ln, err := net.Listen("tcp", s.cfg.PublicAddr)
	if err != nil {
		return fmt.Errorf("server: listen public %q: %w", s.cfg.PublicAddr, err)
	}
	pubHTTP := &http.Server{
		Handler:           s.publicRouter,
		ReadHeaderTimeout: 10 * time.Second,
	}
	pubErr := make(chan error, 1)
	go func() {
		err := pubHTTP.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			pubErr <- err
		}
		close(pubErr)
	}()

	s.publicLn = ln
	s.publicHTTP = pubHTTP
	s.publicAddr = ln.Addr().String()
	s.publicErr = pubErr

	if s.internalAddr != "" && s.internalSvc != nil {
		intErr := make(chan error, 1)
		go func() {
			err := s.internalSvc.Start()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				intErr <- err
			}
			close(intErr)
		}()
		if err := waitTCPReady(s.internalAddr, 5*time.Second); err != nil {
			_ = pubHTTP.Close()
			return fmt.Errorf("server: wait internal callback listener %q: %w", s.internalAddr, err)
		}
		s.internalErr = intErr
	}

	s.started = true
	return nil
}

// Stop gracefully closes WebSocket subscribers, the public listener, and the internal Dapr callback server.
func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		s.hub.Close()
		return nil
	}
	pubHTTP := s.publicHTTP
	pubErr := s.publicErr
	internalSvc := s.internalSvc
	intErr := s.internalErr
	hasInternal := s.internalAddr != ""
	s.started = false
	s.publicLn = nil
	s.publicHTTP = nil
	s.mu.Unlock()

	s.hub.Close()

	var errs []error
	if pubHTTP != nil {
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := pubHTTP.Shutdown(shutCtx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown public server: %w", err))
		}
		cancel()
		if pubErr != nil {
			select {
			case err := <-pubErr:
				if err != nil {
					errs = append(errs, err)
				}
			case <-time.After(2 * time.Second):
			}
		}
	}

	if hasInternal && internalSvc != nil {
		if err := internalSvc.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop internal dapr service: %w", err))
		}
		if intErr != nil {
			select {
			case err := <-intErr:
				if err != nil {
					errs = append(errs, err)
				}
			case <-time.After(2 * time.Second):
			}
		}
	}

	return errors.Join(errs...)
}

func reserveLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func waitTCPReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(15 * time.Millisecond)
	}
}
