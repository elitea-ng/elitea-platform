package adminaccess_test

// Elevation events are security audit events, written in the same
// transaction (ADR-0059 decisions 5 and 8; story R2.3, issue 2517), on the
// real schema: the ledger of shared 0269 and the chained outbox of shared
// 0183.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// reasonMarker and noteMarker are texts no event may carry.
const (
	reasonMarker = "REASON-MARKER-7f3a the provider console is unreachable"
	noteMarker   = "NOTE-MARKER-91c2 rotated the identity provider secret"
)

// elevationEventRow is one admin.elevation row of the outbox.
type elevationEventRow struct {
	action       string
	partitionKey string
	partitionSeq int64
	raw          string
}

func elevationEvents(t *testing.T, pool *pgxpool.Pool, elevationID string) []elevationEventRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event -> 'data' -> 'elevation' ->> 'action', partition_key, partition_seq, event::text
FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1
  AND ($2 = '' OR event -> 'data' -> 'elevation' ->> 'elevation_id' = $2)
ORDER BY seq`, string(securityaudit.TypeAdminElevation), elevationID)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var out []elevationEventRow
	for rows.Next() {
		var row elevationEventRow
		if err := rows.Scan(&row.action, &row.partitionKey, &row.partitionSeq, &row.raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func countElevationsOf(t *testing.T, pool *pgxpool.Pool, userID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM elitea_auth.admin_elevations WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A start whose event cannot be written leaves no row, even when the caller
// commits after the error; an end whose event cannot be written leaves the
// elevation open. Fail closed on audit.
func TestAnElevationStepWithoutItsEventDoesNotHappen(t *testing.T) {
	pool := newElevationDatabase(t)
	start := seededStart(t, pool)
	start.Reason = reasonMarker

	var startErr error
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, startErr = adminaccess.StartElevation(t.Context(), tx, securityaudit.FailingAppender{}, start)
		return nil // the caller commits regardless: the savepoint still drops the row
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if startErr == nil {
		t.Fatal("StartElevation succeeded with a failing appender")
	}
	if n := countElevationsOf(t, pool, start.UserID); n != 0 {
		t.Fatalf("%d elevation rows after a failed append, want 0", n)
	}
	if events := elevationEvents(t, pool, ""); len(events) != 0 {
		t.Fatalf("%d events after a failed append", len(events))
	}

	// The same start with a working appender: one row, one started event.
	elevation := startElevation(t, pool, start)
	var endErr error
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, _, endErr = adminaccess.EndElevation(t.Context(), tx, securityaudit.FailingAppender{}, adminaccess.ElevationEnd{
			ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: start.At.Add(time.Minute),
		})
		return nil
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if endErr == nil {
		t.Fatal("EndElevation succeeded with a failing appender")
	}
	if current := readElevation(t, pool, elevation.ID); !current.EndedAt.IsZero() || current.EndReason != "" {
		t.Fatalf("the elevation ended without its event: %+v", current)
	}
	if events := elevationEvents(t, pool, elevation.ID.String()); len(events) != 1 || events[0].action != "started" {
		t.Fatalf("events = %+v, want the started event only", events)
	}
}

// Start and end each write exactly one chained event with the step's facts
// and never the reason text; a second end writes nothing; an end past the
// expiry is the expired action.
func TestStartAndEndWriteOneEventEach(t *testing.T) {
	pool := newElevationDatabase(t)

	start := seededStart(t, pool)
	start.Reason = reasonMarker
	start.CorrelationID = "trace-start-1"
	elevation := startElevation(t, pool, start)
	if _, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: start.At.Add(10 * time.Minute),
		ActorUserID: start.UserID,
	}); !changed {
		t.Fatal("the end changed nothing")
	}
	if _, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: start.At.Add(11 * time.Minute),
	}); changed {
		t.Fatal("a second end changed the row")
	}
	events := elevationEvents(t, pool, elevation.ID.String())
	if len(events) != 2 || events[0].action != "started" || events[1].action != "ended" {
		t.Fatalf("events = %+v, want started then ended", events)
	}
	for _, event := range events {
		if strings.Contains(event.raw, "REASON-MARKER") {
			t.Fatalf("the reason text reached the chain: %s", event.raw)
		}
		for _, want := range []string{`"reason_code": "incident"`, `"level": "admin"`, `"path": "approved"`} {
			if !strings.Contains(event.raw, want) && !strings.Contains(event.raw, strings.ReplaceAll(want, " ", "")) {
				t.Fatalf("event lacks %s: %s", want, event.raw)
			}
		}
	}
	if !strings.Contains(events[0].raw, "trace-start-1") || !strings.Contains(events[0].raw, "browser_session:") {
		t.Fatalf("the started event lacks its correlation or session ref: %s", events[0].raw)
	}
	if !strings.Contains(events[1].raw, `"end_reason": "holder"`) {
		t.Fatalf("the ended event lacks its end reason: %s", events[1].raw)
	}
	if events[1].partitionKey != events[0].partitionKey || events[1].partitionSeq != events[0].partitionSeq+1 {
		t.Fatalf("the events are not consecutive links of one chain: %+v", events)
	}

	// The holder's end names the holder as the actor, though none was given.
	if !strings.Contains(events[1].raw, fmt.Sprintf(`"principal_ref": "user:%d"`, start.UserID)) {
		t.Fatalf("the ended event lacks the holder as its actor: %s", events[1].raw)
	}

	// Past the expiry: the stored reason is expired, and so is the action;
	// the reason the caller asked for is a reason code of the event, and the
	// actor and the source are the holder who asked, from the console.
	late := seededStart(t, pool)
	lateElevation := startElevation(t, pool, late)
	ended, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: lateElevation.ID, Reason: adminaccess.ElevationEndHolder, At: late.At.Add(late.Duration + time.Minute),
	})
	if !changed || ended.EndReason != adminaccess.ElevationEndExpired {
		t.Fatalf("late end = %+v, %v", ended, changed)
	}
	lateEvents := elevationEvents(t, pool, lateElevation.ID.String())
	if len(lateEvents) != 2 || lateEvents[1].action != "expired" ||
		!strings.Contains(lateEvents[1].raw, "requested_end_reason:holder") ||
		!strings.Contains(lateEvents[1].raw, `"source": "console"`) ||
		!strings.Contains(lateEvents[1].raw, fmt.Sprintf(`"principal_ref": "user:%d"`, late.UserID)) {
		t.Fatalf("late events = %+v", lateEvents)
	}

	// The platform's own expiry has no actor and the system source.
	lapsed := seededStart(t, pool)
	lapsedElevation := startElevation(t, pool, lapsed)
	endElevation(t, pool, adminaccess.ElevationEnd{
		ID: lapsedElevation.ID, Reason: adminaccess.ElevationEndExpired, At: lapsed.At.Add(lapsed.Duration),
	})
	lapsedEvents := elevationEvents(t, pool, lapsedElevation.ID.String())
	if len(lapsedEvents) != 2 || lapsedEvents[1].action != "expired" ||
		!strings.Contains(lapsedEvents[1].raw, `"source": "system"`) || strings.Contains(lapsedEvents[1].raw, "principal_ref") {
		t.Fatalf("lapsed events = %+v", lapsedEvents)
	}

	// An approver's end must name the approver.
	open := startElevation(t, pool, seededStart(t, pool))
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, _, err := adminaccess.EndElevation(t.Context(), tx, auditAppender(t), adminaccess.ElevationEnd{
			ID: open.ID, Reason: adminaccess.ElevationEndApprover, At: start.At.Add(time.Minute),
		})
		return err
	}); !errors.Is(err, adminaccess.ErrElevationInvalid) {
		t.Fatalf("an approver end with no approver: %v", err)
	}
}

// Every action of the list is one chained admin.elevation event through
// AppendElevationEvent on the caller's transaction, and a note is its id and
// length, never its text.
func TestEveryActionIsOneChainedEvent(t *testing.T) {
	pool := newElevationDatabase(t)
	start := seededStart(t, pool)
	elevation := startElevation(t, pool, start)
	before := len(elevationEvents(t, pool, ""))

	for _, action := range securityaudit.ElevationActions() {
		ev := adminaccess.ElevationEvent{
			Action: action, Source: securityaudit.ElevationSourceConsole, Elevation: elevation, ActorUserID: start.ApproverUserID,
		}
		switch action {
		case securityaudit.ElevationEnded:
			ev.Elevation.EndReason = adminaccess.ElevationEndHolder
		case securityaudit.ElevationExpired:
			ev.Elevation.EndReason, ev.Source = adminaccess.ElevationEndExpired, securityaudit.ElevationSourceSystem
		case securityaudit.ElevationNoteAdded:
			ev.NoteID, ev.NoteText = "note-1", noteMarker
		case securityaudit.ElevationCredentialIssued, securityaudit.ElevationCredentialRevoked:
			ev = adminaccess.ElevationEvent{Action: action, Source: securityaudit.ElevationSourceOperator, CredentialID: "cred-1"}
		case securityaudit.ElevationCredentialUsed:
			ev.CredentialID = "cred-1"
		case securityaudit.ElevationLocalAdminSignin:
			ev = adminaccess.ElevationEvent{Action: action, Source: securityaudit.ElevationSourceConsole, UserID: start.UserID}
		}
		if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			return adminaccess.AppendElevationEvent(t.Context(), tx, auditAppender(t), ev)
		}); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	events := elevationEvents(t, pool, "")[before:]
	if len(events) != len(securityaudit.ElevationActions()) {
		t.Fatalf("%d events, want one per action (%d)", len(events), len(securityaudit.ElevationActions()))
	}
	for i, action := range securityaudit.ElevationActions() {
		if events[i].action != string(action) {
			t.Fatalf("event %d is %q, want %q", i, events[i].action, action)
		}
		if i > 0 && (events[i].partitionKey != events[i-1].partitionKey || events[i].partitionSeq != events[i-1].partitionSeq+1) {
			t.Fatalf("event %d is not the next link of the chain", i)
		}
		if strings.Contains(events[i].raw, "NOTE-MARKER") {
			t.Fatalf("the note text reached the chain: %s", events[i].raw)
		}
	}
	note := events[indexOf(securityaudit.ElevationActions(), securityaudit.ElevationNoteAdded)].raw
	if !strings.Contains(note, `"note_id": "note-1"`) ||
		!strings.Contains(note, fmt.Sprintf(`"note_length": %d`, utf8.RuneCountInString(noteMarker))) {
		t.Fatalf("note_added lacks its id or length: %s", note)
	}

	// A failed append rolls the caller's transaction back.
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		return adminaccess.AppendElevationEvent(t.Context(), tx, securityaudit.FailingAppender{}, adminaccess.ElevationEvent{
			Action: securityaudit.ElevationReviewed, Source: securityaudit.ElevationSourceConsole, Elevation: elevation,
		})
	})
	if err == nil {
		t.Fatal("a failing appender did not fail the append")
	}
	if len(elevationEvents(t, pool, "")) != before+len(events) {
		t.Fatal("a failed append left an event")
	}
	if err := adminaccess.AppendElevationEvent(context.Background(), nil, nil, adminaccess.ElevationEvent{}); !errors.Is(err, adminaccess.ErrElevationNoAppender) {
		t.Fatalf("no appender: %v", err)
	}
}

func indexOf(actions []securityaudit.ElevationAction, want securityaudit.ElevationAction) int {
	for i, action := range actions {
		if action == want {
			return i
		}
	}
	return -1
}
