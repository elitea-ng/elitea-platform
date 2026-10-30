// Package agentlifecycle serves agent lifecycle governance (ADR-0040
// decisions 1, 3, 5 and 6) under /api/v2/elitea_core and /api/v2/admin:
//
//	GET  /agent_registry/prompt_lib/{projectID}/{applicationID}
//	     the agent's registry record (models.applications.versions.get)
//	PUT  /agent_registry/prompt_lib/{projectID}/{applicationID}
//	     owners, tier, intended use, data classes
//	     (models.applications.publish.post; an owner or the governance role)
//	POST /agent_registry/prompt_lib/{projectID}/{applicationID}/mark_due
//	     sets a certified agent to due (lifecycle.registry.mark_due)
//	GET  /change_requests/prompt_lib/{projectID}/{applicationID}[/{changeRequestID}]
//	POST /change_requests/prompt_lib/{projectID}/{applicationID}
//	POST /change_requests/prompt_lib/{projectID}/{applicationID}/{changeRequestID}/{submit|approve|reject|withdraw}
//	POST /change_requests/prompt_lib/{projectID}/{applicationID}/{changeRequestID}/gate_override
//	GET  /agent_attestations/prompt_lib/{projectID}
//	POST /agent_attestations/prompt_lib/{projectID}
//	POST /agent_attestations/prompt_lib/{projectID}/{attestationID}/revoke
//	GET  /admin/agent_registry   the cross-project registry, JSON or CSV
//
// The router applies the route permissions. The governance role
// (models.applications.governance) is resolved per request for the
// decisions that depend on it. A handler with no store answers 503.
package agentlifecycle

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	RegistryPath        = "/agent_registry/prompt_lib/{projectID}/{applicationID}"
	MarkDuePath         = "/agent_registry/prompt_lib/{projectID}/{applicationID}/mark_due"
	ChangeRequestsPath  = "/change_requests/prompt_lib/{projectID}/{applicationID}"
	ChangeRequestPath   = "/change_requests/prompt_lib/{projectID}/{applicationID}/{changeRequestID}"
	SubmitPath          = ChangeRequestPath + "/submit"
	ApprovePath         = ChangeRequestPath + "/approve"
	RejectPath          = ChangeRequestPath + "/reject"
	WithdrawPath        = ChangeRequestPath + "/withdraw"
	GateOverridePath    = ChangeRequestPath + "/gate_override"
	AttestationsPath    = "/agent_attestations/prompt_lib/{projectID}"
	RevokeAttestPath    = "/agent_attestations/prompt_lib/{projectID}/{attestationID}/revoke"
	AdminRegistryPath   = "/agent_registry"
	maxBody             = 16 << 10
	defaultAdminListRow = 100
)

// Store is *repos.AgentLifecycle.
type Store interface {
	Registry(ctx context.Context, projectID, applicationID int64) (agentlifecycle.Registry, error)
	UpdateRegistry(ctx context.Context, projectID, applicationID int64, actor repos.RegistryActor, update agentlifecycle.RegistryUpdate) (agentlifecycle.Registry, error)
	MarkDue(ctx context.Context, projectID, applicationID, actorID int64, reason string) (agentlifecycle.Registry, error)
	ListRegistry(ctx context.Context, filter agentlifecycle.RegistryFilter) ([]agentlifecycle.Registry, int, error)
	CreateChangeRequest(ctx context.Context, request agentlifecycle.NewChangeRequest) (agentlifecycle.ChangeRequest, error)
	GetChangeRequest(ctx context.Context, projectID, applicationID, changeID int64) (agentlifecycle.ChangeRequest, error)
	ListChangeRequests(ctx context.Context, projectID, applicationID int64) ([]agentlifecycle.ChangeRequest, error)
	SubmitChangeRequest(ctx context.Context, projectID, applicationID, changeID, actorID int64) (agentlifecycle.ChangeRequest, error)
	ApproveChangeRequest(ctx context.Context, approval repos.Approval) (repos.ApprovalOutcome, error)
	RejectChangeRequest(ctx context.Context, projectID, applicationID, changeID, actorID int64, reason string) (agentlifecycle.ChangeRequest, error)
	WithdrawChangeRequest(ctx context.Context, projectID, applicationID, changeID, actorID int64, governance bool, reason string) (agentlifecycle.ChangeRequest, error)
	OverrideGate(ctx context.Context, projectID, applicationID, changeID, actorID int64, governance bool, reason string) (agentlifecycle.ChangeRequest, error)
	GateResult(ctx context.Context, projectID, gateID int64) (agentlifecycle.GateResult, error)
	RecordAttestation(ctx context.Context, attestation agentlifecycle.Attestation) (agentlifecycle.Attestation, error)
	ListAttestations(ctx context.Context, projectID int64, subjectType agentlifecycle.SubjectType, subjectID string) ([]agentlifecycle.Attestation, error)
	RevokeAttestation(ctx context.Context, projectID, attestationID, actorID int64, reason string) (agentlifecycle.Attestation, error)
}

