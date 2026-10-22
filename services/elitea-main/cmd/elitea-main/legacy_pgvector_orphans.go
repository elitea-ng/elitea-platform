package main

// `elitea-main legacy-pgvector-orphans`: find, and with --drop remove, the
// legacy PgVector databases (project_<id>) and roles (project_<id>_user) whose
// project no longer exists (#1359).
//
// A project delete drops its own legacy database from the cleanup journal
// (projectprovisioning/legacy_vector_database.go). This is for the ones no
// journal will drop: projects deleted before that step came back (their rows
// completed without it), deleted by pylon, or deleted before the journal
// existed. It ships in the elitea-main image, so it runs where the service
// runs (kubectl exec, or a one-off Job from the same image and Secret).
//
// Usage:
//
//	elitea-main legacy-pgvector-orphans                        # dry run: list
//	elitea-main legacy-pgvector-orphans --drop --confirm-count N
//
// It reads ELITEA_LEGACY_PGVECTOR_ADMIN_URL (the legacy server) and
// DATABASE_URL (the platform database, for centry.project and the cleanup
// journal) from the environment, the same variables the service reads.
//
// AN ORPHAN is an id whose database or role is on the legacy server, with no
// row in centry.project and no INCOMPLETE row in centry.project_deletions (an
// open journal row drops its database itself). Only names that are exactly
// project_<id> or project_<id>_user are read; nothing else on the server is
// listed or touched. Each id is checked again against both tables right
// before its drop, since the listing can be minutes old.
//
// SHARED-SERVER RISK. "No project row" is judged against the ONE platform
// database DATABASE_URL names. If that is the wrong database, or the legacy
// server is shared with another platform, other deployments' databases look
// like orphans. So the command prints the platform database and its project
// count and id range first, refuses a platform database with no projects,
// refuses --drop when orphans are more than --max-orphan-fraction of every
// project_<id> on the server (unless --force-fraction), and --drop requires
// --confirm-count equal to the number of orphans it finds, so a drop is always
// of the list the operator just read.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacypgvector"
)

const (
	legacyOrphansCommand          = "legacy-pgvector-orphans"
	legacyOrphansRunTimeout       = 30 * time.Minute
	legacyOrphansDefaultMaxShare  = 0.2
	legacyOrphansExitOK           = 0
	legacyOrphansExitFailure      = 1
	legacyOrphansExitInvalidUsage = 2
)

func runLegacyPgvectorOrphans(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	return runLegacyOrphans(ctx, args, lookup, stdout, stderr, nil)
}

