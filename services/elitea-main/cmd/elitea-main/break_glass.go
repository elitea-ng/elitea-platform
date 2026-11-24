package main

// `elitea-main break-glass`: break-glass elevations from the command line
// (ADR-0059 decision 5, "Path 1" step 5, decision 7, "Process commands";
// story R2.13, issue 2527).
//
// If an elevation is misused and the console is what was taken, the team
// ends it from the workload: the command ships in the elitea-main image and
// runs where the service runs (kubectl exec into the main workload, or a
// one-off Job from the same image and Secret), like `admin-role` and
// `oidc-relink`. It reads DATABASE_URL and talks to the database directly;
// it is not reachable over HTTP or gRPC.
//
// Usage:
//
//	elitea-main break-glass end ID [ID...]
//	elitea-main break-glass end --all
//
// ID is an elevation id (a UUID). `end` goes through the ledger's end
// function (adminaccess.EndElevation) with the end reason `operator`, so the
// elevation moves to review_pending like any other end, and each end writes
// one chained admin.elevation event `ended` with the source `operator`, in
// the same transaction. Every id is ended in one transaction: an unknown id,
// a request that never started, or an elevation that already ended refuses
// the whole command and nothing is written. `--all` ends every active
// elevation. Output is one line per elevation ended, with its id, person and
// session reference.
//
// The `issue`, `list` and `revoke` sub-commands of the sealed credential
// (R2.6a, issue 2520) join this group. A sealed session ends with its
// elevation once the sealed path exists (R2.7a).
//
// Exit codes: 0 done, 1 failed and nothing changed (database unreachable, a
// write failed), 2 invalid invocation, 3 refused (an unknown id, or one that
// is not an active elevation).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

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
	ids []uuid.UUID
	all bool
}

func runBreakGlass(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	return runBreakGlassWith(ctx, args, lookup, stdout, stderr, pgxpool.New, time.Now)
}

func breakGlassUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage:
  elitea-main %[1]s end ID [ID...]
  elitea-main %[1]s end --all
ID is an elevation id. Ends the elevation with the end reason operator and
records it in the security audit. Reads DATABASE_URL. Exit codes: 0 done,
1 failed (nothing changed), 2 invalid invocation, 3 refused (an unknown id, or
one that is not an active elevation; nothing changed).
`, breakGlassCommand)
}

// parseBreakGlassArgs validates the invocation before any connection. It
// returns ok=false after it has printed the reason.
func parseBreakGlassArgs(args []string, stderr io.Writer) (breakGlassRequest, bool) {
	if len(args) == 0 {
		breakGlassUsage(stderr)
		return breakGlassRequest{}, false
	}
	req := breakGlassRequest{sub: args[0]}
	if req.sub != breakGlassEnd {
		_, _ = fmt.Fprintf(stderr, "unknown sub-command %q\n", req.sub)
		breakGlassUsage(stderr)
		return breakGlassRequest{}, false
	}
	flags := flag.NewFlagSet(breakGlassCommand+" "+req.sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	all := flags.Bool("all", false, "end every active elevation")
	flags.Usage = func() { breakGlassUsage(stderr) }
	var values []string
	rest := args[1:]
	for {
		before := len(rest)
		if err := flags.Parse(rest); err != nil {
			return breakGlassRequest{}, false
		}
		if flags.NArg() == 0 {
			break
		}
		consumed := rest[:before-flags.NArg()]
		if len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			values = append(values, flags.Args()...)
			break
		}
		values = append(values, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	req.all = *all
	switch {
	case req.all && len(values) > 0:
		_, _ = fmt.Fprintln(stderr, "end takes either IDs or --all, not both")
		return breakGlassRequest{}, false
	case !req.all && len(values) == 0:
		_, _ = fmt.Fprintln(stderr, "end needs an elevation ID or --all")
		breakGlassUsage(stderr)
		return breakGlassRequest{}, false
	}
	for _, value := range values {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%q is not an elevation id\n", value)
			return breakGlassRequest{}, false
		}
		if !slices.Contains(req.ids, id) {
			req.ids = append(req.ids, id)
		}
	}
	return req, true
}

func runBreakGlassWith(
	ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer,
	connect oidcRelinkConnect, now func() time.Time,
) int {
	req, ok := parseBreakGlassArgs(args, stderr)
	if !ok {
		return breakGlassExitUsage
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

	at := now().UTC()
	var lines []string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ids := req.ids
		if req.all {
			var err error
			if ids, err = activeElevations(ctx, tx, at); err != nil {
				return err
			}
		}
		for _, id := range ids {
			line, err := breakGlassEndTx(ctx, tx, appender, id, at)
			if err != nil {
				return err
			}
			lines = append(lines, line)
		}
		return nil
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
	if len(lines) == 0 {
		_, _ = fmt.Fprintln(stdout, "no active elevation")
	}
	for _, line := range lines {
		_, _ = fmt.Fprintln(stdout, line)
	}
	return breakGlassExitOK
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
