package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/usezoracle/rails-sui/config"
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

// InitializeRedis initializes the Redis client
func InitializeRedis() error {
	opts, err := redisOptions(config.RedisConfig())
	if err != nil {
		return err
	}
	RedisClient = redis.NewClient(opts)

	// Ping Redis to check the connection
	if _, err := RedisClient.Ping(context.Background()).Result(); err != nil {
		return err
	}

	return nil
}
