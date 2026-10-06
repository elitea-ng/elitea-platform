package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestParseArgumentsRequiresOneOrMoreExplicitSpoolRoots(t *testing.T) {
	roots, ok := parseArguments([]string{
		"--spool-root", "/var/lib/elitea-worker-0/output-spool",
		"--spool-root=/var/lib/elitea-worker-1/output-spool",
	})
	if !ok || len(roots) != 2 ||
		roots[0] != "/var/lib/elitea-worker-0/output-spool" ||
		roots[1] != "/var/lib/elitea-worker-1/output-spool" {
		t.Fatalf("roots=%v ok=%v", roots, ok)
	}
	for _, arguments := range [][]string{
		nil,
		{"--spool-root"},
		{"--spool-root", "/tmp/spool", "unexpected"},
	} {
		if roots, ok := parseArguments(arguments); ok || roots != nil {
			t.Fatalf("invalid arguments=%v roots=%v ok=%v", arguments, roots, ok)
		}
	}
}

func TestRunRejectsInvalidUsageBeforeReadingEnvironment(t *testing.T) {
	var stderr strings.Builder
	calls := 0
	status := run(
		context.Background(),
		nil,
		func(string) (string, bool) {
			calls++
			return "", false
		},
		io.Discard,
		&stderr,
	)
	if status != exitInvalidUsage || calls != 0 ||
		!strings.Contains(stderr.String(), "usage: index-v2-preflight") {
		t.Fatalf("status=%d calls=%d stderr=%q", status, calls, stderr.String())
	}
}

func TestPreflightConfigRequiresOnlyTheIndexRouteAndTheRuntimeNATSIdentity(t *testing.T) {
	environment := map[string]string{
		"DATABASE_URL":           "postgres://preflight@postgres/elitea",
		"ELITEA_RUNTIME_ENABLED": "true",
		"ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED": "true",
		"ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM":   "ELITEA_RT_V1_INDEX",
		"ELITEA_RUNTIME_NATS_URL":                      "tls://elitea-nats:4222",
		"ELITEA_RUNTIME_NATS_TLS_CA_FILE":              "/etc/elitea/runtime-nats-client/ca.crt",
		"ELITEA_RUNTIME_NATS_TLS_CERT_FILE":            "/etc/elitea/runtime-nats-client/tls.crt",
		"ELITEA_RUNTIME_NATS_TLS_KEY_FILE":             "/etc/elitea/runtime-nats-client/tls.key",
		"ELITEA_RUNTIME_SIGNING_KEY_FILE":              "not-mounted",
		"ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE":     "not-mounted",
		"ELITEA_RUNTIME_CONTROL_TLS_KEY_FILE":          "not-mounted",
		"ELITEA_RUNTIME_OUTPUT_TLS_KEY_FILE":           "not-mounted",
		"ELITEA_RUNTIME_CONTENT_TLS_KEY_FILE":          "not-mounted",
	}
	config, err := preflightConfigFromEnv(func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.databaseURL != environment["DATABASE_URL"] ||
		config.commandStream != environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] ||
		config.nats.URL != environment["ELITEA_RUNTIME_NATS_URL"] ||
		config.nats.Material.CertFile != environment["ELITEA_RUNTIME_NATS_TLS_CERT_FILE"] {
		t.Fatalf("config=%+v", config)
	}

	for _, missing := range []string{
		"DATABASE_URL",
		"ELITEA_RUNTIME_ENABLED",
		"ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED",
		"ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM",
		"ELITEA_RUNTIME_NATS_URL",
		// A half-configured identity is refused, not silently plaintext.
		"ELITEA_RUNTIME_NATS_TLS_KEY_FILE",
	} {
		t.Run(missing, func(t *testing.T) {
			copy := make(map[string]string, len(environment))
			for name, value := range environment {
				copy[name] = value
			}
			delete(copy, missing)
			if _, err := preflightConfigFromEnv(func(name string) (string, bool) {
				value, ok := copy[name]
				return value, ok
			}); err == nil {
				t.Fatalf("missing %s was accepted", missing)
			}
		})
	}
}
