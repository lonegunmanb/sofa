// Package harnessplugin defines the public Go SDK and handshake contract for
// external sofa AgentHarness plugins. It intentionally imports zero internal/... packages.
package harnessplugin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const (
	// PluginName is the registered go-plugin map key for AgentHarness implementations.
	PluginName = "agent_harness"

	coreProtocolVersion = 1
)

// Handshake defines the shared magic cookie and protocol version required by sofa hosts and plugins.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "SOFA_HARNESS_PLUGIN",
	MagicCookieValue: "sofa-harness-v1",
}

// GRPCPlugin adapts an AgentHarnessServer implementation for hashicorp/go-plugin over gRPC.
type GRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl harnessv1.AgentHarnessServer
}

// GRPCServer registers the AgentHarness gRPC service with the plugin server.
func (p *GRPCPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	harnessv1.RegisterAgentHarnessServer(s, p.Impl)
	return nil
}

// GRPCClient returns a typed AgentHarnessClient from the host's gRPC ClientConn.
func (p *GRPCPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return harnessv1.NewAgentHarnessClient(c), nil
}

// ScrubPluginEnv removes SOFA_HARNESS_PLUGIN and all PLUGIN_* environment variables
// from the current process environment so child shells never inherit handshake credentials.
func ScrubPluginEnv() {
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if key == Handshake.MagicCookieKey || strings.HasPrefix(key, "PLUGIN_") {
			_ = os.Unsetenv(key)
		}
	}
}

// Serve runs the AgentHarness gRPC plugin server.
//
// When running inside a Linux container without PLUGIN_UNIX_SOCKET_DIR and with a pinned
// PLUGIN_MIN_PORT == PLUGIN_MAX_PORT (Windows/macOS Podman port-forwarding mode), go-plugin's
// stock serverListener would force a /tmp Unix socket or bind 127.0.0.1. In that mode Serve
// binds 0.0.0.0:<port> directly with AutoMTLS and standard grpc.health.v1.Health so Podman's
// -p 127.0.0.1::<port> forwarding reaches the plugin securely.
func Serve(impl harnessv1.AgentHarnessServer) {
	if port, ok := containerTCPPortFromEnv(); ok {
		serveContainerTCP(impl, port)
		return
	}

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins: plugin.PluginSet{
			PluginName: &GRPCPlugin{Impl: impl},
		},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			ScrubPluginEnv()
			return grpc.NewServer(opts...)
		},
		Logger: hclog.New(&hclog.LoggerOptions{
			Level:      hclog.Warn,
			Output:     os.Stderr,
			JSONFormat: true,
		}),
	})
}

func containerTCPPortFromEnv() (int, bool) {
	if strings.TrimSpace(os.Getenv("PLUGIN_UNIX_SOCKET_DIR")) != "" {
		return 0, false
	}
	minStr := strings.TrimSpace(os.Getenv("PLUGIN_MIN_PORT"))
	maxStr := strings.TrimSpace(os.Getenv("PLUGIN_MAX_PORT"))
	if minStr == "" || minStr != maxStr {
		return 0, false
	}
	port, err := strconv.Atoi(minStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func prepareContainerTCPConfig() (grpc.ServerOption, string, int, error) {
	cookieOK := os.Getenv(Handshake.MagicCookieKey) == Handshake.MagicCookieValue
	clientCertPEM := os.Getenv("PLUGIN_CLIENT_CERT")
	protoVersions := os.Getenv("PLUGIN_PROTOCOL_VERSIONS")
	ScrubPluginEnv()

	if !cookieOK {
		return nil, "", 0, errors.New("This binary is a sofa harness plugin and must be launched by sofa.")
	}
	if strings.TrimSpace(clientCertPEM) == "" {
		return nil, "", 0, errors.New("PLUGIN_CLIENT_CERT is required in container TCP mode (AutoMTLS is mandatory)")
	}

	negotiatedVersion := negotiateProtocolVersion(protoVersions, int(Handshake.ProtocolVersion))
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM([]byte(clientCertPEM)) {
		return nil, "", 0, errors.New("failed to parse PLUGIN_CLIENT_CERT PEM")
	}
	certPEM, keyPEM, err := generateServerCert()
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to generate server certificate: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to load generated server certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientPool,
		RootCAs:      clientPool,
		MinVersion:   tls.VersionTLS12,
		ServerName:   "localhost",
	}
	serverCertB64 := base64.RawStdEncoding.EncodeToString(cert.Certificate[0])
	return grpc.Creds(credentials.NewTLS(tlsCfg)), serverCertB64, negotiatedVersion, nil
}

func serveContainerTCP(impl harnessv1.AgentHarnessServer, port int) {
	credsOpt, serverCertB64, negotiatedVersion, err := prepareContainerTCPConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(math.MaxInt32),
		grpc.MaxSendMsgSize(math.MaxInt32),
		credsOpt,
	}

	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bind plugin TCP listener on 0.0.0.0:%d: %v\n", port, err)
		os.Exit(1)
	}
	defer func() { _ = lis.Close() }()

	srv := grpc.NewServer(serverOpts...)
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("plugin", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	reflection.Register(srv)
	harnessv1.RegisterAgentHarnessServer(srv, impl)

	// Emit the exact single-line go-plugin handshake onto stdout, then flush immediately.
	fmt.Printf("%d|%d|tcp|%s|grpc|%s\n",
		coreProtocolVersion,
		negotiatedVersion,
		lis.Addr().String(),
		serverCertB64,
	)
	_ = os.Stdout.Sync()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		srv.Stop()
	}()

	if err := srv.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "plugin grpc server exited: %v\n", err)
	}
}

func negotiateProtocolVersion(raw string, defaultVersion int) int {
	if strings.TrimSpace(raw) == "" {
		return defaultVersion
	}
	for _, part := range strings.Split(raw, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && v == defaultVersion {
			return v
		}
	}
	return defaultVersion
}

func generateServerCert() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	snLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	sn, err := rand.Int(rand.Reader, snLimit)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"sofa"},
		},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             now.Add(-30 * time.Second),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	var certBuf bytes.Buffer
	if err := pem.Encode(&certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return nil, nil, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	var keyBuf bytes.Buffer
	if err := pem.Encode(&keyBuf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return nil, nil, err
	}
	return certBuf.Bytes(), keyBuf.Bytes(), nil
}
