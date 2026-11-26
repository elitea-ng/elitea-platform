package main

// `elitea-main break-glass`: break-glass access from the command line
// (ADR-0059 decision 5, "Path 1" step 5 and "Path 2", decision 7, "Process
// commands"; stories R2.13, issue 2527, and R2.6a, issue 2520).
//
// The commands ship in the elitea-main image and run where the service runs
// (kubectl exec into the main workload, or a one-off Job from the same image
// and Secret), like `admin-role` and `oidc-relink`. They read DATABASE_URL and
// talk to the database directly; none is reachable over HTTP or gRPC. Running
// them needs access to the workload, which is the second control on the
// sealed path.
//
// Usage:
//
//	elitea-main break-glass end ID [ID...]
//	elitea-main break-glass end --all
//	elitea-main break-glass issue --user USER [--level admin|super_admin]
//	                              [--label TEXT] [--split] [--expires WHEN]
//	                              [--ticket ID]
//	elitea-main break-glass issue --unbound --ticket ID --label TEXT
//	                              [--level admin|super_admin] [--split]
//	                              [--expires WHEN]
//	elitea-main break-glass list [--elevations]
//	elitea-main break-glass revoke ID [ID...]
//	elitea-main break-glass note ID [--file PATH] < note.txt
//
// END. ID is an elevation id (a UUID). `end` goes through the ledger's end
// function (adminaccess.EndElevation) with the end reason `operator`, so the
// elevation moves to review_pending like any other end, and each end writes
// one chained admin.elevation event `ended` with the source `operator`, in
// the same transaction. Every id is ended in one transaction: an unknown id,
// a request that never started, or an elevation that already ended refuses
// the whole command and nothing is written. `--all` ends every active
// elevation. Output is one line per elevation ended, with its id, person and
// session reference. A sealed session ends with its elevation.
//
// ISSUE (R2.6a). Creates one sealed credential `bg_<selector>.<verifier>`
// bound to USER (an id or an e-mail address), and prints it ONCE, on stdout.
// Only the Argon2id digest of the verifier is stored; the verifier is in no
// log line, no audit event and no row. --level is the level of the elevation
// the credential starts (default admin: the operator is the approver on this
// path). --split prints the verifier as two halves for two custodians instead
// of the whole credential. --expires is a lifetime (`90d`, `720h`) or a date
// (`2027-06-30`, RFC 3339); the default and the ceiling are 365 days. The
// event is credential_issued with the source `operator` and the level.
//
// UNBOUND (R2.6b). `--unbound` binds the credential to the reserved
// break-glass account, break-glass@centry.user, instead of a named person: an
// envelope in a safe cannot name who will open it. It needs `--ticket` (the
// change or incident it is issued under, recorded in the event) and `--label`
// (the custodian), and takes no `--user`. The first unbound issue creates the
// account, with no role; a second one reuses it. The account holds no role,
// creates no token and signs in through no other plane.
//
// LIST. One line per credential, active or not, with its state; never the
// verifier or its digest. The read is recorded as a decision event
// (admin.elevation.credential_list) with the source `operator`.
//
// LIST --ELEVATIONS (R2.9c). One line per elevation, newest first: its id,
// person, level, path and state. Never the reason text.
//
// NOTE (R2.9c). Attaches a note, read from stdin or --file (never an
// argument, so it lands in no shell history and no process list), to the
// record of elevation ID, in any state, with the source `operator`. The state
// does not change. The text is stored in the items table; the note_added
// event carries its id and length only. The command takes no --acknowledge:
// it cannot name the acting person, so the acknowledgement of an
// unreviewed_no_reviewer elevation is the console route's.
//
// REVOKE. Sets the revoked time of each credential, in one transaction, with
// one credential_revoked event each. Rows are never deleted. An unknown id, or
// a credential already revoked or used, refuses the whole command.
//
// Exit codes: 0 done, 1 failed and nothing changed (database unreachable, a
// write failed), 2 invalid invocation, 3 refused (an unknown id, one that is
// not in the state the command needs, or a person who is not active).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	breakGlassCommand = "break-glass"
	breakGlassTimeout = 2 * time.Minute
	// breakGlassAuditSource is the CloudEvents source of the command's events.
	breakGlassAuditSource = "elitea-main/break-glass"
	breakGlassEnd         = "end"
	breakGlassIssue       = "issue"
	breakGlassList        = "list"
	breakGlassRevoke      = "revoke"
	breakGlassNote        = "note"
)

