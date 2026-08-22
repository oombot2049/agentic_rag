package httptransport

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
)

func testServerConfig() config.ServerConfig {
	return config.ServerConfig{Address: "127.0.0.1:0", ReadTimeout: 500 * time.Millisecond,
		ReadHeaderTimeout: 200 * time.Millisecond, IdleTimeout: 500 * time.Millisecond, ShutdownTimeout: time.Second}
}

func TestChainOrderAndConstruction(t *testing.T) {
	t.Parallel()
	var order []string
	layer := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				order = append(order, name+":before")
				next.ServeHTTP(response, request)
				order = append(order, name+":after")
			})
		}
	}
	final := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") })
	chain, err := Chain(final, layer("a"), layer("b"))
	if err != nil {
		t.Fatal(err)
	}
	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.Join(order, ","); got != "a:before,b:before,handler,b:after,a:after" {
		t.Fatalf("order = %s", got)
	}
	if _, err := Chain(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil final error = %v", err)
	}
	if _, err := Chain(final, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil middleware error = %v", err)
	}
	bad := func(http.Handler) http.Handler { return nil }
	if _, err := Chain(final, bad); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil middleware result = %v", err)
	}
}

func TestServerConfigurationStartAndIdempotentStop(t *testing.T) {
	t.Parallel()
	configuration := testServerConfig()
	server, err := New(configuration, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if server.server.ReadTimeout != configuration.ReadTimeout || server.server.ReadHeaderTimeout != configuration.ReadHeaderTimeout ||
		server.server.IdleTimeout != configuration.IdleTimeout || server.server.WriteTimeout != 0 || server.server.MaxHeaderBytes != maxHeaderBytes {
		t.Fatalf("http.Server configuration = %+v", server.server)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v", err)
	}
	address, err := server.Address()
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + address.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("idempotent Stop = %v", err)
	}
	if err := server.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("restart error = %v", err)
	}
}

func TestGracefulShutdownWaitsForAdmittedRequest(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	server, _ := New(testServerConfig(), http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(response, "complete")
	}))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	address, _ := server.Address()
	responseDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + address.String())
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || string(body) != "complete" {
				err = errors.Join(readErr, errors.New("incomplete response"))
			}
		}
		responseDone <- err
	}()
	<-entered
	stopDone := make(chan error, 1)
	go func() { stopDone <- server.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before handler release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
}

func TestRunCancellationModelsSignalShutdown(t *testing.T) {
	t.Parallel()
	server, _ := New(testServerConfig(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := server.Address(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run leaked after cancellation")
	}
}

func TestDrainGateRejectsNewWork(t *testing.T) {
	t.Parallel()
	called := false
	server, _ := New(testServerConfig(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	server.draining.Store(true)
	recorder := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Connection") != "close" || called {
		t.Fatalf("drain response=%d connection=%q called=%v", recorder.Code, recorder.Header().Get("Connection"), called)
	}
}

func TestReadHeaderAndIdleTimeoutsCloseConnections(t *testing.T) {
	t.Parallel()
	configuration := testServerConfig()
	configuration.ReadHeaderTimeout = 100 * time.Millisecond
	configuration.IdleTimeout = 200 * time.Millisecond
	server, _ := New(configuration, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(response, "ok") }))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	address, _ := server.Address()

	slow, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(slow, "GET / HTTP/1.1\r\nHost:")
	_ = slow.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadAll(slow)
	_ = slow.Close()
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("slow header connection was not closed")
	}

	idle, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(idle)
	request, _ := http.NewRequest(http.MethodGet, "http://"+address.String()+"/", nil)
	if err := request.Write(idle); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	time.Sleep(350 * time.Millisecond)
	_ = idle.SetDeadline(time.Now().Add(time.Second))
	_ = request.Write(idle)
	if _, err := http.ReadResponse(reader, request); err == nil {
		t.Fatal("idle connection remained reusable")
	}
	_ = idle.Close()
}

func TestReadTimeoutInterruptsSlowBody(t *testing.T) {
	t.Parallel()
	configuration := testServerConfig()
	configuration.ReadTimeout = 150 * time.Millisecond
	server, _ := New(configuration, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, err := io.ReadAll(request.Body)
		if err != nil {
			response.WriteHeader(http.StatusRequestTimeout)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	address, _ := server.Address()
	connection, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = io.WriteString(connection, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 5\r\n\r\nx")
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	request, _ := http.NewRequest(http.MethodPost, "http://test/", nil)
	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("slow body status = %d", response.StatusCode)
	}
}

func TestConcurrentStopSharesCompletion(t *testing.T) {
	t.Parallel()
	server, _ := New(testServerConfig(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make(chan error, 10)
	for index := 0; index < 10; index++ {
		wait.Add(1)
		go func() { defer wait.Done(); results <- server.Stop(context.Background()) }()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLifecycleValidationAndUnexpectedServeError(t *testing.T) {
	t.Parallel()
	if _, err := New(config.ServerConfig{}, http.NotFoundHandler()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid config error = %v", err)
	}
	server, _ := New(testServerConfig(), http.NotFoundHandler())
	if _, err := server.Address(); err == nil {
		t.Fatal("Address before Start succeeded")
	}
	var nilContext context.Context
	if err := server.Start(nilContext); err == nil {
		t.Fatal("nil Start context succeeded")
	}
	if err := server.Stop(nilContext); err == nil {
		t.Fatal("nil Stop context succeeded")
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	listener := server.listener
	server.mu.Unlock()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server.Errors():
		if err == nil || !strings.Contains(err.Error(), "serve HTTP") {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected serve error was not reported")
	}
	if err := server.Stop(context.Background()); err == nil {
		t.Fatal("Stop hid unexpected serve error")
	}
	stopped, _ := New(testServerConfig(), http.NotFoundHandler())
	if err := stopped.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stopped.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start after pre-start Stop = %v", err)
	}
}

func TestShutdownDeadlineForcesClose(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	server, _ := New(testServerConfig(), http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(entered)
		<-request.Context().Done()
	}))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	address, _ := server.Address()
	go func() {
		response, err := http.Get("http://" + address.String())
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := server.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced Stop error = %v", err)
	}
}
