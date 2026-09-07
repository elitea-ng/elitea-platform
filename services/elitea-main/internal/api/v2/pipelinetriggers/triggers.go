package pipelinetriggers

// The SETTINGS half of the trigger: create, rotate, reveal and revoke, from the
// pipeline's own "Triggers & schedules" tab.
//
//	GET    /pipeline_triggers/prompt_lib/{projectID}/{versionID}         read
//	POST   /pipeline_triggers/prompt_lib/{projectID}/{versionID}         create or rotate
//	GET    /pipeline_triggers/secret/prompt_lib/{projectID}/{versionID}  reveal
//	DELETE /pipeline_triggers/prompt_lib/{projectID}/{versionID}         revoke
//
// These four ARE session-authenticated and project-gated in the ordinary way;
// only the inbound call in inbound.go is not.
//
// # WHY THERE IS A SEPARATE "REVEAL" ROUTE
//
// The plain read is gated on `models.applications.version.details` and returns
// the trigger's STATE — that one exists, when it was made, when it was last
// used, whether it is revoked — and the URL WITHOUT the secret. Anyone who may
// look at the pipeline may see that.
//
// Handing back the live credential is a different act, so it is a different
// route with the WRITE permission on it. Folding it into the read would mean a
// person with view-only access could copy a working webhook URL for a pipeline
// they cannot edit — and would put a credential into the response of the call
// the settings tab makes on every open, where it would sit in the browser's
// query cache and in any HAR anyone ever exports.

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// The two entry points, named in the conversation and in the audit row.
const (
	OriginWebhook  = "Webhook"
	OriginSchedule = "Schedule"
)

// triggerView is what the settings tab reads.
//
// `secret` is present ONLY on the create/rotate response and on the reveal
// response. It is omitted — not blanked — everywhere else, so a client cannot
// tell an absent secret from an empty one and render a copy button that copies
// nothing.
type triggerView struct {
	Configured bool       `json:"configured"`
	TokenID    string     `json:"token_id,omitempty"`
	URL        string     `json:"url,omitempty"`
	Secret     string     `json:"secret,omitempty"`
	SecretURL  string     `json:"secret_url,omitempty"`
	CreatedBy  int64      `json:"created_by,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	RotatedAt  *time.Time `json:"rotated_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func triggerURL(projectID int64, tokenID string) string {
	return "/api/v2/pipeline_trigger/" + strconv.FormatInt(projectID, 10) + "/" + tokenID
}

func viewOf(projectID int64, trigger triggerRow) triggerView {
	view := triggerView{
		Configured: true,
		TokenID:    trigger.TokenID,
		URL:        triggerURL(projectID, trigger.TokenID),
		CreatedBy:  trigger.CreatedBy,
		CreatedAt:  &trigger.CreatedAt,
		RotatedAt:  trigger.RotatedAt,
		RevokedAt:  trigger.RevokedAt,
		LastUsedAt: trigger.LastUsedAt,
	}
	return view
}

// withSecret adds the credential-bearing fields. `secret_url` is the complete
// thing a person copies; `secret` is the same credential on its own, for the
// header form the tab also shows.
func withSecret(view triggerView, secret string) triggerView {
	view.Secret = secret
	view.SecretURL = view.URL + "?" + TriggerTokenQueryParam + "=" + secret
	return view
}

// GetTrigger reads the trigger's state. It answers 200 with
// `{"configured": false}` when there is none, rather than 404: "this pipeline
// has no trigger" is the normal state of almost every pipeline, and a 404 would
// make the settings tab render an error for it.
func (h *Handler) GetTrigger(w http.ResponseWriter, r *http.Request) {
	schema, projectID, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	trigger, err := h.triggerByVersion(r.Context(), schema, versionID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, triggerView{})
		return
	case err != nil:
		h.log().Error("pipelinetriggers: read trigger", "err", err)
		writeError(w, http.StatusInternalServerError, "the trigger could not be read")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(projectID, trigger))
}

// RevealTrigger returns the live credential. WRITE-gated; see the file header.
func (h *Handler) RevealTrigger(w http.ResponseWriter, r *http.Request) {
	schema, projectID, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	if h.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "the credential store is not available on this deployment")
		return
	}
	trigger, err := h.triggerByVersion(r.Context(), schema, versionID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, triggerView{})
		return
	case err != nil:
		h.log().Error("pipelinetriggers: read trigger", "err", err)
		writeError(w, http.StatusInternalServerError, "the trigger could not be read")
		return
	}
	secret, err := h.vault.LookupAdminHiddenSecret(r.Context(), trigger.SecretName)
	if err != nil {
		// The row exists and the vault has no value for it, or will not open.
		// Both are reported as "rotate it" rather than as a server error,
		// because rotation is the only repair a person can perform and it
		// always works.
		h.log().Error("pipelinetriggers: reveal trigger secret", "err", err)
		writeError(w, http.StatusConflict,
			"the stored credential for this trigger could not be read; rotate the trigger to issue a new one")
		return
	}
	writeJSON(w, http.StatusOK, withSecret(viewOf(projectID, trigger), secret))
}

