// Package conversationskills serves the skills pinned to a conversation (ADR-0050
// amendment 1, A1.2; skills epic 2169 story S3).
//
// A pin is a skill, or a Marketplace listing version, that applies to every run
// of the conversation whichever agent or model answers. It is an attachment,
// never a participant, and it grants nothing: a skill is text. The start paths
// read the pins when each turn starts (application/agentexecution), under the
// caller's own visibility; these routes only keep the list.
//
// Reading follows the conversation read rule. Pinning and unpinning follow the
// message-write rule, because a pin is content the model will read as part of
// the conversation: a person who may not write to the conversation must not
// steer it (conversations.Repository.AuthorizeChatWrite). The router adds the
// `models.chat.conversation.update` permission for the writes.
package conversationskills

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

// MaxPins is how many skills one conversation may hold. It is the five skills
// a turn may invoke: the pins share that bound with the message's own picks.
const MaxPins = 5

// Pin kinds.
const (
	KindProject = "project"
	KindListing = "listing"
)

// Trust is the state of a pinned skill's text (ADR-0051 amendment 1, A1.6).
type Trust struct {
	// State is `trusted` or `untrusted`.
	State string `json:"state"`
	// PublisherProjectID is the project a forked skill came from, when it has one.
	PublisherProjectID *int64 `json:"publisher_project_id,omitempty"`
}

// Pin is one skill pinned to a conversation.
type Pin struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	// SkillID is the project skill, for kind `project`.
	SkillID *int64 `json:"skill_id,omitempty"`
	// ListingID and ListingVersionID name the pinned Marketplace listing
	// version, for kind `listing`.
	ListingID        *int64 `json:"listing_id,omitempty"`
	ListingVersionID *int64 `json:"listing_version_id,omitempty"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	// VersionName is the listing version's name, for kind `listing`.
	VersionName string `json:"version_name,omitempty"`
	Trust       Trust  `json:"trust"`
	// Available is false when this pin is NOT applied for the caller: a project
	// skill the caller cannot read, or a listing version that is yanked,
	// withdrawn or delisted. The Skills section says so (ADR-0050 amendment 1).
	Available bool `json:"available"`
	// PinnedBy is the user id of the person who pinned it.
	PinnedBy int64     `json:"pinned_by"`
	PinnedAt time.Time `json:"pinned_at"`
}

// Repository is the storage the handler needs, scoped to one project and one
// conversation (a conversation id or uuid, as the other conversation routes
// take it).
type Repository interface {
	List(ctx context.Context, projectID, conversationID string) ([]Pin, error)
	// PinSkill pins a project skill the caller can read. It answers the existing
	// pin, with created false, when the skill is pinned already.
	PinSkill(ctx context.Context, projectID, conversationID string, skillID int64) (pin Pin, created bool, err error)
	// PinListing pins a listing version of a skill listing that is listed and
	// whose version is not yanked or withdrawn.
	PinListing(ctx context.Context, projectID, conversationID string, listingID, versionID int64) (pin Pin, created bool, err error)
	Unpin(ctx context.Context, projectID, conversationID string, pinID int64) error
}

// Authorize is the conversation rule one route applies, with the route's
// project and conversation ids.
type Authorize func(ctx context.Context, projectID, conversationID string) error

// Handler serves the three routes.
type Handler struct {
	repo           Repository
	authorizeRead  Authorize
	authorizeWrite Authorize
}

// NewHandler builds the handler. authorizeRead is the conversation read rule and
// authorizeWrite the message-write rule.
func NewHandler(repo Repository, authorizeRead, authorizeWrite Authorize) *Handler {
	return &Handler{repo: repo, authorizeRead: authorizeRead, authorizeWrite: authorizeWrite}
}

const maxBodyBytes = 4 << 10

// List serves GET: the conversation's pins, oldest first.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID, conversationID := chi.URLParam(r, "projectID"), chi.URLParam(r, "conversationID")
	if err := h.authorizeRead(r.Context(), projectID, conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	pins, err := h.repo.List(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if pins == nil {
		pins = []Pin{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": pins, "total": len(pins), "max": MaxPins})
}

type pinBody struct {
	SkillID          *int64 `json:"skill_id"`
	ListingID        *int64 `json:"listing_id"`
	ListingVersionID *int64 `json:"listing_version_id"`
}

func (b pinBody) valid() bool {
	project := b.SkillID != nil && b.ListingID == nil && b.ListingVersionID == nil && *b.SkillID > 0
	listing := b.SkillID == nil && b.ListingID != nil && b.ListingVersionID != nil && *b.ListingID > 0 && *b.ListingVersionID > 0
	return project || listing
}

// Pin serves POST: pin a project skill (`skill_id`) or a listing version
// (`listing_id` and `listing_version_id`). Pinning what is already pinned
// answers the existing pin with 200.
func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	projectID, conversationID := chi.URLParam(r, "projectID"), chi.URLParam(r, "conversationID")
	if err := h.authorizeWrite(r.Context(), projectID, conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(raw) > maxBodyBytes {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var body pinBody
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() || !body.valid() {
		apierr.Write(w, apierr.BadRequest("the body must be {\"skill_id\": n} or {\"listing_id\": n, \"listing_version_id\": n}"))
		return
	}
	var (
		pin     Pin
		created bool
	)
	if body.SkillID != nil {
		pin, created, err = h.repo.PinSkill(r.Context(), projectID, conversationID, *body.SkillID)
	} else {
		pin, created, err = h.repo.PinListing(r.Context(), projectID, conversationID, *body.ListingID, *body.ListingVersionID)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		audit.Annotate(r.Context(), audit.Annotation{
			Action: "Pinned " + describe(pin) + " to a conversation", EntityType: "conversation_skill", EntityID: audit.ID(pin.ID),
		})
	}
	writeJSON(w, status, pin)
}

// Unpin serves DELETE: remove one pin from the conversation.
func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	projectID, conversationID := chi.URLParam(r, "projectID"), chi.URLParam(r, "conversationID")
	if err := h.authorizeWrite(r.Context(), projectID, conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	pinID, err := strconv.ParseInt(chi.URLParam(r, "pinID"), 10, 64)
	if err != nil || pinID <= 0 {
		apierr.Write(w, apierr.BadRequest("pin id must be a positive integer"))
		return
	}
	if err := h.repo.Unpin(r.Context(), projectID, conversationID, pinID); err != nil {
		apierr.Write(w, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action: "Unpinned a skill from a conversation", EntityType: "conversation_skill", EntityID: audit.ID(pinID),
	})
	w.WriteHeader(http.StatusNoContent)
}

func describe(pin Pin) string {
	if pin.Kind == KindListing {
		return "listing skill " + strconv.Quote(pin.Name)
	}
	return "skill " + strconv.Quote(pin.Name)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
