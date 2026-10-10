// Command pgvector-orphans finds, and optionally drops, per-project PgVector
// databases and roles whose project no longer exists (#1211).
//
// Before #1211 deleting a project never dropped project_<id> or
// project_<id>_user, so every deleted project left both behind. This lists them.
//
// It is a DRY RUN unless --drop is given, and --drop does nothing until the
// operator repeats the exact names to be removed with --confirm. The drop is
// pgvector.Provisioner.Drop, the same operation a project delete runs, so the
// name rules and the quoting are the ones the delete path uses.
//
// Usage:
//
//	pgvector-orphans --pgvector-url <admin-url> --database-url <platform-url>
//	pgvector-orphans ... --drop --confirm project_12,project_12_user,project_13
//
// The URLs default to PGVECTOR_ADMIN_URL and DATABASE_URL. A name is an orphan
// only if it is EXACTLY project_<id> or project_<id>_user for a canonical
// positive int4 id with no row in centry.project; nothing else on the server is
// ever listed or touched.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/jackc/pgx/v5"
)

const (
	exitOK           = 0
	exitFailure      = 1
	exitInvalidUsage = 2
	runTimeout       = 10 * time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// orphan is one project id whose database and/or role outlived its project.
type orphan struct {
	ProjectID int64
	Database  string // empty when the database is already gone
	Role      string // empty when the role is already gone
}

func (o orphan) names() []string {
	var names []string
	if o.Database != "" {
		names = append(names, o.Database)
	}
	if o.Role != "" {
		names = append(names, o.Role)
	}
	return names
}

func run(
	ctx context.Context,
	args []string,
	lookup func(string) (string, bool),
	stdout, stderr io.Writer,
) int {
	flags := flag.NewFlagSet("pgvector-orphans", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pgvectorURL := flags.String("pgvector-url", "", "PgVector bootstrap admin URL (default $PGVECTOR_ADMIN_URL)")
	databaseURL := flags.String("database-url", "", "platform database URL, to read centry.project (default $DATABASE_URL)")
	drop := flags.Bool("drop", false, "drop the orphans named by --confirm (default: dry run)")
	confirm := flags.String("confirm", "", "comma-separated exact names to drop; must list every name of every orphan to be dropped")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitInvalidUsage
	}
	if *pgvectorURL == "" {
		*pgvectorURL, _ = lookup("PGVECTOR_ADMIN_URL")
	}
	if *databaseURL == "" {
		*databaseURL, _ = lookup("DATABASE_URL")
	}
	if *pgvectorURL == "" || *databaseURL == "" {
		sayln(stderr, "both --pgvector-url (PGVECTOR_ADMIN_URL) and --database-url (DATABASE_URL) are required")
		return exitInvalidUsage
	}
	if *confirm != "" && !*drop {
		sayln(stderr, "--confirm only applies together with --drop")
		return exitInvalidUsage
	}

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	adminConfig, err := pgx.ParseConfig(mustNormalize(*pgvectorURL))
	if err != nil {
		sayln(stderr, "pgvector admin URL is not a valid PostgreSQL URL")
		return exitInvalidUsage
	}
	orphans, err := findOrphans(ctx, adminConfig, *databaseURL)
	if err != nil {
		say(stderr, "list orphans: %v\n", redact(err))
		return exitFailure
	}

	printOrphans(stdout, orphans)
	if !*drop {
		if len(orphans) > 0 {
			sayln(stdout, "\ndry run: nothing was dropped. To drop, re-run with --drop --confirm <the exact names above, comma-separated>.")
		}
		return exitOK
	}

	selected, err := selectConfirmed(orphans, splitNames(*confirm))
	if err != nil {
		say(stderr, "refusing to drop: %v\n", err)
		return exitInvalidUsage
	}
	if len(selected) == 0 {
		sayln(stdout, "nothing to drop")
		return exitOK
	}
	connector, err := pgvector.NewPGXConnector(adminConfig)
	if err != nil {
		sayln(stderr, "pgvector connector is invalid")
		return exitFailure
	}
	provisioner, err := pgvector.NewProvisioner(connector)
	if err != nil {
		sayln(stderr, "pgvector provisioner is invalid")
		return exitFailure
	}
	admin := pgvector.AdminConnection{Database: adminConfig.Database}
	failed := 0
	for _, o := range selected {
		// Database/Role echo the exact confirmed names; Drop re-derives and
		// refuses a mismatch.
		result, err := provisioner.Drop(ctx, pgvector.DropRequest{
			ProjectID: o.ProjectID, Admin: admin, Mode: pgvector.ModeDatabaseRole,
			Database: o.Database, Role: o.Role,
		})
		if err != nil {
			failed++
			say(stderr, "project %d: drop failed: %v\n", o.ProjectID, redact(err))
			continue
		}
		say(stdout, "project %d: database dropped=%v role dropped=%v\n",
			o.ProjectID, result.DatabaseDropped, result.RoleDropped)
	}
	if failed > 0 {
		return exitFailure
	}
	return exitOK
}

// say and sayln write operator output; a failed write to a closed pipe has
// nowhere better to be reported.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func sayln(w io.Writer, args ...any) { _, _ = fmt.Fprintln(w, args...) }

func mustNormalize(raw string) string {
	if dsn, ok := pgvector.NormalizeConnectionString(raw); ok {
		return dsn
	}
	return raw
}

// redact keeps a connection string, which could carry a password, out of output.
func redact(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return fmt.Errorf("postgres error %s", pgErr.SQLState())
	}
	return errors.New("operation failed")
}

