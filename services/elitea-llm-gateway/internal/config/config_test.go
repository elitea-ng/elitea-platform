package config

import (
	"testing"
	"time"
)

func TestFromEnvDefaults(t *testing.T) {
	// Ensure a clean environment for the keys we read.
	for _, k := range []string{
		"GATEWAY_HTTP_ADDR", "DATABASE_URL", "GATEWAY_SHUTDOWN_TIMEOUT",
		"GATEWAY_INITIAL_POOL_SIZE", "GATEWAY_PROVIDER_CONCURRENCY",
		"GATEWAY_LOG_LEVEL", "OTEL_SERVICE_NAME", "SERVICE_VERSION",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "GATEWAY_NATS_URL",
		"GATEWAY_NATS_TLS_CA_FILE", "GATEWAY_NATS_TLS_CERT_FILE", "GATEWAY_NATS_TLS_KEY_FILE",
		"LLM_BUDGET_EXPECTED_REPLICAS", "LLM_BUDGET_CB_FAILURE_THRESHOLD",
		"LLM_BUDGET_CB_OPEN_DURATION_SEC",
		"LLM_STREAM_GRACE_MS", "LLM_STREAM_DRAIN_MAX_INFLIGHT",
	} {
		t.Setenv(k, "")
	}

	cfg := FromEnv()

	if cfg.HTTPAddr != ":8083" {
		t.Errorf("HTTPAddr = %q, want :8083", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
	// §9.5: the shutdown drain window MUST be at least 150s.
	if cfg.ShutdownTimeout < 150*time.Second {
		t.Errorf("ShutdownTimeout = %v, must be >= 150s (§9.5)", cfg.ShutdownTimeout)
	}
	if cfg.InitialPoolSize != DefaultInitialPoolSize {
		t.Errorf("InitialPoolSize = %d, want %d", cfg.InitialPoolSize, DefaultInitialPoolSize)
	}
	if cfg.ProviderConcurrency != DefaultProviderConcurrency {
		t.Errorf("ProviderConcurrency = %d, want %d", cfg.ProviderConcurrency, DefaultProviderConcurrency)
	}
	// §9.5: concurrency MUST be well below bifrost's 1000-worker default.
	if cfg.ProviderConcurrency >= 1000 {
		t.Errorf("ProviderConcurrency = %d, must be tuned below 1000 (§9.5)", cfg.ProviderConcurrency)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.ServiceName != "elitea-llm-gateway" {
		t.Errorf("ServiceName = %q, want elitea-llm-gateway", cfg.ServiceName)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", cfg.OTLPEndpoint)
	}
	// NATS wiring defaults: URL empty (NATS disabled), scale-1 replica count,
	// and the §8.5 breaker defaults.
	if cfg.NATSURL != "" {
		t.Errorf("NATSURL = %q, want empty (NATS disabled by default)", cfg.NATSURL)
	}
	if cfg.ExpectedReplicas != DefaultNATSReplicas {
		t.Errorf("ExpectedReplicas = %d, want %d", cfg.ExpectedReplicas, DefaultNATSReplicas)
	}
	if cfg.NATSTLSCAFile != "" || cfg.NATSTLSCertFile != "" || cfg.NATSTLSKeyFile != "" {
		t.Errorf("NATS TLS material = %q/%q/%q, want empty by default", cfg.NATSTLSCAFile, cfg.NATSTLSCertFile, cfg.NATSTLSKeyFile)
	}
	if cfg.CBFailureThreshold != DefaultCBFailureThreshold {
		t.Errorf("CBFailureThreshold = %d, want %d", cfg.CBFailureThreshold, DefaultCBFailureThreshold)
	}
	if cfg.CBOpenDuration != DefaultCBOpenDuration {
		t.Errorf("CBOpenDuration = %v, want %v", cfg.CBOpenDuration, DefaultCBOpenDuration)
	}
}

// TestStreamGraceFromEnv covers the issue-#9 knobs. LLM_STREAM_GRACE_MS is the
// one env var in this package whose "0" is MEANINGFUL — it disables
// disconnect-billing rather than meaning "unset" — so it needs its own coverage:
// a refactor that routed it through the ordinary intOr helper would silently
// re-enable the mechanism (or, with the old overflow bug, silently disable it).
func TestStreamGraceFromEnv(t *testing.T) {
	cases := []struct {
		name     string
		grace    string
		limit    string
		want     time.Duration
		wantSlot int
	}{
		{"unset", "", "", DefaultStreamGrace, DefaultStreamDrainLimit},
		{"explicit zero disables", "0", "", 0, DefaultStreamDrainLimit},
		{"valid override", "1200", "64", 1200 * time.Millisecond, 64},
		{"clamped to max", "60000", "", MaxStreamGrace, DefaultStreamDrainLimit},
		{"exactly max", "15000", "", MaxStreamGrace, DefaultStreamDrainLimit},
		// Overflow: n*time.Millisecond wraps NEGATIVE, which must not slip past
		// the clamp and disable the mechanism. 9223372036854 ms is still under
		// MaxInt64 nanoseconds and does NOT overflow, so it proved nothing
		// about the clamp; these two do (MaxInt64/1e6 ≈ 9223372036854.77).
		{"overflow clamps to max", "9223372036855", "", MaxStreamGrace, DefaultStreamDrainLimit},
		{"max int overflow clamps to max", "9223372036854775807", "", MaxStreamGrace, DefaultStreamDrainLimit},
		{"negative falls back", "-1", "", DefaultStreamGrace, DefaultStreamDrainLimit},
		{"garbage falls back", "abc", "", DefaultStreamGrace, DefaultStreamDrainLimit},
		{"duration string falls back", "5s", "", DefaultStreamGrace, DefaultStreamDrainLimit},
		{"invalid limit falls back", "", "0", DefaultStreamGrace, DefaultStreamDrainLimit},
		{"negative limit falls back", "", "-4", DefaultStreamGrace, DefaultStreamDrainLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LLM_STREAM_GRACE_MS", tc.grace)
			t.Setenv("LLM_STREAM_DRAIN_MAX_INFLIGHT", tc.limit)
			cfg := FromEnv()
			if cfg.StreamGrace != tc.want {
				t.Errorf("StreamGrace = %v, want %v", cfg.StreamGrace, tc.want)
			}
			if cfg.StreamDrainLimit != tc.wantSlot {
				t.Errorf("StreamDrainLimit = %d, want %d", cfg.StreamDrainLimit, tc.wantSlot)
			}
		})
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("GATEWAY_HTTP_ADDR", ":9999")
	t.Setenv("DATABASE_URL", "postgres://db/x")
	t.Setenv("GATEWAY_SHUTDOWN_TIMEOUT", "200s")
	t.Setenv("GATEWAY_INITIAL_POOL_SIZE", "42")
	t.Setenv("GATEWAY_PROVIDER_CONCURRENCY", "7")
	t.Setenv("GATEWAY_LOG_LEVEL", "debug")
	t.Setenv("OTEL_SERVICE_NAME", "gw-test")
	t.Setenv("SERVICE_VERSION", "1.2.3")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("GATEWAY_NATS_URL", "nats://nats:4222")
	t.Setenv("GATEWAY_NATS_TLS_CA_FILE", "/etc/nats-client/ca.crt")
	t.Setenv("GATEWAY_NATS_TLS_CERT_FILE", "/etc/nats-client/tls.crt")
	t.Setenv("GATEWAY_NATS_TLS_KEY_FILE", "/etc/nats-client/tls.key")
	t.Setenv("LLM_BUDGET_EXPECTED_REPLICAS", "3")
	t.Setenv("LLM_BUDGET_CB_FAILURE_THRESHOLD", "5")
	t.Setenv("LLM_BUDGET_CB_OPEN_DURATION_SEC", "20")

	cfg := FromEnv()

	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.DatabaseURL != "postgres://db/x" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.ShutdownTimeout != 200*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.InitialPoolSize != 42 {
		t.Errorf("InitialPoolSize = %d", cfg.InitialPoolSize)
	}
	if cfg.ProviderConcurrency != 7 {
		t.Errorf("ProviderConcurrency = %d", cfg.ProviderConcurrency)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.ServiceName != "gw-test" {
		t.Errorf("ServiceName = %q", cfg.ServiceName)
	}
	if cfg.ServiceVersion != "1.2.3" {
		t.Errorf("ServiceVersion = %q", cfg.ServiceVersion)
	}
	if cfg.OTLPEndpoint != "http://collector:4317" {
		t.Errorf("OTLPEndpoint = %q", cfg.OTLPEndpoint)
	}
	if cfg.NATSURL != "nats://nats:4222" {
		t.Errorf("NATSURL = %q", cfg.NATSURL)
	}
	if cfg.ExpectedReplicas != 3 {
		t.Errorf("ExpectedReplicas = %d, want 3", cfg.ExpectedReplicas)
	}
	if cfg.NATSTLSCAFile != "/etc/nats-client/ca.crt" || cfg.NATSTLSCertFile != "/etc/nats-client/tls.crt" || cfg.NATSTLSKeyFile != "/etc/nats-client/tls.key" {
		t.Errorf("NATS TLS material = %q/%q/%q", cfg.NATSTLSCAFile, cfg.NATSTLSCertFile, cfg.NATSTLSKeyFile)
	}
	if cfg.CBFailureThreshold != 5 {
		t.Errorf("CBFailureThreshold = %d, want 5", cfg.CBFailureThreshold)
	}
	if cfg.CBOpenDuration != 20*time.Second {
		t.Errorf("CBOpenDuration = %v, want 20s", cfg.CBOpenDuration)
	}
}

func TestFromEnvInvalidValuesFallBackToDefaults(t *testing.T) {
	t.Setenv("GATEWAY_SHUTDOWN_TIMEOUT", "not-a-duration")
	t.Setenv("GATEWAY_INITIAL_POOL_SIZE", "-5")
	t.Setenv("GATEWAY_PROVIDER_CONCURRENCY", "abc")
	t.Setenv("LLM_BUDGET_EXPECTED_REPLICAS", "0")
	t.Setenv("LLM_BUDGET_CB_FAILURE_THRESHOLD", "-1")
	t.Setenv("LLM_BUDGET_CB_OPEN_DURATION_SEC", "notint")

	cfg := FromEnv()

	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want default on invalid input", cfg.ShutdownTimeout)
	}
	if cfg.InitialPoolSize != DefaultInitialPoolSize {
		t.Errorf("InitialPoolSize = %d, want default on non-positive input", cfg.InitialPoolSize)
	}
	if cfg.ProviderConcurrency != DefaultProviderConcurrency {
		t.Errorf("ProviderConcurrency = %d, want default on invalid input", cfg.ProviderConcurrency)
	}
	// Non-positive / non-numeric NATS knobs fall back to the §8.5 defaults.
	if cfg.ExpectedReplicas != DefaultNATSReplicas {
		t.Errorf("ExpectedReplicas = %d, want default on non-positive input", cfg.ExpectedReplicas)
	}
	if cfg.CBFailureThreshold != DefaultCBFailureThreshold {
		t.Errorf("CBFailureThreshold = %d, want default on invalid input", cfg.CBFailureThreshold)
	}
	if cfg.CBOpenDuration != DefaultCBOpenDuration {
		t.Errorf("CBOpenDuration = %v, want default on invalid input", cfg.CBOpenDuration)
	}
}

// TestSelfLLMOriginsParsing covers the GATEWAY_SELF_LLM_ORIGINS CSV parse
// (BFF.6): trim, drop empties, nil when unset.
func TestSelfLLMOriginsParsing(t *testing.T) {
	t.Setenv("GATEWAY_SELF_LLM_ORIGINS", " https://dev.elitea.ai/llm/v1 ,, http://elitea-main:8080/llm/v1 ")
	cfg := FromEnv()
	if len(cfg.SelfLLMOrigins) != 2 ||
		cfg.SelfLLMOrigins[0] != "https://dev.elitea.ai/llm/v1" ||
		cfg.SelfLLMOrigins[1] != "http://elitea-main:8080/llm/v1" {
		t.Fatalf("SelfLLMOrigins = %#v, want two trimmed origins", cfg.SelfLLMOrigins)
	}

	t.Setenv("GATEWAY_SELF_LLM_ORIGINS", "")
	if got := FromEnv().SelfLLMOrigins; got != nil {
		t.Fatalf("SelfLLMOrigins = %#v with unset env, want nil", got)
	}
}

// TestPublicProjectID covers the shared-project scope switch (issue #316).
// Unset must stay unset: the gateway then reads the caller's project only, and
// no deployment changes behaviour when it upgrades.
func TestPublicProjectID(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"unset disables the shared scope", "", ""},
		{"a positive id arms it", "1", "1"},
		{"any positive id is accepted", "42", "42"},
		{"zero is treated as unset", "0", ""},
		{"a negative id is rejected", "-1", ""},
		{"a non-numeric id is rejected", "abc", ""},
		{"an injection attempt is rejected", "1; DROP TABLE", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ELITEA_AI_PROJECT_ID", tc.env)
			if got := FromEnv().PublicProjectIDString(); got != tc.want {
				t.Errorf("PublicProjectIDString() = %q, want %q", got, tc.want)
			}
		})
	}
}
