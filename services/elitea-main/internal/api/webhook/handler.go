package webhook

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Webhook is one row of the outbound webhook registry, and the shape every
// read route returns.
//
// The signing secret is WRITE-ONLY (issue 2963). It lives in the project
// vault as a hidden secret, the row keeps only the `{{secret.NAME}}`
// reference, and neither field below that carries secret material is ever
// marshalled: both are tagged `json:"-"`. A read reports only whether a
// secret is set (HasSecret). Create and RotateSecret return the value once,
// through secretResponse.
type Webhook struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"project_id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	// Secret is plaintext signing material. On the way IN to the repository
	// it is the value to seal. On the way OUT it is non-empty only for a row
	// written before the vault move reached it (see
	// migrations/shared/0267_webhook_secret_reference.sql), which the
	// dispatcher still signs with so an upgrade sends nothing unsigned.
	Secret string `json:"-"`
	// SecretRef is the vault reference the dispatcher resolves per delivery.
	SecretRef string `json:"-"`
	// HasSecret reports that deliveries are signed. It is the only trace of
	// the secret a read carries.
	HasSecret bool      `json:"has_secret"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Repository interface {
	List(ctx context.Context, projectID string) ([]Webhook, error)
	// Create stores wh and, when wh.Secret is non-empty, seals it into the
	// project vault in the same transaction.
	Create(ctx context.Context, projectID string, wh Webhook) (Webhook, error)
	Get(ctx context.Context, projectID, webhookID string) (Webhook, error)
	// Update replaces url, events and active. A nil or empty secret keeps
	// the stored secret; a non-empty one replaces it.
	Update(ctx context.Context, projectID, webhookID string, wh Webhook, secret *string) (Webhook, error)
	Delete(ctx context.Context, projectID, webhookID string) error
	ListByEvent(ctx context.Context, projectID, eventType string) ([]Webhook, error)
	// RotateSecret replaces the stored secret with `secret` and removes the
	// previous vault value, in one transaction.
	RotateSecret(ctx context.Context, projectID, webhookID, secret string) (Webhook, error)
}

// webhookWriteRequest is the create and update body. On create an absent or
// empty `secret` makes the server generate one. On update an absent or empty
// `secret` keeps the stored one, so a client that sends back what it read
// (which carries no secret) cannot strip the signature; a non-empty value
// replaces it. There is no way to remove a secret: every delivery is signed
// (#2959). Update never echoes it.
type webhookWriteRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
	Secret *string  `json:"secret"`
	Active bool     `json:"active"`
}

// secretResponse is the one shape that carries the secret: the answer to
// Create (the caller's value or the generated one) and to RotateSecret. No other route
// returns it, and no route returns it twice.
type secretResponse struct {
	Webhook
	Secret string `json:"secret,omitempty"`
}

// The permissions the five routes below are gated on (#496).
//
// THE LEGACY MATRIX HAS NO ENTRY FOR THIS SURFACE, so these are a proposal
// rather than a transcription, and the reason is recorded here.
//
// There is no legacy `webhooks` plugin. The reference's only webhook module is
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
//   - The disclosure tier matches. Neither read returns secret material: the
//     configurations read returns `{{secret.NAME}}` references, and the
//     webhook read returns `has_secret` (issue 2963). The signing secret is
//     shown once, by Create and RotateSecret, to a caller holding a WRITE
//     string.
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
	// destinationGuard validates `url` on every Create and Update — see
	// ssrf.go. nil answers 400 on BOTH routes: unlike the dispatcher's own
	// (best-effort, degrade-gracefully) use of an absent guard, the WRITE
	// path that admits a new destination into the table is fail-closed, the
	// same direction every other optional dependency in this file that gates
	// an admission decision takes (permissionResolver nil -> 403 on every
	// route). A deployment that has not wired a guard cannot create or
	// repoint a webhook at all, which is the safe failure: silently
	// accepting an unchecked destination is the defect this file exists to
	// close.
	destinationGuard *DestinationGuard
}

// Option configures a Handler. Same shape as the other v2 packages'.
type Option func(*Handler)

// WithPermissionResolver supplies the resolver EVERY route is gated on. Without
// it every route answers 403, which is the safe direction: the surface
// repoints and rotates another tenant's webhooks.
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

// WithDestinationGuard wires the SSRF check every Create and Update runs
// against `url` before it reaches the repository. Without it both routes
// answer 400 unconditionally — see the destinationGuard field's doc comment.
func WithDestinationGuard(guard *DestinationGuard) Option {
	return func(h *Handler) { h.destinationGuard = guard }
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
// project_id = $1`, so the gate is what keeps one project's callers off
// another project's webhooks.
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
	// Rotating the signing secret is a write that returns the new value
	// once, so it takes the update string, the same one PUT takes.
	r.With(h.require(updatePermission)).Post("/{webhookID}/rotate_secret", h.RotateSecret)
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
	items := make([]Webhook, len(webhooks))
	for index, wh := range webhooks {
		items[index] = wh.redacted()
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	wh, err := h.repo.Get(r.Context(), projectID, webhookID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wh.redacted())
}

