// Package config loads elitea-llm-gateway settings from the environment.
//
// The defaults encode the pre-cutover deployment guidance from
// design-bifrost-gateway §9.5: a long shutdown drain window (≥150s) so
// rolling deploys do not truncate in-flight LLM streams, a disabled write
// deadline on the /llm SSE path, a tuned-down bifrost object pool, and a
// per-provider worker concurrency well below bifrost's 1000-worker default.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultShutdownTimeout is the ceiling on srv.Shutdown()'s context.
	// It MUST be ≥150s and rise together with the Deployment's
	// terminationGracePeriodSeconds so provider streams (up to ~120s) drain
	// instead of being hard-killed on every rolling deploy (§9.5).
	DefaultShutdownTimeout = 150 * time.Second

	// DefaultInitialPoolSize tunes BifrostConfig.InitialPoolSize down from
	// bifrost's memory-hungry default so the process fits the ≥1Gi memory
	// limit (§9.5, §6.1).
	DefaultInitialPoolSize = 100

	// DefaultProviderConcurrency tunes each provider's
	// ProviderConfig.ConcurrencyAndBufferSize.Concurrency down from bifrost's
	// 1000-worker-per-provider default (§9.5, §6.1).
	DefaultProviderConcurrency = 50

	// DefaultNATSReplicas is the KV/stream replica count the gateway requests
	// when it provisions its assets. 1 is the scale-1 baseline; HA operators
	// MUST override to the real replica count (≥3) — a 1-replica store has no
	// quorum (design §9.5, LLM_BUDGET_EXPECTED_REPLICAS).
	DefaultNATSReplicas = 1

	// DefaultCBFailureThreshold trips the budget-path circuit breaker after this
	// many consecutive NATS failures (design §8.5, LLM_BUDGET_CB_FAILURE_THRESHOLD).
	DefaultCBFailureThreshold = 3

	// DefaultCBOpenDuration is how long the breaker stays open before probing
	// half-open (design §8.5, LLM_BUDGET_CB_OPEN_DURATION_SEC).
	DefaultCBOpenDuration = 10 * time.Second

	// DefaultStreamGrace / MaxStreamGrace bound how long an early-exiting
	// stream keeps its provider stream alive waiting for the authoritative
	// usage trailer (issue #9, LLM_STREAM_GRACE_MS). These MUST equal
	// llmproxy.DefaultStreamGrace / llmproxy.MaxStreamGrace — the rationale for
	// the values lives there, and TestStreamGraceConstantsInSync enforces the
	// pairing so the env default and the handler default cannot drift apart.
	DefaultStreamGrace = 5 * time.Second
	MaxStreamGrace     = 15 * time.Second

	// DefaultStreamDrainLimit bounds concurrent abandoned-stream drains
	// (LLM_STREAM_DRAIN_MAX_INFLIGHT). MUST equal llmproxy.DefaultStreamDrainLimit.
	DefaultStreamDrainLimit = 256

	// DefaultNATSFailMode is the platform-baseline NATS-failure policy (§8.5,
	// LLM_BUDGET_NATS_FAIL_MODE). A per-project override on
	// gateway.project_budget.nats_fail_mode may narrow it.
	DefaultNATSFailMode = "tiered_hybrid"

	// DefaultPGFreshnessMin is how old the Postgres snapshot may be and still be
	// trusted for the tiered-hybrid fallback (§8.5, LLM_BUDGET_PG_FRESHNESS_MIN).
	// A snapshot older than this ⇒ NATS_DOWN_PG_STALE ⇒ 503.
	DefaultPGFreshnessMin = 5 * time.Minute

	// DefaultNATSDegradedMaxDuration is the continuous-outage ceiling; once NATS
	// has been down longer than this the FSM forces closed (503) regardless of
	// snapshot freshness (§8.5, LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN).
	DefaultNATSDegradedMaxDuration = 10 * time.Minute

	// DefaultNATSDegradedCapPct is the per-replica degraded-window overspend cap
	// expressed as a percentage of hard_limit_usd when
	// LLM_BUDGET_NATS_DEGRADED_CAP_USD is unset (§8.5, "default 10 % of
	// hard_limit_usd"). A positive LLM_BUDGET_NATS_DEGRADED_CAP_USD overrides it
	// with an absolute USD cap.
	DefaultNATSDegradedCapPct = 10

	// DefaultRealtimeBudgetRecheck is how often a LIVE realtime session re-asks
	// the budget gate (LLM_REALTIME_BUDGET_RECHECK_SEC). It MUST equal
	// llmproxy.DefaultRealtimeBudgetRecheck; TestRealtimeConstantsInSync holds
	// the pair together, as TestStreamGraceConstantsInSync does for the grace.
	//
	// Why a periodic re-check is the MANDATORY mechanism, and why 15 s: an HTTP
	// request is gated once because it ends. A realtime session does not end —
	// the tenant holds the socket open — and the only turn-start signal bifrost
	// reports (response.create, and the SERVER-side
	// input_audio_buffer.committed) is one the only known caller never sends, so
	// a re-check armed on turn start ALONE would never fire at all. 15 s bounds
	// the spend that can follow an exhausted budget to about one interval, which
	// is a small number of turns; it costs one budget read per session per
	// interval, i.e. about 9 reads a second at the shipped session ceiling.
	DefaultRealtimeBudgetRecheck = 15 * time.Second

	// DefaultGovernanceRefresh is how often the gateway re-reads the authored
	// governance definitions from gateway.governance_config. It mirrors
	// policy.DefaultRefreshInterval; TestGovernanceRefreshDefaultInSync holds
	// the two together.
	//
	// It bounds how long an operator waits between saving a definition and the
	// gateway enforcing it, for the case where the warm-reload event did not
	// arrive. It is a POLL and not the only path: elitea-main publishes a
	// reload event on every write, and this is the floor under that guarantee.
	DefaultGovernanceRefresh = 30 * time.Second

	// DefaultRealtimeMaxSessions bounds concurrent realtime sessions on one
	// replica (LLM_REALTIME_MAX_SESSIONS). It MUST equal
	// llmproxy.DefaultRealtimeMaxSessions.
	//
	// The resource is not CPU: each session pins four goroutines, two open
	// sockets and their read buffers for as long as the tenant keeps talking,
	// and NO server timeout can reap it — a hijacked connection has its
	// deadlines cleared, so ReadHeaderTimeout and IdleTimeout stop applying the
	// moment the upgrade completes. 128 is half the stream-drain pool, because a
	// session lives for minutes where a drain lives for seconds.
	DefaultRealtimeMaxSessions = 128
)

