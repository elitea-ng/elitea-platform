package conversations

// Participant candidates (client contract 1.1): the people a caller may add
// to a conversation.
//
// The web's "add people" picker reads `userList` (GET /admin/users/default/
// {project}): an ADMIN-tagged operation that answers every member of the
// project in one unpaginated page, and the web filters it in the browser. A
// native client cannot depend on that — it is outside the client contract, it
// grows with the project, and its permission is not the one that governs
// adding a participant. This read answers the same population (project
// members, platform system users excluded) behind the permission that DOES
// govern the add (`models.chat.participants.create`) and the conversation's
// own access check, filtered and paged on the server.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	defaultCandidateLimit = 20
	maxCandidateLimit     = 100
	// maxCandidateQueryRunes bounds `q`. A name search longer than this is
	// not a name search, and it keeps an ILIKE pattern small.
	maxCandidateQueryRunes = 100
)

// ParticipantCandidate is one project member as a candidate participant.
type ParticipantCandidate struct {
	UserID int64  `json:"user_id"`
	Name   string `json:"name"`
	Email  string `json:"email,omitempty"`
	// AlreadyParticipant is true when the user is a participant of the
	// conversation the candidates were asked for.
	AlreadyParticipant bool `json:"already_participant"`
}

// CandidateCursor is the keyset position after the last row of a page: the
// row's sort key and its user id. Opaque to clients.
type CandidateCursor struct {
	SortKey string `json:"k"`
	UserID  int64  `json:"i"`
}

// ParticipantCandidateStore reads project members. Implemented over
// auth_core__project_user_role/auth_core__user by internal/infra/db/repos
// (declared here for the same import-cycle reason AttachmentStore is).
type ParticipantCandidateStore interface {
	// ListProjectMemberCandidates answers up to limit members of projectID
	// whose name or email contains query (case-insensitive; empty matches
	// everyone), ordered by their sort key then id, strictly after `after`
	// when it is non-nil. The sort key of each row is returned beside it so
	// the caller can build the next cursor.
	ListProjectMemberCandidates(ctx context.Context, projectID int64, query string, after *CandidateCursor, limit int) ([]ParticipantCandidate, []string, error)
}

// WithParticipantCandidates wires the candidate read. Left nil, the route
// answers 501: a composition without a database has no members to list.
func (h *Handler) WithParticipantCandidates(store ParticipantCandidateStore) *Handler {
	h.candidates = store
	return h
}

type participantCandidatesPage struct {
	Rows       []ParticipantCandidate `json:"rows"`
	NextCursor *string                `json:"next_cursor"`
	HasMore    bool                   `json:"has_more"`
}

// ListParticipantCandidates answers GET
// /participant_candidates/prompt_lib/{projectID}/{conversationID}.
func (h *Handler) ListParticipantCandidates(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	if h.candidates == nil {
		apierr.Write(w, apierr.NotImplemented("participant candidates are not available on this deployment"))
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || projectID <= 0 {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}
	query := r.URL.Query()
	if kind := query.Get("type"); kind != "" && kind != "user" {
		apierr.Write(w, apierr.BadRequest("type must be user"))
		return
	}
	needle := strings.TrimSpace(query.Get("q"))
	if utf8.RuneCountInString(needle) > maxCandidateQueryRunes || strings.ContainsRune(needle, 0) {
		apierr.Write(w, apierr.BadRequest("q is too long"))
		return
	}
	limit := defaultCandidateLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > maxCandidateLimit {
			apierr.Write(w, apierr.BadRequest("limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	var after *CandidateCursor
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := decodeCandidateCursor(raw)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("invalid cursor"))
			return
		}
		after = &decoded
	}

	// One row past the page says whether another page exists.
	rows, keys, err := h.candidates.ListProjectMemberCandidates(r.Context(), projectID, needle, after, limit+1)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if len(keys) != len(rows) {
		apierr.Write(w, apierr.Internal("participant candidates: sort keys do not match rows"))
		return
	}
	page := participantCandidatesPage{Rows: rows}
	if len(rows) > limit {
		page.Rows, page.HasMore = rows[:limit], true
		cursor := encodeCandidateCursor(CandidateCursor{SortKey: keys[limit-1], UserID: rows[limit-1].UserID})
		page.NextCursor = &cursor
	}
	if page.Rows == nil {
		page.Rows = []ParticipantCandidate{}
	}

	participants, err := h.repo.ListParticipants(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "conversationID"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	present := map[int64]bool{}
	for _, participant := range participants {
		if participant.EntityName != "user" {
			continue
		}
		if id, err := participantPositiveID(participant.EntityMeta["id"]); err == nil {
			present[id] = true
		}
	}
	for index := range page.Rows {
		page.Rows[index].AlreadyParticipant = present[page.Rows[index].UserID]
	}
	writeJSON(w, http.StatusOK, page)
}

func encodeCandidateCursor(cursor CandidateCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

var errInvalidCandidateCursor = errors.New("invalid candidate cursor")

func decodeCandidateCursor(raw string) (CandidateCursor, error) {
	if len(raw) > 1024 {
		return CandidateCursor{}, errInvalidCandidateCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return CandidateCursor{}, errInvalidCandidateCursor
	}
	var cursor CandidateCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.UserID <= 0 {
		return CandidateCursor{}, errInvalidCandidateCursor
	}
	return cursor, nil
}
