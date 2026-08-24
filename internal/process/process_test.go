package process

import (
	"testing"

	"github.com/agenticrag/agenticrag/internal/config"
)

func TestServeDependenciesFailClosed(t *testing.T) {
	t.Setenv("AGENTICRAG_CURSOR_SIGNING_KEY", "")
	t.Setenv("AGENTICRAG_DEV_BEARER_TOKEN", "")
	t.Setenv("AGENTICRAG_DEV_TENANT_ID", "")
	if _, err := serveDependencies(config.EnvironmentDevelopment); err == nil {
		t.Fatal("development dependencies accepted missing credentials")
	}
	if _, err := serveDependencies(config.EnvironmentProduction); err == nil {
		t.Fatal("production dependencies accepted missing signing key")
	}
}

func TestServeDependenciesSelectAuthenticationMode(t *testing.T) {
	t.Setenv("AGENTICRAG_CURSOR_SIGNING_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("AGENTICRAG_DEV_BEARER_TOKEN", "development-token")
	t.Setenv("AGENTICRAG_DEV_TENANT_ID", "ten_process01")

	development, err := serveDependencies(config.EnvironmentDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if development.Authenticator == nil || development.AuthenticatorFactory != nil {
		t.Fatal("development must use the explicit static authenticator")
	}

	production, err := serveDependencies(config.EnvironmentProduction)
	if err != nil {
		t.Fatal(err)
	}
	if production.Authenticator != nil || production.AuthenticatorFactory == nil {
		t.Fatal("production must resolve API keys from PostgreSQL")
	}
}
