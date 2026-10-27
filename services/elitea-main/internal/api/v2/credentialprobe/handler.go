// Package credentialprobe serves the credential probe of ADR-0039 (issue
// #1120):
//
//	POST /api/v2/projects/{projectID}/credential_references/{id}:probe
//
// A probe checks that a governed credential works by running its provider
// adapter's probe operation server side. It needs the `probe` capability
// on an active grant for that credential reference; `use` alone is not
// enough (capabilities never imply one another, decision 8). The material
// stays with the Prober for the one call: the response names the outcome
// only and never holds a secret, a token or a provider body.
//
// router.go mounts the route only when RouterConfig.CredentialProbe is set,
// which the composition root does only with the governance flag on. The
// permission gate (models.credentials.governed.probe in {projectID}) lives
// in router.go.
package credentialprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// References reads a credential reference of a tenant project.
// *repos.CredentialGrants implements it.
type References interface {
	CredentialReference(ctx context.Context, tenantProjectID int64, id string) (credentialgrant.CredentialReference, error)
}

// Grants lists the active grants of a credential reference that list
// capability. The handler checks every returned grant again.
// *repos.CredentialGrants implements it.
type Grants interface {
	ActiveGrantsWithCapability(ctx context.Context, tenantProjectID int64, referenceID string, capability credentialgrant.Capability) ([]credentialgrant.CredentialUseGrant, error)
}

// Audit records one probe event (securityaudit.TypeProbe): allowed with
// its outcome, or denied for want of a grant. An error withholds the
// answer. *repos.CredentialGrants implements it.
type Audit interface {
	RecordProbe(ctx context.Context, event securityaudit.SecurityEvent) error
}

// Descriptors answers a provider's adapter descriptor.
// *credentialadapters.Registry implements it.
type Descriptors interface {
	Descriptor(providerType string) (credentialadapters.AdapterDescriptor, bool)
}

// Config is the handler's dependencies. A nil Prober is the fail-closed
// credentialadapters.UnavailableProber.
type Config struct {
	References  References
	Grants      Grants
	Audit       Audit
	Descriptors Descriptors
	Prober      credentialadapters.Prober
	Now         func() time.Time
}

// Handler serves the probe route.
type Handler struct {
	references  References
	grants      Grants
	audit       Audit
	descriptors Descriptors
	prober      credentialadapters.Prober
	now         func() time.Time
}

// New builds the handler. Missing readers are refused.
func New(cfg Config) (*Handler, error) {
	if cfg.References == nil || cfg.Grants == nil || cfg.Audit == nil || cfg.Descriptors == nil {
		return nil, errors.New("credential probe: references, grants, audit and descriptors are required")
	}
	h := &Handler{
		references: cfg.References, grants: cfg.Grants, audit: cfg.Audit, descriptors: cfg.Descriptors,
		prober: cfg.Prober, now: cfg.Now,
	}
	if h.prober == nil {
		h.prober = credentialadapters.UnavailableProber{}
	}
	if h.now == nil {
		h.now = func() time.Time { return time.Now().UTC() }
	}
	return h, nil
}

// outcomeMessages is the only text a probe answer carries: fixed per
// outcome, so no provider or error text can reach the caller.
var outcomeMessages = map[credentialadapters.ProbeOutcome]string{
	credentialadapters.ProbeSucceeded:   "the provider accepted the credential",
	credentialadapters.ProbeRejected:    "the provider refused the credential",
	credentialadapters.ProbeUnreachable: "the provider target did not answer",
	credentialadapters.ProbeUnavailable: "probe not available for provider",
}

