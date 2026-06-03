package config

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

func TestDBConfig_PrefersDatabaseURL(t *testing.T) {
	viper.Set("DATABASE_URL", "postgresql://u:p@host/db?sslmode=require")
	viper.Set("DB_URL", "")
	defer viper.Set("DATABASE_URL", "")

	assert.Equal(t, "postgresql://u:p@host/db?sslmode=require", DBConfig())
}

func TestDBConfig_DBURLAlias(t *testing.T) {
	viper.Set("DATABASE_URL", "")
	viper.Set("DB_URL", "postgresql://a:b@h2/db2?sslmode=require")
	defer viper.Set("DB_URL", "")

	assert.Equal(t, "postgresql://a:b@h2/db2?sslmode=require", DBConfig())
}

func TestDBConfig_FallsBackToParts(t *testing.T) {
	viper.Set("DATABASE_URL", "")
	viper.Set("DB_URL", "")
	viper.Set("DB_USER", "postgres")
	viper.Set("DB_PASSWORD", "secret")
	viper.Set("DB_HOST", "localhost")
	viper.Set("DB_PORT", "5432")
	viper.Set("DB_NAME", "rails")
	viper.Set("SSL_MODE", "disable")

	assert.Equal(t, "postgresql://postgres:secret@localhost:5432/rails?sslmode=disable", DBConfig())
}
