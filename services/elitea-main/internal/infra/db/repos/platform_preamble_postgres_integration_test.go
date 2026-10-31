package repos

// The platform preamble's revisions against a REAL migrated database
// (shared 0216, ADR-0043 decision 3, #1500): a save that changes the text
// writes the next revision and one security audit event in the same
// transaction; a save of the same text writes nothing; the rows are
// append-only. CI-only: skipped without the integration database.

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

func TestPlatformPreamble_EachChangeIsARevisionWithAnAuditEvent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	save := func(text string) PlatformPreambleRevision {
		t.Helper()
		var outcome PlatformPreambleRevision
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			outcome, err = RecordPlatformPreambleRevisionTx(ctx, tx, text, "admin@example.com")
			return err
		}); err != nil {
			t.Fatalf("save %q: %v", text, err)
		}
		return outcome
	}

	if got := save(""); got.Recorded || got.Revision != 0 {
		t.Fatalf("an empty first save = %+v", got)
	}
	if got := save("Answer in English."); !got.Recorded || got.Revision != 1 {
		t.Fatalf("first text = %+v", got)
	}
	if got := save("Answer in English."); got.Recorded || got.Revision != 1 {
		t.Fatalf("same text = %+v", got)
	}
	if got := save(""); !got.Recorded || got.Revision != 2 {
		t.Fatalf("cleared text = %+v", got)
	}

	var (
		events int
		reason string
	)
	if err := pool.QueryRow(ctx, `
SELECT count(*), coalesce(string_agg(o.event::text, ' '), '')
FROM elitea_runtime.platform_preamble_revisions AS p
JOIN elitea_runtime.security_audit_outbox AS o ON o.event_id = p.audit_event_id
WHERE o.event_type = 'ai.elitea.security.v1.config.changed'`).Scan(&events, &reason); err != nil {
		t.Fatalf("read the audit events: %v", err)
	}
	if events != 2 || !strings.Contains(reason, "revision:") {
		t.Fatalf("events = %d, reasons = %s", events, reason)
	}
	// Admission reads the newest revision; a cleared text is none.
	source, err := NewPlatformPreambleRevisions(pool)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err := source.PlatformPreamble(ctx); err != nil || snapshot != nil {
		t.Fatalf("cleared preamble = %v, err = %v", snapshot, err)
	}
	if got := save("Be brief."); !got.Recorded || got.Revision != 3 {
		t.Fatalf("third text = %+v", got)
	}
	snapshot, err := source.PlatformPreamble(ctx)
	if err != nil || snapshot.GetRevision() != 3 || snapshot.GetText() != "Be brief." ||
		!agentexecution.ValidPlatformPreambleSnapshot(snapshot) {
		t.Fatalf("snapshot = %v, err = %v", snapshot, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.platform_preamble_revisions SET text = 'x'`); err == nil {
		t.Fatal("a revision was changed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM elitea_runtime.platform_preamble_revisions`); err == nil {
		t.Fatal("a revision was deleted")
	}
}
