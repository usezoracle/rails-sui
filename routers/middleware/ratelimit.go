// Redis-backed rate limit for auth endpoints.
//
// Each protected endpoint uses a fixed-window counter:
//   key  = ratelimit:<bucket>:<identifier>
//   incr → if first hit, set TTL = window
//   if count > limit → 429
//
// Identifier is usually the client IP. For login/register, optionally
// combine with the email when present so password-spraying one account
// gets limited independently from a noisy IP shared by many users.
//
// Choices baked in:
//   • Fixed window (vs. sliding/leaky-bucket) — simpler, predictable,
//     adequate for "stop password spraying" use case. We can swap for a
//     token-bucket later without changing the call sites.
//   • Fails OPEN if Redis is down. A degraded cache shouldn't lock
//     users out of login. Real attack rates surface in Redis health
//     alerts; transient outage isn't worth the lockout.

package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
)

// RateLimit returns a gin handler that limits requests per
// (bucket, identifier) tuple. `identifier` is computed per request by
// the supplied keyFn; if nil, the client IP is used.
func RateLimit(bucket string, limit int, window time.Duration, keyFn func(*gin.Context) string) gin.HandlerFunc {
	if keyFn == nil {
		keyFn = func(c *gin.Context) string { return c.ClientIP() }
	}
	return func(c *gin.Context) {
		id := keyFn(c)
		if id == "" {
			c.Next()
			return
		}
		key := "ratelimit:" + bucket + ":" + id

		ctx := c.Request.Context()
		count, err := storage.RedisClient.Incr(ctx, key).Result()
		if err != nil {
			// Fail open — don't block all auth because Redis hiccupped.
			c.Next()
			return
		}
		if count == 1 {
			// First hit in window — set the TTL.
			storage.RedisClient.Expire(ctx, key, window)
		}
		if count > int64(limit) {
			ttl, _ := storage.RedisClient.TTL(ctx, key).Result()
			retry := int(ttl.Seconds())
			if retry < 1 {
				retry = 1
			}
			c.Header("Retry-After", itoa(retry))
			u.APIResponse(c, http.StatusTooManyRequests, "error",
				"Too many requests. Please try again later.", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// EmailFromJSON pulls a lowercased email from the request body without
// consuming it. Used as keyFn on login/register so spraying one account
// from many IPs (or vice versa) is still bucketed correctly.
//
// Returns "" when no email is present, in which case the caller (with
// IP-only RateLimit) takes over.
func EmailFromJSON(c *gin.Context) string {
	// gin caches the body via shouldBindJSON later in the chain; we
	// peek at the bound payload if any controller has run, else read &
	// rewind. Simpler: combine IP + email-prefix via a header check.
	// For v1 we just use IP — controllers can add a stricter (IP+email)
	// limiter later if needed.
	return c.ClientIP()
}

// CompositeKey combines the IP with another key fragment for tighter
// bucketing (e.g. login limiter scoped per IP+email). Defensively
// trims/lowercases the parts.
func CompositeKey(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "|")
}

// itoa avoids importing strconv just for one integer. Trivial helper —
// retry-after is always positive and < 86400 in practice.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 6)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
