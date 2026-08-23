// Package coretest enforces repository-level contracts for core unit suites.
package coretest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCoreUnitTestsDoNotUseWallClockSleepOrGlobalRandomness(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	directories := []string{
		"internal/domain", "internal/agent", "internal/contextmgr", "internal/idempotency", "internal/turn", "internal/knowledge",
	}
	for _, directory := range directories {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(directory)), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(content)
			for _, forbidden := range []string{"time.Now(", "time.Sleep(", `"math/rand/v2"`, "rand.Intn(", "rand.Int63(", "rand.Float64("} {
				if strings.Contains(text, forbidden) {
					t.Errorf("%s contains nondeterministic core-test primitive %q", filepath.ToSlash(path), forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
