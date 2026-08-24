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
	"unicode"
)

const (
	keyEnvironment             = "AGENTICRAG_ENVIRONMENT"
	keyServerAddress           = "AGENTICRAG_SERVER_ADDRESS"
	keyServerReadTimeout       = "AGENTICRAG_SERVER_READ_TIMEOUT"
	keyServerReadHeaderTimeout = "AGENTICRAG_SERVER_READ_HEADER_TIMEOUT"
	keyServerIdleTimeout       = "AGENTICRAG_SERVER_IDLE_TIMEOUT"
	keyServerShutdownTimeout   = "AGENTICRAG_SERVER_SHUTDOWN_TIMEOUT"
	keyPostgresDSN             = "AGENTICRAG_POSTGRES_DSN"
	keyPostgresMaxOpenConns    = "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS"
	keyPostgresMaxIdleConns    = "AGENTICRAG_POSTGRES_MAX_IDLE_CONNS"
	keyPostgresConnLifetime    = "AGENTICRAG_POSTGRES_CONN_MAX_LIFETIME"
	keyPostgresConnectTimeout  = "AGENTICRAG_POSTGRES_CONNECT_TIMEOUT"
	keyPostgresHealthTimeout   = "AGENTICRAG_POSTGRES_HEALTH_TIMEOUT"
	keyRedisAddress            = "AGENTICRAG_REDIS_ADDRESS"
	keyRedisPassword           = "AGENTICRAG_REDIS_PASSWORD"
	keyRedisDB                 = "AGENTICRAG_REDIS_DB"
	keyRedisDialTimeout        = "AGENTICRAG_REDIS_DIAL_TIMEOUT"
	keyRedisReadTimeout        = "AGENTICRAG_REDIS_READ_TIMEOUT"
	keyRedisWriteTimeout       = "AGENTICRAG_REDIS_WRITE_TIMEOUT"
	keyRedisPoolTimeout        = "AGENTICRAG_REDIS_POOL_TIMEOUT"
	keyRedisHealthTimeout      = "AGENTICRAG_REDIS_HEALTH_TIMEOUT"
	keyRedisPoolSize           = "AGENTICRAG_REDIS_POOL_SIZE"
	keyRedisMinIdleConns       = "AGENTICRAG_REDIS_MIN_IDLE_CONNS"
	keyRedisMaxTTL             = "AGENTICRAG_REDIS_MAX_TTL"
	keyRedisOutputTTL          = "AGENTICRAG_REDIS_OUTPUT_TTL"
	keyQueueMaxPendingTenant   = "AGENTICRAG_QUEUE_MAX_PENDING_PER_TENANT"
	keyQueueMaxPendingGlobal   = "AGENTICRAG_QUEUE_MAX_PENDING_GLOBAL"
	keyWorkerID                = "AGENTICRAG_WORKER_ID"
	keyWorkerMaxConcurrency    = "AGENTICRAG_WORKER_MAX_CONCURRENCY"
	keyWorkerMaxPerTenant      = "AGENTICRAG_WORKER_MAX_CONCURRENCY_PER_TENANT"
	keyWorkerPollInterval      = "AGENTICRAG_WORKER_POLL_INTERVAL"
	keyWorkerLeaseDuration     = "AGENTICRAG_WORKER_LEASE_DURATION"
	keyWorkerRenewInterval     = "AGENTICRAG_WORKER_RENEW_INTERVAL"
	keyWorkerRetryDelay        = "AGENTICRAG_WORKER_RETRY_DELAY"
	keyLogLevel                = "AGENTICRAG_LOG_LEVEL"
)

