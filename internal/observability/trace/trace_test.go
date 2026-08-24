package trace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type memoryExporter struct {
	mu       sync.Mutex
	spans    []sdktrace.ReadOnlySpan
	shutdown bool
}

func (e *memoryExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	e.spans = append(e.spans, spans...)
	e.mu.Unlock()
	return nil
}
func (e *memoryExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	e.shutdown = true
	e.mu.Unlock()
	return nil
}
func (e *memoryExporter) snapshot() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

func TestExportedHTTPRunLinkAndSensitiveAttributeBoundary(t *testing.T) {
	exporter := &memoryExporter{}
	config := DefaultConfig()
	config.SampleRatio, config.BatchTimeout = 1, time.Millisecond
	provider, err := NewProvider(config, exporter)
	if err != nil || provider.Start(context.Background()) != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Stop(context.Background()) })

	pgxTracer, _ := NewPGXTracer(provider.TracerProvider())
	redisHook, _ := NewRedisHook(provider.TracerProvider())
	var requestContext oteltrace.SpanContext
	var queueCarrier = Inject(context.Background())
	final := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestContext = oteltrace.SpanContextFromContext(request.Context())
		queueCarrier = Inject(request.Context())
		ragCtx, ragSpan := StartRAG(request.Context(), "hybrid")
		ragSpan.SetAttributes(attribute.Int("agenticrag.rag.result_count", 2))
		End(ragSpan, nil)
		_, llmSpan := StartLLM(ragCtx, "openai", "gpt-5-mini")
		End(llmSpan, nil)
		dbCtx := pgxTracer.TraceQueryStart(request.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT * FROM secret_customer_table WHERE tenant_id=$1"})
		pgxTracer.TraceQueryEnd(dbCtx, nil, pgx.TraceQueryEndData{Err: errors.New("db-secret-value")})
		command := goredis.NewStringCmd(request.Context(), "get", "secret:tenant:key")
		_ = redisHook.ProcessHook(func(context.Context, goredis.Cmder) error { return errors.New("redis-secret-value") })(request.Context(), command)
		pipeline := []goredis.Cmder{goredis.NewStringCmd(request.Context(), "get", "secret:pipeline:key")}
		if err := redisHook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return nil })(request.Context(), pipeline); err != nil {
			t.Error(err)
		}
		response.WriteHeader(http.StatusAccepted)
	})
	middleware, _ := HTTPMiddleware(provider.TracerProvider(), provider.Propagator())
	request := httptest.NewRequest(http.MethodPost, "https://service.example/v1/secret/customer?query=private-prompt", nil)
	request.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	response := httptest.NewRecorder()
	middleware(final).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || queueCarrier.TraceParent == "" {
		t.Fatalf("status=%d trace=%+v", response.Code, queueCarrier)
	}

	runCtx, runSpan := StartRun(context.Background(), queueCarrier, 2)
	runContext := runSpan.SpanContext()
	_, runRAG := StartRAG(runCtx, "rerank")
	End(runRAG, nil)
	End(runSpan, nil)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.snapshot()
	if len(spans) != 8 {
		t.Fatalf("exported spans=%d", len(spans))
	}
	if requestContext.TraceID().String() != "11111111111111111111111111111111" || runContext.TraceID() == requestContext.TraceID() {
		t.Fatalf("request=%s run=%s", requestContext.TraceID(), runContext.TraceID())
	}
	run := findSpan(t, spans, "agenticrag.run process")
	if run.Parent().IsValid() || len(run.Links()) != 1 || run.Links()[0].SpanContext.TraceID() != requestContext.TraceID() ||
		run.Links()[0].SpanContext.SpanID() != requestContext.SpanID() {
		t.Fatalf("run parent=%v links=%+v", run.Parent(), run.Links())
	}
	if child := findSpan(t, spans, "rag.rerank"); child.Parent().SpanID() != runContext.SpanID() {
		t.Fatalf("run RAG parent=%s", child.Parent().SpanID())
	}
	for _, span := range spans {
		serialized := span.Name()
		for _, item := range span.Attributes() {
			serialized += " " + string(item.Key) + "=" + item.Value.Emit()
		}
		for _, event := range span.Events() {
			serialized += " " + event.Name
			for _, item := range event.Attributes {
				serialized += " " + string(item.Key) + "=" + item.Value.Emit()
			}
		}
		for _, forbidden := range []string{"secret_customer_table", "secret:tenant:key", "secret:pipeline:key", "private-prompt", "tenant_id", "/v1/secret/customer", "db-secret-value", "redis-secret-value"} {
			if strings.Contains(serialized, forbidden) {
				t.Errorf("span %q leaked %q: %s", span.Name(), forbidden, serialized)
			}
		}
	}
}

func TestProviderLifecycleValidationAndHelpers(t *testing.T) {
	config := DefaultConfig()
	exporter := &memoryExporter{}
	for index, mutate := range []func(*Config){
		func(v *Config) { v.ServiceName = "Bad Service" }, func(v *Config) { v.SampleRatio = 2 },
		func(v *Config) { v.Environment = "prod\nsecret" },
		func(v *Config) { v.MaximumBatch = v.MaximumQueue + 1 }, func(v *Config) { v.ExportTimeout = 0 },
	} {
		value := config
		mutate(&value)
		if _, err := NewProvider(value, exporter); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d=%v", index, err)
		}
	}
	if _, err := NewProvider(config, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	provider, _ := NewProvider(config, exporter)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.Start(canceled); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := provider.Stop(context.Background()); err != nil || !exporter.shutdown {
		t.Fatalf("stop=%v shutdown=%v", err, exporter.shutdown)
	}
	if err := provider.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background()); err == nil {
		t.Fatal("stopped provider restarted")
	}
	if Inject(context.Background()).TraceParent != "" || TraceID(context.Background()) != "" {
		t.Fatal("untraced context propagated")
	}
	if normalizedMethod("CUSTOM-"+strings.Repeat("X", 100)) != "OTHER" || sqlOperation("vacuum secret") != "OTHER" || redisOperation("get key") != "OTHER" || normalizeStage("query-text") != "other" {
		t.Fatal("unbounded operation accepted")
	}
}

func TestErrorAndMalformedCarrierSpans(t *testing.T) {
	exporter := &memoryExporter{}
	config := DefaultConfig()
	config.SampleRatio, config.BatchTimeout = 1, time.Millisecond
	provider, _ := NewProvider(config, exporter)
	_ = provider.Start(context.Background())
	defer provider.Stop(context.Background())
	_, span := StartRun(context.Background(), structCarrier("invalid"), 1)
	End(span, context.DeadlineExceeded)
	middleware, _ := HTTPMiddleware(provider.TracerProvider(), propagation.TraceContext{})
	middleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(503) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("ODD", "/secret", nil))
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.snapshot()
	if len(spans) != 2 || len(findSpan(t, spans, "agenticrag.run process").Links()) != 0 || findSpan(t, spans, "HTTP OTHER").Status().Code.String() != "Error" {
		t.Fatalf("spans=%+v", spans)
	}
	if _, err := HTTPMiddleware(nil, propagation.TraceContext{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := NewPGXTracer(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := NewRedisHook(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func structCarrier(traceParent string) Carrier {
	return Carrier{TraceParent: traceParent}
}