// Create registers a webhook. Every webhook has a signing secret (#2959): a
// `secret` in the body is used as given, and when the body has none (absent
// or empty) the server generates a random one. Either way it is sealed into
// the project vault and returned in this response only.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var body webhookWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.validateDestination(r.Context(), body.URL); err != nil {
		apierr.Write(w, err)
		return
	}
	secret := ""
	if body.Secret != nil {
		secret = *body.Secret
	}
	if secret == "" {
		generated, err := NewSigningSecret()
		if err != nil {
			apierr.Write(w, apierr.Internal("internal server error"))
			return
		}
		secret = generated
	}

	created, err := h.repo.Create(r.Context(), projectID, Webhook{
		URL: body.URL, Events: body.Events, Secret: secret, Active: body.Active,
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, secretResponse{Webhook: created.redacted(), Secret: secret})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	var body webhookWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.validateDestination(r.Context(), body.URL); err != nil {
		apierr.Write(w, err)
		return
	}

	updated, err := h.repo.Update(r.Context(), projectID, webhookID, Webhook{
		URL: body.URL, Events: body.Events, Active: body.Active,
	}, body.Secret)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated.redacted())
}

// RotateSecret replaces the webhook's signing secret with a new random value
// this server generates, and returns it once. The previous value stops
// signing at once, so a receiver must switch to the new one.
func (h *Handler) RotateSecret(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	webhookID := chi.URLParam(r, "webhookID")

	secret, err := NewSigningSecret()
	if err != nil {
		apierr.Write(w, apierr.Internal("internal server error"))
		return
	}
	rotated, err := h.repo.RotateSecret(r.Context(), projectID, webhookID, secret)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretResponse{Webhook: rotated.redacted(), Secret: secret})
}

// signingSecretBytes is the entropy of one generated signing secret: 256
// bits, the width of an HMAC-SHA256 output.
const signingSecretBytes = 32

// NewSigningSecret returns one random signing secret, base64url with no
// padding, from the operating system's cryptographically secure source.
func NewSigningSecret() (string, error) {
	raw := make([]byte, signingSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// redacted is the row as a route returns it. The secret fields are already
// `json:"-"`; clearing them as well means a later change to a tag cannot turn
// a response into a disclosure.
func (wh Webhook) redacted() Webhook {
	wh.HasSecret = wh.HasSecret || wh.Secret != "" || wh.SecretRef != ""
	wh.Secret = ""
	wh.SecretRef = ""
	return wh
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

// validateDestination is Create's and Update's shared gate on `url`. It
// answers a 400 whose message names the reason (never a bare 500 and never a
// silent accept) — see the destinationGuard field's doc comment for why a
// nil guard is also a 400 rather than a pass-through.
func (h *Handler) validateDestination(ctx context.Context, destinationURL string) error {
	if h.destinationGuard == nil {
		return apierr.BadRequest("this deployment cannot validate webhook destinations; contact an administrator")
	}
	if err := h.destinationGuard.Validate(ctx, destinationURL); err != nil {
		return apierr.BadRequest(err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
