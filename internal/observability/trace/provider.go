// Package trace owns OpenTelemetry setup and bounded instrumentation contracts.
package trace

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var ErrInvalid = errors.New("invalid tracing configuration")

var serviceToken = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,127}$`)
var configToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	SampleRatio    float64
	ExportTimeout  time.Duration
	BatchTimeout   time.Duration
	MaximumQueue   int
	MaximumBatch   int
}

func DefaultConfig() Config {
	return Config{ServiceName: "agenticrag", ServiceVersion: "dev", Environment: "development", SampleRatio: 0.1,
		ExportTimeout: 5 * time.Second, BatchTimeout: time.Second, MaximumQueue: 2048, MaximumBatch: 512}
}

type Provider struct {
	mu         sync.Mutex
	provider   *sdktrace.TracerProvider
	propagator propagation.TextMapPropagator
	started    bool
	stopped    bool
}

func NewProvider(config Config, exporter sdktrace.SpanExporter) (*Provider, error) {
	if exporter == nil || !validConfig(config) {
		return nil, ErrInvalid
	}
	processor := sdktrace.NewBatchSpanProcessor(exporter, sdktrace.WithMaxQueueSize(config.MaximumQueue),
		sdktrace.WithMaxExportBatchSize(config.MaximumBatch), sdktrace.WithBatchTimeout(config.BatchTimeout),
		sdktrace.WithExportTimeout(config.ExportTimeout))
	resources := resource.NewSchemaless(attribute.String("service.name", config.ServiceName),
		attribute.String("service.version", config.ServiceVersion), attribute.String("deployment.environment.name", config.Environment))
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(resources),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(config.SampleRatio))), sdktrace.WithSpanProcessor(processor))
	return &Provider{provider: provider, propagator: propagation.TraceContext{}}, nil
}

func validConfig(config Config) bool {
	return serviceToken.MatchString(config.ServiceName) && configToken.MatchString(config.ServiceVersion) &&
		configToken.MatchString(config.Environment) && config.SampleRatio >= 0 && config.SampleRatio <= 1 &&
		config.ExportTimeout > 0 && config.ExportTimeout <= time.Minute && config.BatchTimeout > 0 && config.BatchTimeout <= time.Minute &&
		config.MaximumQueue >= 1 && config.MaximumQueue <= 65536 && config.MaximumBatch >= 1 && config.MaximumBatch <= config.MaximumQueue
}

func (p *Provider) Name() string { return "opentelemetry-tracing" }

func (p *Provider) Start(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return errors.New("tracing provider is stopped")
	}
	if p.started {
		return nil
	}
	otel.SetTracerProvider(p.provider)
	otel.SetTextMapPropagator(p.propagator)
	p.started = true
	return nil
}

func (p *Provider) ForceFlush(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	return p.provider.ForceFlush(ctx)
}

func (p *Provider) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()
	err := p.provider.Shutdown(ctx)
	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return err
}

func (p *Provider) TracerProvider() oteltrace.TracerProvider  { return p.provider }
func (p *Provider) Propagator() propagation.TextMapPropagator { return p.propagator }
