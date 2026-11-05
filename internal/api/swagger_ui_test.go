package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newSwaggerUIRouter() *gin.Engine {
	s := &Server{}
	r := gin.New()
	r.GET("/api/docs", s.handleSwaggerUI)
	r.GET("/api/docs/*any", s.handleSwaggerUI)

	return r
}

var swaggerUIRefRe = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// TestSwaggerUINoExternalAssets: the docs page runs on the origin that holds
// the session token, so it must load nothing from a third-party origin.
func TestSwaggerUINoExternalAssets(t *testing.T) {
	t.Parallel()

	r := newSwaggerUIRouter()

	for _, path := range []string{"/api/docs", "/api/docs/", "/api/docs/index.html"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, w.Code)
		}

		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, ct)
		}

		body := w.Body.String()
		if strings.Contains(body, "unpkg.com") || strings.Contains(body, "://") {
			t.Fatalf("GET %s references an absolute URL:\n%s", path, body)
		}

		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("GET %s CSP = %q, want script-src 'self' without unsafe-eval", path, csp)
		}

		if strings.Contains(body, "<script>") {
			t.Fatalf("GET %s has an inline script, which its CSP forbids:\n%s", path, body)
		}
	}
}

// TestSwaggerUIAssetsServed: positive control, every asset the page references
// is served locally with a usable content type.
func TestSwaggerUIAssetsServed(t *testing.T) {
	t.Parallel()

	r := newSwaggerUIRouter()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/docs", nil))

	refs := swaggerUIRefRe.FindAllStringSubmatch(w.Body.String(), -1)
	if len(refs) != 3 {
		t.Fatalf("found %d asset references, want 3 (css, bundle, initializer)", len(refs))
	}

	for _, ref := range refs {
		url := ref[1]
		if !strings.HasPrefix(url, "/api/docs/") {
			t.Fatalf("asset %q is not served under /api/docs/", url)
		}

		aw := httptest.NewRecorder()
		r.ServeHTTP(aw, httptest.NewRequest(http.MethodGet, url, nil))

		if aw.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", url, aw.Code)
		}

		ct := aw.Header().Get("Content-Type")
		if strings.HasPrefix(ct, "text/html") || ct == "" {
			t.Fatalf("GET %s Content-Type = %q, want the asset's own type", url, ct)
		}

		if aw.Body.Len() < 100 {
			t.Fatalf("GET %s returned %d bytes, want the asset", url, aw.Body.Len())
		}
	}
}