// The exit codes, as admin-role's.
const (
	breakGlassExitOK      = adminRoleExitOK
	breakGlassExitFailed  = adminRoleExitFailed
	breakGlassExitUsage   = adminRoleExitUsage
	breakGlassExitRefused = adminRoleExitRefused
)

type breakGlassRequest struct {
	sub string
	// ids are the elevation ids of end, or the credential ids of revoke.
	ids []uuid.UUID
	all bool
	// The issue: the person, the level, the label, the split and the
	// lifetime from the command's clock.
	user     string
	level    string
	label    string
	split    bool
	lifetime time.Duration
	// unbound binds the credential to the reserved account; ticket is the
	// change or incident it is issued under (R2.6b).
	unbound bool
	ticket  string
	// elevations lists the elevations instead of the credentials (R2.9c).
	elevations bool
	// file is the note's file; "" reads the note from stdin. note is the
	// text read, before any connection.
	file string
	note string
}

func runBreakGlass(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	return runBreakGlassWithInput(ctx, args, lookup, os.Stdin, stdout, stderr, pgxpool.New, time.Now)
}

// runBreakGlassWith is the test seam: no standard input.
func runBreakGlassWith(
	ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer,
	connect oidcRelinkConnect, now func() time.Time,
) int {
	return runBreakGlassWithInput(ctx, args, lookup, strings.NewReader(""), stdout, stderr, connect, now)
}

func breakGlassUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage:
  elitea-main %[1]s end ID [ID...]
  elitea-main %[1]s end --all
  elitea-main %[1]s issue --user USER [--level admin|super_admin] [--label TEXT] [--split] [--expires WHEN] [--ticket ID]
  elitea-main %[1]s issue --unbound --ticket ID --label TEXT [--level admin|super_admin] [--split] [--expires WHEN]
  elitea-main %[1]s list [--elevations]
  elitea-main %[1]s revoke ID [ID...]
  elitea-main %[1]s note ID [--file PATH] < note.txt
end: ID is an elevation id. Ends the elevation with the end reason operator.
issue: creates a sealed credential bound to USER (an id or an e-mail address)
  and prints it once; only its digest is stored. --level defaults to admin.
  --split prints the verifier as two halves for two custodians. --expires is
  a lifetime (90d, 720h) or a date (2027-06-30); default and maximum 365 days.
  --unbound binds it to the reserved account break-glass@centry.user instead
  of a person, and needs --ticket and --label (the custodian).
list: every sealed credential and its state; never a secret.
  --elevations lists the newest elevations, their ids and states instead;
  never a reason text.
revoke: ID is a sealed credential id; the row is kept.
note: ID is an elevation id. Attaches the note read from stdin (or --file) to
  its record, in any state, without changing the state. The acknowledgement
  of an elevation nobody could review is the console's, not this command's.
