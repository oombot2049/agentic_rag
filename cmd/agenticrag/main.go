// Command agenticrag is the AgenticRAG service executable.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/agenticrag/agenticrag/internal/buildinfo"
)

const usage = `Usage: agenticrag <command>

Commands:
  version  print build version information
  help     print this help message
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}

	switch arguments[0] {
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
