package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Webhook struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Secret    string    `json:"secret,omitempty"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Repository interface {
	List(ctx context.Context, projectID string) ([]Webhook, error)
	Create(ctx context.Context, projectID string, wh Webhook) (Webhook, error)
	Get(ctx context.Context, projectID, webhookID string) (Webhook, error)
	Update(ctx context.Context, projectID, webhookID string, wh Webhook) (Webhook, error)
	Delete(ctx context.Context, projectID, webhookID string) error
	ListByEvent(ctx context.Context, projectID, eventType string) ([]Webhook, error)
}

// The permissions the five routes below are gated on (#496).
//
// THE LEGACY MATRIX HAS NO ENTRY FOR THIS SURFACE, so these are a proposal
// rather than a transcription, and the reason is recorded here.
//
// There is no pylon `webhooks` plugin. The reference's only webhook module is
// elitea_core's INBOUND pipeline trigger (api/v2/webhook.py, api/v2/
// pipeline_trigger.py), which hangs a signature secret off an application
// VERSION and gates on `models.applications.version.details` / `.update`. This
// table is not that: `webhooks` is keyed on project_id alone and carries no
// version, so naming the version strings would claim a relationship the schema
// does not have.
//
// What the row IS, is a per-project integration record that holds a credential —
// a destination url, an event selector and a shared `secret`. That is the same
// thing a `p_{id}.configuration` row is, and the `configurations` plugin's five
// strings are the platform's existing names for list, read, create, update and
// delete of exactly that. Reusing them has three properties an invented name
// would not:
//
//   - They are GRANTABLE today. migrations/shared/0072 grants all five in
//     DEFAULT mode, so no route here answers 403 to every caller on a clean
//     database. A new name would be granted by nothing, which is #354/#359/#402.
//   - Their viewer/editor split is already decided by the legacy matrix: the two
//     reads to admin, editor and viewer; the three writes to admin and editor.
//   - The disclosure tier matches. The read returns `secret`, and the
//     configurations read those strings normally gate returns `data`, which on
//     this platform holds the provider api_key verbatim. Granting the webhook
//     read to a role that already reads the other would be inconsistent in one
//     direction, and withholding it would be inconsistent in the other.
//
// The listing still discloses the `secret` to every holder of the read string.
// That is a redaction question about the response body, not an authorization
// question about the route, and it is deliberately not answered here: #496 is
// the missing gate, and the gate is what stops project A reading project B.
const (
	listPermission    = "configurations.configurations.list"
	detailsPermission = "configurations.configuration.details"
	createPermission  = "configurations.configuration.create"
	updatePermission  = "configurations.configuration.update"
	deletePermission  = "configurations.configuration.delete"
)

type Handler struct {
	repo Repository
	// permissionResolver gates every route in Routes(). nil answers 403 on all
	// five — see require below.
	permissionResolver auth.PermissionResolver
	// deliveries backs the two delivery routes (ListDeliveries, Redeliver).
	// nil answers 501 (not configured on this deployment) on both, the same
	// "named refusal, not a bare 500 or a misleading 404" shape apierr
	// .NotImplemented documents — see require() and the two handlers below.
	deliveries DeliveryRepository
	// dispatcher backs Redeliver's actual re-send. nil answers 501, same as
	// deliveries — the two are always wired together (see WithDispatcher).
	dispatcher *Dispatcher
}

// Option configures a Handler. Same shape as the other v2 packages'.
type Option func(*Handler)

// WithPermissionResolver supplies the resolver EVERY route is gated on. Without
// it all five answer 403, which is the safe direction: the surface reads and
// rotates another tenant's webhook secret.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.permissionResolver = resolver }
}

// WithDispatcher wires the delivery log routes (GET .../deliveries, POST
// .../deliveries/{deliveryID}/redeliver) to a live Dispatcher and its
// DeliveryRepository. Both routes answer 501 until this is applied.
func WithDispatcher(dispatcher *Dispatcher, deliveries DeliveryRepository) Option {
	return func(h *Handler) {
		h.dispatcher = dispatcher
		h.deliveries = deliveries
	}
}

func NewHandler(repo Repository, opts ...Option) *Handler {
	h := &Handler{repo: repo}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Routes returns the webhook subrouter. router.go mounts it at
// "/webhooks/prompt_lib/{projectID}", so `{projectID}` is a segment of the MOUNT
// pattern and chi carries it into this subrouter's route context — which is what
// lets require read it.
//
// It applied no gate at all until #496. Every handler below takes that path
// segment straight to the repository, which filters `webhooks WHERE
// project_id = $1`, so any authenticated caller could list another project's
// webhook url and `secret`, replace the destination, or rotate the secret.
//
// The gate lives HERE rather than at the mount in router.go because the five
// routes need four different strings and one mount can carry only one
// middleware — the same reason /secrets gates inside its own package. Keeping it
// in the package also means a future second mount of Routes() cannot be an
// ungated one.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.With(h.require(listPermission)).Get("/", h.List)
	r.With(h.require(createPermission)).Post("/", h.Create)
	r.With(h.require(detailsPermission)).Get("/{webhookID}", h.Get)
	r.With(h.require(updatePermission)).Put("/{webhookID}", h.Update)
	r.With(h.require(deletePermission)).Delete("/{webhookID}", h.Delete)
	// The delivery log is a READ of what the webhook already did, so it
	// takes the same permission as reading the webhook itself.
	r.With(h.require(detailsPermission)).Get("/{webhookID}/deliveries", h.ListDeliveries)
	// Redeliver is a WRITE — it makes the platform issue a new outbound
	// HTTP call on the caller's behalf — so it takes the update string, the
	// same one rotating the secret or flipping active takes.
	r.With(h.require(updatePermission)).Post("/{webhookID}/deliveries/{deliveryID}/redeliver", h.Redeliver)
	return r
}