// Config holds the resolved gateway configuration.
type Config struct {
	// HTTPAddr is the listen address for the /llm HTTP server.
	HTTPAddr string
	// DatabaseURL is the Postgres DSN (Fernet vault + governance rows live here).
	DatabaseURL string
	// ShutdownTimeout bounds graceful shutdown; ≥150s (§9.5).
	ShutdownTimeout time.Duration
	// InitialPoolSize is passed to BifrostConfig.InitialPoolSize.
	InitialPoolSize int
	// ProviderConcurrency is applied per-provider via the Account interface.
	ProviderConcurrency int
	// LogLevel controls the slog handler level ("debug"|"info"|"warn"|"error").
	LogLevel string
	// ServiceName / ServiceVersion feed the OTel resource.
	ServiceName    string
	ServiceVersion string
	// OTLPEndpoint is the OTel collector endpoint ("" disables export).
	OTLPEndpoint string
	// IdentitySecret is the HMAC key the gateway uses to verify the edge's
	// signed identity headers (design §5.3). Empty disables verification (the
	// mTLS transport still authenticates the hop); it MUST match elitea-main's
	// IdentitySecret when set.
	IdentitySecret string

	// HopSecret is the DEDICATED key material behind the hop marker — the
	// anti-circular-routing mechanism (issue #164, GATEWAY_HOP_SECRET). The
	// gateway sets internal/hopmarker.Header on every outbound provider
	// request and refuses any inbound request that already carries this
	// deployment's own marker, which contains a routing loop on its first
	// re-entry.
	//
	// It MUST NOT be IdentitySecret, and nothing derives one from the other.
	// The marker travels to every upstream, and a provider api_base is
	// tenant-authored, so the marker is published to addresses a tenant picks;
	// the key that signs the X-Elitea-* identity headers must not follow it
	// there, and marker rotation must not force identity rotation.
	//
	// It MUST hold the same value on every replica: a loop can leave through
	// replica A and re-enter on replica B, and only a shared key makes B
	// recognise A's marker.
	//
	// Empty leaves hop detection UNARMED. That is a supported posture, but
	// never a silent one — main() states the mode once at startup
	// (logHopMarkerMode), for the reason logLoopBreakerMode exists.
	HopSecret string

	// NATSURL is the NATS JetStream server URL (nats://host:4222) backing the
	// budget-enforcement path (design §8). Empty disables NATS wiring: the
	// gateway then serves /llm without budget enforcement (dev/test only), so
	// startup does not hard-fail when no NATS cluster is reachable.
	NATSURL string
	// NATSReplicas is the KV/stream replica count the gateway requests when
	// provisioning its assets (§9.5); ≥3 for HA quorum.
	NATSReplicas int
	// CBFailureThreshold trips the budget-path circuit breaker after this many
	// consecutive NATS failures (§8.5).
	CBFailureThreshold uint32
	// CBOpenDuration is how long the breaker stays open before probing half-open
	// (§8.5).
	CBOpenDuration time.Duration

	// NATSFailMode is the platform-baseline tiered-hybrid fail policy (§8.5):
	// "tiered_hybrid" (default) | "fail_open" | "fail_closed". A per-project
	// gateway.project_budget.nats_fail_mode overrides it (NULL inherits this).
	NATSFailMode string
	// PGFreshnessMin bounds how stale the Postgres snapshot may be before the
	// fallback degrades to 503 (§8.5, NATS_DOWN_PG_STALE).
	PGFreshnessMin time.Duration
	// NATSDegradedMaxDuration is the continuous-outage ceiling before the FSM
	// forces closed (503) regardless of snapshot freshness (§8.5).
	NATSDegradedMaxDuration time.Duration
	// NATSDegradedCapUSD is an absolute per-replica degraded-window overspend cap
	// in USD (§8.5). 0 means "use DefaultNATSDegradedCapPct % of hard_limit_usd".
	NATSDegradedCapUSD float64
	// ExpectedReplicas is the operator-configured replica count used for the
	// NATS_DOWN_PG_FRESH_NEAR per-replica cap (§8.5, LLM_BUDGET_EXPECTED_REPLICAS,
	// default 1). It reuses NATSReplicas' env var; kept distinct so the FSM reads
	// an int replica count without re-parsing.
	ExpectedReplicas int

	// TLS / mTLS (FIX #10). When TLSCertFile and TLSKeyFile are both set the
	// server switches to ListenAndServeTLS. When TLSCAFile is also set, client
	// certificates are required and verified against the CA bundle (mTLS).
	// All three are empty by default (plain HTTP, for local/dev use).
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string

	// StreamGrace is how long a streamed response whose SSE loop exited early
	// (client disconnect, mid-stream provider error, failed stream setup) may
	// keep its PROVIDER stream alive waiting for the authoritative usage
	// trailer, so the tokens the provider actually produced can be billed
	// (issue #9, DECISIONS.md 2026-08-05). Read from LLM_STREAM_GRACE_MS,
	// clamped to [0, llmproxy.MaxStreamGrace]. 0 disables the mechanism: the
	// provider stream is then torn down with the client request as before and
	// an early exit bills nothing (the loss is still metered).
	StreamGrace time.Duration
	// StreamDrainLimit bounds how many abandoned streams may be drained
	// concurrently; each holds a goroutine and an open provider socket for up
	// to StreamGrace. Read from LLM_STREAM_DRAIN_MAX_INFLIGHT.
	StreamDrainLimit int

	// SelfLLMOrigins are the platform's own /llm origins (comma-separated in
	// GATEWAY_SELF_LLM_ORIGINS, e.g. "https://dev.elitea.ai/llm/v1,
	// http://elitea-main:8080/llm/v1"). Any credential api_base matching one
	// of these is rejected with SELF_REFERENTIAL_CREDENTIAL (spec §2.6 guard
	// #1). Empty = the request-time guard is inert (the upsert-time guard in
	// elitea-main still applies).
	SelfLLMOrigins []string

	// EgressAllowlist enumerates the hosts a TENANT-AUTHORED credential
	// `api_base` may point at (comma-separated in GATEWAY_EGRESS_ALLOWLIST,
	// e.g. "vllm.ml.svc.cluster.local:8000,*.openai.azure.com"). Entries are
	// `host` or `host:port`, with an optional leading "*." wildcard covering
	// exactly one or more leading labels.
	//
	// Issue #13. This is the operator's egress policy for the three provider
	// classes whose endpoint is tenant-supplied (Ollama, Azure, vLLM).
	//
	// GAP G6: it is the BOOTSTRAP FLOOR, not the whole policy. internal/account
	// merges it with the `egress_allowlist` rows internal/policy compiles from
	// gateway.governance_config, and enforces the UNION. An authored row can
	// only ADD a destination; nothing authored at runtime withdraws a host this
	// variable names, because the chart and the admin console are different
	// authorities.
	//
	// Entries are also accepted as CIDR blocks, and that matters:
	//
	//   host restriction — off while BOTH sources are empty. The first entry
	//     from either source turns it on, and every credential api_base must
	//     then match (checked BEFORE the Fernet vault resolves its secret, so a
	//     non-allowlisted destination never sees a decrypted key).
	//   private network — bifrost's SSRF-safe dialer is relaxed for the
	//     self-hosted classes only when an entry EXPLICITLY names a private
	//     host or block. It used to follow from "is any allowlist configured",
	//     which relaxed the dialer for an allowlist of public SaaS hosts and
	//     still left an on-premise endpoint unreachable without a chart edit.
	EgressAllowlist []string

	// LoopBreakerThreshold / LoopBreakerWindow / LoopBreakerOpenFor are the
	// per-(project_id, model) amplification backstop's numbers (issue #12,
	// LLM_LOOP_BREAKER_THRESHOLD / _WINDOW_MS / _OPEN_SEC). They were hardcoded
	// at 5 / 1s / 30s, which made the layer a de-facto 5 req/s rate limiter
	// armed in production — a 50-VU run against one tuple measured 99.96% 429.
	//
	// LoopBreakerThreshold < 0 DISARMS the backstop entirely; 0 means "unset,
	// use the default". Either way main() logs the resulting mode at startup —
	// the guard must never quietly pretend to be armed.
	// See llmproxy.DefaultLoopBreakerThreshold for the derivation of 1000.
	LoopBreakerThreshold int
	LoopBreakerWindow    time.Duration
	LoopBreakerOpenFor   time.Duration

	// PublicProjectID is the platform's shared ("public") project id
	// (ELITEA_AI_PROJECT_ID — the same variable elitea-main reads). The gateway
	// reads that project's `shared = true` configuration rows IN ADDITION to the
	// caller's own rows, so a platform-published model and its credential
	// resolve for every project (issue #316).
	//
	// 0 means UNSET, and unset disables the second scope completely: the gateway
	// then reads p_{caller} only, exactly as it did before. This default is
	// deliberate — an id naming a schema that does not exist would make every
	// credential read fail, so an operator opts in.
	//
	// This is operator configuration, NOT request data. It must never come from
	// a header: the value selects a second schema to read, so a request-supplied
	// value would let a caller name any project and read its rows. See
	// DECISIONS.md ("Shared/public project scope").
	PublicProjectID int

	// RealtimeBudgetRecheck is how often a live realtime session re-asks the
	// budget gate (LLM_REALTIME_BUDGET_RECHECK_SEC). See
	// DefaultRealtimeBudgetRecheck for why the periodic re-check is mandatory
	// and not an optimisation.
	RealtimeBudgetRecheck time.Duration

	// GovernanceRefresh is the poll interval for the authored governance
	// definitions (LLM_GOVERNANCE_REFRESH_SEC). 0 or negative selects
	// DefaultGovernanceRefresh.
	GovernanceRefresh time.Duration

	// RealtimeMaxSessions bounds concurrent realtime sessions on this replica
	// (LLM_REALTIME_MAX_SESSIONS). The per-project cap is derived from it.
	RealtimeMaxSessions int

	// RealtimeAllowedOrigins lists the browser origins that may open
	// /llm/v1/realtime (comma-separated in LLM_REALTIME_ALLOWED_ORIGINS, e.g.
	// "https://dev.elitea.ai"). Entries are matched with path.Match against the
	// Origin header host, or against "scheme://host" when the entry carries a
	// scheme.
	//
	// EMPTY IS THE SECURE DEFAULT, and it is not "no policy": a WebSocket
	// handshake is not subject to CORS, so a browser page can open one
	// cross-site with the ambient credentials of the user. With this empty the
	// gateway admits only a handshake whose Origin matches its OWN host, or one
	// that carries no Origin at all — which is every non-browser client,
	// including the pylon-indexer relay this route exists for. A browser origin
	// must be named here on purpose.
	RealtimeAllowedOrigins []string
}

