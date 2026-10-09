package platformconfig

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

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

	// Data controls (client contract 1.3). Each one gates a client feature
	// that moves content across the app's boundary; an enterprise admin can
	// switch each off.
	KeyNativeAllowShareOut            = "allow_share_out"
	KeyNativeAllowShareIn             = "allow_share_in"
	KeyNativeAllowCloudSTT            = "allow_cloud_stt"
	KeyNativeNotificationPreview      = "notification_preview"
	KeyNativeAllowNotificationActions = "allow_notification_actions"
	KeyNativeAllowSystemSurfaces      = "allow_system_surfaces"

	// Local work on the desktop (ADR-0029 decision 6, client contract 1.5).
	// The section stores flat keys, because the admin form and the save
	// validator are flat; the policy delivers them as one `local_work` group.
	KeyNativeLocalWorkAllowed        = "local_work_allowed"
	KeyNativeLocalWorkShell          = "local_work_shell"
	KeyNativeLocalWorkMaxSandboxMode = "local_work_max_sandbox_mode"
	KeyNativeLocalWorkNetwork        = "local_work_network"
	KeyNativeLocalWorkCommandAllow   = "local_work_command_allow"
	KeyNativeLocalWorkCommandDeny    = "local_work_command_deny"
	KeyNativeLocalWorkPathDeny       = "local_work_path_deny"
	KeyNativeLocalWorkLocalMCP       = "local_work_local_mcp"
	KeyNativeLocalWorkLocalIndex     = "local_work_local_index"
	KeyNativeLocalWorkCloudSync      = "local_work_cloud_sync"
	KeyNativeLocalWorkMemoryWrite    = "local_work_memory_write"
)

// The values of KeyNativeLocalWorkMaxSandboxMode, from the most to the least
// confined. The policy names the WIDEST mode the desktop may use; a workspace
// or the user can always choose a narrower one.
const (
	SandboxModeReadOnly       = "read-only"
	SandboxModeWorkspaceWrite = "workspace-write"
	SandboxModeFullAccess     = "full-access"
)

// SandboxModeValues lists the allowed max_sandbox_mode values, most confined
// first, in the order the admin form offers them.
func SandboxModeValues() []string {
	return []string{SandboxModeReadOnly, SandboxModeWorkspaceWrite, SandboxModeFullAccess}
}

// ValidSandboxMode reports whether value is an allowed max_sandbox_mode,
// compared exactly.
func ValidSandboxMode(value string) bool {
	for _, allowed := range SandboxModeValues() {
		if value == allowed {
			return true
		}
	}
	return false
}

// Bounds of the local_work pattern lists (command_allow, command_deny,
// path_deny). The save path refuses a list over them; the loader keeps only
// valid patterns and at most MaxLocalWorkPatterns of them.
const (
	MaxLocalWorkPatterns     = 200
	MaxLocalWorkPatternBytes = 512
)

