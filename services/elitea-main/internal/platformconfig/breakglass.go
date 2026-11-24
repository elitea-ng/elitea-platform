package platformconfig

// The `break_glass` section (ADR-0059 decision 5, "Path 1: elevation while
// single sign-on works" and "Switching approval off", owner decisions 2 and
// 5; story R2.2a, issue 2516): the durations, the caps under the ceilings in
// code, the request lifetime, the review times, the re-authentication window
// of the start, and the approval switch with its delay.
//
// The approval switch is stored as a REQUESTED state plus an effective-from
// time, never flipped by a job: switching approval off stores
// `approval_required: false` and `approval_off_effective_from` (the save time
// plus the delay), and BreakGlassSettings.At compares that time with the
// clock. So "scheduled to go off" and "off" are distinct answers of the
// reader, and nothing has to run for the switch to take effect. Switching it
// back on stores `approval_required: true` and clears the time: at once, and a
// pending switch-off is cancelled.
//
// The readers are R2.2b (the request routes), R2.2c (the start), R2.7b (the
// sealed path), R2.14b (the posture checks) and the elevation reconciler
// (R2.16, the request lifetime). Until R2.2c ships nothing starts an
// elevation, so the values are a policy no route applies yet.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// SectionBreakGlass is the break-glass policy section.
const SectionBreakGlass = "break_glass"

// Keys of SectionBreakGlass.
const (
	// KeyBreakGlassApprovalRequired is the REQUESTED approval state (default
	// true: dual control). false takes effect at
	// KeyBreakGlassApprovalOffEffectiveFrom.
	KeyBreakGlassApprovalRequired = "approval_required"
	// KeyBreakGlassApprovalOffEffectiveFrom is when a requested switch-off
	// takes effect (RFC 3339). Written by the save path only, never by the
	// form: it is not a schema field.
	KeyBreakGlassApprovalOffEffectiveFrom = "approval_off_effective_from"

	KeyBreakGlassOnlineDefaultMinutes  = "online_default_minutes"
	KeyBreakGlassOnlineMaxMinutes      = "online_max_minutes"
	KeyBreakGlassSealedDefaultMinutes  = "sealed_default_minutes"
	KeyBreakGlassSealedMaxMinutes      = "sealed_max_minutes"
	KeyBreakGlassRequestLifetimeMins   = "request_lifetime_minutes"
	KeyBreakGlassReviewHoursOnline     = "review_hours_online"
	KeyBreakGlassReviewHoursSealed     = "review_hours_sealed"
	KeyBreakGlassReauthenticationMins  = "reauthentication_minutes"
	KeyBreakGlassApprovalOffDelayHours = "approval_off_delay_hours"
)

// The ceilings in code (owner decision 5): a deployment can raise a cap only
// up to these. They are the ledger's own constants, so the section, the
// request and sealed paths and the table's CHECK constraint have one source.
const (
	BreakGlassCeilingOnlineMinutes = int64(adminaccess.MaxElevationDurationOnline / time.Minute)
	BreakGlassCeilingSealedMinutes = int64(adminaccess.MaxElevationDurationSealed / time.Minute)
)

// BreakGlassBound is one whole-number setting of the section: its key, its
// default and its range. Max of a cap is the ceiling in code.
type BreakGlassBound struct {
	Key      string
	Default  int64
	Min, Max int64
	// Unit is the time unit of the value.
	Unit time.Duration
}

// BreakGlassBounds lists the whole-number settings, in the order of the
// form. A value that weakens a control when raised (the re-authentication
// window) has its default as its maximum; the approval-off delay can be
// lowered to an hour and no further.
func BreakGlassBounds() []BreakGlassBound {
	return []BreakGlassBound{
		{KeyBreakGlassOnlineDefaultMinutes, 60, 1, BreakGlassCeilingOnlineMinutes, time.Minute},
		{KeyBreakGlassOnlineMaxMinutes, 240, 1, BreakGlassCeilingOnlineMinutes, time.Minute},
		{KeyBreakGlassSealedDefaultMinutes, 60, 1, BreakGlassCeilingSealedMinutes, time.Minute},
		{KeyBreakGlassSealedMaxMinutes, 120, 1, BreakGlassCeilingSealedMinutes, time.Minute},
		{KeyBreakGlassRequestLifetimeMins, 30, 1, 1440, time.Minute},
		{KeyBreakGlassReviewHoursOnline, 72, 1, 720, time.Hour},
		{KeyBreakGlassReviewHoursSealed, 24, 1, 720, time.Hour},
		{KeyBreakGlassReauthenticationMins, 15, 1, 15, time.Minute},
		{KeyBreakGlassApprovalOffDelayHours, 24, 1, 168, time.Hour},
	}
}

