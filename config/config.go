package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/viper"
)

// Configuration type
type Configuration struct {
	Server       ServerConfiguration
	Database     DatabaseConfiguration
	Auth         AuthConfiguration
	Order        OrderConfiguration
	Notification NotificationConfiguration
}

// SetupConfig configuration
func SetupConfig() error {
	var configuration *Configuration

	viper.AddConfigPath("../..")
	viper.AddConfigPath("..")
	viper.AddConfigPath(".")

	envFilePath := os.Getenv("ENV_FILE_PATH")
	if envFilePath == "" {
		envFilePath = ".env" // Set default value to ".env"
	}

	viper.SetConfigName(envFilePath)
	viper.SetConfigType("env")

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		// A missing .env is expected in containers (Railway, Docker) where env
		// vars are injected directly — fall back to AutomaticEnv + defaults.
		// Only a malformed/unreadable file is fatal.
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && !os.IsNotExist(err) {
			fmt.Printf("Error to reading config file, %s", err)
			return err
		}
	}

	if err := viper.Unmarshal(&configuration); err != nil {
		fmt.Printf("error to decode, %v", err)
		return err
	}
	return nil
}
