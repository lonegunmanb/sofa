package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
)

const (
	defaultServerAddr  = "http://127.0.0.1:8080"
	defaultHTTPTimeout = 30 * time.Second
)

type clientFlags struct {
	Server string
	Token  string
}

func bindClientFlags(cmd *cobra.Command, cf *clientFlags) {
	cmd.Flags().StringVar(&cf.Server, "server", "", "sofa server base URL (env: SOFA_SERVER_ADDR, default "+defaultServerAddr+")")
	cmd.Flags().StringVar(&cf.Token, "token", "", "sofa API Bearer token (env: SOFA_API_TOKEN)")
}

func lookupEnvFunc(opts Options) func(string) (string, bool) {
	if opts.LookupEnv != nil {
		return opts.LookupEnv
	}
	return os.LookupEnv
}

func envOrDefault(lookup func(string) (string, bool), key, fallback string) string {
	if lookup != nil {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return fallback
}

// APIError represents a structured HTTP error response from the sofa server.
type APIError struct {
	StatusCode  int
	Message     string
	MissingKeys []string
}

func (e *APIError) Error() string {
	if len(e.MissingKeys) > 0 {
		return fmt.Sprintf("server error (%d): %s (missing_keys: %s)", e.StatusCode, e.Message, strings.Join(e.MissingKeys, ", "))
	}
	if e.Message != "" {
		return fmt.Sprintf("server error (%d): %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("server error (%d)", e.StatusCode)
}

type apiClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newAPIClient(cf clientFlags, opts Options) (*apiClient, error) {
	lookup := lookupEnvFunc(opts)
	rawServer := strings.TrimSpace(cf.Server)
	if rawServer == "" {
		rawServer = envOrDefault(lookup, "SOFA_SERVER_ADDR", defaultServerAddr)
	}
	u, err := url.Parse(rawServer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &usageError{err: fmt.Errorf("invalid server URL %q: expected http(s)://host[:port]", rawServer)}
	}
	baseURL := strings.TrimRight(rawServer, "/")

	token := strings.TrimSpace(cf.Token)
	if token == "" {
		token = envOrDefault(lookup, "SOFA_API_TOKEN", "")
	}

	return &apiClient{
		baseURL:    baseURL,
		token:      token,
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
	}, nil
}

func (c *apiClient) doJSON(ctx context.Context, method, path string, reqBody, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyAuth(req.Header)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return decodeHTTPError(resp)
	}
	if respOut == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respOut); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	return nil
}

func (c *apiClient) getBytes(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request GET %s: %w", path, err)
	}
	c.applyAuth(req.Header)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, decodeHTTPError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (c *apiClient) postMultipartRun(
	ctx context.Context,
	namespace string,
	envs, secrets map[string]string,
	planBytes []byte,
) (runID, effectiveNS, status string, err error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if strings.TrimSpace(namespace) != "" {
		if err := mw.WriteField("namespace", strings.TrimSpace(namespace)); err != nil {
			return "", "", "", err
		}
	}
	if len(envs) > 0 {
		raw, err := json.Marshal(envs)
		if err != nil {
			return "", "", "", err
		}
		if err := mw.WriteField("envs", string(raw)); err != nil {
			return "", "", "", err
		}
	}
	if len(secrets) > 0 {
		raw, err := json.Marshal(secrets)
		if err != nil {
			return "", "", "", err
		}
		if err := mw.WriteField("secrets", string(raw)); err != nil {
			return "", "", "", err
		}
	}
	fw, err := mw.CreateFormFile("plan", "plan.sofaplan")
	if err != nil {
		return "", "", "", err
	}
	if _, err := fw.Write(planBytes); err != nil {
		return "", "", "", err
	}
	if err := mw.Close(); err != nil {
		return "", "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/runs", &body)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.applyAuth(req.Header)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("submit run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return "", "", "", decodeHTTPError(resp)
	}
	var out struct {
		RunID     string `json:"run_id"`
		Namespace string `json:"namespace"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", "", fmt.Errorf("decode create run response: %w", err)
	}
	return out.RunID, out.Namespace, out.Status, nil
}

func (c *apiClient) dialWS(ctx context.Context, path string) (*websocket.Conn, error) {
	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	hdr := make(http.Header)
	c.applyAuth(hdr)
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *apiClient) applyAuth(h http.Header) {
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
}

func decodeHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &APIError{StatusCode: resp.StatusCode}
	var parsed struct {
		Error       string   `json:"error"`
		MissingKeys []string `json:"missing_keys"`
	}
	if json.Unmarshal(raw, &parsed) == nil && (parsed.Error != "" || len(parsed.MissingKeys) > 0) {
		apiErr.Message = parsed.Error
		apiErr.MissingKeys = parsed.MissingKeys
		return apiErr
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = resp.Status
	}
	apiErr.Message = msg
	return apiErr
}

func parseKeyValuePairs(items []string, flagName string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, raw := range items {
		k, v, ok := strings.Cut(raw, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, &usageError{err: fmt.Errorf("invalid %s %q: expected KEY=VALUE", flagName, raw)}
		}
		out[k] = v
	}
	return out, nil
}
