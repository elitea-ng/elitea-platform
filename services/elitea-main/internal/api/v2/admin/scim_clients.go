package admin

// The admin surface of the dedicated SCIM client credentials (shared migration
// 0134).
//
// An identity provider authenticates to the SCIM tree with a SCIM CLIENT, not
// with a person's personal access token. This surface creates, lists, rotates,
// revokes and deletes those clients. A secret is returned ONCE, in the answer
// to the create or rotate that minted it. No read returns it again: the store
// keeps only its hash.
//
// # The permission
//
// `admin.auth.users` in administration mode — the permission the SCIM tree
// itself required before this change, and the one the admin Users page's write
// routes carry. A SCIM client can create and deactivate every account, so the
// right to mint one must not be weaker than the right to do that by hand. A
// separate, weaker permission (for example the `runtime.plugins` that gates the
// identity provider editor beside it) would let its holder mint a credential
// with more power than they have. No new permission string arrives, so the
// grant gate in router_permission_grant_gate_test.go stays untripped.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimclient"
)

// The two paths an identity provider is configured with. They are stated here
// as well as in internal/api/scim because importing that package from here
// would make the admin package depend on the SCIM HTTP tree.
const (
	scimBasePath          = "/api/v2/scim/v2"
	scimTokenEndpointPath = "/api/v2/scim/oauth/token"
)

// SCIMClientStore is the store seam.
type SCIMClientStore interface {
	List(ctx context.Context) ([]scimclient.Client, error)
	Create(ctx context.Context, name, method string, createdBy *int) (scimclient.Issued, error)
	Rotate(ctx context.Context, id int64) (scimclient.Issued, error)
	Revoke(ctx context.Context, id int64) (scimclient.Client, error)
	Delete(ctx context.Context, id int64) error
	AccessTokenTTL() time.Duration
}

// WithSCIMClients supplies the SCIM client store.
func WithSCIMClients(store SCIMClientStore) Option {
	return func(h *Handler) {
		if store == nil {
			return
		}
		h.scimClients = store
	}
}

// scimClientView is the wire shape of one client. It has no secret field, so a
// read cannot return one by mistake.
type scimClientView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	AuthMethod    string  `json:"auth_method"`
	ClientID      string  `json:"client_id,omitempty"`
	SecretHint    string  `json:"secret_hint"`
	Status        string  `json:"status"`
	CreatedBy     *int    `json:"created_by"`
	CreatedByName string  `json:"created_by_name,omitempty"`
	CreatedAt     string  `json:"created_at"`
	LastUsedAt    *string `json:"last_used_at"`
	RotatedAt     *string `json:"rotated_at"`
	RevokedAt     *string `json:"revoked_at"`
	ExpiresAt     *string `json:"expires_at"`
}

func scimTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func scimClientToView(client scimclient.Client) scimClientView {
	status := "active"
	switch {
	case client.RevokedAt != nil:
		status = "revoked"
	case !client.Active(time.Now()):
		status = "expired"
	}
	return scimClientView{
		ID:            strconv.FormatInt(client.ID, 10),
		Name:          client.Name,
		AuthMethod:    client.AuthMethod,
		ClientID:      client.ClientID,
		SecretHint:    client.SecretHint,
		Status:        status,
		CreatedBy:     client.CreatedBy,
		CreatedByName: client.CreatedByName,
		CreatedAt:     client.CreatedAt.UTC().Format(time.RFC3339),
		LastUsedAt:    scimTime(client.LastUsedAt),
		RotatedAt:     scimTime(client.RotatedAt),
		RevokedAt:     scimTime(client.RevokedAt),
		ExpiresAt:     scimTime(client.ExpiresAt),
	}
}

func (h *Handler) scimClientsReady(w http.ResponseWriter) bool {
	if h.scimClients != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable,
		map[string]any{"error": "SCIM provisioning is not available on this deployment"})
	return false
}

// noStoreSecret marks an answer that carries a secret. The `/api/v2` group
// sets no-store already; this keeps the guarantee if the route ever moves.
func noStoreSecret(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// SCIMClientList answers `GET /admin/scim_clients/administration`.
func (h *Handler) SCIMClientList(w http.ResponseWriter, r *http.Request) {
	if !h.scimClientsReady(w) {
		return
	}
	clients, err := h.scimClients.List(r.Context())
	if err != nil {
		slog.Error("admin: SCIM client list failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the SCIM clients could not be read"})
		return
	}
	views := make([]scimClientView, 0, len(clients))
	for _, client := range clients {
		views = append(views, scimClientToView(client))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clients":                  views,
		"total":                    len(views),
		"scim_base_path":           scimBasePath,
		"token_endpoint_path":      scimTokenEndpointPath,
		"access_token_ttl_seconds": int(h.scimClients.AccessTokenTTL().Seconds()),
	})
}

