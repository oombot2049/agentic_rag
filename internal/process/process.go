// Package process owns executable role composition.
package process

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/app"
	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
	"github.com/agenticrag/agenticrag/internal/outbox"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
	"github.com/agenticrag/agenticrag/internal/queue"
	"github.com/agenticrag/agenticrag/internal/repository"
	runcontrol "github.com/agenticrag/agenticrag/internal/run"
	"github.com/agenticrag/agenticrag/internal/session"
	httptransport "github.com/agenticrag/agenticrag/internal/transport/http"
	"github.com/agenticrag/agenticrag/internal/transport/sse"
	"github.com/agenticrag/agenticrag/internal/turn"
	"github.com/agenticrag/agenticrag/internal/worker"
)

type Role string

const (
	RoleServe        Role = "serve"
	RoleWorker       Role = "worker"
	RoleRelay        Role = "relay"
	RoleIngestWorker Role = "ingest-worker"
)

// Run constructs only dependencies owned by role. Worker roles fail closed
// until their external Provider adapters are explicitly supplied; an inert
// consumer must never claim durable work.
func Run(ctx context.Context, role Role, logger *slog.Logger) error {
	if ctx == nil {
		return errors.New("process context is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch role {
	case RoleRelay:
		return runRelay(ctx, cfg, logger)
	case RoleServe:
		dependencies, err := serveDependencies(cfg.Environment)
		if err != nil {
			return err
		}
		return RunServe(ctx, cfg, dependencies, logger)
	case RoleWorker:
		return errors.New("worker composition requires an LLM/retrieval Processor adapter")
	case RoleIngestWorker:
		return errors.New("ingest-worker composition requires Artifact and embedding Provider adapters")
	default:
		return fmt.Errorf("unknown process role %q", role)
	}
}

func serveDependencies(environment config.Environment) (ServeDependencies, error) {
	if environment == config.EnvironmentProduction {
		cursorKey := []byte(os.Getenv("AGENTICRAG_CURSOR_SIGNING_KEY"))
		if len(cursorKey) < 32 || strings.TrimSpace(string(cursorKey)) != string(cursorKey) {
			return ServeDependencies{}, errors.New("production serve requires AGENTICRAG_CURSOR_SIGNING_KEY with at least 32 bytes")
		}
		return ServeDependencies{CursorSigningKey: cursorKey, AuthenticatorFactory: postgresAPIKeyAuthenticator}, nil
	}
	rawToken := os.Getenv("AGENTICRAG_DEV_BEARER_TOKEN")
	tenantID, err := domain.ParseTenantID(os.Getenv("AGENTICRAG_DEV_TENANT_ID"))
	if err != nil || len(rawToken) < 16 || strings.TrimSpace(rawToken) != rawToken {
		return ServeDependencies{}, errors.New("development serve requires AGENTICRAG_DEV_BEARER_TOKEN (>=16 bytes) and AGENTICRAG_DEV_TENANT_ID")
	}
	principal, err := auth.NewPrincipal(tenantID, "development-operator", []string{"sessions:read", "sessions:write"})
	if err != nil {
		return ServeDependencies{}, err
	}
	verifier := auth.VerifierFunc(func(_ context.Context, supplied string) (auth.Principal, error) {
		if subtle.ConstantTimeCompare([]byte(rawToken), []byte(supplied)) != 1 {
			return auth.Principal{}, auth.ErrInvalidCredential
		}
		return principal, nil
	})
	authenticator, err := auth.NewAuthenticator(verifier, nil)
	if err != nil {
		return ServeDependencies{}, err
	}
	cursorKey := []byte(os.Getenv("AGENTICRAG_CURSOR_SIGNING_KEY"))
	if len(cursorKey) < 32 || strings.TrimSpace(string(cursorKey)) != string(cursorKey) {
		return ServeDependencies{}, errors.New("development serve requires AGENTICRAG_CURSOR_SIGNING_KEY with at least 32 bytes")
	}
	return ServeDependencies{Authenticator: authenticator, CursorSigningKey: cursorKey}, nil
}

// ServeDependencies are security-owned adapters that cannot be inferred from
// generic process configuration. A deployment must provide them explicitly.
type ServeDependencies struct {
	Authenticator        *auth.Authenticator
	AuthenticatorFactory func(auth.APIKeyDB) (*auth.Authenticator, error)
	CursorSigningKey     []byte
}

type WorkerDependencies struct{ Processor worker.Processor }

type IngestWorkerDependencies struct{ Component app.Component }

// RunIngestWorker owns lifecycle for a fully composed ingestion component. The
// Artifact, parser, embedding and index adapters remain explicit dependencies
// of that component and are never replaced by silent no-op implementations.
func RunIngestWorker(ctx context.Context, cfg config.Config, dependencies IngestWorkerDependencies) error {
	if dependencies.Component == nil {
		return errors.New("ingestion component is required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := dependencies.Component.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	return dependencies.Component.Stop(stopCtx)
}

// RunWorker wires the PostgreSQL Run Queue and bounded Worker lifecycle around
// an explicitly supplied Agent processor. Provider construction remains at the
// deployment boundary so a missing LLM adapter cannot consume and Nack work.
func RunWorker(ctx context.Context, cfg config.Config, dependencies WorkerDependencies, logger *slog.Logger) error {
	if dependencies.Processor == nil {
		return errors.New("Agent worker Processor is required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	pool, err := platformpostgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	queueAdapter, err := queue.NewPostgresAdapter(queueTransactions(pool), queue.Config{
		MaxPendingPerTenant: cfg.Queue.MaxPendingPerTenant, MaxPendingGlobal: cfg.Queue.MaxPendingGlobal,
	})
	if err != nil {
		return err
	}
	component, err := worker.New(queueAdapter, dependencies.Processor, worker.Config{
		ID: cfg.Worker.ID, MaxConcurrency: cfg.Worker.MaxConcurrency,
		MaxConcurrencyPerTenant: cfg.Worker.MaxConcurrencyPerTenant,
		PollInterval:            cfg.Worker.PollInterval, LeaseDuration: cfg.Worker.LeaseDuration,
		RenewInterval: cfg.Worker.RenewInterval, RetryDelay: cfg.Worker.RetryDelay, TenantScanLimit: 100,
	}, logger)
	if err != nil {
		return err
	}
	if err := component.Start(ctx); err != nil {
		return err
	}
	logger.Info("starting role", "role", RoleWorker)
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	return component.Stop(stopCtx)
}

// RunServe wires PostgreSQL repositories, application services, authenticated
// HTTP routes, Redis SSE live tail and the production HTTP lifecycle.
func RunServe(ctx context.Context, cfg config.Config, dependencies ServeDependencies, logger *slog.Logger) error {
	if (dependencies.Authenticator == nil && dependencies.AuthenticatorFactory == nil) || len(dependencies.CursorSigningKey) < 32 {
		return errors.New("serve authentication and cursor signing dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	pool, err := platformpostgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	redisClient, redisErr := platformredis.Open(ctx, cfg.Redis)
	authenticator := dependencies.Authenticator
	if authenticator == nil {
		authenticator, err = dependencies.AuthenticatorFactory(pool)
		if err != nil {
			return err
		}
	}
	authLayer, err := auth.Middleware(authenticator, nil)
	if err != nil {
		return err
	}

	transactions := repository.WithinTxFunc(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, repository.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	})
	sessionStore, err := repository.NewSessionAPIStore(pool, transactions)
	if err != nil {
		return err
	}
	sessionService, err := session.NewService(sessionStore, nil)
	if err != nil {
		return err
	}
	sessionHandler, err := httptransport.NewSessionHandler(sessionService)
	if err != nil {
		return err
	}
	turnStore, err := repository.NewTurnAPIStore(transactions)
	if err != nil {
		return err
	}
	turnService, err := turn.NewService(turnStore, nil)
	if err != nil {
		return err
	}
	turnHandler, err := httptransport.NewTurnHandler(turnService)
	if err != nil {
		return err
	}
	queryStore, err := repository.NewTurnQueryStore(transactions)
	if err != nil {
		return err
	}
	cursor, err := turn.NewCursorCodec(dependencies.CursorSigningKey)
	if err != nil {
		return err
	}
	queryService, err := turn.NewQueryService(queryStore, cursor)
	if err != nil {
		return err
	}
	queryHandler, err := httptransport.NewQueryHandler(queryService)
	if err != nil {
		return err
	}
	cancelStore, err := repository.NewRunCancelStore(transactions)
	if err != nil {
		return err
	}
	cancelService, err := runcontrol.NewService(cancelStore, nil)
	if err != nil {
		return err
	}
	cancelHandler, err := httptransport.NewRunCancelHandler(cancelService)
	if err != nil {
		return err
	}

	var subscriber sse.Subscriber
	var outputBroker *sse.RedisOutputBroker
	if redisErr != nil {
		logger.Warn("Redis live tail unavailable; SSE using PostgreSQL polling", "error", redisErr)
		subscriber = sse.NewPollingSubscriber(redisErr)
	} else {
		defer redisClient.Close()
		keys, keyErr := platformredis.NewKeyBuilder(string(cfg.Environment))
		if keyErr != nil {
			return keyErr
		}
		broker, brokerErr := sse.NewRedisBroker(redisClient, keys, cfg.Redis.MaxTTL, time.Second)
		if brokerErr != nil {
			return brokerErr
		}
		subscriber = broker
		outputBroker, brokerErr = sse.NewRedisOutputBroker(redisClient, keys, cfg.Redis.OutputTTL, time.Second)
		if brokerErr != nil {
			return brokerErr
		}
	}
	eventRepository, err := events.NewRepository(pool)
	if err != nil {
		return err
	}
	eventHandler, err := sse.NewHandler(eventRepository, subscriber, sse.DefaultConfig())
	if err != nil {
		return err
	}
	var outputHandler *sse.OutputHandler
	if outputBroker != nil {
		outputHandler, err = sse.NewOutputHandler(eventRepository, outputBroker, sse.DefaultConfig())
		if err != nil {
			return err
		}
	}

	routes := http.NewServeMux()
	mount(routes, sessionHandler.Routes(), "POST /v1/sessions", "GET /v1/sessions/{session_id}")
	mount(routes, turnHandler.Routes(), "POST /v1/sessions/{session_id}/turns")
	mount(routes, queryHandler.Routes(), "GET /v1/turns/{turn_id}", "GET /v1/runs/{run_id}", "GET /v1/sessions/{session_id}/messages")
	mount(routes, cancelHandler.Routes(), "POST /v1/runs/{run_action}")
	mount(routes, eventHandler.Routes(), "GET /v1/runs/{run_id}/events")
	if outputHandler != nil {
		mount(routes, outputHandler.Routes(), "GET /v1/runs/{run_id}/output")
	} else {
		routes.HandleFunc("GET /v1/runs/{run_id}/output", func(response http.ResponseWriter, request *http.Request) {
			auth.WriteFailure(response, request, apperror.New(apperror.CodeServiceUnavailable, true, redisErr))
		})
	}
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(response http.ResponseWriter, request *http.Request) {
		if err := pool.Health(request.Context()); err != nil {
			http.Error(response, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	})
	root.Handle("/", authLayer(routes))
	server, err := httptransport.New(cfg.Server, root)
	if err != nil {
		return err
	}
	logger.Info("starting role", "role", RoleServe, "address", cfg.Server.Address)
	return server.Run(ctx)
}

func postgresAPIKeyAuthenticator(db auth.APIKeyDB) (*auth.Authenticator, error) {
	store, err := auth.NewPostgresAPIKeyStore(db)
	if err != nil {
		return nil, err
	}
	verifier, err := auth.NewAPIKeyVerifier(store)
	if err != nil {
		return nil, err
	}
	return auth.NewAuthenticator(nil, verifier)
}

func mount(target *http.ServeMux, source http.Handler, patterns ...string) {
	for _, pattern := range patterns {
		target.Handle(pattern, source)
	}
}

func runRelay(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	pool, err := platformpostgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	redisClient, err := platformredis.Open(ctx, cfg.Redis)
	if err != nil {
		return err
	}
	defer redisClient.Close()
	keys, err := platformredis.NewKeyBuilder(string(cfg.Environment))
	if err != nil {
		return err
	}
	broker, err := sse.NewRedisBroker(redisClient, keys, cfg.Redis.MaxTTL, time.Second)
	if err != nil {
		return err
	}
	ssePublisher, err := sse.NewOutboxPublisher(broker)
	if err != nil {
		return err
	}
	queueAdapter, err := queue.NewPostgresAdapter(queueTransactions(pool), queue.Config{
		MaxPendingPerTenant: cfg.Queue.MaxPendingPerTenant, MaxPendingGlobal: cfg.Queue.MaxPendingGlobal,
	})
	if err != nil {
		return err
	}
	queuePublisher, err := queue.NewRunQueuedAdapter(queueAdapter)
	if err != nil {
		return err
	}
	router, err := outbox.NewRouter(map[string]outbox.Publisher{
		"RunQueued.v1": queuePublisher,
		"RunEvent.v1":  ssePublisher,
	})
	if err != nil {
		return err
	}
	repository, err := outbox.NewRepository(pool)
	if err != nil {
		return err
	}
	relay, err := outbox.NewRelay(repository, router, outbox.RelayConfig{
		BatchSize: 100, ClaimDuration: 30 * time.Second, MaxAttempts: 12,
		RetryBase: time.Second, RetryMaximum: 5 * time.Minute,
	})
	if err != nil {
		return err
	}
	component, err := outbox.NewComponent(repository, relay, outbox.ComponentConfig{
		Owner: cfg.Worker.ID + "-relay", PollInterval: cfg.Worker.PollInterval, TenantScanLimit: 100,
	}, logger)
	if err != nil {
		return err
	}
	if err := component.Start(ctx); err != nil {
		return err
	}
	logger.Info("starting role", "role", RoleRelay)
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	return component.Stop(stopCtx)
}

func queueTransactions(pool *platformpostgres.Pool) queue.WithinTxFunc {
	return func(ctx context.Context, options *sql.TxOptions, work func(context.Context, queue.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	}
}
