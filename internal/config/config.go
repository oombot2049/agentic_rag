package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	keyEnvironment             = "AGENTICRAG_ENVIRONMENT"
	keyServerAddress           = "AGENTICRAG_SERVER_ADDRESS"
	keyServerReadHeaderTimeout = "AGENTICRAG_SERVER_READ_HEADER_TIMEOUT"
	keyServerShutdownTimeout   = "AGENTICRAG_SERVER_SHUTDOWN_TIMEOUT"
	keyPostgresDSN             = "AGENTICRAG_POSTGRES_DSN"
	keyPostgresMaxOpenConns    = "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS"
	keyPostgresMaxIdleConns    = "AGENTICRAG_POSTGRES_MAX_IDLE_CONNS"
	keyPostgresConnLifetime    = "AGENTICRAG_POSTGRES_CONN_MAX_LIFETIME"
	keyRedisAddress            = "AGENTICRAG_REDIS_ADDRESS"
	keyRedisPassword           = "AGENTICRAG_REDIS_PASSWORD"
	keyRedisDB                 = "AGENTICRAG_REDIS_DB"
	keyLogLevel                = "AGENTICRAG_LOG_LEVEL"
)

var knownKeys = []string{
	keyEnvironment, keyServerAddress, keyServerReadHeaderTimeout,
	keyServerShutdownTimeout, keyPostgresDSN, keyPostgresMaxOpenConns,
	keyPostgresMaxIdleConns, keyPostgresConnLifetime, keyRedisAddress,
	keyRedisPassword, keyRedisDB, keyLogLevel,
}

// Environment identifies the deployment behavior class.
type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentTest        Environment = "test"
	EnvironmentProduction  Environment = "production"
)

// LogLevel is the minimum structured log level.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

type ServerConfig struct {
	Address           string
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
}

type PostgresConfig struct {
	DSN             Secret
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type RedisConfig struct {
	Address  string
	Password Secret
	DB       int
}

type LoggingConfig struct {
	Level LogLevel
}

// Config is the process configuration after parsing and validation.
type Config struct {
	Environment Environment
	Server      ServerConfig
	Postgres    PostgresConfig
	Redis       RedisConfig
	Logging     LoggingConfig
}

// Defaults returns non-secret development defaults. PostgreSQL DSN remains
// empty and therefore requires an explicit source before validation succeeds.
func Defaults() Config {
	return Config{
		Environment: EnvironmentDevelopment,
		Server: ServerConfig{
			Address:           ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ShutdownTimeout:   15 * time.Second,
		},
		Postgres: PostgresConfig{
			MaxOpenConns:    50,
			MaxIdleConns:    10,
			ConnMaxLifetime: 30 * time.Minute,
		},
		Redis:   RedisConfig{Address: "127.0.0.1:6379"},
		Logging: LoggingConfig{Level: LogLevelInfo},
	}
}

// Source provides configuration values. The bool distinguishes an absent key
// from a deliberately supplied empty value.
type Source interface {
	Lookup(key string) (string, bool)
}

type sourceFunc func(string) (string, bool)

func (f sourceFunc) Lookup(key string) (string, bool) { return f(key) }

// EnvironmentSource reads the current process environment lazily.
func EnvironmentSource() Source { return sourceFunc(os.LookupEnv) }

// MapSource creates a deterministic source, primarily for tests and adapters.
type MapSource map[string]string

func (s MapSource) Lookup(key string) (string, bool) {
	value, ok := s[key]
	return value, ok
}

// Load reads defaults followed by the process environment.
func Load() (Config, error) {
	return LoadSources(EnvironmentSource())
}

// LoadSources overlays sources from left to right, then validates once.
func LoadSources(sources ...Source) (Config, error) {
	cfg := Defaults()
	values := make(map[string]string, len(knownKeys))
	for _, source := range sources {
		if source == nil {
			continue
		}
		for _, key := range knownKeys {
			if value, ok := source.Lookup(key); ok {
				values[key] = value
			}
		}
	}

	var issues []FieldError
	applyString(values, keyEnvironment, func(value string) { cfg.Environment = Environment(value) })
	applyString(values, keyServerAddress, func(value string) { cfg.Server.Address = value })
	applyDuration(values, keyServerReadHeaderTimeout, &cfg.Server.ReadHeaderTimeout, &issues)
	applyDuration(values, keyServerShutdownTimeout, &cfg.Server.ShutdownTimeout, &issues)
	applyString(values, keyPostgresDSN, func(value string) { cfg.Postgres.DSN = NewSecret(value) })
	applyInt(values, keyPostgresMaxOpenConns, &cfg.Postgres.MaxOpenConns, &issues)
	applyInt(values, keyPostgresMaxIdleConns, &cfg.Postgres.MaxIdleConns, &issues)
	applyDuration(values, keyPostgresConnLifetime, &cfg.Postgres.ConnMaxLifetime, &issues)
	applyString(values, keyRedisAddress, func(value string) { cfg.Redis.Address = value })
	applyString(values, keyRedisPassword, func(value string) { cfg.Redis.Password = NewSecret(value) })
	applyInt(values, keyRedisDB, &cfg.Redis.DB, &issues)
	applyString(values, keyLogLevel, func(value string) { cfg.Logging.Level = LogLevel(value) })

	issues = append(issues, cfg.validate()...)
	if len(issues) > 0 {
		return Config{}, newValidationError(issues)
	}
	return cfg, nil
}

func applyString(values map[string]string, key string, set func(string)) {
	if value, ok := values[key]; ok {
		set(strings.TrimSpace(value))
	}
}

func applyDuration(values map[string]string, key string, target *time.Duration, issues *[]FieldError) {
	value, ok := values[key]
	if !ok {
		return
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		*issues = append(*issues, FieldError{Key: key, Rule: "must be a valid duration"})
		return
	}
	*target = parsed
}

func applyInt(values map[string]string, key string, target *int, issues *[]FieldError) {
	value, ok := values[key]
	if !ok {
		return
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		*issues = append(*issues, FieldError{Key: key, Rule: "must be a base-10 integer"})
		return
	}
	*target = parsed
}

func (c Config) validate() []FieldError {
	var issues []FieldError
	if !oneOf(string(c.Environment), "development", "test", "production") {
		issues = append(issues, FieldError{Key: keyEnvironment, Rule: "must be development, test, or production"})
	}
	validateAddress(&issues, keyServerAddress, c.Server.Address)
	validateRange(&issues, keyServerReadHeaderTimeout, c.Server.ReadHeaderTimeout, 100*time.Millisecond, 30*time.Second)
	validateRange(&issues, keyServerShutdownTimeout, c.Server.ShutdownTimeout, time.Second, 2*time.Minute)
	if c.Postgres.DSN.IsZero() {
		issues = append(issues, FieldError{Key: keyPostgresDSN, Rule: "is required"})
	}
	validateIntRange(&issues, keyPostgresMaxOpenConns, c.Postgres.MaxOpenConns, 1, 500)
	validateIntRange(&issues, keyPostgresMaxIdleConns, c.Postgres.MaxIdleConns, 0, 500)
	if c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		issues = append(issues, FieldError{Key: keyPostgresMaxIdleConns, Rule: "must not exceed max open connections"})
	}
	validateRange(&issues, keyPostgresConnLifetime, c.Postgres.ConnMaxLifetime, time.Minute, 24*time.Hour)
	validateAddress(&issues, keyRedisAddress, c.Redis.Address)
	validateIntRange(&issues, keyRedisDB, c.Redis.DB, 0, 15)
	if !oneOf(string(c.Logging.Level), "debug", "info", "warn", "error") {
		issues = append(issues, FieldError{Key: keyLogLevel, Rule: "must be debug, info, warn, or error"})
	}
	return issues
}