var _ Store = (*repos.AgentLifecycle)(nil)

// GovernanceCheck reports whether the caller holds the governance role in
// the path's project.
type GovernanceCheck func(*http.Request) (bool, error)

// Handler serves the routes.
type Handler struct {
	store      Store
	governance GovernanceCheck
}

// NewHandler builds the handler. A nil store (or a typed nil) answers 503;
// a nil check answers "no governance role" for every caller.
func NewHandler(store Store, governance GovernanceCheck) *Handler {
	if typed, ok := store.(*repos.AgentLifecycle); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, governance: governance}
}

// ---------------------------------------------------------------------------
// Response shapes

// RegistryRecord is one registry record in a response.
type RegistryRecord struct {
	ProjectID          int64      `json:"project_id"`
	ApplicationID      int64      `json:"application_id"`
	ApplicationName    string     `json:"application_name"`
	BusinessOwnerID    *int64     `json:"business_owner_id"`
	TechnicalOwnerID   *int64     `json:"technical_owner_id"`
	RiskTier           string     `json:"risk_tier"`
	IntendedUse        string     `json:"intended_use"`
	OutOfScopeUse      string     `json:"out_of_scope_use"`
	DataClasses        []string   `json:"data_classes"`
	CertificationState string     `json:"certification_state"`
	CertifiedReleaseID *int64     `json:"certified_release_id"`
	CertifiedAt        *time.Time `json:"certified_at"`
	RecertifyBy        *time.Time `json:"recertify_by"`
	StateReason        string     `json:"state_reason"`
	StateChangedAt     time.Time  `json:"state_changed_at"`
	SuspensionID       *int64     `json:"suspension_id"`
	UpdatedAt          time.Time  `json:"updated_at"`
	// CircuitBreaker is the agent's circuit breaker override (ADR-0041
	// decision 2); {} for none.
	CircuitBreaker json.RawMessage `json:"circuit_breaker"`
}

// Approval is one recorded approval in a response.
type Approval struct {
	ApproverID          int64     `json:"approver_id"`
	HoldsGovernanceRole bool      `json:"holds_governance_role"`
	Note                string    `json:"note"`
	CreatedAt           time.Time `json:"created_at"`
}

// Gate is the release gate of a change request in a response.
type Gate struct {
	ID         int64    `json:"id"`
	State      string   `json:"state"`
	Failures   []string `json:"failures"`
	Overridden bool     `json:"overridden"`
}

