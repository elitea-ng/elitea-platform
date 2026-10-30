package platformconfig

import (
	"context"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionDelegation is the platform's delegation policy (ADR-0044 decision 3,
// #1501). When an agent calls a saved agent or pipeline, a pipeline node runs
// one, or an agent starts a sub-agent, the callee runs with the intersection
// of its own grants and its caller's, and no deeper than max_depth. The admin
// Configuration page writes it; Main reads it (through a per-replica cache)
// every time it prepares an agent execution command and puts it in the signed
// command (WorkerCommandV1.delegation_policy).
const SectionDelegation = "delegation"

// Field keys of SectionDelegation.
const (
	KeyDelegationMaxDepth = "max_depth"
	KeyDelegationMode     = "mode"
)

// Bounds of KeyDelegationMaxDepth: the worker's DelegationPolicy bounds.
const (
	DefaultDelegationMaxDepth = 4
	MinDelegationMaxDepth     = 1
	MaxDelegationMaxDepth     = 16
)

// DelegationMode is how a missing grant is treated.
type DelegationMode string

const (
	// DelegationEnforce refuses the delegation (DELEGATION_GRANT_MISSING).
	DelegationEnforce DelegationMode = "enforce"
	// DelegationReport records the missing grant on the delegation step and
	// lets the callee run. The depth is enforced in both modes. A governed
	// run (the credential governance profile) is always enforce.
	DelegationReport DelegationMode = "report"
)

// DelegationSettings is the section.
type DelegationSettings struct {
	MaxDepth int            `json:"max_depth"`
	Mode     DelegationMode `json:"mode"`
}

// DefaultDelegationSettings is a deployment that never saved the section.
func DefaultDelegationSettings() DelegationSettings {
	return DelegationSettings{MaxDepth: DefaultDelegationMaxDepth, Mode: DelegationEnforce}
}

// IsDefault reports whether the settings equal the defaults: Main then omits
// the field from the command.
func (s DelegationSettings) IsDefault() bool {
	return s == DefaultDelegationSettings()
}

// DelegationSettingsFromValues is the overlay. A depth out of range is
// clamped into range and an unreadable one is the smallest depth; an unknown
// mode is enforce. A hand-written row can tighten the policy and never
// silently loosen it.
func DelegationSettingsFromValues(values Values) DelegationSettings {
	settings := DefaultDelegationSettings()
	if raw, present := values[KeyDelegationMaxDepth]; present {
		number, ok := raw.(float64)
		switch {
		case !ok || math.IsNaN(number):
			settings.MaxDepth = MinDelegationMaxDepth
		case number < MinDelegationMaxDepth:
			settings.MaxDepth = MinDelegationMaxDepth
		case number > MaxDelegationMaxDepth:
			settings.MaxDepth = MaxDelegationMaxDepth
		default:
			settings.MaxDepth = int(math.Trunc(number))
		}
	}
	if raw, present := values[KeyDelegationMode]; present {
		mode, _ := raw.(string)
		if DelegationMode(strings.TrimSpace(mode)) == DelegationReport {
			settings.Mode = DelegationReport
		}
	}
	return settings
}

// ValidDelegationMaxDepth reports whether a saved value is a whole number
// from MinDelegationMaxDepth to MaxDelegationMaxDepth. JSON numbers arrive as
// float64.
func ValidDelegationMaxDepth(raw any) bool {
	number, ok := raw.(float64)
	return ok && !math.IsNaN(number) && number == math.Trunc(number) &&
		number >= MinDelegationMaxDepth && number <= MaxDelegationMaxDepth
}

// LoadDelegationSettings reads the section. A database without the table
// never saved it and answers the defaults.
func LoadDelegationSettings(ctx context.Context, pool *pgxpool.Pool) (DelegationSettings, error) {
	values, err := Load(ctx, pool, SectionDelegation)
	if missingTable(err) {
		return DefaultDelegationSettings(), nil
	}
	if err != nil {
		return DefaultDelegationSettings(), err
	}
	return DelegationSettingsFromValues(values), nil
}
