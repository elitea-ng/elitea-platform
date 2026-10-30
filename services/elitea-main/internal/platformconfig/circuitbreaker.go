package platformconfig

// SectionCircuitBreaker holds the agent circuit breaker rules (ADR-0041
// decision 2, #1498): whether the breaker runs, the error-rate window, the
// storm limit and, per risk tier, the rule limits and who may resume a
// trip. The main.agent_breaker routine reads it once per pass; the parsing,
// the defaults and the validation live in internal/application/breaker,
// beside the evaluation that applies them.
//
// A key that is absent keeps the default (breaker.DefaultSettings).
const SectionCircuitBreaker = "circuit_breaker"

// Field keys of SectionCircuitBreaker. The tier keys are
// "<tier>_<suffix>", for example "high_error_rate_percent".
const (
	KeyBreakerEnabled              = "enabled"
	KeyBreakerErrorRateWindowRuns  = "error_rate_window_runs"
	KeyBreakerStormLimit           = "storm_limit"
	KeyBreakerErrorRatePercentSfx  = "error_rate_percent"
	KeyBreakerSpendPerHourUSDSfx   = "spend_per_hour_usd"
	KeyBreakerToolFailureStreakSfx = "tool_failure_streak"
	KeyBreakerRunsPerHourSfx       = "runs_per_hour"
	KeyBreakerResumeRequiresSfx    = "resume_requires"
)
