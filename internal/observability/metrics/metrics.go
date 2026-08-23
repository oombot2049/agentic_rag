// Package metrics owns bounded-cardinality Prometheus instrumentation.
package metrics

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	ErrInvalid  = errors.New("invalid metrics observation")
	ErrFinished = errors.New("run metrics handle already finished")
)

type Plan string

const (
	PlanFree       Plan = "free"
	PlanStandard   Plan = "standard"
	PlanEnterprise Plan = "enterprise"
	PlanUnknown    Plan = "unknown"
)

type RunOutcome string

const (
	RunSucceeded RunOutcome = "succeeded"
	RunFailed    RunOutcome = "failed"
	RunCanceled  RunOutcome = "canceled"
	RunTimedOut  RunOutcome = "timed_out"
	RunLimited   RunOutcome = "limited"
)

type LLMOutcome string

const (
	LLMSucceeded   LLMOutcome = "succeeded"
	LLMFailed      LLMOutcome = "failed"
	LLMTimedOut    LLMOutcome = "timed_out"
	LLMRateLimited LLMOutcome = "rate_limited"
)

type SLI string

const (
	SLITurnCommand        SLI = "turn_command"
	SLISessionRead        SLI = "session_read"
	SLISSEResume          SLI = "sse_resume"
	SLIRunTerminalization SLI = "run_terminalization"
	SLIRetrieval          SLI = "retrieval"
	SLIFinalCommit        SLI = "final_commit"
)

type SLIClassification string

const (
	SLIGood     SLIClassification = "good"
	SLIBad      SLIClassification = "bad"
	SLIExcluded SLIClassification = "excluded"
)

type Config struct {
	AllowedModels map[string][]string
}

type Metrics struct {
	registry      *prometheus.Registry
	allowedModels map[string]struct{}
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	activeRuns    *prometheus.GaugeVec
	queueWait     *prometheus.HistogramVec
	runs          *prometheus.CounterVec
	runDuration   *prometheus.HistogramVec
	runSteps      *prometheus.HistogramVec
	llmRequests   *prometheus.CounterVec
	llmTTFT       *prometheus.HistogramVec
	inputTokens   *prometheus.CounterVec
	outputTokens  *prometheus.CounterVec
	llmCost       *prometheus.CounterVec
	sliEvents     *prometheus.CounterVec
}

var configuredLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func New(config Config) (*Metrics, error) {
	allowed, err := normalizeModels(config.AllowedModels)
	if err != nil {
		return nil, err
	}
	value := &Metrics{registry: prometheus.NewRegistry(), allowedModels: allowed,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_http_requests_total", Help: "Completed HTTP requests."}, []string{"plan", "method", "status_class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agenticrag_http_request_duration_seconds", Help: "HTTP request duration.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}}, []string{"plan", "method", "status_class"}),
		activeRuns:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agenticrag_worker_active_runs", Help: "Currently active Runs."}, []string{"plan"}),
		queueWait:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agenticrag_run_queue_wait_seconds", Help: "Time from durable enqueue to processing.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300}}, []string{"plan"}),
		runs:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_runs_total", Help: "Terminal Runs."}, []string{"plan", "outcome"}),
		runDuration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agenticrag_run_duration_seconds", Help: "Run execution duration.", Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300, 600}}, []string{"plan", "outcome"}),
		runSteps:     prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agenticrag_run_steps", Help: "Terminal Run Step count.", Buckets: []float64{1, 2, 3, 5, 8, 12, 16, 20}}, []string{"plan", "outcome"}),
		llmRequests:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_llm_requests_total", Help: "Terminal LLM calls."}, []string{"plan", "provider", "model", "outcome"}),
		llmTTFT:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agenticrag_llm_ttft_seconds", Help: "LLM time to first token when available.", Buckets: []float64{.025, .05, .1, .25, .5, 1, 2, 5, 10, 30}}, []string{"plan", "provider", "model"}),
		inputTokens:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_llm_input_tokens_total", Help: "LLM input Tokens."}, []string{"plan", "provider", "model"}),
		outputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_llm_output_tokens_total", Help: "LLM output Tokens."}, []string{"plan", "provider", "model"}),
		llmCost:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_llm_cost_nanos_total", Help: "Estimated LLM cost in currency nanos."}, []string{"plan", "provider", "model"}),
		sliEvents:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agenticrag_sli_events_total", Help: "Logical SLI events classified at the owning business boundary."}, []string{"sli", "classification"})}
	value.registry.MustRegister(value.httpRequests, value.httpDuration, value.activeRuns, value.queueWait, value.runs,
		value.runDuration, value.runSteps, value.llmRequests, value.llmTTFT, value.inputTokens, value.outputTokens, value.llmCost,
		value.sliEvents)
	return value, nil
}

func normalizeModels(values map[string][]string) (map[string]struct{}, error) {
	if len(values) > 32 {
		return nil, ErrInvalid
	}
	result := make(map[string]struct{})
	count := 0
	for provider, models := range values {
		if !configuredLabel.MatchString(provider) || len(models) == 0 {
			return nil, ErrInvalid
		}
		for _, model := range models {
			if !configuredLabel.MatchString(model) {
				return nil, ErrInvalid
			}
			key := provider + "\x00" + model
			if _, duplicate := result[key]; duplicate {
				return nil, ErrInvalid
			}
			result[key] = struct{}{}
			count++
			if count > 128 {
				return nil, ErrInvalid
			}
		}
	}
	return result, nil
}

