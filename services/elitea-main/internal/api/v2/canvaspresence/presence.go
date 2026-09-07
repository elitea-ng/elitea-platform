// Package canvaspresence serves canvas editor presence over the EXISTING
// project SSE plane (#622).
//
//	POST /api/v2/elitea_core/canvas/prompt_lib/{projectID}/{canvasID}/presence
//
// It is a HEARTBEAT, not a socket. A client that has a canvas open calls it on
// mount, on a timer, when the tab is hidden or shown again, and once with
// `state: "left"` on unmount. The answer is the current roster, and the same
// roster is published as a `canvas.editors` event on
// events.ProjectChannel(projectID) so every OTHER subscriber of that project's
// stream learns about it without polling.
//
// WHY NOT A SOCKET SERVER. #615 and #622 record the decision in full. The short
// form: every control on this surface — authentication, project resolution,
// membership, the named permission — is a chi middleware wrapping ONE HTTP
// request. A socket has one HTTP request at the start and then N untyped
// events, so each control has to be re-implemented per event by hand. The
// deleted 332-line prototype got the room name, the field name and the presence
// identity wrong in ~40 lines of canvas handling.
//
// THE ROOM IS THE PROJECT CHANNEL. events.ProjectChannel is derived SERVER-SIDE
// from the {projectID} segment of the mount pattern, which
// apimw.RequireResolvedPermissions has already gated. No part of the channel
// name comes from the request body. This is the specific defect the prototype
// had: its room was `"canvas:" + canvasID` with no project component, and
// canvas ids are per-tenant-schema integers, so canvas 42 in project 1 and
// canvas 42 in project 7 were THE SAME ROOM by construction.
//
// A PROJECT PREFIX ALONE IS NOT ENOUGH, which is why CanvasResolver exists. The
// canvas has to be shown to exist INSIDE schema(projectID) before anything is
// published: without that, a member of project A could heartbeat canvas id 7,
// have it accepted because the channel is A's, and publish a presence entry for
// an id that only means something in project B. The resolver answers 404 and
// nothing is published.
//
// IDENTITY IS SERVER-DERIVED. The editor's name comes from
// auth.RuntimePrincipalFromContext, never from the request body. The prototype
// read `data["user_id"]` straight off the wire.
//
// THE PAYLOAD IS THE REFERENCE'S. legacy/plugins/elitea_core emits
// `chat_canvas_editors_change` with {editors, canvas_uuid, message_group_uuid},
// and each editor is exactly {user_name, user_avatar}
// (utils/participant_utils.py get_entity_details). Both names are kept, so the
// product UI's existing `useChatCanvasEditorsChange` patch path
// (item_details.editors) applies unchanged. `user_id` is ADDED: the reference
// matched editors by display-name string comparison, which two people with the
// same display name defeat.
//
// WHAT IS DELIBERATELY NOT PORTED:
//
//   - THE LOCK. The reference's presence set IS an edit lock: `edit_canvas`
//     refuses a write from anyone not already in the set and answers
//     `chat_canvas_error: "Canvas is locked"`. Nothing here refuses a write.
//     The canvas WRITE route (PUT /canvas/...) is not part of this change, and
//     a lock enforced by the presence route while the write route ignores it
//     would be a control that looks live and never fires — the #126 class. The
//     product UI still uses the roster to go read-only, which is what the
//     reference user saw; the server-side refusal is a separate change.
//
//   - `user_avatar` IS ALWAYS ABSENT. The reference fills it from the `social`
//     plugin's avatar store. This package composes no reader for it, so the key
//     is OMITTED rather than sent as an empty string: an empty string renders
//     as "this user has no avatar", which is a different and wrong claim. The
//     product UI falls back to initials, which is what it does for a genuinely
//     absent avatar too.
//
//   - THE 60-SECOND CRON RE-BROADCAST. The reference re-emits the roster from a
//     `* * * * *` job because its presence entries have no per-user expiry — the
//     whole Redis SET expires at once after 120s and a leaver is never removed
//     unless they are the LAST one. Here every entry carries its own deadline
//     (see Store), so a stale editor disappears from the next roster anyone
//     reads without a sweeper.
package canvaspresence

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Permission is the gate this route is registered behind. It is NOT a new
// permission name: `models.chat.canvas.details` is the string the canvas READ
// already takes (router.go's GetCanvas registration), and a heartbeat announces
// presence on a canvas the caller is reading. Reusing it means this route needs
// no migration and 403s nobody who can already open the canvas — the trap
// recorded in migrations/shared/0063's header, where a route gated on a
// permission nothing grants is 403-for-everyone.
const Permission = "models.chat.canvas.details"