// runLegacyOrphans is the command. scope, when not nil, limits it to the ids
// it accepts: a test shares its PostgreSQL server with other packages' tests,
// whose project_<id> databases it must neither count nor drop. The command
// itself passes nil and sees the whole server.
func runLegacyOrphans(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, scope func(int64) bool) int {
	flags := flag.NewFlagSet(legacyOrphansCommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	drop := flags.Bool("drop", false, "drop the orphans (default: dry run, list only)")
	confirmCount := flags.Int("confirm-count", -1, "with --drop: the number of orphans the dry run listed; the drop is refused if the count differs")
	maxShare := flags.Float64("max-orphan-fraction", legacyOrphansDefaultMaxShare,
		"refuse --drop when orphans are more than this fraction of every project_<id> on the legacy server: "+
			"a high fraction means a wrong DATABASE_URL or a server shared with another platform")
	forceShare := flags.Bool("force-fraction", false, "allow --drop although the orphan fraction exceeds --max-orphan-fraction")
	flags.Usage = func() {
		orphansSay(stderr, "usage: elitea-main %s [--drop --confirm-count N]\n", legacyOrphansCommand)
		orphansSay(stderr, "reads %s (the legacy PgVector server) and DATABASE_URL (the platform database)\n\n", legacyPgvectorAdminURLEnv)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return legacyOrphansExitInvalidUsage
	}
	if *maxShare < 0 || *maxShare > 1 {
		orphansSayln(stderr, "--max-orphan-fraction must be between 0 and 1")
		return legacyOrphansExitInvalidUsage
	}
	if !*drop && *confirmCount >= 0 {
		orphansSayln(stderr, "--confirm-count only applies together with --drop")
		return legacyOrphansExitInvalidUsage
	}
	adminURL, _ := lookup(legacyPgvectorAdminURLEnv)
	platformURL, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(adminURL) == "" || strings.TrimSpace(platformURL) == "" {
		orphansSay(stderr, "both %s and DATABASE_URL are required\n", legacyPgvectorAdminURLEnv)
		return legacyOrphansExitInvalidUsage
	}
	dropper, err := legacypgvector.NewDropper(adminURL)
	if err != nil {
		orphansSay(stderr, "%s: %v\n", legacyPgvectorAdminURLEnv, err)
		return legacyOrphansExitInvalidUsage
	}

	ctx, cancel := context.WithTimeout(ctx, legacyOrphansRunTimeout)
	defer cancel()
	platform, err := pgx.Connect(ctx, platformURL)
	if err != nil {
		// pgx's error can quote the DSN; say what failed, not with what.
		orphansSayln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return legacyOrphansExitFailure
	}
	defer func() { _ = platform.Close(context.WithoutCancel(ctx)) }()

	var (
		platformDatabase string
		projectCount     int64
		minID, maxID     *int64
	)
	if err := platform.QueryRow(ctx,
		`SELECT current_database(), count(*), min(id)::bigint, max(id)::bigint FROM centry.project`,
	).Scan(&platformDatabase, &projectCount, &minID, &maxID); err != nil {
		orphansSay(stderr, "read centry.project: %v\n", err)
		return legacyOrphansExitFailure
	}
	orphansSay(stdout, "platform database: %s, %d project(s)", platformDatabase, projectCount)
	if minID != nil && maxID != nil {
		orphansSay(stdout, ", ids %d..%d", *minID, *maxID)
	}
	orphansSay(stdout, "\nlegacy PgVector server: %s (admin database %s)\n", dropper.AdminHost(), dropper.AdminDatabase())
	if projectCount == 0 {
		orphansSayln(stderr, "refusing: the platform database has no projects, so every legacy database would look orphaned. Check DATABASE_URL")
		return legacyOrphansExitFailure
	}

	leftovers, err := dropper.List(ctx)
	if err != nil {
		orphansSay(stderr, "list the legacy server: %v\n", err)
		return legacyOrphansExitFailure
	}
	if scope != nil {
		leftovers = slices.DeleteFunc(leftovers, func(leftover legacypgvector.Leftover) bool { return !scope(leftover.ProjectID) })
	}
	var orphans []legacypgvector.Leftover
	for _, leftover := range leftovers {
		owned, err := legacyIDOwned(ctx, platform, leftover.ProjectID)
		if err != nil {
			orphansSay(stderr, "check project %d: %v\n", leftover.ProjectID, err)
			return legacyOrphansExitFailure
		}
		if !owned {
			orphans = append(orphans, leftover)
		}
	}
	orphansSay(stdout, "%d project id(s) with a legacy database or role, %d orphaned\n", len(leftovers), len(orphans))
	for _, orphan := range orphans {
		orphansSay(stdout, "  project %d: %s\n", orphan.ProjectID, strings.Join(nonEmpty(orphan.Database, orphan.Role), ", "))
	}
	if !*drop {
		if len(orphans) > 0 {
			orphansSay(stdout, "\ndry run: nothing was dropped. To drop, re-run with --drop --confirm-count %d.\n", len(orphans))
		}
		return legacyOrphansExitOK
	}
	if len(orphans) == 0 {
		orphansSayln(stdout, "nothing to drop")
		return legacyOrphansExitOK
	}
	if *confirmCount != len(orphans) {
		orphansSay(stderr, "refusing to drop: --confirm-count is %d, but %d orphan(s) are listed above. Read the list, then pass its count\n", *confirmCount, len(orphans))
		return legacyOrphansExitInvalidUsage
	}
	if share := float64(len(orphans)) / float64(len(leftovers)); share > *maxShare && !*forceShare {
		orphansSay(stderr, "refusing to drop: %.0f%% of the legacy databases are orphans (limit %.0f%%), which is what a wrong DATABASE_URL or a shared server looks like. Check the header, then pass --force-fraction\n",
			share*100, *maxShare*100)
		return legacyOrphansExitInvalidUsage
	}

	failed := 0
	for _, orphan := range orphans {
		// The listing is minutes old: a project created since (its id reused,
		// a restored database) or a delete whose journal row opened since owns
		// the database now.
		owned, err := legacyIDOwned(ctx, platform, orphan.ProjectID)
		if err != nil || owned {
			orphansSay(stdout, "  project %d: skipped (no longer an orphan, or the check failed)\n", orphan.ProjectID)
			if err != nil {
				failed++
			}
			continue
		}
		dropped, err := dropper.Drop(ctx, orphan.ProjectID)
		if err != nil {
			failed++
			orphansSay(stdout, "  project %d: FAILED: %v\n", orphan.ProjectID, err)
			continue
		}
		orphansSay(stdout, "  project %d: %s\n", orphan.ProjectID, dropped.Summary())
	}
	if failed > 0 {
		orphansSay(stderr, "%d drop(s) failed; re-run to retry them\n", failed)
		return legacyOrphansExitFailure
	}
	return legacyOrphansExitOK
}

// legacyIDOwned reports whether something still owns the id's legacy database:
// a project row, or an open cleanup journal row (which drops it itself).
func legacyIDOwned(ctx context.Context, platform *pgx.Conn, projectID int64) (bool, error) {
	var owned bool
	err := platform.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)
    OR EXISTS (SELECT 1 FROM centry.project_deletions WHERE project_id = $1 AND completed_at IS NULL)`,
		projectID).Scan(&owned)
	if err != nil {
		return false, errors.Join(errors.New("read centry.project and centry.project_deletions"), err)
	}
	return owned, nil
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// orphansSay and orphansSayln write the command's output. A write that fails
// (a closed pipe) has nowhere else to be reported, so it is dropped.
func orphansSay(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func orphansSayln(w io.Writer, args ...any) { _, _ = fmt.Fprintln(w, args...) }
