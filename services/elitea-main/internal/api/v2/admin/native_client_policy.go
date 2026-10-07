package admin

import (
	"fmt"
	"math"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/clientversion"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The `native_client_policy` Configuration section (ADR-0025 decision 5,
// WP4). It is LIVE — no unavailable_reason — because three consumers read it
// through internal/application/nativepolicy: the discovery document's public
// subset, the `client_policy` object of every native token response, and the
// 426 minimum-version gate (apimw.ClientVersion and the token endpoint). The
// device-side fields (lock, screenshots, offline limits) are enforced by the
// client; the server's part is to deliver them with every token.

// PolicyInvalidator drops a cached policy after a save (*nativepolicy.Service).
type PolicyInvalidator interface {
	Invalidate()
}

// WithNativeClientPolicy supplies the cache a save invalidates. Without it a
// save still lands; replicas pick it up within the cache TTL.
func WithNativeClientPolicy(policy PolicyInvalidator) Option {
	return func(h *Handler) {
		if policy == nil {
			return
		}
		h.nativePolicy = policy
	}
}

func (h *Handler) invalidateNativeClientPolicy() {
	if h.nativePolicy != nil {
		h.nativePolicy.Invalidate()
	}
}

// nativeClientPolicySection is declared in config_schemas.go's list. Its
// permission is the one both native admin surfaces share (coordinator
// decision 6), granted by shared 0141 — no migration of its own.
func nativeClientPolicySection() map[string]any {
	const section = platformconfig.SectionNativeClientPolicy
	defaults := platformconfig.DefaultNativeClientPolicy()
	return map[string]any{
		"id":    section,
		"title": "Native client policy",
		"description": "What this deployment requires of its mobile and desktop apps. The policy is sent with " +
			"every token an app receives, so a change reaches signed-in devices within one access-token " +
			"lifetime. The app enforces the device settings; the server refuses app versions below the minimum.",
		"order":               7,
		"icon":                "phonelink_lock",
		"always_visible":      true,
		"required_permission": "configuration.native_clients",
		"fields": []map[string]any{
			{
				"key":         platformconfig.KeyNativeRequireDeviceLock,
				"type":        "boolean",
				"title":       "Require Device Lock",
				"description": "The app refuses to run on a device without a passcode or biometric lock.",
				"section":     section,
				"default":     defaults.RequireDeviceLock,
			},
			{
				"key":   platformconfig.KeyNativeIdleLockSeconds,
				"type":  "integer",
				"title": "Idle Lock (seconds)",
				"description": fmt.Sprintf("Lock the app after this many seconds in the background. 0 never "+
					"locks. At most %d.", platformconfig.MaxIdleLockSeconds),
				"section": section,
				"default": defaults.IdleLockSeconds,
				"minimum": 0,
				"maximum": platformconfig.MaxIdleLockSeconds,
			},
			{
				"key":         platformconfig.KeyNativeAllowScreenshots,
				"type":        "boolean",
				"title":       "Allow Screenshots",
				"description": "When off, the app blocks screenshots and screen recording where the platform allows it.",
				"section":     section,
				"default":     defaults.AllowScreenshots,
			},
			{
				"key":   platformconfig.KeyNativeOfflineRetentionDays,
				"type":  "integer",
				"title": "Offline Retention (days)",
				"description": fmt.Sprintf("How long the app may keep conversations on the device without "+
					"reaching this deployment. 0 disables offline storage. At most %d.",
					platformconfig.MaxOfflineRetentionDays),
				"section": section,
				"default": defaults.OfflineRetentionDays,
				"minimum": 0,
				"maximum": platformconfig.MaxOfflineRetentionDays,
			},
			{
				"key":         platformconfig.KeyNativeOfflineMaxMB,
				"type":        "integer",
				"title":       "Offline Storage Limit (MB)",
				"description": "The most the app may store on the device for offline use.",
				"section":     section,
				"default":     defaults.OfflineMaxMB,
				"minimum":     0,
				"maximum":     platformconfig.MaxOfflineMB,
			},
			{
				"key":         platformconfig.KeyNativeOfflineAttachments,
				"type":        "boolean",
				"title":       "Offline Attachments",
				"description": "Whether attachments are kept on the device for offline use, or only messages.",
				"section":     section,
				"default":     defaults.OfflineAttachments,
			},
			{
				"key":   platformconfig.KeyNativeMinClientVersion,
				"type":  "string",
				"title": "Minimum App Version",
				"description": "Apps older than this version (for example 1.4.0) are told to update and refused " +
					"with 426. Leave empty to serve every version. A registered app can set a higher minimum " +
					"of its own.",
				"section": section,
				"default": defaults.MinClientVersion,
			},
			// Data controls (client contract 1.3). The app enforces each one;
			// discovery publishes them so a share extension or a widget obeys
			// them before the app holds a token.
			{
				"key":         platformconfig.KeyNativeAllowShareOut,
				"type":        "boolean",
				"title":       "Allow Copy, Share and Export",
				"description": "Whether people may copy, share or export messages and transcripts out of the app.",
				"section":     section,
				"default":     defaults.AllowShareOut,
			},
			{
				"key":         platformconfig.KeyNativeAllowShareIn,
				"type":        "boolean",
				"title":       "Allow Sharing Into the App",
				"description": "Whether other apps may send text, links, files or photos to an agent through the system share sheet.",
				"section":     section,
				"default":     defaults.AllowShareIn,
			},
			{
				"key":   platformconfig.KeyNativeAllowCloudSTT,
				"type":  "boolean",
				"title": "Allow Cloud Speech Recognition",
				"description": "When off, dictation runs on the device only. When on, the app may use a recogniser " +
					"that sends audio off the device.",
				"section": section,
				"default": defaults.AllowCloudSTT,
			},
			{
				"key":   platformconfig.KeyNativeNotificationPreview,
				"type":  "string",
				"title": "Notification Preview",
				"description": "What a notification shows on the lock screen: `none` shows a generic text only, " +
					"`title` shows the conversation or item title. Message content is never shown.",
				"section": section,
				"default": defaults.NotificationPreview,
				"enum":    platformconfig.NotificationPreviewValues(),
			},
			{
				"key":         platformconfig.KeyNativeAllowNotificationActions,
				"type":        "boolean",
				"title":       "Allow Notification Actions",
				"description": "Whether a notification offers actions such as Mark as read. A decision always needs the app unlocked.",
				"section":     section,
				"default":     defaults.AllowNotificationActions,
			},
			{
				"key":   platformconfig.KeyNativeAllowSystemSurfaces,
				"type":  "boolean",
				"title": "Show Titles on Widgets and Quick Actions",
				"description": "When off, widgets and home-screen quick actions show counts only. When on, they may " +
					"show conversation and agent titles, which are then visible outside the app's lock.",
				"section": section,
				"default": defaults.AllowSystemSurfaces,
			},
		},
	}
}

// validateNativeClientPolicyValues applies the rules the schema cannot: integer
// ranges (JSON numbers arrive as float64, so integrality too), the version
// grammar, and the notification_preview values (also an `enum` in the schema;
// checked here as well so this function alone is the whole rule). The generic validator has already checked each value's JSON type.
func validateNativeClientPolicyValues(values map[string]any) string {
	bounds := map[string]int64{
		platformconfig.KeyNativeIdleLockSeconds:      platformconfig.MaxIdleLockSeconds,
		platformconfig.KeyNativeOfflineRetentionDays: platformconfig.MaxOfflineRetentionDays,
		platformconfig.KeyNativeOfflineMaxMB:         platformconfig.MaxOfflineMB,
	}
	for _, key := range []string{
		platformconfig.KeyNativeIdleLockSeconds,
		platformconfig.KeyNativeOfflineRetentionDays,
		platformconfig.KeyNativeOfflineMaxMB,
	} {
		raw, present := values[key]
		if !present {
			continue
		}
		number, ok := raw.(float64)
		if !ok || math.IsNaN(number) || number != math.Trunc(number) || number < 0 || number > float64(bounds[key]) {
			return fmt.Sprintf("%q must be a whole number from 0 to %d", key, bounds[key])
		}
	}
	if raw, present := values[platformconfig.KeyNativeNotificationPreview]; present {
		preview, _ := raw.(string)
		if !platformconfig.ValidNotificationPreview(preview) {
			return fmt.Sprintf("%q must be one of: %s", platformconfig.KeyNativeNotificationPreview,
				strings.Join(platformconfig.NotificationPreviewValues(), ", "))
		}
	}
	if raw, present := values[platformconfig.KeyNativeMinClientVersion]; present {
		version, _ := raw.(string)
		if version != "" && !clientversion.Valid(version) {
			return fmt.Sprintf("%q must be empty or a version MAJOR.MINOR.PATCH with an optional -prerelease "+
				"(for example 1.4.0)", platformconfig.KeyNativeMinClientVersion)
		}
	}
	return ""
}
