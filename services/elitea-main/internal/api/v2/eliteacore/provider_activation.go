package eliteacore

// ACTIVATION on the administration surface (ADR-0012 phase P3, migration 0109).
//
//	POST /elitea_core/register_descriptor/{projectID}/activate?provider_name=
//	POST /elitea_core/register_descriptor/{projectID}/deactivate?provider_name=
//
// WHAT CHANGED SINCE provider_admission.go's HEADER WAS WRITTEN. That file
// records, correctly for the time, that this deployment "can RECORD a provider
// and SHOW it, and it still cannot ACTIVATE one, because activation requires a
// policy overlay nothing here can issue". Migration 0109 issues one. The limit
// it describes is lifted, and the constraint that expressed it is NOT: 0107's
// `status <> 'active' OR overlay_revision IS NOT NULL` still stands, and the
// foreign key 0109 adds is what makes the reference in it true. Activation goes
// through a reviewed overlay row or it does not happen.
//
// THREE DECISIONS ON THIS SURFACE, each against a shape that would look fine:
//
//   - A SEPARATE PERMISSION. `provider_hub.descriptor.activate`, not
//     `.register`. A facade's registrar files a registration on every boot, so
//     `.register` is a permission a deployment hands out freely; activation is
//     the switch that lets agents call the provider. Sharing one string would
//     make the operator who may record a descriptor automatically the operator
//     who may put it in force.
//
//   - expected_digest IS REQUIRED, and a mismatch is 422 rather than a
//     silently-retargeted activation. The overlay is a statement about the
//     manifest the operator read. If the provider republished in between, the
//     revision cites bytes nobody reviewed.
//
//   - A REASON IS REQUIRED. The revision row's `reason` is NOT NULL and
//     non-empty by constraint, and an activation whose reason was invented by
//     the server ("activated through the administration surface") tells a later
//     reader nothing about why. Registration and revoke can default one because
//     the fact itself is the information; a decision to put something in force
//     cannot.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

// maxOverlayBytes bounds a posted overlay. Far smaller than a descriptor: v1 is
// a handful of reviewed facts, not a document, and a limit that admits a
// megabyte would make this route a way to write arbitrary volume into a JSONB
// column.
const maxOverlayBytes = 64 << 10

// activateRequest is the posted body.
//
// `overlay` is FREE JSON in v1, deliberately. Migration 0109 pins no schema for
// it because the schema lives in libs/jsonschema and is still being written,
// and a Go struct with the fields as they stand today would be a second copy
// that drifts. The three named fields below are the ones a reviewer has
// something to say about at this stage; anything else the caller sends is kept
// verbatim in the stored body rather than dropped, because dropping a reviewed
// fact silently is the worst available behaviour.
type activateRequest struct {
	ExpectedDigest string          `json:"expected_digest"`
	Reason         string          `json:"reason"`
	RevisionID     string          `json:"admitted_provider_revision"`
	Overlay        json.RawMessage `json:"overlay"`
}

type activateResponse struct {
	AdmittedProviderRevision string `json:"admitted_provider_revision"`
	OverlayRevision          string `json:"overlay_revision"`
	PublishedManifestDigest  string `json:"published_manifest_digest"`
	Status                   string `json:"status"`
	Reason                   string `json:"reason"`
}

