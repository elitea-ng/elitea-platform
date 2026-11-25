package adminaccess_test

// The sealed credential store on the real schema (ADR-0059 decision 5, "Path
// 2"; story R2.6a, issue 2520): 001_initial.sql and the shared corpus, so the
// table, its constraints and its trigger are the ones shared 0271 creates.
// CI-only: skipped without ELITEA_TEST_DATABASE_URL.

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

var sealedClock = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func issueSealed(t *testing.T, pool *pgxpool.Pool, issue adminaccess.SealedIssue) adminaccess.IssuedSealedCredential {
	t.Helper()
	if issue.At.IsZero() {
		issue.At = sealedClock
	}
	var issued adminaccess.IssuedSealedCredential
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		issued, err = adminaccess.IssueSealedCredential(t.Context(), tx, auditAppender(t), issue)
		return err
	}); err != nil {
		t.Fatalf("issue a sealed credential: %v", err)
	}
	return issued
}

func verifySealed(t *testing.T, pool *pgxpool.Pool, presented string, at time.Time) (adminaccess.SealedCredential, error) {
	t.Helper()
	return adminaccess.VerifySealedCredential(t.Context(), pool, presented, at)
}

// sealedEvent is one security audit event about a sealed credential.
type sealedEvent struct {
	eventType, action, source, credentialID, level, raw string
}

func sealedEvents(t *testing.T, pool *pgxpool.Pool) []sealedEvent {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event_type, event -> 'data' ->> 'action',
       COALESCE(event -> 'data' -> 'elevation' ->> 'source', ''),
       COALESCE(event -> 'data' -> 'elevation' ->> 'credential_id', ''),
       COALESCE(event -> 'data' -> 'elevation' ->> 'level', ''),
       event::text
FROM elitea_runtime.security_audit_outbox
WHERE event -> 'data' ->> 'action' LIKE 'admin.elevation.credential%'
ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sealedEvent, error) {
		var e sealedEvent
		err := row.Scan(&e.eventType, &e.action, &e.source, &e.credentialID, &e.level, &e.raw)
		return e, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// The verifier is in the value IssueSealedCredential returns and nowhere in
// the database: no column of any table of any schema holds it, nor either
// half of it. The row holds an Argon2id digest, which verifies.
func TestSealedCredentialSecretIsNowhereInTheDatabase(t *testing.T) {
	pool := newElevationDatabase(t)
	person := seedPerson(t, pool)
	issued := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person, Label: "envelope A", Split: true})
	first, second := issued.Halves()
	if first+second != issued.Verifier || len(issued.Verifier) != 64 {
		t.Fatalf("the halves do not make the verifier: %q + %q != %q", first, second, issued.Verifier)
	}

	for _, needle := range []string{issued.Verifier, first, second, issued.Credential()} {
		if hits := grepDatabase(t, pool, needle); len(hits) > 0 {
			t.Fatalf("a secret part of the credential is stored in %v", hits)
		}
	}
	var digest string
	if err := pool.QueryRow(t.Context(), `SELECT verifier_digest FROM elitea_auth.sealed_credentials WHERE id = $1`,
		issued.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("the stored digest is not an Argon2id PHC string: %q", digest)
	}
	if _, err := verifySealed(t, pool, issued.Credential(), sealedClock.Add(time.Minute)); err != nil {
		t.Fatalf("the issued credential does not verify: %v", err)
	}
	if !issued.Split {
		t.Fatal("the credential is not marked split")
	}
}

