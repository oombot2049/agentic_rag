package turn

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const maximumFinalContent = 1024 * 1024

var (
	ErrFinalFenced   = errors.New("final commit authority is fenced")
	ErrFinalConflict = errors.New("final commit conflicts with durable state")
	ErrFinalNotFound = errors.New("final commit aggregate not found")
)

// FinalAuthority is the complete, non-transferable worker lease proof. Both
// token and execution version are required because renewal advances the latter.
type FinalAuthority struct {
	Owner            string
	Token            uint64
	ExecutionVersion uint64
}

// FinalRequest identifies one immutable answer. MessageID must be allocated
// before entering the retryable transaction and reused after redelivery.
type FinalRequest struct {
	TenantID                  domain.TenantID
	SessionID                 domain.SessionID
	TurnID                    domain.TurnID
	RunID                     domain.RunID
	MessageID                 domain.MessageID
	Content                   string
	ExpectedContextVersion    uint64
	ExpectedSessionRowVersion uint64
	Authority                 FinalAuthority
	At                        time.Time
	EventRetention            time.Duration
}

type FinalResult struct {
	MessageID         domain.MessageID
	ContextVersion    uint64
	SessionRowVersion uint64
	EventSequence     uint64
	Replayed          bool
}

type FinalStore interface {
	CommitFinal(context.Context, FinalRequest) (FinalResult, error)
}

type FinalCommitService struct{ store FinalStore }

func NewFinalCommitService(store FinalStore) (*FinalCommitService, error) {
	if store == nil {
		return nil, errors.New("final commit store is required")
	}
	return &FinalCommitService{store: store}, nil
}

func (s *FinalCommitService) Commit(ctx context.Context, request FinalRequest) (FinalResult, error) {
	if ctx == nil {
		return FinalResult{}, errors.New("final commit context is nil")
	}
	if err := validateFinalRequest(request); err != nil {
		return FinalResult{}, err
	}
	result, err := s.store.CommitFinal(ctx, request)
	if err != nil {
		return FinalResult{}, fmt.Errorf("commit final answer: %w", err)
	}
	if result.MessageID != request.MessageID || result.ContextVersion == 0 || result.SessionRowVersion == 0 {
		return FinalResult{}, errors.New("final commit store returned an invalid result")
	}
	return result, nil
}

func validateFinalRequest(request FinalRequest) error {
	parsers := []error{}
	_, err := domain.ParseTenantID(request.TenantID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseSessionID(request.SessionID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseTurnID(request.TurnID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseRunID(request.RunID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseMessageID(request.MessageID.String())
	parsers = append(parsers, err)
	for _, parseErr := range parsers {
		if parseErr != nil {
			return parseErr
		}
	}
	if request.ExpectedContextVersion >= math.MaxInt64 || request.ExpectedSessionRowVersion == 0 || request.ExpectedSessionRowVersion >= math.MaxInt64 {
		return errors.New("final commit versions are outside PostgreSQL bigint range")
	}
	if len(request.Content) == 0 || len(request.Content) > maximumFinalContent || strings.TrimSpace(request.Content) == "" {
		return errors.New("final answer must be non-blank and no larger than 1 MiB")
	}
	if !validBoundedText(request.Authority.Owner, 1, 256) || request.Authority.Token == 0 || request.Authority.ExecutionVersion == 0 {
		return errors.New("final commit authority is invalid")
	}
	if request.At.IsZero() || request.EventRetention < time.Hour || request.EventRetention > 30*24*time.Hour {
		return errors.New("final commit time and event retention 1h..30d are required")
	}
	return nil
}
