// Package httptransport owns the production HTTP server lifecycle.
package httptransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
)

const maxHeaderBytes = 1 << 20

var (
	ErrAlreadyStarted = errors.New("HTTP server already started")
	ErrStopped        = errors.New("HTTP server is stopped")
	ErrInvalidConfig  = errors.New("HTTP server configuration is invalid")
)

type Middleware func(http.Handler) http.Handler

func Chain(final http.Handler, middleware ...Middleware) (http.Handler, error) {
	if final == nil {
		return nil, fmt.Errorf("%w: final handler is nil", ErrInvalidConfig)
	}
	for index, layer := range middleware {
		if layer == nil {
			return nil, fmt.Errorf("%w: middleware %d is nil", ErrInvalidConfig, index)
		}
	}
	result := final
	for index := len(middleware) - 1; index >= 0; index-- {
		result = middleware[index](result)
		if result == nil {
			return nil, fmt.Errorf("%w: middleware %d returned nil", ErrInvalidConfig, index)
		}
	}
	return result, nil
}

type serverState uint8

const (
	stateReady serverState = iota
	stateRunning
	stateStopping
	stateStopped
)

type Server struct {
	mu            sync.Mutex
	state         serverState
	configuration config.ServerConfig
	server        *http.Server
	listener      net.Listener
	serveDone     chan error
	stopDone      chan struct{}
	stopErr       error
	errors        chan error
	cancelBase    context.CancelFunc
	draining      atomic.Bool
}

func New(configuration config.ServerConfig, final http.Handler, middleware ...Middleware) (*Server, error) {
	if err := validateConfig(configuration); err != nil {
		return nil, err
	}
	chain, err := Chain(final, middleware...)
	if err != nil {
		return nil, err
	}
	component := &Server{configuration: configuration, state: stateReady, errors: make(chan error, 1)}
	component.server = &http.Server{
		Addr:              configuration.Address,
		Handler:           component.drainGate(chain),
		ReadTimeout:       configuration.ReadTimeout,
		ReadHeaderTimeout: configuration.ReadHeaderTimeout,
		WriteTimeout:      0,
		IdleTimeout:       configuration.IdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return component, nil
}

func (s *Server) Name() string { return "http-server" }

func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("HTTP server start context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case stateRunning, stateStopping:
		return ErrAlreadyStarted
	case stateStopped:
		return ErrStopped
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.configuration.Address)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	baseContext, cancel := context.WithCancel(context.Background())
	s.server.BaseContext = func(net.Listener) context.Context { return baseContext }
	s.listener = listener
	s.cancelBase = cancel
	s.serveDone = make(chan error, 1)
	s.stopDone = make(chan struct{})
	s.state = stateRunning
	go s.serve(listener)
	return nil
}

func (s *Server) serve(listener net.Listener) {
	err := s.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		select {
		case s.errors <- fmt.Errorf("serve HTTP: %w", err):
		default:
		}
	}
	s.serveDone <- err
	close(s.serveDone)
}

func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("HTTP server stop context is nil")
	}
	s.mu.Lock()
	switch s.state {
	case stateReady:
		s.state = stateStopped
		s.draining.Store(true)
		s.mu.Unlock()
		return nil
	case stateStopped:
		err := s.stopErr
		s.mu.Unlock()
		return err
	case stateStopping:
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.stopErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.state = stateStopping
	s.draining.Store(true)
	s.server.SetKeepAlivesEnabled(false)
	done := s.serveDone
	cancelBase := s.cancelBase
	stopDone := s.stopDone
	s.mu.Unlock()

	shutdownErr := s.server.Shutdown(ctx)
	var closeErr error
	if shutdownErr != nil {
		closeErr = s.server.Close()
	}
	serveErr := <-done
	cancelBase()
	result := errors.Join(shutdownErr, closeErr, serveErr)

	s.mu.Lock()
	s.stopErr = result
	s.state = stateStopped
	close(stopDone)
	s.mu.Unlock()
	return result
}

func (s *Server) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("HTTP server run context is nil")
	}
	if err := s.Start(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), s.configuration.ShutdownTimeout)
		defer cancel()
		return s.Stop(shutdownContext)
	case err := <-s.errors:
		shutdownContext, cancel := context.WithTimeout(context.Background(), s.configuration.ShutdownTimeout)
		defer cancel()
		return errors.Join(err, s.Stop(shutdownContext))
	}
}

func (s *Server) Errors() <-chan error { return s.errors }

func (s *Server) Address() (net.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil, errors.New("HTTP listener is not started")
	}
	return s.listener.Addr(), nil
}

func (s *Server) drainGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if s.draining.Load() {
			response.Header().Set("Connection", "close")
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func validateConfig(configuration config.ServerConfig) error {
	if _, _, err := net.SplitHostPort(configuration.Address); err != nil {
		return fmt.Errorf("%w: address", ErrInvalidConfig)
	}
	if configuration.ReadTimeout < 100*time.Millisecond || configuration.ReadTimeout > 5*time.Minute ||
		configuration.ReadHeaderTimeout < 100*time.Millisecond || configuration.ReadHeaderTimeout > 30*time.Second ||
		configuration.IdleTimeout < configuration.ReadHeaderTimeout || configuration.IdleTimeout > 5*time.Minute ||
		configuration.ShutdownTimeout < time.Second || configuration.ShutdownTimeout > 2*time.Minute {
		return fmt.Errorf("%w: timeout boundary", ErrInvalidConfig)
	}
	return nil
}
