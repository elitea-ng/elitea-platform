package admin

// The break_glass Configuration section (ADR-0059 decision 5, "Path 1" and
// "Switching approval off", owner decisions 2 and 5; story R2.2a, issue
// 2516).
//
// The section is gated on access.breakglass.manage alone (both halves of its
// narrow pair and its required_permission), so a caller without it neither
// opens the values nor sees the section in the schema list. Its PUT is a
// `critical` action of the registry with elevation `deny` and token `never`
// (adminaccess action_rows.go).
//
// Two rules sit on top of the form's field checks:
//
//   - the caps never exceed the ceilings in code (480 minutes online, 240
//     sealed: platformconfig.BreakGlassCeiling*), a default never exceeds
//     its cap, and every value stays in its range: 400 otherwise;
//   - switching approval OFF needs admin.auth.users.super_admin in the
//     caller's STANDING set (an elevation never counts towards it): 403
//     otherwise. It takes effect after the delay in force before the save,
//     and its admin.access.changed event (kind break_glass_approval, reason
//     approval_off_requested) is written at once, in the save's transaction.
//     Switching it back on needs only the section and takes effect at once,
//     cancelling a pending switch-off; it writes the event with
//     approval_on_restored. The rule is on the ACTION: a holder of the
//     section without super_admin still edits the durations.
//
// Nothing starts an elevation before R2.2c (issue 2597), so the section's
// description says the policy is not applied yet; the values are stored and
// read back like any other section, and the typed reader is
// platformconfig.LoadBreakGlassPolicy.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// breakGlassNotInForce says, on the section, that no route applies the
// policy yet.
const breakGlassNotInForce = "No route starts an elevation in this release yet: the values are stored and " +
	"become the policy of the request, start and sealed paths when they ship."

// errBreakGlassApprovalOffForbidden refuses a switch-off by a caller without
// a standing admin.auth.users.super_admin.
var errBreakGlassApprovalOffForbidden = errors.New("switching break-glass approval off needs admin.auth.users.super_admin")

// errBreakGlassDelayShortenForbidden refuses a shorter approval-off delay to
// a caller without a standing super_admin: a shorter delay weakens the next
// switch-off as much as the switch-off itself.
var errBreakGlassDelayShortenForbidden = errors.New("shortening the break-glass approval-off delay needs admin.auth.users.super_admin")

// shortensApprovalOffDelay reports whether next holds an effective
// approval-off delay shorter than the one stored.
func shortensApprovalOffDelay(stored platformconfig.Values, next map[string]any) bool {
	return platformconfig.BreakGlassSettingsFromValues(platformconfig.Values(next)).ApprovalOffDelay <
		platformconfig.BreakGlassSettingsFromValues(stored).ApprovalOffDelay
}

// breakGlassSectionAccess is the section's pair: access.breakglass.manage for
// both halves, in the Security area.
var breakGlassSectionAccess = singleAccess(adminaccess.AreaSecurity, adminaccess.AccessBreakglassManage)

// breakGlassFieldText is the title and description of each whole-number field.
var breakGlassFieldText = map[string][2]string{
	platformconfig.KeyBreakGlassOnlineDefaultMinutes: {"Online Default Duration (minutes)",
		"The duration an online elevation gets when the request names none."},
	platformconfig.KeyBreakGlassOnlineMaxMinutes: {"Online Maximum Duration (minutes)",
		"The longest online elevation a request may ask for. At most the ceiling in code."},
	platformconfig.KeyBreakGlassSealedDefaultMinutes: {"Sealed Default Duration (minutes)",
		"The duration of an elevation started with the sealed credential when none is named."},
	platformconfig.KeyBreakGlassSealedMaxMinutes: {"Sealed Maximum Duration (minutes)",
		"The longest elevation the sealed credential may start. At most the ceiling in code."},
	platformconfig.KeyBreakGlassRequestLifetimeMins: {"Request Lifetime (minutes)",
		"A request nobody approves or denies in this time expires."},
	platformconfig.KeyBreakGlassReviewHoursOnline: {"Review Time, Online (hours)",
		"An ended online elevation not reviewed in this time is overdue."},
	platformconfig.KeyBreakGlassReviewHoursSealed: {"Review Time After a Sealed Sign-in (hours)",
		"An ended elevation of the sealed path not reviewed in this time is overdue."},
	platformconfig.KeyBreakGlassReauthenticationMins: {"Re-authentication Window (minutes)",
		"The start needs a sign-in within this time. It can be lowered, not raised."},
	platformconfig.KeyBreakGlassApprovalOffDelayHours: {"Approval-off Delay (hours)",
		"Switching approval off takes effect this long after the save. The delay in force before the " +
			"save applies."},
}

