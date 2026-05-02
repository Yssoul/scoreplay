// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	HTTPAddr          string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	DatabaseURL string
}

// Load reads the environment and returns a validated Config, or an error
// describing every problem found (accumulated, not failed-fast, so an
// operator sees the full picture on a single run).
func Load() (Config, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	databaseURL, err := buildDatabaseURL()
	collect(err)

	readHeaderTimeout, err := getDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	collect(err)
	readTimeout, err := getDuration("HTTP_READ_TIMEOUT", 15*time.Second)
	collect(err)
	writeTimeout, err := getDuration("HTTP_WRITE_TIMEOUT", 15*time.Second)
	collect(err)
	idleTimeout, err := getDuration("HTTP_IDLE_TIMEOUT", 60*time.Second)
	collect(err)
	shutdownTimeout, err := getDuration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second)
	collect(err)

	cfg := Config{
		HTTPAddr:          getEnv("HTTP_ADDR", ":8080"),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ShutdownTimeout:   shutdownTimeout,
		DatabaseURL:       databaseURL,
	}

	if cfg.HTTPAddr == "" {
		errs = append(errs, errors.New("HTTP_ADDR must not be empty"))
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// buildDatabaseURL assembles a Postgres connection string from the libpq-style
// PG* environment variables so psql, pg_isready, tern and the app all read
// the same configuration.
func buildDatabaseURL() (string, error) {
	required := map[string]string{
		"PGHOST":     os.Getenv("PGHOST"),
		"PGPORT":     os.Getenv("PGPORT"),
		"PGUSER":     os.Getenv("PGUSER"),
		"PGPASSWORD": os.Getenv("PGPASSWORD"),
		"PGDATABASE": os.Getenv("PGDATABASE"),
	}
	var missing []string
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(required["PGUSER"], required["PGPASSWORD"]),
		Host:   required["PGHOST"] + ":" + required["PGPORT"],
		Path:   "/" + required["PGDATABASE"],
	}
	q := u.Query()
	q.Set("sslmode", getEnv("PGSSLMODE", "disable"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// getDuration returns the parsed value of key, or fallback when the var is
// unset. A set-but-unparseable value is a typo-sized operator mistake, not a
// reason to silently use the default: return an error so Load can surface
// it at startup.
func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback, fmt.Errorf("%s=%q is not a valid duration: %w", key, v, err)
	}
	return d, nil
}
