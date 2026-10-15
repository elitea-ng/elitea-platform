package workspacehandoff

import (
	"context"
	"errors"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
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
}

// NewPolicyService builds the policy. encryptionConfirmed is the startup
// check's answer; resources are the chart's sandbox limits.
func NewPolicyService(
	platform PlatformReader, project ProjectSettingReader, device DevicePolicyReader,
	encryptionConfirmed bool, resources platformconfig.CloudExecutorResources,
) (*PolicyService, error) {
	if platform == nil || project == nil || device == nil {
		return nil, errors.New("workspace handoff policy dependencies are required")
	}
	return &PolicyService{
		platform: platform, project: project, device: device, encrypted: encryptionConfirmed, resources: resources,
	}, nil
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

// Narrow is the effective policy: a project can turn a switch off but never on
// when the platform has it off, can only shrink the allowlist and lower the
// caps, and the device switch and the encryption check can only turn the
// executor off.
func Narrow(ceiling platformconfig.CloudExecutorSettings, project ProjectSetting, device, encrypted bool) Policy {
	and := func(platform bool, value *bool) bool { return platform && (value == nil || *value) }
	lower := func(platform int64, value *int64) int64 {
		if value != nil && *value >= 0 && *value < platform {
			return *value
		}
		return platform
	}
	policy := Policy{
		Enabled:            and(ceiling.Enabled, project.Enabled) && device && encrypted,
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
	// The exclusion set and the environment deny list only grow: a project
	// adds patterns and can never remove a platform one.
	policy.Exclusions = mergePatterns(ceiling.Exclusions, project.ExclusionsAdd)
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
			allowed := map[string]bool{}
			for _, host := range ceiling.NetworkAllowlist {
				allowed[host] = true
			}
			narrowed := []string{}
			for _, host := range requested {
				if allowed[host] {
					narrowed = append(narrowed, host)
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

func mergePatterns(platform []string, added *[]string) []string {
	merged := append([]string{}, platform...)
	if added != nil {
		merged = append(merged, *added...)
	}
	return platformconfig.CloudExecutorPatterns(merged)
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
