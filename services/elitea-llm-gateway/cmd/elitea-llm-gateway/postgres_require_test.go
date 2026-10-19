package main

import (
	"os"
	"testing"
)

// requirePostgresEnv turns a skipped Postgres integration test into a FAILURE.
// CI - Gateway runs a Postgres service and sets it, so losing the service, the
// URL or a migration file fails the job instead of reading as green (the #423
// pattern: a skipped test prints `ok` and covers nothing).
const requirePostgresEnv = "ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST"

func skipPostgresTest(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requirePostgresEnv) != "" {
		t.Fatalf(requirePostgresEnv+" is set, so this Postgres test may not skip: "+format, args...)
	}
	t.Skipf(format, args...)
}