var knownKeys = []string{
	keyEnvironment, keyServerAddress, keyServerReadTimeout, keyServerReadHeaderTimeout, keyServerIdleTimeout,
	keyServerShutdownTimeout, keyPostgresDSN, keyPostgresMaxOpenConns,
	keyPostgresMaxIdleConns, keyPostgresConnLifetime, keyRedisAddress,
	keyPostgresConnectTimeout, keyPostgresHealthTimeout, keyRedisPassword, keyRedisDB,
	keyRedisDialTimeout, keyRedisReadTimeout, keyRedisWriteTimeout, keyRedisPoolTimeout,
	keyRedisHealthTimeout, keyRedisPoolSize, keyRedisMinIdleConns, keyRedisMaxTTL, keyRedisOutputTTL, keyLogLevel,
	keyQueueMaxPendingTenant, keyQueueMaxPendingGlobal,
	keyWorkerID, keyWorkerMaxConcurrency, keyWorkerMaxPerTenant, keyWorkerPollInterval,
	keyWorkerLeaseDuration, keyWorkerRenewInterval, keyWorkerRetryDelay,
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
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

type PostgresConfig struct {
	DSN             Secret
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
	HealthTimeout   time.Duration
}

type RedisConfig struct {
	Address       string
	Password      Secret
	DB            int
	DialTimeout   time.Duration
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	PoolTimeout   time.Duration
	HealthTimeout time.Duration
	PoolSize      int
	MinIdleConns  int
	MaxTTL        time.Duration
	OutputTTL     time.Duration
}

type LoggingConfig struct {
	Level LogLevel
}

type QueueConfig struct {
	MaxPendingPerTenant int
	MaxPendingGlobal    int
}

type WorkerConfig struct {
	ID                      string
	MaxConcurrency          int
	MaxConcurrencyPerTenant int
	PollInterval            time.Duration
	LeaseDuration           time.Duration
	RenewInterval           time.Duration
	RetryDelay              time.Duration
}

// Config is the process configuration after parsing and validation.
type Config struct {
	Environment Environment
	Server      ServerConfig
	Postgres    PostgresConfig
	Redis       RedisConfig
	Queue       QueueConfig
	Worker      WorkerConfig
	Logging     LoggingConfig
}

// Defaults returns non-secret development defaults. PostgreSQL DSN remains
// empty and therefore requires an explicit source before validation succeeds.
func Defaults() Config {
	return Config{
		Environment: EnvironmentDevelopment,
		Server: ServerConfig{
			Address:           ":8080",
			ReadTimeout:       30 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       2 * time.Minute,
			ShutdownTimeout:   15 * time.Second,
		},
		Postgres: PostgresConfig{
			MaxOpenConns:    50,
			MaxIdleConns:    10,
			ConnMaxLifetime: 30 * time.Minute,
			ConnectTimeout:  5 * time.Second,
			HealthTimeout:   2 * time.Second,
		},
		Redis: RedisConfig{
			Address: "127.0.0.1:6379", DialTimeout: 5 * time.Second,
			ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
			PoolTimeout: 2 * time.Second, HealthTimeout: 2 * time.Second,
			PoolSize: 100, MinIdleConns: 5, MaxTTL: 24 * time.Hour, OutputTTL: time.Hour,
		},
		Queue: QueueConfig{MaxPendingPerTenant: 250, MaxPendingGlobal: 6000},
		Worker: WorkerConfig{ID: "worker-1", MaxConcurrency: 32, MaxConcurrencyPerTenant: 4,
			PollInterval: 100 * time.Millisecond, LeaseDuration: 30 * time.Second,
			RenewInterval: 10 * time.Second, RetryDelay: time.Second},
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
	applyDuration(values, keyServerReadTimeout, &cfg.Server.ReadTimeout, &issues)
	applyDuration(values, keyServerReadHeaderTimeout, &cfg.Server.ReadHeaderTimeout, &issues)
	applyDuration(values, keyServerIdleTimeout, &cfg.Server.IdleTimeout, &issues)
	applyDuration(values, keyServerShutdownTimeout, &cfg.Server.ShutdownTimeout, &issues)
	applyString(values, keyPostgresDSN, func(value string) { cfg.Postgres.DSN = NewSecret(value) })
	applyInt(values, keyPostgresMaxOpenConns, &cfg.Postgres.MaxOpenConns, &issues)
	applyInt(values, keyPostgresMaxIdleConns, &cfg.Postgres.MaxIdleConns, &issues)
	applyDuration(values, keyPostgresConnLifetime, &cfg.Postgres.ConnMaxLifetime, &issues)
	applyDuration(values, keyPostgresConnectTimeout, &cfg.Postgres.ConnectTimeout, &issues)
	applyDuration(values, keyPostgresHealthTimeout, &cfg.Postgres.HealthTimeout, &issues)
	applyString(values, keyRedisAddress, func(value string) { cfg.Redis.Address = value })
	applyString(values, keyRedisPassword, func(value string) { cfg.Redis.Password = NewSecret(value) })
	applyInt(values, keyRedisDB, &cfg.Redis.DB, &issues)
	applyDuration(values, keyRedisDialTimeout, &cfg.Redis.DialTimeout, &issues)
	applyDuration(values, keyRedisReadTimeout, &cfg.Redis.ReadTimeout, &issues)
	applyDuration(values, keyRedisWriteTimeout, &cfg.Redis.WriteTimeout, &issues)
	applyDuration(values, keyRedisPoolTimeout, &cfg.Redis.PoolTimeout, &issues)
	applyDuration(values, keyRedisHealthTimeout, &cfg.Redis.HealthTimeout, &issues)
	applyInt(values, keyRedisPoolSize, &cfg.Redis.PoolSize, &issues)
	applyInt(values, keyRedisMinIdleConns, &cfg.Redis.MinIdleConns, &issues)
	applyDuration(values, keyRedisMaxTTL, &cfg.Redis.MaxTTL, &issues)
	applyDuration(values, keyRedisOutputTTL, &cfg.Redis.OutputTTL, &issues)
	applyInt(values, keyQueueMaxPendingTenant, &cfg.Queue.MaxPendingPerTenant, &issues)
	applyInt(values, keyQueueMaxPendingGlobal, &cfg.Queue.MaxPendingGlobal, &issues)
	applyString(values, keyWorkerID, func(value string) { cfg.Worker.ID = value })
	applyInt(values, keyWorkerMaxConcurrency, &cfg.Worker.MaxConcurrency, &issues)
	applyInt(values, keyWorkerMaxPerTenant, &cfg.Worker.MaxConcurrencyPerTenant, &issues)
	applyDuration(values, keyWorkerPollInterval, &cfg.Worker.PollInterval, &issues)
	applyDuration(values, keyWorkerLeaseDuration, &cfg.Worker.LeaseDuration, &issues)
	applyDuration(values, keyWorkerRenewInterval, &cfg.Worker.RenewInterval, &issues)
	applyDuration(values, keyWorkerRetryDelay, &cfg.Worker.RetryDelay, &issues)
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
	validateRange(&issues, keyServerReadTimeout, c.Server.ReadTimeout, 100*time.Millisecond, 5*time.Minute)
	validateRange(&issues, keyServerReadHeaderTimeout, c.Server.ReadHeaderTimeout, 100*time.Millisecond, 30*time.Second)
	validateRange(&issues, keyServerIdleTimeout, c.Server.IdleTimeout, 100*time.Millisecond, 5*time.Minute)
	if c.Server.IdleTimeout < c.Server.ReadHeaderTimeout {
		issues = append(issues, FieldError{Key: keyServerIdleTimeout, Rule: "must not be shorter than read header timeout"})
	}
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
	validateRange(&issues, keyPostgresConnectTimeout, c.Postgres.ConnectTimeout, 100*time.Millisecond, time.Minute)
	validateRange(&issues, keyPostgresHealthTimeout, c.Postgres.HealthTimeout, 100*time.Millisecond, 30*time.Second)
	validateAddress(&issues, keyRedisAddress, c.Redis.Address)
	validateIntRange(&issues, keyRedisDB, c.Redis.DB, 0, 15)
	validateRange(&issues, keyRedisDialTimeout, c.Redis.DialTimeout, 100*time.Millisecond, time.Minute)
	validateRange(&issues, keyRedisReadTimeout, c.Redis.ReadTimeout, 100*time.Millisecond, 30*time.Second)
	validateRange(&issues, keyRedisWriteTimeout, c.Redis.WriteTimeout, 100*time.Millisecond, 30*time.Second)
	validateRange(&issues, keyRedisPoolTimeout, c.Redis.PoolTimeout, 100*time.Millisecond, time.Minute)
	validateRange(&issues, keyRedisHealthTimeout, c.Redis.HealthTimeout, 100*time.Millisecond, 30*time.Second)
	validateIntRange(&issues, keyRedisPoolSize, c.Redis.PoolSize, 1, 1000)
	validateIntRange(&issues, keyRedisMinIdleConns, c.Redis.MinIdleConns, 0, 1000)
	if c.Redis.MinIdleConns > c.Redis.PoolSize {
		issues = append(issues, FieldError{Key: keyRedisMinIdleConns, Rule: "must not exceed pool size"})
	}
	validateRange(&issues, keyRedisMaxTTL, c.Redis.MaxTTL, time.Second, 24*time.Hour)
	validateRange(&issues, keyRedisOutputTTL, c.Redis.OutputTTL, time.Second, 24*time.Hour)
	if c.Redis.OutputTTL > c.Redis.MaxTTL {
		issues = append(issues, FieldError{Key: keyRedisOutputTTL, Rule: "must not exceed Redis max TTL"})
	}
	validateIntRange(&issues, keyQueueMaxPendingTenant, c.Queue.MaxPendingPerTenant, 1, 100_000)
	validateIntRange(&issues, keyQueueMaxPendingGlobal, c.Queue.MaxPendingGlobal, 1, 1_000_000)
	if c.Queue.MaxPendingGlobal < c.Queue.MaxPendingPerTenant {
		issues = append(issues, FieldError{Key: keyQueueMaxPendingGlobal, Rule: "must not be smaller than per-tenant pending capacity"})
	}
	if c.Worker.ID == "" || len(c.Worker.ID) > 256 || strings.TrimSpace(c.Worker.ID) != c.Worker.ID || strings.IndexFunc(c.Worker.ID, unicode.IsControl) >= 0 {
		issues = append(issues, FieldError{Key: keyWorkerID, Rule: "must be safe text with at most 256 bytes"})
	}
	validateIntRange(&issues, keyWorkerMaxConcurrency, c.Worker.MaxConcurrency, 1, 1000)
	validateIntRange(&issues, keyWorkerMaxPerTenant, c.Worker.MaxConcurrencyPerTenant, 1, 1000)
	if c.Worker.MaxConcurrencyPerTenant > c.Worker.MaxConcurrency {
		issues = append(issues, FieldError{Key: keyWorkerMaxPerTenant, Rule: "must not exceed global concurrency"})
	}
	validateRange(&issues, keyWorkerPollInterval, c.Worker.PollInterval, time.Millisecond, time.Minute)
	validateRange(&issues, keyWorkerLeaseDuration, c.Worker.LeaseDuration, 5*time.Second, 5*time.Minute)
	validateRange(&issues, keyWorkerRenewInterval, c.Worker.RenewInterval, time.Millisecond, 5*time.Minute)
	if c.Worker.RenewInterval >= c.Worker.LeaseDuration {
		issues = append(issues, FieldError{Key: keyWorkerRenewInterval, Rule: "must be shorter than lease duration"})
	}
	validateRange(&issues, keyWorkerRetryDelay, c.Worker.RetryDelay, 0, 24*time.Hour)
	if !oneOf(string(c.Logging.Level), "debug", "info", "warn", "error") {
		issues = append(issues, FieldError{Key: keyLogLevel, Rule: "must be debug, info, warn, or error"})
	}
	return issues
}

// Validate checks an already constructed Config. It is primarily used by
// explicit composition roots and tests whose loader is injected.
func (c Config) Validate() error {
	issues := c.validate()
	if len(issues) == 0 {
		return nil
	}
	return newValidationError(issues)
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
		"environment":                  c.Environment,
		"server.address":               c.Server.Address,
		"server.read_timeout":          c.Server.ReadTimeout,
		"server.read_header_timeout":   c.Server.ReadHeaderTimeout,
		"server.idle_timeout":          c.Server.IdleTimeout,
		"server.shutdown_timeout":      c.Server.ShutdownTimeout,
		"postgres.max_open_conns":      c.Postgres.MaxOpenConns,
		"postgres.max_idle_conns":      c.Postgres.MaxIdleConns,
		"postgres.conn_max_lifetime":   c.Postgres.ConnMaxLifetime,
		"postgres.connect_timeout":     c.Postgres.ConnectTimeout,
		"postgres.health_timeout":      c.Postgres.HealthTimeout,
		"redis.address":                c.Redis.Address,
		"redis.db":                     c.Redis.DB,
		"redis.dial_timeout":           c.Redis.DialTimeout,
		"redis.read_timeout":           c.Redis.ReadTimeout,
		"redis.write_timeout":          c.Redis.WriteTimeout,
		"redis.pool_timeout":           c.Redis.PoolTimeout,
		"redis.health_timeout":         c.Redis.HealthTimeout,
		"redis.pool_size":              c.Redis.PoolSize,
		"redis.min_idle_conns":         c.Redis.MinIdleConns,
		"redis.max_ttl":                c.Redis.MaxTTL,
		"redis.output_ttl":             c.Redis.OutputTTL,
		"queue.max_pending_per_tenant": c.Queue.MaxPendingPerTenant,
		"queue.max_pending_global":     c.Queue.MaxPendingGlobal,
		"worker.id":                    c.Worker.ID,
		"worker.max_concurrency":       c.Worker.MaxConcurrency,
		"worker.max_per_tenant":        c.Worker.MaxConcurrencyPerTenant,
		"worker.poll_interval":         c.Worker.PollInterval,
		"worker.lease_duration":        c.Worker.LeaseDuration,
		"worker.renew_interval":        c.Worker.RenewInterval,
		"worker.retry_delay":           c.Worker.RetryDelay,
		"log.level":                    c.Logging.Level,
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