// EventType is the DomainEvent type published on the project channel. It is the
// name #622 specifies. The reference socket event it replaces is
// `chat_canvas_editors_change`; the SSE name is dotted like every other entry in
// internal/events/publisher.go rather than copying the socket spelling, because
// the transport is not the socket.
const EventType = "canvas.editors"

// TTL is how long one heartbeat keeps an editor in the roster. The client
// re-sends at TTL/3 (apps/elitea-web's useCanvasPresence), so two consecutive
// lost beats still do not evict a live tab, and a tab that is closed without
// running its unmount handler — a crash, a killed browser, a lost network —
// disappears within one TTL.
//
// 120s is the reference's own CANVAS_CONTENT_TTL
// (legacy/plugins/elitea_core/utils/chat_constants.py). Keeping it means a
// deployment running both halves during the migration evicts at the same moment
// on both sides.
const TTL = 120 * time.Second

// maxRequestBytes bounds the body. The body carries a state word and an
// optional message-group uuid, so this is generous rather than absent.
const maxRequestBytes = 4 << 10

// Reference sentinel accounts. legacy filters these out of the roster in three
// places (Canvas.jsx, CanvasEditor.jsx, ApplicationAnswer.jsx) because they are
// service principals that would otherwise render as avatars beside the humans.
// Filtering them HERE as well as in the UI is deliberate: the UI filter only
// protects the UI, and the roster is also the answer to a REST call.
const (
	adminSentinelUser  = "admin@centry.user"
	systemSentinelUser = "system@centry.user"
)

// State is the heartbeat's declared intent.
type State string

const (
	// StateEditing — the caller has the canvas open in the editor.
	StateEditing State = "editing"
	// StateViewing — the caller is looking at the canvas but is not editing it.
	// Kept in the roster like an editor: the reference's set does not
	// distinguish the two either, and the UI shows "is editing…" for both.
	StateViewing State = "viewing"
	// StateLeft — remove the caller from the roster now, rather than waiting a
	// TTL. Sent on unmount and on `visibilitychange` to hidden.
	StateLeft State = "left"
)

// Editor is one entry in the roster. The json names are the reference's
// (participant_utils.get_entity_details), so the SPA's existing
// `item_details.editors` patch path needs no translation.
type Editor struct {
	// UserID is the server-derived principal id. ADDED to the reference shape;
	// see the package doc.
	UserID string `json:"user_id"`
	// UserName is what the UI renders and what the reference matched on.
	UserName string `json:"user_name"`
	// UserAvatar is always absent today; see the package doc.
	UserAvatar string `json:"user_avatar,omitempty"`
	// State is the last state this editor heartbeated.
	State State `json:"state"`
}

// Store holds one roster per (project, canvas). Every entry carries its OWN
// deadline, so List never returns an editor whose last heartbeat is older than
// the TTL, with no sweeper and no cron.
//
// Implementations: NewRedisStore (shared across replicas) and NewMemoryStore
// (single replica; see NewHandler's degrade note).
type Store interface {
	// Touch records or refreshes one editor under key, expiring after ttl.
	Touch(ctx context.Context, key string, editor Editor, ttl time.Duration) error
	// Remove drops one editor by user id. Removing an absent editor is not an
	// error.
	Remove(ctx context.Context, key string, userID string) error
	// List returns the live roster, sorted by user id so two callers reading the
	// same roster see the same order.
	List(ctx context.Context, key string) ([]Editor, error)
}

