package nativeauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The device registry reads (ADR-0025 decision 4, WP3): every refresh-token
// family is a device a user can see and revoke, and an administrator can see
// and revoke any of.

// Device is one row of the registry.
type Device struct {
	ID            string     `json:"id"`
	UserID        int64      `json:"user_id"`
	Email         string     `json:"email,omitempty"`
	ClientID      string     `json:"client_id"`
	DeviceName    string     `json:"device_name"`
	Platform      string     `json:"platform"`
	ClientVersion string     `json:"client_version"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
	RevokeReason  *string    `json:"revoke_reason"`
	// anchor is the live auth_core__token id; it decides `current`.
	anchor *int64
}

// IsAnchor reports whether this device is anchored on the token row tokenID
// (the caller's own credential when it authenticated with this device).
func (d Device) IsAnchor(tokenID string) bool {
	return d.anchor != nil && tokenID != "" && formatID(*d.anchor) == tokenID
}

// ErrDeviceNotFound is a device that does not exist or is not the caller's:
// the two are one answer, so ownership is not disclosed.
var ErrDeviceNotFound = errors.New("nativeauth: device not found")

const deviceColumns = `s.id::text, s.user_id, COALESCE(owner.email, ''), s.client_id, s.device_name, s.platform,
	s.client_version, s.created_at, s.last_seen_at, s.revoked_at, s.revoke_reason, s.token_id`

func scanDevices(rows pgx.Rows) ([]Device, error) {
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		var device Device
		if err := rows.Scan(&device.ID, &device.UserID, &device.Email, &device.ClientID, &device.DeviceName,
			&device.Platform, &device.ClientVersion, &device.CreatedAt, &device.LastSeenAt, &device.RevokedAt,
			&device.RevokeReason, &device.anchor); err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

// UserDevices lists one user's devices, newest first. Revoked devices are
// included only when asked for.
func (s *Store) UserDevices(ctx context.Context, userID int64, includeRevoked bool) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deviceColumns+`
		FROM elitea_auth.native_sessions AS s
		LEFT JOIN public.auth_core__user AS owner ON owner.id = s.user_id
		WHERE s.user_id = $1 AND ($2 OR s.revoked_at IS NULL)
		ORDER BY s.created_at DESC, s.id`, userID, includeRevoked)
	if err != nil {
		return nil, unavailable("list devices", err)
	}
	devices, err := scanDevices(rows)
	if err != nil {
		return nil, unavailable("list devices", err)
	}
	return devices, nil
}

// RevokeUserDevice revokes one of the user's OWN devices (reason `user`).
// Another user's device, an unknown id and an already-revoked device are all
// ErrDeviceNotFound — except that an already-revoked device of the caller's
// own is a no-op success, so a double click is not an error.
func (s *Store) RevokeUserDevice(ctx context.Context, userID int64, deviceID string) error {
	return s.revokeDevice(ctx, deviceID, &userID, ReasonUser, userID)
}

// AdminDeviceFilter narrows the admin list.
type AdminDeviceFilter struct {
	UserID   int64
	ClientID string
	// State is "active" (the default), "revoked" or "all".
	State  string
	Limit  int
	Offset int
}

// AdminDevices lists devices across users with a total for paging.
func (s *Store) AdminDevices(ctx context.Context, filter AdminDeviceFilter) ([]Device, int64, error) {
	clauses := []string{"TRUE"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if filter.UserID > 0 {
		add("s.user_id = $%d", filter.UserID)
	}
	if filter.ClientID != "" {
		add("s.client_id = $%d", filter.ClientID)
	}
	switch filter.State {
	case "revoked":
		clauses = append(clauses, "s.revoked_at IS NOT NULL")
	case "all":
	default:
		clauses = append(clauses, "s.revoked_at IS NULL")
	}
	where := strings.Join(clauses, " AND ")
	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_auth.native_sessions AS s WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, unavailable("count devices", err)
	}
	limit, offset := filter.Limit, filter.Offset
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, `
		SELECT `+deviceColumns+`
		FROM elitea_auth.native_sessions AS s
		LEFT JOIN public.auth_core__user AS owner ON owner.id = s.user_id
		WHERE `+where+fmt.Sprintf(`
		ORDER BY s.last_seen_at DESC, s.id
		LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, unavailable("list devices", err)
	}
	devices, err := scanDevices(rows)
	if err != nil {
		return nil, 0, unavailable("list devices", err)
	}
	return devices, total, nil
}

// AdminRevokeDevice revokes any device (reason `admin`, revoked_by = actor).
func (s *Store) AdminRevokeDevice(ctx context.Context, deviceID string, actor int64) error {
	return s.revokeDevice(ctx, deviceID, nil, ReasonAdmin, actor)
}

func (s *Store) revokeDevice(ctx context.Context, deviceID string, owner *int64, reason string, actor int64) error {
	if !validUUID(deviceID) {
		return ErrDeviceNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable("begin device revoke", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID int64
	err = tx.QueryRow(ctx, `
		SELECT user_id FROM elitea_auth.native_sessions WHERE id = $1 FOR UPDATE`, deviceID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != nil && userID != *owner) {
		return ErrDeviceNotFound
	}
	if err != nil {
		return unavailable("lock device", err)
	}
	var by *int64
	if actor > 0 {
		by = &actor
	}
	if _, err := revokeFamily(ctx, tx, deviceID, reason, by); err != nil {
		return unavailable("revoke device", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable("commit device revoke", err)
	}
	return nil
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, r := range value {
		switch index {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}
