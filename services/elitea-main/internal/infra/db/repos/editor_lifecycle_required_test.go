package repos

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Exercise the real fixture entrypoint without starting a database.
func TestEditorLifecycleFixtureRequiredMode(t *testing.T) {
	for _, mode := range []string{"true", "1"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEditorLifecyclePostgres$", "-test.v")
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				switch key {
				case editorLifecycleFixtureURL, editorLifecycleFixtureRequired, "ELITEA_TEST_DATABASE_URL", "ELITEA_TEST_USE_SERVICE_DATABASE_URL", "DATABASE_URL", "GOMAXPROCS":
					continue
				}
				command.Env = append(command.Env, entry)
			}
			command.Env = append(command.Env, editorLifecycleFixtureRequired+"="+mode, "GOMAXPROCS=2")
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatal("required fixture omission did not fail the real test entrypoint")
			}
			if !strings.Contains(string(output), "ELITEA_EDITOR_TEST_DATABASE_URL is required for editor PostgreSQL acceptance") || strings.Contains(string(output), "--- SKIP:") {
				t.Fatal("required fixture omission did not fail before database access")
			}
		})
	}
}
