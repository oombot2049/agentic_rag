package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testDSN = "postgres://user:top-secret@db.example/agenticrag"

func validSource() MapSource {
	return MapSource{keyPostgresDSN: testDSN}
}

func TestLoadSourcesDefaultsAndPrecedence(t *testing.T) {
	cfg, err := LoadSources(
		validSource(),
		MapSource{keyEnvironment: "test", keyServerAddress: "127.0.0.1:9090", keyPostgresMaxOpenConns: "80"},
		MapSource{keyPostgresMaxOpenConns: "90", keyRedisPassword: "redis-secret"},
	)
	if err != nil {
		t.Fatalf("LoadSources() error = %v", err)
	}
	if cfg.Environment != EnvironmentTest || cfg.Server.Address != "127.0.0.1:9090" {
		t.Fatalf("unexpected overlay result: %+v", cfg.SafeFields())
	}
	if cfg.Postgres.MaxOpenConns != 90 || cfg.Postgres.MaxIdleConns != 10 {
		t.Fatalf("database defaults/overlay mismatch: %+v", cfg.Postgres)
	}
	if cfg.Postgres.DSN.Reveal() != testDSN || cfg.Redis.Password.Reveal() != "redis-secret" {
		t.Fatal("secrets were not retained for explicit consumption")
	}
}

func TestLoadSourcesValidation(t *testing.T) {
	tests := []struct {
		name   string
		values MapSource
		keys   []string
	}{
		{name: "missing required DSN", values: MapSource{}, keys: []string{keyPostgresDSN}},
		{name: "empty required DSN", values: MapSource{keyPostgresDSN: " "}, keys: []string{keyPostgresDSN}},
		{name: "bad environment", values: MapSource{keyPostgresDSN: testDSN, keyEnvironment: "staging"}, keys: []string{keyEnvironment}},
		{name: "bad duration", values: MapSource{keyPostgresDSN: testDSN, keyServerShutdownTimeout: "eventually"}, keys: []string{keyServerShutdownTimeout}},
		{name: "duration below minimum", values: MapSource{keyPostgresDSN: testDSN, keyServerReadHeaderTimeout: "50ms"}, keys: []string{keyServerReadHeaderTimeout}},
		{name: "idle shorter than header", values: MapSource{keyPostgresDSN: testDSN, keyServerIdleTimeout: "1s"}, keys: []string{keyServerIdleTimeout}},
		{name: "bad integer", values: MapSource{keyPostgresDSN: testDSN, keyRedisDB: "secret-number"}, keys: []string{keyRedisDB}},
		{name: "integer above maximum", values: MapSource{keyPostgresDSN: testDSN, keyRedisDB: "16"}, keys: []string{keyRedisDB}},
		{name: "redis idle exceeds pool", values: MapSource{keyPostgresDSN: testDSN, keyRedisPoolSize: "4", keyRedisMinIdleConns: "5"}, keys: []string{keyRedisMinIdleConns}},
		{name: "redis ttl above retention", values: MapSource{keyPostgresDSN: testDSN, keyRedisMaxTTL: "25h"}, keys: []string{keyRedisMaxTTL}},
		{name: "idle exceeds open", values: MapSource{keyPostgresDSN: testDSN, keyPostgresMaxOpenConns: "5", keyPostgresMaxIdleConns: "6"}, keys: []string{keyPostgresMaxIdleConns}},
		{name: "bad addresses", values: MapSource{keyPostgresDSN: testDSN, keyServerAddress: "8080", keyRedisAddress: "redis"}, keys: []string{keyRedisAddress, keyServerAddress}},
		{name: "bad log level", values: MapSource{keyPostgresDSN: testDSN, keyLogLevel: "verbose"}, keys: []string{keyLogLevel}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSources(tt.values)
			if err == nil || !IsValidationError(err) {
				t.Fatalf("LoadSources() error = %v, want ValidationError", err)
			}
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("errors.As(%T) failed", err)
			}
			got := make([]string, len(validation.Fields))
			for i, field := range validation.Fields {
				got[i] = field.Key
			}
			if !reflect.DeepEqual(got, tt.keys) {
				t.Fatalf("error keys = %v, want %v; error: %v", got, tt.keys, err)
			}
		})
	}
}

