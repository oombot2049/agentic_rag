package trace

import (
	"context"
	"net"
	"strings"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type RedisHook struct{ tracer oteltrace.Tracer }

func NewRedisHook(provider oteltrace.TracerProvider) (*RedisHook, error) {
	if provider == nil {
		return nil, ErrInvalid
	}
	return &RedisHook{tracer: provider.Tracer(instrumentationName)}, nil
}

func (*RedisHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return next(ctx, network, address)
	}
}

func (h *RedisHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, command goredis.Cmder) error {
		operation := redisOperation(command.Name())
		ctx, span := h.tracer.Start(ctx, "db.redis "+operation, oteltrace.WithSpanKind(oteltrace.SpanKindClient),
			oteltrace.WithAttributes(attribute.String("db.system.name", "redis"), attribute.String("db.operation.name", operation)))
		err := next(ctx, command)
		finishClientSpan(span, err)
		return err
	}
}

func (h *RedisHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, commands []goredis.Cmder) error {
		ctx, span := h.tracer.Start(ctx, "db.redis PIPELINE", oteltrace.WithSpanKind(oteltrace.SpanKindClient),
			oteltrace.WithAttributes(attribute.String("db.system.name", "redis"), attribute.String("db.operation.name", "PIPELINE"), attribute.Int("db.operation.batch.size", len(commands))))
		err := next(ctx, commands)
		finishClientSpan(span, err)
		return err
	}
}

func finishClientSpan(span oteltrace.Span, err error) {
	if err != nil {
		span.AddEvent("exception", oteltrace.WithAttributes(attribute.String("exception.type", "database_error")))
		span.SetStatus(codes.Error, "database_error")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func redisOperation(value string) string {
	value = strings.ToUpper(value)
	switch value {
	case "PING", "SET", "GET", "DEL", "XADD", "EXPIRE", "XREAD", "EXEC":
		return value
	default:
		return "OTHER"
	}
}

var _ goredis.Hook = (*RedisHook)(nil)