// ChangeRequest is one change request in a response.
type ChangeRequest struct {
	ID                          int64      `json:"id"`
	ApplicationID               int64      `json:"application_id"`
	Kind                        string     `json:"kind"`
	State                       string     `json:"state"`
	RiskTier                    string     `json:"risk_tier"`
	CandidateVersionID          *int64     `json:"candidate_version_id"`
	CandidateRevisionID         *int64     `json:"candidate_revision_id"`
	VersionName                 string     `json:"version_name"`
	Category                    string     `json:"category"`
	Summary                     string     `json:"summary"`
	ProposedBy                  *int64     `json:"proposed_by"`
	SelfApproved                bool       `json:"self_approved"`
	RequiredApprovals           int        `json:"required_approvals"`
	RequiredGovernanceApprovals int        `json:"required_governance_approvals"`
	GateID                      *int64     `json:"gate_id"`
	Gate                        *Gate      `json:"gate,omitempty"`
	OverrideReason              string     `json:"override_reason"`
	OverriddenBy                *int64     `json:"overridden_by"`
	OverriddenAt                *time.Time `json:"overridden_at"`
	CurrentReleaseID            *int64     `json:"current_release_id"`
	ReleaseID                   *int64     `json:"release_id"`
	DecidedBy                   *int64     `json:"decided_by"`
	DecidedAt                   *time.Time `json:"decided_at"`
	DecisionReason              string     `json:"decision_reason"`
	CreatedAt                   time.Time  `json:"created_at"`
	SubmittedAt                 *time.Time `json:"submitted_at"`
	PublishedAt                 *time.Time `json:"published_at"`
	Approvals                   []Approval `json:"approvals"`
}