func validateAddress(issues *[]FieldError, key, value string) {
	if _, _, err := net.SplitHostPort(value); err != nil {
		*issues = append(*issues, FieldError{Key: key, Rule: "must be a valid TCP host:port"})
	}
}

func validateRange(issues *[]FieldError, key string, value, minimum, maximum time.Duration) {
	if value < minimum || value > maximum {
		*issues = append(*issues, FieldError{Key: key, Rule: fmt.Sprintf("must be between %s and %s", minimum, maximum)})
	}
}

func validateIntRange(issues *[]FieldError, key string, value, minimum, maximum int) {
	if value < minimum || value > maximum {
		*issues = append(*issues, FieldError{Key: key, Rule: fmt.Sprintf("must be between %d and %d", minimum, maximum)})
	}
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// SafeFields returns an allowlist suitable for structured startup logs.
func (c Config) SafeFields() map[string]any {
	return map[string]any{
		"environment":                c.Environment,
		"server.address":             c.Server.Address,
		"server.read_header_timeout": c.Server.ReadHeaderTimeout,
		"server.shutdown_timeout":    c.Server.ShutdownTimeout,
		"postgres.max_open_conns":    c.Postgres.MaxOpenConns,
		"postgres.max_idle_conns":    c.Postgres.MaxIdleConns,
		"postgres.conn_max_lifetime": c.Postgres.ConnMaxLifetime,
		"redis.address":              c.Redis.Address,
		"redis.db":                   c.Redis.DB,
		"log.level":                  c.Logging.Level,
	}
}

// FieldError describes one invalid key without retaining its supplied value.
type FieldError struct {
	Key  string
	Rule string
}

func (e FieldError) Error() string { return e.Key + " " + e.Rule }

// ValidationError contains every parsing and validation issue in stable order.
type ValidationError struct {
	Fields []FieldError
}

func newValidationError(fields []FieldError) *ValidationError {
	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].Key == fields[j].Key {
			return fields[i].Rule < fields[j].Rule
		}
		return fields[i].Key < fields[j].Key
	})
	return &ValidationError{Fields: fields}
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		parts = append(parts, field.Error())
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// IsValidationError supports callers that only need a category check while
// errors.As remains available for field-level details.
func IsValidationError(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}