Every command but list --elevations records a security audit event. Reads
DATABASE_URL. Exit codes:
0 done, 1 failed (nothing changed), 2 invalid invocation, 3 refused (nothing
changed).
`, breakGlassCommand)
}

// parseInterleaved parses flags that may come before, between or after the
// positional values, and answers the values. It returns ok=false after the
// flag set has printed the reason.
func parseInterleaved(flags *flag.FlagSet, args []string) ([]string, bool) {
	var values []string
	rest := args
	for {
		before := len(rest)
		if err := flags.Parse(rest); err != nil {
			return nil, false
		}
		if flags.NArg() == 0 {
			return values, true
		}
		consumed := rest[:before-flags.NArg()]
		if len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			return append(values, flags.Args()...), true
		}
		values = append(values, flags.Arg(0))
		rest = flags.Args()[1:]
	}
}

// breakGlassFlags are the flags of one sub-command.
type breakGlassFlags struct {
	all                                 *bool
	user, level, label, expires, ticket *string
	split, unbound                      *bool
	elevations                          *bool
	file                                *string
	// idKind names what a positional value is; "" for a sub-command that
	// takes none.
	idKind string
}

func newBreakGlassFlags(sub string, flags *flag.FlagSet) (breakGlassFlags, bool) {
	var f breakGlassFlags
	switch sub {
	case breakGlassEnd:
		f.all = flags.Bool("all", false, "end every active elevation")
		f.idKind = "an elevation id"
	case breakGlassIssue:
		f.user = flags.String("user", "", "the person the credential is bound to: an id or an e-mail address")
		f.level = flags.String("level", adminaccess.ElevationLevelAdmin,
			"the level of the elevation the credential starts: admin or super_admin")
		f.label = flags.String("label", "", "what the custodians call the credential")
		f.split = flags.Bool("split", false, "print the verifier as two halves for two custodians")
		f.expires = flags.String("expires", "", "a lifetime (90d, 720h) or a date (2027-06-30); at most 365 days")
		f.unbound = flags.Bool("unbound", false,
			"bind the credential to the reserved account "+adminaccess.ReservedBreakGlassEmail+" instead of a person")
		f.ticket = flags.String("ticket", "", "the change or incident ticket the credential is issued under")
	case breakGlassList:
		f.elevations = flags.Bool("elevations", false, "list the elevations and their states instead of the credentials")
	case breakGlassRevoke:
		f.idKind = "a sealed credential id"
	case breakGlassNote:
		f.file = flags.String("file", "", "read the note from this file instead of stdin")
		f.idKind = "an elevation id"
	default:
		return breakGlassFlags{}, false
	}
	return f, true
}

// parseBreakGlassArgs validates the invocation before any connection. It
// returns ok=false after it has printed the reason. now is the command's
// clock, which an --expires date is measured from.
func parseBreakGlassArgs(args []string, stderr io.Writer, now time.Time) (breakGlassRequest, bool) {
	if len(args) == 0 {
		breakGlassUsage(stderr)
		return breakGlassRequest{}, false
	}
	req := breakGlassRequest{sub: args[0]}
	flags := flag.NewFlagSet(breakGlassCommand+" "+req.sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { breakGlassUsage(stderr) }
	f, known := newBreakGlassFlags(req.sub, flags)
	if !known {
		_, _ = fmt.Fprintf(stderr, "unknown sub-command %q\n", req.sub)
		breakGlassUsage(stderr)
		return breakGlassRequest{}, false
	}
	values, ok := parseInterleaved(flags, args[1:])
	if !ok {
		return breakGlassRequest{}, false
	}
	if f.all != nil {
		req.all = *f.all
	}
	switch {
	case f.idKind == "" && len(values) > 0:
		_, _ = fmt.Fprintf(stderr, "%s takes no positional argument, got %q\n", req.sub, values[0])
		return breakGlassRequest{}, false
	case req.all && len(values) > 0:
		_, _ = fmt.Fprintln(stderr, "end takes either IDs or --all, not both")
		return breakGlassRequest{}, false
	case f.idKind != "" && !req.all && len(values) == 0:
		if req.sub == breakGlassEnd {
			_, _ = fmt.Fprintln(stderr, "end needs an elevation ID or --all")
		} else {
			_, _ = fmt.Fprintf(stderr, "%s needs %s\n", req.sub, f.idKind)
		}
		breakGlassUsage(stderr)
		return breakGlassRequest{}, false
	}
	for _, value := range values {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%q is not %s\n", value, f.idKind)
			return breakGlassRequest{}, false
		}
		if !slices.Contains(req.ids, id) {
			req.ids = append(req.ids, id)
		}
	}
	if f.elevations != nil {
		req.elevations = *f.elevations
	}
	if req.sub == breakGlassNote {
		if len(req.ids) != 1 {
			_, _ = fmt.Fprintln(stderr, "note takes exactly one elevation ID")
			return breakGlassRequest{}, false
		}
		req.file = strings.TrimSpace(*f.file)
	}
	if req.sub == breakGlassIssue {
		return parseBreakGlassIssue(req, f, stderr, now)
	}
	return req, true
}

// breakGlassNoteLimit bounds what is read for a note: more than the longest
// note in bytes, so a longer one is refused by its length, not cut.
const breakGlassNoteLimit = 4 * adminaccess.MaxReviewNoteLength

// readBreakGlassNote reads the note of a `note` from its file or stdin and
// checks it, before any connection. It returns ok=false after it has printed
// the reason. The text comes from a file or stdin, never an argument, so it
// lands in no shell history and no process list.
func readBreakGlassNote(req breakGlassRequest, stdin io.Reader, stderr io.Writer) (breakGlassRequest, bool) {
	source := "stdin"
	input := stdin
	if req.file != "" {
		file, err := os.Open(req.file)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "note: --file: %v\n", err)
			return breakGlassRequest{}, false
		}
		defer func() { _ = file.Close() }()
		source, input = req.file, file
	}
	raw, err := io.ReadAll(io.LimitReader(input, breakGlassNoteLimit+1))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "note: read %s: %v\n", source, err)
		return breakGlassRequest{}, false
	}
	text := strings.TrimSpace(string(raw))
	switch {
	case !utf8.ValidString(text):
		_, _ = fmt.Fprintf(stderr, "note: %s is not UTF-8 text\n", source)
		return breakGlassRequest{}, false
	case text == "":
		_, _ = fmt.Fprintf(stderr, "note: the note read from %s is empty; write it on stdin or pass --file\n", source)
		return breakGlassRequest{}, false
	case len(raw) > breakGlassNoteLimit || utf8.RuneCountInString(text) > adminaccess.MaxReviewNoteLength:
		_, _ = fmt.Fprintf(stderr, "note: the note is longer than %d characters\n", adminaccess.MaxReviewNoteLength)
		return breakGlassRequest{}, false
	}
	req.note = text
	return req, true
}

// parseBreakGlassIssue checks the flags of an issue.
func parseBreakGlassIssue(req breakGlassRequest, f breakGlassFlags, stderr io.Writer, now time.Time) (breakGlassRequest, bool) {
	req.user, req.level, req.label, req.split = strings.TrimSpace(*f.user), strings.TrimSpace(*f.level), *f.label, *f.split
	req.unbound, req.ticket = *f.unbound, strings.TrimSpace(*f.ticket)
	switch {
	case req.unbound && req.user != "":
		_, _ = fmt.Fprintln(stderr, "issue takes either --user or --unbound, not both")
		return breakGlassRequest{}, false
	case req.unbound && req.ticket == "":
		_, _ = fmt.Fprintln(stderr, "issue --unbound needs --ticket: the change or incident the credential is issued under")
		return breakGlassRequest{}, false
	case req.unbound && strings.TrimSpace(req.label) == "":
		_, _ = fmt.Fprintln(stderr, "issue --unbound needs --label: the custodian of the envelope")
		return breakGlassRequest{}, false
	case !req.unbound && req.user == "":
		_, _ = fmt.Fprintln(stderr, "issue needs --user (a credential is bound to a named person) or --unbound")
		return breakGlassRequest{}, false
	}
	if req.level != adminaccess.ElevationLevelAdmin && req.level != adminaccess.ElevationLevelSuperAdmin {
		_, _ = fmt.Fprintf(stderr, "--level %q is not admin or super_admin\n", req.level)
		return breakGlassRequest{}, false
	}
	lifetime, err := parseSealedExpiry(*f.expires, now)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "--expires: %v\n", err)
		return breakGlassRequest{}, false
	}
	req.lifetime = lifetime
	// The store's own check, with a stand-in person: the label's rules.
	probe := adminaccess.SealedIssue{UserID: 1, Level: req.level, Label: req.label, Ticket: req.ticket,
		At: now, Lifetime: req.lifetime}
	if req.unbound {
		probe.UserID, probe.Unbound = 0, true
	}
	if err := probe.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "issue: %v\n", err)
		return breakGlassRequest{}, false
	}
	return req, true
}

// parseSealedExpiry answers the lifetime --expires asks for, from now: a
// number of days (`90d`), a Go duration (`720h`), a date (`2027-06-30`, the
// start of that day in UTC) or an RFC 3339 time. "" is the default lifetime.
// More than 365 days is refused here, so nothing connects.
func parseSealedExpiry(value string, now time.Time) (time.Duration, error) {
	value = strings.TrimSpace(value)
	var lifetime time.Duration
	switch {
	case value == "":
		return adminaccess.DefaultSealedCredentialLifetime, nil
	case strings.HasSuffix(value, "d"):
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of days", value)
		}
		// Clamped both ways before the multiplication, so a huge count of
		// either sign cannot wrap into a valid lifetime; both are refused
		// below.
		days = max(-1, min(days, 366))
		lifetime = time.Duration(days) * 24 * time.Hour
	default:
		if d, err := time.ParseDuration(value); err == nil {
			lifetime = d
			break
		}
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			if at, err = time.Parse(time.DateOnly, value); err != nil {
				return 0, fmt.Errorf("%q is not a lifetime (90d, 720h) or a date (2027-06-30)", value)
			}
		}
		lifetime = at.Sub(now)
	}
	switch {
	case lifetime <= 0:
		return 0, fmt.Errorf("%q is not in the future", value)
	case lifetime > adminaccess.MaxSealedCredentialLifetime:
		return 0, fmt.Errorf("%q is more than 365 days away; a sealed credential lives at most 365 days", value)
	}
	return lifetime, nil
}

func runBreakGlassWithInput(
	ctx context.Context, args []string, lookup func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer,
	connect oidcRelinkConnect, now func() time.Time,
) int {
	at := now().UTC()
	req, ok := parseBreakGlassArgs(args, stderr, at)
	if !ok {
		return breakGlassExitUsage
	}
	if req.sub == breakGlassNote {
		if req, ok = readBreakGlassNote(req, stdin, stderr); !ok {
			return breakGlassExitUsage
		}
	}
	dsn, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		_, _ = fmt.Fprintln(stderr, "DATABASE_URL is required")
		return breakGlassExitUsage
	}
	appender, err := securityaudit.NewPostgresAppender(breakGlassAuditSource)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "compose the security audit appender: %v\n", err)
		return breakGlassExitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, breakGlassTimeout)
	defer cancel()
	pool, err := connect(ctx, dsn)
	if err != nil {
		// pgx's error can quote the DSN; say what failed, not with what.
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return breakGlassExitFailed
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return breakGlassExitFailed
	}

	var lines []string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		switch req.sub {
		case breakGlassIssue:
			lines, err = breakGlassIssueTx(ctx, tx, appender, req, at)
		case breakGlassList:
			if req.elevations {
				lines, err = breakGlassListElevationsTx(ctx, tx, at)
			} else {
				lines, err = breakGlassListTx(ctx, tx, appender, at)
			}
		case breakGlassNote:
			lines, err = breakGlassNoteTx(ctx, tx, appender, req, at)
		case breakGlassRevoke:
			lines, err = breakGlassRevokeTx(ctx, tx, appender, req.ids, at)
		default:
			lines, err = breakGlassEndAllTx(ctx, tx, appender, req, at)
		}
		return err
	})
	var typed *adminRoleError
	switch {
	case err == nil:
	case errors.As(err, &typed):
		_, _ = fmt.Fprintf(stderr, "%s %s: %s; nothing was changed\n", breakGlassCommand, req.sub, typed.message)
		return typed.code
	default:
		_, _ = fmt.Fprintf(stderr, "%s %s failed; nothing was changed: %v\n", breakGlassCommand, req.sub, err)
		return breakGlassExitFailed
	}
	// Only after the commit: a credential whose row did not commit is never
	// shown.
	for _, line := range lines {
		_, _ = fmt.Fprintln(stdout, line)
	}
	return breakGlassExitOK
}

// breakGlassEndAllTx ends the elevations of an `end`.
func breakGlassEndAllTx(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, req breakGlassRequest, at time.Time) ([]string, error) {
	ids := req.ids
	if req.all {
		var err error
		if ids, err = activeElevations(ctx, tx, at); err != nil {
			return nil, err
		}
	}
	var lines []string
	for _, id := range ids {
		line, err := breakGlassEndTx(ctx, tx, appender, id, at)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = []string{"no active elevation"}
	}
	return lines, nil
}

// activeElevations answers every active elevation at at, locked for the end.
func activeElevations(ctx context.Context, tx pgx.Tx, at time.Time) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
SELECT id FROM elitea_auth.admin_elevations
WHERE started_at IS NOT NULL AND ended_at IS NULL AND expires_at > $1
ORDER BY started_at, id
FOR UPDATE`, at)
	if err != nil {
		return nil, fmt.Errorf("list the active elevations: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("list the active elevations: %w", err)
	}
	return ids, nil
}

