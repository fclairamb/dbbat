package api

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

// A dev server's whole point is HMR, which rides a websocket, so the proxy has
// to carry a protocol upgrade end to end: the 101 handshake, then bytes in both
// directions over the hijacked connection. httputil.ReverseProxy has done this
// natively since Go 1.12 (it re-adds Connection/Upgrade after stripping the
// hop-by-hop headers, before Rewrite runs), and this test is what lets the dead
// `ModifyResponse` hook that used to sit under a "// Handle WebSocket upgrades"
// comment stay deleted. A raw handshake rather than a websocket library: the
// upgrade mechanics are what is under test, not RFC 6455 framing.
func TestProxyToDevServerCarriesWebSocketUpgrades(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	type upgradeSeen struct {
		path       string
		query      string
		connection string
		upgrade    string
	}

	got := make(chan upgradeSeen, 1)

	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got <- upgradeSeen{
			path:       req.URL.Path,
			query:      req.URL.RawQuery,
			connection: req.Header.Get("Connection"),
			upgrade:    req.Header.Get("Upgrade"),
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "not hijackable", http.StatusInternalServerError)

			return
		}

		conn, buf, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		if _, err := buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}

		if err := buf.Flush(); err != nil {
			return
		}

		// Echo one line back, so the test proves bytes flow both ways over the
		// upgraded connection rather than only that the handshake succeeded.
		line, err := buf.ReadString('\n')
		if err != nil {
			return
		}

		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
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
		TargetPath: "/",
	}

	engine := gin.New()
	engine.GET("/app/*rest", func(c *gin.Context) {
		srv.proxyToDevServer(c, rule, c.Request.URL.Path)
	})

	front := httptest.NewServer(engine)
	defer front.Close()

	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("parse front URL: %v", err)
	}

	// Raw connection: a normal http.Client would hand back the 101 without the
	// hijacked byte stream underneath it.
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// The query rides along deliberately: Vite gates its HMR socket on a
	// `?token=` it puts in the client's URL, so a Rewrite that rebuilt the
	// outbound URL without the query would turn every real HMR connection into
	// a 400 while a bare-path test still passed.
	if _, err := conn.Write([]byte("GET /app/@vite/client?token=s3cret&foo=bar HTTP/1.1\r\n" +
		"Host: dbbat.example.com\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: ZmFrZWtleWZha2VrZXkxMg==\r\n\r\n")); err != nil {
		t.Fatalf("write the handshake: %v", err)
	}

	reader := bufio.NewReader(conn)

	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the handshake response: %v", err)
	}

	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want a 101 Switching Protocols", strings.TrimSpace(status))
	}

	// Drain the response headers up to the blank line.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the handshake headers: %v", err)
		}

		if strings.TrimSpace(line) == "" {
			break
		}
	}

	if _, err := conn.Write([]byte("hmr-ping\n")); err != nil {
		t.Fatalf("write over the upgraded connection: %v", err)
	}

	echoed, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read over the upgraded connection: %v", err)
	}

	if want := "echo:hmr-ping\n"; echoed != want {
		t.Errorf("echoed = %q, want %q", echoed, want)
	}

	select {
	case s := <-got:
		if want := "/@vite/client"; s.path != want {
			t.Errorf("path = %q, want %q", s.path, want)
		}

		if want := "token=s3cret&foo=bar"; s.query != want {
			t.Errorf("query = %q, want %q", s.query, want)
		}
		// ReverseProxy strips the hop-by-hop headers and then re-adds these
		// two; if that ever stopped happening the dev server would answer with
		// a plain 200 and HMR would silently never reconnect.
		if !strings.EqualFold(s.connection, "Upgrade") {
			t.Errorf("Connection = %q, want Upgrade", s.connection)
		}

		if !strings.EqualFold(s.upgrade, "websocket") {
			t.Errorf("Upgrade = %q, want websocket", s.upgrade)
		}
	default:
		t.Fatal("the dev server was never reached")
	}
}
