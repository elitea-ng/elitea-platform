package nativeauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	v2discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/discovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

// The admin registry editor (the `native_clients` Configuration section's
// managed surface). The router gates every route on
// `configuration.native_clients` in the administration mode (coordinator
// decision 6, granted by shared 0141).
//
//	GET    /api/v2/admin/native_clients/administration
//	PUT    /api/v2/admin/native_clients/administration/{client_id}
//	DELETE /api/v2/admin/native_clients/administration/{client_id}

// AdminClientRow is one row of the admin list.
type AdminClientRow struct {
	domain.Client
	ActiveDevices int64 `json:"active_devices"`
}

// AdminList answers the effective registry with each client's live devices.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	clients, err := h.cfg.Registry.Clients(r.Context())
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	counts, err := h.cfg.Store.ActiveDeviceCounts(r.Context())
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	rows := make([]AdminClientRow, 0, len(clients))
	for _, client := range clients {
		rows = append(rows, AdminClientRow{Client: client, ActiveDevices: counts[client.ClientID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

type adminClientBody struct {
	DisplayName  *string  `json:"display_name"`
	RedirectURIs []string `json:"redirect_uris"`
	Enabled      *bool    `json:"enabled"`
}

// AdminSave upserts a DB-layer client. Invalid input is a 422 with a reason per
// field and per redirect URI. Disabling a client revokes its devices in the
// same transaction.
func (h *Handler) AdminSave(w http.ResponseWriter, r *http.Request) {
	clientID := chi.URLParam(r, "clientID")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body adminClientBody
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}
	if h.cfg.PublicOrigin == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "public_origin_required",
			"message": "set DEPLOYMENT_URL to this deployment's public origin before registering a native client: " +
				"it is the issuer every native sign-in carries",
		})
		return
	}
	client := domain.Client{ClientID: clientID, RedirectURIs: body.RedirectURIs, Enabled: true}
	if body.DisplayName != nil {
		client.DisplayName = *body.DisplayName
	}
	if body.Enabled != nil {
		client.Enabled = *body.Enabled
	}
	actor := callerID(r)
	revoked, err := h.cfg.Store.UpsertClient(r.Context(), client, actor)
	var invalid *domain.ClientValidationError
	if errors.As(err, &invalid) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "invalid_native_client", "reasons": invalid.Reasons,
		})
		return
	}
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	h.cfg.Registry.Invalidate()
	action := "Native client saved: " + clientID
	if !client.Enabled {
		action = "Native client disabled: " + clientID
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action: action, EntityType: "native_client", EntityName: clientID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"client_id": clientID, "revoked_devices": revoked})
}

// AdminDelete removes a DB-layer client and revokes its devices. A file entry
// with the same id applies again afterwards.
func (h *Handler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	clientID := chi.URLParam(r, "clientID")
	revoked, err := h.cfg.Store.DeleteClient(r.Context(), clientID, callerID(r))
	if errors.Is(err, domain.ErrClientNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	h.cfg.Registry.Invalidate()
	audit.Annotate(r.Context(), audit.Annotation{
		Action: "Native client removed: " + clientID, EntityType: "native_client", EntityName: clientID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"client_id": clientID, "revoked_devices": revoked})
}

func (h *Handler) adminUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "native clients admin: store did not answer", "err", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
}

func callerID(r *http.Request) int64 {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0
	}
	id, resolved := user.OwningUserID()
	if !resolved {
		return 0
	}
	return id
}

/* ── the discovery seam ─────────────────────────────────────────────────── */

// NativeAuth implements v2discovery.NativeAuthSource: the endpoints while at
// least one client is enabled, nil otherwise (`native_auth: null`).
func (h *Handler) NativeAuth(ctx context.Context, origin string) (*v2discovery.NativeAuth, error) {
	enabled, err := h.cfg.Registry.AnyEnabled(ctx)
	if err != nil || !enabled {
		return nil, err
	}
	issuer := h.cfg.PublicOrigin
	if issuer == "" {
		// Unreachable on a configured deployment: registering a client needs
		// DEPLOYMENT_URL. Publish nothing rather than an issuer the
		// authorization responses would not carry.
		return nil, nil
	}
	_ = origin
	return &v2discovery.NativeAuth{
		Issuer:                        issuer,
		AuthorizationEndpoint:         issuer + AuthorizePath,
		TokenEndpoint:                 issuer + TokenPath,
		RevocationEndpoint:            issuer + RevokePath,
		CodeChallengeMethodsSupported: []string{"S256"},
	}, nil
}