type probeView struct {
	CredentialReferenceID string  `json:"credential_reference_id"`
	ProviderType          string  `json:"provider_type"`
	Operation             *string `json:"operation"`
	Outcome               string  `json:"outcome"`
	Message               string  `json:"message"`
	ProviderStatus        *int    `json:"provider_status"`
	CheckedAt             string  `json:"checked_at"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// Probe serves POST …/credential_references/{id}:probe.
func (h *Handler) Probe(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if strings.TrimSpace(user.UserID) == "" {
		writeError(w, http.StatusForbidden, "the caller has no resolved user id")
		return
	}
	rawProject := chi.URLParam(r, "projectID")
	tenant, err := strconv.ParseInt(rawProject, 10, 64)
	if err != nil || tenant <= 0 || strconv.FormatInt(tenant, 10) != rawProject {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	referenceID := strings.TrimSuffix(chi.URLParam(r, "id"), ":probe")
	ctx := r.Context()

	ref, err := h.references.CredentialReference(ctx, tenant, referenceID)
	if errors.Is(err, credentialgrant.ErrReferenceNotFound) {
		writeError(w, http.StatusNotFound, credentialgrant.ErrReferenceNotFound.Error())
		return
	}
	if err != nil || ref.TenantProjectID != tenant || ref.ID != referenceID {
		writeError(w, http.StatusInternalServerError, "credential probe: internal error")
		return
	}

	grants, err := h.grants.ActiveGrantsWithCapability(ctx, tenant, ref.ID, credentialgrant.CapProbe)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "credential probe: internal error")
		return
	}
	now := h.now()
	grant, granted := probeGrant(grants, ref, tenant, now)
	if !granted {
		if h.record(ctx, w, probeEvent(tenant, user.UserID, ref.ID, "", now, securityaudit.EffectDeny, "probe.no_grant")) {
			writeError(w, http.StatusForbidden, "no active grant with the probe capability covers this credential reference")
		}
		return
	}
	if !ref.State.Usable() {
		writeError(w, http.StatusConflict, "the credential reference is not active")
		return
	}
	descriptor, ok := h.descriptors.Descriptor(ref.ProviderType)
	if !ok || !descriptor.Accepts(ref.Class) {
		writeError(w, http.StatusConflict, "no provider adapter serves this credential reference")
		return
	}

	view := probeView{CredentialReferenceID: ref.ID, ProviderType: ref.ProviderType}
	result := credentialadapters.ProbeResult{Outcome: credentialadapters.ProbeUnavailable}
	if descriptor.Probe != nil {
		operation := string(descriptor.Probe.Operation)
		view.Operation = &operation
		result, err = h.prober.Probe(ctx, credentialadapters.ProbeRequest{
			TenantProjectID: tenant, Reference: ref, Operation: descriptor.Probe.Operation, Descriptor: descriptor,
		})
		if err != nil || !result.Outcome.Valid() {
			writeError(w, http.StatusBadGateway, "credential probe: the probe failed")
			return
		}
	}
	view.Outcome = string(result.Outcome)
	view.Message = outcomeMessages[result.Outcome]
	if result.ProviderStatus >= 100 && result.ProviderStatus <= 599 {
		status := result.ProviderStatus
		view.ProviderStatus = &status
	}
	checked := h.now().UTC()
	view.CheckedAt = checked.Format(time.RFC3339Nano)
	if h.record(ctx, w, probeEvent(tenant, user.UserID, ref.ID, grant.ID, checked, securityaudit.EffectAllow,
		"probe."+string(result.Outcome))) {
		writeJSON(w, http.StatusOK, view)
	}
}

// record appends the audit event; on failure it answers 500 and false.
func (h *Handler) record(ctx context.Context, w http.ResponseWriter, event securityaudit.SecurityEvent) bool {
	if err := h.audit.RecordProbe(ctx, event); err != nil {
		writeError(w, http.StatusInternalServerError, "credential probe: internal error")
		return false
	}
	return true
}

func probeEvent(tenant int64, userID, referenceID, grantID string, at time.Time, effect securityaudit.Effect, reason string) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{
		Type:            securityaudit.TypeProbe,
		Time:            at,
		TenantProjectID: tenant,
		Action:          "credential.probe",
		Effect:          effect,
		Reasons:         []string{reason},
		PrincipalRef:    "user:" + userID,
		GrantRef:        grantID,
		ResourceType:    "credential_reference",
		ResourceID:      referenceID,
	}
}

// probeGrant finds one grant that is active at now, lists `probe` and is
// for exactly this reference and its target.
func probeGrant(grants []credentialgrant.CredentialUseGrant, ref credentialgrant.CredentialReference, tenant int64, now time.Time) (credentialgrant.CredentialUseGrant, bool) {
	for _, g := range grants {
		if g.TenantProjectID == tenant && g.CredentialReferenceID == ref.ID &&
			g.TargetBindingID == ref.TargetBindingID && g.ActiveAt(now) && g.Allows(credentialgrant.CapProbe) {
			return g, true
		}
	}
	return credentialgrant.CredentialUseGrant{}, false
}