// Attestation is one attestation in a response.
type Attestation struct {
	ID             int64      `json:"id"`
	Kind           string     `json:"kind"`
	CustomKind     string     `json:"custom_kind"`
	SubjectType    string     `json:"subject_type"`
	SubjectID      string     `json:"subject_id"`
	Attester       string     `json:"attester"`
	AttesterUserID *int64     `json:"attester_user_id"`
	ValidUntil     *time.Time `json:"valid_until"`
	EvidenceURL    string     `json:"evidence_url"`
	Note           string     `json:"note"`
	RecordedBy     int64      `json:"recorded_by"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	RevokedBy      *int64     `json:"revoked_by"`
	RevokeReason   string     `json:"revoke_reason"`
	Valid          bool       `json:"valid"`
}

func positive(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func registryRecord(r agentlifecycle.Registry) RegistryRecord {
	classes := r.DataClasses
	if classes == nil {
		classes = []string{}
	}
	return RegistryRecord{
		ProjectID: r.ProjectID, ApplicationID: r.ApplicationID, ApplicationName: r.ApplicationName,
		BusinessOwnerID: positive(r.BusinessOwnerID), TechnicalOwnerID: positive(r.TechnicalOwnerID),
		RiskTier: string(r.Tier), IntendedUse: r.IntendedUse, OutOfScopeUse: r.OutOfScopeUse, DataClasses: classes,
		CertificationState: string(r.State), CertifiedReleaseID: positive(r.CertifiedReleaseID),
		CertifiedAt: r.CertifiedAt, RecertifyBy: r.RecertifyBy, StateReason: r.StateReason,
		StateChangedAt: r.StateChangedAt, SuspensionID: positive(r.SuspensionID), UpdatedAt: r.UpdatedAt,
		CircuitBreaker: breakerOverride(r.CircuitBreaker),
	}
}

// breakerOverride is the stored override, {} for none or an unreadable one.
func breakerOverride(raw []byte) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}

func changeRequest(c agentlifecycle.ChangeRequest) ChangeRequest {
	approvals := make([]Approval, 0, len(c.Approvals))
	for _, a := range c.Approvals {
		approvals = append(approvals, Approval{ApproverID: a.ApproverID, HoldsGovernanceRole: a.Governance, Note: a.Note, CreatedAt: a.CreatedAt})
	}
	return ChangeRequest{
		ID: c.ID, ApplicationID: c.ApplicationID, Kind: string(c.Kind), State: string(c.State), RiskTier: string(c.Tier),
		CandidateVersionID: positive(c.CandidateVersionID), CandidateRevisionID: positive(c.CandidateRevisionID),
		VersionName: c.VersionName, Category: c.Category, Summary: c.Summary, ProposedBy: positive(c.ProposedBy),
		SelfApproved: c.SelfApproved, RequiredApprovals: c.RequiredApprovals,
		RequiredGovernanceApprovals: c.RequiredGovernanceApprovals, GateID: positive(c.GateID),
		OverrideReason: c.OverrideReason, OverriddenBy: positive(c.OverriddenBy), OverriddenAt: c.OverriddenAt,
		CurrentReleaseID: positive(c.CurrentReleaseID), ReleaseID: positive(c.ReleaseID),
		DecidedBy: positive(c.DecidedBy), DecidedAt: c.DecidedAt, DecisionReason: c.DecisionReason,
		CreatedAt: c.CreatedAt, SubmittedAt: c.SubmittedAt, PublishedAt: c.PublishedAt, Approvals: approvals,
	}
}

func attestation(a agentlifecycle.Attestation) Attestation {
	return Attestation{
		ID: a.ID, Kind: string(a.Kind), CustomKind: a.CustomKind, SubjectType: string(a.SubjectType),
		SubjectID: a.SubjectID, Attester: a.Attester, AttesterUserID: positive(a.AttesterUserID),
		ValidUntil: a.ValidUntil, EvidenceURL: a.EvidenceURL, Note: a.Note, RecordedBy: a.RecordedBy,
		CreatedAt: a.CreatedAt, RevokedAt: a.RevokedAt, RevokedBy: positive(a.RevokedBy),
		RevokeReason: a.RevokeReason, Valid: a.Valid(time.Now()),
	}
}

// ---------------------------------------------------------------------------
// Registry

// GetRegistry answers the agent's registry record.
func (h *Handler) GetRegistry(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	registry, err := h.store.Registry(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, registryRecord(registry))
}

// UpdateRegistry edits the agent's registry record.
func (h *Handler) UpdateRegistry(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		BusinessOwnerID  *int64    `json:"business_owner_id"`
		TechnicalOwnerID *int64    `json:"technical_owner_id"`
		RiskTier         *string   `json:"risk_tier"`
		IntendedUse      *string   `json:"intended_use"`
		OutOfScopeUse    *string   `json:"out_of_scope_use"`
		DataClasses      *[]string `json:"data_classes"`
		// CircuitBreaker replaces the agent's breaker override; {} or null
		// clears it. A looser override than the tier default is refused
		// with 422 circuit_breaker_override_loosens.
		CircuitBreaker json.RawMessage `json:"circuit_breaker"`
	}
	if !decode(w, r, &body, false) {
		return
	}
	update := agentlifecycle.RegistryUpdate{
		BusinessOwnerID: body.BusinessOwnerID, TechnicalOwnerID: body.TechnicalOwnerID,
		IntendedUse: body.IntendedUse, OutOfScopeUse: body.OutOfScopeUse, DataClasses: body.DataClasses,
	}
	if len(body.CircuitBreaker) > 0 {
		update.CircuitBreaker = []byte(body.CircuitBreaker)
	}
	if body.RiskTier != nil {
		tier := agentlifecycle.Tier(*body.RiskTier)
		if !tier.Valid() {
			writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "risk_tier is low, medium, high or critical."))
			return
		}
		update.Tier = &tier
	}
	governance, ok := h.holdsGovernance(w, r)
	if !ok {
		return
	}
	registry, err := h.store.UpdateRegistry(r.Context(), projectID, applicationID,
		repos.RegistryActor{UserID: actorID, Governance: governance}, update)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, registryRecord(registry))
}

// MarkDue sets a certified agent to due.
func (h *Handler) MarkDue(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body, true) {
		return
	}
	registry, err := h.store.MarkDue(r.Context(), projectID, applicationID, actorID, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, registryRecord(registry))
}

// AdminRegistry is the cross-project registry: JSON, or CSV with
// ?format=csv (up to MaxRegistryExportRows rows).
func (h *Handler) AdminRegistry(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	query := r.URL.Query()
	filter := agentlifecycle.RegistryFilter{
		Tier: agentlifecycle.Tier(query.Get("tier")), State: agentlifecycle.CertificationState(query.Get("state")),
	}
	if filter.Tier != "" && !filter.Tier.Valid() {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "tier is low, medium, high or critical."))
		return
	}
	if filter.State != "" && !filter.State.Valid() {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			"state is uncertified, certified, due, lapsed or suspended."))
		return
	}
	format := query.Get("format")
	if format != "" && format != "json" && format != "csv" {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "format is json or csv."))
		return
	}
	for _, field := range []struct {
		name   string
		target *int64
	}{{"owner_id", &filter.OwnerID}, {"project_id", &filter.ProjectID}} {
		if raw := query.Get(field.name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value <= 0 {
				writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, field.name+" is a positive id."))
				return
			}
			*field.target = value
		}
	}
	limit, offset := defaultAdminListRow, 0
	if format == "csv" {
		limit = agentlifecycle.MaxRegistryExportRows
	}
	for _, field := range []struct {
		name   string
		target *int
		max    int
	}{{"limit", &limit, agentlifecycle.MaxRegistryListRows}, {"offset", &offset, 1 << 30}} {
		if raw := query.Get(field.name); raw != "" && format != "csv" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || value > field.max {
				writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
					field.name+" is an integer from 0 to "+strconv.Itoa(field.max)+"."))
				return
			}
			*field.target = value
		}
	}
	if limit == 0 {
		limit = defaultAdminListRow
	}
	filter.Limit, filter.Offset = limit, offset
	rows, total, err := h.store.ListRegistry(r.Context(), filter)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if format == "csv" {
		writeRegistryCSV(w, rows)
		return
	}
	records := make([]RegistryRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, registryRecord(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": records})
}

func writeRegistryCSV(w http.ResponseWriter, rows []agentlifecycle.Registry) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="agent_registry.csv"`)
	w.WriteHeader(http.StatusOK)
	out := csv.NewWriter(w)
	_ = out.Write([]string{"project_id", "application_id", "application_name", "business_owner_id",
		"technical_owner_id", "risk_tier", "certification_state", "certified_release_id", "certified_at",
		"recertify_by", "state_reason", "data_classes", "intended_use", "out_of_scope_use", "suspension_id"})
	id := func(value int64) string {
		if value <= 0 {
			return ""
		}
		return strconv.FormatInt(value, 10)
	}
	stamp := func(value *time.Time) string {
		if value == nil {
			return ""
		}
		return value.UTC().Format(time.RFC3339)
	}
	for _, row := range rows {
		_ = out.Write([]string{id(row.ProjectID), id(row.ApplicationID), csvSafe(row.ApplicationName),
			id(row.BusinessOwnerID), id(row.TechnicalOwnerID), string(row.Tier), string(row.State),
			id(row.CertifiedReleaseID), stamp(row.CertifiedAt), stamp(row.RecertifyBy), csvSafe(row.StateReason),
			strings.Join(row.DataClasses, ";"), csvSafe(row.IntendedUse), csvSafe(row.OutOfScopeUse), id(row.SuspensionID)})
	}
	out.Flush()
}

