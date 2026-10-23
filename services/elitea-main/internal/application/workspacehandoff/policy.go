package workspacehandoff

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// PlatformReader reads the platform ceiling (platformconfig.SectionCloudExecutor).
type PlatformReader interface {
	CloudExecutorSettings(ctx context.Context) (platformconfig.CloudExecutorSettings, error)
}

// ProjectSettingReader reads a project's `cloud_executor` configuration row.
type ProjectSettingReader interface {
	CloudExecutorSetting(ctx context.Context, projectID int64) (ProjectSetting, bool, error)
}

// DevicePolicyReader is the native client policy (*nativepolicy.Service).
type DevicePolicyReader interface {
	Policy(ctx context.Context) (platformconfig.NativeClientPolicy, error)
}

// ProjectSetting is the project's narrowing of the ceiling. A nil field is
// unset and keeps the platform value.
type ProjectSetting struct {
	Enabled               *bool     `json:"enabled"`
	Shell                 *bool     `json:"shell"`
	Network               *bool     `json:"network"`
	NetworkAllowlist      *[]string `json:"network_allowlist"`
	History               *string   `json:"history"`
	MaxSegmentMB          *int64    `json:"max_segment_mb"`
	MaxFileMB             *int64    `json:"max_file_mb"`
	MaxFiles              *int64    `json:"max_files"`
	MaxUnpackedMB         *int64    `json:"max_unpacked_mb"`
	MaxProjectWorkspaceGB *int64    `json:"max_project_workspace_gb"`
	IdleRetentionDays     *int64    `json:"idle_retention_days"`
	ReturnGraceDays       *int64    `json:"return_grace_days"`
	// Amendment 2026-10-15 (decision 11): a project adds exclusion and env
	// deny patterns, turns include and the harvest off (or on under a ceiling
	// that allows them), and narrows env_allow.
	ExclusionsAdd    *[]string `json:"exclusions_add"`
	AllowUserInclude *bool     `json:"allow_user_include"`
	EnvHarvest       *bool     `json:"env_harvest"`
	EnvDenyAdd       *[]string `json:"env_deny_add"`
	EnvAllow         *[]string `json:"env_allow"`
}

// PolicyService computes the effective policy (decision 11): the platform
// ceiling, narrowed by the project setting and the device switch, and off
// whenever the object store's default encryption could not be confirmed at
// startup (decision 4). It is recomputed on every call: at every send and,
// through Service.EffectivePolicy, at every cloud turn claim.
type PolicyService struct {
	platform  PlatformReader
	project   ProjectSettingReader
	device    DevicePolicyReader
	encrypted bool
	resources platformconfig.CloudExecutorResources
	// noExecutor: the deployment says it runs no cloud executor
	// (WithExecutorConfigured(false)). Unknown is not "none".
	noExecutor bool
}

// PolicyOption configures a PolicyService.
type PolicyOption func(*PolicyService)

// WithExecutorConfigured states whether the deployment runs a cloud executor
// (the chart sets ELITEA_CLOUD_EXECUTOR_CONFIGURED from its workspace-run
// and supervisor values). false turns the policy off with
// DisabledNoExecutor; without this option Main does not know, and claims
// nothing.
func WithExecutorConfigured(configured bool) PolicyOption {
	return func(p *PolicyService) { p.noExecutor = !configured }
}

// NewPolicyService builds the policy. encryptionConfirmed is the startup
// check's answer; resources are the chart's sandbox limits.
func NewPolicyService(
	platform PlatformReader, project ProjectSettingReader, device DevicePolicyReader,
	encryptionConfirmed bool, resources platformconfig.CloudExecutorResources, options ...PolicyOption,
) (*PolicyService, error) {
	if platform == nil || project == nil || device == nil {
		return nil, errors.New("workspace handoff policy dependencies are required")
	}
	service := &PolicyService{
		platform: platform, project: project, device: device, encrypted: encryptionConfirmed, resources: resources,
	}
	for _, option := range options {
		option(service)
	}
	return service, nil
}

var _ PolicySource = (*PolicyService)(nil)
var _ workspacestore.LimitsSource = (*PolicyService)(nil)

// Resources answers the chart's sandbox limits (for the later runtime context).
func (p *PolicyService) Resources() platformconfig.CloudExecutorResources { return p.resources }

// Effective answers the project's effective policy.
func (p *PolicyService) Effective(ctx context.Context, projectID int64) (Policy, error) {
	ceiling, err := p.platform.CloudExecutorSettings(ctx)
	if err != nil {
		return Policy{}, err
	}
	setting, _, err := p.project.CloudExecutorSetting(ctx, projectID)
	if err != nil {
		return Policy{}, err
	}
	device, err := p.device.Policy(ctx)
	if err != nil {
		return Policy{}, err
	}
	policy := Narrow(ceiling, setting, device.LocalWork.CloudHandoff, p.encrypted)
	// No executor outranks only the storage check: a deliberate switch that
	// is off stays the reason the caller reads.
	if p.noExecutor && (policy.Enabled || policy.DisabledReason == DisabledStorageEncryptionUnconfirmed) {
		policy.Enabled, policy.DisabledReason = false, DisabledNoExecutor
	}
	policy.PathDeny = append([]string{}, device.LocalWork.PathDeny...)
	return policy, nil
}

