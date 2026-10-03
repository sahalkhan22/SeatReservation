// Package config loads all runtime settings from the environment.
// Every value has a dev default except the two secrets, which are required
// so the service refuses to start half-configured.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port            string
	DatabaseURL     string
	JWTSecret       string
	MaxSeatsPerUser int
	DBPoolMax       int32
	QueryTimeout    time.Duration
	EnableDevToken  bool
	UnsafeMode      bool
}

func Load() (Config, error) {
	c := Config{
		Port:        str("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.JWTSecret == "" {
		return c, fmt.Errorf("JWT_SECRET is required")
	}

	var err error
	if c.MaxSeatsPerUser, err = num("MAX_SEATS_PER_USER", 4); err != nil {
		return c, err
	}
	poolMax, err := num("DB_POOL_MAX", 20)
	if err != nil {
		return c, err
	}
	c.DBPoolMax = int32(poolMax)

	if c.QueryTimeout, err = dur("QUERY_TIMEOUT", 2*time.Second); err != nil {
		return c, err
	}
	c.EnableDevToken = boolean("ENABLE_DEV_TOKEN", false)
	c.UnsafeMode = boolean("UNSAFE_MODE", false)

	return c, nil
}

func str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func num(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, v)
	}
	return n, nil
}

func dur(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 2s, got %q", key, v)
	}
	return d, nil
}

func boolean(key string, def bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE":
		return true
	case "0", "false", "FALSE":
		return false
	}
	return def
}
