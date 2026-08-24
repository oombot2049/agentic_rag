package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type APIKeyDB interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PostgresAPIKeyStore resolves only SHA-256 digests. Raw credentials never
// cross the storage boundary or enter query/log values.
type PostgresAPIKeyStore struct{ db APIKeyDB }

func NewPostgresAPIKeyStore(db APIKeyDB) (*PostgresAPIKeyStore, error) {
	if db == nil {
		return nil, errors.New("API key database is required")
	}
	return &PostgresAPIKeyStore{db: db}, nil
}

func (s *PostgresAPIKeyStore) ResolveAPIKey(ctx context.Context, digest [sha256.Size]byte) (APIKeyRecord, error) {
	var tenant, subject string
	var permissionsJSON []byte
	var expiresAt time.Time
	var revoked bool
	err := s.db.QueryRowContext(ctx, selectAPIKeySQL, digest[:]).Scan(&tenant, &subject, &permissionsJSON, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKeyRecord{}, ErrCredentialNotFound
	}
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("resolve API key digest: %w", err)
	}
	tenantID, err := domain.ParseTenantID(tenant)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("stored API key tenant is invalid: %w", err)
	}
	var permissions []string
	if err := json.Unmarshal(permissionsJSON, &permissions); err != nil {
		return APIKeyRecord{}, fmt.Errorf("stored API key permissions are invalid: %w", err)
	}
	principal, err := NewPrincipal(tenantID, subject, permissions)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("stored API key principal is invalid: %w", err)
	}
	return APIKeyRecord{Principal: principal, ExpiresAt: expiresAt.UTC(), Revoked: revoked}, nil
}

const selectAPIKeySQL = `SELECT tenant_id,subject_id,permissions,expires_at,revoked
FROM api_keys WHERE key_sha256=$1`

var _ APIKeyStore = (*PostgresAPIKeyStore)(nil)
