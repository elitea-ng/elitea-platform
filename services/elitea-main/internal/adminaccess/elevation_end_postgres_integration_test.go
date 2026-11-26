package adminaccess_test

// An elevation ends with its session and with its person (ADR-0059 decision
// 5, decision 3; story R2.11, issue 2525), on the real schema: the browser
// session store, the deactivation hook, the sealed credential store and the
// reconciler's orphan pass.

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func sessionStore(t *testing.T, pool *pgxpool.Pool) *browsersession.PostgresStore {
	t.Helper()
	store, err := browsersession.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func sessionIDOf(t *testing.T, pool *pgxpool.Pool, ref int64) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(),
		`SELECT id FROM elitea_auth.browser_sessions WHERE ref = $1`, ref).Scan(&id); err != nil {
		t.Fatalf("read the session id: %v", err)
	}
	return id
}

func elevatedSet(t *testing.T, pool *pgxpool.Pool, userID, ref int64) []string {
	t.Helper()
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(t.Context(),
		sessionPrincipal(userID, ref), auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return resolution.ElevatedPermissions
}

func revokePersonAccess(t *testing.T, pool *pgxpool.Pool, userID int64, reason string, at time.Time) {
	t.Helper()
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.RevokePersonAccess(t.Context(), tx, userID,
			adminaccess.PersonAccessEnd{ElevationEnd: reason, Appender: auditAppender(t), At: at})
		return err
	}); err != nil {
		t.Fatalf("revoke the access of the person: %v", err)
	}
}

// After a logout the elevation is ended with `logout` and one `ended` event,
// and a new session of the same person resolves no elevated permission.
func TestLogoutEndsTheElevationOfThatSession(t *testing.T) {
	pool := newElevationDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	if len(elevatedSet(t, pool, elevation.UserID, elevation.BrowserSessionRef)) == 0 {
		t.Fatal("premise: the active elevation resolves nothing")
	}

	if err := sessionStore(t, pool).Revoke(t.Context(), sessionIDOf(t, pool, elevation.BrowserSessionRef), now); err != nil {
		t.Fatal(err)
	}
	ended := readElevation(t, pool, elevation.ID)
	if ended.EndReason != adminaccess.ElevationEndLogout || ended.ReviewState != adminaccess.ElevationReviewPending {
		t.Fatalf("after logout: %+v", ended)
	}
	if got := actions(elevationEvents(t, pool, elevation.ID.String())); len(got) != 2 || got[1] != "ended" {
		t.Fatalf("events = %v, want started then ended", got)
	}
	fresh := seedLiveSession(t, pool, elevation.UserID, now)
	if got := elevatedSet(t, pool, elevation.UserID, fresh); len(got) != 0 {
		t.Fatalf("a new session of the same person resolves %v", got)
	}
}

// Of two people elevated in two sessions, the logout of one ends only its
// own elevation.
func TestLogoutEndsOnlyItsOwnElevation(t *testing.T) {
	pool := newElevationDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	second := startAt(t, pool, now.Add(-time.Minute), time.Hour)

	if err := sessionStore(t, pool).Revoke(t.Context(), sessionIDOf(t, pool, first.BrowserSessionRef), now); err != nil {
		t.Fatal(err)
	}
	if got := readElevation(t, pool, first.ID); got.EndReason != adminaccess.ElevationEndLogout {
		t.Fatalf("the logged-out elevation: %+v", got)
	}
	if got := readElevation(t, pool, second.ID); !got.EndedAt.IsZero() {
		t.Fatalf("the other person's elevation ended: %+v", got)
	}
	if len(elevatedSet(t, pool, second.UserID, second.BrowserSessionRef)) == 0 {
		t.Fatal("the other person's elevation resolves nothing")
	}
}

// The active-elevations gauge returns to 0 after each of the five ways an
// elevation ends with its session or its person, and each records its reason.
func TestEveryWayToEndReturnsTheGaugeToZero(t *testing.T) {
	pool := newElevationDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	metrics := adminaccess.NewBreakGlassMetrics(pool).WithClock(func() time.Time { return now })
	store := sessionStore(t, pool)
	cases := map[string]func(e adminaccess.ElevationRecord){
		adminaccess.ElevationEndLogout: func(e adminaccess.ElevationRecord) {
			if err := store.Revoke(t.Context(), sessionIDOf(t, pool, e.BrowserSessionRef), now); err != nil {
				t.Fatal(err)
			}
		},
		adminaccess.ElevationEndSessionRevoked: func(e adminaccess.ElevationRecord) {
			if _, err := store.RevokeAllForUser(t.Context(), e.UserID, now); err != nil {
				t.Fatal(err)
			}
		},
		adminaccess.ElevationEndUserSuspended: func(e adminaccess.ElevationRecord) {
			revokePersonAccess(t, pool, e.UserID, adminaccess.ElevationEndUserSuspended, now)
		},
		adminaccess.ElevationEndUserDeleted: func(e adminaccess.ElevationRecord) {
			revokePersonAccess(t, pool, e.UserID, adminaccess.ElevationEndUserDeleted, now)
			if _, err := pool.Exec(t.Context(), `DELETE FROM public.auth_core__user WHERE id = $1`, e.UserID); err != nil {
				t.Fatal(err)
			}
		},
		adminaccess.ElevationEndSCIMDeactivated: func(e adminaccess.ElevationRecord) {
			revokePersonAccess(t, pool, e.UserID, adminaccess.ElevationEndSCIMDeactivated, now)
		},
	}
	for reason, end := range cases {
		t.Run(reason, func(t *testing.T) {
			elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
			if got := gaugeAt(t, metrics); got != 1 {
				t.Fatalf("premise: gauge = %d, want 1", got)
			}
			end(elevation)
			if got := gaugeAt(t, metrics); got != 0 {
				t.Fatalf("gauge after %s = %d, want 0", reason, got)
			}
			if got := readElevation(t, pool, elevation.ID); got.EndReason != reason {
				t.Fatalf("end reason = %q, want %q", got.EndReason, reason)
			}
		})
	}
}

