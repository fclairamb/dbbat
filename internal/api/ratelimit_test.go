package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

func TestRateLimiter_Check(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:               true,
		RequestsPerMinute:     5,
		RequestsPerMinuteAnon: 3,
		Burst:                 2,
	})

	t.Run("allows requests within limit", func(t *testing.T) {
		t.Parallel()
		key := "test:within-limit"
		limit := 5

		// With burst of 2, effective limit is 7
		for i := 0; i < 7; i++ {
			allowed, remaining, _ := rl.check(key, limit)
			if !allowed {
				t.Errorf("Request %d should be allowed", i)
			}
			// After each request, remaining = effectiveLimit - currentCount - 1
			// currentCount is i (after check increments it to i+1)
			// So remaining = 7 - (i+1) - 1 = 7 - i - 2 = 5 - i
			// Wait, the implementation calculates remaining BEFORE adding the current request
			// remaining = effectiveLimit - currentCount - 1, where currentCount doesn't include current request yet
			// Actually looking at code: remaining is calculated before the request is added
			expectedRemaining := 7 - i - 1
			if expectedRemaining < 0 {
				expectedRemaining = 0
			}
			if remaining != expectedRemaining {
				t.Errorf("Request %d: remaining = %d, want %d", i, remaining, expectedRemaining)
			}
		}
	})

	t.Run("blocks requests over limit", func(t *testing.T) {
		t.Parallel()
		key := "test:over-limit"
		limit := 5

		// Exhaust the limit (5 + 2 burst = 7)
		for i := 0; i < 7; i++ {
			allowed, _, _ := rl.check(key, limit)
			if !allowed {
				t.Errorf("Request %d should be allowed", i)
			}
		}

		// 8th request should be blocked
		allowed, remaining, _ := rl.check(key, limit)
		if allowed {
			t.Error("8th request should be blocked")
		}
		if remaining != 0 {
			t.Errorf("remaining = %d, want 0", remaining)
		}
	})

	t.Run("resets after window expires", func(t *testing.T) {
		t.Parallel()
		key := "test:reset"
		limit := 2

		// Use all requests (2 + 2 burst = 4)
		for i := 0; i < 4; i++ {
			rl.check(key, limit)
		}

		// Should be blocked
		allowed, _, _ := rl.check(key, limit)
		if allowed {
			t.Error("Request should be blocked")
		}

		// Manually clear the window to simulate time passing
		rl.mu.Lock()
		window := rl.windows[key]
		if window != nil {
			window.mu.Lock()
			window.timestamps = []time.Time{}
			window.mu.Unlock()
		}
		rl.mu.Unlock()

		// Should be allowed again
		allowed, _, _ = rl.check(key, limit)
		if !allowed {
			t.Error("Request should be allowed after window reset")
		}
	})
}

func TestRateLimiter_Disabled(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:               false,
		RequestsPerMinute:     5,
		RequestsPerMinuteAnon: 3,
		Burst:                 2,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(rl.Middleware())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	// Make many requests - all should succeed
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d: status = %d, want %d", i, w.Code, http.StatusOK)
		}
	}
}

func TestRateLimiter_PreAuthMiddleware(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:               true,
		RequestsPerMinute:     60,
		RequestsPerMinuteAnon: 3, // Low limit for testing
		Burst:                 0,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(rl.PreAuthMiddleware())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	// Make requests up to the limit
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d: status = %d, want %d", i, w.Code, http.StatusOK)
		}

		// Check headers
		if w.Header().Get("X-RateLimit-Limit") != "3" {
			t.Errorf("X-RateLimit-Limit = %q, want %q", w.Header().Get("X-RateLimit-Limit"), "3")
		}
	}

	// 4th request should be blocked
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("4th request: status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	// Check Retry-After header
	if w.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header should be set")
	}
}

func TestRateLimiter_PostAuthMiddleware_ExemptUser(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:               true,
		RequestsPerMinute:     2,
		RequestsPerMinuteAnon: 2,
		Burst:                 0,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Middleware to set up exempt user
	router.Use(func(c *gin.Context) {
		user := &store.User{
			RateLimitExempt: true,
		}
		c.Set("current_user", user)
		c.Next()
	})

	router.Use(rl.PostAuthMiddleware())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	// Make many requests - all should succeed for exempt user
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d: status = %d, want %d (exempt user)", i, w.Code, http.StatusOK)
		}
	}
}

func TestRateLimiter_ResponseFormat(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:               true,
		RequestsPerMinute:     60,
		RequestsPerMinuteAnon: 1, // Very low limit
		Burst:                 0,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(rl.PreAuthMiddleware())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	// First request succeeds
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("First request: status = %d, want %d", w.Code, http.StatusOK)
	}

	// Second request should be rate limited
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("Second request: status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	// Check response body contains expected fields
	body := w.Body.String()
	if body == "" {
		t.Error("Response body should not be empty")
	}

	// Check all required headers are present
	requiredHeaders := []string{
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
		"Retry-After",
	}
	for _, header := range requiredHeaders {
		if w.Header().Get(header) == "" {
			t.Errorf("Header %q should be set", header)
		}
	}
}

func TestRateLimiter_DifferentIPs(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:               true,
		RequestsPerMinute:     60,
		RequestsPerMinuteAnon: 2,
		Burst:                 0,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(rl.PreAuthMiddleware())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	// IP 1: Use up all requests
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "1.1.1.1:12345"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("IP1 Request %d: status = %d, want %d", i, w.Code, http.StatusOK)
		}
	}

	// IP 1: Should be blocked
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "1.1.1.1:12345"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("IP1 blocked request: status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	// IP 2: Should still be allowed
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "2.2.2.2:12345"
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("IP2 request: status = %d, want %d", w.Code, http.StatusOK)
	}
}
