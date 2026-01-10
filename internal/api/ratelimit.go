package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/config"
)

// RateLimiter implements a sliding window rate limiter
type RateLimiter struct {
	mu sync.RWMutex

	// Configuration
	enabled               bool
	requestsPerMinute     int
	requestsPerMinuteAnon int
	burst                 int

	// Sliding window storage: key -> list of request timestamps
	windows map[string]*slidingWindow
}

// slidingWindow tracks requests in a sliding time window
type slidingWindow struct {
	mu         sync.Mutex
	timestamps []time.Time
}

// NewRateLimiter creates a new rate limiter with the given configuration
func NewRateLimiter(cfg config.RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		enabled:               cfg.Enabled,
		requestsPerMinute:     cfg.RequestsPerMinute,
		requestsPerMinuteAnon: cfg.RequestsPerMinuteAnon,
		burst:                 cfg.Burst,
		windows:               make(map[string]*slidingWindow),
	}

	// Start cleanup goroutine
	go rl.cleanup()

	return rl
}

// cleanup periodically removes old entries from the windows map
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for key, window := range rl.windows {
			window.mu.Lock()
			// Remove if all timestamps are older than 1 minute
			if len(window.timestamps) == 0 {
				delete(rl.windows, key)
			} else if now.Sub(window.timestamps[len(window.timestamps)-1]) > time.Minute {
				delete(rl.windows, key)
			}
			window.mu.Unlock()
		}
		rl.mu.Unlock()
	}
}

// getWindow gets or creates a sliding window for the given key
func (rl *RateLimiter) getWindow(key string) *slidingWindow {
	rl.mu.RLock()
	window, exists := rl.windows[key]
	rl.mu.RUnlock()

	if exists {
		return window
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Double-check after acquiring write lock
	if window, exists = rl.windows[key]; exists {
		return window
	}

	window = &slidingWindow{
		timestamps: make([]time.Time, 0, 100),
	}
	rl.windows[key] = window
	return window
}

// check checks if a request is allowed and records it if so
// Returns: allowed, remaining, resetTime
func (rl *RateLimiter) check(key string, limit int) (bool, int, time.Time) {
	window := rl.getWindow(key)
	window.mu.Lock()
	defer window.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-time.Minute)

	// Remove old timestamps outside the window
	validStart := 0
	for i, ts := range window.timestamps {
		if ts.After(windowStart) {
			validStart = i
			break
		}
		if i == len(window.timestamps)-1 {
			validStart = len(window.timestamps)
		}
	}
	window.timestamps = window.timestamps[validStart:]

	// Calculate effective limit with burst
	effectiveLimit := limit + rl.burst

	// Check if request is allowed
	currentCount := len(window.timestamps)
	remaining := effectiveLimit - currentCount - 1
	if remaining < 0 {
		remaining = 0
	}

	// Calculate reset time (when the oldest request in window expires)
	resetTime := now.Add(time.Minute)
	if len(window.timestamps) > 0 {
		resetTime = window.timestamps[0].Add(time.Minute)
	}

	if currentCount >= effectiveLimit {
		return false, 0, resetTime
	}

	// Record this request
	window.timestamps = append(window.timestamps, now)

	return true, remaining, resetTime
}

// Middleware returns a Gin middleware for rate limiting
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rl.enabled {
			c.Next()
			return
		}

		// Determine the rate limit key and limit based on authentication
		var key string
		var limit int

		user := getCurrentUser(c)
		if user != nil {
			// Authenticated request - rate limit by user ID
			// Check if user is exempt
			if user.RateLimitExempt {
				c.Next()
				return
			}
			key = "user:" + user.UID.String()
			limit = rl.requestsPerMinute
		} else {
			// Unauthenticated request - rate limit by IP
			key = "ip:" + c.ClientIP()
			limit = rl.requestsPerMinuteAnon
		}

		allowed, remaining, resetTime := rl.check(key, limit)

		// Set rate limit headers
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Header("X-RateLimit-Reset", strconv.FormatInt(resetTime.Unix(), 10))

		if !allowed {
			retryAfter := int(time.Until(resetTime).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}

			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate_limit_exceeded",
				"message":     "Too many requests. Please retry after " + strconv.Itoa(retryAfter) + " seconds.",
				"retry_after": retryAfter,
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// PostAuthMiddleware is a rate limiter middleware that runs after authentication
// It uses the authenticated user ID for rate limiting
func (rl *RateLimiter) PostAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rl.enabled {
			c.Next()
			return
		}

		user := getCurrentUser(c)
		if user == nil {
			// No user in context means auth failed or this is before auth
			// Skip rate limiting here, let the pre-auth middleware handle it
			c.Next()
			return
		}

		// Check if user is exempt
		if user.RateLimitExempt {
			c.Next()
			return
		}

		key := "user:" + user.UID.String()
		limit := rl.requestsPerMinute

		allowed, remaining, resetTime := rl.check(key, limit)

		// Set rate limit headers
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Header("X-RateLimit-Reset", strconv.FormatInt(resetTime.Unix(), 10))

		if !allowed {
			retryAfter := int(time.Until(resetTime).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}

			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate_limit_exceeded",
				"message":     "Too many requests. Please retry after " + strconv.Itoa(retryAfter) + " seconds.",
				"retry_after": retryAfter,
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// PreAuthMiddleware is a rate limiter middleware that runs before authentication
// It rate limits by IP for unauthenticated requests
func (rl *RateLimiter) PreAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rl.enabled {
			c.Next()
			return
		}

		// Rate limit by IP before authentication
		key := "ip:" + c.ClientIP()
		limit := rl.requestsPerMinuteAnon

		allowed, remaining, resetTime := rl.check(key, limit)

		// Set rate limit headers (may be overwritten by PostAuthMiddleware)
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Header("X-RateLimit-Reset", strconv.FormatInt(resetTime.Unix(), 10))

		if !allowed {
			retryAfter := int(time.Until(resetTime).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}

			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate_limit_exceeded",
				"message":     "Too many requests. Please retry after " + strconv.Itoa(retryAfter) + " seconds.",
				"retry_after": retryAfter,
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// GetStats returns statistics for a given key (for testing/debugging)
func (rl *RateLimiter) GetStats(userID *uuid.UUID, ip string) (int, time.Time) {
	var key string
	if userID != nil {
		key = "user:" + userID.String()
	} else {
		key = "ip:" + ip
	}

	rl.mu.RLock()
	window, exists := rl.windows[key]
	rl.mu.RUnlock()

	if !exists {
		return 0, time.Now().Add(time.Minute)
	}

	window.mu.Lock()
	defer window.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-time.Minute)

	// Count valid timestamps
	count := 0
	for _, ts := range window.timestamps {
		if ts.After(windowStart) {
			count++
		}
	}

	resetTime := now.Add(time.Minute)
	if len(window.timestamps) > 0 {
		for _, ts := range window.timestamps {
			if ts.After(windowStart) {
				resetTime = ts.Add(time.Minute)
				break
			}
		}
	}

	return count, resetTime
}
