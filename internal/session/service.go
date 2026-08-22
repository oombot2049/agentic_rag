package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
)

const (
	createOperation    = "CREATE_SESSION"
	defaultRetention   = 7 * 24 * time.Hour
	maximumMetadata    = 20
	maximumMetadataKey = 128
	maximumMetadataVal = 256
)

// Metadata is server-retained caller annotation for a Session.
type Metadata map[string]string

// CreateRequest contains trusted identity plus validated client command data.
type CreateRequest struct {
	TenantID               domain.TenantID
	AuthenticatedSubjectID string
	IdempotencyKey         string
	Metadata               Metadata
}

// CreatePersistence is the atomic input consumed by a persistence adapter.
type CreatePersistence struct {
	Session     *domain.Session
	Metadata    Metadata
	Scope       idempotency.Scope
	Fingerprint idempotency.Fingerprint
	Response    idempotency.Response
	Now         time.Time
	Retention   time.Duration
}

// CreateResult reports the durable idempotency decision and associated aggregate.
type CreateResult struct {
	Decision idempotency.Decision
	Session  *domain.Session
	Response *idempotency.Response
}

// Store is the tenant-scoped persistence port for the Session API.
type Store interface {
	Create(context.Context, CreatePersistence) (CreateResult, error)
	Get(context.Context, domain.TenantID, domain.SessionID) (*domain.Session, error)
}

// IDGenerator creates typed Session identifiers.
type IDGenerator interface {
	NewSessionID() (domain.SessionID, error)
}

type IDGeneratorFunc func() (domain.SessionID, error)

func (f IDGeneratorFunc) NewSessionID() (domain.SessionID, error) { return f() }

// Service applies Session command/query policy independently of HTTP and SQL.
type Service struct {
	store     Store
	ids       IDGenerator
	now       func() time.Time
	retention time.Duration
}

func NewService(store Store, ids IDGenerator) (*Service, error) {
	if store == nil {
		return nil, errors.New("session service store is required")
	}
	if ids == nil {
		ids = IDGeneratorFunc(randomSessionID)
	}
	return &Service{store: store, ids: ids, now: time.Now, retention: defaultRetention}, nil
}