// breakGlassEndTx ends one elevation with the reason operator and its event.
// Anything but an active elevation is refused: an unknown id, a request that
// never started, and one that already ended or ran out (the reconciler
// records that one as expired).
func breakGlassEndTx(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, id uuid.UUID, at time.Time) (string, error) {
	current, err := adminaccess.ReadElevation(ctx, tx, id)
	if errors.Is(err, adminaccess.ErrElevationNotFound) {
		return "", refusedError("no elevation %s", id)
	}
	if err != nil {
		return "", err
	}
	switch {
	case current.StartedAt.IsZero():
		return "", refusedError("elevation %s is a request that never started", id)
	case !current.EndedAt.IsZero():
		return "", refusedError("elevation %s already ended (%s)", id, current.EndReason)
	case !current.ActiveAt(at):
		return "", refusedError("elevation %s already ran out at %s", id, current.ExpiresAt.UTC().Format(time.RFC3339))
	}
	ended, changed, err := adminaccess.EndElevation(ctx, tx, appender, adminaccess.ElevationEnd{
		ID: id, Reason: adminaccess.ElevationEndOperator, At: at, Source: securityaudit.ElevationSourceOperator,
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return "", refusedError("elevation %s already ended", id)
	}
	return fmt.Sprintf("ended elevation %s of user %d (browser session %d) at %s; review pending",
		ended.ID, ended.UserID, ended.BrowserSessionRef, ended.EndedAt.UTC().Format(time.RFC3339)), nil
}
