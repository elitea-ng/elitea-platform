package workspacehandoff

import (
	"context"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type memorySwitchStore struct {
	setting ProjectSetting
	writes  int
	actor   int64
}

func (m *memorySwitchStore) CloudExecutorSetting(context.Context, int64) (ProjectSetting, bool, error) {
	return m.setting, true, nil
}

func (m *memorySwitchStore) SetCloudExecutorProjectEnabled(_ context.Context, _, actorID int64, enabled *bool) error {
	m.writes++
	m.actor = actorID
	m.setting.Enabled = enabled
	return nil
}

func TestProjectSwitchStateAndSet(t *testing.T) {
	ceiling := platformconfig.DefaultCloudExecutorSettings()
	store := &memorySwitchStore{}
	service, err := NewProjectSwitchService(ceilingReader(ceiling), store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The platform off: on is refused and nothing is written; off is kept.
	if _, err := service.Set(ctx, 7, 41, boolPtr(true)); !errors.Is(err, ErrProjectSwitchPlatformOff) || store.writes != 0 {
		t.Fatalf("on under the platform off = %v (%d writes)", err, store.writes)
	}
	state, err := service.Set(ctx, 7, 41, boolPtr(false))
	if err != nil || state.Source != ProjectSwitchExplicit || state.Enabled || state.ProjectID != 7 || store.actor != 41 {
		t.Fatalf("off = %+v, %v (actor %d)", state, err, store.actor)
	}
	// The platform on with its default on: unset follows the default.
	ceiling.Enabled, ceiling.ProjectDefaultEnabled = true, true
	service, _ = NewProjectSwitchService(ceilingReader(ceiling), store)
	if state, _ := service.State(ctx, 7); state.Enabled || state.Explicit == nil {
		t.Fatalf("explicit off under default on = %+v", state)
	}
	state, err = service.Set(ctx, 7, 41, nil)
	if err != nil || state.Source != ProjectSwitchDefault || !state.Enabled || state.Explicit != nil {
		t.Fatalf("unset = %+v, %v", state, err)
	}
	if _, err := NewProjectSwitchService(nil, store); err == nil {
		t.Fatal("a nil platform reader was accepted")
	}
}