// FromEnv builds a Config from environment variables, applying the §9.5
// defaults for any value that is unset or invalid.
func FromEnv() Config {
	return Config{
		HTTPAddr:            envOr("GATEWAY_HTTP_ADDR", ":8083"),
		DatabaseURL:         envOr("DATABASE_URL", "postgres://localhost:5432/elitea?sslmode=disable"),
		ShutdownTimeout:     durationOr("GATEWAY_SHUTDOWN_TIMEOUT", DefaultShutdownTimeout),
		InitialPoolSize:     intOr("GATEWAY_INITIAL_POOL_SIZE", DefaultInitialPoolSize),
		ProviderConcurrency: intOr("GATEWAY_PROVIDER_CONCURRENCY", DefaultProviderConcurrency),
		LogLevel:            envOr("GATEWAY_LOG_LEVEL", "info"),
		ServiceName:         envOr("OTEL_SERVICE_NAME", "elitea-llm-gateway"),
		ServiceVersion:      envOr("SERVICE_VERSION", "dev"),
		OTLPEndpoint:        os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		IdentitySecret:      os.Getenv("GATEWAY_IDENTITY_SECRET"),
		HopSecret:           os.Getenv("GATEWAY_HOP_SECRET"),
		NATSURL:             os.Getenv("GATEWAY_NATS_URL"),
		NATSReplicas:        intOr("LLM_BUDGET_EXPECTED_REPLICAS", DefaultNATSReplicas),
		CBFailureThreshold:  uint32Or("LLM_BUDGET_CB_FAILURE_THRESHOLD", DefaultCBFailureThreshold),
		CBOpenDuration:      secondsOr("LLM_BUDGET_CB_OPEN_DURATION_SEC", DefaultCBOpenDuration),

		NATSFailMode:            failModeOr("LLM_BUDGET_NATS_FAIL_MODE", DefaultNATSFailMode),
		PGFreshnessMin:          minutesOr("LLM_BUDGET_PG_FRESHNESS_MIN", DefaultPGFreshnessMin),
		NATSDegradedMaxDuration: minutesOr("LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN", DefaultNATSDegradedMaxDuration),
		NATSDegradedCapUSD:      floatOr("LLM_BUDGET_NATS_DEGRADED_CAP_USD", 0),
		ExpectedReplicas:        intOr("LLM_BUDGET_EXPECTED_REPLICAS", DefaultNATSReplicas),
		TLSCertFile:             os.Getenv("GATEWAY_TLS_CERT_FILE"),
		TLSKeyFile:              os.Getenv("GATEWAY_TLS_KEY_FILE"),
		TLSCAFile:               os.Getenv("GATEWAY_TLS_CA_FILE"),
		StreamGrace:             millisOr("LLM_STREAM_GRACE_MS", DefaultStreamGrace, MaxStreamGrace),
		StreamDrainLimit:        intOr("LLM_STREAM_DRAIN_MAX_INFLIGHT", DefaultStreamDrainLimit),
		SelfLLMOrigins:          csvOr("GATEWAY_SELF_LLM_ORIGINS"),
		EgressAllowlist:         csvOr("GATEWAY_EGRESS_ALLOWLIST"),
		LoopBreakerThreshold:    signedIntOr("LLM_LOOP_BREAKER_THRESHOLD", 0),
		LoopBreakerWindow:       plainMillisOr("LLM_LOOP_BREAKER_WINDOW_MS", 0),
		LoopBreakerOpenFor:      secondsOr("LLM_LOOP_BREAKER_OPEN_SEC", 0),
		PublicProjectID:         intOr("ELITEA_AI_PROJECT_ID", 0),

		RealtimeBudgetRecheck:  secondsOr("LLM_REALTIME_BUDGET_RECHECK_SEC", DefaultRealtimeBudgetRecheck),
		GovernanceRefresh:      secondsOr("LLM_GOVERNANCE_REFRESH_SEC", DefaultGovernanceRefresh),
		RealtimeMaxSessions:    intOr("LLM_REALTIME_MAX_SESSIONS", DefaultRealtimeMaxSessions),
		RealtimeAllowedOrigins: csvOr("LLM_REALTIME_ALLOWED_ORIGINS"),
	}
}