// SessionView is the public representation shared by create and get routes.
type SessionView struct {
	ID                    string  `json:"id"`
	ContextVersion        uint64  `json:"context_version"`
	RowVersion            uint64  `json:"row_version"`
	NextTurnSequence      uint64  `json:"next_turn_sequence"`
	CommittedTurnSequence uint64  `json:"committed_turn_sequence"`
	ActiveRunID           *string `json:"active_run_id"`
	HeadTurnID            *string `json:"head_turn_id"`
	CreatedAt             string  `json:"created_at"`
	UpdatedAt             string  `json:"updated_at"`
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (SessionView, error) {
	if ctx == nil {
		return SessionView{}, invalid(errors.New("context is nil"))
	}
	metadata, err := normalizeMetadata(request.Metadata)
	if err != nil {
		return SessionView{}, invalid(err)
	}
	if _, err := domain.ParseTenantID(request.TenantID.String()); err != nil {
		return SessionView{}, invalid(err)
	}
	if !validBoundedText(request.AuthenticatedSubjectID, 1, 128) {
		return SessionView{}, invalid(errors.New("authenticated subject is invalid"))
	}
	if !validBoundedText(request.IdempotencyKey, 16, 128) {
		return SessionView{}, apperror.New(apperror.CodeInvalidIdempotencyKey, false, nil)
	}
	fingerprint, err := idempotency.NewFingerprint(idempotency.Request{
		Operation: createOperation,
		Body: struct {
			Metadata Metadata `json:"metadata"`
		}{Metadata: metadata},
	})
	if err != nil {
		return SessionView{}, invalid(err)
	}
	id, err := s.ids.NewSessionID()
	if err != nil {
		return SessionView{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("generate session ID: %w", err))
	}
	now := s.now().UTC()
	value, err := domain.NewSession(id, request.TenantID, now)
	if err != nil {
		return SessionView{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("create session aggregate: %w", err))
	}
	view := viewOf(value)
	body, err := json.Marshal(view)
	if err != nil {
		return SessionView{}, apperror.New(apperror.CodeInternal, false, err)
	}
	location := "/v1/sessions/" + id.String()
	result, err := s.store.Create(ctx, CreatePersistence{
		Session: value, Metadata: metadata,
		Scope: idempotency.Scope{TenantID: request.TenantID, AuthenticatedSubjectID: request.AuthenticatedSubjectID,
			Operation: createOperation, Key: request.IdempotencyKey},
		Fingerprint: fingerprint, Now: now, Retention: s.retention,
		Response: idempotency.Response{StatusCode: 201, Body: body, Location: location,
			ResourceType: "session", ResourceID: id.String()},
	})
	if err != nil {
		return SessionView{}, apperror.New(apperror.CodeInternal, true, fmt.Errorf("persist session: %w", err))
	}
	switch result.Decision {
	case idempotency.DecisionAcquired:
		if result.Session == nil {
			return SessionView{}, apperror.New(apperror.CodeInternal, false, errors.New("store returned nil session"))
		}
		return viewOf(result.Session), nil
	case idempotency.DecisionReplaySucceeded:
		if result.Response == nil {
			return SessionView{}, apperror.New(apperror.CodeInternal, false, errors.New("store returned no replay response"))
		}
		var replay SessionView
		if err := json.Unmarshal(result.Response.Body, &replay); err != nil {
			return SessionView{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("decode stored Session response: %w", err))
		}
		if replay.ID == "" {
			return SessionView{}, apperror.New(apperror.CodeInternal, false, errors.New("stored Session response has no ID"))
		}
		if result.Response.StatusCode != 201 || result.Response.ResourceType != "session" ||
			result.Response.ResourceID != replay.ID {
			return SessionView{}, apperror.New(apperror.CodeInternal, false, errors.New("stored Session response reference is inconsistent"))
		}
		return replay, nil
	case idempotency.DecisionInProgress:
		return SessionView{}, apperror.New(apperror.CodeIdempotencyInProgress, true, nil)
	case idempotency.DecisionKeyReused, idempotency.DecisionReplayFailedFinal:
		return SessionView{}, apperror.New(apperror.CodeIdempotencyKeyReused, false, nil)
	default:
		return SessionView{}, apperror.New(apperror.CodeInternal, false, errors.New("unknown idempotency decision"))
	}
}

func (s *Service) Get(ctx context.Context, tenantID domain.TenantID, rawID string) (SessionView, error) {
	if ctx == nil {
		return SessionView{}, invalid(errors.New("context is nil"))
	}
	id, err := domain.ParseSessionID(rawID)
	if err != nil {
		return SessionView{}, invalid(err)
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return SessionView{}, invalid(err)
	}
	value, err := s.store.Get(ctx, tenantID, id)
	if errors.Is(err, ErrNotFound) {
		return SessionView{}, apperror.New(apperror.CodeSessionNotFound, false, nil)
	}
	if err != nil {
		return SessionView{}, apperror.New(apperror.CodeInternal, true, fmt.Errorf("get session: %w", err))
	}
	if value == nil {
		return SessionView{}, apperror.New(apperror.CodeInternal, false, errors.New("store returned nil session"))
	}
	return viewOf(value), nil
}

func ETag(rowVersion uint64) string { return fmt.Sprintf(`"rv-%d"`, rowVersion) }

func viewOf(value *domain.Session) SessionView {
	view := SessionView{
		ID: value.ID().String(), ContextVersion: value.ContextVersion(), RowVersion: value.RowVersion(),
		NextTurnSequence: value.NextTurnSequence(), CommittedTurnSequence: value.CommittedTurnSequence(),
		CreatedAt: value.CreatedAt().Format(time.RFC3339Nano), UpdatedAt: value.UpdatedAt().Format(time.RFC3339Nano),
	}
	if id, ok := value.ActiveRunID(); ok {
		raw := id.String()
		view.ActiveRunID = &raw
	}
	if id, ok := value.HeadTurnID(); ok {
		raw := id.String()
		view.HeadTurnID = &raw
	}
	return view
}

func normalizeMetadata(source Metadata) (Metadata, error) {
	if len(source) > maximumMetadata {
		return nil, errors.New("metadata contains more than 20 properties")
	}
	result := make(Metadata, len(source))
	for key, value := range source {
		if !validBoundedText(key, 1, maximumMetadataKey) || strings.TrimSpace(key) != key {
			return nil, errors.New("metadata key is invalid")
		}
		if len(value) > maximumMetadataVal || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, errors.New("metadata value is invalid")
		}
		result[key] = value
	}
	return result, nil
}

func validBoundedText(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum &&
		strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func invalid(cause error) error { return apperror.New(apperror.CodeInvalidArgument, false, cause) }

func randomSessionID() (domain.SessionID, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return domain.ParseSessionID("ses_" + hex.EncodeToString(entropy[:]))
}
