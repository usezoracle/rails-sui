package accounts

import (
	"context"
	"fmt"
	"time"

	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// otpMaxAttempts is how many wrong codes we tolerate per (email, scope) before
// every further attempt is rejected until the window expires or a new code is
// issued. A 6-digit OTP has 10^6 values, so 5 guesses leaks a ~5-in-a-million
// chance per issued code — negligible, while still forgiving of fat-fingers.
const otpMaxAttempts = 5

// otpGuardKey namespaces the per-(scope,email) failure counter in Redis.
func otpGuardKey(scope, email string) string {
	return fmt.Sprintf("otp_attempts:%s:%s", scope, email)
}

// otpAttemptsExceeded reports whether the failure counter is already at/over the
// cap. Fails OPEN on a Redis error: the per-IP rate limit on the endpoint is the
// backstop, and a Redis blip must not lock every user out of verification.
func otpAttemptsExceeded(ctx context.Context, scope, email string) bool {
	if db.RedisClient == nil {
		return false
	}
	n, err := db.RedisClient.Get(ctx, otpGuardKey(scope, email)).Int()
	if err != nil {
		// redis.Nil (no key) or a transport error → treat as "not exceeded".
		return false
	}
	return n >= otpMaxAttempts
}

// recordOTPFailure increments the failure counter, setting the window TTL to the
// code's own lifespan on first failure so the lock clears when the code would
// have expired anyway.
func recordOTPFailure(ctx context.Context, scope, email string, ttl time.Duration) {
	if db.RedisClient == nil {
		return
	}
	key := otpGuardKey(scope, email)
	n, err := db.RedisClient.Incr(ctx, key).Result()
	if err != nil {
		logger.Errorf("otp guard incr %s: %v", key, err)
		return
	}
	if n == 1 {
		if err := db.RedisClient.Expire(ctx, key, ttl).Err(); err != nil {
			logger.Errorf("otp guard expire %s: %v", key, err)
		}
	}
}

// clearOTPAttempts resets the counter — called when a code is issued (fresh
// window) and when one is consumed successfully.
func clearOTPAttempts(ctx context.Context, scope, email string) {
	if db.RedisClient == nil {
		return
	}
	if err := db.RedisClient.Del(ctx, otpGuardKey(scope, email)).Err(); err != nil {
		logger.Errorf("otp guard del %s: %v", otpGuardKey(scope, email), err)
	}
}
