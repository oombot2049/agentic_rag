// Package redaction provides tenant-aware, fail-safe sensitive-data policies.
package redaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	Replacement            = "[REDACTED]"
	maximumRules           = 32
	maximumRuleSize        = 256
	maximumDepth           = 32
	maximumNodes           = 10_000
	mandatoryDetectorCount = 5
)

var (
	ErrInvalidPolicy     = errors.New("invalid redaction policy")
	ErrPolicyUnavailable = errors.New("redaction policy unavailable")
	ErrArtifactPolicy    = errors.New("artifact violates sensitive-data policy")
	namePattern          = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	versionPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	keySeparator         = strings.NewReplacer("-", "_", ".", "_", " ", "_")
	mandatoryKeyNames    = []string{
		"authorization", "cookie", "set_cookie", "password", "passwd", "secret", "api_key",
		"access_token", "refresh_token", "dsn", "prompt", "prompt_text", "prompt_content",
		"content", "message_content", "tool_arguments", "retrieval_content", "error", "err",
		"cause", "error_detail", "provider_error", "stack", "stack_trace",
	}
)

// Boundary identifies a bounded outbound data channel.
type Boundary string

const (
	BoundaryLog             Boundary = "LOG"
	BoundaryArtifactPreview Boundary = "ARTIFACT_PREVIEW"
	BoundaryErrorDetail     Boundary = "ERROR_DETAIL"
)

// FindingKind classifies a redaction without retaining matched content.
type FindingKind string

const (
	FindingSensitiveKey FindingKind = "SENSITIVE_KEY"
	FindingCredential   FindingKind = "CREDENTIAL"
	FindingEmail        FindingKind = "EMAIL"
	FindingPhone        FindingKind = "PHONE"
	FindingCustom       FindingKind = "CUSTOM"
	FindingLimit        FindingKind = "LIMIT"
)

// Rule is one tenant-owned RE2 detector.
type Rule struct {
	Name    string
	Pattern string
}

// Config defines one immutable tenant policy version.
type Config struct {
	Version                 string
	RedactEmail             bool
	RedactPhone             bool
	SensitiveKeys           []string
	CustomRules             []Rule
	SensitiveArtifactMaxTTL time.Duration
}

type detector struct {
	kind       FindingKind
	name       string
	expression *regexp.Regexp
}

// Policy is immutable after construction and safe for concurrent reuse.
type Policy struct {
	version                 string
	sensitiveKeys           map[string]struct{}
	detectors               []detector
	sensitiveArtifactMaxTTL time.Duration
}

// Finding reports only safe detector metadata and a match count.
type Finding struct {
	Kind  FindingKind
	Rule  string
	Count int
}

// Result contains safe output and the policy evidence used to produce it.
type Result struct {
	Value         string
	PolicyVersion string
	Findings      []Finding
	Degraded      bool
}

