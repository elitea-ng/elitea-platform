package runtimecomposition

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPhaseOneDatabasePoolLimitsArePositiveAndBounded(t *testing.T) {
	limits := PhaseOneDatabasePoolLimits()
	if err := limits.Validate(); err != nil {
		t.Fatal(err)
	}
	if limits.AdmissionPublisher != 10 || limits.Control != 8 || limits.Output != 8 ||
		limits.Replay != 4 || limits.TerminalEffects != 2 || limits.Content != 4 ||
		limits.VectorIntrospection != 8 {
		t.Fatalf("unexpected phase-one database profile: %+v", limits)
	}
}

func TestDatabasePoolLimitsFromEnvUsesDefaults(t *testing.T) {
	got, err := DatabasePoolLimitsFromEnv(mapLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := PhaseOneDatabasePoolLimits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
}

func TestDatabasePoolLimitsFromEnvAppliesMixedDeploymentBudget(t *testing.T) {
	got, err := DatabasePoolLimitsFromEnv(mapLookup(map[string]string{
		"ELITEA_RUNTIME_DB_ADMISSION_MAX_CONNS": "3",
		"ELITEA_RUNTIME_DB_CONTROL_MAX_CONNS":   "2",
		"ELITEA_RUNTIME_DB_OUTPUT_MAX_CONNS":    "2",
		"ELITEA_RUNTIME_DB_REPLAY_MAX_CONNS":    "1",
		"ELITEA_RUNTIME_DB_TERMINAL_MAX_CONNS":  "1",
		"ELITEA_RUNTIME_DB_CONTENT_MAX_CONNS":   "1",

		"ELITEA_RUNTIME_DB_VECTOR_INTROSPECTION_MAX_CONNS": "5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := DatabasePoolLimits{3, 2, 2, 1, 1, 1, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
}

func TestDatabasePoolLimitsFromEnvRejectsInvalidCapacity(t *testing.T) {
	for _, value := range []string{"0", "65", "01", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			_, err := DatabasePoolLimitsFromEnv(mapLookup(map[string]string{
				"ELITEA_RUNTIME_DB_CONTROL_MAX_CONNS": value,
			}))
			if err == nil || !strings.Contains(err.Error(), "database pool") && !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("expected bounded canonical capacity error, got %v", err)
			}
		})
	}
}

func TestRuntimeDependenciesRejectSharedDatabaseCapacity(t *testing.T) {
	shared := new(pgxpool.Pool)
	dependencies := Dependencies{
		AdmissionPool:       shared,
		ControlPool:         shared,
		OutputPool:          new(pgxpool.Pool),
		ReplayPool:          new(pgxpool.Pool),
		TerminalEffectsPool: new(pgxpool.Pool),
		ContentPool:         new(pgxpool.Pool),
		PermissionResolver:  &authorizationPermissionResolver{},
		Logger:              slog.Default(),
	}
	if err := validateDependencies(dependencies); err == nil {
		t.Fatal("shared admission/control database pool was accepted")
	}

	dependencies.ControlPool = new(pgxpool.Pool)
	if err := validateDependencies(dependencies); err != nil {
		t.Fatal(err)
	}

	isolatedTerminalEffects := dependencies.TerminalEffectsPool
	for name, shared := range map[string]*pgxpool.Pool{
		"admission": dependencies.AdmissionPool,
		"control":   dependencies.ControlPool,
		"output":    dependencies.OutputPool,
		"replay":    dependencies.ReplayPool,
		"content":   dependencies.ContentPool,
	} {
		dependencies.TerminalEffectsPool = shared
		if err := validateDependencies(dependencies); err == nil {
			t.Fatalf(
				"shared %s/terminal-effects database pool was accepted",
				name,
			)
		}
	}
	dependencies.TerminalEffectsPool = isolatedTerminalEffects
}

// The introspection pool is optional, but when composed it is its own pool:
// never the control pool (nor any other).
func TestRuntimeDependenciesRejectAnIntrospectionPoolThatSharesCapacity(t *testing.T) {
	control := new(pgxpool.Pool)
	dependencies := Dependencies{
		AdmissionPool:       new(pgxpool.Pool),
		ControlPool:         control,
		OutputPool:          new(pgxpool.Pool),
		ReplayPool:          new(pgxpool.Pool),
		TerminalEffectsPool: new(pgxpool.Pool),
		ContentPool:         new(pgxpool.Pool),
		PermissionResolver:  &authorizationPermissionResolver{},
		Logger:              slog.Default(),
	}
	if err := validateDependencies(dependencies); err != nil {
		t.Fatalf("no introspection pool is valid: %v", err)
	}
	dependencies.VectorIntrospectionPool = new(pgxpool.Pool)
	if err := validateDependencies(dependencies); err != nil {
		t.Fatalf("a dedicated introspection pool is valid: %v", err)
	}
	dependencies.VectorIntrospectionPool = control
	if err := validateDependencies(dependencies); err == nil {
		t.Fatal("an introspection pool shared with the control pool was accepted")
	}
}
