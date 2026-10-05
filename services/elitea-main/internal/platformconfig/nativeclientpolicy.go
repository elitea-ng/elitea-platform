package platformconfig

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/clientversion"
)

// SectionNativeClientPolicy is the server-driven native client policy
// (ADR-0025 decision 5). The admin Configuration page writes it through the
// generic plugin-config door; internal/application/nativepolicy reads it for
// three consumers: the discovery document's public subset, the `client_policy`
// of every native token response, and the `426` minimum-version gate.
const SectionNativeClientPolicy = "native_client_policy"

// Field keys of SectionNativeClientPolicy.
const (
	KeyNativeRequireDeviceLock    = "require_device_lock"
	KeyNativeIdleLockSeconds      = "idle_lock_seconds"
	KeyNativeAllowScreenshots     = "allow_screenshots"
	KeyNativeOfflineRetentionDays = "offline_retention_days"
	KeyNativeOfflineMaxMB         = "offline_max_mb"
	KeyNativeOfflineAttachments   = "offline_attachments"
	KeyNativeMinClientVersion     = "min_client_version"
)

// Bounds the save path enforces (admin.validateNativeClientPolicyValues). The
// loader re-applies them, so a row written by hand cannot widen them either.
const (
	MaxIdleLockSeconds = 86400
	// MaxOfflineRetentionDays caps how long a client may keep data offline.
	// The sync tombstone retention (ADR-0025 WP6) is derived from it: a
	// cursor can be at most this old, so tombstones must outlive it.
	MaxOfflineRetentionDays = 90
	MaxOfflineMB            = 102400
)

// NativeClientPolicy is the full policy. Field order is the JSON key order of
// the `client_policy` object in a token response; adding a field is additive,
// renaming or removing one is a client_contract major bump.
type NativeClientPolicy struct {
	RequireDeviceLock    bool   `json:"require_device_lock"`
	IdleLockSeconds      int64  `json:"idle_lock_seconds"`
	AllowScreenshots     bool   `json:"allow_screenshots"`
	OfflineRetentionDays int64  `json:"offline_retention_days"`
	OfflineMaxMB         int64  `json:"offline_max_mb"`
	OfflineAttachments   bool   `json:"offline_attachments"`
	MinClientVersion     string `json:"min_client_version"`
}

// DefaultNativeClientPolicy is the policy of a deployment that never saved the
// section: nothing is required of the device, offline use is allowed with
// conservative limits, and every client version is served.
func DefaultNativeClientPolicy() NativeClientPolicy {
	return NativeClientPolicy{
		RequireDeviceLock:    false,
		IdleLockSeconds:      0,
		AllowScreenshots:     true,
		OfflineRetentionDays: 30,
		OfflineMaxMB:         512,
		OfflineAttachments:   true,
		MinClientVersion:     "",
	}
}

// OfflineEnabled is the public bit: retention 0 disables offline storage.
func (p NativeClientPolicy) OfflineEnabled() bool { return p.OfflineRetentionDays > 0 }

// LoadNativeClientPolicy reads the section and overlays it on the defaults. A
// value of the wrong type, out of range or (for the version) unparsable is
// ABSENT and keeps its default — the wrong-type-is-absent rule Values applies.
// A nil pool yields the defaults; a query error yields the error and the
// defaults, and the caller decides (nativepolicy keeps its last good value).
func LoadNativeClientPolicy(ctx context.Context, pool *pgxpool.Pool) (NativeClientPolicy, error) {
	values, err := Load(ctx, pool, SectionNativeClientPolicy)
	if err != nil {
		return DefaultNativeClientPolicy(), err
	}
	return NativeClientPolicyFromValues(values), nil
}

// NativeClientPolicyFromValues is the overlay, separated for tests.
func NativeClientPolicyFromValues(values Values) NativeClientPolicy {
	policy := DefaultNativeClientPolicy()
	policy.RequireDeviceLock = values.Bool(KeyNativeRequireDeviceLock, policy.RequireDeviceLock)
	policy.AllowScreenshots = values.Bool(KeyNativeAllowScreenshots, policy.AllowScreenshots)
	policy.OfflineAttachments = values.Bool(KeyNativeOfflineAttachments, policy.OfflineAttachments)
	if n, ok := values.Int(KeyNativeIdleLockSeconds); ok && n >= 0 && n <= MaxIdleLockSeconds {
		policy.IdleLockSeconds = n
	}
	if n, ok := values.Int(KeyNativeOfflineRetentionDays); ok && n >= 0 && n <= MaxOfflineRetentionDays {
		policy.OfflineRetentionDays = n
	}
	if n, ok := values.Int(KeyNativeOfflineMaxMB); ok && n >= 0 && n <= MaxOfflineMB {
		policy.OfflineMaxMB = n
	}
	if version := values.trimmed(KeyNativeMinClientVersion); version != "" && clientversion.Valid(version) {
		policy.MinClientVersion = version
	}
	return policy
}