// CanvasResolver proves that canvasID names a canvas INSIDE schema(projectID).
// This is the cross-project control; see the package doc.
//
// It returns the canvas's uuid and the uuid of the message group that holds it,
// because the reference's event carries both and the SPA matches on both.
type CanvasResolver interface {
	ResolveCanvas(ctx context.Context, projectID, canvasID string) (canvasUUID, messageGroupUUID string, err error)
}

// Emitter publishes the roster on the project channel. *events.Publisher
// satisfies it; a nil Emitter means "publish nothing", which is the state a
// deployment with no Redis is in.
type Emitter interface {
	Emit(ctx context.Context, projectID, eventType string, payload any)
}

// Handler serves the heartbeat.
type Handler struct {
	resolver CanvasResolver
	store    Store
	emitter  Emitter
	ttl      time.Duration
	now      func() time.Time
}

// Option configures a Handler, same shape as the other v2 packages'.
type Option func(*Handler)

// WithStore replaces the default in-process store. Passing a nil Store is a
// no-op rather than an error, so a caller can hand it a value that may be
// absent without branching — the WithPool convention in
// internal/api/v2/conversations.
func WithStore(store Store) Option {
	return func(h *Handler) {
		if store != nil {
			h.store = store
		}
	}
}

// WithEmitter supplies the publisher. A nil Emitter (the default) still serves
// the route: the caller gets the roster back in the response body, so a single
// tab's own presence is correct even with no bus. What is lost is the push to
// the OTHER tabs, and that is exactly what a deployment with no Redis loses on
// every other surface too.
func WithEmitter(emitter Emitter) Option {
	return func(h *Handler) {
		if emitter != nil {
			h.emitter = emitter
		}
	}
}

// WithClock replaces time.Now. Tests use it to walk a roster past its TTL
// without sleeping.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) {
		if now != nil {
			h.now = now
		}
	}
}

