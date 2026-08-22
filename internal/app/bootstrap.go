package app

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/agenticrag/agenticrag/internal/buildinfo"
	"github.com/agenticrag/agenticrag/internal/config"
	applog "github.com/agenticrag/agenticrag/internal/observability/log"
)

// ConfigLoader supplies a complete configuration value.
type ConfigLoader func() (config.Config, error)

// Dependencies are the process-level values available to component factories.
type Dependencies struct {
	Config    config.Config
	Logger    *slog.Logger
	BuildInfo buildinfo.Info
}

// Factory constructs an inactive component from explicit dependencies.
type Factory func(Dependencies) (Component, error)

// Options defines replaceable bootstrap boundaries.
type Options struct {
	LoadConfig ConfigLoader
	LogWriter  io.Writer
	BuildInfo  buildinfo.Info
	Factories  []Factory
}

// Build validates process inputs and constructs an inactive application.
func Build(options Options) (*App, error) {
	loader := options.LoadConfig
	if loader == nil {
		loader = config.Load
	}
	cfg, err := loader()
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}

	writer := options.LogWriter
	if writer == nil {
		writer = os.Stdout
	}
	info := options.BuildInfo
	if strings.TrimSpace(info.Version) == "" {
		info = buildinfo.Current()
	}
	logger := applog.NewJSON(writer, applog.Options{
		Level:       logLevel(cfg.Logging.Level),
		Service:     "agenticrag",
		Environment: string(cfg.Environment),
		Version:     info.Version,
	})
	dependencies := Dependencies{Config: cfg, Logger: logger, BuildInfo: info}

	components := make([]Component, 0, len(options.Factories))
	names := make(map[string]struct{}, len(options.Factories))
	for index, factory := range options.Factories {
		if factory == nil {
			return nil, fmt.Errorf("component factory %d is nil", index)
		}
		component, err := factory(dependencies)
		if err != nil {
			return nil, fmt.Errorf("construct component %d: %w", index, err)
		}
		if component == nil {
			return nil, fmt.Errorf("component factory %d returned nil", index)
		}
		name := strings.TrimSpace(component.Name())
		if name == "" {
			return nil, fmt.Errorf("component factory %d returned an empty name", index)
		}
		if _, duplicate := names[name]; duplicate {
			return nil, fmt.Errorf("duplicate component name %q", name)
		}
		names[name] = struct{}{}
		components = append(components, component)
	}

	return &App{
		state: stateReady, components: components, shutdownTimeout: cfg.Server.ShutdownTimeout,
	}, nil
}

func logLevel(level config.LogLevel) slog.Level {
	switch level {
	case config.LogLevelDebug:
		return slog.LevelDebug
	case config.LogLevelWarn:
		return slog.LevelWarn
	case config.LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