// Resolver returns the current immutable policy for an authenticated tenant.
type Resolver interface {
	Resolve(context.Context, string) (*Policy, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(context.Context, string) (*Policy, error)

func (f ResolverFunc) Resolve(ctx context.Context, tenantID string) (*Policy, error) {
	return f(ctx, tenantID)
}

// Service resolves tenant policies and applies a strict fallback on failure.
type Service struct {
	resolver Resolver
	fallback *Policy
}

// View is one immutable policy snapshot used across a complete output record.
type View struct {
	policy          *Policy
	degraded        bool
	fallbackVersion string
}

// NewPolicy validates, compiles and freezes one policy configuration.
func NewPolicy(config Config) (*Policy, error) {
	if !versionPattern.MatchString(config.Version) || len(config.SensitiveKeys) > maximumRules ||
		len(config.CustomRules) > maximumRules || config.SensitiveArtifactMaxTTL < time.Minute ||
		config.SensitiveArtifactMaxTTL > 30*24*time.Hour {
		return nil, ErrInvalidPolicy
	}
	keys := mandatorySensitiveKeys()
	for _, key := range config.SensitiveKeys {
		normalized := normalizeKey(key)
		if normalized == "" || len(normalized) > 128 {
			return nil, ErrInvalidPolicy
		}
		keys[normalized] = struct{}{}
	}
	detectors := mandatoryDetectors()
	if config.RedactEmail {
		detectors = append(detectors, detector{FindingEmail, "email", regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,63}\b`)})
	}
	if config.RedactPhone {
		detectors = append(detectors, detector{FindingPhone, "phone", regexp.MustCompile(`\b(?:\+?[1-9][0-9][ -]?)?(?:[0-9][ -]?){9,14}[0-9]\b`)})
	}
	for _, rule := range config.CustomRules {
		if !namePattern.MatchString(rule.Name) || len(rule.Pattern) == 0 || len(rule.Pattern) > maximumRuleSize {
			return nil, ErrInvalidPolicy
		}
		expression, err := regexp.Compile(rule.Pattern)
		if err != nil || expression.MatchString("") || expression.MatchString(Replacement) {
			return nil, fmt.Errorf("%w: custom rule %q", ErrInvalidPolicy, rule.Name)
		}
		detectors = append(detectors, detector{FindingCustom, rule.Name, expression})
	}
	return &Policy{
		version: config.Version, sensitiveKeys: keys, detectors: detectors,
		sensitiveArtifactMaxTTL: config.SensitiveArtifactMaxTTL,
	}, nil
}

// StrictPolicy returns the mandatory credential and PII fallback policy.
func StrictPolicy() *Policy {
	policy, err := NewPolicy(Config{
		Version: "strict-v1", RedactEmail: true, RedactPhone: true,
		SensitiveArtifactMaxTTL: 24 * time.Hour,
	})
	if err != nil {
		panic(err)
	}
	return policy
}

// NewService constructs a tenant-aware redaction service.
func NewService(resolver Resolver, fallback *Policy) (*Service, error) {
	if resolver == nil || !fallback.valid() {
		return nil, ErrInvalidPolicy
	}
	return &Service{resolver: resolver, fallback: fallback}, nil
}

// NewStatic constructs a service that always uses one policy.
func NewStatic(policy *Policy) (*Service, error) {
	if policy == nil {
		return nil, ErrInvalidPolicy
	}
	return NewService(ResolverFunc(func(context.Context, string) (*Policy, error) {
		return policy, nil
	}), policy)
}

// View resolves a tenant policy once for a complete output record.
func (s *Service) View(ctx context.Context, tenantID string) View {
	policy, degraded := s.policy(ctx, tenantID)
	return View{policy: policy, degraded: degraded, fallbackVersion: fallbackVersion(s)}
}

// Redact sanitizes bounded unstructured text.
func (s *Service) Redact(ctx context.Context, tenantID string, boundary Boundary, value string) Result {
	return s.View(ctx, tenantID).Redact(boundary, value)
}

// Redact sanitizes bounded unstructured text with one resolved policy.
func (v View) Redact(boundary Boundary, value string) Result {
	limit, valid := boundaryLimit(boundary)
	if !v.policy.valid() || !valid || len(value) > limit {
		return Result{Value: Replacement, PolicyVersion: v.fallbackVersion, Findings: []Finding{{Kind: FindingLimit, Rule: "fail_closed", Count: 1}}, Degraded: true}
	}
	redacted, findings := v.policy.redactString(value)
	if len(redacted) > limit {
		return Result{Value: Replacement, PolicyVersion: v.policy.version, Findings: []Finding{{Kind: FindingLimit, Rule: "output_bytes", Count: 1}}, Degraded: true}
	}
	return Result{Value: redacted, PolicyVersion: v.policy.version, Findings: findings, Degraded: v.degraded}
}

// RedactField applies full-value key classification before content detectors.
func (s *Service) RedactField(ctx context.Context, tenantID string, boundary Boundary, key string, value string) Result {
	return s.View(ctx, tenantID).RedactField(boundary, key, value)
}

// RedactField sanitizes one field with a resolved policy.
func (v View) RedactField(boundary Boundary, key string, value string) Result {
	limit, valid := boundaryLimit(boundary)
	if !v.policy.valid() || !valid || len(value) > limit {
		return Result{Value: Replacement, PolicyVersion: v.fallbackVersion, Findings: []Finding{{Kind: FindingLimit, Rule: "fail_closed", Count: 1}}, Degraded: true}
	}
	if v.policy.sensitiveKey(key) {
		return Result{Value: Replacement, PolicyVersion: v.policy.version, Findings: []Finding{{Kind: FindingSensitiveKey, Rule: normalizeKey(key), Count: 1}}, Degraded: v.degraded}
	}
	redacted, findings := v.policy.redactString(value)
	if len(redacted) > limit {
		return Result{Value: Replacement, PolicyVersion: v.policy.version, Findings: []Finding{{Kind: FindingLimit, Rule: "output_bytes", Count: 1}}, Degraded: true}
	}
	return Result{Value: redacted, PolicyVersion: v.policy.version, Findings: findings, Degraded: v.degraded}
}

// SensitiveKey reports whether a field must be replaced in full.
func (s *Service) SensitiveKey(ctx context.Context, tenantID string, key string) bool {
	return s.View(ctx, tenantID).SensitiveKey(key)
}

// SensitiveKey reports key sensitivity with a resolved policy.
func (v View) SensitiveKey(key string) bool {
	return !v.policy.valid() || v.policy.sensitiveKey(key)
}

// RedactJSON sanitizes structured JSON while preserving safe scalar types.
func (s *Service) RedactJSON(ctx context.Context, tenantID string, boundary Boundary, input []byte) Result {
	return s.View(ctx, tenantID).RedactJSON(boundary, input)
}

// RedactJSON sanitizes JSON with one resolved policy.
func (v View) RedactJSON(boundary Boundary, input []byte) Result {
	limit, valid := boundaryLimit(boundary)
	if !valid || len(input) > limit {
		return Result{Value: Replacement, PolicyVersion: v.fallbackVersion, Findings: []Finding{{Kind: FindingLimit, Rule: "input_bytes", Count: 1}}, Degraded: true}
	}
	if !v.policy.valid() {
		return Result{Value: Replacement, PolicyVersion: v.fallbackVersion, Findings: []Finding{{Kind: FindingLimit, Rule: "fail_closed", Count: 1}}, Degraded: true}
	}
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.UseNumber()
	var value any
	decodeErr := decoder.Decode(&value)
	trailingErr := decoder.Decode(&struct{}{})
	if decodeErr != nil || !errors.Is(trailingErr, io.EOF) {
		redacted, findings := v.policy.redactString(string(input))
		if len(redacted) > limit {
			return Result{Value: Replacement, PolicyVersion: v.policy.version, Findings: []Finding{{Kind: FindingLimit, Rule: "output_bytes", Count: 1}}, Degraded: true}
		}
		return Result{Value: redacted, PolicyVersion: v.policy.version, Findings: findings, Degraded: v.degraded}
	}
	findings := make([]Finding, 0)
	nodes := 0
	clean, ok := v.policy.redactJSONValue(value, 0, &nodes, &findings)
	if !ok {
		return Result{Value: Replacement, PolicyVersion: v.policy.version, Findings: []Finding{{Kind: FindingLimit, Rule: "json_shape", Count: 1}}, Degraded: true}
	}
	encoded, err := json.Marshal(clean)
	if err != nil || len(encoded) > limit {
		return Result{Value: Replacement, PolicyVersion: v.policy.version, Findings: []Finding{{Kind: FindingLimit, Rule: "json_encode", Count: 1}}, Degraded: true}
	}
	return Result{Value: string(encoded), PolicyVersion: v.policy.version, Findings: findings, Degraded: v.degraded}
}

func (s *Service) policy(ctx context.Context, tenantID string) (*Policy, bool) {
	if s == nil || s.fallback == nil || ctx == nil || ctx.Err() != nil {
		return nil, true
	}
	policy, err := s.resolver.Resolve(ctx, tenantID)
	if err != nil || !policy.valid() {
		return s.fallback, true
	}
	return policy, false
}

func (p *Policy) valid() bool {
	if p == nil || !versionPattern.MatchString(p.version) || len(p.detectors) < mandatoryDetectorCount ||
		p.sensitiveArtifactMaxTTL < time.Minute || p.sensitiveArtifactMaxTTL > 30*24*time.Hour {
		return false
	}
	for _, key := range mandatoryKeyNames {
		if _, ok := p.sensitiveKeys[key]; !ok {
			return false
		}
	}
	return true
}

func (p *Policy) redactString(value string) (string, []Finding) {
	result := value
	findings := make([]Finding, 0)
	for _, current := range p.detectors {
		count := len(current.expression.FindAllStringIndex(result, -1))
		if count == 0 {
			continue
		}
		result = current.expression.ReplaceAllString(result, Replacement)
		findings = append(findings, Finding{Kind: current.kind, Rule: current.name, Count: count})
	}
	return result, findings
}

func (p *Policy) redactJSONValue(value any, depth int, nodes *int, findings *[]Finding) (any, bool) {
	(*nodes)++
	if depth > maximumDepth || *nodes > maximumNodes {
		return nil, false
	}
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, child := range typed {
			if p.sensitiveKey(key) {
				clean[key] = Replacement
				*findings = append(*findings, Finding{Kind: FindingSensitiveKey, Rule: normalizeKey(key), Count: 1})
				continue
			}
			redacted, ok := p.redactJSONValue(child, depth+1, nodes, findings)
			if !ok {
				return nil, false
			}
			clean[key] = redacted
		}
		return clean, true
	case []any:
		clean := make([]any, len(typed))
		for index, child := range typed {
			redacted, ok := p.redactJSONValue(child, depth+1, nodes, findings)
			if !ok {
				return nil, false
			}
			clean[index] = redacted
		}
		return clean, true
	case string:
		clean, found := p.redactString(typed)
		*findings = append(*findings, found...)
		return clean, true
	default:
		return value, true
	}
}

func (p *Policy) sensitiveKey(key string) bool {
	normalized := normalizeKey(key)
	if _, ok := p.sensitiveKeys[normalized]; ok {
		return true
	}
	for _, suffix := range []string{"_password", "_passwd", "_secret", "_api_key", "_authorization", "_cookie", "_dsn"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func mandatorySensitiveKeys() map[string]struct{} {
	result := make(map[string]struct{}, len(mandatoryKeyNames)+8)
	for _, value := range mandatoryKeyNames {
		result[value] = struct{}{}
	}
	return result
}

func mandatoryDetectors() []detector {
	return []detector{
		{FindingCredential, "authorization", regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)},
		{FindingCredential, "jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
		{FindingCredential, "api_key", regexp.MustCompile(`\b(?:sk|rk|pk)-[A-Za-z0-9_-]{12,}\b`)},
		{FindingCredential, "aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
		{FindingCredential, "secret_assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[_-]?key)\s*[:=]\s*[^\s,;]{4,}`)},
	}
}

func normalizeKey(value string) string {
	return keySeparator.Replace(strings.ToLower(strings.TrimSpace(value)))
}

func boundaryLimit(boundary Boundary) (int, bool) {
	switch boundary {
	case BoundaryLog:
		return 64 << 10, true
	case BoundaryErrorDetail:
		return 32 << 10, true
	case BoundaryArtifactPreview:
		return 1 << 20, true
	default:
		return 0, false
	}
}

func fallbackVersion(service *Service) string {
	if service != nil && service.fallback != nil {
		return service.fallback.version
	}
	return "unavailable"
}
