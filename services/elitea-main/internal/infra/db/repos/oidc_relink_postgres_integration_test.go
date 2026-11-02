package repos

// The OIDC relink (issue #1460) against a REAL migrated database: a plan
// writes nothing, an apply changes the link and writes one security audit
// event in the same transaction, and every ambiguous case is refused.
// CI-only: skipped without the integration database.

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRelinkOIDCIdentity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	mustExec := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The auth tables are owned by 001_initial.sql, outside the ledgered
	// corpus; create them in that shape (auth_core_baseline.sql).
	seedAuthCoreTables(t, pool)
	mustExec(`CREATE TABLE IF NOT EXISTS public.auth_core__user_provider (
    user_id integer NOT NULL REFERENCES public.auth_core__user (id) ON UPDATE CASCADE ON DELETE CASCADE,
    provider_ref text NOT NULL,
    PRIMARY KEY (user_id, provider_ref),
    UNIQUE (provider_ref))`)
	mustExec(`INSERT INTO public.auth_core__user (id, email, name) VALUES
		(910001, 'Recreated@Example.com', 'Recreated'),
		(910002, 'other@example.com', 'Other'),
		(910003, 'two-links@example.com', 'Two Links'),
		(910004, 'operator@example.com', 'Operator'),
		(910005, 'twin@example.com', 'Twin A'),
		(910006, 'TWIN@example.com', 'Twin B')`)
	mustExec(`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES
		(910001, 'oidc:old-sub'),
		(910002, 'oidc:taken-sub'),
		(910003, 'oidc:first'),
		(910003, 'oidc:second')`)

	relink := func(req OIDCRelinkRequest) (OIDCRelinkPlan, error) {
		t.Helper()
		var plan OIDCRelinkPlan
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			plan, err = RelinkOIDCIdentityTx(ctx, tx, req)
			return err
		})
		return plan, err
	}
	links := func(userID int) string {
		t.Helper()
		var refs string
		if err := pool.QueryRow(ctx, `
SELECT coalesce(string_agg(provider_ref, ',' ORDER BY provider_ref), '')
FROM public.auth_core__user_provider WHERE user_id = $1`, userID).Scan(&refs); err != nil {
			t.Fatal(err)
		}
		return refs
	}
	events := func(action string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = 'ai.elitea.security.v1.identity.link.changed' AND event->'data'->>'action' = $1`,
			action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a plan writes nothing", func(t *testing.T) {
		plan, err := relink(OIDCRelinkRequest{User: "recreated@example.com", NewSubject: "new-sub"})
		if err != nil || plan.UserID != 910001 || plan.Remove != "old-sub" || plan.Add != "new-sub" || plan.Applied {
			t.Fatalf("plan = %+v, err = %v", plan, err)
		}
		if got := links(910001); got != "oidc:old-sub" {
			t.Fatalf("links = %q after a plan", got)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for name, tc := range map[string]struct {
			req  OIDCRelinkRequest
			want error
		}{
			"unknown user":          {OIDCRelinkRequest{User: "nobody@example.com", Clear: true}, ErrRelinkUserNotFound},
			"email names two":       {OIDCRelinkRequest{User: "twin@example.com", Clear: true}, ErrRelinkAmbiguousUser},
			"two links, none named": {OIDCRelinkRequest{User: "910003", NewSubject: "third"}, ErrRelinkAmbiguousLink},
			"named link absent":     {OIDCRelinkRequest{User: "910003", NewSubject: "third", OldSubject: "zero"}, ErrRelinkOldLinkNotFound},
			"subject held elsewhere": {OIDCRelinkRequest{User: "910001", NewSubject: "taken-sub", Apply: true, Operator: "910004"},
				ErrRelinkSubjectTaken},
			"both operations":  {OIDCRelinkRequest{User: "910001", NewSubject: "x", Clear: true}, ErrRelinkInvalidOperation},
			"neither":          {OIDCRelinkRequest{User: "910001"}, ErrRelinkInvalidOperation},
			"control in a sub": {OIDCRelinkRequest{User: "910001", NewSubject: "a\nb"}, ErrRelinkInvalidSubject},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := relink(tc.req); !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
			})
		}
		if got := links(910001); got != "oidc:old-sub" {
			t.Fatalf("links = %q after refusals", got)
		}
	})

	t.Run("apply replaces the subject and audits it", func(t *testing.T) {
		plan, err := relink(OIDCRelinkRequest{User: "910001", NewSubject: "new-sub", Apply: true, Operator: "910004"})
		if err != nil || !plan.Applied || plan.AuditEventID == "" {
			t.Fatalf("plan = %+v, err = %v", plan, err)
		}
		if got := links(910001); got != "oidc:new-sub" {
			t.Fatalf("links = %q", got)
		}
		if n := events(ActionOIDCLinkReplaced); n != 1 {
			t.Fatalf("replaced events = %d", n)
		}
		var event string
		if err := pool.QueryRow(ctx, `SELECT event::text FROM elitea_runtime.security_audit_outbox WHERE event_id = $1::uuid`,
			plan.AuditEventID).Scan(&event); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(event, "new-sub") || strings.Contains(event, "old-sub") {
			t.Fatalf("the audit event holds a raw subject: %s", event)
		}
		again, err := relink(OIDCRelinkRequest{User: "910001", NewSubject: "new-sub", Apply: true, Operator: "910004"})
		if err != nil || !again.NoChange || again.Applied {
			t.Fatalf("repeat = %+v, err = %v", again, err)
		}
	})

	t.Run("clear the named link of two", func(t *testing.T) {
		plan, err := relink(OIDCRelinkRequest{User: "two-links@example.com", Clear: true, OldSubject: "second",
			Apply: true, Operator: "910004"})
		if err != nil || !plan.Applied || plan.Remove != "second" {
			t.Fatalf("plan = %+v, err = %v", plan, err)
		}
		if got := links(910003); got != "oidc:first" {
			t.Fatalf("links = %q", got)
		}
		if n := events(ActionOIDCLinkCleared); n != 1 {
			t.Fatalf("cleared events = %d", n)
		}
	})

	t.Run("an apply needs a numeric operator", func(t *testing.T) {
		if _, err := relink(OIDCRelinkRequest{User: "910002", Clear: true, Apply: true, Operator: "admin"}); err == nil {
			t.Fatal("an apply without a numeric operator was accepted")
		}
		if got := links(910002); got != "oidc:taken-sub" {
			t.Fatalf("links = %q", got)
		}
	})
}
