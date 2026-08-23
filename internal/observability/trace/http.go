package trace

import (
	"net/http"
	"strings"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func HTTPMiddleware(provider oteltrace.TracerProvider, propagator propagation.TextMapPropagator) (func(http.Handler) http.Handler, error) {
	if provider == nil || propagator == nil {
		return nil, ErrInvalid
	}
	tracer := provider.Tracer(instrumentationName)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			ctx := propagator.Extract(request.Context(), propagation.HeaderCarrier(request.Header))
			method := normalizedMethod(request.Method)
			ctx, span := tracer.Start(ctx, "HTTP "+method, oteltrace.WithSpanKind(oteltrace.SpanKindServer),
				oteltrace.WithAttributes(attribute.String("http.request.method", method)))
			metrics := httpsnoop.CaptureMetrics(next, response, request.WithContext(ctx))
			span.SetAttributes(attribute.Int("http.response.status_code", metrics.Code))
			if metrics.Code >= 500 {
				span.SetStatus(codes.Error, "server_error")
			} else {
				span.SetStatus(codes.Ok, "")
			}
			span.End()
		})
	}, nil
}

func normalizedMethod(value string) string {
	value = strings.ToUpper(value)
	switch value {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return value
	default:
		return "OTHER"
	}
}
