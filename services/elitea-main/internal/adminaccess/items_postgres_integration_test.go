package adminaccess_test

// The notes and the acknowledgement on the real schema (ADR-0059 R2.9c,
// issue 2604): shared 0283 and 0285.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const itemsNoteMarker = "ITEMS-MARKER-77d0 nobody else holds the review permission"

func addNote(t *testing.T, pool *pgxpool.Pool, note adminaccess.ElevationNote) (adminaccess.NoteResult, error) {
	t.Helper()
	var result adminaccess.NoteResult
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		result, err = adminaccess.AddElevationNote(t.Context(), tx, auditAppender(t), note)
		return err
	})
	return result, err
}

// unreviewedAt is an elevation whose review became unreviewed_no_reviewer
// on the deadline pass at base+73h.
func unreviewedAt(t *testing.T, pool *pgxpool.Pool, base time.Time) adminaccess.ElevationRecord {
	t.Helper()
	elevation := endedElevation(t, pool, adminaccess.ElevationPathApproved, base, adminaccess.RoleAdmin)
	if result := deadlinePass(t, pool, base.Add(73*time.Hour)); result.Unreviewed != 1 {
		t.Fatalf("premise: pass = %+v, want one move to unreviewed_no_reviewer", result)
	}
	return readElevation(t, pool, elevation.ID)
}

// A global administrator acknowledges an unreviewed_no_reviewer elevation
// once: the elevation is marked acknowledged with the note id, its state is
// unchanged, and the one note_added event carries the id, the length and the
// reason `acknowledgement`, never the text. A second acknowledgement is
// refused; a pending review is not acknowledged.
func TestAnAcknowledgementMarksTheElevationAndKeepsItsState(t *testing.T) {
	pool := newNoticeDatabase(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	elevation := unreviewedAt(t, pool, base)
	super := seedPerson(t, pool, adminaccess.RoleSuperAdmin)
	at := base.Add(74 * time.Hour)
	before := len(elevationEvents(t, pool, elevation.ID.String()))

	result, err := addNote(t, pool, adminaccess.ElevationNote{
		ID: elevation.ID, Text: itemsNoteMarker, Source: adminaccess.NoteSourceConsole,
		AuthorUserID: super, Acknowledge: true, At: at,
	})
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	note, ok, err := adminaccess.ElevationAcknowledgement(t.Context(), pool, elevation.ID)
	if err != nil || !ok || note != result.NoteID {
		t.Fatalf("acknowledgement = %s, %v (%v), want the note %s", note, ok, err, result.NoteID)
	}
	if state := readElevation(t, pool, elevation.ID).ReviewState; state != adminaccess.ElevationUnreviewedNoReviewer {
		t.Fatalf("the state after the acknowledgement is %q, want it unchanged", state)
	}
	events := elevationEvents(t, pool, elevation.ID.String())
	if len(events) != before+1 {
		t.Fatalf("%d events written, want 1", len(events)-before)
	}
	last := events[len(events)-1]
	if last.action != string(securityaudit.ElevationNoteAdded) || strings.Contains(last.raw, "ITEMS-MARKER") ||
		!strings.Contains(last.raw, result.NoteID.String()) || !strings.Contains(last.raw, `"acknowledgement"`) {
		t.Fatalf("the event is %s, want note_added with the note id and the acknowledgement reason, no text", last.raw)
	}

	if _, err := addNote(t, pool, adminaccess.ElevationNote{
		ID: elevation.ID, Text: "again", Source: adminaccess.NoteSourceConsole,
		AuthorUserID: super, Acknowledge: true, At: at,
	}); !errors.Is(err, adminaccess.ErrElevationAcknowledged) {
		t.Fatalf("a second acknowledgement: %v, want %v", err, adminaccess.ErrElevationAcknowledged)
	}

	pending := endedElevation(t, pool, adminaccess.ElevationPathApproved, base, adminaccess.RoleAdmin)
	if _, err := addNote(t, pool, adminaccess.ElevationNote{
		ID: pending.ID, Text: "too early", Source: adminaccess.NoteSourceConsole,
		AuthorUserID: super, Acknowledge: true, At: at,
	}); !errors.Is(err, adminaccess.ErrElevationNotAcknowledgeable) {
		t.Fatalf("an acknowledgement of a pending review: %v, want %v", err, adminaccess.ErrElevationNotAcknowledgeable)
	}
	if _, ok, _ := adminaccess.ElevationAcknowledgement(t.Context(), pool, pending.ID); ok {
		t.Fatal("a refused acknowledgement was written")
	}

	// The schema holds the rules too: no second acknowledgement row, no
	// operator acknowledgement, no note edited.
	for name, statement := range map[string]string{
		"second": `INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, user_id, note, note_source, acknowledgement, created_at)
			VALUES ($1, 'note', 1, 'x', 'console', true, now())`,
		"operator": `INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, note, note_source, acknowledgement, created_at)
			VALUES ($1, 'note', 'x', 'operator', true, now())`,
		"edit": `UPDATE elitea_auth.admin_elevation_items SET note = 'rewritten' WHERE elevation_id = $1 AND kind = 'note'`,
	} {
		if _, err := pool.Exec(t.Context(), statement, elevation.ID); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// An operator note is attached in any state, names no person, and never
// changes the state.
func TestAnOperatorNoteLeavesTheStateAsItIs(t *testing.T) {
	pool := newElevationDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	active := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	result, err := addNote(t, pool, adminaccess.ElevationNote{
		ID: active.ID, Text: "  " + itemsNoteMarker + "  ", Source: adminaccess.NoteSourceOperator, At: now,
	})
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	if got := readElevation(t, pool, active.ID); got.State(now) != adminaccess.ElevationStateActive || !got.EndedAt.IsZero() {
		t.Fatalf("the note changed the elevation: %+v", got)
	}
	notes, err := adminaccess.ElevationNotes(t.Context(), pool, active.ID)
	if err != nil || len(notes) != 1 || notes[0].ID != result.NoteID || notes[0].Text != itemsNoteMarker ||
		notes[0].AuthorUserID != 0 || notes[0].Source != adminaccess.NoteSourceOperator {
		t.Fatalf("notes = %+v (%v)", notes, err)
	}
	for name, note := range map[string]adminaccess.ElevationNote{
		"empty":                 {ID: active.ID, Text: " ", Source: adminaccess.NoteSourceOperator, At: now},
		"a NUL byte":            {ID: active.ID, Text: "a\x00b", Source: adminaccess.NoteSourceOperator, At: now},
		"operator with person":  {ID: active.ID, Text: "x", Source: adminaccess.NoteSourceOperator, AuthorUserID: 3, At: now},
		"operator acknowledges": {ID: active.ID, Text: "x", Source: adminaccess.NoteSourceOperator, Acknowledge: true, At: now},
		"console without one":   {ID: active.ID, Text: "x", Source: adminaccess.NoteSourceConsole, At: now},
	} {
		if _, err := addNote(t, pool, note); !errors.Is(err, adminaccess.ErrElevationInvalid) {
			t.Errorf("%s: %v, want %v", name, err, adminaccess.ErrElevationInvalid)
		}
	}
}
