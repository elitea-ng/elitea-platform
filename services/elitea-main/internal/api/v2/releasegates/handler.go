// Package releasegates serves the evaluation release gate of ADR-0040
// decision 4 under /api/v2/elitea_core:
//
//	GET /release_gate_suites/prompt_lib/{projectID}/{applicationID}
//	    the agent's gate suites (models.applications.evaluation.dataset.read)
//	PUT /release_gate_suites/prompt_lib/{projectID}/{applicationID}/{datasetID}
//	    marks a dataset as a gate suite with its criteria, or unmarks it
//	    (models.applications.evaluation.dataset.update and
//	    models.applications.publish.post; a change that loosens the gate of
//	    an agent whose tier needs approval also needs
//	    models.applications.governance, releasegate.Loosens)
//	GET /release_gates/prompt_lib/{projectID}/{applicationID}
//	    the agent's gates, newest first (models.applications.evaluation.run.read)
//	GET /release_gates/prompt_lib/{projectID}/{applicationID}/{gateID}
//	    one gate: its runs, the result per suite and dimension, the
//	    regressed cases and the override (models.applications.evaluation.run.read)
//
// A gate is started and overridden by the change-request flow through
// releasegate.Gate, not by a route here. The router applies the gates. A
// handler with no gate answers 503.
package releasegates

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/releasegate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	SuitesPath = "/release_gate_suites/prompt_lib/{projectID}/{applicationID}"
	SuitePath  = "/release_gate_suites/prompt_lib/{projectID}/{applicationID}/{datasetID}"
	GatesPath  = "/release_gates/prompt_lib/{projectID}/{applicationID}"
	GatePath   = "/release_gates/prompt_lib/{projectID}/{applicationID}/{gateID}"
)

// maxSuiteBody bounds the suite request body.
const maxSuiteBody = 16 << 10

// gateListLimit is the most gates one listing answers.
const gateListLimit = 50

// Gates is *releasegate.Gate.
type Gates interface {
	ListSuites(ctx context.Context, tenantProjectID, applicationID int64) ([]releasegate.SuiteConfig, error)
	PutSuite(ctx context.Context, tenantProjectID int64, suite releasegate.SuiteConfig) (releasegate.SuiteConfig, error)
	DeleteSuite(ctx context.Context, tenantProjectID, applicationID, datasetID int64) (bool, error)
	ListGates(ctx context.Context, tenantProjectID, applicationID int64, limit int) ([]releasegate.GateResult, error)
	Result(ctx context.Context, tenantProjectID, gateID int64) (releasegate.GateResult, error)
}

var _ Gates = (*releasegate.Gate)(nil)

// Governor is the agent lifecycle governance a gate suite edit answers to
// (*repos.AgentLifecycle): the author of an agent whose tier needs
// approval must not empty or soften the gate their own change request is
// tested against.
type Governor interface {
	// CheckDirectPublish refuses (change_request_required) when the
	// agent's tier needs an approved change request.
	CheckDirectPublish(ctx context.Context, projectID, applicationID int64) error
}

// Handler serves the routes.
type Handler struct {
	gates    Gates
	governor Governor
	// holdsGovernance resolves the caller's governance role in the path's
	// project.
	holdsGovernance func(*http.Request) (bool, error)
}

// Option configures the handler.
type Option func(*Handler)

// WithGovernance makes a suite edit that loosens the gate of an agent
// whose tier needs approval require the governance role. A nil governor
// (or a typed nil pointer) is absent.
func WithGovernance(governor Governor, holdsGovernance func(*http.Request) (bool, error)) Option {
	return func(h *Handler) {
		if governor == nil || holdsGovernance == nil {
			return
		}
		if value := reflect.ValueOf(governor); value.Kind() == reflect.Ptr && value.IsNil() {
			return
		}
		h.governor, h.holdsGovernance = governor, holdsGovernance
	}
}

// NewHandler builds the handler. A nil gate (or a typed nil) answers 503.
func NewHandler(gates Gates, options ...Option) *Handler {
	if gate, ok := gates.(*releasegate.Gate); ok && gate == nil {
		gates = nil
	}
	h := &Handler{gates: gates}
	for _, option := range options {
		option(h)
	}
	return h
}