// CreateOrRotateTrigger mints a credential.
//
// It is ONE operation for both acts on purpose. "Create" and "rotate" differ
// only in whether a row was already there, they have the same permission, the
// same effect on any holder of the previous secret, and the same response. Two
// routes would be two chances to implement the rotation half differently from
// the creation half.
//
// The ORDER of the two writes is the part that matters. The vault entry is
// written FIRST and the row second: if the row lands and the vault write then
// fails, a live trigger exists whose secret nobody can ever be shown, and the
// only repair is another rotation. With this order a failed row write leaves an
// orphan vault entry, which is invisible and harmless, and the caller sees an
// error and retries.
func (h *Handler) CreateOrRotateTrigger(w http.ResponseWriter, r *http.Request) {
	schema, projectID, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	if h.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "the credential store is not available on this deployment")
		return
	}
	actorID, ok := h.actorID(w, r)
	if !ok {
		return
	}
	target, err := h.resolveRunTarget(r.Context(), schema, versionID)
	switch {
	case errors.Is(err, ErrVersionNotRunnable):
		writeError(w, http.StatusNotFound, "no such pipeline version in this project")
		return
	case err != nil:
		h.log().Error("pipelinetriggers: resolve version", "err", err)
		writeError(w, http.StatusInternalServerError, "the pipeline version could not be read")
		return
	}

	previous, previousErr := h.triggerByVersion(r.Context(), schema, versionID)
	tokenID, secret, hash, err := newCredential()
	if err != nil {
		h.log().Error("pipelinetriggers: mint credential", "err", err)
		writeError(w, http.StatusInternalServerError, "a trigger credential could not be created")
		return
	}
	secretName := vaultSecretName(tokenID)
	if err := h.vault.StoreAdminHiddenSecret(r.Context(), secretName, secret); err != nil {
		h.log().Error("pipelinetriggers: store credential", "err", err)
		writeError(w, http.StatusInternalServerError, "the trigger credential could not be stored")
		return
	}
	trigger, err := h.upsertTrigger(r.Context(), schema,
		target.ApplicationID, versionID, actorID, tokenID, hash, secretName)
	if err != nil {
		h.log().Error("pipelinetriggers: write trigger", "err", err)
		writeError(w, http.StatusInternalServerError, "the trigger could not be saved")
		return
	}
	// The superseded secret is removed only AFTER the new row is committed. A
	// delete before the commit would leave a window in which the old secret is
	// gone and the new row is not there — every holder refused, with nothing to
	// roll back to.
	if previousErr == nil && previous.SecretName != secretName {
		if err := h.vault.DeleteAdminHiddenSecret(r.Context(), previous.SecretName); err != nil {
			h.log().Warn("pipelinetriggers: could not remove the superseded credential", "err", err)
		}
	}
	h.annotate(r, versionID, target.Name, projectID)
	writeJSON(w, http.StatusOK, withSecret(viewOf(projectID, trigger), secret))
}

// RevokeTrigger makes the credential unusable and keeps the row.
//
// The vault entry is deleted too, so the secret cannot be revealed again after
// revocation. The ROW stays: a revoked trigger is the evidence that answers
// "was this live last Tuesday", and the inbound path refuses any row whose
// `revoked_at` is set.
func (h *Handler) RevokeTrigger(w http.ResponseWriter, r *http.Request) {
	schema, projectID, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	actorID, ok := h.actorID(w, r)
	if !ok {
		return
	}
	trigger, err := h.revokeTrigger(r.Context(), schema, versionID, actorID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "this pipeline has no trigger")
		return
	case err != nil:
		h.log().Error("pipelinetriggers: revoke trigger", "err", err)
		writeError(w, http.StatusInternalServerError, "the trigger could not be revoked")
		return
	}
	if h.vault != nil {
		if err := h.vault.DeleteAdminHiddenSecret(r.Context(), trigger.SecretName); err != nil {
			h.log().Warn("pipelinetriggers: could not remove the revoked credential", "err", err)
		}
	}
	h.annotate(r, versionID, "", projectID)
	writeJSON(w, http.StatusOK, viewOf(projectID, trigger))
}

// routeTarget resolves and validates the two path segments every settings route
// carries. It answers the refusal itself and reports whether the caller may
// continue.
func (h *Handler) routeTarget(w http.ResponseWriter, r *http.Request) (string, int64, int64, bool) {
	if h.pool == nil {
		writeError(w, http.StatusServiceUnavailable, "pipeline triggers are not available on this deployment")
		return "", 0, 0, false
	}
	projectIDText := chi.URLParam(r, "projectID")
	schema, err := tenantschema.Quote(projectIDText)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return "", 0, 0, false
	}
	projectID, err := strconv.ParseInt(projectIDText, 10, 64)
	if err != nil || projectID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return "", 0, 0, false
	}
	versionID, err := strconv.ParseInt(chi.URLParam(r, "versionID"), 10, 64)
	if err != nil || versionID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid version id")
		return "", 0, 0, false
	}
	return schema, projectID, versionID, true
}

// actorID is the authenticated caller's user id.
//
// It is taken from the CONTEXT the Auth middleware wrote and never from the
// body. `created_by` and `author_id` decide what an unattended run may do, so a
// caller-supplied value there would be self-service impersonation.
func (h *Handler) actorID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication is required")
		return 0, false
	}
	raw := user.UserID
	if raw == "" {
		raw = user.ID
	}
	actorID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || actorID <= 0 {
		writeError(w, http.StatusUnauthorized, "this caller has no user identity")
		return 0, false
	}
	return actorID, true
}

// annotate gives the audit middleware the domain meaning of a settings write.
// Without it the row still exists and still names the route; with it the row
// names the pipeline.
func (h *Handler) annotate(r *http.Request, versionID int64, name string, projectID int64) {
	audit.Annotate(r.Context(), audit.Annotation{
		EntityType: "pipeline",
		EntityID:   audit.ID(versionID),
		EntityName: name,
		ProjectID:  audit.ID(projectID),
	})
}
