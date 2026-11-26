package adminaccess_test

// A `critical` action allowed under an elevation is announced in the
// transaction of the change (ADR-0059 decision 5; story R2.8b, issue 2602),
// on the real schema: the ledger, the chained outbox and the notice outbox.
// The change is the endpoint of a global provider, written the way the
// configuration store writes it: its row, then securityaudit.RunChangeHook,
// then the commit.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type failingAppender struct{}

func (failingAppender) Append(context.Context, pgx.Tx, securityaudit.SecurityEvent) error {
	return errors.New("the security audit outbox is unavailable")
}

const globalProviderRoute = "PUT /api/v2/admin/gateway/providers/{configID} route"

// changeEndpoint writes the provider's endpoint as the configuration store
// does, with the announcement armed as the restriction arms it.
func changeEndpoint(t *testing.T, pool *pgxpool.Pool, caller adminaccess.ElevationCaller, endpoint string) error {
	t.Helper()
	ctx := securityaudit.ContextWithTransactionHooks(t.Context())
	if refusal := adminaccess.ArmCriticalChange(ctx, criticalEntry(t, globalProviderRoute), caller); refusal != nil {
		t.Fatalf("the route change was refused: %v", refusal)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE r28b_global_provider SET endpoint = $1 WHERE id = 1`, endpoint); err != nil {
			return err
		}
		return securityaudit.RunChangeHook(ctx, tx, "1")
	})
}

func criticalFixture(t *testing.T) (*pgxpool.Pool, adminaccess.ElevationRecord, int64) {
	t.Helper()
	pool := newNoticeDatabase(t)
	seedRecipient(t, pool, adminaccess.RoleSuperAdmin)
	seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	if _, err := pool.Exec(t.Context(), `
CREATE TABLE r28b_global_provider (id int PRIMARY KEY, endpoint text NOT NULL);
INSERT INTO r28b_global_provider VALUES (1, 'https://old.example/v1')`); err != nil {
		t.Fatal(err)
	}
	start := seededStart(t, pool)
	elevation := startElevation(t, pool, start)
	return pool, elevation, start.UserID
}

func criticalCaller(elevation adminaccess.ElevationRecord, appender securityaudit.Appender) adminaccess.ElevationCaller {
	return adminaccess.ElevationCaller{
		Elevated: true, UserID: elevation.UserID, SessionRef: elevation.BrowserSessionRef,
		ElevationIDs: []string{elevation.ID.String()},
		Announcer: &adminaccess.CriticalAnnouncer{Appender: appender,
			Now: func() time.Time { return elevation.StartedAt.Add(time.Minute) }},
	}
}

func criticalNotices(t *testing.T, pool *pgxpool.Pool, elevation adminaccess.ElevationRecord) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_auth.admin_access_notices
WHERE elevation_id = $1 AND kind = 'critical_change' AND detail ->> 'action' = $2`,
		elevation.ID, globalProviderRoute).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func endpointOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var endpoint string
	if err := pool.QueryRow(t.Context(), `SELECT endpoint FROM r28b_global_provider WHERE id = 1`).Scan(&endpoint); err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func TestACriticalChangeUnderAnElevationIsAnnouncedInItsTransaction(t *testing.T) {
	pool, elevation, _ := criticalFixture(t)
	if err := changeEndpoint(t, pool, criticalCaller(elevation, auditAppender(t)), "https://new.example/v1"); err != nil {
		t.Fatalf("the change: %v", err)
	}
	if got := endpointOf(t, pool); got != "https://new.example/v1" {
		t.Fatalf("endpoint = %q", got)
	}
	critical := 0
	for _, event := range elevationEvents(t, pool, elevation.ID.String()) {
		if event.action == string(securityaudit.ElevationCriticalChange) {
			critical++
		}
	}
	if critical != 1 {
		t.Fatalf("%d critical_change events, want 1", critical)
	}
	// The super_admin, the auditor and the approver of seededStart (admin).
	if n := criticalNotices(t, pool, elevation); n != 3 {
		t.Fatalf("%d critical_change notice rows, want 3", n)
	}
}

func TestACriticalChangeWhoseAnnouncementFailsIsRolledBack(t *testing.T) {
	pool, elevation, _ := criticalFixture(t)
	if err := changeEndpoint(t, pool, criticalCaller(elevation, failingAppender{}), "https://new.example/v1"); err == nil {
		t.Fatal("the change committed with no announcement")
	}
	if got := endpointOf(t, pool); got != "https://old.example/v1" {
		t.Fatalf("the provider change was not rolled back: endpoint = %q", got)
	}
	if n := criticalNotices(t, pool, elevation); n != 0 {
		t.Fatalf("%d notice rows of a rolled-back change", n)
	}
}
