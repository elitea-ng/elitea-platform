package browsersession

// The sealed session kind against the migrated tables (ADR-0059 R2.7a, issue
// 2521): the session and its elevation start together and end together, the
// ordinary cookie never reads it, ending the elevation makes the next request
// unauthenticated, and a failed audit append leaves neither row. Nothing in
// this file stands in an identity provider: the session needs none.
// CI-only: skipped without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type sealedFixture struct {
	pool     *pgxpool.Pool
	manager  *Manager
	dial     *clock
	appender securityaudit.Appender
	userID   int64
}

func newSealedFixture(t *testing.T) sealedFixture {
	t.Helper()
	pool := newBrowserSessionPool(t)
	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	dial := &clock{now: time.Date(2026, time.November, 24, 9, 0, 0, 0, time.UTC)}
	manager, err := NewManager(store, Policy{IdleTimeout: time.Hour, AbsoluteLifetime: 8 * time.Hour}, WithClock(dial.Now))
	if err != nil {
		t.Fatal(err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/sealed-session-test")
	if err != nil {
		t.Fatal(err)
	}
	var userID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ('sealed@example.test', 'Sealed') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed a person: %v", err)
	}
	return sealedFixture{pool: pool, manager: manager, dial: dial, appender: appender, userID: userID}
}

func (f sealedFixture) start(t *testing.T, appender securityaudit.Appender, duration time.Duration) (SealedSession, error) {
	t.Helper()
	var started SealedSession
	err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		started, err = StartSealedSession(context.Background(), tx, appender, SealedStart{
			UserID: f.userID, Level: adminaccess.ElevationLevelAdmin,
			ReasonCode: adminaccess.ElevationReasonIdentityOutage, Reason: "the identity provider is down",
			At: f.dial.Now(), Duration: duration, Source: securityaudit.ElevationSourceConsole,
		})
		// The caller commits whatever happened: the savepoint is what keeps
		// a failed start from leaving a row.
		_ = err
		return nil
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if started.CookieValue == "" {
		return started, errors.New("not started")
	}
	return started, nil
}

func (f sealedFixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, f.userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSealedSessionLivesExactlyAsLongAsItsElevation(t *testing.T) {
	f := newSealedFixture(t)
	ctx := context.Background()
	started, err := f.start(t, f.appender, 90*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !started.Session.ExpiresAt.Equal(started.Elevation.ExpiresAt) ||
		!started.Elevation.ExpiresAt.Equal(f.dial.Now().Add(90*time.Minute)) {
		t.Fatalf("session expires %s, elevation %s; want both at the start plus 90 minutes",
			started.Session.ExpiresAt, started.Elevation.ExpiresAt)
	}
	if started.Elevation.Path != adminaccess.ElevationPathSealed || started.Elevation.BrowserSessionRef != started.Session.Ref {
		t.Fatalf("elevation %+v is not bound to the sealed session %d", started.Elevation, started.Session.Ref)
	}

	session, err := f.manager.ValidateSealed(ctx, started.CookieValue)
	if err != nil {
		t.Fatalf("ValidateSealed: %v", err)
	}
	if session.Provider != ProviderSealed || session.User().CredentialKind != auth.CredentialKindSealed {
		t.Fatalf("session %+v is not sealed", session)
	}
	// The ordinary cookie never reads it.
	if _, err := f.manager.Validate(ctx, started.CookieValue); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Validate of a sealed identifier = %v, want ErrNotFound", err)
	}

	// Within the elevation, past the policy's idle window? No: the session
	// is used every few minutes until a second before the expiry.
	for f.dial.Now().Before(started.Elevation.ExpiresAt.Add(-time.Second)) {
		f.dial.advance(20 * time.Minute)
		if f.dial.Now().After(started.Elevation.ExpiresAt.Add(-time.Second)) {
			break
		}
		if _, err := f.manager.ValidateSealed(ctx, started.CookieValue); err != nil {
			t.Fatalf("at %s, before the expiry: %v", f.dial.Now(), err)
		}
	}
	// At the elevation's expiry, the session ends with it.
	f.dial.now = started.Elevation.ExpiresAt
	if _, err := f.manager.ValidateSealed(ctx, started.CookieValue); err == nil {
		t.Fatal("the sealed session outlived its elevation")
	}
}

func TestEndingTheElevationSignsTheSealedSessionOut(t *testing.T) {
	f := newSealedFixture(t)
	ctx := context.Background()
	started, err := f.start(t, f.appender, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.dial.advance(5 * time.Minute)
	if _, err := f.manager.ValidateSealed(ctx, started.CookieValue); err != nil {
		t.Fatalf("premise: %v", err)
	}
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		_, changed, err := adminaccess.EndElevation(ctx, tx, f.appender, adminaccess.ElevationEnd{
			ID: started.Elevation.ID, Reason: adminaccess.ElevationEndHolder, At: f.dial.Now(),
		})
		if err == nil && !changed {
			err = errors.New("nothing ended")
		}
		return err
	}); err != nil {
		t.Fatalf("end the elevation: %v", err)
	}
	if _, err := f.manager.ValidateSealed(ctx, started.CookieValue); !errors.Is(err, ErrRevoked) {
		t.Fatalf("the next request after the end = %v, want ErrRevoked", err)
	}
	var revokedAt, endedAt *time.Time
	var endReason *string
	if err := f.pool.QueryRow(ctx, `SELECT revoked_at, ended_at, end_reason FROM elitea_auth.browser_sessions WHERE ref = $1`,
		started.Session.Ref).Scan(&revokedAt, &endedAt, &endReason); err != nil {
		t.Fatal(err)
	}
	// All three, as every other revocation writes them, so the lifecycle
	// relay records the session's end.
	if revokedAt == nil || !revokedAt.Equal(f.dial.Now()) || endedAt == nil || !endedAt.Equal(f.dial.Now()) ||
		endReason == nil || *endReason != EndRevoked {
		t.Fatalf("the sealed session row after the end: revoked %v, ended %v, reason %v", revokedAt, endedAt, endReason)
	}
	lifecycle, err := NewLifecycle(f.pool, BoundsFunc(func(context.Context) (Bounds, error) { return Bounds{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.now = f.dial.Now
	if _, err := lifecycle.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var ends int
	if err := f.pool.QueryRow(ctx, `
SELECT count(*) FROM centry.audit_events
WHERE event_type = 'session' AND entity_type = $1 AND entity_id = $2 AND action = $3`,
		AuditEntityType, started.Session.Ref, AuditRevoke).Scan(&ends); err != nil {
		t.Fatal(err)
	}
	if ends != 1 {
		t.Fatalf("the lifecycle relay wrote %d revoke events for the sealed session, want 1", ends)
	}
}

// Even with the row left unrevoked (an end written before this release, or
// by hand), the session reads its elevation on every request.
func TestSealedSessionWithoutAnActiveElevationIsUnauthenticated(t *testing.T) {
	f := newSealedFixture(t)
	ctx := context.Background()
	started, err := f.start(t, f.appender, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
UPDATE elitea_auth.admin_elevations SET ended_at = $2, end_reason = 'holder', review_state = 'review_pending'
WHERE id = $1`, started.Elevation.ID, f.dial.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ValidateSealed(ctx, started.CookieValue); !errors.Is(err, ErrRevoked) {
		t.Fatalf("a sealed session with an ended elevation = %v, want ErrRevoked", err)
	}
}

func TestFailedSealedStartLeavesNoSessionAndNoElevation(t *testing.T) {
	f := newSealedFixture(t)
	for name, tc := range map[string]struct {
		appender securityaudit.Appender
		duration time.Duration
	}{
		"the audit append fails":   {securityaudit.FailingAppender{}, time.Hour},
		"above the sealed ceiling": {f.appender, adminaccess.MaxElevationDurationSealed + time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.start(t, tc.appender, tc.duration); err == nil {
				t.Fatal("started")
			}
			if n := f.count(t, `SELECT count(*) FROM elitea_auth.browser_sessions WHERE user_id = $1`); n != 0 {
				t.Errorf("%d sessions left", n)
			}
			if n := f.count(t, `SELECT count(*) FROM elitea_auth.admin_elevations WHERE user_id = $1`); n != 0 {
				t.Errorf("%d elevations left", n)
			}
		})
	}

	// A suspended person starts nothing either.
	if _, err := f.pool.Exec(context.Background(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.start(t, f.appender, time.Hour); err == nil {
		t.Fatal("a suspended person started a sealed session")
	}
	if n := f.count(t, `SELECT count(*) FROM elitea_auth.browser_sessions WHERE user_id = $1`); n != 0 {
		t.Errorf("%d sessions left for a suspended person", n)
	}
}

// The ordinary writer cannot create the sealed kind, and the sealed cookie
// never reads an ordinary session.
func TestSealedKindIsWrittenOnlyByItsOwnPath(t *testing.T) {
	f := newSealedFixture(t)
	ctx := context.Background()
	if _, err := f.manager.Create(ctx, NewSession{UserID: f.userID, Provider: ProviderSealed}); err == nil {
		t.Fatal("Manager.Create wrote a sealed session")
	}
	ordinary, err := f.manager.Create(ctx, NewSession{UserID: f.userID, Provider: ProviderOIDC})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ValidateSealed(ctx, ordinary); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ValidateSealed of an ordinary session = %v, want ErrNotFound", err)
	}
}
