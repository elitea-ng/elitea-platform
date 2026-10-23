package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func legacyLookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestLegacyPgvectorConfigShapes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	dropper, retired, err := composeLegacyPgvector(logger, legacyLookup(nil))
	if err != nil || dropper != nil || retired {
		t.Fatalf("unset: dropper=%v retired=%v err=%v, want nothing", dropper, retired, err)
	}
	dropper, retired, err = composeLegacyPgvector(logger, legacyLookup(map[string]string{
		legacyPgvectorAdminURLEnv: " postgresql+psycopg://admin:pw@pgvector:5432/postgres ",
	}))
	if err != nil || dropper == nil || retired || dropper.AdminHost() != "pgvector:5432" {
		t.Fatalf("URL: dropper=%v retired=%v err=%v", dropper, retired, err)
	}
	dropper, retired, err = composeLegacyPgvector(logger, legacyLookup(map[string]string{legacyPgvectorRetiredEnv: "true"}))
	if err != nil || dropper != nil || !retired {
		t.Fatalf("retired: dropper=%v retired=%v err=%v", dropper, retired, err)
	}

	for name, env := range map[string]map[string]string{
		"both":            {legacyPgvectorAdminURLEnv: "postgres://admin:pw@pgvector/postgres", legacyPgvectorRetiredEnv: "true"},
		"not a boolean":   {legacyPgvectorRetiredEnv: "yes please"},
		"not a URL":       {legacyPgvectorAdminURLEnv: "host=pgvector password=s3cret"},
		"project db name": {legacyPgvectorAdminURLEnv: "postgres://admin:s3cret@pgvector/project_7"},
	} {
		if _, _, err := composeLegacyPgvector(logger, legacyLookup(env)); err == nil {
			t.Errorf("%s: no boot error", name)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: the boot error carries the password: %v", name, err)
		}
	}
}
