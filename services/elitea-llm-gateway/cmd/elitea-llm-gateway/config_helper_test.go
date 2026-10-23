package main

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/config"
)

// mustConfigFromEnv is config.FromEnv for tests whose environment is valid.
func mustConfigFromEnv(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	return cfg
}
