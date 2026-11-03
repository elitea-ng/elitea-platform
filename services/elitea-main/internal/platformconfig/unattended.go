package platformconfig

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// Unattended grants and run chains (ADR-0045 decisions 1, 2 and 4, #1435,
// #1436). The keys live in SectionApprovals, next to the schedule and
// trigger action, because they answer the same question: what a run does
// when a step needs a person and no person is there.
//
// A grant is a person's advance decision on a run that the person starts.
// A schedule or webhook run never carries one (ADR-0041 decision 5 stays in
// force for those origins).
const (
	// KeyApprovalsUnattendedGrants turns grants on or off. Default on.
	KeyApprovalsUnattendedGrants = "unattended_grants"
	// KeyApprovalsWithheldCategories lists the tool categories a grant can
	// never name. Default [payment].
	KeyApprovalsWithheldCategories = "unattended_withheld_categories"
	// The chain limits: a default, used when the grant names none, and a
	// maximum, above which a start is refused.
	KeyApprovalsChainDefaultContinuations = "chain_default_continuations"
	KeyApprovalsChainMaxContinuations     = "chain_max_continuations"
	// KeyApprovalsChainSpendMultiplier: the default chain spend is the
	// per-run spend cap of the agent's tier times this number.
	KeyApprovalsChainSpendMultiplier = "chain_spend_multiplier"
	// KeyApprovalsChainMaxSpendUSD is the largest chain spend a grant can
	// name, in USD. 0 sets no platform maximum; the gateway's period budgets
	// still apply to every link.
	KeyApprovalsChainMaxSpendUSD          = "chain_max_spend_usd"
	KeyApprovalsChainDefaultDeadlineHours = "chain_default_deadline_hours"
	KeyApprovalsChainMaxDeadlineHours     = "chain_max_deadline_hours"
)

// Defaults and bounds of the chain keys (ADR-0045 decision 4).
const (
	DefaultChainContinuations    = 20
	DefaultChainMaxContinuations = 200
	MaxChainContinuationsBound   = 10_000

	DefaultChainSpendMultiplier = 10
	MaxChainSpendMultiplier     = 1_000
	MaxChainSpendUSDBound       = 1_000_000

	DefaultChainDeadlineHours    = 12
	DefaultChainMaxDeadlineHours = 7 * 24
	MinChainDeadlineHours        = 1
	MaxChainDeadlineHoursBound   = 30 * 24
)

// UnattendedSettings is the grant part of SectionApprovals.
type UnattendedSettings struct {
	// Enabled is approvals.unattended_grants.
	Enabled bool
	// Withheld are the categories no grant can name.
	Withheld []toolcategory.Category
	// DefaultContinuations and MaxContinuations bound max_continuations.
	DefaultContinuations int
	MaxContinuations     int
	// SpendMultiplier makes the default chain spend from the per-run cap.
	SpendMultiplier float64
	// MaxSpendUSD is the platform maximum of max_spend; 0 is none.
	MaxSpendUSD float64
	// DefaultDeadline and MaxDeadline bound the chain's wall time.
	DefaultDeadline time.Duration
	MaxDeadline     time.Duration
}

// DefaultUnattendedSettings is a deployment that never saved the keys.
func DefaultUnattendedSettings() UnattendedSettings {
	return UnattendedSettings{
		Enabled:              true,
		Withheld:             []toolcategory.Category{toolcategory.Payment},
		DefaultContinuations: DefaultChainContinuations,
		MaxContinuations:     DefaultChainMaxContinuations,
		SpendMultiplier:      DefaultChainSpendMultiplier,
		DefaultDeadline:      DefaultChainDeadlineHours * time.Hour,
		MaxDeadline:          DefaultChainMaxDeadlineHours * time.Hour,
	}
}

// Withholds reports whether no grant can name a tool of category.
func (s UnattendedSettings) Withholds(category toolcategory.Category) bool {
	for _, withheld := range s.Withheld {
		if withheld == category {
			return true
		}
	}
	return false
}

