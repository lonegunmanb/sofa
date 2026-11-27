package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dapr/go-sdk/service/common"
)

const (
	// WebSocketSubprotocol is the canonical application subprotocol echoed on authenticated upgrades.
	WebSocketSubprotocol = "sofa.v1"
	// WebSocketBearerPrefix carries base64url-encoded Bearer tokens in Sec-WebSocket-Protocol.
	WebSocketBearerPrefix = "sofa.bearer."
	// ArtifactPreviewCSP enforces zero-trust sandboxing on agent-generated HTML previews.
	ArtifactPreviewCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'"
)

var (
	// ErrInsecureListenAddr indicates an empty APIToken on a non-loopback or non-dev listener.
	ErrInsecureListenAddr = errors.New("server: api_token is required unless dev_mode is enabled on a loopback IP (127.0.0.1 or ::1)")
	// ErrInvalidPreviewTicket indicates a malformed, expired, tampered, or wrong-scope preview ticket.
	ErrInvalidPreviewTicket = errors.New("server: invalid or expired preview ticket")
)

func isLoopbackListenAddr(addr string) bool {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return false
	}
	host, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateListenSecurity enforces that tokenless mode is strictly restricted to DevMode on 127.0.0.1 / ::1,
// and that InternalAddr (when set) always binds strictly to loopback.
func ValidateListenSecurity(publicAddr, internalAddr, apiToken string, devMode bool) error {
	if strings.TrimSpace(apiToken) == "" {
		if !(devMode && isLoopbackListenAddr(publicAddr)) {
			return fmt.Errorf("%w: public_addr=%q dev_mode=%v", ErrInsecureListenAddr, publicAddr, devMode)
		}
	}
	if strings.TrimSpace(internalAddr) != "" && !isLoopbackListenAddr(internalAddr) {
		return fmt.Errorf("server: internal dapr callback addr %q must be a loopback IP (127.0.0.1 or ::1)", internalAddr)
	}
	return nil
}

func constantTimeEqual(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func extractBearerToken(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authz) <= len("Bearer ") {
		return "", false
	}
	if !strings.EqualFold(authz[:7], "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(authz[7:])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func extractWebSocketAuth(r *http.Request) (token string, offerSofaV1 bool, hasToken bool) {
	if tok, ok := extractBearerToken(r); ok {
		token = tok
		hasToken = true
	}
	for _, hdr := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(hdr, ",") {
			proto := strings.TrimSpace(part)
			if proto == "" {
				continue
			}
			if proto == WebSocketSubprotocol {
				offerSofaV1 = true
				continue
			}
			if !hasToken && strings.HasPrefix(proto, WebSocketBearerPrefix) {
				encoded := strings.TrimPrefix(proto, WebSocketBearerPrefix)
				if decoded, err := decodeBase64URLToken(encoded); err == nil && decoded != "" {
					token = decoded
					hasToken = true
				}
			}
		}
	}
	return token, offerSofaV1, hasToken
}

func decodeBase64URLToken(encoded string) (string, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(encoded); err == nil {
		return string(raw), nil
	}
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// bearerAuthMiddleware enforces constant-time Authorization: Bearer <token> when apiToken is configured.
func bearerAuthMiddleware(apiToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiToken != "" {
				tok, ok := extractBearerToken(r)
				if !ok || !constantTimeEqual(tok, apiToken) {
					writeJSONError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originAntiCSRFMiddleware rejects Origin: null and cross-origin requests with 403 Forbidden
// on all mutating /api/v1/* routes and GET /api/v1/ws, even in --dev tokenless mode.
func originAntiCSRFMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requiresOriginCheck(r) {
				if err := validateSameOriginHeader(r); err != nil {
					writeJSONError(w, http.StatusForbidden, err.Error())
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func requiresOriginCheck(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	case http.MethodGet:
		return r.URL != nil && r.URL.Path == "/api/v1/ws"
	default:
		return false
	}
}

func validateSameOriginHeader(r *http.Request) error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	if strings.EqualFold(origin, "null") {
		return errors.New("forbidden: null origin is not permitted")
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("forbidden: invalid origin header")
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("forbidden: origin host %q does not match request host %q", u.Host, r.Host)
	}
	return nil
}

// internalAppTokenMiddleware authenticates Dapr sidecar callbacks using dapr-api-token.
func internalAppTokenMiddleware(tokenProvider func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			expected := ""
			if tokenProvider != nil {
				expected = tokenProvider()
			}
			if expected == "" || !constantTimeEqual(r.Header.Get(common.APITokenKey), expected) {
				http.Error(w, "unauthorized dapr-api-token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type previewTicketClaims struct {
	RunID      string `json:"run_id"`
	ArtifactID string `json:"artifact_id"`
	ExpNano    int64  `json:"exp"`
}

// previewTicketSigner mints and verifies short-lived HMAC-SHA256 tickets scoped to (run_id, artifact_id).
type previewTicketSigner struct {
	secret []byte
	clock  func() time.Time
}

func newPreviewTicketSigner(secret []byte, apiToken string, clock func() time.Time) (*previewTicketSigner, error) {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	key := append([]byte(nil), secret...)
	if len(key) == 0 {
		if apiToken != "" {
			sum := sha256.Sum256([]byte("sofa-preview-ticket-v1:" + apiToken))
			key = sum[:]
		} else {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, fmt.Errorf("server: generate preview secret: %w", err)
			}
		}
	}
	return &previewTicketSigner{secret: key, clock: clock}, nil
}

func (s *previewTicketSigner) mint(runID, artifactID string, ttl time.Duration) (string, time.Time, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(artifactID) == "" {
		return "", time.Time{}, errors.New("server: run_id and artifact_id are required to mint preview ticket")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	expiresAt := s.clock().Add(ttl)
	claims := previewTicketClaims{
		RunID:      runID,
		ArtifactID: artifactID,
		ExpNano:    expiresAt.UnixNano(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(payload)
	sig := mac.Sum(nil)
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	return token, expiresAt, nil
}

func (s *previewTicketSigner) verify(ticket, runID, artifactID string) error {
	payloadB64, sigB64, ok := strings.Cut(strings.TrimSpace(ticket), ".")
	if !ok || payloadB64 == "" || sigB64 == "" {
		return ErrInvalidPreviewTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return ErrInvalidPreviewTicket
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return ErrInvalidPreviewTicket
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(payload)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, expectedSig) {
		return ErrInvalidPreviewTicket
	}
	var claims previewTicketClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ErrInvalidPreviewTicket
	}
	if claims.RunID != runID || claims.ArtifactID != artifactID {
		return ErrInvalidPreviewTicket
	}
	if !s.clock().Before(time.Unix(0, claims.ExpNano)) {
		return ErrInvalidPreviewTicket
	}
	return nil
}
