// Package skilltrust serves a project's trust in the text of a skill written in
// another project (ADR-0051 amendment 1, A1.6; skills epic 2169 story S1).
//
// A skill forked from another project is third-party text. On the chat surfaces
// the worker shows it as quoted untrusted data until a person in THIS project has
// read that exact text and trusted it. The unit of trust is one skill and one
// revision (the SHA-256 digest of its instructions); any change to the text is a
// new revision and is untrusted again. A trust record grants the skill nothing: it
// only decides how the text is framed.
//
// Reading needs the permission that reads a skill. Trusting and revoking need the
// permission that edits one, and each change is an audit row.
package skilltrust

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Record is one trust decision, kept after a revoke as history.
type Record struct {
	Revision           string     `json:"revision"`
	PublisherProjectID *int64     `json:"publisher_project_id,omitempty"`
	TrustedBy          *int64     `json:"trusted_by,omitempty"`
	TrustedAt          time.Time  `json:"trusted_at"`
	RevokedBy          *int64     `json:"revoked_by,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	// Source is `user`, or `migration` for a fork that existed before trust did.
	Source string `json:"source"`
}

// Status is what the project trusts of one skill's current text.
type Status struct {
	SkillID int64  `json:"skill_id"`
	Name    string `json:"name"`
	// Foreign is true when the skill was written in another project. A skill that
	// is not foreign needs no trust.
	Foreign            bool   `json:"foreign"`
	PublisherProjectID *int64 `json:"publisher_project_id,omitempty"`
	// Revision is the digest of the skill's current base text: what a trust
	// request must name.
	Revision string `json:"revision"`
	// State is `not_required` (the skill is not foreign), `trusted` or `untrusted`.
	State string `json:"state"`
	// PublisherTrusted is true when the state is `trusted` because an
	// administrator trusts the publisher project (the skill-publishing
	// section's trusted publishers), not because this project reviewed the text.
	PublisherTrusted bool     `json:"publisher_trusted,omitempty"`
	History          []Record `json:"history"`
}

// Repository is the storage the handler needs, for one project.
type Repository interface {
	Get(ctx context.Context, projectID string, skillID int64) (Status, error)
	// Trust records the project's trust in revision, which must be the skill's
	// current base revision. It is a no-op when that revision is trusted already.
	Trust(ctx context.Context, projectID string, skillID int64, revision string) (status Status, changed bool, err error)
	// Revoke revokes the active trust of the skill's revisions. It reports how many
	// it revoked.
	Revoke(ctx context.Context, projectID string, skillID int64) (status Status, revoked int, err error)
}

// Handler serves the three routes.
type Handler struct{ repo Repository }

func NewHandler(repo Repository) *Handler { return &Handler{repo: repo} }

func skillID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "skillID"), 10, 64)
	if err != nil || id <= 0 || id > 1<<31-1 {
		return 0, apierr.BadRequest("skill id must be a positive integer")
	}
	return id, nil
}

// Get serves GET: the trust state of the skill's current text, and its history.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := skillID(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	status, err := h.repo.Get(r.Context(), chi.URLParam(r, "projectID"), id)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// Trust serves POST: `{"revision": "<digest>"}` trusts that exact text. A
// revision that is not the skill's current one is refused, so a review of old
// text cannot trust new text.
func (h *Handler) Trust(w http.ResponseWriter, r *http.Request) {
	id, err := skillID(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10+1))
	if err != nil || len(raw) > 4<<10 {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var body struct {
		Revision string `json:"revision"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() || len(body.Revision) != 64 {
		apierr.Write(w, apierr.BadRequest("the body must be {\"revision\": the 64-character revision of the skill's current text}"))
		return
	}
	status, changed, err := h.repo.Trust(r.Context(), chi.URLParam(r, "projectID"), id, body.Revision)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if changed {
		audit.Annotate(r.Context(), audit.Annotation{
			Action: "Trusted version " + body.Revision[:12] + " of skill " + strconv.Quote(status.Name), EntityType: "skill", EntityID: audit.ID(id),
		})
	}
	writeJSON(w, http.StatusOK, status)
}

// Revoke serves DELETE: the project stops trusting the skill's text.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	id, err := skillID(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	status, revoked, err := h.repo.Revoke(r.Context(), chi.URLParam(r, "projectID"), id)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if revoked > 0 {
		audit.Annotate(r.Context(), audit.Annotation{
			Action: "Revoked trust in skill " + strconv.Quote(status.Name), EntityType: "skill", EntityID: audit.ID(id),
		})
	}
	writeJSON(w, http.StatusOK, status)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