// A suspension, a deletion and a SCIM deactivation revoke the person's
// unused sealed credentials, each with its credential_revoked event, and the
// credential no longer verifies.
func TestPersonAccessEndRevokesTheSealedCredentials(t *testing.T) {
	pool := newElevationDatabase(t)
	for _, reason := range []string{adminaccess.ElevationEndUserSuspended, adminaccess.ElevationEndUserDeleted,
		adminaccess.ElevationEndSCIMDeactivated} {
		t.Run(reason, func(t *testing.T) {
			person := seedPerson(t, pool)
			issued := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person})
			if _, err := verifySealed(t, pool, issued.Credential(), sealedClock.Add(time.Minute)); err != nil {
				t.Fatalf("premise: the credential does not verify: %v", err)
			}
			revokePersonAccess(t, pool, person, reason, sealedClock.Add(time.Minute))

			var revoked bool
			if err := pool.QueryRow(t.Context(), `
SELECT revoked_at IS NOT NULL FROM elitea_auth.sealed_credentials WHERE id = $1`, issued.ID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if !revoked {
				t.Fatalf("the sealed credential of a person who lost access is not revoked")
			}
			if _, err := verifySealed(t, pool, issued.Credential(), sealedClock.Add(2*time.Minute)); err == nil {
				t.Fatal("a revoked sealed credential still verifies")
			}
			found := false
			for _, event := range sealedEvents(t, pool) {
				if event.credentialID == issued.ID.String() &&
					event.action == "admin.elevation."+string(securityaudit.ElevationCredentialRevoked) {
					found = true
				}
			}
			if !found {
				t.Fatal("no credential_revoked event for the revoked credential")
			}
		})
	}
}

// An end whose event cannot be written does not fail the revocation; the
// reconciler records it on its next pass.
func TestAnUnrecordedEndIsLeftToTheReconciler(t *testing.T) {
	pool := newElevationDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)

	failing := securityaudit.FailingAppender{Err: errors.New("the outbox is unavailable")}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.RevokePersonAccess(t.Context(), tx, elevation.UserID,
			adminaccess.PersonAccessEnd{ElevationEnd: adminaccess.ElevationEndUserSuspended, Appender: failing, At: now})
		if err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, elevation.UserID)
		return err
	}); err != nil {
		t.Fatalf("a failing audit append failed the suspension: %v", err)
	}
	if got := readElevation(t, pool, elevation.ID); !got.EndedAt.IsZero() {
		t.Fatalf("premise: the end was recorded with a failing appender: %+v", got)
	}
	// A suspended person resolves nothing at all: the resolver refuses them.
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(t.Context(),
		sessionPrincipal(elevation.UserID, elevation.BrowserSessionRef), auth.PermissionModeAdministration, "")
	if err == nil && resolution.Elevated() {
		t.Fatalf("the elevation of a suspended person resolves %v", resolution.ElevatedPermissions)
	}

	if result := reconcile(t, newReconciler(t, pool, auditAppender(t), now)); result.Orphaned != 1 {
		t.Fatalf("pass = %+v, want one orphaned elevation", result)
	}
	if got := readElevation(t, pool, elevation.ID); got.EndReason != adminaccess.ElevationEndUserSuspended {
		t.Fatalf("after the pass: %+v", got)
	}
	if got := actions(elevationEvents(t, pool, elevation.ID.String())); len(got) != 2 || got[1] != "ended" {
		t.Fatalf("events = %v, want started then ended", got)
	}
}

// A SCIM deactivation (PATCH active:false) ends the person's elevation with
// scim_deactivated and revokes the bound browser session, in its transaction.
func TestASCIMDeactivationEndsTheElevation(t *testing.T) {
	pool := newElevationDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)

	if _, err := scimdirectory.NewStore(pool).SetActive(t.Context(), int(elevation.UserID), false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if got := readElevation(t, pool, elevation.ID); got.EndReason != adminaccess.ElevationEndSCIMDeactivated {
		t.Fatalf("after a SCIM deactivation: %+v", got)
	}
	var revoked bool
	if err := pool.QueryRow(t.Context(), `
SELECT revoked_at IS NOT NULL FROM elitea_auth.browser_sessions WHERE ref = $1`,
		elevation.BrowserSessionRef).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("the elevated browser session survived the SCIM deactivation")
	}
}