// ValidLocalWorkPattern reports whether one command or path pattern may be
// stored: not blank, at most MaxLocalWorkPatternBytes, valid UTF-8 and free of
// control characters (a pattern is shown to the user and matched by the
// desktop; a newline or NUL in it is never intended).
func ValidLocalWorkPattern(pattern string) bool {
	if strings.TrimSpace(pattern) == "" || len(pattern) > MaxLocalWorkPatternBytes || !utf8.ValidString(pattern) {
		return false
	}
	for _, r := range pattern {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// LocalWorkPolicy is the `local_work` group of the policy (ADR-0029 decision
// 6). The desktop enforces it. The server enforces only `allowed`: it refuses
// a local turn start when it is false. Field order is the JSON key order.
type LocalWorkPolicy struct {
	// Allowed: local work (agent turns that run on the user's machine) is
	// permitted. Off unless an administrator enables it.
	Allowed bool `json:"allowed"`
	// Shell: local turns may run shell commands.
	Shell bool `json:"shell"`
	// MaxSandboxMode is the widest sandbox mode the desktop may use, one of
	// SandboxModeValues.
	MaxSandboxMode string `json:"max_sandbox_mode"`
	// Network: sandboxed commands may reach the network.
	Network bool `json:"network"`
	// CommandAllow and CommandDeny are command patterns the desktop's approval
	// rules apply before any workspace or user rule. A deny wins.
	CommandAllow []string `json:"command_allow"`
	CommandDeny  []string `json:"command_deny"`
	// PathDeny are path patterns local tools must not read or write.
	PathDeny []string `json:"path_deny"`
	// LocalMCP: the user may configure local (stdio) MCP servers.
	LocalMCP bool `json:"local_mcp"`
	// LocalIndex: the desktop may build a local index of a workspace.
	LocalIndex bool `json:"local_index"`
	// CloudSync: a workspace folder may be synced to an artifact bucket.
	CloudSync bool `json:"cloud_sync"`
	// MemoryWrite: local turns may save to cloud memory. Recall stays allowed.
	MemoryWrite bool `json:"memory_write"`
}

// DefaultLocalWorkPolicy is the group of a deployment that never saved it:
// local work is off. The other values apply once an administrator turns it
// on, and they are the conservative ones: workspace-write sandbox without
// network, no local MCP servers, no cloud sync.
func DefaultLocalWorkPolicy() LocalWorkPolicy {
	return LocalWorkPolicy{
		Allowed:        false,
		Shell:          true,
		MaxSandboxMode: SandboxModeWorkspaceWrite,
		Network:        false,
		CommandAllow:   []string{},
		CommandDeny:    []string{},
		PathDeny:       []string{},
		LocalMCP:       false,
		LocalIndex:     true,
		CloudSync:      false,
		MemoryWrite:    true,
	}
}

func localWorkPolicyFromValues(values Values) LocalWorkPolicy {
	policy := DefaultLocalWorkPolicy()
	policy.Allowed = values.Bool(KeyNativeLocalWorkAllowed, policy.Allowed)
	policy.Shell = values.Bool(KeyNativeLocalWorkShell, policy.Shell)
	if mode, ok := values[KeyNativeLocalWorkMaxSandboxMode].(string); ok && ValidSandboxMode(mode) {
		policy.MaxSandboxMode = mode
	}
	policy.Network = values.Bool(KeyNativeLocalWorkNetwork, policy.Network)
	policy.CommandAllow = localWorkPatterns(values, KeyNativeLocalWorkCommandAllow)
	policy.CommandDeny = localWorkPatterns(values, KeyNativeLocalWorkCommandDeny)
	policy.PathDeny = localWorkPatterns(values, KeyNativeLocalWorkPathDeny)
	policy.LocalMCP = values.Bool(KeyNativeLocalWorkLocalMCP, policy.LocalMCP)
	policy.LocalIndex = values.Bool(KeyNativeLocalWorkLocalIndex, policy.LocalIndex)
	policy.CloudSync = values.Bool(KeyNativeLocalWorkCloudSync, policy.CloudSync)
	policy.MemoryWrite = values.Bool(KeyNativeLocalWorkMemoryWrite, policy.MemoryWrite)
	return policy
}

// localWorkPatterns keeps the valid patterns of a stored list, at most
// MaxLocalWorkPatterns. It never answers nil, so the JSON is always an array.
func localWorkPatterns(values Values, key string) []string {
	out := []string{}
	for _, pattern := range values.Strings(key) {
		if len(out) == MaxLocalWorkPatterns {
			break
		}
		if ValidLocalWorkPattern(pattern) {
			out = append(out, pattern)
		}
	}
	return out
}

// The values of KeyNativeNotificationPreview: what a notification may show on
// the lock screen and in the notification centre.
const (
	// NotificationPreviewNone: a generic text only ("New activity").
	NotificationPreviewNone = "none"
	// NotificationPreviewTitle: the conversation or item title, never content.
	NotificationPreviewTitle = "title"
)

// NotificationPreviewValues lists the allowed notification_preview values in
// the order the admin form offers them.
func NotificationPreviewValues() []string {
	return []string{NotificationPreviewNone, NotificationPreviewTitle}
}

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

	// Data controls (client contract 1.3).
	//
	// AllowShareOut: copy, share and export of messages and transcripts.
	AllowShareOut bool `json:"allow_share_out"`
	// AllowShareIn: the system share sheet may send content into the app.
	AllowShareIn bool `json:"allow_share_in"`
	// AllowCloudSTT: dictation may use a speech recogniser that sends audio
	// off the device (the platform's server recogniser or the workspace's
	// transcription model). Off means on-device recognition only.
	AllowCloudSTT bool `json:"allow_cloud_stt"`
	// NotificationPreview is NotificationPreviewNone or NotificationPreviewTitle.
	NotificationPreview string `json:"notification_preview"`
	// AllowNotificationActions: actions on a notification (mark read, open an
	// approval) without opening the app first. A decision always needs the
	// app unlocked.
	AllowNotificationActions bool `json:"allow_notification_actions"`
	// AllowSystemSurfaces: widgets, quick actions and other surfaces outside
	// the app may show titles. Off means they show counts only.
	AllowSystemSurfaces bool `json:"allow_system_surfaces"`

	// LocalWork is the desktop's local work group (client contract 1.5).
	LocalWork LocalWorkPolicy `json:"local_work"`
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

		AllowShareOut:            true,
		AllowShareIn:             true,
		AllowCloudSTT:            false,
		NotificationPreview:      NotificationPreviewNone,
		AllowNotificationActions: true,
		AllowSystemSurfaces:      false,

		LocalWork: DefaultLocalWorkPolicy(),
	}
}

// ValidNotificationPreview reports whether value is an allowed
// notification_preview, compared exactly (no trimming or case folding).
func ValidNotificationPreview(value string) bool {
	for _, allowed := range NotificationPreviewValues() {
		if value == allowed {
			return true
		}
	}
	return false
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
	policy.AllowShareOut = values.Bool(KeyNativeAllowShareOut, policy.AllowShareOut)
	policy.AllowShareIn = values.Bool(KeyNativeAllowShareIn, policy.AllowShareIn)
	policy.AllowCloudSTT = values.Bool(KeyNativeAllowCloudSTT, policy.AllowCloudSTT)
	policy.AllowNotificationActions = values.Bool(KeyNativeAllowNotificationActions, policy.AllowNotificationActions)
	policy.AllowSystemSurfaces = values.Bool(KeyNativeAllowSystemSurfaces, policy.AllowSystemSurfaces)
	if preview, ok := values[KeyNativeNotificationPreview].(string); ok && ValidNotificationPreview(preview) {
		policy.NotificationPreview = preview
	}
	policy.LocalWork = localWorkPolicyFromValues(values)
	return policy
}
