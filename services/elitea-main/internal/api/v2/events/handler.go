package events

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/natsbus"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/ssewriter"
)

// EventSource is the seam the SSE handler consumes. It yields decoded events on
// the given channel until the caller invokes the returned cancel func or the
// request context is cancelled. The live-update NATS bus
// (internal/infra/natsbus.EventBus.Raw) implements it. The Redis adapter that
// used to sit beside it was deleted with the plain Redis at REDIS_URL.
// Whatever arrives, only the one type each family carries reaches a client
// (streamFamilies).
type EventSource interface {
	Raw(ctx context.Context, channel string) (<-chan natsbus.Event, func(), error)
}

// StreamPermission gates the project event stream (#496).
//
// THE LEGACY MATRIX HAS NO ENTRY FOR THIS ROUTE — the reference serves no
// project SSE stream — so this is a proposal, and this is its reason.
//
// The stream is the project's own live feed. What it forwards is exactly
// streamFamilies: canvas presence rosters and the LLM gateway's
// budget.soft_alert. The soft alert carries the project's accrued cost. The platform already has a name for "this caller
// may observe this project": `models.project_context.view`. It gates
// GET /api/v2/elitea_core/project_info/{mode}/{projectID}/project-info and every
// project-scoped budget read — /usage/prompt_lib/{projectID}/usage and
// /user_budgets/prompt_lib/{projectID} among them (internal/api/v2/budgets,
// ProjectViewPermission). Putting the live feed of the same cost figures on the
// same string is the choice that leaves no way in through the stream that the
// REST read does not already allow.
//
// It is granted in DEFAULT mode by migrations/shared/0062 to admin, editor and
// viewer, so this route does not answer 403 to every caller on a clean database.
// The legacy matrix gives the same three roles the same string in the default
// mode, so the split is transcribed even though the route is not.
//
// The narrower alternative — one of the notification strings, as
// CurrentNotificationEventsRoute takes `models.notifications.notifications.list`
// for the notification stream — was rejected because this stream is not
// notifications: it carries no notification event and would then be gated on a
// permission that describes none of its payloads.
const StreamPermission = "models.project_context.view"

// streamFamilies is the COMPLETE description of what the stream relays: one
// subject family per producer, and the ONE event type each may carry.
// Everything else that arrives is dropped, payload and all.
//
// Each family is its own NATS subject in its own account (#1076):
//
//   - canvas presence: events.PresenceChannel → elitea.events.project.<id>.presence,
//     in the MAIN account, published by elitea-main alone. The roster (#622):
//     who has a canvas open. Every viewer of the canvas sees the same roster
//     in the heartbeat's own response.
//   - the LLM gateway's 80% budget soft alert (design §8.3):
//     events.ProjectChannel → gateway.events.project.<id>.events, published in
//     the GATEWAY account and imported into MAIN. The project's accrued cost,
//     already readable through the project-scoped budget routes gated on the
//     same permission.
//
// Pinning a type to its family is what stops one producer from speaking for
// the other: a canvas.editors frame on the gateway's subject (the gateway
// forging a roster into a project's stream) or a budget.soft_alert on the
// presence subject is dropped, whatever its payload says.
//
// It is an allowlist, not a denylist, because the stream's gate
// (StreamPermission, which project viewers hold) is wider than many payloads a
// project can produce. Domain events — conversation.created carries a private
// conversation's name and creator — are kept off the bus at the composition
// root (cmd/elitea-main, newDomainEventsPublisher); this table is the second
// layer, so a regression there still never reaches a browser. Adding a type
// or a family here is a decision that every project viewer may read its
// payload.
var streamFamilies = []struct {
	channel   func(projectID string) string
	eventType string
}{
	{events.ProjectChannel, events.EventBudgetSoftAlert},
	{events.PresenceChannel, canvaspresence.EventType},
}

