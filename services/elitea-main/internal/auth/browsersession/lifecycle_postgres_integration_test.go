package browsersession

// The session lifecycle audit (shared 0200) against the migrated table: the
// sweep stamps sessions past a deadline under the administrator's bounds,
// and the relay writes exactly one start and one end event per session into
// centry.audit_events, keyed by the non-secret reference, never the cookie.

import (
	"context"
	"testing"
	"time"
)

func TestPostgresLifecycleAuditsStartAndEveryEnd(t *testing.T) {
	pool := newBrowserSessionPool(t)
	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	dial := &clock{now: time.Date(2026, time.October, 29, 9, 0, 0, 0, time.UTC)}
	bounds := Bounds{}
	manager, err := NewManager(store, Policy{IdleTimeout: 0, AbsoluteLifetime: 7 * 24 * time.Hour},
		WithClock(dial.Now), WithBounds(BoundsFunc(func(context.Context) (Bounds, error) { return bounds, nil })))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	idle, err := manager.Create(ctx, NewSession{UserID: 1, Email: "idle@example.test", Provider: ProviderOIDC})
	if err != nil {
		t.Fatal(err)
	}
	logout, err := manager.Create(ctx, NewSession{UserID: 2, Provider: ProviderSAML})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, NewSession{UserID: 3, Provider: ProviderForm}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RevokeUser(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(ctx, logout); err != nil {
		t.Fatal(err)
	}

	// The administrator sets a 30-minute idle maximum; the idle session was
	// issued with none. Two hours later the sweep ends it at the deadline.
	bounds = Bounds{MaxIdleTimeout: 30 * time.Minute}
	lifecycle, err := NewLifecycle(pool, BoundsFunc(func(context.Context) (Bounds, error) { return bounds, nil }))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.now = func() time.Time { return dial.Now().Add(2 * time.Hour) }
	result, err := lifecycle.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if result.Expired != 1 || result.Events != 6 {
		t.Fatalf("pass = %+v, want 1 expired and 6 events (3 starts, 3 ends)", result)
	}
	if again, err := lifecycle.Pass(ctx); err != nil || again != (LifecycleResult{}) {
		t.Fatalf("second pass = %+v, %v; want nothing (exactly once)", again, err)
	}

	idleID, _ := ParseCookieValue(idle)
	var ref int64
	var reason string
	var endedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT ref, end_reason, ended_at FROM elitea_auth.browser_sessions WHERE id = $1`,
		idleID).Scan(&ref, &reason, &endedAt); err != nil {
		t.Fatal(err)
	}
	if reason != EndIdle || !endedAt.Equal(dial.Now().Add(30*time.Minute)) {
		t.Fatalf("idle session ended %q at %s", reason, endedAt)
	}

	rows, err := pool.Query(ctx, `
SELECT action, entity_type, entity_id, entity_name, COALESCE(user_email, '')
FROM centry.audit_events WHERE event_type = 'session' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	var idleEnd string
	for rows.Next() {
		var action, entityType, entityName, email string
		var entityID int64
		if err := rows.Scan(&action, &entityType, &entityID, &entityName, &email); err != nil {
			t.Fatal(err)
		}
		if entityType != AuditEntityType || entityID <= 0 {
			t.Fatalf("event %s names %s %d", action, entityType, entityID)
		}
		for _, secret := range []string{idle, logout, idleID} {
			if entityName == secret {
				t.Fatal("an audit event carries the session credential")
			}
		}
		counts[action]++
		if action == AuditExpire && entityID == ref {
			idleEnd = entityName
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{AuditStart: 3, AuditExpire: 1, AuditLogout: 1, AuditRevoke: 1}
	for action, n := range want {
		if counts[action] != n {
			t.Errorf("%s events = %d, want %d (all: %v)", action, counts[action], n, counts)
		}
	}
	if idleEnd != "provider=oidc reason=idle" {
		t.Fatalf("idle expiry entity_name = %q", idleEnd)
	}
}