// BreakGlassBoundFor answers the bound of key.
func BreakGlassBoundFor(key string) (BreakGlassBound, bool) {
	for _, bound := range BreakGlassBounds() {
		if bound.Key == key {
			return bound, true
		}
	}
	return BreakGlassBound{}, false
}

// BreakGlassSettings is the section as stored, over the defaults: the
// requested approval state and its effective-from time, not yet compared
// with a clock (At does that).
type BreakGlassSettings struct {
	// ApprovalRequested is the requested approval state.
	ApprovalRequested bool
	// ApprovalOffEffectiveFrom is when a requested switch-off takes effect;
	// zero when approval is requested on.
	ApprovalOffEffectiveFrom time.Time

	OnlineDefault    time.Duration
	OnlineMax        time.Duration
	SealedDefault    time.Duration
	SealedMax        time.Duration
	RequestLifetime  time.Duration
	ReviewOnline     time.Duration
	ReviewSealed     time.Duration
	Reauthentication time.Duration
	ApprovalOffDelay time.Duration
}

// The approval states of the effective policy.
const (
	// BreakGlassApprovalOn: a request needs a second person.
	BreakGlassApprovalOn = "on"
	// BreakGlassApprovalOffScheduled: a switch-off was requested and its
	// delay has not passed; a request still needs a second person.
	BreakGlassApprovalOffScheduled = "off_scheduled"
	// BreakGlassApprovalOff: the delay has passed; a request starts at the
	// admin level without an approver.
	BreakGlassApprovalOff = "off"
)

// BreakGlassPolicy is the effective policy at one time.
type BreakGlassPolicy struct {
	BreakGlassSettings
	// At is the time the policy was evaluated at.
	At time.Time
	// ApprovalRequired is the EFFECTIVE answer: true unless a switch-off was
	// requested and its effective-from time has passed.
	ApprovalRequired bool
	// ApprovalState is on, off_scheduled or off.
	ApprovalState string
}

// DefaultBreakGlassSettings is the ADR default: approval on, 60/240 minutes
// online, 60/120 sealed, a 30-minute request lifetime, a review within 72
// hours (24 after a sealed sign-in), a 15-minute re-authentication window and
// a 24-hour approval-off delay.
func DefaultBreakGlassSettings() BreakGlassSettings {
	return BreakGlassSettingsFromValues(nil)
}

// BreakGlassSettingsFromValues overlays the stored section on the defaults.
// A number out of range or of the wrong type keeps the default, and a default
// above its cap is lowered to the cap, so the default never exceeds what a
// request may ask for. A requested switch-off
// without a readable effective-from time (a hand edit) keeps approval ON:
// the switch fails closed.
func BreakGlassSettingsFromValues(values Values) BreakGlassSettings {
	read := func(key string) time.Duration {
		bound, _ := BreakGlassBoundFor(key)
		n, ok := values.Int(key)
		if !ok || n < bound.Min || n > bound.Max {
			n = bound.Default
		}
		return time.Duration(n) * bound.Unit
	}
	settings := BreakGlassSettings{
		ApprovalRequested: true,
		OnlineDefault:     read(KeyBreakGlassOnlineDefaultMinutes),
		OnlineMax:         read(KeyBreakGlassOnlineMaxMinutes),
		SealedDefault:     read(KeyBreakGlassSealedDefaultMinutes),
		SealedMax:         read(KeyBreakGlassSealedMaxMinutes),
		RequestLifetime:   read(KeyBreakGlassRequestLifetimeMins),
		ReviewOnline:      read(KeyBreakGlassReviewHoursOnline),
		ReviewSealed:      read(KeyBreakGlassReviewHoursSealed),
		Reauthentication:  read(KeyBreakGlassReauthenticationMins),
		ApprovalOffDelay:  read(KeyBreakGlassApprovalOffDelayHours),
	}
	settings.OnlineDefault = min(settings.OnlineDefault, settings.OnlineMax)
	settings.SealedDefault = min(settings.SealedDefault, settings.SealedMax)
	if !values.Bool(KeyBreakGlassApprovalRequired, true) {
		from, err := time.Parse(time.RFC3339Nano, values.String(KeyBreakGlassApprovalOffEffectiveFrom, ""))
		if err == nil {
			settings.ApprovalRequested = false
			settings.ApprovalOffEffectiveFrom = from.UTC()
		}
	}
	return settings
}

