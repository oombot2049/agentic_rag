package trace

import (
	"context"
	"errors"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/agenticrag/agenticrag"

type Carrier struct {
	TraceParent string
	TraceState  string
}

func StartRun(ctx context.Context, carrier Carrier, deliveryCount uint64) (context.Context, oteltrace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	extracted := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": carrier.TraceParent, "tracestate": carrier.TraceState})
	linked := oteltrace.SpanContextFromContext(extracted)
	options := []oteltrace.SpanStartOption{oteltrace.WithNewRoot(), oteltrace.WithSpanKind(oteltrace.SpanKindConsumer),
		oteltrace.WithAttributes(attribute.String("messaging.system", "agenticrag.run_queue"),
			attribute.String("messaging.operation.name", "process"), attribute.Int64("messaging.message.delivery_count", int64(deliveryCount)))}
	if linked.IsValid() {
		options = append(options, oteltrace.WithLinks(oteltrace.Link{SpanContext: linked}))
	}
	return otel.Tracer(instrumentationName).Start(ctx, "agenticrag.run process", options...)
}

func Inject(ctx context.Context) Carrier {
	if ctx == nil || !oteltrace.SpanContextFromContext(ctx).IsValid() {
		return Carrier{}
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return Carrier{TraceParent: carrier.Get("traceparent"), TraceState: carrier.Get("tracestate")}
}

func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	spanContext := oteltrace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

func StartRAG(ctx context.Context, stage string) (context.Context, oteltrace.Span) {
	stage = normalizeStage(stage)
	return otel.Tracer(instrumentationName).Start(ctx, "rag."+stage,
		oteltrace.WithAttributes(attribute.String("gen_ai.operation.name", stage)))
}

func StartLLM(ctx context.Context, provider, model string) (context.Context, oteltrace.Span) {
	attributes := []attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat")}
	if validConfiguredToken(provider, 128) {
		attributes = append(attributes, attribute.String("gen_ai.provider.name", provider))
	}
	if validConfiguredToken(model, 256) {
		attributes = append(attributes, attribute.String("gen_ai.request.model", model))
	}
	return otel.Tracer(instrumentationName).Start(ctx, "gen_ai.chat", oteltrace.WithAttributes(attributes...))
}

func End(span oteltrace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		class := errorClass(err)
		span.AddEvent("exception", oteltrace.WithAttributes(attribute.String("exception.type", class)))
		span.SetStatus(codes.Error, class)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "operation_failed"
	}
}

func normalizeStage(stage string) string {
	switch stage {
	case "rewrite", "hybrid", "rerank", "embedding", "ingestion":
		return stage
	default:
		return "other"
	}
}

func validConfiguredToken(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == ':' || r == '/')
	}) < 0
}
