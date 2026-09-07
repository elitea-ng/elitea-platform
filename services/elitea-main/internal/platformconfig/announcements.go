package platformconfig

// The two PLATFORM-WIDE ANNOUNCEMENTS an operator can raise from the admin
// Configuration page: the notification banner and maintenance mode.
//
// They live together because they are the same shape — a switch, a message, and
// a rule about who sees it — and because the mistake to avoid is the same one
// for both: an operator flips the switch, the page reports success, and nothing
// happens. That is what BOTH sections did before this file existed. The banner
// was a build-time environment variable in the legacy SPA
// (`VITE_MAINTENANCE_BANNER`, parsed in EliteaUI's
// `features/maintenance/lib/helpers/bannerConfig.js`), so the admin form wrote a
// row no deployment ever read; maintenance mode was a gevent router hook on a
// Pylon bootstrap plugin (`legacy/plugins/bootstrap/tools/splash.py`), so the
// form had nothing to install a hook on.
//
// Both now resolve from `centry.platform_config`, which is what makes them a
// runtime switch rather than a redeploy: an operator raising a banner or taking
// the platform down for maintenance is doing so BECAUSE something is wrong, and
// a control that needs a build is not available at the moment it is needed.
//
// # Where each one is enforced
//
//   - The banner is presentation. `GET /elitea_core/platform_settings/prompt_lib`
//     marshals it and apps/elitea-web's `MaintenanceBanner` renders it. Nothing
//     is gated by it.
//   - Maintenance mode is ENFORCED, by `internal/api/middleware`'s Maintenance
//     middleware, which answers 503 to everyone who does not hold the admin
//     permission. The same resolved state is marshalled onto platform_settings
//     so the SPA can paint the splash rather than a wall of failed requests.
//
// # Failure is permissive here too, with one asymmetry that is deliberate
//
// An unreadable store yields the zero state — no banner, no maintenance — for
// the same reason every read in `internal/api/v2/eliteacore/platform_flags.go`
// is permissive: a database hiccup must not invent a platform-wide outage. The
// asymmetry is that for maintenance the permissive answer is "the platform is
// UP", which is the failure direction an operator can see and recover from. The
// other direction — an unreachable database locking every non-admin out of the
// product — is an outage this service would have caused rather than reported.

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Banner icons and styles. The schema declares these as the enum on both fields,
// and the resolver normalises to them rather than trusting the row: the legacy
// SPA normalised a free-text value the same way, and a row written before the
// enum existed must degrade to a rendered banner, not to a broken one.
const (
	BannerStyleInfo    = "info"
	BannerStyleWarning = "warning"
)

// Banner is the resolved notification-banner state.
type Banner struct {
	// Enabled is the operator's switch AND the presence of a message. A banner
	// with nothing to say is not a banner: the legacy component made the same
	// judgement (`if (!bannerConfig.enabled || !bannerConfig.message) return
	// null`), and resolving it here means the client cannot disagree.
	Enabled bool `json:"enabled"`
	// Message is Markdown. It is passed through verbatim; the renderer decides
	// what subset it honours.
	Message string `json:"message"`
	// Dismissible lets a user close it. Dismissal is per-message on the client,
	// so raising a NEW message re-shows a banner the user had closed.
	Dismissible bool   `json:"dismissible"`
	Icon        string `json:"icon"`
	Style       string `json:"style"`
}

// Maintenance is the resolved maintenance-mode state.
type Maintenance struct {
	// Enabled means non-admin traffic is refused. Unlike Banner.Enabled this is
	// NOT conditioned on the message being non-empty: a maintenance window with
	// no explanation is a worse experience, not a cancelled one, and silently
	// letting everyone back in because a field was blank would be the most
	// surprising possible reading of the switch.
	Enabled bool   `json:"enabled"`
	Title   string `json:"title"`
	Message string `json:"message"`
	// HTML is the operator's own splash body, the port of pylon's
	// `splash_template` tunable.
	//
	// It is SEPARATE from Message and does not default. Message is markdown
	// with raw HTML disabled and always resolves to something, so the splash
	// can always say a sentence; HTML is markup the operator supplied, and an
	// empty one means "use the product's own splash". A default here would put
	// this platform's words inside a field whose whole purpose is to hold
	// somebody else's.
	//
	// The server refuses executable markup on the way in (admin
	// config_values.go's validateSplashHTML) and every renderer sanitises on
	// the way out. Neither check is sufficient alone: the stored value predates
	// any given renderer, and a renderer cannot know what a future writer will
	// store.
	HTML string `json:"html"`
}

// Default copy for the splash, used when the operator left the fields empty.
// It is resolved on the SERVER so the 503 body and the SPA's splash cannot show
// different words for the same window.
const (
	DefaultMaintenanceTitle   = "Maintenance in progress"
	DefaultMaintenanceMessage = "The platform is temporarily unavailable while maintenance is carried out. " +
		"Please try again shortly."
)

// LoadBanner resolves the banner. A read error yields a disabled banner and the
// error, and every caller ignores it — see the package doc.
func LoadBanner(ctx context.Context, pool *pgxpool.Pool) (Banner, error) {
	values, err := Load(ctx, pool, SectionDedicatedBanner)
	if err != nil {
		return Banner{}, err
	}
	return bannerFrom(values), nil
}

// bannerFrom is the resolution, split from the read so the rules it applies are
// testable without a database. Everything with a decision in it lives here.
func bannerFrom(values Values) Banner {
	message := strings.TrimSpace(values.String(KeyBannerMessage, ""))
	return Banner{
		Enabled:     values.Bool(KeyBannerEnabled, false) && message != "",
		Message:     message,
		Dismissible: values.Bool(KeyBannerDismissible, false),
		Icon:        normaliseBannerToken(values.String(KeyBannerIcon, BannerStyleInfo)),
		Style:       normaliseBannerToken(values.String(KeyBannerStyle, BannerStyleInfo)),
	}
}

// LoadMaintenance resolves maintenance mode, filling the default copy.
func LoadMaintenance(ctx context.Context, pool *pgxpool.Pool) (Maintenance, error) {
	values, err := Load(ctx, pool, SectionMaintenance)
	if err != nil {
		return Maintenance{}, err
	}
	return maintenanceFrom(values), nil
}

// maintenanceFrom is the resolution half, split for the same reason bannerFrom is.
func maintenanceFrom(values Values) Maintenance {
	state := Maintenance{
		Enabled: values.Bool(KeyMaintenanceEnabled, false),
		Title:   strings.TrimSpace(values.String(KeyMaintenanceTitle, "")),
		Message: strings.TrimSpace(values.String(KeyMaintenanceMessage, "")),
		HTML:    strings.TrimSpace(values.String(KeyMaintenanceHTML, "")),
	}
	if state.Title == "" {
		state.Title = DefaultMaintenanceTitle
	}
	if state.Message == "" {
		state.Message = DefaultMaintenanceMessage
	}
	return state
}

// normaliseBannerToken folds a stored icon/style onto the enum, defaulting
// anything unrecognised to `info`. Trim-and-lowercase reproduces the legacy
// normaliser exactly (`normalizeBannerIcon`), which matters because rows written
// through the legacy admin page are the rows this deployment already holds.
func normaliseBannerToken(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case BannerStyleWarning:
		return BannerStyleWarning
	default:
		return BannerStyleInfo
	}
}
