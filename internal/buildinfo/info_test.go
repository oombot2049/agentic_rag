package buildinfo

import "testing"

func TestCurrentNormalizesBuildValues(t *testing.T) {
	originalVersion, originalCommit, originalBuildDate := Version, Commit, BuildDate
	t.Cleanup(func() {
		Version, Commit, BuildDate = originalVersion, originalCommit, originalBuildDate
	})

	Version = "  "
	Commit = " abc123 "
	BuildDate = ""

	got := Current()
	want := Info{Version: "dev", Commit: "abc123", BuildDate: "unknown"}
	if got != want {
		t.Fatalf("Current() = %+v, want %+v", got, want)
	}
}

func TestInfoString(t *testing.T) {
	info := Info{Version: "v1.2.3", Commit: "abc123", BuildDate: "2026-08-23T00:00:00Z"}
	want := "agenticrag v1.2.3 (commit abc123, built 2026-08-23T00:00:00Z)"
	if got := info.String(); got != want {
		t.Fatalf("Info.String() = %q, want %q", got, want)
	}
}