func (m *Metrics) ObserveHTTP(plan Plan, method string, status int, duration time.Duration) error {
	if m == nil || status < 100 || status > 599 || duration < 0 {
		return ErrInvalid
	}
	labels := []string{normalizePlan(plan), normalizeMethod(method), fmt.Sprintf("%dxx", status/100)}
	m.httpRequests.WithLabelValues(labels...).Inc()
	m.httpDuration.WithLabelValues(labels...).Observe(duration.Seconds())
	return nil
}

type RunHandle struct {
	metrics *Metrics
	plan    string
	once    sync.Once
}

func (m *Metrics) StartRun(plan Plan, queueWait time.Duration) (*RunHandle, error) {
	if m == nil || queueWait < 0 {
		return nil, ErrInvalid
	}
	normalized := normalizePlan(plan)
	m.activeRuns.WithLabelValues(normalized).Inc()
	m.queueWait.WithLabelValues(normalized).Observe(queueWait.Seconds())
	return &RunHandle{metrics: m, plan: normalized}, nil
}

func (h *RunHandle) Finish(outcome RunOutcome, steps int, duration time.Duration) error {
	if h == nil || h.metrics == nil || !validRunOutcome(outcome) || steps < 0 || steps > 10000 || duration < 0 {
		return ErrInvalid
	}
	completed := false
	h.once.Do(func() {
		completed = true
		h.metrics.activeRuns.WithLabelValues(h.plan).Dec()
		h.metrics.runs.WithLabelValues(h.plan, string(outcome)).Inc()
		h.metrics.runDuration.WithLabelValues(h.plan, string(outcome)).Observe(duration.Seconds())
		h.metrics.runSteps.WithLabelValues(h.plan, string(outcome)).Observe(float64(steps))
	})
	if !completed {
		return ErrFinished
	}
	return nil
}

type LLMObservation struct {
	Plan            Plan
	Provider, Model string
	Outcome         LLMOutcome
	Duration, TTFT  time.Duration
	InputTokens     int64
	OutputTokens    int64
	CostNanos       int64
}

func (m *Metrics) ObserveLLM(value LLMObservation) error {
	if m == nil || !validLLMOutcome(value.Outcome) || value.Duration < 0 || value.TTFT < 0 || value.TTFT > value.Duration ||
		value.InputTokens < 0 || value.OutputTokens < 0 || value.CostNanos < 0 {
		return ErrInvalid
	}
	plan := normalizePlan(value.Plan)
	provider, model := m.normalizeModel(value.Provider, value.Model)
	m.llmRequests.WithLabelValues(plan, provider, model, string(value.Outcome)).Inc()
	if value.TTFT > 0 {
		m.llmTTFT.WithLabelValues(plan, provider, model).Observe(value.TTFT.Seconds())
	}
	m.inputTokens.WithLabelValues(plan, provider, model).Add(float64(value.InputTokens))
	m.outputTokens.WithLabelValues(plan, provider, model).Add(float64(value.OutputTokens))
	m.llmCost.WithLabelValues(plan, provider, model).Add(float64(value.CostNanos))
	return nil
}

// ObserveSLI records one logical event after the owning business boundary has
// applied eligibility rules. Physical retries must not call this method.
func (m *Metrics) ObserveSLI(sli SLI, classification SLIClassification) error {
	if m == nil || !validSLI(sli) || !validSLIClassification(classification) {
		return ErrInvalid
	}
	m.sliEvents.WithLabelValues(string(sli), string(classification)).Inc()
	return nil
}

func (m *Metrics) normalizeModel(provider, model string) (string, string) {
	if _, allowed := m.allowedModels[provider+"\x00"+model]; !allowed {
		return "other", "other"
	}
	return provider, model
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

func (m *Metrics) Gatherer() prometheus.Gatherer { return m.registry }

type PlanFunc func(*http.Request) Plan

func (m *Metrics) HTTPMiddleware(plans PlanFunc) (func(http.Handler) http.Handler, error) {
	if m == nil || plans == nil {
		return nil, ErrInvalid
	}
	return func(next http.Handler) http.Handler {
		if next == nil {
			return nil
		}
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			started := time.Now()
			observed := httpsnoop.CaptureMetrics(next, response, request)
			_ = m.ObserveHTTP(plans(request), request.Method, observed.Code, time.Since(started))
		})
	}, nil
}

func normalizePlan(value Plan) string {
	switch value {
	case PlanFree, PlanStandard, PlanEnterprise:
		return string(value)
	default:
		return string(PlanUnknown)
	}
}

func normalizeMethod(value string) string {
	value = strings.ToUpper(value)
	switch value {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return value
	default:
		return "OTHER"
	}
}

func validRunOutcome(value RunOutcome) bool {
	return value == RunSucceeded || value == RunFailed || value == RunCanceled || value == RunTimedOut || value == RunLimited
}
func validLLMOutcome(value LLMOutcome) bool {
	return value == LLMSucceeded || value == LLMFailed || value == LLMTimedOut || value == LLMRateLimited
}

func validSLI(value SLI) bool {
	return value == SLITurnCommand || value == SLISessionRead || value == SLISSEResume || value == SLIRunTerminalization ||
		value == SLIRetrieval || value == SLIFinalCommit
}

func validSLIClassification(value SLIClassification) bool {
	return value == SLIGood || value == SLIBad || value == SLIExcluded
}