// Suite is one gate suite in a response.
type Suite struct {
	releasegate.SuiteConfig
	ReleaseGate bool `json:"release_gate"`
}

// SuiteRequest is the PUT body. release_gate false unmarks the dataset.
type SuiteRequest struct {
	ReleaseGate         bool                    `json:"release_gate"`
	Regression          bool                    `json:"regression"`
	Criteria            []releasegate.Criterion `json:"criteria"`
	ReevaluateEveryDays *int                    `json:"reevaluate_every_days"`
}

// Gate is one gate in a response.
type Gate struct {
	releasegate.GateResult
	// Blocks is true while the gate blocks approval: pending, or failed
	// and not overridden.
	Blocks bool `json:"blocks"`
}

func gateResponse(gate releasegate.GateResult) Gate {
	return Gate{GateResult: gate, Blocks: gate.Blocks()}
}

func suiteResponse(suite releasegate.SuiteConfig) Suite {
	if suite.Criteria == nil {
		suite.Criteria = []releasegate.Criterion{}
	}
	return Suite{SuiteConfig: suite, ReleaseGate: true}
}

// ListSuites answers the agent's gate suites.
func (h *Handler) ListSuites(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	suites, err := h.gates.ListSuites(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	rows := make([]Suite, 0, len(suites))
	for _, suite := range suites {
		rows = append(rows, suiteResponse(suite))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

// PutSuite marks a dataset as a gate suite, or unmarks it.
func (h *Handler) PutSuite(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	datasetID, ok := pathID(w, r, "datasetID", "invalid dataset id")
	if !ok {
		return
	}
	var body SuiteRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSuiteBody+1))
	if err != nil || len(raw) > maxSuiteBody {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid gate suite request")
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid gate suite request")
		return
	}

	var next *releasegate.SuiteConfig
	if body.ReleaseGate {
		next = &releasegate.SuiteConfig{
			ApplicationID: applicationID, DatasetID: datasetID, Regression: body.Regression,
			Criteria: body.Criteria, ReevaluateEveryDays: body.ReevaluateEveryDays, UpdatedBy: callerID(r),
		}
	}
	if !h.editAllowed(w, r, projectID, applicationID, datasetID, next) {
		return
	}

	if next == nil {
		removed, err := h.gates.DeleteSuite(r.Context(), projectID, applicationID, datasetID)
		if err != nil {
			writeError(r.Context(), w, err)
			return
		}
		if removed {
			annotateSuite(r, projectID, applicationID, datasetID, "unmarked")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"application_id": applicationID, "dataset_id": datasetID, "release_gate": false,
		})
		return
	}
	stored, err := h.gates.PutSuite(r.Context(), projectID, *next)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	kind := "marked"
	if stored.Regression {
		kind = "marked regression"
	}
	annotateSuite(r, projectID, applicationID, datasetID, kind)
	writeJSON(w, http.StatusOK, suiteResponse(stored))
}

