package platformconfig

import (
	"context"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionBrowser is the platform's cloud browser policy (ADR-0049 decisions
// 5 and 8, #1596). Main reads it when it serves the `browser` runtime-context
// field to the worker.
//
// `browser_egress` sets the egress token of a browser run. `open` (the
// default) mints an open token: the egress proxy refuses only private,
// link-local and metadata addresses. `allowlist` mints a token with the
// platform worker egress allowlist, the project's worker egress entries and
// the project's browser `allow_domains`. A governed run always gets the
// allowlist token.
const SectionBrowser = "browser"

// Field keys of SectionBrowser.
const (
	KeyBrowserEgress              = "browser_egress"
	KeyBrowserMaxRunSeconds       = "max_run_seconds"
	KeyBrowserMonthlyQuotaSeconds = "monthly_quota_seconds"
)

// BrowserEgressMode is the platform browser egress mode.
type BrowserEgressMode string

const (
	BrowserEgressOpen      BrowserEgressMode = "open"
	BrowserEgressAllowlist BrowserEgressMode = "allowlist"
)

// Defaults and bounds of SectionBrowser. 0 is no cap and no quota.
const (
	DefaultBrowserMaxRunSeconds   = 1800
	MaxBrowserMaxRunSeconds       = 86_400
	MaxBrowserMonthlyQuotaSeconds = 100_000_000
)

// BrowserSettings is the platform section.
type BrowserSettings struct {
	Egress BrowserEgressMode
	// MaxRunSeconds is the most browser engine seconds one run may use.
	// 0 is no cap.
	MaxRunSeconds int64
	// MonthlyQuotaSeconds is the most browser engine seconds one project
	// may use in one calendar month (UTC). 0 is no quota.
	MonthlyQuotaSeconds int64
}

// DefaultBrowserSettings is a deployment that never saved the section.
func DefaultBrowserSettings() BrowserSettings {
	return BrowserSettings{Egress: BrowserEgressOpen, MaxRunSeconds: DefaultBrowserMaxRunSeconds}
}

// LoadBrowserSettings reads the section over the defaults. A query error
// answers the defaults and the error. A database without the table never
// saved the section, so it answers the defaults without an error.
func LoadBrowserSettings(ctx context.Context, pool *pgxpool.Pool) (BrowserSettings, error) {
	values, err := Load(ctx, pool, SectionBrowser)
	if UndefinedTable(err) {
		return DefaultBrowserSettings(), nil
	}
	if err != nil {
		return DefaultBrowserSettings(), err
	}
	return BrowserSettingsFromValues(values), nil
}

// BrowserSettingsFromValues is the overlay. An unknown egress mode is
// `allowlist`: a hand-written row narrows, never widens. A number that does
// not read is the tightest value (1 second), never "no cap"; a number past
// its range is clamped to it.
func BrowserSettingsFromValues(values Values) BrowserSettings {
	settings := DefaultBrowserSettings()
	if raw, present := values[KeyBrowserEgress]; present {
		mode, _ := raw.(string)
		switch BrowserEgressMode(strings.TrimSpace(mode)) {
		case BrowserEgressOpen:
			settings.Egress = BrowserEgressOpen
		default:
			settings.Egress = BrowserEgressAllowlist
		}
	}
	if raw, present := values[KeyBrowserMaxRunSeconds]; present {
		settings.MaxRunSeconds = readBrowserSeconds(raw, MaxBrowserMaxRunSeconds)
	}
	if raw, present := values[KeyBrowserMonthlyQuotaSeconds]; present {
		settings.MonthlyQuotaSeconds = readBrowserSeconds(raw, MaxBrowserMonthlyQuotaSeconds)
	}
	return settings
}

func readBrowserSeconds(raw any, limit int64) int64 {
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || number < 0 {
		return 1
	}
	if number > float64(limit) {
		return limit
	}
	if number == 0 {
		return 0
	}
	return max(int64(math.Trunc(number)), 1)
}

// ValidBrowserSeconds reports whether a saved value is 0 or a whole number of
// seconds up to limit.
func ValidBrowserSeconds(raw any, limit int64) bool {
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) {
		return false
	}
	return number >= 0 && number <= float64(limit)
}
