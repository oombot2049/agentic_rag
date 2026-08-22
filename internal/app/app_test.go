package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/buildinfo"
	"github.com/agenticrag/agenticrag/internal/config"
)

type fakeComponent struct {
	name     string
	events   *[]string
	startErr error
	stopErr  error
	mu       *sync.Mutex
}

func (c *fakeComponent) Name() string { return c.name }

func (c *fakeComponent) record(event string) {
	if c.mu != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
	}
	*c.events = append(*c.events, event)
}

func (c *fakeComponent) Start(context.Context) error {
	c.record("start:" + c.name)
	return c.startErr
}

func (c *fakeComponent) Stop(context.Context) error {
	c.record("stop:" + c.name)
	return c.stopErr
}

func validConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": "postgres://test"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func buildWithComponents(t *testing.T, components ...Component) *App {
	t.Helper()
	factories := make([]Factory, 0, len(components))
	for _, component := range components {
		component := component
		factories = append(factories, func(Dependencies) (Component, error) { return component, nil })
	}
	application, err := Build(Options{
		LoadConfig: func() (config.Config, error) { return validConfig(t), nil },
		LogWriter:  &bytes.Buffer{},
		Factories:  factories,
	})
	if err != nil {
		t.Fatal(err)
	}
	return application
}

func TestBuildInjectsDependenciesAndLoggerPolicy(t *testing.T) {
	cfg := validConfig(t)
	cfg.Environment = config.EnvironmentTest
	cfg.Logging.Level = config.LogLevelWarn
	var output bytes.Buffer
	info := buildinfo.Info{Version: "v-test", Commit: "deadbeef", BuildDate: "now"}
	var received Dependencies
	application, err := Build(Options{
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		LogWriter:  &output,
		BuildInfo:  info,
		Factories: []Factory{func(dependencies Dependencies) (Component, error) {
			received = dependencies
			return &fakeComponent{name: "worker", events: &[]string{}}, nil
		}},
	})
	if err != nil || application == nil {
		t.Fatalf("Build() = %v, %v", application, err)
	}
	if received.Config.Environment != config.EnvironmentTest || received.BuildInfo != info || received.Logger == nil {
		t.Fatalf("dependencies = %+v", received)
	}
	received.Logger.Info("filtered_info")
	received.Logger.Warn("visible_warning")
	logged := output.String()
	if strings.Contains(logged, "filtered_info") || !strings.Contains(logged, "visible_warning") ||
		!strings.Contains(logged, `"service":"agenticrag"`) || !strings.Contains(logged, `"version":"v-test"`) ||
		!strings.Contains(logged, `"environment":"test"`) {
		t.Fatalf("logger policy mismatch: %s", logged)
	}
}

func TestLifecycleOrderAndIdempotence(t *testing.T) {
	var events []string
	components := []Component{
		&fakeComponent{name: "database", events: &events},
		&fakeComponent{name: "http", events: &events},
		&fakeComponent{name: "worker", events: &events},
	}
	application := buildWithComponents(t, components...)
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := application.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start() error = %v", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	want := "start:database,start:http,start:worker,stop:worker,stop:http,stop:database"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if err := application.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start after shutdown error = %v", err)
	}
}

func TestStartFailureRollsBackAndJoinsErrors(t *testing.T) {
	var events []string
	startFailure := errors.New("start failure")
	rollbackFailure := errors.New("rollback failure")
	application := buildWithComponents(t,
		&fakeComponent{name: "database", events: &events, stopErr: rollbackFailure},
		&fakeComponent{name: "http", events: &events},
		&fakeComponent{name: "worker", events: &events, startErr: startFailure},
	)
	err := application.Start(context.Background())
	if !errors.Is(err, startFailure) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("Start() error = %v", err)
	}
	want := "start:database,start:http,start:worker,stop:http,stop:database"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after rollback = %v", err)
	}
}

