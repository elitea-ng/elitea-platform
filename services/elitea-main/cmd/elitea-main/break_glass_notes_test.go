package main

// `elitea-main break-glass note` and `list --elevations` (ADR-0059 R2.9c,
// issue 2604): the argument and input rules before any connection, and,
// against the full corpus (skipped without ELITEA_TEST_DATABASE_URL), the
// stored note, its one event and the unchanged state.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const breakGlassNoteMarker = "NOTE-MARKER-5e1b the holder was the only administrator on call"

func runBreakGlassNoteAgainst(t *testing.T, dsn string, stdin io.Reader, args ...string) breakGlassRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runBreakGlassWithInput(context.Background(), args,
		func(key string) (string, bool) { return dsn, key == "DATABASE_URL" }, stdin, &stdout, &stderr,
		pgxpool.New, time.Now)
	return breakGlassRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// A note needs exactly one elevation id and a text from stdin or --file; a
// bad one is refused before any connection, with exit 2.
func TestBreakGlassNoteRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	long := strings.Repeat("x", adminaccess.MaxReviewNoteLength+1)
	for name, tc := range map[string]struct {
		args  []string
		stdin string
		want  string
	}{
		"no id":           {[]string{"note"}, "a note", "needs an elevation id"},
		"two ids":         {[]string{"note", uuid.NewString(), uuid.NewString()}, "a note", "exactly one elevation ID"},
		"a bad id":        {[]string{"note", "42"}, "a note", `"42" is not an elevation id`},
		"empty stdin":     {[]string{"note", uuid.NewString()}, "  \n", "is empty"},
		"too long":        {[]string{"note", uuid.NewString()}, long, "longer than"},
		"a missing file":  {[]string{"note", uuid.NewString(), "--file", "/nonexistent/note.txt"}, "", "--file"},
		"list with an id": {[]string{"list", "--elevations", uuid.NewString()}, "", "takes no positional argument"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			connected := false
			code := runBreakGlassWithInput(context.Background(), tc.args,
				func(string) (string, bool) { return "postgres://unused", true }, strings.NewReader(tc.stdin),
				&stdout, &stderr, mustNotConnect(&connected), time.Now)
			if code != breakGlassExitUsage || connected {
				t.Fatalf("code = %d, connected = %v, stderr = %s", code, connected, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused invocation printed %q", stdout.String())
			}
		})
	}
}