// require gates one route on the named permission, resolved in DEFAULT mode
// against the `{projectID}` the mount pattern supplies. Fail-closed by
// construction: RequireResolvedPermissionsForProject answers 403 on a nil
// resolver, and legacyrbac refuses a project id that is not a positive integer
// before the handler runs.
func (h *Handler) require(permission string) func(http.Handler) http.Handler {
	return apimw.RequireResolvedPermissions(
		h.permissionResolver,
		auth.PermissionModeDefault,
		permission,
	)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	webhooks, err := h.repo.List(r.Context(), projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": webhooks})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	wh, err := h.repo.Get(r.Context(), projectID, webhookID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wh)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var wh Webhook
	if err := json.NewDecoder(r.Body).Decode(&wh); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	created, err := h.repo.Create(r.Context(), projectID, wh)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	var wh Webhook
	if err := json.NewDecoder(r.Body).Decode(&wh); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	updated, err := h.repo.Update(r.Context(), projectID, webhookID, wh)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	if err := h.repo.Delete(r.Context(), projectID, webhookID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxRecentDeliveries bounds the "Recent deliveries" panel. Older attempts
// remain in the table (redelivering old evidence is exactly what Redeliver's
// own comment says the log is FOR) — this only bounds the ONE listing route.
const maxRecentDeliveries = 20

// ListDeliveries answers the "Recent deliveries" panel: this webhook's last
// maxRecentDeliveries attempt sequences, newest first, whether they were
// originals or redeliveries.
func (h *Handler) ListDeliveries(w http.ResponseWriter, r *http.Request) {
	if h.deliveries == nil {
		apierr.Write(w, apierr.NotImplemented("delivery log not available on this deployment"))
		return
	}
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	// A webhook id that names nothing in this project must answer 404, not
	// an empty deliveries list — the same reason Get() below checks the
	// webhook exists before reading its deliveries: a caller probing ids
	// must see no difference between "no deliveries yet" and "not your
	// webhook" from the deliveries route alone, but the two ARE different to
	// a legitimate caller and the 404 says which.
	if _, err := h.repo.Get(r.Context(), projectID, webhookID); err != nil {
		apierr.Write(w, err)
		return
	}

	deliveries, err := h.deliveries.ListRecent(r.Context(), projectID, webhookID, maxRecentDeliveries)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": deliveries})
}

// Redeliver re-sends one logged delivery's exact payload and logs the new
// attempt as its own row. See Dispatcher.Redeliver's doc comment for why it
// is synchronous and why it is a new row rather than an update.
func (h *Handler) Redeliver(w http.ResponseWriter, r *http.Request) {
	if h.dispatcher == nil || h.deliveries == nil {
		apierr.Write(w, apierr.NotImplemented("delivery log not available on this deployment"))
		return
	}
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")
	deliveryID := chi.URLParam(r, "deliveryID")

	delivery, err := h.dispatcher.Redeliver(r.Context(), projectID, webhookID, deliveryID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, delivery)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
