package main

import (
	"strings"
	"testing"
)

// The AUTH_DEV_MODE bypass is deleted (ADR-0017, #260). Deleting a feature
// leaves no failing test behind, so these pin the startup guard that keeps a
// stale manifest from being silently ignored: an operator who still sets
// AUTH_DEV_MODE=true believes authentication is disabled and may have deployed
// on that assumption. Refusing to start is the only safe answer.
//
// These exercise developmentFlagsFromEnv directly rather than run(). run() is
// not a pure predicate: past these checks it opens database pools, contacts
// the object store, and ends in serveApplication, which binds the public HTTP
// port and blocks on a context this test would never cancel. Calling it here
// made the outcome depend on whether unrelated startup happened to fail first
// — on a host with the dev stack up and :8080 free, the test hung until the
// Go test timeout. Worse, the bootstrap case would have reached
// infradb.RunMigrations against the default local DSN if the guard ever
// regressed, so the test guarding destructive behaviour could itself cause it.
//
// The middleware half — that the variable cannot inject a principal even if
// the process does start — lives in
// internal/api.TestProductionRuntimeRoutesRejectDevelopmentFallback.

// envMap turns a fixed map into the getenv function developmentFlagsFromEnv
// takes, so no test here mutates real process environment.
func envMap(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestDevelopmentFlagsRejectAuthDevModeTrue(t *testing.T) {
	_, err := developmentFlagsFromEnv(envMap(map[string]string{"AUTH_DEV_MODE": "true"}))
	if err == nil {
		t.Fatal("developmentFlagsFromEnv returned nil: the service would start with AUTH_DEV_MODE=true")
	}
	if !strings.Contains(err.Error(), "AUTH_DEV_MODE=true is no longer supported") {
		t.Fatalf("error = %q, want the ADR-0017 removal message", err)
	}
}

// A lingering "false" is inert and must stay tolerated — deployments that
// pinned it off (the retired deploy/centry-hybrid did) must not be broken by the removal.
// Any other value is likewise not the dangerous one and must not be rejected.
func TestDevelopmentFlagsTolerateNonTrueAuthDevMode(t *testing.T) {
	for _, value := range []string{"", "false", "1", "TRUE", "yes"} {
		t.Run("AUTH_DEV_MODE="+value, func(t *testing.T) {
			bootstrap, err := developmentFlagsFromEnv(envMap(map[string]string{"AUTH_DEV_MODE": value}))
			if err != nil {
				t.Fatalf("AUTH_DEV_MODE=%q was rejected: %v", value, err)
			}
			if bootstrap {
				t.Fatal("legacy bootstrap reported as requested without its own flag")
			}
		})
	}
}

// The legacy-schema bootstrap no longer cross-checks AUTH_DEV_MODE; it is
// self-gating. It must refuse on anything with a real authentication mode
// configured, and must report false rather than an error when it is simply
// not requested.
func TestDevelopmentFlagsRejectLegacyBootstrapWhenAuthenticationIsConfigured(t *testing.T) {
	for _, configured := range []string{"APPLICATION_SECRET_KEY", "OIDC_ISSUER_URL", "ELITEA_AUTH_CONFIG_FILE"} {
		t.Run(configured, func(t *testing.T) {
			bootstrap, err := developmentFlagsFromEnv(envMap(map[string]string{
				"ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA": "true",
				configured:                           "set-to-something",
			}))
			if err == nil || !strings.Contains(err.Error(), "ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA") {
				t.Fatalf("error = %v, want a refusal naming ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA", err)
			}
			// A refusal with no alternative is what sent an operator to psql
			// with 001_initial.sql against a production database (#556). The
			// message must name the binary that does this for a deployment.
			if !strings.Contains(err.Error(), "elitea-migrate") {
				t.Fatalf("the refusal does not name the supported path: %v", err)
			}
			if bootstrap {
				t.Fatal("bootstrap reported as requested despite the refusal; run() would migrate a real database")
			}
		})
	}
}

func TestDevelopmentFlagsAllowLegacyBootstrapOnADeveloperMachine(t *testing.T) {
	bootstrap, err := developmentFlagsFromEnv(envMap(map[string]string{
		"ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA": "true",
	}))
	if err != nil {
		t.Fatalf("unexpected error on a machine with no authentication configured: %v", err)
	}
	if !bootstrap {
		t.Fatal("bootstrap = false, want true: the local-development opt-in stopped working")
	}
}

func TestDevelopmentFlagsDefaultToNoBootstrap(t *testing.T) {
	bootstrap, err := developmentFlagsFromEnv(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error on an empty environment: %v", err)
	}
	if bootstrap {
		t.Fatal("bootstrap = true on an empty environment: the flag is not fail-closed")
	}
}
