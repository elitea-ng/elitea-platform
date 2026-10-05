package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound marks a run the project does not hold. The API layer answers it
// 404: an execution id from another project and an id nobody minted are the
// same answer, so the route does not confirm that another project's id exists.
var ErrNotFound = errors.New("analytics: not found")

// ErrBadID marks a run id the caller sent that cannot name a run. The API
// layer answers it 400.
var ErrBadID = errors.New("analytics: invalid id")

// BadIDError names the offending value.
func BadIDError(kind, raw string) error {
	return fmt.Errorf("%w: %q is not a valid %s id", ErrBadID, raw, kind)
}

// Reasons a run's figures are unavailable. A run with no figures is reported
// with one of these, never with zero totals: zero is a measurement.
const (
	// UnavailableBeforeAttribution is a run that started before this
	// deployment attributed model calls to it: shared migration 0100 for a
	// runtime execution, shared migration 0140 for an evaluation run.
	UnavailableBeforeAttribution = "before_attribution"
	// UnavailableLogPruned is a run whose calls the gateway request log no
	// longer holds. The gateway prunes the log after its retention window.
	UnavailableLogPruned = "log_pruned"
)

// UsageFigures is one scope's model calls, tokens and estimated cost, read
// from gateway.llm_request_logs.
//
// The money is an ESTIMATE at the gateway.gateway_models catalogue rate, the
// same estimate /analytics_costs publishes under `estimate`. It is a string
// (json.Number) so no float touches it, and it is ABSENT when no call in the
// scope has a catalogue price: a zero would claim the calls were free.
type UsageFigures struct {
	LLMCalls         int64 `json:"llm_calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	Errors           int64 `json:"errors"`
	// AvgDurationMS is the mean gateway latency of the calls.
	AvgDurationMS float64 `json:"avg_duration_ms"`
	// UnpricedCalls is how many calls the catalogue does not price. A large
	// value says the money covers only part of the calls.
	UnpricedCalls int64        `json:"unpriced_calls"`
	InputCost     *json.Number `json:"input_cost,omitempty"`
	OutputCost    *json.Number `json:"output_cost,omitempty"`
	TotalCost     *json.Number `json:"total_cost,omitempty"`
}

// ModelFigures is one (provider, model) inside a run.
type ModelFigures struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	UsageFigures
}

// UserFigures is one caller inside a run. Email and Name are empty when the
// identity tables are absent.
type UserFigures struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	UsageFigures
}

// ExecutionAnalytics is one runtime execution's totals (legacy issues 6667
// and 6816): the calls that carry its execution id, inside its lifetime. A
// nested agent signs its parent's id, so its calls are part of these totals.
type ExecutionAnalytics struct {
	ExecutionID  string `json:"execution_id"`
	CapabilityID string `json:"capability_id"`
	// TriggerOrigin is how the run started (shared 0140): manual, api,
	// schedule, webhook or index.
	TriggerOrigin string     `json:"trigger_origin"`
	ActorUserID   string     `json:"actor_user_id"`
	State         string     `json:"state"`
	AdmittedAt    time.Time  `json:"admitted_at"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`

	// Available is false when the figures cannot be measured. The figure
	// fields are then ABSENT, and UnavailableReason says why.
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	Totals      *UsageFigures    `json:"totals,omitempty"`
	ByModel     []ModelFigures   `json:"by_model,omitempty"`
	ByUser      []UserFigures    `json:"by_user,omitempty"`
	ByErrorCode []ErrorCodeCount `json:"by_error_code,omitempty"`

	// ToolsAvailable is false when this database records no tool calls
	// (shared 0119). Tools is then absent.
	ToolsAvailable bool            `json:"tool_dimension_available"`
	Tools          []ToolAnalytics `json:"tools,omitempty"`
}

// EvaluationCaseFigures is one case of an evaluation run, with the agent turn
// and the judge calls kept apart. They are different roles and neither is
// added into the other.
type EvaluationCaseFigures struct {
	CaseID string       `json:"case_id"`
	Agent  UsageFigures `json:"agent"`
	Judge  UsageFigures `json:"judge"`
}

// EvaluationRunAnalytics is one evaluation run's spend (legacy issues 6677
// and 6817): every call attributed `eval:<run>:`.
type EvaluationRunAnalytics struct {
	RunID                string     `json:"run_id"`
	ApplicationID        *int       `json:"application_id"`
	ApplicationVersionID *int       `json:"application_version_id"`
	Status               string     `json:"status"`
	CreatedBy            *int       `json:"created_by"`
	CreatedAt            time.Time  `json:"created_at"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`

	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	// Totals is the whole run. Agent and Judge are its two roles, and they
	// add up to Totals.
	Totals *UsageFigures           `json:"totals,omitempty"`
	Agent  *UsageFigures           `json:"agent,omitempty"`
	Judge  *UsageFigures           `json:"judge,omitempty"`
	ByCase []EvaluationCaseFigures `json:"by_case,omitempty"`
	// ByCaseTruncated is true when ByCase was cut to its cap. The roles and
	// the total still cover every case.
	ByCaseTruncated bool             `json:"by_case_truncated,omitempty"`
	ByModel         []ModelFigures   `json:"by_model,omitempty"`
	ByErrorCode     []ErrorCodeCount `json:"by_error_code,omitempty"`
}

// AutomatedActivity is one unattended trigger origin's share of a window
// (legacy issues 6802 and 6881). These calls are excluded from the
// active-user figures and still counted in every total and in the money.
//
// LLMCalls, the tokens and the money count COMPLETED calls, the row set of
// the window's call and token totals, so each row is a share of them. Errors
// counts the failed attempts, which that row set leaves out.
type AutomatedActivity struct {
	TriggerOrigin string `json:"trigger_origin"`
	Executions    int64  `json:"executions"`
	Users         int64  `json:"users"`
	UsageFigures
}
