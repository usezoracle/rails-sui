package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/usezoracle/rails-sui/config"
)

func TestRedisOptions_PrefersURL(t *testing.T) {
	opts, err := redisOptions(config.RedisConfiguration{
		URL:  "redis://default:secret@my-redis.railway.internal:6380/2",
		Host: "localhost", Port: "6379", // ignored when URL is set
	})
	assert.NoError(t, err)
	assert.Equal(t, "my-redis.railway.internal:6380", opts.Addr)
	assert.Equal(t, "secret", opts.Password)
	assert.Equal(t, 2, opts.DB)
}

func TestRedisOptions_TLSScheme(t *testing.T) {
	opts, err := redisOptions(config.RedisConfiguration{URL: "rediss://h:6379"})
	assert.NoError(t, err)
	assert.NotNil(t, opts.TLSConfig, "rediss:// enables TLS")
}

func TestRedisOptions_FallsBackToParts(t *testing.T) {
	opts, err := redisOptions(config.RedisConfiguration{
		Host: "localhost", Port: "6379", Password: "pw", DB: 0,
	})
	assert.NoError(t, err)
	assert.Equal(t, "localhost:6379", opts.Addr)
	assert.Equal(t, "pw", opts.Password)
}

func TestRedisOptions_InvalidURL(t *testing.T) {
	_, err := redisOptions(config.RedisConfiguration{URL: "not-a-url"})
	assert.Error(t, err)
}
