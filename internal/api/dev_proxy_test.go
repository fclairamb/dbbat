package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/config"
)

// The dev-server proxy is dev-mode only, but it was rewritten from the
// deprecated ReverseProxy.Director onto Rewrite, and the two differ in ways
// that are silent at compile time: Rewrite's SetURL repoints the outbound Host
// at the target, and neither X-Forwarded-For nor the rewritten path comes for
// free. So assert the shape of what actually reaches the dev server.
func TestProxyToDevServerRewritesTheRequest(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	type seen struct {
		path   string
		host   string
		xff    string
		method string
		body   string
	}

	got := make(chan seen, 1)

	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		got <- seen{
			path:   req.URL.Path,
			host:   req.Host,
			xff:    req.Header.Get("X-Forwarded-For"),
			method: req.Method,
			body:   string(body),
		}
		_, _ = w.Write([]byte("from the dev server"))
	}))
	defer dev.Close()

	devURL, err := url.Parse(dev.URL)
	if err != nil {
		t.Fatalf("parse dev server URL: %v", err)
	}

	srv := &Server{logger: slog.New(slog.DiscardHandler)}
	rule := &config.RedirectRule{
		PathPrefix: "/app",
		TargetHost: devURL.Host,
		TargetPath: "/dashboard",
	}

	// A real server rather than an httptest.Recorder: ReverseProxy reaches for
	// CloseNotify on the ResponseWriter, and gin's wrapper panics when the
	// writer underneath it is a recorder.
	engine := gin.New()
	engine.POST("/app/*rest", func(c *gin.Context) {
		srv.proxyToDevServer(c, rule, c.Request.URL.Path)
	})

	front := httptest.NewServer(engine)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/app/users/42", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = "dbbat.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request the proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "from the dev server" {
		t.Errorf("body = %q, want the dev server's response", body)
	}

	select {
	case s := <-got:
		// The prefix is swapped for the target path, suffix preserved.
		if want := "/dashboard/users/42"; s.path != want {
			t.Errorf("path = %q, want %q", s.path, want)
		}
		// SetURL would have rewritten this to the dev server's host; the
		// Director-based original passed the client's Host through, and dev
		// servers key virtual hosts off it.
		if want := "dbbat.example.com"; s.host != want {
			t.Errorf("Host = %q, want the client's own %q", s.host, want)
		}
		if s.xff == "" {
			t.Error("X-Forwarded-For is empty, want the client address")
		}
		if s.method != http.MethodPost {
			t.Errorf("method = %q, want %q", s.method, http.MethodPost)
		}
		if s.body != "payload" {
			t.Errorf("body = %q, want the request body forwarded", s.body)
		}
	default:
		t.Fatal("the dev server was never reached")
	}
}
