package toolkitexecution

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestDelegatedAuthToolkitSettingsUsesClaimModeWithoutExternalExposure(t *testing.T) {
	toolkit := exposedToolkit()
	toolkit.Meta = map[string]any{"mcp_options": map[string]any{"available_by_mcp": false}}
	reader := &toolkitReaderStub{toolkit: toolkit}
	settings := &settingsResolverStub{resolved: map[string]any{
		"github_configuration": map[string]any{"token": "resolved-only-here"},
	}}
	resolver, err := NewCurrentDelegatedAuthToolkitSettings(reader, settings)
	if err != nil {
		t.Fatal(err)
	}

	resolved, found, err := resolver.ResolveDelegatedAuthToolkitSettings(
		context.Background(), 7, 11, 19,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !found || resolved.ToolkitType != "github" {
		t.Fatalf("resolved = %+v, found = %v", resolved, found)
	}
	if reader.projectID != 7 || reader.userID != 11 || reader.toolkitID != 19 {
		t.Fatalf("reader identity = %d/%d/%d", reader.projectID, reader.userID, reader.toolkitID)
	}
	if settings.request.Mode != configurationapp.CurrentToolkitSettingsClaimMode ||
		settings.request.ProjectID != 7 || settings.request.UserID != 11 ||
		settings.request.ToolkitType != "github" {
		t.Fatalf("settings request = %+v", settings.request)
	}
	if resolved.Settings["github_configuration"].(map[string]any)["token"] != "resolved-only-here" {
		t.Fatalf("settings = %#v", resolved.Settings)
	}
}

func TestDelegatedAuthToolkitSettingsKeepsAbsenceAndDependencyFailureDistinct(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		settings := &settingsResolverStub{}
		resolver, err := NewCurrentDelegatedAuthToolkitSettings(&toolkitReaderStub{}, settings)
		if err != nil {
			t.Fatal(err)
		}
		_, found, err := resolver.ResolveDelegatedAuthToolkitSettings(context.Background(), 7, 11, 19)
		if err != nil || found {
			t.Fatalf("found = %v, error = %v", found, err)
		}
		if settings.calls != 0 {
			t.Fatal("absent toolkit reached settings resolution")
		}
	})

	t.Run("dependency failure", func(t *testing.T) {
		dependencyErr := errors.New("secret dependency detail")
		resolver, err := NewCurrentDelegatedAuthToolkitSettings(
			&toolkitReaderStub{toolkit: exposedToolkit()},
			&settingsResolverStub{err: dependencyErr},
		)
		if err != nil {
			t.Fatal(err)
		}
		_, found, err := resolver.ResolveDelegatedAuthToolkitSettings(context.Background(), 7, 11, 19)
		if found || !errors.Is(err, dependencyErr) {
			t.Fatalf("found = %v, error = %v", found, err)
		}
	})
}

func TestDelegatedAuthToolkitSettingsRejectsInvalidIdentityAndCancellation(t *testing.T) {
	resolver, err := NewCurrentDelegatedAuthToolkitSettings(
		&toolkitReaderStub{toolkit: exposedToolkit()},
		&settingsResolverStub{resolved: map[string]any{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.ResolveDelegatedAuthToolkitSettings(context.Background(), 0, 11, 19); !errors.Is(err, ErrInvalidDelegatedAuthToolkitSettings) {
		t.Fatalf("invalid identity error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := resolver.ResolveDelegatedAuthToolkitSettings(canceled, 7, 11, 19); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}
