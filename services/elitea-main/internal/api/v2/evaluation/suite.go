package evaluation

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/entitylimits"
)

// The evaluation suite holds the SETTINGS a run is started with (issue 2013,
// step 1): token and cost limits per case and per run, and what the run does
// when its run limit is reached. Read goes through PermissionRunRead and write
// through PermissionRunCreate: a suite is the configuration of starting a run,
// so it needs no vocabulary of its own.
//
// A run does not read its suite while it executes. Start copies the settings
// into the run's snapshot (RunSnapshot.Limits), so editing or deleting a suite
// cannot change a run that already started.

// What a run does when its run limit is reached.
const (
	// RunLimitActionStop ends the run as errored, with the reason on the row.
	RunLimitActionStop = "stop"
	// RunLimitActionReport lets the run finish and only records that the limit
	// was reached (Run.LimitReached).
	RunLimitActionReport = "report"
)

// Which run limit was reached.
const (
	LimitReachedTokens = "tokens"
	LimitReachedCost   = "cost"
)

// Bounds on a limit, so a value always fits the columns (integer and
// numeric(14,4)) and a typo is refused instead of stored.
const (
	MaxTokenLimit = 2_000_000_000
	MaxCostLimit  = 1_000_000.0
)

// RunLimits is the settings part of a suite. A nil limit is "no limit". Tokens
// are prompt plus completion tokens; costs are the estimated USD of the
// gateway's price catalogue, so a call the catalogue does not price adds
// tokens and no cost.
type RunLimits struct {
	TokenLimitPerCase *int64   `json:"token_limit_per_case"`
	CostLimitPerCase  *float64 `json:"cost_limit_per_case"`
	TokenLimitPerRun  *int64   `json:"token_limit_per_run"`
	CostLimitPerRun   *float64 `json:"cost_limit_per_run"`
	// OnRunLimit is RunLimitActionStop or RunLimitActionReport.
	OnRunLimit string `json:"on_run_limit"`
}

// HasAny reports whether any limit is set.
func (l RunLimits) HasAny() bool {
	return l.TokenLimitPerCase != nil || l.CostLimitPerCase != nil ||
		l.TokenLimitPerRun != nil || l.CostLimitPerRun != nil
}

func (l RunLimits) hasRunLimit() bool { return l.TokenLimitPerRun != nil || l.CostLimitPerRun != nil }

func (l RunLimits) hasCaseLimit() bool {
	return l.TokenLimitPerCase != nil || l.CostLimitPerCase != nil
}

// Validate refuses a limit that is not positive or does not fit its column.
func (l RunLimits) Validate() error {
	for name, value := range map[string]*int64{
		"token_limit_per_case": l.TokenLimitPerCase,
		"token_limit_per_run":  l.TokenLimitPerRun,
	} {
		if value != nil && (*value < 1 || *value > MaxTokenLimit) {
			return apierr.BadRequest(fmt.Sprintf("%s must be a whole number between 1 and %d", name, MaxTokenLimit))
		}
	}
	for name, value := range map[string]*float64{
		"cost_limit_per_case": l.CostLimitPerCase,
		"cost_limit_per_run":  l.CostLimitPerRun,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 || *value > MaxCostLimit) {
			return apierr.BadRequest(fmt.Sprintf("%s must be a positive amount of at most %.0f USD", name, MaxCostLimit))
		}
	}
	if l.OnRunLimit != RunLimitActionStop && l.OnRunLimit != RunLimitActionReport {
		return apierr.BadRequest("on_run_limit must be stop or report")
	}
	return nil
}

// Suite is one stored suite row.
type Suite struct {
	ID          string `json:"id"`
	UUID        string `json:"uuid,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RunLimits
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// SuiteWrite is the body of a create or an update.
type SuiteWrite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	RunLimits
}

// Normalize applies the defaults a body may omit.
func (w *SuiteWrite) Normalize() {
	w.Name = strings.TrimSpace(w.Name)
	if w.OnRunLimit == "" {
		w.OnRunLimit = RunLimitActionStop
	}
}

// Validate refuses the bodies the table would refuse.
func (w SuiteWrite) Validate() error {
	if w.Name == "" {
		return apierr.BadRequest("name is required")
	}
	if err := entitylimits.CheckName("name", w.Name); err != nil {
		return err
	}
	if err := entitylimits.CheckDescription("description", w.Description); err != nil {
		return err
	}
	return w.RunLimits.Validate()
}

// SuiteRepository is the suite storage.
type SuiteRepository interface {
	ListSuites(ctx context.Context, projectID string) ([]Suite, error)
	GetSuite(ctx context.Context, projectID, suiteID string) (Suite, error)
	CreateSuite(ctx context.Context, projectID string, suite SuiteWrite) (Suite, error)
	UpdateSuite(ctx context.Context, projectID, suiteID string, suite SuiteWrite) (Suite, error)
	DeleteSuite(ctx context.Context, projectID, suiteID string) error
}

// SuiteReader is what the run start needs of a suite.
type SuiteReader interface {
	GetSuite(ctx context.Context, projectID, suiteID string) (Suite, error)
}

// SuiteHandler serves the suite routes.
type SuiteHandler struct{ suites SuiteRepository }

func NewSuiteHandler(suites SuiteRepository) *SuiteHandler { return &SuiteHandler{suites: suites} }

type suiteListResponse struct {
	Rows  []Suite `json:"rows"`
	Total int     `json:"total"`
}

func (h *SuiteHandler) List(w http.ResponseWriter, r *http.Request) {
	suites, err := h.suites.ListSuites(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if suites == nil {
		suites = []Suite{}
	}
	writeJSON(w, http.StatusOK, suiteListResponse{Rows: suites, Total: len(suites)})
}

func (h *SuiteHandler) decode(w http.ResponseWriter, r *http.Request) (SuiteWrite, bool) {
	var body SuiteWrite
	if !decodeStrict(w, r, &body) {
		return SuiteWrite{}, false
	}
	body.Normalize()
	if err := body.Validate(); err != nil {
		apierr.Write(w, err)
		return SuiteWrite{}, false
	}
	return body, true
}

func (h *SuiteHandler) Create(w http.ResponseWriter, r *http.Request) {
	body, ok := h.decode(w, r)
	if !ok {
		return
	}
	created, err := h.suites.CreateSuite(r.Context(), chi.URLParam(r, "projectID"), body)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *SuiteHandler) Update(w http.ResponseWriter, r *http.Request) {
	body, ok := h.decode(w, r)
	if !ok {
		return
	}
	updated, err := h.suites.UpdateSuite(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "suiteID"), body)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *SuiteHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.suites.DeleteSuite(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "suiteID")); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