func breakGlassSection() map[string]any {
	const section = platformconfig.SectionBreakGlass
	fields := []map[string]any{{
		"key":   platformconfig.KeyBreakGlassApprovalRequired,
		"type":  "boolean",
		"title": "Approval Required",
		"description": "A second person with access.breakglass.approve approves every elevation request. " +
			"Switching it off needs admin.auth.users.super_admin, takes effect after the approval-off delay " +
			"and is recorded in the security audit at once; then a request starts at the admin level " +
			"without an approver. Switching it on takes effect at once and cancels a pending switch-off.",
		"section": section,
		"default": true,
	}}
	for _, bound := range platformconfig.BreakGlassBounds() {
		text := breakGlassFieldText[bound.Key]
		fields = append(fields, map[string]any{
			"key":         bound.Key,
			"type":        "integer",
			"title":       text[0],
			"description": fmt.Sprintf("%s From %d to %d.", text[1], bound.Min, bound.Max),
			"section":     section,
			"default":     bound.Default,
			"minimum":     bound.Min,
			"maximum":     bound.Max,
		})
	}
	return map[string]any{
		"id":     section,
		"access": breakGlassSectionAccess,
		"title":  "Break-glass",
		"description": fmt.Sprintf("Durations, caps, the request lifetime, the review times and the approval "+
			"switch of break-glass elevations. A deployment can lower any value and raise a cap only up to "+
			"the ceilings in code: %d minutes online and %d sealed. %s",
			platformconfig.BreakGlassCeilingOnlineMinutes, platformconfig.BreakGlassCeilingSealedMinutes,
			breakGlassNotInForce),
		"order":               10,
		"icon":                "shield",
		"always_visible":      true,
		"required_permission": adminaccess.AccessBreakglassManage,
		"fields":              fields,
	}
}

// validateBreakGlassValues checks the section as it will be stored: the body
// over the stored values. Every number is a whole number in its range (the
// caps' maxima are the ceilings in code), and a default does not exceed its
// cap.
func validateBreakGlassValues(section string, merged map[string]any) string {
	if section != platformconfig.SectionBreakGlass {
		return ""
	}
	values := platformconfig.Values(merged)
	for _, bound := range platformconfig.BreakGlassBounds() {
		raw, present := merged[bound.Key]
		if !present {
			continue
		}
		number, ok := raw.(float64)
		if !ok || number != float64(int64(number)) || int64(number) < bound.Min || int64(number) > bound.Max {
			return fmt.Sprintf("%q must be a whole number from %d to %d", bound.Key, bound.Min, bound.Max)
		}
	}
	for _, pair := range [][2]string{
		{platformconfig.KeyBreakGlassOnlineDefaultMinutes, platformconfig.KeyBreakGlassOnlineMaxMinutes},
		{platformconfig.KeyBreakGlassSealedDefaultMinutes, platformconfig.KeyBreakGlassSealedMaxMinutes},
	} {
		defaultBound, _ := platformconfig.BreakGlassBoundFor(pair[0])
		capBound, _ := platformconfig.BreakGlassBoundFor(pair[1])
		value, ok := values.Int(pair[0])
		if !ok {
			value = defaultBound.Default
		}
		limit, ok := values.Int(pair[1])
		if !ok {
			limit = capBound.Default
		}
		if value > limit {
			return fmt.Sprintf("%q (%d) must not exceed %q (%d)", pair[0], value, pair[1], limit)
		}
	}
	return ""
}

// storeHook runs first in the save's transaction and may add keys to the
// values it stores.
type storeHook func(ctx context.Context, tx pgx.Tx, values map[string]any) error

