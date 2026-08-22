// Package buildinfo exposes deterministic build metadata for the executable.
package buildinfo

import (
	"fmt"
	"strings"
)

// These values may be replaced with -ldflags -X during a release build.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info is the normalized metadata embedded in one executable build.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
}

// Current returns normalized process build metadata.
func Current() Info {
	return Info{
		Version:   valueOr(Version, "dev"),
		Commit:    valueOr(Commit, "unknown"),
		BuildDate: valueOr(BuildDate, "unknown"),
	}
}

// String returns the stable one-line representation used by the CLI.
func (info Info) String() string {
	return fmt.Sprintf("agenticrag %s (commit %s, built %s)", info.Version, info.Commit, info.BuildDate)
}

func valueOr(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
