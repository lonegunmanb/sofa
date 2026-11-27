package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var embeddedDist embed.FS

const unbuiltFallbackHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>sofa Web UI — Not Built</title>
  <style>
    body { font-family: system-ui, -apple-system, sans-serif; background: #0f172a; color: #e2e8f0; margin: 0; padding: 3rem 1.5rem; }
    main { max-width: 640px; margin: 0 auto; background: #1e293b; border: 1px solid #334155; border-radius: 12px; padding: 2rem; }
    h1 { margin-top: 0; font-size: 1.35rem; color: #f8fafc; }
    p { line-height: 1.6; color: #cbd5e1; }
    pre { background: #020617; border: 1px solid #334155; border-radius: 8px; padding: 0.9rem 1rem; overflow-x: auto; color: #38bdf8; }
  </style>
</head>
<body>
  <main>
    <h1>sofa Web UI bundle is not built yet</h1>
    <p>This binary was compiled without a built frontend bundle in <code>internal/webui/dist</code>. Run the following command from the repository root to build and embed the Vue 3 dashboard:</p>
    <pre><code>go run ./scripts/build-webui.go</code></pre>
    <p>REST endpoints under <code>/api/v1/*</code> and <code>/healthz</code> remain active.</p>
  </main>
</body>
</html>
`

var staticAssetExts = map[string]struct{}{
	".js":    {},
	".mjs":   {},
	".css":   {},
	".map":   {},
	".png":   {},
	".jpg":   {},
	".jpeg":  {},
	".gif":   {},
	".svg":   {},
	".ico":   {},
	".woff":  {},
	".woff2": {},
	".ttf":   {},
	".eot":   {},
	".json":  {},
	".wasm":  {},
	".txt":   {},
}

var explicitMIMETypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".mjs":   "application/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".wasm":  "application/wasm",
	".txt":   "text/plain; charset=utf-8",
}

// NewHandler returns an http.Handler backed by the embedded frontend dist filesystem.
func NewHandler() http.Handler {
	sub, err := fs.Sub(embeddedDist, "dist")
	if err != nil {
		return NewHandlerFromFS(nil)
	}
	return NewHandlerFromFS(sub)
}

// NewHandlerFromFS returns an http.Handler serving a built SPA from distFS with History
// routing fallback, or an unbuilt status page when index.html is absent.
func NewHandlerFromFS(distFS fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil || r.URL == nil {
			http.NotFound(w, r)
			return
		}
		if isReservedPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if distFS == nil {
			serveUnbuiltFallback(w, r)
			return
		}

		indexBytes, indexErr := fs.ReadFile(distFS, "index.html")
		if indexErr != nil {
			serveUnbuiltFallback(w, r)
			return
		}

		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if clean != "" && clean != "." && clean != ".keep" {
			if info, err := fs.Stat(distFS, clean); err == nil && !info.IsDir() {
				data, readErr := fs.ReadFile(distFS, clean)
				if readErr == nil {
					serveBytes(w, r, clean, data)
					return
				}
			}
			ext := strings.ToLower(path.Ext(clean))
			if _, isAsset := staticAssetExts[ext]; isAsset {
				http.NotFound(w, r)
				return
			}
		}

		serveBytes(w, r, "index.html", indexBytes)
	})
}

func isReservedPath(p string) bool {
	for _, prefix := range []string{"/api", "/actors", "/dapr", "/healthz"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func serveUnbuiltFallback(w http.ResponseWriter, r *http.Request) {
	serveBytes(w, r, "index.html", []byte(unbuiltFallbackHTML))
}

func serveBytes(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	ct := detectMIME(name, data)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = bytes.NewReader(data).WriteTo(w)
}

func detectMIME(name string, data []byte) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := explicitMIMETypes[ext]; ok {
		return ct
	}
	if ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return http.DetectContentType(data)
}