func TestShutdownAttemptsEveryComponent(t *testing.T) {
	var events []string
	firstFailure := errors.New("first stop failure")
	secondFailure := errors.New("second stop failure")
	application := buildWithComponents(t,
		&fakeComponent{name: "database", events: &events, stopErr: firstFailure},
		&fakeComponent{name: "http", events: &events, stopErr: secondFailure},
	)
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := application.Shutdown(context.Background())
	if !errors.Is(err, firstFailure) || !errors.Is(err, secondFailure) {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !strings.HasSuffix(strings.Join(events, ","), "stop:http,stop:database") {
		t.Fatalf("not all components stopped: %v", events)
	}
}

func TestRunCancellationUsesFreshShutdownContext(t *testing.T) {
	var events []string
	component := &contextCheckingComponent{name: "worker", events: &events}
	application := buildWithComponents(t, component)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if component.stopContextCanceled {
		t.Fatal("Run passed canceled run context to Stop")
	}
	//lint:ignore SA1012 Deliberately verifies the defensive nil-context contract.
	if err := application.Run(nil); err == nil {
		t.Fatal("Run(nil) error = nil")
	}
}

type contextCheckingComponent struct {
	name                string
	events              *[]string
	stopContextCanceled bool
}

func (c *contextCheckingComponent) Name() string { return c.name }
func (c *contextCheckingComponent) Start(context.Context) error {
	*c.events = append(*c.events, "start:"+c.name)
	return nil
}
func (c *contextCheckingComponent) Stop(ctx context.Context) error {
	c.stopContextCanceled = ctx.Err() != nil
	*c.events = append(*c.events, "stop:"+c.name)
	return nil
}

func TestBuildRejectsInvalidInputsBeforeStartup(t *testing.T) {
	loadFailure := errors.New("load failure")
	factoryFailure := errors.New("factory failure")
	tests := []struct {
		name    string
		options Options
		match   string
		is      error
	}{
		{name: "loader failure", options: Options{LoadConfig: func() (config.Config, error) { return config.Config{}, loadFailure }}, match: "load configuration", is: loadFailure},
		{name: "invalid injected config", options: Options{LoadConfig: func() (config.Config, error) { return config.Config{}, nil }}, match: "validate configuration"},
		{name: "nil factory", options: Options{LoadConfig: func() (config.Config, error) { return validConfig(t), nil }, Factories: []Factory{nil}}, match: "factory 0 is nil"},
		{name: "factory failure", options: Options{LoadConfig: func() (config.Config, error) { return validConfig(t), nil }, Factories: []Factory{func(Dependencies) (Component, error) { return nil, factoryFailure }}}, match: "construct component 0", is: factoryFailure},
		{name: "nil component", options: Options{LoadConfig: func() (config.Config, error) { return validConfig(t), nil }, Factories: []Factory{func(Dependencies) (Component, error) { return nil, nil }}}, match: "returned nil"},
		{name: "empty name", options: Options{LoadConfig: func() (config.Config, error) { return validConfig(t), nil }, Factories: []Factory{func(Dependencies) (Component, error) { return &fakeComponent{name: " ", events: &[]string{}}, nil }}}, match: "empty name"},
		{name: "duplicate name", options: Options{LoadConfig: func() (config.Config, error) { return validConfig(t), nil }, Factories: []Factory{
			func(Dependencies) (Component, error) { return &fakeComponent{name: "same", events: &[]string{}}, nil },
			func(Dependencies) (Component, error) { return &fakeComponent{name: "same", events: &[]string{}}, nil },
		}}, match: "duplicate component name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			application, err := Build(tt.options)
			if err == nil || application != nil || !strings.Contains(err.Error(), tt.match) {
				t.Fatalf("Build() = %v, %v; want %q", application, err, tt.match)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("Build() error %v does not wrap %v", err, tt.is)
			}
		})
	}
}

func TestShutdownBeforeStartStopsApplication(t *testing.T) {
	application := buildWithComponents(t)
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := application.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestLogLevelMapping(t *testing.T) {
	tests := []struct {
		input config.LogLevel
		want  slog.Level
	}{
		{config.LogLevelDebug, slog.LevelDebug}, {config.LogLevelInfo, slog.LevelInfo},
		{config.LogLevelWarn, slog.LevelWarn}, {config.LogLevelError, slog.LevelError},
		{config.LogLevel("unexpected"), slog.LevelInfo},
	}
	for _, tt := range tests {
		if got := logLevel(tt.input); got != tt.want {
			t.Errorf("logLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestBuildDefaultBuildInfo(t *testing.T) {
	var received buildinfo.Info
	_, err := Build(Options{
		LoadConfig: func() (config.Config, error) { return validConfig(t), nil },
		LogWriter:  &bytes.Buffer{},
		Factories: []Factory{func(dependencies Dependencies) (Component, error) {
			received = dependencies.BuildInfo
			return &fakeComponent{name: "component", events: &[]string{}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Version == "" {
		t.Fatal("default build info was not injected")
	}
}

func TestShutdownTimeoutIsConfigured(t *testing.T) {
	cfg := validConfig(t)
	cfg.Server.ShutdownTimeout = 2 * time.Second
	application, err := Build(Options{LoadConfig: func() (config.Config, error) { return cfg, nil }, LogWriter: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if application.shutdownTimeout != 2*time.Second {
		t.Fatalf("shutdown timeout = %s", application.shutdownTimeout)
	}
}

func TestConcurrentLifecycleDoesNotDuplicateCalls(t *testing.T) {
	var events []string
	var eventMu sync.Mutex
	component := &fakeComponent{name: "worker", events: &events, mu: &eventMu}
	application := buildWithComponents(t, component)

	const callers = 20
	results := make(chan error, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- application.Start(context.Background())
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyStarted) {
			t.Fatalf("concurrent Start() error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful starts = %d, want 1", successes)
	}

	wait = sync.WaitGroup{}
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := application.Shutdown(context.Background()); err != nil {
				t.Errorf("concurrent Shutdown() error = %v", err)
			}
		}()
	}
	wait.Wait()
	eventMu.Lock()
	got := strings.Join(events, ",")
	eventMu.Unlock()
	if got != "start:worker,stop:worker" {
		t.Fatalf("events = %s", got)
	}
}