// NewHandler builds the heartbeat handler.
//
// The DEFAULT store is in-process. That is a real degrade and it is stated
// rather than hidden: with more than one replica and no Redis store injected,
// two editors served by different replicas do not see each other. The
// production wiring passes NewRedisStore (router.go), and every elitea-main
// deployment in this repository runs Redis — the same fact
// cmd/elitea-main/event_stream_redis.go relies on for the SSE stream this route
// publishes onto.
func NewHandler(resolver CanvasResolver, opts ...Option) *Handler {
	h := &Handler{
		resolver: resolver,
		store:    NewMemoryStore(),
		ttl:      TTL,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// request is the heartbeat body. Every field is optional; an empty body is a
// `viewing` beat.
//
// There is no project_id and no canvas_id field, and there will not be one:
// both come from the gated mount pattern. A body field for either would be the
// prototype's defect written a second time.
type request struct {
	State State `json:"state"`
	// MessageGroupUUID is what the SPA already knows and the reference event
	// carries. It is ECHOED, never trusted: the resolver returns the real one
	// and that is what is published.
	MessageGroupUUID string `json:"message_group_uuid"`
}

// Response is the heartbeat's answer AND the published event's payload. One
// shape, so a client that reads its own POST response and a client that reads
// the SSE frame parse the same thing.
type Response struct {
	ProjectID string `json:"project_id"`
	// EntityID/EntityType/Action mirror events.DomainEvent so a subscriber that
	// already switches on those fields keeps working.
	EntityID   string `json:"entity_id"`
	EntityType string `json:"entity_type"`
	Action     string `json:"action"`

	CanvasUUID       string `json:"canvas_uuid"`
	MessageGroupUUID string `json:"message_group_uuid"`
	// Editors is the live roster. NEVER null: an empty roster is `[]`, because a
	// client that reads `null` cannot tell "nobody is editing" from "this server
	// does not answer this".
	Editors []Editor `json:"editors"`
	// TTLSeconds tells the client how long its beat is good for, so the resend
	// interval is not a constant duplicated on both sides.
	TTLSeconds int `json:"ttl_seconds"`
}

const (
	entityTypeCanvas = "canvas"
	actionEditors    = "editors"
)

// Heartbeat serves POST .../{canvasID}/presence.
func (h *Handler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	canvasID := chi.URLParam(r, "canvasID")

	principal, ok := auth.RuntimePrincipalFromContext(r.Context())
	if !ok {
		apierr.Write(w, apierr.Unauthorized("authentication is required"))
		return
	}

	body, err := decodeRequest(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	if h.resolver == nil {
		apierr.Write(w, apierr.Internal("canvas presence is not configured"))
		return
	}
	canvasUUID, messageGroupUUID, err := h.resolver.ResolveCanvas(r.Context(), projectID, canvasID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	key := RosterKey(projectID, canvasUUID)
	editor := Editor{
		UserID:   principal.ID,
		UserName: principalName(principal),
		State:    body.State,
	}

	if body.State == StateLeft {
		if err := h.store.Remove(r.Context(), key, editor.UserID); err != nil {
			apierr.Write(w, apierr.Internal("canvas presence store is unavailable"))
			return
		}
	} else if err := h.store.Touch(r.Context(), key, editor, h.ttl); err != nil {
		apierr.Write(w, apierr.Internal("canvas presence store is unavailable"))
		return
	}

	roster, err := h.store.List(r.Context(), key)
	if err != nil {
		apierr.Write(w, apierr.Internal("canvas presence store is unavailable"))
		return
	}

	response := Response{
		ProjectID:        projectID,
		EntityID:         canvasUUID,
		EntityType:       entityTypeCanvas,
		Action:           actionEditors,
		CanvasUUID:       canvasUUID,
		MessageGroupUUID: messageGroupUUID,
		Editors:          filterSentinels(roster),
		TTLSeconds:       int(h.ttl / time.Second),
	}

	if h.emitter != nil {
		// projectID is the RESOLVED mount segment. events.Publisher.Emit turns
		// it into events.ProjectChannel(projectID) itself, so no caller of this
		// package ever names a channel.
		h.emitter.Emit(r.Context(), projectID, EventType, response)
	}

	writeJSON(w, response)
}

// RosterKey is the store key for one canvas's roster. Both components are
// server-derived: projectID is the gated mount segment and canvasUUID is what
// the resolver read out of schema(projectID). Exported so the Redis store's
// tests and the router's wiring name the same thing.
func RosterKey(projectID, canvasUUID string) string {
	return "canvas_presence:" + projectID + ":" + canvasUUID
}

// principalName picks what the roster renders. The reference's `user_name` is
// the auth `name` field, which in a real deployment is email-shaped; Email is
// the fallback for a principal that carries no display name, and the id is the
// last resort so an entry is never nameless.
func principalName(principal auth.User) string {
	if name := strings.TrimSpace(principal.Name); name != "" {
		return name
	}
	if email := strings.TrimSpace(principal.Email); email != "" {
		return email
	}
	return principal.ID
}

func filterSentinels(roster []Editor) []Editor {
	filtered := make([]Editor, 0, len(roster))
	for _, editor := range roster {
		if editor.UserName == adminSentinelUser || editor.UserName == systemSentinelUser {
			continue
		}
		filtered = append(filtered, editor)
	}
	return filtered
}

func decodeRequest(r *http.Request) (request, error) {
	body := request{State: StateViewing}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return body, apierr.BadRequest("could not read the request body")
	}
	if len(raw) > maxRequestBytes {
		return body, apierr.BadRequest("the request body is too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return body, apierr.BadRequest("the request body must be a JSON object")
	}
	switch body.State {
	case "":
		body.State = StateViewing
	case StateEditing, StateViewing, StateLeft:
	default:
		return body, apierr.BadRequest(`state must be one of "editing", "viewing" or "left"`)
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("canvaspresence: encode response", "err", err)
	}
}

// ---------------------------------------------------------------------------
// In-process store
// ---------------------------------------------------------------------------

// entry is one editor plus its deadline.
type entry struct {
	editor   Editor
	deadline time.Time
}

// sortRoster keeps List's answer stable. Two subscribers comparing rosters must
// not see a difference that is only map iteration order.
func sortRoster(roster []Editor) []Editor {
	sort.Slice(roster, func(i, j int) bool { return roster[i].UserID < roster[j].UserID })
	return roster
}
