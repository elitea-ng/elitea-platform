package platformconfig

import (
	"context"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
)

// SectionProactivity is the platform's settings for proactive agents
// (ADR-0050 decision 2, issue 1618):
//
//   - enabled turns every heartbeat on the platform on or off;
//   - max_wakes_per_day, max_acts_per_day and max_spend_per_day_usd are the
//     maximums of each agent's daily envelope. An agent's own limits can
//     only be lower;
//   - personal_self_approve lets the one member of a personal project turn
//     on an agent's heartbeat without a tier review (ADR-0050 Q3, option
//     D). Regulated tenants turn it off;
//   - personal_acts_per_day is the tighter act limit of a personal project.
//
// The scan job reads it at every pass.
const SectionProactivity = "proactivity"

// Field keys of SectionProactivity.
const (
	KeyProactivityEnabled             = "enabled"
	KeyProactivityMaxWakesPerDay      = "max_wakes_per_day"
	KeyProactivityMaxActsPerDay       = "max_acts_per_day"
	KeyProactivityMaxSpendPerDayUSD   = "max_spend_per_day_usd"
	KeyProactivityPersonalSelfApprove = "personal_self_approve"
	KeyProactivityPersonalActsPerDay  = "personal_acts_per_day"
)

// The defaults of SectionProactivity.
const (
	DefaultProactivityMaxWakesPerDay     = proactivity.MaxWakesPerDay
	DefaultProactivityMaxActsPerDay      = 24
	DefaultProactivityMaxSpendPerDayUSD  = 10.0
	DefaultProactivityPersonalActsPerDay = 2
)

// ProactivitySettings is the section.
type ProactivitySettings struct {
	Enabled             bool
	Ceilings            proactivity.Ceilings
	PersonalSelfApprove bool
}

// DefaultProactivitySettings is the section with no row saved.
func DefaultProactivitySettings() ProactivitySettings {
	return ProactivitySettings{
		Enabled: true,
		Ceilings: proactivity.Ceilings{
			MaxWakesPerDay:     DefaultProactivityMaxWakesPerDay,
			MaxActsPerDay:      DefaultProactivityMaxActsPerDay,
			MaxSpendPerDayUSD:  DefaultProactivityMaxSpendPerDayUSD,
			PersonalActsPerDay: DefaultProactivityPersonalActsPerDay,
		},
		PersonalSelfApprove: true,
	}
}

// ProactivitySettingsFromValues is the overlay on the defaults. A value
// that is out of range or of the wrong type is clamped to the TIGHTER end:
// a hand-written row can tighten the envelope, never silently loosen it.
func ProactivitySettingsFromValues(values Values) ProactivitySettings {
	settings := DefaultProactivitySettings()
	settings.Enabled = values.Bool(KeyProactivityEnabled, settings.Enabled)
	settings.PersonalSelfApprove = values.Bool(KeyProactivityPersonalSelfApprove, settings.PersonalSelfApprove)
	if raw, present := values[KeyProactivityMaxWakesPerDay]; present {
		settings.Ceilings.MaxWakesPerDay = clampWhole(raw, 1, proactivity.MaxWakesPerDay, 1)
	}
	if raw, present := values[KeyProactivityMaxActsPerDay]; present {
		settings.Ceilings.MaxActsPerDay = clampWhole(raw, 0, proactivity.MaxActsPerDay, 0)
	}
	if raw, present := values[KeyProactivityPersonalActsPerDay]; present {
		settings.Ceilings.PersonalActsPerDay = clampWhole(raw, 0, proactivity.MaxActsPerDay, 0)
	}
	if raw, present := values[KeyProactivityMaxSpendPerDayUSD]; present {
		number, ok := raw.(float64)
		switch {
		case !ok || math.IsNaN(number) || number < 0:
			settings.Ceilings.MaxSpendPerDayUSD = 0
		case number > proactivity.MaxSpendPerDayUSD:
			settings.Ceilings.MaxSpendPerDayUSD = proactivity.MaxSpendPerDayUSD
		default:
			settings.Ceilings.MaxSpendPerDayUSD = number
		}
	}
	return settings
}

// clampWhole reads a whole JSON number into [low, high]; a value of the
// wrong type is invalid.
func clampWhole(raw any, low, high, invalid int) int {
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) {
		return invalid
	}
	number = math.Trunc(number)
	switch {
	case number < float64(low):
		return low
	case number > float64(high):
		return high
	}
	return int(number)
}

// ValidProactivityWhole reports a whole number from low to high. JSON
// numbers arrive as float64.
func ValidProactivityWhole(raw any, low, high int) bool {
	number, ok := raw.(float64)
	return ok && !math.IsNaN(number) && number == math.Trunc(number) &&
		number >= float64(low) && number <= float64(high)
}

// ValidProactivitySpend reports a spend ceiling from 0 to the maximum.
func ValidProactivitySpend(raw any) bool {
	number, ok := raw.(float64)
	return ok && !math.IsNaN(number) && number >= 0 && number <= proactivity.MaxSpendPerDayUSD
}

// LoadProactivitySettings reads the section. A database without the table
// never saved it and answers the defaults.
func LoadProactivitySettings(ctx context.Context, pool *pgxpool.Pool) (ProactivitySettings, error) {
	values, err := Load(ctx, pool, SectionProactivity)
	if UndefinedTable(err) {
		return DefaultProactivitySettings(), nil
	}
	if err != nil {
		return ProactivitySettings{}, err
	}
	return ProactivitySettingsFromValues(values), nil
}
