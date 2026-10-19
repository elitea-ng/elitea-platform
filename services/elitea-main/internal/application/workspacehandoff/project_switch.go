package workspacehandoff

import (
	"context"
	"errors"
)

// The project's own Send to cloud switch (owner decision 2026-10-19): the
// `enabled` field of its `cloud_executor` setting, explicit on, explicit off,
// or unset to follow the platform's project default. The admin Projects page
// and the project settings page read and write it through ProjectSwitchService;
// Narrow is what makes it take effect.

// ProjectSwitchStore reads the project's setting and writes its `enabled`
// field alone, keeping every other field of the row.
type ProjectSwitchStore interface {
	ProjectSettingReader
	// SetCloudExecutorProjectEnabled stores the explicit value, or removes it
	// when enabled is nil, as a configuration mutation whose actor is
	// actorID (the user making the change). It answers ErrProjectNotFound for
	// a project with no tenant schema.
	SetCloudExecutorProjectEnabled(ctx context.Context, projectID, actorID int64, enabled *bool) error
}

// Where a project's switch value comes from.
const (
	ProjectSwitchExplicit = "explicit"
	ProjectSwitchDefault  = "default"
)

var (
	// ErrProjectNotFound: no such project (no tenant schema).
	ErrProjectNotFound = errors.New("project not found")
	// ErrProjectSwitchPlatformOff: a project cannot turn the cloud executor
	// on while the platform switch is off.
	ErrProjectSwitchPlatformOff = errors.New("the cloud executor is off for this deployment")
)

// ProjectSwitchState is the project's switch with the layers that decide it.
type ProjectSwitchState struct {
	ProjectID             int64 `json:"project_id"`
	PlatformEnabled       bool  `json:"platform_enabled"`
	ProjectDefaultEnabled bool  `json:"project_default_enabled"`
	// Explicit is the project's own value; nil follows the default.
	Explicit *bool  `json:"explicit"`
	Source   string `json:"source"`
	// Enabled is the platform switch and the project's own value together,
	// before the device switch and the storage and executor checks, which
	// the handoff policy adds.
	Enabled bool `json:"enabled"`
}

// ProjectSwitchService reads and writes the project switch.
type ProjectSwitchService struct {
	platform PlatformReader
	store    ProjectSwitchStore
}

// NewProjectSwitchService builds the service.
func NewProjectSwitchService(platform PlatformReader, store ProjectSwitchStore) (*ProjectSwitchService, error) {
	if platform == nil || store == nil {
		return nil, errors.New("project switch dependencies are required")
	}
	return &ProjectSwitchService{platform: platform, store: store}, nil
}

// State answers the project's switch.
func (s *ProjectSwitchService) State(ctx context.Context, projectID int64) (ProjectSwitchState, error) {
	ceiling, err := s.platform.CloudExecutorSettings(ctx)
	if err != nil {
		return ProjectSwitchState{}, err
	}
	setting, _, err := s.store.CloudExecutorSetting(ctx, projectID)
	if err != nil {
		return ProjectSwitchState{}, err
	}
	state := ProjectSwitchState{
		ProjectID: projectID, PlatformEnabled: ceiling.Enabled, ProjectDefaultEnabled: ceiling.ProjectDefaultEnabled,
		Explicit: setting.Enabled, Source: ProjectSwitchDefault,
		Enabled: ceiling.Enabled && ProjectEnabled(ceiling, setting),
	}
	if setting.Enabled != nil {
		state.Source = ProjectSwitchExplicit
	}
	return state, nil
}

// Set stores the project's explicit value (nil: follow the default) on
// behalf of actorID and answers the new state. Turning it on while the
// platform is off is refused with ErrProjectSwitchPlatformOff; off and unset
// are always accepted.
func (s *ProjectSwitchService) Set(ctx context.Context, projectID, actorID int64, enabled *bool) (ProjectSwitchState, error) {
	if enabled != nil && *enabled {
		ceiling, err := s.platform.CloudExecutorSettings(ctx)
		if err != nil {
			return ProjectSwitchState{}, err
		}
		if !ceiling.Enabled {
			return ProjectSwitchState{}, ErrProjectSwitchPlatformOff
		}
	}
	if err := s.store.SetCloudExecutorProjectEnabled(ctx, projectID, actorID, enabled); err != nil {
		return ProjectSwitchState{}, err
	}
	return s.State(ctx, projectID)
}