// Limits answers the effective caps (workspacestore.LimitsSource).
func (p *PolicyService) Limits(ctx context.Context, projectID int64) (workspacestore.Limits, error) {
	policy, err := p.Effective(ctx, projectID)
	if err != nil {
		return workspacestore.Limits{}, err
	}
	return policy.Limits, nil
}

// MinIdleRetention is the shortest idle retention: a 0 setting means this.
const MinIdleRetention = 24 * time.Hour

// Narrow is the effective policy: a project can turn a switch off but never on
// when the platform has it off, can only shrink the allowlist and lower the
// caps, and the device switch and the encryption check can only turn the
// executor off.
//
// `enabled` is the one switch that is not narrow-only: under a platform that
// has the executor on, the project's explicit value wins either way, and a
// project that set none follows the platform's project default
// (ProjectDefaultEnabled). The platform's off still always wins.
func Narrow(ceiling platformconfig.CloudExecutorSettings, project ProjectSetting, device, encrypted bool) Policy {
	and := func(platform bool, value *bool) bool { return platform && (value == nil || *value) }
	projectOn := ProjectEnabled(ceiling, project)
	lower := func(platform int64, value *int64) int64 {
		if value != nil && *value >= 0 && *value < platform {
			return *value
		}
		return platform
	}
	policy := Policy{
		Enabled:            ceiling.Enabled && projectOn && device && encrypted,
		DisabledReason:     disabledReason(ceiling.Enabled, projectOn, device, encrypted),
		Shell:              and(ceiling.Shell, project.Shell),
		Network:            and(ceiling.Network, project.Network),
		HistoryFullAllowed: ceiling.HistoryFullAllowed && (project.History == nil || *project.History == HistoryFull),
		Limits: workspacestore.Limits{
			MaxSegmentBytes:          lower(ceiling.MaxSegmentMB, project.MaxSegmentMB) << 20,
			MaxFileBytes:             lower(ceiling.MaxFileMB, project.MaxFileMB) << 20,
			MaxFiles:                 lower(ceiling.MaxFiles, project.MaxFiles),
			MaxUnpackedBytes:         lower(ceiling.MaxUnpackedMB, project.MaxUnpackedMB) << 20,
			MaxProjectWorkspaceBytes: lower(ceiling.MaxProjectWorkspaceGB, project.MaxProjectWorkspaceGB) << 30,
		},
		IdleRetention: time.Duration(lower(ceiling.IdleRetentionDays, project.IdleRetentionDays)) * 24 * time.Hour,
		ReturnGrace:   time.Duration(lower(ceiling.ReturnGraceDays, project.ReturnGraceDays)) * 24 * time.Hour,
	}
	// Idle retention 0 (from the platform or a project) is the minimum, one
	// day, never "keep forever". Return grace 0 means the copy is erased at
	// release (delete_after = now), which is already bounded.
	if policy.IdleRetention < MinIdleRetention {
		policy.IdleRetention = MinIdleRetention
	}
	if policy.ReturnGrace < 0 {
		policy.ReturnGrace = 0
	}
	// The exclusion set and the environment deny list only grow: a project
	// adds patterns and can never remove a platform one.
	policy.Exclusions = mergePatterns(ceiling.Exclusions, project.ExclusionsAdd)
	policy.ExclusionsPlatform = platformconfig.CloudExecutorPatterns(ceiling.Exclusions)
	policy.ExclusionsProject = projectAdditions(policy.ExclusionsPlatform, project.ExclusionsAdd)
	policy.EnvDeny = mergePatterns(ceiling.EnvDeny, project.EnvDenyAdd)
	policy.AllowUserInclude = and(ceiling.AllowUserInclude, project.AllowUserInclude)
	policy.EnvHarvest = and(ceiling.EnvHarvest, project.EnvHarvest)
	policy.EnvAllow = narrowNames(ceiling.EnvAllow, project.EnvAllow)
	// The allowlist. Empty means the public internet, so an intersection that
	// comes out empty must not widen into "everything": the network is then off.
	policy.NetworkAllowlist = append([]string{}, ceiling.NetworkAllowlist...)
	if project.NetworkAllowlist != nil && len(*project.NetworkAllowlist) > 0 {
		requested := platformconfig.CloudExecutorHosts(*project.NetworkAllowlist)
		if len(ceiling.NetworkAllowlist) == 0 {
			policy.NetworkAllowlist = requested
		} else {
			narrowed := []string{}
			for _, host := range requested {
				for _, ceilingHost := range ceiling.NetworkAllowlist {
					if HostCovered(ceilingHost, host) {
						narrowed = append(narrowed, host)
						break
					}
				}
			}
			policy.NetworkAllowlist = narrowed
		}
		if len(policy.NetworkAllowlist) == 0 {
			policy.Network = false
		}
	}
	return policy
}

