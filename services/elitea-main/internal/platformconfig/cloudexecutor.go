package platformconfig

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionCloudExecutor is the platform ceiling of the desktop ⇄ cloud handoff
// (ADR-0034 decision 11). The admin Configuration page writes it; the handoff
// reads it at every send and at every cloud turn claim, narrowed by the
// project setting (configuration type `cloud_executor`) and the native client
// policy's `local_work_cloud_handoff`.
const SectionCloudExecutor = "cloud_executor"

// Field keys of SectionCloudExecutor.
const (
	KeyCloudExecutorEnabled               = "enabled"
	KeyCloudExecutorShell                 = "shell"
	KeyCloudExecutorNetwork               = "network"
	KeyCloudExecutorNetworkAllowlist      = "network_allowlist"
	KeyCloudExecutorHistoryFullAllowed    = "history_full_allowed"
	KeyCloudExecutorMaxSegmentMB          = "max_segment_mb"
	KeyCloudExecutorMaxFileMB             = "max_file_mb"
	KeyCloudExecutorMaxFiles              = "max_files"
	KeyCloudExecutorMaxUnpackedMB         = "max_unpacked_mb"
	KeyCloudExecutorMaxProjectWorkspaceGB = "max_project_workspace_gb"
	KeyCloudExecutorIdleRetentionDays     = "idle_retention_days"
	KeyCloudExecutorReturnGraceDays       = "return_grace_days"
)

// Bounds the loader applies, so a row written by hand cannot widen them.
const (
	MaxCloudExecutorSegmentMB          = 10240
	MaxCloudExecutorFileMB             = 2048
	MaxCloudExecutorFiles              = 1000000
	MaxCloudExecutorUnpackedMB         = 102400
	MaxCloudExecutorProjectWorkspaceGB = 10240
	MaxCloudExecutorRetentionDays      = 365
	MaxCloudExecutorAllowlistHosts     = 200
	MaxCloudExecutorHostBytes          = 253
)

// CloudExecutorResources are the sandbox limits the chart sets (decision 11:
// `turn_wall_seconds`, `cpu`, `memory`, `disk`). They are deployment values,
// never saved through the admin page.
type CloudExecutorResources struct {
	TurnWallSeconds int64  `json:"turn_wall_seconds"`
	CPU             string `json:"cpu"`
	Memory          string `json:"memory"`
	Disk            string `json:"disk"`
}

// CloudExecutorSettings is the platform ceiling.
type CloudExecutorSettings struct {
	Enabled               bool                   `json:"enabled"`
	Shell                 bool                   `json:"shell"`
	Network               bool                   `json:"network"`
	NetworkAllowlist      []string               `json:"network_allowlist"`
	HistoryFullAllowed    bool                   `json:"history_full_allowed"`
	MaxSegmentMB          int64                  `json:"max_segment_mb"`
	MaxFileMB             int64                  `json:"max_file_mb"`
	MaxFiles              int64                  `json:"max_files"`
	MaxUnpackedMB         int64                  `json:"max_unpacked_mb"`
	MaxProjectWorkspaceGB int64                  `json:"max_project_workspace_gb"`
	IdleRetentionDays     int64                  `json:"idle_retention_days"`
	ReturnGraceDays       int64                  `json:"return_grace_days"`
	Resources             CloudExecutorResources `json:"resources"`
}

// DefaultCloudExecutorSettings is the ceiling of a deployment that never saved
// the section: the cloud executor is off; the rest applies once an
// administrator enables it.
func DefaultCloudExecutorSettings() CloudExecutorSettings {
	return CloudExecutorSettings{
		Enabled: false, Shell: true, Network: true, NetworkAllowlist: []string{}, HistoryFullAllowed: false,
		MaxSegmentMB: 200, MaxFileMB: 25, MaxFiles: 20000, MaxUnpackedMB: 2048, MaxProjectWorkspaceGB: 50,
		IdleRetentionDays: 30, ReturnGraceDays: 7,
	}
}

// LoadCloudExecutorSettings reads the section over the defaults. A query error
// answers the defaults (off) and the error.
func LoadCloudExecutorSettings(ctx context.Context, pool *pgxpool.Pool) (CloudExecutorSettings, error) {
	values, err := Load(ctx, pool, SectionCloudExecutor)
	if err != nil {
		return DefaultCloudExecutorSettings(), err
	}
	return CloudExecutorSettingsFromValues(values), nil
}

// CloudExecutorSettingsFromValues is the overlay. A value of the wrong type or
// out of range is ABSENT and keeps its default.
func CloudExecutorSettingsFromValues(values Values) CloudExecutorSettings {
	settings := DefaultCloudExecutorSettings()
	settings.Enabled = values.Bool(KeyCloudExecutorEnabled, settings.Enabled)
	settings.Shell = values.Bool(KeyCloudExecutorShell, settings.Shell)
	settings.Network = values.Bool(KeyCloudExecutorNetwork, settings.Network)
	settings.HistoryFullAllowed = values.Bool(KeyCloudExecutorHistoryFullAllowed, settings.HistoryFullAllowed)
	settings.NetworkAllowlist = CloudExecutorHosts(values.Strings(KeyCloudExecutorNetworkAllowlist))
	bounded := func(key string, minimum, maximum int64, into *int64) {
		if n, ok := values.Int(key); ok && n >= minimum && n <= maximum {
			*into = n
		}
	}
	bounded(KeyCloudExecutorMaxSegmentMB, 1, MaxCloudExecutorSegmentMB, &settings.MaxSegmentMB)
	bounded(KeyCloudExecutorMaxFileMB, 1, MaxCloudExecutorFileMB, &settings.MaxFileMB)
	bounded(KeyCloudExecutorMaxFiles, 1, MaxCloudExecutorFiles, &settings.MaxFiles)
	bounded(KeyCloudExecutorMaxUnpackedMB, 1, MaxCloudExecutorUnpackedMB, &settings.MaxUnpackedMB)
	bounded(KeyCloudExecutorMaxProjectWorkspaceGB, 1, MaxCloudExecutorProjectWorkspaceGB, &settings.MaxProjectWorkspaceGB)
	bounded(KeyCloudExecutorIdleRetentionDays, 1, MaxCloudExecutorRetentionDays, &settings.IdleRetentionDays)
	bounded(KeyCloudExecutorReturnGraceDays, 0, MaxCloudExecutorRetentionDays, &settings.ReturnGraceDays)
	return settings
}

// CloudExecutorHosts keeps the valid host names of an allowlist (lowercased,
// at most MaxCloudExecutorAllowlistHosts). It never answers nil.
func CloudExecutorHosts(hosts []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if !ValidCloudExecutorHost(host) || seen[host] {
			continue
		}
		if len(out) == MaxCloudExecutorAllowlistHosts {
			break
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}

// ValidCloudExecutorHost reports a DNS host name, optionally with a leading
// `*.` wildcard label.
func ValidCloudExecutorHost(host string) bool {
	if host == "" || len(host) > MaxCloudExecutorHostBytes {
		return false
	}
	host = strings.TrimPrefix(host, "*.")
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
