package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// DatabaseConfiguration type defines the server configurations
type DatabaseConfiguration struct {
	Driver   string
	Dbname   string
	Username string
	Password string
	Host     string
	Port     string
	LogMode  bool
}

// DBConfig returns the Postgres DSN. A full connection string takes precedence:
// managed providers (Neon, Railway, Heroku) hand you a complete URL that already
// carries sslmode=require — using it verbatim avoids the "connection is insecure"
// failure you get when the discrete parts default to sslmode=disable. Falls back
// to assembling the DSN from discrete DB_* vars for local dev.
func DBConfig() (DSN string) {
	if url := strings.TrimSpace(viper.GetString("DATABASE_URL")); url != "" {
		return url
	}
	if url := strings.TrimSpace(viper.GetString("DB_URL")); url != "" {
		return url
	}

	DbName := viper.GetString("DB_NAME")
	DbUser := viper.GetString("DB_USER")
	DbPassword := viper.GetString("DB_PASSWORD")
	DbHost := viper.GetString("DB_HOST")
	DbPort := viper.GetString("DB_PORT")
	DbSslMode := viper.GetString("SSL_MODE")
	if DbSslMode == "" {
		DbSslMode = "disable" // local default; managed DBs should use DATABASE_URL
	}

	DSN = fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=%s", DbUser, DbPassword, DbHost, DbPort, DbName, DbSslMode)

	return
}

func init() {
	if err := SetupConfig(); err != nil {
		panic(fmt.Sprintf("config SetupConfig() error: %s", err))
	}
}
