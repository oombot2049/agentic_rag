package trace

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type PGXTracer struct{ tracer oteltrace.Tracer }

func NewPGXTracer(provider oteltrace.TracerProvider) (*PGXTracer, error) {
	if provider == nil {
		return nil, ErrInvalid
	}
	return &PGXTracer{tracer: provider.Tracer(instrumentationName)}, nil
}

func (t *PGXTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	operation := sqlOperation(data.SQL)
	ctx, _ = t.tracer.Start(ctx, "db.postgresql "+operation, oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attribute.String("db.system.name", "postgresql"), attribute.String("db.operation.name", operation)))
	return ctx
}

func (*PGXTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := oteltrace.SpanFromContext(ctx)
	if data.Err != nil {
		span.AddEvent("exception", oteltrace.WithAttributes(attribute.String("exception.type", "database_error")))
		span.SetStatus(codes.Error, "database_error")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func sqlOperation(statement string) string {
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return "OTHER"
	}
	operation := strings.ToUpper(fields[0])
	switch operation {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "BEGIN", "COMMIT", "ROLLBACK", "COPY", "CREATE", "ALTER", "DROP":
		return operation
	default:
		return "OTHER"
	}
}

var _ pgx.QueryTracer = (*PGXTracer)(nil)
