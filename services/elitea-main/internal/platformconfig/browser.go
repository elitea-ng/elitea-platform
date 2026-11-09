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
// `browser_egress` sets the egress token of a browser run. `allowlist` (the
// default) mints a token with the platform worker egress allowlist, the
// project's worker egress entries and the project's browser `allow_domains`.
// `open`, which an administrator must choose (ADR-0049 decision 5), mints an
// open token: the egress proxy refuses only private, link-local and metadata
// addresses. A governed run always gets the allowlist token.
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

// The cloud browser engines (ADR-0049 decision 2). `full` is
// chrome-headless-shell, the default. `lite` is Moli. `auto` runs Moli first
// and moves the session to Chrome on an unsupported protocol or an empty
// snapshot.
const (
	BrowserEngineFull = "full"
	BrowserEngineLite = "lite"
	BrowserEngineAuto = "auto"
)

// BrowserAutoAdmitted says whether the bake-off of ADR-0049 decision 9
// admitted `auto` (Moli first, Chrome when Moli cannot serve the page). It is
// true by the owner decision of 2026-11-08 recorded in the ADR-0049
// appendix ("Appendix A: the engine bake-off"): bake-off run 42513255705
// (browser-bakeoff workflow, corpus revision 4) measured `auto` at 40/40,
// read and extract 100%, 0 points behind `full`, and no crash or hang. It
// moved to Chrome on the 3 tasks Moli crashed on (`engine_lost`).
const BrowserAutoAdmitted = true

// BrowserLiteAdmitted says whether the same bake-off admitted `lite` (Moli
// alone). It is false: in run 42513255705 Moli passed the success rules
// (37/40, read and extract 90.6%, 7.5 points behind `full`) but crashed on 3
// tasks, and the rule admits no engine with a crash or hang the session
// agent cannot recover (ADR-0049 appendix). Only a change that records a
// passing re-run in the appendix may set this to true; until then a
// project's `browser` setting refuses `lite`, whatever the deployment ships.
const BrowserLiteAdmitted = false

// BrowserAdmission is what the bake-off admitted. `full` is always admitted.
type BrowserAdmission struct {
	Auto bool
	Lite bool
}

// AdmittedBrowserEngines is the platform's admission (BrowserAutoAdmitted,
// BrowserLiteAdmitted).
var AdmittedBrowserEngines = BrowserAdmission{Auto: BrowserAutoAdmitted, Lite: BrowserLiteAdmitted}

// BrowserEngineNotAdmittedMessage is the refusal of a project setting that
// names an engine the bake-off did not admit.
const BrowserEngineNotAdmittedMessage = "engine_not_admitted: the cloud browser engine is not admitted by the " +
	"ADR-0049 engine bake-off (Appendix A); choose full or auto. lite (Moli alone) is not offered"

// BrowserEngines answers the engines a project may choose: `full` always,
// `lite` when it is admitted and `auto` when it is, in that order. So `full`
// and `auto` today, and all three once Moli alone passes a re-run.
func BrowserEngines(admission BrowserAdmission) []string {
	engines := []string{BrowserEngineFull}
	if admission.Lite {
		engines = append(engines, BrowserEngineLite)
	}
	if admission.Auto {
		engines = append(engines, BrowserEngineAuto)
	}
	return engines
}

// IsBrowserEngine reports whether engine is one of the three engines,
// admitted or not.
func IsBrowserEngine(engine string) bool {
	switch engine {
	case BrowserEngineFull, BrowserEngineLite, BrowserEngineAuto:
		return true
	default:
		return false
	}
}

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

// DefaultBrowserSettings is a deployment that never saved the section. Its
// egress is the allowlist: open egress is an administrator's explicit choice.
func DefaultBrowserSettings() BrowserSettings {
	return BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: DefaultBrowserMaxRunSeconds}
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