// ProjectEnabled answers the project's own `enabled`, before the platform
// switch: its explicit value, or the platform's project default when it set
// none.
func ProjectEnabled(ceiling platformconfig.CloudExecutorSettings, project ProjectSetting) bool {
	if project.Enabled != nil {
		return *project.Enabled
	}
	return ceiling.ProjectDefaultEnabled
}

// disabledReason answers why the executor is off, the first that applies,
// or "" when it is on. project_off covers both an explicit off and a project
// that set nothing under a project default that is off.
func disabledReason(platform, project, device, encrypted bool) string {
	switch {
	case !platform:
		return DisabledPlatformOff
	case !project:
		return DisabledProjectOff
	case !device:
		return DisabledDeviceOff
	case !encrypted:
		return DisabledStorageEncryptionUnconfirmed
	}
	return ""
}

func mergePatterns(platform []string, added *[]string) []string {
	merged := append([]string{}, platform...)
	if added != nil {
		merged = append(merged, *added...)
	}
	return platformconfig.CloudExecutorPatterns(merged)
}

// projectAdditions answers the project's patterns that the platform does not
// already list, in the project's order.
func projectAdditions(platform []string, added *[]string) []string {
	if added == nil {
		return []string{}
	}
	listed := map[string]bool{}
	for _, pattern := range platform {
		listed[pattern] = true
	}
	out := []string{}
	for _, pattern := range platformconfig.CloudExecutorPatterns(*added) {
		if !listed[pattern] {
			listed[pattern] = true
			out = append(out, pattern)
		}
	}
	return out
}

// narrowNames intersects two optional allow lists: unset (nil) allows
// everything, so the project's list applies under an unset ceiling, and a
// list under a set ceiling keeps only names the ceiling lists too.
func narrowNames(platform, project *[]string) *[]string {
	switch {
	case platform == nil && project == nil:
		return nil
	case platform == nil:
		names := platformconfig.CloudExecutorPatterns(*project)
		return &names
	case project == nil:
		names := append([]string{}, *platform...)
		return &names
	}
	allowed := map[string]bool{}
	for _, name := range *platform {
		allowed[name] = true
	}
	names := []string{}
	for _, name := range platformconfig.CloudExecutorPatterns(*project) {
		if allowed[name] {
			names = append(names, name)
		}
	}
	return &names
}

// HostCovered reports whether a project allowlist entry is inside a ceiling
// entry: the same entry, or a host (or a narrower wildcard) under a ceiling
// wildcard. `*.github.com` covers `api.github.com`, `a.b.github.com` and
// `*.api.github.com`, never `github.com` itself. Ports follow the egress
// proxy's grammar: an entry without a port means 443 and 80, so it is
// covered only by a ceiling entry without a port; an entry with a port is
// covered by the same port, or by no port when it is 443 or 80.
//
// TODO(egress-proxy): use runtoken.Rule's matcher from
// libs/go/egresslib/runtoken once feat/egress-proxy is merged, so the policy
// and the proxy cannot disagree.
func HostCovered(ceiling, entry string) bool {
	ceilingHost, ceilingPort, ok := splitAllowlistEntry(ceiling)
	if !ok {
		return false
	}
	host, port, ok := splitAllowlistEntry(entry)
	if !ok {
		return false
	}
	switch {
	case ceilingPort == port:
	case ceilingPort == 0 && (port == 443 || port == 80):
	default:
		return false
	}
	if host == ceilingHost {
		return true
	}
	suffix, wildcard := strings.CutPrefix(ceilingHost, "*")
	if !wildcard {
		return false
	}
	// suffix is ".github.com": the entry must be strictly under it.
	name := strings.TrimPrefix(host, "*")
	return strings.HasSuffix(name, suffix) && len(strings.TrimSuffix(name, suffix)) > 0 &&
		!strings.HasSuffix(strings.TrimSuffix(name, suffix), ".") && name != suffix
}

// splitAllowlistEntry answers the lower-cased host and the port (0 when
// none) of `host`, `host:port`, `*.domain` or `*.domain:port`.
func splitAllowlistEntry(entry string) (string, int, bool) {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if entry == "" {
		return "", 0, false
	}
	host, rawPort, hasPort := strings.Cut(entry, ":")
	if !hasPort {
		return host, 0, true
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != rawPort {
		return "", 0, false
	}
	return host, port, true
}
