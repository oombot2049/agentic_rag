// Command agenticrag is the AgenticRAG service executable.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/agenticrag/agenticrag/internal/buildinfo"
	processrole "github.com/agenticrag/agenticrag/internal/process"
)

const usage = `Usage: agenticrag <command>

Commands:
  serve          run the HTTP API and SSE gateway
  worker         run the Agent worker
  relay          run the transactional outbox relay
  ingest-worker  run the ingestion worker
  version  print build version information
  help     print this help message
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var runRole = func(role processrole.Role) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return processrole.Run(ctx, role, slog.Default())
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}

	switch arguments[0] {
	case "serve", "worker", "relay", "ingest-worker":
		if err := runRole(processrole.Role(arguments[0])); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s failed: %v\n", arguments[0], err)
			return 1
		}
		return 0
	case "version":
		_, _ = fmt.Fprintln(stdout, buildinfo.Current().String())
		return 0
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", arguments[0], usage)
		return 2
	}
}