// grepDatabase answers every table of a user schema whose rows, as JSON, hold
// needle.
func grepDatabase(t *testing.T, pool *pgxpool.Pool, needle string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT format('%I.%I', table_schema, table_name)
FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 50 {
		t.Fatalf("premise: the corpus has %d tables", len(tables))
	}
	var hits []string
	for _, table := range tables {
		var found bool
		if err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM `+table+` AS t WHERE strpos(lower(to_jsonb(t)::text), lower($1)) > 0)`,
			needle).Scan(&found); err != nil {
			t.Fatalf("grep %s: %v", table, err)
		}
		if found {
			hits = append(hits, table)
		}
	}
	return hits
}

// A used, a revoked and an expired credential cannot be used again: the
// verify function refuses each, with the verifier right. A wrong verifier and
// an unknown selector are refused too, all with the one error.
func TestSealedVerifyRefusesUsedRevokedAndExpiredCredentials(t *testing.T) {
	pool := newElevationDatabase(t)
	person := seedPerson(t, pool)
	used := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person})
	revoked := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person})
	expiring := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person, Lifetime: time.Hour})
	active := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person})
	now := sealedClock.Add(2 * time.Hour)

	// Every one verifies before anything happens to it (the expiring one
	// within its hour).
	for _, c := range []adminaccess.IssuedSealedCredential{used, revoked, active} {
		if _, err := verifySealed(t, pool, c.Credential(), now); err != nil {
			t.Fatalf("premise: %s does not verify: %v", c.ID, err)
		}
	}
	if _, err := verifySealed(t, pool, expiring.Credential(), sealedClock.Add(30*time.Minute)); err != nil {
		t.Fatalf("premise: the expiring credential does not verify within its lifetime: %v", err)
	}

	// The consume of R2.7b is one UPDATE of used_at; this stands in for it.
	if _, err := pool.Exec(t.Context(), `UPDATE elitea_auth.sealed_credentials SET used_at = $2 WHERE id = $1`,
		used.ID, sealedClock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, changed, err := adminaccess.RevokeSealedCredential(t.Context(), tx, auditAppender(t),
			adminaccess.SealedRevoke{ID: revoked.ID, At: sealedClock.Add(time.Hour)})
		if err == nil && !changed {
			err = errors.New("the revoke changed nothing")
		}
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	wrong := adminaccess.FormatSealedCredential(active.Selector, strings.Repeat("0", 64))
	unknown := adminaccess.FormatSealedCredential(strings.Repeat("a", 24), active.Verifier)
	for name, tc := range map[string]struct {
		presented string
		cause     adminaccess.SealedRefusal
	}{
		"used":             {used.Credential(), adminaccess.SealedRefusalUsed},
		"revoked":          {revoked.Credential(), adminaccess.SealedRefusalRevoked},
		"expired":          {expiring.Credential(), adminaccess.SealedRefusalExpired},
		"a wrong verifier": {wrong, adminaccess.SealedRefusalMismatch},
		"unknown selector": {unknown, adminaccess.SealedRefusalUnknown},
		"malformed":        {"bg_" + active.Selector, adminaccess.SealedRefusalMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verifySealed(t, pool, tc.presented, now)
			if !errors.Is(err, adminaccess.ErrSealedCredentialRefused) || adminaccess.SealedRefusalOf(err) != tc.cause {
				t.Fatalf("verify = %v (cause %q), want a refusal for %q", err, adminaccess.SealedRefusalOf(err), tc.cause)
			}
		})
	}
	// The active one still verifies, in capitals too (typed from paper).
	if got, err := verifySealed(t, pool, strings.ToUpper(active.Credential()), now); err != nil || got.ID != active.ID {
		t.Fatalf("the active credential: %+v, %v", got, err)
	}
}

// Issue, list and revoke each write one security audit event, with the
// source operator; the issue and the revoke carry the credential and its
// level, the list is a decision event that counts the rows. No event holds
// the verifier.
func TestSealedIssueListAndRevokeEachWriteAnEvent(t *testing.T) {
	pool := newElevationDatabase(t)
	person := seedPerson(t, pool)
	issued := issueSealed(t, pool, adminaccess.SealedIssue{UserID: person, Level: adminaccess.ElevationLevelSuperAdmin,
		Source: securityaudit.ElevationSourceOperator})

	var listed []adminaccess.SealedCredential
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		listed, err = adminaccess.ListSealedCredentials(t.Context(), tx, auditAppender(t), adminaccess.SealedList{})
		return err
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != issued.ID || listed[0].Level != adminaccess.ElevationLevelSuperAdmin ||
		listed[0].State(sealedClock) != adminaccess.SealedStateActive {
		t.Fatalf("list = %+v", listed)
	}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, _, err := adminaccess.RevokeSealedCredential(t.Context(), tx, auditAppender(t),
			adminaccess.SealedRevoke{ID: issued.ID, At: sealedClock.Add(time.Minute)})
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	events := sealedEvents(t, pool)
	var got []string
	for _, e := range events {
		got = append(got, e.action)
		if strings.Contains(e.raw, issued.Verifier) || strings.Contains(e.raw, issued.Selector) {
			t.Fatalf("an event holds the selector or the verifier: %s", e.raw)
		}
	}
	want := []string{"admin.elevation.credential_issued", adminaccess.SealedCredentialListAction, "admin.elevation.credential_revoked"}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for _, i := range []int{0, 2} {
		e := events[i]
		if e.eventType != string(securityaudit.TypeAdminElevation) || e.source != securityaudit.ElevationSourceOperator ||
			e.credentialID != issued.ID.String() || e.level != adminaccess.ElevationLevelSuperAdmin {
			t.Errorf("%s: %+v", e.action, e)
		}
	}
	if list := events[1]; list.eventType != string(securityaudit.TypeDecision) ||
		!strings.Contains(list.raw, `"source:operator"`) || !strings.Contains(list.raw, `"credentials:1"`) {
		t.Errorf("the list event: %s", list.raw)
	}
}

// Rows are kept: DELETE, TRUNCATE and an UPDATE of a column fixed at issue
// are refused, and the use and the revocation are written once.
func TestSealedCredentialRowsAreNeverDeleted(t *testing.T) {
	pool := newElevationDatabase(t)
	issued := issueSealed(t, pool, adminaccess.SealedIssue{UserID: seedPerson(t, pool)})
	ctx := t.Context()
	for name, statement := range map[string]string{
		"DELETE":           `DELETE FROM elitea_auth.sealed_credentials WHERE id = $1`,
		"the label":        `UPDATE elitea_auth.sealed_credentials SET label = 'x' WHERE id = $1`,
		"the level":        `UPDATE elitea_auth.sealed_credentials SET level = 'super_admin' WHERE id = $1`,
		"the digest":       `UPDATE elitea_auth.sealed_credentials SET verifier_digest = verifier_digest || 'A' WHERE id = $1`,
		"the expiry":       `UPDATE elitea_auth.sealed_credentials SET expires_at = expires_at - interval '1 day' WHERE id = $1`,
		"the bound person": `UPDATE elitea_auth.sealed_credentials SET user_id = user_id + 1 WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, statement, issued.ID); err == nil || !strings.Contains(err.Error(), "keeps its history") {
			t.Errorf("%s was not refused by the trigger: %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE elitea_auth.sealed_credentials`); err == nil {
		t.Error("TRUNCATE was not refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.sealed_credentials SET revoked_at = $2 WHERE id = $1`,
		issued.ID, sealedClock.Add(time.Minute)); err != nil {
		t.Fatalf("the revocation was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.sealed_credentials SET revoked_at = $2 WHERE id = $1`,
		issued.ID, sealedClock.Add(2*time.Minute)); err == nil {
		t.Error("a second revocation time was accepted")
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_auth.sealed_credentials (selector, verifier_digest, level, user_id, issued_at, expires_at)
VALUES ($1, '$argon2id$v=19$m=1,t=1,p=1$AAAA$AAAA', 'admin', 1, $2, $2::timestamptz + interval '366 days')`,
		strings.Repeat("b", 24), sealedClock); err == nil {
		t.Error("a lifetime of 366 days was accepted by the table")
	}
}

// A refused issue writes nothing: a person who is suspended or unknown, and
// an append that fails (the row and its event are one savepoint).
func TestSealedIssueWritesNothingWhenRefused(t *testing.T) {
	pool := newElevationDatabase(t)
	suspended := seedPerson(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	active := seedPerson(t, pool)
	for name, tc := range map[string]struct {
		appender securityaudit.Appender
		user     int64
		want     error
	}{
		"a suspended person":   {auditAppender(t), suspended, adminaccess.ErrSealedCredentialPerson},
		"an unknown person":    {auditAppender(t), 987654321, adminaccess.ErrSealedCredentialPerson},
		"the append fails":     {securityaudit.FailingAppender{}, active, securityaudit.ErrAppendFailed},
		"a lifetime too long":  {auditAppender(t), active, adminaccess.ErrSealedCredentialInvalid},
		"an unknown level":     {auditAppender(t), active, adminaccess.ErrSealedCredentialInvalid},
		"a control in a label": {auditAppender(t), active, adminaccess.ErrSealedCredentialInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			issue := adminaccess.SealedIssue{UserID: tc.user, At: sealedClock}
			switch name {
			case "a lifetime too long":
				issue.Lifetime = adminaccess.MaxSealedCredentialLifetime + time.Second
			case "an unknown level":
				issue.Level = "owner"
			case "a control in a label":
				issue.Label = "a\nb"
			}
			// The caller commits after the error: the savepoint still drops
			// the row.
			if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
				_, err := adminaccess.IssueSealedCredential(t.Context(), tx, tc.appender, issue)
				if !errors.Is(err, tc.want) {
					t.Errorf("issue = %v, want %v", err, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_auth.sealed_credentials`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d credentials were written by refused issues", rows)
	}
	if events := sealedEvents(t, pool); len(events) != 0 {
		t.Fatalf("%d events were written by refused issues", len(events))
	}
}

// Revoking an unknown credential is ErrSealedCredentialNotFound, and a
// second revoke changes nothing and writes no event.
func TestSealedRevokeIsOnce(t *testing.T) {
	pool := newElevationDatabase(t)
	issued := issueSealed(t, pool, adminaccess.SealedIssue{UserID: seedPerson(t, pool)})
	revoke := func(id uuid.UUID) (bool, error) {
		var changed bool
		err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			var err error
			_, changed, err = adminaccess.RevokeSealedCredential(t.Context(), tx, auditAppender(t),
				adminaccess.SealedRevoke{ID: id, At: sealedClock.Add(time.Minute)})
			return err
		})
		return changed, err
	}
	if _, err := revoke(uuid.New()); !errors.Is(err, adminaccess.ErrSealedCredentialNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	if changed, err := revoke(issued.ID); err != nil || !changed {
		t.Fatalf("the first revoke: %v, %v", changed, err)
	}
	if changed, err := revoke(issued.ID); err != nil || changed {
		t.Fatalf("the second revoke: %v, %v", changed, err)
	}
	if n := len(sealedEvents(t, pool)); n != 2 {
		t.Fatalf("%d credential events, want issued and one revoked", n)
	}
}

// An unbound issue that fails creates no reserved account (the account is in
// the issue's savepoint); one for a reserved account that holds a role, or is
// suspended, is ErrReservedAccountUnusable and writes nothing; a bound issue
// naming the account is ErrSealedCredentialReserved. Story R2.6b, issue 2599.
func TestUnboundIssueRefusalsWriteNothing(t *testing.T) {
	pool := newElevationDatabase(t)
	unbound := adminaccess.SealedIssue{Unbound: true, Ticket: "INC-1", Label: "envelope A", At: sealedClock}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.IssueSealedCredential(t.Context(), tx, securityaudit.FailingAppender{}, unbound)
		if !errors.Is(err, securityaudit.ErrAppendFailed) {
			t.Errorf("issue with a failing append = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var accounts int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.auth_core__user WHERE lower(email) = $1`,
		adminaccess.ReservedBreakGlassEmail).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("a failed unbound issue left %d reserved accounts (%v)", accounts, err)
	}

	issued := issueSealed(t, pool, unbound)
	reserved := issued.UserID
	if !issued.ReservedAccountCreated {
		t.Fatal("the first unbound issue did not report the account it created")
	}
	for name, setup := range map[string]string{
		"holds a role": `INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = 'admin' AND mode = 'administration'`,
		"is suspended": `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), setup, reserved); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = pool.Exec(t.Context(), `DELETE FROM public.auth_core__user_role WHERE user_id = $1`, reserved)
				_, _ = pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = false WHERE id = $1`, reserved)
			}()
			if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
				_, err := adminaccess.IssueSealedCredential(t.Context(), tx, auditAppender(t), unbound)
				if !errors.Is(err, adminaccess.ErrReservedAccountUnusable) {
					t.Errorf("issue = %v, want ErrReservedAccountUnusable", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.IssueSealedCredential(t.Context(), tx, auditAppender(t),
			adminaccess.SealedIssue{UserID: reserved, At: sealedClock})
		if !errors.Is(err, adminaccess.ErrSealedCredentialReserved) {
			t.Errorf("a bound issue for the reserved account = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_auth.sealed_credentials`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d credentials (%v), want only the first", rows, err)
	}
}