// At is the effective policy at now.
func (s BreakGlassSettings) At(now time.Time) BreakGlassPolicy {
	policy := BreakGlassPolicy{BreakGlassSettings: s, At: now, ApprovalRequired: true, ApprovalState: BreakGlassApprovalOn}
	if !s.ApprovalRequested {
		if now.Before(s.ApprovalOffEffectiveFrom) {
			policy.ApprovalState = BreakGlassApprovalOffScheduled
		} else {
			policy.ApprovalRequired, policy.ApprovalState = false, BreakGlassApprovalOff
		}
	}
	return policy
}

// The approval changes a save can make.
const (
	// BreakGlassApprovalUnchanged: the save leaves the requested state as it
	// was (a pending switch-off keeps its time).
	BreakGlassApprovalUnchanged = ""
	// BreakGlassApprovalOffRequested: approval was on and a switch-off is now
	// scheduled. Needs admin.auth.users.super_admin.
	BreakGlassApprovalOffRequested = "approval_off_requested"
	// BreakGlassApprovalOnRestored: approval was off or scheduled to go off
	// and is on again, at once.
	BreakGlassApprovalOnRestored = "approval_on_restored"
)

// BreakGlassApprovalPlan is what a save of the approval switch writes.
type BreakGlassApprovalPlan struct {
	// Change is one of the BreakGlassApproval* changes.
	Change string
	// Before is the effective approval state at the save, before it.
	Before string
	// Writes are the keys to store beside the saved values (the
	// effective-from time, or its removal as null).
	Writes map[string]any
	// EffectiveFrom is when a requested switch-off takes effect.
	EffectiveFrom time.Time
}

// PlanBreakGlassApproval decides what a save that sets approval_required to
// requested does, against the stored section, at now. A switch-off takes
// effect after the delay IN FORCE BEFORE the save, so a save that also lowers
// the delay does not shorten its own wait. Saving false again while a
// switch-off is pending keeps its time; saving true cancels it.
func PlanBreakGlassApproval(stored Values, requested bool, now time.Time) BreakGlassApprovalPlan {
	current := BreakGlassSettingsFromValues(stored)
	plan := BreakGlassApprovalPlan{Before: current.At(now).ApprovalState}
	switch {
	case !requested && current.ApprovalRequested:
		plan.Change = BreakGlassApprovalOffRequested
		plan.EffectiveFrom = now.UTC().Add(current.ApprovalOffDelay)
		plan.Writes = map[string]any{
			KeyBreakGlassApprovalOffEffectiveFrom: plan.EffectiveFrom.Format(time.RFC3339Nano),
		}
	case requested && !current.ApprovalRequested:
		plan.Change = BreakGlassApprovalOnRestored
		plan.Writes = map[string]any{KeyBreakGlassApprovalOffEffectiveFrom: nil}
	case requested:
		// Approval on and saved on: clear any stray effective-from time a
		// hand edit left, so a later false cannot inherit it.
		if _, present := stored[KeyBreakGlassApprovalOffEffectiveFrom]; present {
			plan.Writes = map[string]any{KeyBreakGlassApprovalOffEffectiveFrom: nil}
		}
	}
	return plan
}

// LoadBreakGlassSettings reads the section. A database without the table
// never saved it and answers the defaults.
func LoadBreakGlassSettings(ctx context.Context, pool *pgxpool.Pool) (BreakGlassSettings, error) {
	values, err := Load(ctx, pool, SectionBreakGlass)
	if UndefinedTable(err) {
		return DefaultBreakGlassSettings(), nil
	}
	if err != nil {
		return BreakGlassSettings{}, err
	}
	return BreakGlassSettingsFromValues(values), nil
}

// LoadBreakGlassPolicy reads the effective policy at now.
func LoadBreakGlassPolicy(ctx context.Context, pool *pgxpool.Pool, now time.Time) (BreakGlassPolicy, error) {
	settings, err := LoadBreakGlassSettings(ctx, pool)
	if err != nil {
		return BreakGlassPolicy{}, err
	}
	return settings.At(now), nil
}