// csvSafe defuses a cell a spreadsheet would read as a formula.
func csvSafe(value string) string {
	if value != "" && strings.ContainsRune("=+-@\t\r", rune(value[0])) {
		return "'" + value
	}
	return value
}

// ---------------------------------------------------------------------------
// Change requests

// ListChangeRequests answers the agent's change requests, newest first.
func (h *Handler) ListChangeRequests(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	changes, err := h.store.ListChangeRequests(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	rows := make([]ChangeRequest, 0, len(changes))
	for _, change := range changes {
		rows = append(rows, changeRequest(change))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

// GetChangeRequest answers one change request with its approvals and the
// state of its release gate.
func (h *Handler) GetChangeRequest(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, changeID, ok := h.change(w, r)
	if !ok {
		return
	}
	change, err := h.store.GetChangeRequest(r.Context(), projectID, applicationID, changeID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	body := changeRequest(change)
	if change.GateID > 0 {
		if result, err := h.store.GateResult(r.Context(), projectID, change.GateID); err == nil {
			failures := result.Failures
			if failures == nil {
				failures = []string{}
			}
			body.Gate = &Gate{ID: change.GateID, State: string(result.State), Failures: failures, Overridden: result.Overridden}
		} else {
			slog.WarnContext(r.Context(), "agent lifecycle: read the release gate", "gate_id", change.GateID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// CreateChangeRequest creates a draft.
func (h *Handler) CreateChangeRequest(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind        string `json:"kind"`
		VersionID   int64  `json:"version_id"`
		VersionName string `json:"version_name"`
		Category    string `json:"category"`
		Summary     string `json:"summary"`
	}
	if !decode(w, r, &body, false) {
		return
	}
	if body.Kind == "" {
		body.Kind = string(agentlifecycle.KindRelease)
	}
	change, err := h.store.CreateChangeRequest(r.Context(), agentlifecycle.NewChangeRequest{
		ProjectID: projectID, ApplicationID: applicationID, Kind: agentlifecycle.ChangeKind(body.Kind),
		CandidateVersionID: body.VersionID, VersionName: body.VersionName, Category: body.Category,
		Summary: body.Summary, ProposedBy: actorID,
	})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusCreated, changeRequest(change))
}

// SubmitChangeRequest submits the caller's draft.
func (h *Handler) SubmitChangeRequest(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, changeID, ok := h.change(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	change, err := h.store.SubmitChangeRequest(r.Context(), projectID, applicationID, changeID, actorID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, changeRequest(change))
}

// ApproveChangeRequest records the caller's approval; the final one
// publishes or recertifies.
func (h *Handler) ApproveChangeRequest(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, changeID, ok := h.change(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &body, true) {
		return
	}
	governance, ok := h.holdsGovernance(w, r)
	if !ok {
		return
	}
	outcome, err := h.store.ApproveChangeRequest(r.Context(), repos.Approval{
		ProjectID: projectID, ApplicationID: applicationID, ChangeID: changeID,
		ApproverID: actorID, Governance: governance, Note: body.Note,
	})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	response := map[string]any{"change_request": changeRequest(outcome.Change), "complete": outcome.Complete}
	if outcome.Published != nil {
		response["published"] = map[string]any{
			"version_id": outcome.Published.PublishedVersionID, "release_id": positive(outcome.Published.ReleaseID),
			"version_name": outcome.Published.VersionName,
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// RejectChangeRequest rejects a submitted change request.
func (h *Handler) RejectChangeRequest(w http.ResponseWriter, r *http.Request) {
	h.closeChange(w, r, false)
}

// WithdrawChangeRequest withdraws a draft or submitted change request.
func (h *Handler) WithdrawChangeRequest(w http.ResponseWriter, r *http.Request) {
	h.closeChange(w, r, true)
}

func (h *Handler) closeChange(w http.ResponseWriter, r *http.Request, withdraw bool) {
	projectID, applicationID, changeID, ok := h.change(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body, true) {
		return
	}
	var (
		change agentlifecycle.ChangeRequest
		err    error
	)
	if withdraw {
		governance, ok := h.holdsGovernance(w, r)
		if !ok {
			return
		}
		change, err = h.store.WithdrawChangeRequest(r.Context(), projectID, applicationID, changeID, actorID, governance, body.Reason)
	} else {
		change, err = h.store.RejectChangeRequest(r.Context(), projectID, applicationID, changeID, actorID, body.Reason)
	}
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, changeRequest(change))
}

// OverrideGate accepts a failed release gate (the governance role).
func (h *Handler) OverrideGate(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, changeID, ok := h.change(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body, true) {
		return
	}
	governance, ok := h.holdsGovernance(w, r)
	if !ok {
		return
	}
	change, err := h.store.OverrideGate(r.Context(), projectID, applicationID, changeID, actorID, governance, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, changeRequest(change))
}

// ---------------------------------------------------------------------------
// Attestations

// ListAttestations answers the project's attestations, optionally of one
// subject (?subject_type=&subject_id=).
func (h *Handler) ListAttestations(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	subjectType := agentlifecycle.SubjectType(r.URL.Query().Get("subject_type"))
	subjectID := r.URL.Query().Get("subject_id")
	if (subjectType != "" && !subjectType.Valid()) || (subjectID != "" && !agentlifecycle.ValidSubjectID(subjectID)) {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "subject_type or subject_id is not valid."))
		return
	}
	rows, err := h.store.ListAttestations(r.Context(), projectID, subjectType, subjectID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	out := make([]Attestation, 0, len(rows))
	for _, row := range rows {
		out = append(out, attestation(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "rows": out})
}

// RecordAttestation records an attestation.
func (h *Handler) RecordAttestation(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind           string     `json:"kind"`
		CustomKind     string     `json:"custom_kind"`
		SubjectType    string     `json:"subject_type"`
		SubjectID      string     `json:"subject_id"`
		Attester       string     `json:"attester"`
		AttesterUserID int64      `json:"attester_user_id"`
		ValidUntil     *time.Time `json:"valid_until"`
		EvidenceURL    string     `json:"evidence_url"`
		Note           string     `json:"note"`
	}
	if !decode(w, r, &body, false) {
		return
	}
	if body.EvidenceURL != "" {
		parsed, err := url.Parse(body.EvidenceURL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
			writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "evidence_url is an http or https URL without credentials."))
			return
		}
	}
	if body.ValidUntil != nil && !body.ValidUntil.After(time.Now()) {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "valid_until is in the future."))
		return
	}
	recorded, err := h.store.RecordAttestation(r.Context(), agentlifecycle.Attestation{
		ProjectID: projectID, Kind: agentlifecycle.AttestationKind(body.Kind), CustomKind: body.CustomKind,
		SubjectType: agentlifecycle.SubjectType(body.SubjectType), SubjectID: body.SubjectID,
		Attester: body.Attester, AttesterUserID: body.AttesterUserID, ValidUntil: body.ValidUntil,
		EvidenceURL: body.EvidenceURL, Note: body.Note, RecordedBy: actorID,
	})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusCreated, attestation(recorded))
}

// RevokeAttestation revokes an attestation.
func (h *Handler) RevokeAttestation(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	attestationID, ok := pathID(w, r, "attestationID")
	if !ok {
		return
	}
	actorID, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body, true) {
		return
	}
	revoked, err := h.store.RevokeAttestation(r.Context(), projectID, attestationID, actorID, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, attestation(revoked))
}

// ---------------------------------------------------------------------------
// Helpers

func (h *Handler) project(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return 0, false
	}
	return pathID(w, r, "projectID")
}

func (h *Handler) application(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	projectID, ok := h.project(w, r)
	if !ok {
		return 0, 0, false
	}
	applicationID, ok := pathID(w, r, "applicationID")
	return projectID, applicationID, ok
}

func (h *Handler) change(w http.ResponseWriter, r *http.Request) (int64, int64, int64, bool) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return 0, 0, 0, false
	}
	changeID, ok := pathID(w, r, "changeRequestID")
	return projectID, applicationID, changeID, ok
}