// prepareBreakGlassSave validates a save of the section and answers the hook
// that applies the approval switch in the save's transaction. A status of 0
// means the save may proceed.
func (h *Handler) prepareBreakGlassSave(r *http.Request, body map[string]any) (storeHook, int, string) {
	stored, err := h.loadSectionValues(r, platformconfig.SectionBreakGlass)
	if err != nil {
		return nil, http.StatusInternalServerError, "could not read the platform configuration store"
	}
	merged := make(map[string]any, len(stored)+len(body))
	for key, value := range stored {
		merged[key] = value
	}
	for key, value := range body {
		merged[key] = value
	}
	if reason := validateBreakGlassValues(platformconfig.SectionBreakGlass, merged); reason != "" {
		return nil, http.StatusBadRequest, reason
	}
	requested, present := body[platformconfig.KeyBreakGlassApprovalRequired].(bool)
	_, delayInBody := body[platformconfig.KeyBreakGlassApprovalOffDelayHours]
	if !present && !delayInBody {
		return nil, 0, ""
	}
	// Both weakening changes need a standing super_admin: switching approval
	// off, and shortening the delay a later switch-off waits.
	superAdmin := h.holdsStandingSuperAdmin(r)
	if !superAdmin {
		// Refused before the transaction when the stored state already says
		// so; the hook decides again under the section's lock.
		if delayInBody && shortensApprovalOffDelay(platformconfig.Values(stored), merged) {
			return nil, http.StatusForbidden, errBreakGlassDelayShortenForbidden.Error()
		}
		if present && !requested && platformconfig.PlanBreakGlassApproval(platformconfig.Values(stored), false, h.clock()).Change ==
			platformconfig.BreakGlassApprovalOffRequested {
			return nil, http.StatusForbidden, errBreakGlassApprovalOffForbidden.Error()
		}
	}
	hook := func(ctx context.Context, tx pgx.Tx, values map[string]any) error {
		// One save of the approval switch at a time: the plan reads the state
		// it changes.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('platform_config:break_glass', 0))`); err != nil {
			return fmt.Errorf("lock the break_glass section: %w", err)
		}
		current, err := platformconfig.LoadVia(ctx, func(ctx context.Context, sql string, args ...any) (platformconfig.Rows, error) {
			return tx.Query(ctx, sql, args...)
		}, platformconfig.SectionBreakGlass)
		if err != nil {
			return fmt.Errorf("read the break_glass section: %w", err)
		}
		if delayInBody && !superAdmin && shortensApprovalOffDelay(current, values) {
			return errBreakGlassDelayShortenForbidden
		}
		if !present {
			return nil
		}
		plan := platformconfig.PlanBreakGlassApproval(current, requested, h.clock())
		if plan.Change == platformconfig.BreakGlassApprovalOffRequested && !superAdmin {
			return errBreakGlassApprovalOffForbidden
		}
		for key, value := range plan.Writes {
			values[key] = value
		}
		if plan.Change == platformconfig.BreakGlassApprovalUnchanged {
			return nil
		}
		before, after := []string{platformconfig.KeyBreakGlassApprovalRequired}, []string(nil)
		if plan.Change == platformconfig.BreakGlassApprovalOnRestored {
			before, after = nil, []string{platformconfig.KeyBreakGlassApprovalRequired}
		}
		return h.appendAccessChange(ctx, tx, r, securityaudit.AdminAccessChanged{
			SubjectType: "configuration_section",
			SubjectID:   platformconfig.SectionBreakGlass,
			Change: securityaudit.AccessChange{
				Kind:       securityaudit.AccessBreakGlassApproval,
				Before:     before,
				After:      after,
				Fields:     []string{platformconfig.KeyBreakGlassApprovalRequired},
				ReasonCode: plan.Change,
			},
		})
	}
	return hook, 0, ""
}

// holdsStandingSuperAdmin reports whether the caller's STANDING
// administration set holds admin.auth.users.super_admin: an elevation never
// counts towards it. It fails closed.
func (h *Handler) holdsStandingSuperAdmin(r *http.Request) bool {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok || h.resolver == nil {
		return false
	}
	resolution, err := auth.ResolveStandingAdministration(r.Context(), h.resolver, principal)
	if err != nil {
		return false
	}
	return slices.Contains(resolution.Permissions, adminaccess.SuperAdminPermission)
}

// breakGlassStatus is what the section shows beside its values: the
// effective approval state now, when a pending switch-off takes effect, and
// that no route applies the policy yet.
func (h *Handler) breakGlassStatus(stored map[string]any) map[string]any {
	policy := platformconfig.BreakGlassSettingsFromValues(stored).At(h.clock())
	status := map[string]any{
		"approval_state": policy.ApprovalState,
		"in_force":       false,
		"note":           breakGlassNotInForce,
	}
	if !policy.ApprovalOffEffectiveFrom.IsZero() {
		status["approval_off_effective_from"] = policy.ApprovalOffEffectiveFrom.Format(time.RFC3339)
	}
	return status
}

// WithClock sets the clock of the handler's time-dependent rules (the
// break-glass approval switch); nil keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) { h.now = now }
}

func (h *Handler) clock() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}