// unreviewedElevation is an elevation whose review reached
// unreviewed_no_reviewer: started, ended by the operator, then moved by the
// deadline (written here as the reconciler writes it).
func unreviewedElevation(t *testing.T, pool *pgxpool.Pool, dsn string) adminaccess.ElevationRecord {
	t.Helper()
	elevation := startTestElevation(t, pool)
	if run := runBreakGlassAgainst(t, dsn, "end", elevation.ID.String()); run.code != breakGlassExitOK {
		t.Fatalf("end: exit %d, %s", run.code, run.stderr)
	}
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_auth.admin_elevations SET review_state = $2 WHERE id = $1`,
		elevation.ID, adminaccess.ElevationUnreviewedNoReviewer); err != nil {
		t.Fatalf("move the review to unreviewed_no_reviewer: %v", err)
	}
	return elevation
}

// `note` stores the note and one chained note_added event that carries the
// note's id and length and not its text, and the state stays
// unreviewed_no_reviewer. A note from --file is the same.
func TestBreakGlassNoteStoresTheNoteAndOneEventWithoutItsText(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	elevation := unreviewedElevation(t, pool, dsn)
	before := countElevationEvents(t, pool)

	run := runBreakGlassNoteAgainst(t, dsn, strings.NewReader(breakGlassNoteMarker+"\n"), "note", elevation.ID.String())
	if run.code != breakGlassExitOK || !strings.Contains(run.stdout, "added note") ||
		!strings.Contains(run.stdout, adminaccess.ElevationUnreviewedNoReviewer) {
		t.Fatalf("note: exit %d\nstdout: %s\nstderr: %s", run.code, run.stdout, run.stderr)
	}
	notes, err := adminaccess.ElevationNotes(t.Context(), pool, elevation.ID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v (%v), want one", notes, err)
	}
	note := notes[0]
	if note.Text != breakGlassNoteMarker || note.Source != adminaccess.NoteSourceOperator ||
		note.AuthorUserID != 0 || note.Acknowledgement {
		t.Fatalf("note = %+v, want the operator's text, no person, no acknowledgement", note)
	}
	if after := countElevationEvents(t, pool); after != before+1 {
		t.Fatalf("%d events written, want 1", after-before)
	}
	var raw string
	if err := pool.QueryRow(t.Context(), `
SELECT event::text FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event -> 'data' -> 'elevation' ->> 'action' = $2
  AND event -> 'data' -> 'elevation' ->> 'elevation_id' = $3`,
		string(securityaudit.TypeAdminElevation), string(securityaudit.ElevationNoteAdded), elevation.ID.String()).Scan(&raw); err != nil {
		t.Fatalf("read the note_added event: %v", err)
	}
	if strings.Contains(raw, "NOTE-MARKER") || !strings.Contains(raw, note.ID.String()) ||
		!strings.Contains(raw, fmt.Sprintf(`"note_length": %d`, len(breakGlassNoteMarker))) || !strings.Contains(raw, `"source": "operator"`) {
		t.Fatalf("the note_added event is %s, want the note's id and length, the operator source and no text", raw)
	}
	if current, err := adminaccess.ReadElevation(t.Context(), pool, elevation.ID); err != nil ||
		current.ReviewState != adminaccess.ElevationUnreviewedNoReviewer {
		t.Fatalf("the state after the note: %+v (%v), want unreviewed_no_reviewer", current, err)
	}
	if _, ok, err := adminaccess.ElevationAcknowledgement(t.Context(), pool, elevation.ID); err != nil || ok {
		t.Fatalf("an operator note acknowledged the elevation (%v)", err)
	}

	// From a file, in another state: an active elevation stays active.
	active := startTestElevation(t, pool)
	file := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(file, []byte("the console was taken over"), 0o600); err != nil {
		t.Fatal(err)
	}
	if run := runBreakGlassNoteAgainst(t, dsn, strings.NewReader(""), "note", active.ID.String(), "--file", file); run.code != breakGlassExitOK ||
		!strings.Contains(run.stdout, adminaccess.ElevationStateActive) {
		t.Fatalf("note --file: exit %d, %s %s", run.code, run.stdout, run.stderr)
	}

	// An unknown elevation is refused and writes nothing.
	before = countElevationEvents(t, pool)
	if run := runBreakGlassNoteAgainst(t, dsn, strings.NewReader("x"), "note", uuid.NewString()); run.code != breakGlassExitRefused ||
		!strings.Contains(run.stderr, "no elevation") {
		t.Fatalf("an unknown id: exit %d, %s", run.code, run.stderr)
	}
	if after := countElevationEvents(t, pool); after != before {
		t.Fatalf("%d events written by a refused note", after-before)
	}
}

// `list --elevations` prints every elevation's id with its state, and never
// its reason text.
func TestBreakGlassListElevationsPrintsIDsAndStatesWithoutTheReason(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	active := startTestElevation(t, pool)
	unreviewed := unreviewedElevation(t, pool, dsn)
	run := runBreakGlassAgainst(t, dsn, "list", "--elevations")
	if run.code != breakGlassExitOK {
		t.Fatalf("list --elevations: exit %d, %s", run.code, run.stderr)
	}
	for _, want := range []string{
		active.ID.String() + " " + adminaccess.ElevationStateActive,
		unreviewed.ID.String() + " " + adminaccess.ElevationUnreviewedNoReviewer,
	} {
		if !strings.Contains(run.stdout, want) {
			t.Fatalf("stdout = %s, want a line with %q", run.stdout, want)
		}
	}
	if strings.Contains(run.stdout, "the console was taken") {
		t.Fatalf("list --elevations printed a reason text: %s", run.stdout)
	}
}
