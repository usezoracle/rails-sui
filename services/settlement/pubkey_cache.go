package settlement

import (
	"context"
	"sync"
	"time"
)

// pubkeyCache caches the aggregator's PEM with a TTL. Refresh is lazy —
// next caller after expiry triggers the refetch; concurrent callers
// during refresh see the stale value (acceptable: pubkeys rotate rarely
// and we'd rather serve a slightly-stale key than block on the network).
type pubkeyCache struct {
	mu        sync.RWMutex
	pem       string
	expiresAt time.Time
	ttl       time.Duration
	fetch     func(ctx context.Context) (string, error)
}

func newPubkeyCache(ttl time.Duration, fetch func(ctx context.Context) (string, error)) *pubkeyCache {
	return &pubkeyCache{ttl: ttl, fetch: fetch}
}

// Get returns a cached PEM if still fresh; otherwise refetches.
func (c *pubkeyCache) Get(ctx context.Context) (string, error) {
	c.mu.RLock()
	if c.pem != "" && time.Now().Before(c.expiresAt) {
		pem := c.pem
		c.mu.RUnlock()
		return pem, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Double-check after taking the write lock — another goroutine may
	// have just refreshed.
	if c.pem != "" && time.Now().Before(c.expiresAt) {
		return c.pem, nil
	}
	pem, err := c.fetch(ctx)
	if err != nil {
		// On error, serve stale if we have it. Better to keep going
		// than block all order dispatch on a transient outage.
		if c.pem != "" {
			return c.pem, nil
		}
		return "", err
	}
	c.pem = pem
	c.expiresAt = time.Now().Add(c.ttl)
	return c.pem, nil
}
