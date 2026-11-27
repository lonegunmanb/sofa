package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/lonegunmanb/sofa/internal/webui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_UnbuiltFallbackPage(t *testing.T) {
	unbuiltFS := fstest.MapFS{
		".keep": &fstest.MapFile{Data: []byte("placeholder")},
	}
	h := webui.NewHandlerFromFS(unbuiltFS)

	for _, reqPath := range []string{"/", "/runs/123", "/decisions"} {
		req := httptest.NewRequest(http.MethodGet, reqPath, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		res := rec.Result()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		_ = res.Body.Close()

		assert.Equal(t, http.StatusOK, res.StatusCode, "path=%s", reqPath)
		assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
		assert.Contains(t, string(body), "go run ./scripts/build-webui.go")
	}
}

func TestHandler_BuiltSPAHistoryFallbackAndStaticMIME(t *testing.T) {
	builtFS := fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte("<!doctype html><html><body>sofa-ui</body></html>")},
		"assets/app.js":   &fstest.MapFile{Data: []byte("console.log('sofa');")},
		"assets/app.css":  &fstest.MapFile{Data: []byte("body { color: #111; }")},
		"assets/logo.svg": &fstest.MapFile{Data: []byte("<svg></svg>")},
		"favicon.ico":     &fstest.MapFile{Data: []byte("ico")},
	}
	h := webui.NewHandlerFromFS(builtFS)

	// SPA History routes fall back to index.html
	for _, route := range []string{"/", "/runs", "/runs/123", "/decisions", "/settings"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		res := rec.Result()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		_ = res.Body.Close()

		assert.Equal(t, http.StatusOK, res.StatusCode, "route=%s", route)
		assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
		assert.Contains(t, string(body), "sofa-ui")
	}

	// Static JS asset served with JavaScript MIME type
	jsReq := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	jsRec := httptest.NewRecorder()
	h.ServeHTTP(jsRec, jsReq)
	assert.Equal(t, http.StatusOK, jsRec.Code)
	jsCT := jsRec.Header().Get("Content-Type")
	assert.True(t,
		assert.ObjectsAreEqual(true, len(jsCT) > 0) &&
			(containsAny(jsCT, "application/javascript", "text/javascript")),
		"expected JS MIME type, got %q", jsCT,
	)
	assert.Equal(t, "console.log('sofa');", jsRec.Body.String())

	// Static CSS asset served with text/css
	cssReq := httptest.NewRequest(http.MethodGet, "/assets/app.css", nil)
	cssRec := httptest.NewRecorder()
	h.ServeHTTP(cssRec, cssReq)
	assert.Equal(t, http.StatusOK, cssRec.Code)
	assert.Contains(t, cssRec.Header().Get("Content-Type"), "text/css")
	assert.Equal(t, "body { color: #111; }", cssRec.Body.String())

	// Missing static asset extension returns 404 (not index.html)
	missingReq := httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil)
	missingRec := httptest.NewRecorder()
	h.ServeHTTP(missingRec, missingReq)
	assert.Equal(t, http.StatusNotFound, missingRec.Code)
}

func TestHandler_NeverInterceptsReservedPrefixes(t *testing.T) {
	builtFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><body>spa</body></html>")},
	}
	h := webui.NewHandlerFromFS(builtFS)

	for _, reserved := range []string{"/api", "/api/v1/unknown", "/actors/foo", "/dapr/config", "/healthz"} {
		req := httptest.NewRequest(http.MethodGet, reserved, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "reserved=%s", reserved)
	}
}

func TestHandler_NewHandlerEmbeddedSmoke(t *testing.T) {
	h := webui.NewHandler()
	require.NotNil(t, h)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && containsFold(s, sub) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