// Forwarded reports whether the stream relays eventType at all (from its own
// family; see ForwardedOn).
func Forwarded(eventType string) bool {
	for _, f := range streamFamilies {
		if f.eventType == eventType {
			return true
		}
	}
	return false
}

// ForwardedOn reports whether the stream relays eventType when it arrives on
// channel for projectID: only the one type that channel's family carries.
func ForwardedOn(projectID, channel, eventType string) bool {
	for _, f := range streamFamilies {
		if f.channel(projectID) == channel {
			return f.eventType == eventType
		}
	}
	return false
}

type Handler struct {
	source EventSource
	// permissionResolver gates Stream. nil answers 403 — see require below.
	permissionResolver auth.PermissionResolver
}

// Option configures a Handler. Same shape as the other v2 packages'.
type Option func(*Handler)

// WithPermissionResolver supplies the resolver the stream is gated on. Without
// it the route answers 403, which is the safe direction: the stream is another
// tenant's live event bus.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.permissionResolver = resolver }
}

// NewHandlerFromSource builds the handler over an EventSource (the live-update
// NATS bus in production).
func NewHandlerFromSource(src EventSource, opts ...Option) *Handler {
	return newHandler(src, opts...)
}

func newHandler(src EventSource, opts ...Option) *Handler {
	h := &Handler{source: src}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Routes returns the SSE subrouter. router.go mounts it at
// "/events/prompt_lib/{projectID}", so `{projectID}` is a segment of the MOUNT
// pattern and chi carries it into this subrouter's route context.
//
// It applied no gate at all until #496. Stream subscribes to the project's
// channels (streamFamilies) straight from that segment, so any authenticated
// caller could read any tenant's live event bus.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.With(h.require(StreamPermission)).Get("/", h.Stream)
	return r
}

// require gates the stream on the named permission, resolved in DEFAULT mode
// against the `{projectID}` the mount pattern supplies. Fail-closed by
// construction: RequireResolvedPermissionsForProject answers 403 on a nil
// resolver, and legacyrbac refuses a project id that is not a positive integer
// before the handler runs.
//
// The gate is applied at the ROUTE, not inside Stream, so the refusal is a plain
// 403 with no response body started. A check inside the handler would have to
// run after ssewriter.New had already taken over the connection.
func (h *Handler) require(permission string) func(http.Handler) http.Handler {
	return apimw.RequireResolvedPermissions(
		h.permissionResolver,
		auth.PermissionModeDefault,
		permission,
	)
}

func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	sse, err := ssewriter.New(w)
	if err != nil {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	// One subscription per family, each delivering into one loop that knows
	// which family a frame came from.
	type frame struct {
		channel string
		event   natsbus.Event
	}
	merged := make(chan frame)
	ended := make(chan struct{}, len(streamFamilies))
	for _, f := range streamFamilies {
		channel := f.channel(projectID)
		evCh, cancel, err := h.source.Raw(ctx, channel)
		if err != nil {
			http.Error(w, "event source unavailable", http.StatusInternalServerError)
			return
		}
		defer cancel()
		go func() {
			defer func() { ended <- struct{}{} }()
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-evCh:
					if !ok {
						return
					}
					select {
					case merged <- frame{channel: channel, event: evt}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	_ = sse.Comment("connected")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ended:
			// A family's source closed: the upstream is gone.
			return
		case f := <-merged:
			if !ForwardedOn(projectID, f.channel, f.event.Type) {
				// Type and family only, never the payload: an unlisted frame
				// is either a regression upstream or a forgery, and either
				// way its body is not ours to copy into logs.
				slog.Debug("events: dropped an event the project stream does not forward on this subject",
					"type", f.event.Type, "source", f.event.Source, "channel", f.channel)
				continue
			}
			_ = sse.Event(f.event.Type, string(f.event.Payload))
		case <-heartbeat.C:
			if err := sse.Comment("heartbeat"); err != nil {
				return
			}
		}
	}
}