// UnattendedSettingsFromValues is the overlay of SectionApprovals. A value of
// the wrong type keeps the default. A value out of range is clamped. A
// default above its maximum is lowered to the maximum.
func UnattendedSettingsFromValues(values Values) UnattendedSettings {
	settings := DefaultUnattendedSettings()
	settings.Enabled = values.Bool(KeyApprovalsUnattendedGrants, true)
	if _, isList := values[KeyApprovalsWithheldCategories].([]any); isList {
		withheld := []toolcategory.Category{}
		for _, raw := range values.Strings(KeyApprovalsWithheldCategories) {
			if category, ok := toolcategory.Parse(raw); ok {
				withheld = append(withheld, category)
			}
		}
		settings.Withheld = withheld
	}
	settings.MaxContinuations = int(wholeInRange(values, KeyApprovalsChainMaxContinuations,
		DefaultChainMaxContinuations, 0, MaxChainContinuationsBound))
	settings.DefaultContinuations = int(wholeInRange(values, KeyApprovalsChainDefaultContinuations,
		DefaultChainContinuations, 0, MaxChainContinuationsBound))
	settings.DefaultContinuations = min(settings.DefaultContinuations, settings.MaxContinuations)
	settings.SpendMultiplier = wholeInRange(values, KeyApprovalsChainSpendMultiplier,
		DefaultChainSpendMultiplier, 1, MaxChainSpendMultiplier)
	if raw, ok := values[KeyApprovalsChainMaxSpendUSD].(float64); ok && !math.IsNaN(raw) {
		settings.MaxSpendUSD = math.Min(math.Max(raw, 0), MaxChainSpendUSDBound)
	}
	maxHours := wholeInRange(values, KeyApprovalsChainMaxDeadlineHours,
		DefaultChainMaxDeadlineHours, MinChainDeadlineHours, MaxChainDeadlineHoursBound)
	defaultHours := wholeInRange(values, KeyApprovalsChainDefaultDeadlineHours,
		DefaultChainDeadlineHours, MinChainDeadlineHours, MaxChainDeadlineHoursBound)
	settings.MaxDeadline = time.Duration(maxHours) * time.Hour
	settings.DefaultDeadline = min(time.Duration(defaultHours)*time.Hour, settings.MaxDeadline)
	return settings
}

// wholeInRange reads a whole number: absent or not a number is fallback, a
// value out of range is clamped, a fraction is cut.
func wholeInRange(values Values, key string, fallback, low, high float64) float64 {
	raw, ok := values[key].(float64)
	if !ok || math.IsNaN(raw) {
		return fallback
	}
	return math.Trunc(math.Min(math.Max(raw, low), high))
}

// ValidWholeInRange reports whether a saved value is a whole number from low
// to high.
func ValidWholeInRange(raw any, low, high float64) bool {
	number, ok := raw.(float64)
	return ok && number == math.Trunc(number) && number >= low && number <= high
}

// LoadUnattendedSettings reads the grant part of SectionApprovals. A
// database without the table never saved it and answers the defaults.
func LoadUnattendedSettings(ctx context.Context, pool *pgxpool.Pool) (UnattendedSettings, error) {
	values, err := Load(ctx, pool, SectionApprovals)
	if UndefinedTable(err) {
		return DefaultUnattendedSettings(), nil
	}
	if err != nil {
		return DefaultUnattendedSettings(), err
	}
	return UnattendedSettingsFromValues(values), nil
}

// UnattendedSettingsSource reads the grant settings on every call: the
// approvals section is live and nothing caches it.
type UnattendedSettingsSource struct {
	Pool *pgxpool.Pool
}

// Get reads the settings.
func (s UnattendedSettingsSource) Get(ctx context.Context) (UnattendedSettings, error) {
	return LoadUnattendedSettings(ctx, s.Pool)
}
