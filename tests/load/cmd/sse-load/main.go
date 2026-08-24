// Command sse-load measures long-lived SSE connections or paced reconnects.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type runPool struct {
	RunIDs []string `json:"run_ids"`
}
type summary struct {
	Mode        string  `json:"mode"`
	Stream      string  `json:"stream"`
	Attempted   int64   `json:"attempted"`
	Connected   int64   `json:"connected"`
	Errors      int64   `json:"errors"`
	ResumeP50MS float64 `json:"resume_p50_ms"`
	ResumeP95MS float64 `json:"resume_p95_ms"`
	ResumeP99MS float64 `json:"resume_p99_ms"`
	TTFTP50MS   float64 `json:"ttft_p50_ms"`
	TTFTP95MS   float64 `json:"ttft_p95_ms"`
	TTFTP99MS   float64 `json:"ttft_p99_ms"`
}
type collector struct {
	attempted atomic.Int64
	connected atomic.Int64
	failures  atomic.Int64
	mu        sync.Mutex
	resume    []time.Duration
	ttft      []time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sse-load:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("sse-load", flag.ContinueOnError)
	baseURL := fs.String("base-url", "", "service base URL")
	poolPath := fs.String("run-pool", "", "JSON run pool")
	mode := fs.String("mode", "connections", "connections or reconnect")
	stream := fs.String("stream", "lifecycle", "lifecycle or output")
	connections := fs.Int("connections", 1, "parallel long-lived connections")
	rate := fs.Float64("reconnect-rate", 1, "reconnects per second")
	duration := fs.Duration("duration", 10*time.Second, "test duration")
	output := fs.String("output", "", "summary file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *baseURL == "" || *poolPath == "" || *output == "" {
		return errors.New("base-url, run-pool, and output are required")
	}
	if (*mode != "connections" && *mode != "reconnect") || (*stream != "lifecycle" && *stream != "output") || *connections < 1 || *connections > 100_000 || *rate <= 0 || *rate > 100_000 || *duration <= 0 {
		return errors.New("invalid mode or limits")
	}
	token := os.Getenv("AGENTICRAG_LOAD_TOKEN")
	if token == "" {
		return errors.New("AGENTICRAG_LOAD_TOKEN is required")
	}
	data, err := os.ReadFile(*poolPath)
	if err != nil {
		return err
	}
	var pool runPool
	if err := json.Unmarshal(data, &pool); err != nil || len(pool.RunIDs) == 0 {
		return errors.New("run pool must contain run_ids")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: *connections + 100, MaxConnsPerHost: *connections + 100}
	client := &http.Client{Transport: transport}
	var c collector
	var wg sync.WaitGroup
	suffix := "/events"
	if *stream == "output" {
		suffix = "/output"
	}
	launch := func(index int, firstOnly bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.consume(ctx, client, strings.TrimRight(*baseURL, "/")+"/v1/runs/"+pool.RunIDs[index%len(pool.RunIDs)]+suffix, token, firstOnly)
		}()
	}
	if *mode == "connections" {
		for i := 0; i < *connections; i++ {
			launch(i, false)
		}
	} else {
		ticker := time.NewTicker(time.Duration(float64(time.Second) / *rate))
		defer ticker.Stop()
		i := 0
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-ticker.C:
				launch(i, true)
				i++
			}
		}
	}
	<-ctx.Done()
	wg.Wait()
	transport.CloseIdleConnections()
	result := c.result(*mode)
	result.Stream = *stream
	encoded, _ := json.MarshalIndent(result, "", "  ")
	encoded = append(encoded, '\n')
	return os.WriteFile(*output, encoded, 0o600)
}

func (c *collector) consume(ctx context.Context, client *http.Client, endpoint, token string, firstOnly bool) {
	c.attempted.Add(1)
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		c.failures.Add(1)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			c.failures.Add(1)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		c.failures.Add(1)
		return
	}
	c.connected.Add(1)
	resume := time.Since(started)
	c.addResume(resume)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "event: assistant.delta" {
			c.addTTFT(time.Since(started))
			if firstOnly {
				return
			}
		} else if firstOnly && strings.HasPrefix(scanner.Text(), "id:") {
			return
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		c.failures.Add(1)
	}
}
func (c *collector) addResume(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resume = append(c.resume, d)
}
func (c *collector) addTTFT(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttft = append(c.ttft, d)
}
func (c *collector) result(mode string) summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	return summary{Mode: mode, Attempted: c.attempted.Load(), Connected: c.connected.Load(), Errors: c.failures.Load(), ResumeP50MS: percentile(c.resume, .50), ResumeP95MS: percentile(c.resume, .95), ResumeP99MS: percentile(c.resume, .99), TTFTP50MS: percentile(c.ttft, .50), TTFTP95MS: percentile(c.ttft, .95), TTFTP99MS: percentile(c.ttft, .99)}
}
func percentile(values []time.Duration, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]time.Duration(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	index := int(float64(len(copyValues)-1)*q + .999999)
	if index >= len(copyValues) {
		index = len(copyValues) - 1
	}
	return float64(copyValues[index]) / float64(time.Millisecond)
}
