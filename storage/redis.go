package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/utils/logger"
)

var (
	// Client holds the Redis client
	RedisClient *redis.Client
)

// redisOptions builds *redis.Options from config. A full REDIS_URL takes
// precedence (managed providers give you one, with TLS via rediss://); else it
// assembles from the discrete REDIS_* parts for local dev.
func redisOptions(conf config.RedisConfiguration) (*redis.Options, error) {
	if url := strings.TrimSpace(conf.URL); url != "" {
		opts, err := redis.ParseURL(url)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URL: %w", err)
		}
		return opts, nil
	}
	return &redis.Options{
		Addr:     fmt.Sprintf("%s:%s", conf.Host, conf.Port),
		Password: conf.Password,
		DB:       conf.DB,
	}, nil
}

// redisPingAttempts and redisPingDelay bound the startup ping retry. Managed
// private networking (e.g. Railway's *.railway.internal) can take a few seconds
// to resolve when the container starts, so we must not Fatal on the first miss.
const (
	redisPingAttempts = 10
	redisPingDelay    = 2 * time.Second
)

// InitializeRedis initializes the Redis client, retrying the initial ping so a
// transient startup DNS/connect failure (private networking warming up) doesn't
// crash the boot.
func InitializeRedis() error {
	opts, err := redisOptions(config.RedisConfig())
	if err != nil {
		return err
	}
	RedisClient = redis.NewClient(opts)

	var lastErr error
	for attempt := 1; attempt <= redisPingAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, lastErr = RedisClient.Ping(ctx).Result()
		cancel()
		if lastErr == nil {
			return nil
		}
		logger.Infof("Redis ping attempt %d/%d failed: %v — retrying in %s", attempt, redisPingAttempts, lastErr, redisPingDelay)
		time.Sleep(redisPingDelay)
	}
	return fmt.Errorf("redis unreachable after %d attempts: %w", redisPingAttempts, lastErr)
}