func (h *Handler) holdsGovernance(w http.ResponseWriter, r *http.Request) (bool, bool) {
	if h.governance == nil {
		return false, true
	}
	holds, err := h.governance(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "agent lifecycle: resolve the governance role", "error", err)
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "could not resolve the caller's permissions")
		return false, false
	}
	return holds, true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil || value <= 0 {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The path's "+name+" is not a valid id."))
		return 0, false
	}
	return value, true
}

func signedIn(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, _ := auth.UserFromContext(r.Context())
	id, ok := principal.OwningUserID()
	if !ok || id <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return 0, false
	}
	return id, true
}

// decode reads a JSON body with no unknown fields; optional accepts an
// empty body.
func decode(w http.ResponseWriter, r *http.Request, target any, optional bool) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The request body is not readable or too large."))
		return false
	}
	if strings.TrimSpace(string(raw)) == "" {
		if optional {
			return true
		}
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "A JSON body is required."))
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		writeRefusal(w, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The request body is not valid JSON for this route."))
		return false
	}
	return true
}

func writeRefusal(w http.ResponseWriter, refusal *agentlifecycle.Refusal) {
	writeJSON(w, refusal.Status(), refusal.Body())
}

// writeError answers a typed refusal as it is, a publish refusal of the
// final approval as the publish route would, and anything else as a
// generic 500 (the cause is logged, never returned).
func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	if refusal, ok := agentlifecycle.AsRefusal(err); ok {
		writeRefusal(w, refusal)
		return
	}
	if refusal, ok := agentlifecycle.AsPublishRefusal(err); ok {
		writeJSON(w, refusal.Status, refusal.Body)
		return
	}
	slog.ErrorContext(ctx, "agent lifecycle", "error", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the agent lifecycle record")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
