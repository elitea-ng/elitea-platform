package nativeauth

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

// The device registry surfaces (ADR-0025 decision 4, WP3).
//
// User routes, inside the /api/v2 group (any credential the group accepts):
//
//	GET    /api/v2/auth/native/devices?include_revoked=false
//	DELETE /api/v2/auth/native/devices/{deviceID}
//
// Like every /api/v2/auth/native/* route they answer 404 while no native
// client is registered (coordinator decision 4).
//
// Admin routes, gated on `admin.auth.users` (the permission that already gates
// user suspension and SCIM clients), always mounted:
//
//	GET    /api/v2/admin/native_devices/administration?user_id=&client_id=&state=&limit=&offset=
//	DELETE /api/v2/admin/native_devices/administration/{deviceID}

// DevicesPath is the user device list.
const DevicesPath = BasePath + "/devices"

// UserDevice is one row of the user's list.
type UserDevice struct {
	ID            string     `json:"id"`
	ClientID      string     `json:"client_id"`
	ClientName    string     `json:"client_name"`
	DeviceName    string     `json:"device_name"`
	Platform      string     `json:"platform"`
	ClientVersion string     `json:"client_version"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
	RevokeReason  *string    `json:"revoke_reason"`
	// Current is true on the device whose token authenticated this request.
	Current bool `json:"current"`
}

// AdminDevice is one row of the admin list.
type AdminDevice struct {
	UserDevice
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
}

// RegisteredOnly answers 404 while no client is registered, for the user
// routes mounted inside the /api/v2 group.
func (h *Handler) RegisteredOnly(next http.Handler) http.Handler {
	return h.registered(next.ServeHTTP, true)
}

func (h *Handler) clientNames(r *http.Request) map[string]string {
	names := map[string]string{}
	clients, err := h.cfg.Registry.Clients(r.Context())
	if err != nil {
		return names
	}
	for _, client := range clients {
		names[client.ClientID] = client.DisplayName
	}
	return names
}

func userDevice(device domain.Device, names map[string]string, callerToken string) UserDevice {
	name := names[device.ClientID]
	if name == "" {
		name = device.ClientID
	}
	return UserDevice{
		ID: device.ID, ClientID: device.ClientID, ClientName: name, DeviceName: device.DeviceName,
		Platform: device.Platform, ClientVersion: device.ClientVersion, CreatedAt: device.CreatedAt,
		LastSeenAt: device.LastSeenAt, RevokedAt: device.RevokedAt, RevokeReason: device.RevokeReason,
		Current: device.IsAnchor(callerToken),
	}
}

// ListDevices answers the caller's own devices.
func (h *Handler) ListDevices(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	userID, resolved := user.OwningUserID()
	if !ok || !resolved {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	includeRevoked := r.URL.Query().Get("include_revoked") == "true"
	devices, err := h.cfg.Store.UserDevices(r.Context(), userID, includeRevoked)
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	names := h.clientNames(r)
	rows := make([]UserDevice, 0, len(devices))
	for _, device := range devices {
		rows = append(rows, userDevice(device, names, user.TokenID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": rows})
}

// RevokeDevice revokes one of the caller's own devices. Another user's id is
// a 404, so ownership is not disclosed. Revoking the device the caller is
// using is allowed; its next call answers device_revoked.
func (h *Handler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	userID, resolved := user.OwningUserID()
	if !ok || !resolved {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	deviceID := chi.URLParam(r, "deviceID")
	err := h.cfg.Store.RevokeUserDevice(r.Context(), userID, deviceID)
	if errors.Is(err, domain.ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action: "Native device revoked by its owner", EntityType: "native_device", EntityName: deviceID,
	})
	w.WriteHeader(http.StatusNoContent)
}

// AdminListDevices answers devices across users.
func (h *Handler) AdminListDevices(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := domain.AdminDeviceFilter{ClientID: query.Get("client_id"), State: query.Get("state")}
	for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		if raw := query.Get(name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": name + " must be a non-negative integer"})
				return
			}
			*target = value
		}
	}
	if raw := query.Get("user_id"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id must be a positive integer"})
			return
		}
		filter.UserID = value
	}
	switch filter.State {
	case "", "active", "revoked", "all":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state must be active, revoked or all"})
		return
	}
	devices, total, err := h.cfg.Store.AdminDevices(r.Context(), filter)
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	names := h.clientNames(r)
	caller, _ := auth.UserFromContext(r.Context())
	rows := make([]AdminDevice, 0, len(devices))
	for _, device := range devices {
		rows = append(rows, AdminDevice{
			UserDevice: userDevice(device, names, caller.TokenID), UserID: device.UserID, Email: device.Email,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

// AdminRevokeDevice revokes any device and records who did it.
func (h *Handler) AdminRevokeDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	err := h.cfg.Store.AdminRevokeDevice(r.Context(), deviceID, callerID(r))
	if errors.Is(err, domain.ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err != nil {
		h.adminUnavailable(w, r, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action: "Native device revoked by an administrator", EntityType: "native_device", EntityName: deviceID,
	})
	w.WriteHeader(http.StatusNoContent)
}
