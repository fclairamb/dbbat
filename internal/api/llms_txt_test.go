package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/config"
)

func TestHandleLLMsTxt(t *testing.T) {
	t.Parallel()

	s := &Server{}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/llms.txt", nil)

	// No auth middleware is registered in this test at all, which is the
	// point: GET /llms.txt must be reachable without any credentials.
	s.handleLLMsTxt(c)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain prefix", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, "https://dbbat.com/llms.txt") {
		t.Fatalf("body does not link the canonical https://dbbat.com/llms.txt index:\n%s", body)
	}
	if !strings.Contains(body, "https://dbbat.com/llms-full.txt") {
		t.Fatalf("body does not link https://dbbat.com/llms-full.txt:\n%s", body)
	}

	// Must not leak instance topology: no listen addresses / enabled proxies.
	for _, leaky := range []string{"DBB_LISTEN", ":5433", ":1522", ":3307", ":27018"} {
		if strings.Contains(body, leaky) {
			t.Fatalf("body leaks instance topology (%q):\n%s", leaky, body)
		}
	}
}

func TestHandleLLMsTxtRespectsBaseURL(t *testing.T) {
	t.Parallel()

	s := &Server{config: &config.Config{BaseURL: "/custom-base"}}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/llms.txt", nil)

	s.handleLLMsTxt(c)

	body := w.Body.String()
	if !strings.Contains(body, "(/custom-base/)") {
		t.Fatalf("body does not respect configured BaseURL:\n%s", body)
	}
}
