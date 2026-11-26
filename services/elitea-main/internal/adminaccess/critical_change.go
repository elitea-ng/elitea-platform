package adminaccess

// A `critical` action allowed under an elevation is announced at once
// (ADR-0059 decision 5, "Five rules are not per action"; story R2.8b, issue
// 2602): it writes the admin.elevation action `critical_change` (R2.3) and
// the notice rows of an activation (R2.5a) IN THE TRANSACTION OF THE CHANGE.
// The review is then not the first time someone hears of it.
//
// Which actions are `critical` is the action table's Danger: there is no
// second list. The restriction (the middleware of internal/api, or the
// handler half RestrictElevation) arms the announcement when it admits a
// critical entry under an elevation; the write that makes the change runs it
// in its transaction:
//
//   - a store that owns the transaction runs it through
//     securityaudit.RunChangeHook, which every store with a change hook
//     already calls (the identity provider store, the SCIM client store, the
//     configuration writes behind the global providers);
//   - a handler that owns its transaction calls
//     securityaudit.RunTransactionHooks before its commit (the two role
//     assignment routes).
//
// A failed append or notice write fails the hook, and the change rolls back
// with it: fail closed on audit. A critical action whose writer does not run
// the hook is NOT in CriticalChangeAnnounced, and is refused under an
// elevation (elevation_restricted) rather than made silently.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// CriticalChangeAnnounced names, by action key, the critical entries whose
// writer runs the announcement in the transaction of the change. A critical
// entry whose elevation value is not `deny` and that is not here is refused
// under an elevation: TestEveryCriticalElevationActionIsAnnouncedOrRefused
// lists them, so a new critical entry is one or the other on purpose.
var CriticalChangeAnnounced = map[string]bool{
	// The two role assignment routes run the hook before their commit.
	"POST /api/v2/admin/auth_users/{mode} set_admin_role": true,
	"POST /api/v2/admin/modes/administration":             true,
	"DELETE /api/v2/admin/modes/administration":           true,
	// The global provider writes run the configuration change hook.
	"POST /api/v2/admin/gateway/providers":                 true,
	"PUT /api/v2/admin/gateway/providers/{configID} route": true,
	// The identity provider and SCIM client stores run the change hook.
	"PUT /api/v2/admin/identity_providers/administration/{key} disable": true,
	"PUT /api/v2/admin/identity_providers/administration/{key} trust":   true,
	"DELETE /api/v2/admin/identity_providers/administration/{key}":      true,
	"POST /api/v2/admin/identity_providers/administration/{key}/rebind": true,
	"POST /api/v2/admin/scim_clients/administration":                    true,
	"POST /api/v2/admin/scim_clients/administration/{id}/rotate":        true,
}

// CriticalAnnouncer is what the announcement writes with: the security
// audit appender and the clock. The router builds one; nil refuses every
// critical action under an elevation.
type CriticalAnnouncer struct {
	Appender securityaudit.Appender
	Now      func() time.Time
}

func (a *CriticalAnnouncer) usable() bool {
	return a != nil && a.Appender != nil && a.Now != nil
}

// criticalRefusal is the refusal of a critical action under an elevation
// that cannot be announced.
func criticalRefusal() *Refusal {
	return elevationRefusal("this critical administration action cannot be announced under an elevation, " +
		"so an elevation cannot do it: it needs the permission by a standing role")
}

// ArmCriticalChange arms the announcement of entry for an elevated caller
// whose request the restriction admitted: it answers nil when entry is not
// critical or the announcement is armed, and a refusal when it cannot be
// (the entry's writer does not run it, or there is no announcer or no hook
// set on ctx). An unelevated caller, and an entry the standing set admits,
// arm nothing.
func ArmCriticalChange(ctx context.Context, entry Action, caller ElevationCaller) *Refusal {
	if entry.Danger != DangerCritical || !caller.Elevated || entry.Permission == "" ||
		standingAdmits(caller.Standing, entry.Permission) {
		return nil
	}
	if !CriticalChangeAnnounced[entry.Key()] || !caller.Announcer.usable() || len(caller.ElevationIDs) == 0 {
		return criticalRefusal()
	}
	ids := make([]uuid.UUID, 0, len(caller.ElevationIDs))
	for _, raw := range caller.ElevationIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return criticalRefusal()
		}
		ids = append(ids, id)
	}
	announcer := caller.Announcer
	change := CriticalChange{
		Action: entry, ActorUserID: caller.UserID, SessionRef: caller.SessionRef,
	}
	armed := securityaudit.ArmTransactionHook(ctx, func(ctx context.Context, tx pgx.Tx) error {
		change.At = announcer.Now()
		for _, id := range ids {
			change.ElevationID = id
			if err := AnnounceCriticalChange(ctx, tx, announcer.Appender, change); err != nil {
				return err
			}
		}
		return nil
	})
	if !armed {
		return criticalRefusal()
	}
	return nil
}

// CriticalChange is one critical change made under an elevation.
type CriticalChange struct {
	ElevationID uuid.UUID
	// Action is the action table entry the change is.
	Action Action
	// ActorUserID is the elevated person.
	ActorUserID int64
	// SessionRef is the browser session the elevation is bound to.
	SessionRef int64
	At         time.Time
}

// AnnounceCriticalChange writes the `critical_change` event of one change and
// its notice rows on tx, the transaction of the change. An error means the
// caller rolls the change back.
func AnnounceCriticalChange(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, change CriticalChange) error {
	if appender == nil {
		return ErrElevationNoAppender
	}
	if change.At.IsZero() {
		return ErrElevationNoTimestamp
	}
	elevation, err := ReadElevation(ctx, tx, change.ElevationID)
	if err != nil {
		return fmt.Errorf("announce the critical change: %w", err)
	}
	if !elevation.ActiveAt(change.At) {
		return errors.New("announce the critical change: the elevation is not active")
	}
	if err := AppendElevationEvent(ctx, tx, appender, ElevationEvent{
		Action:            securityaudit.ElevationCriticalChange,
		Source:            securityaudit.ElevationSourceConsole,
		Elevation:         elevation,
		ActorUserID:       change.ActorUserID,
		AuthenticationRef: sessionAuthenticationRef(change.SessionRef),
		Reasons:           []string{"permission:" + change.Action.Permission},
	}); err != nil {
		return err
	}
	if _, err := WriteElevationNotices(ctx, tx, ElevationNotice{
		Kind: NoticeCriticalChange, Elevation: elevation, ActorUserID: change.ActorUserID,
		Action: change.Action.Key(), At: change.At,
	}); err != nil {
		return fmt.Errorf("announce the critical change: write the notices: %w", err)
	}
	slog.WarnContext(ctx, "a critical administration action was made under an elevation",
		"elevation_id", elevation.ID.String(), "action", change.Action.Key())
	return nil
}