// PublicProjectIDString returns the configured public project id as the string
// the schema-name builders take, or "" when the scope is unset. intOr already
// rejects a non-numeric or non-positive value, so the result is always either
// "" or a positive decimal integer.
func (c Config) PublicProjectIDString() string {
	if c.PublicProjectID <= 0 {
		return ""
	}
	return strconv.Itoa(c.PublicProjectID)
}

// millisOr reads an integer number of milliseconds from key and clamps it to
// [0, max]. Unlike the other *Or helpers it accepts an explicit 0 — for
// LLM_STREAM_GRACE_MS zero is a meaningful value (disable the stream-grace
// mechanism), not "unset". A negative or unparsable value falls back to def.
func millisOr(key string, def, max time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	// Clamp on the INTEGER, before the multiply. n * time.Millisecond overflows
	// int64 for absurd values and wraps negative, which would sail past a
	// post-multiply `d > max` check and silently DISABLE the mechanism instead
	// of capping it.
	if int64(n) > int64(max/time.Millisecond) {
		return max
	}
	return time.Duration(n) * time.Millisecond
}

// signedIntOr reads an integer that may legitimately be NEGATIVE. The other
// int helpers treat "not > 0" as unset, which would silently swallow the
// loop-breaker's "-1 = disarm" sentinel (issue #12).
func signedIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// plainMillisOr reads an integer number of milliseconds with no upper clamp
// (unlike millisOr, which exists for the bounded stream grace).
func plainMillisOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return def
}

// csvOr splits a comma-separated env var into trimmed, non-empty entries.
func csvOr(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func durationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func uint32Or(key string, def uint32) uint32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n > 0 {
			return uint32(n)
		}
	}
	return def
}

// secondsOr reads an integer number of seconds from key and returns it as a
// time.Duration. The design surfaces the breaker open-duration knob as
// LLM_BUDGET_CB_OPEN_DURATION_SEC (a bare integer), not a Go duration string.
func secondsOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// minutesOr reads an integer number of minutes from key. The §8.5 freshness /
// max-duration knobs are surfaced as bare integer minutes
// (LLM_BUDGET_PG_FRESHNESS_MIN, LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN).
func minutesOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return def
}

// floatOr reads a non-negative float from key (LLM_BUDGET_NATS_DEGRADED_CAP_USD,
// a USD amount). A missing / invalid / negative value returns def.
func floatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}

// failModeOr reads the NATS fail-mode policy, accepting only the three valid
// values (§8.5). Any other value falls back to def so a typo cannot silently
// disable enforcement.
func failModeOr(key, def string) string {
	switch os.Getenv(key) {
	case "tiered_hybrid", "fail_open", "fail_closed":
		return os.Getenv(key)
	default:
		return def
	}
}
