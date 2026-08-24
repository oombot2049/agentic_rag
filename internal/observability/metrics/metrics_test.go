package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientmodel "github.com/prometheus/client_model/go"
)

func TestMetricFamiliesAndHTTPMiddleware(t *testing.T) {
	value := testMetrics(t)
	middleware, err := value.HTTPMiddleware(func(*http.Request) Plan { return PlanStandard })
	if err != nil {
		t.Fatal(err)
	}
	if middleware(nil) != nil {
		t.Fatal("nil HTTP handler accepted")
	}
	flusherPreserved := false
	handler := middleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, flusherPreserved = response.(http.Flusher)
		response.WriteHeader(http.StatusCreated)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/turns?tenant_id=secret", nil))
	if response.Code != http.StatusCreated || !flusherPreserved {
		t.Fatalf("status=%d flusher=%v", response.Code, flusherPreserved)
	}
	if err = value.ObserveHTTP(Plan("tenant-premium-123"), "ATTACKER_METHOD", 503, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	run, err := value.StartRun(PlanEnterprise, 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := metricValue(t, value, "agenticrag_worker_active_runs", map[string]string{"plan": "enterprise"}); got != 1 {
		t.Fatalf("active=%v", got)
	}
	if err = run.Finish(RunSucceeded, 7, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = run.Finish(RunSucceeded, 7, 3*time.Second); err != ErrFinished {
		t.Fatalf("duplicate=%v", err)
	}
	if got := metricValue(t, value, "agenticrag_worker_active_runs", map[string]string{"plan": "enterprise"}); got != 0 {
		t.Fatalf("active=%v", got)
	}

	if err = value.ObserveLLM(LLMObservation{Plan: PlanStandard, Provider: "openai", Model: "gpt-5-mini", Outcome: LLMSucceeded,
		Duration: time.Second, TTFT: 100 * time.Millisecond, InputTokens: 100, OutputTokens: 20, CostNanos: 1234}); err != nil {
		t.Fatal(err)
	}
	if got := metricValue(t, value, "agenticrag_llm_input_tokens_total", map[string]string{"plan": "standard", "provider": "openai", "model": "gpt-5-mini"}); got != 100 {
		t.Fatalf("tokens=%v", got)
	}
	if got := histogramCount(t, value, "agenticrag_llm_ttft_seconds", map[string]string{"plan": "standard", "provider": "openai", "model": "gpt-5-mini"}); got != 1 {
		t.Fatalf("ttft count=%d", got)
	}

	exposition := httptest.NewRecorder()
	value.Handler().ServeHTTP(exposition, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if exposition.Code != 200 || !strings.Contains(exposition.Body.String(), "agenticrag_run_steps_bucket") || strings.Contains(exposition.Body.String(), "tenant_id") {
		t.Fatalf("metrics response=%d body=%s", exposition.Code, exposition.Body.String())
	}
}

func TestCardinalityAttackCollapsesUntrustedDimensions(t *testing.T) {
	value := testMetrics(t)
	for i := 0; i < 2000; i++ {
		attacker := fmt.Sprintf("tenant_%d_secret", i)
		if err := value.ObserveHTTP(Plan(attacker), "METHOD_"+attacker, 200, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := value.ObserveLLM(LLMObservation{Plan: Plan(attacker), Provider: "provider-" + attacker, Model: "model-" + attacker,
			Outcome: LLMFailed, Duration: time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	families, err := value.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if len(family.Metric) > 8 {
			t.Fatalf("family %s series=%d", family.GetName(), len(family.Metric))
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "tenant_id" || label.GetName() == "run_id" || strings.Contains(label.GetValue(), "tenant_") {
					t.Fatalf("unbounded label %s=%s", label.GetName(), label.GetValue())
				}
			}
		}
	}
	if got := metricValue(t, value, "agenticrag_http_requests_total", map[string]string{"plan": "unknown", "method": "OTHER", "status_class": "2xx"}); got != 2000 {
		t.Fatalf("collapsed HTTP=%v", got)
	}
	if got := metricValue(t, value, "agenticrag_llm_requests_total", map[string]string{"plan": "unknown", "provider": "other", "model": "other", "outcome": "failed"}); got != 2000 {
		t.Fatalf("collapsed LLM=%v", got)
	}
}

func TestConcurrentRunGaugeAndSingleFinish(t *testing.T) {
	value := testMetrics(t)
	const count = 200
	handles := make([]*RunHandle, count)
	for i := range handles {
		handles[i], _ = value.StartRun(PlanFree, time.Duration(i)*time.Millisecond)
	}
	var wait sync.WaitGroup
	for _, handle := range handles {
		wait.Add(1)
		go func(handle *RunHandle) {
			defer wait.Done()
			if err := handle.Finish(RunSucceeded, 3, time.Second); err != nil {
				t.Errorf("finish=%v", err)
			}
		}(handle)
	}
	wait.Wait()
	if active := metricValue(t, value, "agenticrag_worker_active_runs", map[string]string{"plan": "free"}); active != 0 {
		t.Fatalf("active=%v", active)
	}
	if runs := metricValue(t, value, "agenticrag_runs_total", map[string]string{"plan": "free", "outcome": "succeeded"}); runs != count {
		t.Fatalf("runs=%v", runs)
	}

	handle, _ := value.StartRun(PlanFree, 0)
	var winners atomic.Int64
	wait = sync.WaitGroup{}
	for i := 0; i < 20; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if handle.Finish(RunFailed, 1, time.Second) == nil {
				winners.Add(1)
			}
		}()
	}
	wait.Wait()
	if winners.Load() != 1 || metricValue(t, value, "agenticrag_worker_active_runs", map[string]string{"plan": "free"}) != 0 {
		t.Fatalf("winners=%d", winners.Load())
	}
}

func TestValidationAndTTFTOmission(t *testing.T) {
	for index, config := range []Config{
		{AllowedModels: map[string][]string{"bad provider": {"model"}}},
		{AllowedModels: map[string][]string{"provider": {"model", "model"}}},
		{AllowedModels: tooManyModels()},
	} {
		if _, err := New(config); err != ErrInvalid {
			t.Errorf("config %d=%v", index, err)
		}
	}
	value := testMetrics(t)
	if value.ObserveHTTP(PlanFree, "GET", 99, 0) != ErrInvalid {
		t.Fatal("bad status accepted")
	}
	if _, err := value.StartRun(PlanFree, -time.Second); err != ErrInvalid {
		t.Fatal(err)
	}
	handle, _ := value.StartRun(PlanFree, 0)
	if err := handle.Finish("unknown", 1, time.Second); err != ErrInvalid {
		t.Fatal(err)
	}
	if err := handle.Finish(RunCanceled, 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := value.ObserveLLM(LLMObservation{Plan: PlanFree, Provider: "openai", Model: "gpt-5-mini", Outcome: LLMSucceeded,
		Duration: time.Second, TTFT: 2 * time.Second}); err != ErrInvalid {
		t.Fatal(err)
	}
	if err := value.ObserveLLM(LLMObservation{Plan: PlanFree, Provider: "openai", Model: "gpt-5-mini", Outcome: LLMSucceeded,
		Duration: time.Second}); err != nil {
		t.Fatal(err)
	}
	if hasMetric(value, "agenticrag_llm_ttft_seconds", map[string]string{"plan": "free", "provider": "openai", "model": "gpt-5-mini"}) {
		t.Fatal("zero TTFT emitted")
	}
	if _, err := value.HTTPMiddleware(nil); err != ErrInvalid {
		t.Fatal(err)
	}
}

func TestLogicalSLIClassificationIsClosedAndObservable(t *testing.T) {
	value := testMetrics(t)
	for _, classification := range []SLIClassification{SLIGood, SLIBad, SLIExcluded} {
		if err := value.ObserveSLI(SLITurnCommand, classification); err != nil {
			t.Fatal(err)
		}
		if got := metricValue(t, value, "agenticrag_sli_events_total", map[string]string{
			"sli": "turn_command", "classification": string(classification),
		}); got != 1 {
			t.Errorf("%s count = %v", classification, got)
		}
	}
	if err := value.ObserveSLI("attacker", SLIGood); err != ErrInvalid {
		t.Fatalf("invalid SLI error = %v", err)
	}
	if err := value.ObserveSLI(SLITurnCommand, "attacker"); err != ErrInvalid {
		t.Fatalf("invalid classification error = %v", err)
	}
}

func testMetrics(t *testing.T) *Metrics {
	t.Helper()
	value, err := New(Config{AllowedModels: map[string][]string{"openai": {"gpt-5-mini"}, "anthropic": {"claude-sonnet"}}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func metricValue(t *testing.T, value *Metrics, family string, labels map[string]string) float64 {
	t.Helper()
	metric := findMetric(t, value, family, labels)
	if metric.Counter != nil {
		return metric.Counter.GetValue()
	}
	if metric.Gauge != nil {
		return metric.Gauge.GetValue()
	}
	t.Fatalf("family %s is not counter/gauge", family)
	return 0
}

func histogramCount(t *testing.T, value *Metrics, family string, labels map[string]string) uint64 {
	t.Helper()
	return findMetric(t, value, family, labels).GetHistogram().GetSampleCount()
}

func hasMetric(value *Metrics, family string, labels map[string]string) bool {
	families, _ := value.registry.Gather()
	for _, item := range families {
		if item.GetName() == family {
			for _, metric := range item.Metric {
				if matches(metric, labels) {
					return true
				}
			}
		}
	}
	return false
}

func findMetric(t *testing.T, value *Metrics, family string, labels map[string]string) *clientmodel.Metric {
	t.Helper()
	families, err := value.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range families {
		if item.GetName() == family {
			for _, metric := range item.Metric {
				if matches(metric, labels) {
					return metric
				}
			}
		}
	}
	t.Fatalf("metric %s labels=%v not found", family, labels)
	return nil
}

func matches(metric *clientmodel.Metric, labels map[string]string) bool {
	if len(metric.Label) != len(labels) {
		return false
	}
	for _, label := range metric.Label {
		if labels[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}

func tooManyModels() map[string][]string {
	models := make([]string, 129)
	for i := range models {
		models[i] = fmt.Sprintf("model-%d", i)
	}
	return map[string][]string{"provider": models}
}
