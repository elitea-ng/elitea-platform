package runtimecomposition

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

var databasePoolEnvironment = []struct {
	name string
	set  func(*DatabasePoolLimits, int32)
}{
	{"ELITEA_RUNTIME_DB_ADMISSION_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.AdmissionPublisher = v }},
	{"ELITEA_RUNTIME_DB_CONTROL_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.Control = v }},
	{"ELITEA_RUNTIME_DB_OUTPUT_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.Output = v }},
	{"ELITEA_RUNTIME_DB_REPLAY_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.Replay = v }},
	{"ELITEA_RUNTIME_DB_TERMINAL_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.TerminalEffects = v }},
	{"ELITEA_RUNTIME_DB_CONTENT_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.Content = v }},
	{"ELITEA_RUNTIME_DB_VECTOR_INTROSPECTION_MAX_CONNS", func(l *DatabasePoolLimits, v int32) { l.VectorIntrospection = v }},
}

// DatabasePoolLimits is the fixed phase-one capacity profile. The six pools
// are intentionally separate so public SSE replay or content load cannot
// consume connections needed for admission, lease/control, output settlement,
// or terminal projection progress.
//
// VectorIntrospection sizes a seventh pool, opened only when
// ELITEA_VECTOR_INTROSPECTION_CLIENTS is set: elitea-vector's token
// introspection (one claim-token read per uncached token) runs on it, so a
// burst of introspection calls cannot starve the lease/control pool.
type DatabasePoolLimits struct {
	AdmissionPublisher  int32
	Control             int32
	Output              int32
	Replay              int32
	TerminalEffects     int32
	Content             int32
	VectorIntrospection int32
}

func PhaseOneDatabasePoolLimits() DatabasePoolLimits {
	return DatabasePoolLimits{
		AdmissionPublisher:  10,
		Control:             8,
		Output:              8,
		Replay:              4,
		TerminalEffects:     2,
		Content:             4,
		VectorIntrospection: 8,
	}
}

// DatabasePoolLimitsFromEnv keeps the phase-one defaults while allowing a
// mixed deployment to share one PostgreSQL instance with the current Python
// services. Every override retains the compiled capacity invariant.
func DatabasePoolLimitsFromEnv(lookup LookupEnv) (DatabasePoolLimits, error) {
	limits := PhaseOneDatabasePoolLimits()
	for _, option := range databasePoolEnvironment {
		raw, ok := lookup(option.name)
		if !ok || raw == "" {
			continue
		}
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || strconv.FormatInt(parsed, 10) != raw {
			return DatabasePoolLimits{}, fmt.Errorf("%s must be a canonical base-10 integer", option.name)
		}
		option.set(&limits, int32(parsed))
	}
	if err := limits.Validate(); err != nil {
		return DatabasePoolLimits{}, err
	}
	return limits, nil
}

func (l DatabasePoolLimits) Validate() error {
	for _, limit := range []int32{
		l.AdmissionPublisher,
		l.Control,
		l.Output,
		l.Replay,
		l.TerminalEffects,
		l.Content,
		l.VectorIntrospection,
	} {
		if limit <= 0 || limit > 64 {
			return errors.New("runtime database pool capacity is invalid")
		}
	}
	return nil
}

func validateDependencies(dependencies Dependencies) error {
	if dependencies.Logger == nil {
		return errors.New("runtime logger is required")
	}
	if dependencies.PermissionResolver == nil {
		return errors.New("runtime permission resolver is required")
	}
	pools := []*pgxpool.Pool{
		dependencies.AdmissionPool,
		dependencies.ControlPool,
		dependencies.OutputPool,
		dependencies.ReplayPool,
		dependencies.TerminalEffectsPool,
		dependencies.ContentPool,
	}
	for _, pool := range pools {
		if pool == nil {
			return errors.New("six isolated runtime database pools are required")
		}
	}
	// The optional introspection pool is held to the same rule.
	if dependencies.VectorIntrospectionPool != nil {
		pools = append(pools, dependencies.VectorIntrospectionPool)
	}
	for i, pool := range pools {
		for j := i + 1; j < len(pools); j++ {
			if pool == pools[j] {
				return errors.New("runtime database pools must not share capacity")
			}
		}
	}
	return nil
}
