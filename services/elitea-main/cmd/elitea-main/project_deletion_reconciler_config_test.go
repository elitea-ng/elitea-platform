package main

import (
	"testing"
	"time"
)

func TestProjectDeletionReconcilerFromEnv(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	}

	// On by default, with the five-minute grace period.
	got, err := projectDeletionReconcilerFromEnv(lookup(nil))
	if err != nil || !got.Enabled || got.Grace != 5*time.Minute {
		t.Fatalf("defaults = %+v, %v; want enabled with a 5m grace", got, err)
	}
	got, err = projectDeletionReconcilerFromEnv(lookup(map[string]string{
		projectDeletionReconcilerEnv: "false", projectDeletionGraceEnv: "90s",
	}))
	if err != nil || got.Enabled || got.Grace != 90*time.Second {
		t.Fatalf("overrides = %+v, %v", got, err)
	}
	for name, values := range map[string]map[string]string{
		"bad flag":       {projectDeletionReconcilerEnv: "maybe"},
		"bad duration":   {projectDeletionGraceEnv: "soon"},
		"zero duration":  {projectDeletionGraceEnv: "0s"},
		"negative grace": {projectDeletionGraceEnv: "-1m"},
	} {
		if _, err := projectDeletionReconcilerFromEnv(lookup(values)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
