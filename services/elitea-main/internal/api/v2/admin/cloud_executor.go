package admin

import (
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// cloudExecutorSection is the platform ceiling of the desktop ⇄ cloud handoff
// (ADR-0034 decision 11). It is LIVE: every send and every cloud turn claim
// reads it (internal/application/workspacehandoff.PolicyService), narrowed by
// the project setting and the native client policy's switch. Its permission
// is the one the native client admin surfaces share (shared 0141), so no
// migration seeds anything. The sandbox resources (turn wall time, CPU,
// memory, disk) are chart values, not fields here.
func cloudExecutorSection() map[string]any {
	const section = platformconfig.SectionCloudExecutor
	defaults := platformconfig.DefaultCloudExecutorSettings()
	integer := func(key, title, description string, value, minimum, maximum int64) map[string]any {
		return map[string]any{
			"key": key, "type": "integer", "title": title, "description": description,
			"section": section, "default": value, "minimum": minimum, "maximum": maximum,
		}
	}
	patterns := func(key, title, description string, value []any) map[string]any {
		return map[string]any{
			"key": key, "type": "array", "items": map[string]any{"type": "string"}, "title": title,
			"description": description, "section": section, "default": value,
			"maxItems": platformconfig.MaxCloudExecutorPatterns,
		}
	}
	return map[string]any{
		"id":    section,
		"title": "Cloud executor",
		"description": "Whether desktop Local work threads can be sent to a cloud executor, and its limits. The " +
			"project settings can only narrow these values. The executor needs object storage with default " +
			"encryption at rest; the deployment refuses to enable it otherwise.",
		"order":               8,
		"icon":                "cloud_sync",
		"always_visible":      true,
		"required_permission": "configuration.native_clients",
		"fields": []map[string]any{
			{"key": platformconfig.KeyCloudExecutorEnabled, "type": "boolean", "title": "Enable the Cloud Executor",
				"description": "Allow Send to cloud from the desktop.", "section": section, "default": defaults.Enabled},
			{"key": platformconfig.KeyCloudExecutorProjectDefaultEnabled, "type": "boolean", "title": "Default for Projects",
				"description": "Whether a project that has not turned the cloud executor on or off has it on. " +
					"Existing projects follow this default too. A project can still turn it on or off itself; " +
					"with the cloud executor disabled above, it is off everywhere.",
				"section": section, "default": defaults.ProjectDefaultEnabled},
			{"key": platformconfig.KeyCloudExecutorShell, "type": "boolean", "title": "Allow Shell Commands",
				"description": "Whether cloud turns may run shell commands in their sandbox.", "section": section,
				"default": defaults.Shell},
			{"key": platformconfig.KeyCloudExecutorNetwork, "type": "boolean", "title": "Allow Network",
				"description": "Whether the cloud sandbox may reach the public internet.", "section": section,
				"default": defaults.Network},
			{"key": platformconfig.KeyCloudExecutorNetworkAllowlist, "type": "array", "items": map[string]any{"type": "string"},
				"title": "Network Allowlist",
				"description": fmt.Sprintf("Host names the sandbox may reach; empty means the public internet. At most %d.",
					platformconfig.MaxCloudExecutorAllowlistHosts),
				"section": section, "default": []any{}, "maxItems": platformconfig.MaxCloudExecutorAllowlistHosts},
			{"key": platformconfig.KeyCloudExecutorHistoryFullAllowed, "type": "boolean", "title": "Allow Full History",
				"description": "Whether a send may carry the repository history, not only the current tree.",
				"section":     section, "default": defaults.HistoryFullAllowed},
			patterns(platformconfig.KeyCloudExecutorExclusions, "Exclusions",
				"Files a send never uploads, as path patterns (for example .env or *.pem), even when they are "+
					"tracked. A project can add patterns but cannot remove these.", toAny(defaults.Exclusions)),
			{"key": platformconfig.KeyCloudExecutorAllowUserInclude, "type": "boolean", "title": "Allow Explicit Include",
				"description": "Whether a user may send a file the exclusions match, for example a .env the cloud " +
					"build needs. The local path deny list can never be included.",
				"section": section, "default": defaults.AllowUserInclude},
			{"key": platformconfig.KeyCloudExecutorEnvHarvest, "type": "boolean", "title": "Allow Environment Variables",
				"description": "Whether a user may send selected environment variables, which the cloud sandbox's " +
					"commands receive. They are stored in the vault, never in the uploaded files.",
				"section": section, "default": defaults.EnvHarvest},
			patterns(platformconfig.KeyCloudExecutorEnvDeny, "Denied Variable Names",
				"Environment variable names (patterns such as SSH_*) that are never offered. A project can add names.",
				toAny(defaults.EnvDeny)),
			patterns(platformconfig.KeyCloudExecutorEnvAllow, "Allowed Variable Names",
				"When set, the only environment variable names that can be sent.", []any{}),
			integer(platformconfig.KeyCloudExecutorMaxSegmentMB, "Maximum Upload (MB)", "The largest bundle one send uploads.",
				defaults.MaxSegmentMB, 1, platformconfig.MaxCloudExecutorSegmentMB),
			integer(platformconfig.KeyCloudExecutorMaxFileMB, "Maximum File Size (MB)", "The largest single file a send carries.",
				defaults.MaxFileMB, 1, platformconfig.MaxCloudExecutorFileMB),
			integer(platformconfig.KeyCloudExecutorMaxFiles, "Maximum Files", "The most files a send carries.",
				defaults.MaxFiles, 1, platformconfig.MaxCloudExecutorFiles),
			integer(platformconfig.KeyCloudExecutorMaxUnpackedMB, "Maximum Unpacked Size (MB)", "The largest working tree a send carries.",
				defaults.MaxUnpackedMB, 1, platformconfig.MaxCloudExecutorUnpackedMB),
			integer(platformconfig.KeyCloudExecutorMaxProjectWorkspaceGB, "Cloud Copies per Project (GB)",
				"The storage all cloud copies of one project may use. It does not count against the artifact quota.",
				defaults.MaxProjectWorkspaceGB, 1, platformconfig.MaxCloudExecutorProjectWorkspaceGB),
			integer(platformconfig.KeyCloudExecutorIdleRetentionDays, "Idle Retention (days)",
				"A cloud copy nobody used for this long is erased.", defaults.IdleRetentionDays, 1,
				platformconfig.MaxCloudExecutorRetentionDays),
			integer(platformconfig.KeyCloudExecutorReturnGraceDays, "Return Grace (days)",
				"How long a cloud copy is kept after the thread was taken back.", defaults.ReturnGraceDays, 0,
				platformconfig.MaxCloudExecutorRetentionDays),
		},
	}
}

// WithCloudExecutorEncryption supplies the startup answer for the cloud
// executor's storage encryption at rest, which the section shows as its
// notice: "attested by operator" is not the same evidence as a backend that
// reports its rule (ADR-0034 decision 4).
func WithCloudExecutorEncryption(status string) Option {
	return func(h *Handler) { h.cloudExecutorEncryption = status }
}

// cloudExecutorEncryptionNotice is the section's notice for a startup answer.
func cloudExecutorEncryptionNotice(status string) string {
	switch status {
	case "confirmed":
		return "Storage encryption at rest: reported by the storage backend."
	case "attested":
		return "Storage encryption at rest: attested by operator. The storage backend cannot report it, and the " +
			"deployment sets ELITEA_CLOUD_EXECUTOR_STORAGE_ENCRYPTION_ATTESTED=true."
	case "refused":
		return "Storage encryption at rest: not confirmed. The cloud executor stays off until the storage " +
			"bucket has default encryption, or, on a backend that cannot report it, until the operator attests it."
	default:
		return ""
	}
}

func toAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