type scimClientCreateBody struct {
	Name       string `json:"name"`
	AuthMethod string `json:"auth_method"`
}

// issuedBody is the answer that carries a secret, once.
func issuedBody(issued scimclient.Issued) map[string]any {
	body := map[string]any{
		"client":              scimClientToView(issued.Client),
		"secret":              issued.Secret,
		"scim_base_path":      scimBasePath,
		"token_endpoint_path": scimTokenEndpointPath,
	}
	if issued.Client.ClientID != "" {
		body["client_id"] = issued.Client.ClientID
	}
	return body
}

// SCIMClientCreate answers `POST /admin/scim_clients/administration`.
func (h *Handler) SCIMClientCreate(w http.ResponseWriter, r *http.Request) {
	if !h.scimClientsReady(w) {
		return
	}
	var body scimClientCreateBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the request body is not a valid SCIM client"})
		return
	}
	var createdBy *int
	if principal, ok := auth.UserFromContext(r.Context()); ok {
		if id, resolved := principal.OwningUserID(); resolved && id > 0 && id <= int64(^uint32(0)>>1) {
			value := int(id)
			createdBy = &value
		}
	}
	issued, err := h.scimClients.Create(r.Context(), body.Name, body.AuthMethod, createdBy)
	switch {
	case errors.Is(err, scimclient.ErrInvalidName), errors.Is(err, scimclient.ErrInvalidMethod):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	case errors.Is(err, scimclient.ErrDuplicateName):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	case err != nil:
		slog.Error("admin: SCIM client create failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the SCIM client could not be created"})
		return
	}
	noStoreSecret(w)
	writeJSON(w, http.StatusCreated, issuedBody(issued))
}

func scimClientID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no SCIM client has that id"})
		return 0, false
	}
	return id, true
}

// SCIMClientRotate answers `POST /admin/scim_clients/administration/{id}/rotate`.
// The old secret stops working at once. There is no grace window: an operator
// rotates because a secret may have leaked, and a window would keep the leaked
// value valid.
func (h *Handler) SCIMClientRotate(w http.ResponseWriter, r *http.Request) {
	if !h.scimClientsReady(w) {
		return
	}
	id, ok := scimClientID(w, r)
	if !ok {
		return
	}
	issued, err := h.scimClients.Rotate(r.Context(), id)
	switch {
	case errors.Is(err, scimclient.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no SCIM client has that id"})
		return
	case errors.Is(err, scimclient.ErrRevoked):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a revoked SCIM client cannot be rotated; create a new one"})
		return
	case err != nil:
		slog.Error("admin: SCIM client rotate failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the SCIM client could not be rotated"})
		return
	}
	noStoreSecret(w)
	writeJSON(w, http.StatusOK, issuedBody(issued))
}

// SCIMClientRevoke answers `POST /admin/scim_clients/administration/{id}/revoke`.
func (h *Handler) SCIMClientRevoke(w http.ResponseWriter, r *http.Request) {
	if !h.scimClientsReady(w) {
		return
	}
	id, ok := scimClientID(w, r)
	if !ok {
		return
	}
	client, err := h.scimClients.Revoke(r.Context(), id)
	switch {
	case errors.Is(err, scimclient.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no SCIM client has that id"})
		return
	case err != nil:
		slog.Error("admin: SCIM client revoke failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the SCIM client could not be revoked"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": scimClientToView(client)})
}

// SCIMClientDelete answers `DELETE /admin/scim_clients/administration/{id}`.
func (h *Handler) SCIMClientDelete(w http.ResponseWriter, r *http.Request) {
	if !h.scimClientsReady(w) {
		return
	}
	id, ok := scimClientID(w, r)
	if !ok {
		return
	}
	err := h.scimClients.Delete(r.Context(), id)
	switch {
	case errors.Is(err, scimclient.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no SCIM client has that id"})
		return
	case err != nil:
		slog.Error("admin: SCIM client delete failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the SCIM client could not be deleted"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