func TestLoadSourcesParsesBoundaries(t *testing.T) {
	cfg, err := LoadSources(MapSource{
		keyPostgresDSN:             testDSN,
		keyServerReadTimeout:       "5m",
		keyServerReadHeaderTimeout: "100ms",
		keyServerIdleTimeout:       "5m",
		keyServerShutdownTimeout:   "2m",
		keyPostgresMaxOpenConns:    "500",
		keyPostgresMaxIdleConns:    "0",
		keyPostgresConnLifetime:    "24h",
		keyRedisDB:                 "15",
		keyRedisDialTimeout:        "100ms",
		keyRedisReadTimeout:        "30s",
		keyRedisWriteTimeout:       "100ms",
		keyRedisPoolTimeout:        "1m",
		keyRedisHealthTimeout:      "30s",
		keyRedisPoolSize:           "1000",
		keyRedisMinIdleConns:       "1000",
		keyRedisMaxTTL:             "24h",
		keyLogLevel:                "debug",
	})
	if err != nil {
		t.Fatalf("LoadSources() error = %v", err)
	}
	if cfg.Server.ReadTimeout != 5*time.Minute || cfg.Server.ReadHeaderTimeout != 100*time.Millisecond ||
		cfg.Server.IdleTimeout != 5*time.Minute || cfg.Server.ShutdownTimeout != 2*time.Minute {
		t.Fatalf("duration boundaries not retained: %+v", cfg.Server)
	}
	if cfg.Postgres.MaxOpenConns != 500 || cfg.Postgres.MaxIdleConns != 0 || cfg.Redis.DB != 15 {
		t.Fatalf("numeric boundaries not retained: %+v", cfg.SafeFields())
	}
	if cfg.Redis.PoolSize != 1000 || cfg.Redis.MinIdleConns != 1000 || cfg.Redis.MaxTTL != 24*time.Hour {
		t.Fatalf("Redis boundaries not retained: %+v", cfg.Redis)
	}
}

func TestValidationErrorsAreStableAndSecretSafe(t *testing.T) {
	secret := "do-not-leak-this-password"
	_, err := LoadSources(MapSource{
		keyPostgresDSN:          secret,
		keyEnvironment:          "invalid-environment",
		keyPostgresMaxOpenConns: "not-a-number",
		keyRedisPassword:        secret,
		keyRedisDB:              secret,
	})
	if err == nil {
		t.Fatal("LoadSources() error = nil")
	}
	message := err.Error()
	if strings.Contains(message, secret) {
		t.Fatalf("error leaked secret: %q", message)
	}
	wantOrder := []string{keyEnvironment, keyPostgresMaxOpenConns, keyRedisDB}
	last := -1
	for _, key := range wantOrder {
		index := strings.Index(message, key)
		if index <= last {
			t.Fatalf("error order is not stable: %q", message)
		}
		last = index
	}
}

func TestSecretRedaction(t *testing.T) {
	secret := NewSecret("classified")
	formats := []string{"%s", "%v", "%+v", "%#v", "%q"}
	for _, format := range formats {
		if got := fmt.Sprintf(format, secret); strings.Contains(got, "classified") || got != redacted {
			t.Errorf("format %q = %q, want %q", format, got, redacted)
		}
	}
	data, err := json.Marshal(struct {
		Password Secret `json:"password"`
	}{Password: secret})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got := string(data); got != `{"password":"[REDACTED]"}` {
		t.Fatalf("JSON = %s", got)
	}
	text, err := secret.MarshalText()
	if err != nil || string(text) != redacted {
		t.Fatalf("MarshalText() = %q, %v", text, err)
	}
}

func TestSafeFieldsExcludeSecrets(t *testing.T) {
	cfg, err := LoadSources(MapSource{keyPostgresDSN: testDSN, keyRedisPassword: "redis-secret"})
	if err != nil {
		t.Fatalf("LoadSources() error = %v", err)
	}
	encoded := fmt.Sprint(cfg.SafeFields())
	for _, forbidden := range []string{testDSN, "redis-secret", "dsn", "password"} {
		if strings.Contains(strings.ToLower(encoded), strings.ToLower(forbidden)) {
			t.Fatalf("SafeFields leaked forbidden content %q: %s", forbidden, encoded)
		}
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv(keyPostgresDSN, testDSN)
	t.Setenv(keyEnvironment, "test")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Environment != EnvironmentTest || cfg.Postgres.DSN.Reveal() != testDSN {
		t.Fatalf("Load() did not read environment: %+v", cfg.SafeFields())
	}
}

func TestValidateConstructedConfig(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err == nil || !IsValidationError(err) {
		t.Fatalf("Defaults().Validate() error = %v, want required DSN validation", err)
	}
	cfg.Postgres.DSN = NewSecret(testDSN)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid constructed Config.Validate() error = %v", err)
	}
}