// editAllowed refuses a suite edit that loosens the gate (releasegate.
// Loosens) of an agent whose tier needs an approved change request, unless
// the caller holds the governance role: 403 governance_role_required.
// Adding a suite or tightening one stays open to the author.
func (h *Handler) editAllowed(w http.ResponseWriter, r *http.Request, projectID, applicationID, datasetID int64, next *releasegate.SuiteConfig) bool {
	if h.governor == nil {
		return true
	}
	suites, err := h.gates.ListSuites(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(r.Context(), w, err)
		return false
	}
	var current *releasegate.SuiteConfig
	for index := range suites {
		if suites[index].DatasetID == datasetID {
			current = &suites[index]
		}
	}
	if !releasegate.Loosens(current, next) {
		return true
	}
	err = h.governor.CheckDirectPublish(r.Context(), projectID, applicationID)
	if err == nil {
		return true
	}
	if refusal, ok := agentlifecycle.AsRefusal(err); !ok || refusal.Code != agentlifecycle.ChangeRequiredCode {
		if ok {
			writeJSON(w, refusal.Status(), refusal.Body())
			return false
		}
		slog.ErrorContext(r.Context(), "release gates: read the agent's tier", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read the agent's governance")
		return false
	}
	held, err := h.holdsGovernance(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "release gates: resolve the governance role", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not resolve the governance role")
		return false
	}
	if held {
		return true
	}
	refusal := agentlifecycle.Refuse(agentlifecycle.GovernanceRequiredCode,
		"This agent's tier needs approval, so only a holder of the governance role ("+
			agentlifecycle.GovernancePermission+") can unmark a gate suite, unset its regression flag, "+
			"remove or change a criterion's kind, lower a min_score or raise a max_drop.")
	writeJSON(w, refusal.Status(), refusal.Body())
	return false
}

// AuditActionSuiteUpdated is the audit action of a gate suite edit: it
// changes what a release must pass (ADR-0040 decision 4).
const AuditActionSuiteUpdated = "agent.gate_suite.updated"

// annotateSuite names the suite edit in the request's audit event (the
// /release_gate_suites/ audited surface).
func annotateSuite(r *http.Request, projectID, applicationID, datasetID int64, change string) {
	project, dataset := projectID, datasetID
	audit.Annotate(r.Context(), audit.Annotation{
		Action: AuditActionSuiteUpdated, EntityType: "release_gate_suite", EntityID: &dataset,
		EntityName: "application:" + strconv.FormatInt(applicationID, 10) + " dataset:" +
			strconv.FormatInt(datasetID, 10) + " " + change,
		ProjectID: &project,
	})
}

// ListGates answers the agent's gates.
func (h *Handler) ListGates(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	gates, err := h.gates.ListGates(r.Context(), projectID, applicationID, gateListLimit)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	rows := make([]Gate, 0, len(gates))
	for _, gate := range gates {
		rows = append(rows, gateResponse(gate))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

// GetGate answers one gate of the agent.
func (h *Handler) GetGate(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	gateID, ok := pathID(w, r, "gateID", "invalid gate id")
	if !ok {
		return
	}
	gate, err := h.gates.Result(r.Context(), projectID, gateID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if gate.ApplicationID != applicationID {
		writeError(r.Context(), w, releasegate.ErrGateNotFound)
		return
	}
	writeJSON(w, http.StatusOK, gateResponse(gate))
}

// application reads the project and application of the path.
func (h *Handler) application(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	if h == nil || h.gates == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return 0, 0, false
	}
	projectID, ok := pathID(w, r, "projectID", "invalid project id")
	if !ok {
		return 0, 0, false
	}
	applicationID, ok := pathID(w, r, "applicationID", "invalid application id")
	if !ok {
		return 0, 0, false
	}
	return projectID, applicationID, true
}

func pathID(w http.ResponseWriter, r *http.Request, name, message string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil || value <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, message)
		return 0, false
	}
	return value, true
}

// callerID is the signed-in user, recorded as the suite's updated_by. The
// scheduled re-evaluation makes its model calls for this user.
func callerID(r *http.Request) *int64 {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return nil
	}
	raw := user.UserID
	if raw == "" {
		raw = user.ID
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, releasegate.ErrInvalidConfig):
		writeCode(w, http.StatusBadRequest, "release_gate_invalid_config",
			strings.TrimPrefix(err.Error(), releasegate.ErrInvalidConfig.Error()+": "))
	case errors.Is(err, releasegate.ErrDatasetNotFound):
		writeCode(w, http.StatusNotFound, "release_gate_dataset_not_found",
			"The dataset does not exist in this project or belongs to another agent.")
	case errors.Is(err, releasegate.ErrGateNotFound):
		writeCode(w, http.StatusNotFound, "release_gate_not_found", "The gate is not a gate of this agent.")
	case errors.Is(err, releasegate.ErrRegressionTaken):
		writeCode(w, http.StatusConflict, "release_gate_regression_taken",
			"The agent already has a regression suite. Unmark it first.")
	case errors.Is(err, releasegate.ErrTooManySuites):
		writeCode(w, http.StatusConflict, "release_gate_too_many_suites",
			"The agent already has 10 gate suites.")
	default:
		slog.ErrorContext(ctx, "release gates", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the release gate")
	}
}

func writeCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "code": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
