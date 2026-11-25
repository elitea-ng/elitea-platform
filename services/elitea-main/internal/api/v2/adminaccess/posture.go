package adminaccess

// The access posture (ADR-0059 decision 8, "Posture"; story R2.14a, issue
// 2528):
//
//	GET /api/v2/admin/access/posture
//
// The checks of registry.PostureChecks, each with its id, severity, state,
// text key, English text and facts, and the counts by severity of the
// checks that do not pass. Only `fail` counts as a failure. For a holder of
// access.roles.view (or the names it narrows), as the other Access reads:
// the facts name people (the holders of `system` and of `super_admin`),
// which decision 6 allows to that permission only. A scoped token that
// holds it reads the list (the action table says `token: allowed`); the
// posture card of ADR-0058 E5.3 reads the same route.
//
// No cache: each check is one query or one configuration read.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// PosturePath is the route, relative to the /api/v2/admin group.
const PosturePath = "/access/posture"

// PostureHandler serves the route.
type PostureHandler struct {
	sources registry.PostureSources
	checks  []registry.PostureCheck
}

// NewPostureHandler returns the handler over sources and the registry's
// checks. A source without a database answers 503.
func NewPostureHandler(sources registry.PostureSources) *PostureHandler {
	return &PostureHandler{sources: sources, checks: registry.PostureChecks()}
}

// WithChecks replaces the checks (tests).
func (h *PostureHandler) WithChecks(checks []registry.PostureCheck) *PostureHandler {
	h.checks = checks
	return h
}

// Register mounts the route on the /api/v2/admin router, behind middlewares
// (the route gate on access.roles.view).
func (h *PostureHandler) Register(r chi.Router, middlewares ...func(http.Handler) http.Handler) {
	r.With(middlewares...).Get(PosturePath, h.Posture)
}

/* ── the wire shape (v2.yaml AccessPosture) ────────────────────────────── */

type postureBody struct {
	Checks []postureCheckBody `json:"checks"`
	// Failures counts the `fail` checks that do not pass; Warnings and
	// Notices the `warn` and `info` ones. Only Failures is a failure.
	Failures  int       `json:"failures"`
	Warnings  int       `json:"warnings"`
	Notices   int       `json:"notices"`
	CheckedAt time.Time `json:"checked_at"`
}

type postureCheckBody struct {
	ID       string           `json:"id"`
	Severity string           `json:"severity"`
	State    string           `json:"state"`
	TextKey  string           `json:"text_key"`
	Text     string           `json:"text"`
	Facts    postureFactsBody `json:"facts"`
}

type postureFactsBody struct {
	Count  int                 `json:"count"`
	Roles  []string            `json:"roles"`
	People []posturePersonBody `json:"people"`
}

type posturePersonBody struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Posture answers the list.
func (h *PostureHandler) Posture(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFromContext(r.Context()); !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.sources.DB == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the access posture is not configured")
		return
	}
	sources := h.sources
	if sources.Now == nil {
		sources.Now = time.Now
	}
	checkedAt := sources.Now().UTC()
	sources.Now = func() time.Time { return checkedAt }
	results, err := registry.RunPosture(r.Context(), sources, h.checks)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		slog.ErrorContext(r.Context(), "admin access: the posture could not be read", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "the access posture could not be read")
		return
	}
	body := postureBody{Checks: make([]postureCheckBody, 0, len(results)), CheckedAt: checkedAt}
	for _, result := range results {
		people := make([]posturePersonBody, 0, len(result.Facts.People))
		for _, person := range result.Facts.People {
			people = append(people, posturePersonBody{ID: person.ID, Name: person.Name, Email: person.Email})
		}
		body.Checks = append(body.Checks, postureCheckBody{
			ID:       result.Check.ID,
			Severity: string(result.Check.Severity),
			State:    result.State(),
			TextKey:  result.Check.TextKey(),
			Text:     result.Text,
			Facts:    postureFactsBody{Count: result.Facts.Count, Roles: result.Facts.Roles, People: people},
		})
		if result.Pass {
			continue
		}
		switch result.Check.Severity {
		case registry.PostureFail:
			body.Failures++
		case registry.PostureWarn:
			body.Warnings++
		case registry.PostureInfo:
			body.Notices++
		}
	}
	writeJSON(w, body)
}
