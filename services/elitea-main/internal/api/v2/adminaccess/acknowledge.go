package adminaccess

// The acknowledgement of an elevation nobody independent could review
// (ADR-0059 decision 5, "Review after use", "Notes"; story R2.9c, issue
// 2604): POST /api/v2/admin/access/elevations/{elevationID}/acknowledge.
//
// A standing global administrator (admin.auth.users.super_admin; the action
// row is `elevation: deny`, so an elevation never supplies it, and
// `token: never`) acknowledges an unreviewed_no_reviewer elevation with a
// note. The note is stored as the acknowledgement, once per elevation; the
// elevation's state does not change, so the record keeps saying that no
// independent review happened. The posture check (R2.14b) reads it.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// ElevationAcknowledgePath is the route, relative to the /api/v2/admin group.
const ElevationAcknowledgePath = "/access/elevations/{elevationID}/acknowledge"

// The stable codes of the acknowledgement refusals (v2.yaml ElevationRefusal).
const (
	CodeElevationNotAcknowledgeable = "elevation_not_acknowledgeable"
	CodeElevationAcknowledged       = "elevation_acknowledged"
)

// AcknowledgeStore writes the acknowledgement. *PostgresReviews is the
// production one.
type AcknowledgeStore interface {
	Acknowledge(ctx context.Context, note registry.ElevationNote) (registry.NoteResult, error)
}

// Acknowledge implements AcknowledgeStore.
func (p *PostgresReviews) Acknowledge(ctx context.Context, note registry.ElevationNote) (registry.NoteResult, error) {
	var result registry.NoteResult
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		result, err = registry.AddElevationNote(ctx, tx, p.appender, note)
		return err
	})
	return result, err
}

// AcknowledgeHandler serves the route.
type AcknowledgeHandler struct {
	store AcknowledgeStore
	now   func() time.Time
}

// NewAcknowledgeHandler returns the handler. A nil store answers 503.
func NewAcknowledgeHandler(store AcknowledgeStore) *AcknowledgeHandler {
	return &AcknowledgeHandler{store: store, now: time.Now}
}

// Register mounts the route behind gate, the standing super_admin
// permission. A nil gate refuses every request.
func (h *AcknowledgeHandler) Register(r chi.Router, gate func(http.Handler) http.Handler) {
	if gate == nil {
		gate = func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeElevationRefusal(w, http.StatusForbidden, CodeElevationCredentialRefused,
					"the acknowledgement is not configured")
			})
		}
	}
	r.With(RequireElevationCredential, gate).Post(ElevationAcknowledgePath, h.Acknowledge)
}

type acknowledgeBody struct {
	Note string `json:"note"`
}

type acknowledgedView struct {
	ElevationID  string `json:"elevation_id"`
	NoteID       string `json:"note_id"`
	State        string `json:"state"`
	Acknowledged bool   `json:"acknowledged"`
}

// Acknowledge writes the acknowledgement.
func (h *AcknowledgeHandler) Acknowledge(w http.ResponseWriter, r *http.Request) {
	reviews := &ReviewsHandler{}
	actor, ok := acknowledgeActor(w, r)
	if !ok {
		return
	}
	if h.store == nil {
		writeElevationRefusal(w, http.StatusServiceUnavailable, CodeElevationRequestInvalid,
			"the elevation acknowledgement is not configured")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "elevationID"))
	if err != nil {
		writeElevationRefusal(w, http.StatusNotFound, CodeElevationNotFound, "no such elevation")
		return
	}
	var body acknowledgeBody
	if !decodeElevationBody(w, r, &body) {
		return
	}
	note := strings.TrimSpace(body.Note)
	if note == "" {
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid,
			"an acknowledgement needs a note: what is known about the elevation nobody could review")
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	at := h.now().UTC()
	result, err := h.store.Acknowledge(r.Context(), registry.ElevationNote{
		ID:                id,
		Text:              note,
		Source:            registry.NoteSourceConsole,
		AuthorUserID:      actor,
		Acknowledge:       true,
		At:                at,
		AuthenticationRef: browserSessionRef(principal.BrowserSessionRef),
		CorrelationID:     correlationID(r.Context()),
	})
	switch {
	case err == nil:
	case errors.Is(err, registry.ErrElevationNotAcknowledgeable):
		writeElevationRefusal(w, http.StatusConflict, CodeElevationNotAcknowledgeable,
			"only an elevation nobody independent could review by its deadline (unreviewed_no_reviewer) is acknowledged")
		return
	case errors.Is(err, registry.ErrElevationAcknowledged):
		writeElevationRefusal(w, http.StatusConflict, CodeElevationAcknowledged,
			"the elevation was acknowledged already")
		return
	default:
		reviews.writeReviewError(w, r, err)
		return
	}
	writeElevationJSON(w, http.StatusOK, acknowledgedView{
		ElevationID: id.String(), NoteID: result.NoteID.String(),
		State: result.Elevation.State(at), Acknowledged: true,
	})
}

// principalActor is the browser-session caller of an acknowledgement: a
// token, a native session or a run token is refused.
func acknowledgeActor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeElevationRefusal(w, http.StatusUnauthorized, CodeElevationCredentialRefused, "authentication required")
		return 0, false
	}
	if !ElevationCredentialAdmitted(principal.Credential()) {
		writeElevationRefusal(w, http.StatusForbidden, CodeElevationCredentialRefused,
			"the acknowledgement takes a browser session only")
		return 0, false
	}
	actor, owned := principal.OwningUserID()
	if !owned || actor <= 0 {
		writeElevationRefusal(w, http.StatusUnauthorized, CodeElevationCredentialRefused, "authentication required")
		return 0, false
	}
	return actor, true
}

var _ AcknowledgeStore = (*PostgresReviews)(nil)