func findOrphans(ctx context.Context, adminConfig *pgx.ConnConfig, databaseURL string) ([]orphan, error) {
	platform, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, errors.New("connect to platform database")
	}
	defer func() { _ = platform.Close(context.Background()) }()
	rows, err := platform.Query(ctx, `SELECT id FROM centry.project`)
	if err != nil {
		return nil, err
	}
	projects := map[int64]struct{}{}
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		projects[int64(id)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		return nil, errors.New("connect to pgvector admin database")
	}
	defer func() { _ = admin.Close(context.Background()) }()
	databases, err := queryNames(ctx, admin, `SELECT datname FROM pg_catalog.pg_database WHERE datname LIKE 'project\_%'`)
	if err != nil {
		return nil, err
	}
	roles, err := queryNames(ctx, admin, `SELECT rolname FROM pg_catalog.pg_roles WHERE rolname LIKE 'project\_%'`)
	if err != nil {
		return nil, err
	}
	return computeOrphans(projects, databases, roles), nil
}

func queryNames(ctx context.Context, conn *pgx.Conn, query string) ([]string, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// computeOrphans keeps only names that are exactly project_<id> /
// project_<id>_user for a canonical id that has no project row.
func computeOrphans(projects map[int64]struct{}, databases, roles []string) []orphan {
	byID := map[int64]*orphan{}
	entry := func(id int64) *orphan {
		if byID[id] == nil {
			byID[id] = &orphan{ProjectID: id}
		}
		return byID[id]
	}
	for _, name := range databases {
		if id, ok := pgvector.ParseProjectDatabaseName(name); ok {
			if _, live := projects[id]; !live {
				entry(id).Database = name
			}
		}
	}
	for _, name := range roles {
		if id, ok := pgvector.ParseProjectRoleName(name); ok {
			if _, live := projects[id]; !live {
				entry(id).Role = name
			}
		}
	}
	result := make([]orphan, 0, len(byID))
	for _, o := range byID {
		result = append(result, *o)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ProjectID < result[j].ProjectID })
	return result
}

// selectConfirmed returns the orphans whose every name was confirmed. Any
// confirmed name that is not a current orphan name is an error, so a typo or a
// stale list cannot drop something the operator did not mean.
func selectConfirmed(orphans []orphan, confirmed []string) ([]orphan, error) {
	if len(confirmed) == 0 {
		return nil, errors.New("--drop requires --confirm with the exact names to drop")
	}
	known := map[string]int64{}
	for _, o := range orphans {
		for _, n := range o.names() {
			known[n] = o.ProjectID
		}
	}
	confirmedSet := map[string]struct{}{}
	for _, n := range confirmed {
		if _, ok := known[n]; !ok {
			return nil, fmt.Errorf("%q is not a current orphan", n)
		}
		confirmedSet[n] = struct{}{}
	}
	var selected []orphan
	for _, o := range orphans {
		any, all := false, true
		for _, n := range o.names() {
			if _, ok := confirmedSet[n]; ok {
				any = true
			} else {
				all = false
			}
		}
		if any && !all {
			return nil, fmt.Errorf("project %d: confirm every name (%s); a project is dropped as a whole",
				o.ProjectID, strings.Join(o.names(), ", "))
		}
		if all && any {
			selected = append(selected, o)
		}
	}
	return selected, nil
}

func splitNames(value string) []string {
	var names []string
	for _, n := range strings.Split(value, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func printOrphans(w io.Writer, orphans []orphan) {
	if len(orphans) == 0 {
		sayln(w, "no orphaned project_* databases or roles")
		return
	}
	say(w, "%d orphaned project(s) (no centry.project row):\n", len(orphans))
	for _, o := range orphans {
		say(w, "  project %d: %s\n", o.ProjectID, strings.Join(o.names(), ", "))
	}
}
