package runtimecomposition

import "testing"

func TestNewCurrentDelegatedAuthToolkitSettingsRejectsMissingDependencies(t *testing.T) {
	if _, err := NewCurrentDelegatedAuthToolkitSettings(nil, nil, nil); err == nil {
		t.Fatal("missing dependencies were accepted")
	}
}
