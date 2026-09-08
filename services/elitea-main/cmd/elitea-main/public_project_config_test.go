package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

func TestResolvePublicProject(t *testing.T) {
	t.Parallel()

	t.Run("nothing set keeps the historical default", func(t *testing.T) {
		t.Parallel()
		id, err := resolvePublicProject(slog.Default(), envLookup(map[string]string{}))
		if err != nil {
			t.Fatalf("resolvePublicProject: %v", err)
		}
		if id != publicproject.Default {
			t.Errorf("id = %d, want %d", id, publicproject.Default)
		}
	})

	t.Run("the canonical variable is accepted without a warning", func(t *testing.T) {
		t.Parallel()
		var log bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&log, nil))
		id, err := resolvePublicProject(logger, envLookup(map[string]string{publicproject.Canonical: "12"}))
		if err != nil {
			t.Fatalf("resolvePublicProject: %v", err)
		}
		if id != 12 {
			t.Errorf("id = %d, want 12", id)
		}
		if strings.Contains(log.String(), "deprecated") {
			t.Errorf("the canonical name must not be reported as deprecated: %s", log.String())
		}
	})

	t.Run("a deprecated alias works and is named in the log", func(t *testing.T) {
		t.Parallel()
		var log bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&log, nil))
		id, err := resolvePublicProject(logger, envLookup(map[string]string{"PUBLIC_PROJECT_ID": "8"}))
		if err != nil {
			t.Fatalf("resolvePublicProject: %v", err)
		}
		if id != 8 {
			t.Errorf("id = %d, want 8", id)
		}
		if !strings.Contains(log.String(), "PUBLIC_PROJECT_ID") ||
			!strings.Contains(log.String(), publicproject.Canonical) {
			t.Errorf("the warning must name both variables: %s", log.String())
		}
	})

	t.Run("two names that disagree stop the process", func(t *testing.T) {
		t.Parallel()
		_, err := resolvePublicProject(slog.Default(), envLookup(map[string]string{
			publicproject.Canonical: "1",
			"SHARED_PROJECT_ID":     "4",
		}))
		if err == nil {
			t.Fatal("expected a refusal, got none")
		}
		if !strings.Contains(err.Error(), "public project") {
			t.Errorf("the refusal must say what it is about: %v", err)
		}
	})

	t.Run("two names that agree are accepted", func(t *testing.T) {
		t.Parallel()
		id, err := resolvePublicProject(slog.Default(), envLookup(map[string]string{
			publicproject.Canonical: "4",
			"SHARED_PROJECT_ID":     "4",
		}))
		if err != nil {
			t.Fatalf("resolvePublicProject: %v", err)
		}
		if id != 4 {
			t.Errorf("id = %d, want 4", id)
		}
	})
}