// ActivateDescriptor serves `POST …/register_descriptor/{projectID}/activate`.
func (h *Handler) ActivateDescriptor(w http.ResponseWriter, r *http.Request) {
	// 501 covers BOTH missing migrations. 0107 without 0109 is a deployment
	// that can record and still cannot activate — exactly the state
	// provider_admission.go described — and answering 500 to it would tell an
	// operator nothing about the migration behind it.
	if h.pool == nil || !h.admissionPlanePresent(r.Context()) ||
		!providerhub.OverlayPlanePresent(r.Context(), h.pool) {
		writeServiceDescriptorsRefusal(w)
		return
	}
	projectID, err := projectIDFromPath(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	provider, err := providerNameFromQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req, err := decodeActivateRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	activated, err := providerhub.Activate(r.Context(), h.pool, providerhub.ActivateRequest{
		ProjectID:      projectID,
		ProviderID:     provider,
		RevisionID:     req.RevisionID,
		ExpectedDigest: req.ExpectedDigest,
		Body:           req.Overlay,
		Reason:         req.Reason,
		Actor:          providerActor(r),
	})
	if err != nil {
		writeActivationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, activateResponse{
		AdmittedProviderRevision: activated.RevisionID,
		OverlayRevision:          activated.OverlayRevision,
		PublishedManifestDigest:  activated.ManifestDigest,
		Status:                   activated.Status,
		Reason:                   activated.Reason,
	})
}

// DeactivateDescriptor serves `POST …/register_descriptor/{projectID}/deactivate`.
//
// It takes no digest. Turning something OFF is safe whatever bytes it cites —
// requiring a compare-and-swap here would mean an operator whose provider
// republished under them could not stop it, which is the one moment they most
// want to.
func (h *Handler) DeactivateDescriptor(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil || !h.admissionPlanePresent(r.Context()) ||
		!providerhub.OverlayPlanePresent(r.Context(), h.pool) {
		writeServiceDescriptorsRefusal(w)
		return
	}
	projectID, err := projectIDFromPath(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	provider, err := providerNameFromQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if reason == "" {
		// Defaulted, unlike an activation's. Stopping a provider is a complete
		// fact on its own; starting one is a judgement, and a judgement with a
		// server-invented reason is worse than none.
		reason = providerhub.InactiveReason
	}

	if err := providerhub.Deactivate(r.Context(), h.pool, projectID, provider, reason); err != nil {
		writeActivationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "inactive", "reason": reason})
}

// providerNameFromQuery reads the provider the request acts on.
//
// A query parameter rather than a path segment, matching the revoke verb this
// sits beside: provider ids are free text with no path-safety guarantee, and
// the DELETE route already established the spelling.
func providerNameFromQuery(r *http.Request) (string, error) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider_name"))
	if provider == "" {
		return "", errors.New("provider_name is required, so that the request names what it acts on")
	}
	return provider, nil
}

func decodeActivateRequest(r *http.Request) (activateRequest, error) {
	var req activateRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOverlayBytes+1))
	if err != nil {
		return req, errors.New("the request body could not be read")
	}
	if len(body) > maxOverlayBytes {
		return req, fmt.Errorf("the overlay exceeds the %d byte limit", maxOverlayBytes)
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return req, errors.New("the request body is not valid JSON")
		}
	}
	req.ExpectedDigest = strings.TrimSpace(req.ExpectedDigest)
	req.Reason = strings.TrimSpace(req.Reason)
	req.RevisionID = strings.TrimSpace(req.RevisionID)
	if req.Reason == "" {
		return req, errors.New("reason is required: an activation is a decision, and a " +
			"decision with no recorded reason cannot be reviewed later")
	}
	if len(req.ExpectedDigest) != 64 {
		return req, errors.New("expected_digest must be the sha256 of the manifest being " +
			"activated, so that a provider that republished since the review is refused")
	}
	// Checked here as well as in the store, so a caller learns their overlay is
	// not an object rather than meeting a database type error.
	if _, err := providerhub.CanonicalOverlay(req.Overlay); err != nil {
		return req, err
	}
	return req, nil
}

// writeActivationError maps the store's typed outcomes onto status codes.
//
// FOUR CODES, NOT ONE. Each names a different thing for the operator to do:
// 404 fix the name (or wait for the facade to register), 409 look at what state
// the row is really in, 422 re-read the manifest that changed, 501 apply the
// migration.
func writeActivationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providerhub.ErrOverlayPlaneAbsent):
		writeServiceDescriptorsRefusal(w)
	case errors.Is(err, providerhub.ErrProviderNotRegistered):
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such provider is registered for this project",
		})
	case errors.Is(err, providerhub.ErrNoAdmittedRevision):
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this provider has no admitted revision to activate",
		})
	case errors.Is(err, providerhub.ErrManifestDigestMismatch):
		// 422, not 409. The request was well formed and the state is fine; what
		// is wrong is the CONTENT of the assertion the caller made about it.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "the manifest changed since it was reviewed: " + err.Error(),
		})
	case errors.Is(err, providerhub.ErrRevisionNotInactive),
		errors.Is(err, providerhub.ErrAnotherRevisionActive),
		errors.Is(err, providerhub.ErrNoActiveRevision):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the provider's admission could not be changed",
		})
	}
}

// admissionPostureName is the posture this deployment runs, for the listing.
//
// A BAD SPELLING FALLS BACK TO `record` HERE, and only here, because the
// composition root already refuses to start on one — facade.AdmissionPostureFromEnv
// returns an error and main.go does not swallow it. So this branch is
// unreachable in a running server, and a listing that answered 500 for it would
// be a second, weaker copy of a startup check.
func admissionPostureName() string {
	posture, err := facade.AdmissionPostureFromEnv(nil)
	if err != nil {
		return string(facade.AdmissionRecord)
	}
	return string(posture)
}
