package redaction

import (
	"context"
	"time"
)

// ArtifactSensitivity is the caller's mandatory data classification.
type ArtifactSensitivity string

const (
	ArtifactPublic       ArtifactSensitivity = "PUBLIC"
	ArtifactInternal     ArtifactSensitivity = "INTERNAL"
	ArtifactConfidential ArtifactSensitivity = "CONFIDENTIAL"
	ArtifactRestricted   ArtifactSensitivity = "RESTRICTED"
)

// ArtifactRequest contains security metadata, never Artifact payload bytes.
type ArtifactRequest struct {
	TenantID       string
	Sensitivity    ArtifactSensitivity
	EncryptionMode string
	TTL            time.Duration
}

// ArtifactDecision records admission and the effective retention ceiling.
type ArtifactDecision struct {
	Allowed       bool
	PolicyVersion string
	MaximumTTL    time.Duration
}

// AuthorizeArtifact enforces encryption and retention before raw upload.
func (s *Service) AuthorizeArtifact(ctx context.Context, request ArtifactRequest) (ArtifactDecision, error) {
	view := s.View(ctx, request.TenantID)
	policy := view.policy
	if !policy.valid() || view.degraded || request.TTL <= 0 {
		return ArtifactDecision{}, ErrArtifactPolicy
	}
	switch request.Sensitivity {
	case ArtifactPublic, ArtifactInternal:
		return ArtifactDecision{Allowed: true, PolicyVersion: policy.version}, nil
	case ArtifactConfidential, ArtifactRestricted:
		if request.EncryptionMode != "SSE-KMS" && request.EncryptionMode != "CLIENT" {
			return ArtifactDecision{PolicyVersion: policy.version, MaximumTTL: policy.sensitiveArtifactMaxTTL}, ErrArtifactPolicy
		}
		if request.TTL > policy.sensitiveArtifactMaxTTL {
			return ArtifactDecision{PolicyVersion: policy.version, MaximumTTL: policy.sensitiveArtifactMaxTTL}, ErrArtifactPolicy
		}
		return ArtifactDecision{Allowed: true, PolicyVersion: policy.version, MaximumTTL: policy.sensitiveArtifactMaxTTL}, nil
	default:
		return ArtifactDecision{}, ErrArtifactPolicy
	}
}
