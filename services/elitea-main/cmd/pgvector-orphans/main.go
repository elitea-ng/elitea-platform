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
// The URLs default to PGVECTOR_ADMIN_URL and DATABASE_URL. An admin URL that
// names no database connects to `postgres`. A name is an orphan only if it is
// EXACTLY project_<id> or project_<id>_user for a canonical positive int4 id
// with no row in centry.project and no incomplete row in the project-delete
// cleanup journal (centry.project_deletions): a deleted project whose cleanup is
// still pending belongs to the journal, which drops its database itself. Nothing
// else on the server is ever listed or touched. Each id is checked again,
// against both tables, immediately before its drop.
//
// SHARED-SERVER RISK. "No row in centry.project" is judged against the one
// platform database named by --database-url. If that URL points at the wrong
// database, or the PgVector server is SHARED with another platform or
// environment, every project_<id> database that belongs to someone else looks
// like an orphan, and --drop would destroy it irreversibly. So the tool prints
// the platform database it is reading (with current_database()) and its project
// count and id range before listing; refuses to run against a platform database
// with zero projects; and refuses --drop when the orphan fraction exceeds
// --max-orphan-fraction (default 0.2) unless --force-fraction is given. Read the
// header lines, and check them against the environment you mean to clean.
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
	maxFraction := flags.Float64("max-orphan-fraction", defaultMaxOrphanFraction,
		"refuse --drop when orphans are more than this fraction of all project_<id> databases on the server: "+
			"a high fraction means a wrong --database-url or a PgVector server shared with another platform")
	forceFraction := flags.Bool("force-fraction", false, "allow --drop although the orphan fraction exceeds --max-orphan-fraction")
	flags.Usage = func() {
		sayln(stderr, "usage: pgvector-orphans --pgvector-url URL --database-url URL [--drop --confirm NAMES]")
		sayln(stderr, "\nWARNING: orphans are judged against ONE platform database. If --database-url is wrong, or the")
		sayln(stderr, "PgVector server is shared with another platform, other deployments' project_<id> databases look")
		sayln(stderr, "like orphans and --drop destroys them irreversibly. Check the header the tool prints first.")
		sayln(stderr)
		flags.PrintDefaults()
	}
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
		say(stderr, "%v\n", redact(errParseAdminURL))
		return exitInvalidUsage
	}
	if adminConfig.Database == "" {
		// A server URL with no database: connect where every server has one.
		// The drop runs from this database, so it is never a project_<id>.
		adminConfig.Database = defaultAdminDatabase
	}
	if *maxFraction < 0 || *maxFraction > 1 {
		sayln(stderr, "--max-orphan-fraction must be between 0 and 1")
		return exitInvalidUsage
	}
	platform, err := connectPlatform(ctx, *databaseURL)
	if err != nil {
		say(stderr, "list orphans: %v\n", redact(err))
		return exitFailure
	}
	defer func() { _ = platform.Close(context.Background()) }()
	report, err := findOrphans(ctx, platform, adminConfig)
	if err != nil {
		say(stderr, "list orphans: %v\n", redact(err))
		return exitFailure
	}
	report.PlatformURLDatabase = platform.Config().Database
	orphans := report.Orphans

	printHeader(stdout, report)
	if report.ProjectCount == 0 {
		say(stderr, "%v\n", redact(errNoProjects))
		return exitFailure
	}
	printOrphans(stdout, orphans)
	if !*drop {
		if len(orphans) > 0 {
			sayln(stdout, "\ndry run: nothing was dropped. To drop, re-run with --drop --confirm <the exact names above, comma-separated>.")
		}
		return exitOK
	}

	if fraction := report.orphanFraction(); fraction > *maxFraction && !*forceFraction {
		say(stderr, "%v (%.0f%% are orphans, limit %.0f%%)\n", redact(errOrphanFraction), fraction*100, *maxFraction*100)
		return exitInvalidUsage
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
		beforeDrop(o)
		// The listing is minutes old by now. A project created since (its id
		// reused by nothing, but an operator can restore a row) or a delete
		// whose cleanup journal now owns the id must not be dropped from here.
		if reason, err := stillOrphaned(ctx, platform, o.ProjectID); err != nil {
			failed++
			say(stderr, "project %d: not dropped, the re-check failed: %v\n", o.ProjectID, redact(err))
			continue
		} else if reason != "" {
			say(stdout, "project %d: skipped, %s\n", o.ProjectID, reason)
			continue
		}
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

// The refusals and failures this command reports. Each message is fixed text:
// none carries a URL, a host or a credential, so redact can pass them on.
var (
	errParsePlatformURL = errors.New("cannot parse --database-url")
	errParseAdminURL    = errors.New("cannot parse --pgvector-url")
	errConnectPlatform  = errors.New("cannot connect to the platform database")
	errConnectAdmin     = errors.New("cannot connect to the PgVector admin database")
	errNoProjects       = errors.New("refusing to run: the platform database has no projects, so every project_<id> database " +
		"would look orphaned; check --database-url")
	errOrphanFraction = errors.New("refusing to drop: too large a share of the project_<id> databases on this server are " +
		"orphans. That usually means a wrong --database-url or a PgVector server shared with another platform. " +
		"If it is really right, re-run with --force-fraction")
)

// defaultAdminDatabase is the database an admin URL with none connects to.
const defaultAdminDatabase = "postgres"

// beforeDrop runs before each drop's re-check. A test hook: production leaves it
// empty.
var beforeDrop = func(orphan) {}

// safeMessages maps the sentinels this command can receive to the text shown
// for them. A sentinel is matched with errors.Is, so a wrapped one still maps.
var safeMessages = []struct {
	target error
	text   string
}{
	{errParsePlatformURL, errParsePlatformURL.Error()},
	{errParseAdminURL, errParseAdminURL.Error()},
	{errConnectPlatform, errConnectPlatform.Error()},
	{errConnectAdmin, errConnectAdmin.Error()},
	{errNoProjects, errNoProjects.Error()},
	{errOrphanFraction, errOrphanFraction.Error()},
	{pgvector.ErrDropLockTimeout, "timed out waiting for the project's PgVector advisory lock; a provision or another drop holds it, retry later"},
	{pgvector.ErrInvalidDropTarget, "refusing to drop a name that is not the project's own database or role"},
	{pgvector.ErrInvalidRequest, "invalid drop request: check that --pgvector-url names the admin database and the id is a project id"},
	{pgvector.ErrInvalidConnector, "the PgVector admin connection is not usable"},
	{pgvector.ErrProvisioning, "a statement on the PgVector server failed; see the server log"},
}

// redact turns an error into text that is safe to print: a connection string
// could carry a password, and the driver's errors repeat it.
//
//   - a known sentinel maps to its fixed message;
//   - a context error is printed as it is (it carries nothing);
//   - a raw Postgres error is reduced to its SQLSTATE, so the operator can look
//     the code up without the message, which can quote an identifier or a host;
//   - anything else is "operation failed".
func redact(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range safeMessages {
		if errors.Is(err, known.target) {
			return errors.New(known.text)
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return fmt.Errorf("postgres error %s", pgErr.SQLState())
	}
	return errors.New("operation failed")
}

const defaultMaxOrphanFraction = 0.2

// report is what one run learned about the platform database and the server.
type report struct {
	PlatformURLDatabase string // the database name in --database-url
	CurrentDatabase     string // current_database() on that connection
	AdminDatabase       string // the database the PgVector admin connects to
	// JournalOwned are project ids with a project_* name and an incomplete
	// cleanup-journal row: their delete drops them, so they are not orphans.
	JournalOwned []int64
	ProjectCount int
	MinProjectID int64
	MaxProjectID int64
	Orphans      []orphan
	// ServerProjects is the number of distinct project ids that own a project_*
	// database or role on the PgVector server, live or orphaned.
	ServerProjects int
}

// orphanFraction is orphans over every project id found on the server.
func (r report) orphanFraction() float64 {
	if r.ServerProjects == 0 {
		return 0
	}
	return float64(len(r.Orphans)) / float64(r.ServerProjects)
}

func printHeader(w io.Writer, r report) {
	say(w, "platform database: %s (current_database() = %s)\n", r.PlatformURLDatabase, r.CurrentDatabase)
	say(w, "centry.project: %d project(s), id range %d..%d\n", r.ProjectCount, r.MinProjectID, r.MaxProjectID)
	say(w, "pgvector server: admin database %s; %d project id(s) with a project_* database or role; %d orphaned (%.0f%%)\n",
		r.AdminDatabase, r.ServerProjects, len(r.Orphans), r.orphanFraction()*100)
	if len(r.JournalOwned) > 0 {
		say(w, "left to the project-delete cleanup journal (not orphans): %v\n", r.JournalOwned)
	}
}

// connectPlatform opens the platform database connection the listing and the
// pre-drop re-checks read.
func connectPlatform(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	platformConfig, err := pgx.ParseConfig(mustNormalize(databaseURL))
	if err != nil {
		return nil, errParsePlatformURL
	}
	platform, err := pgx.ConnectConfig(ctx, platformConfig)
	if err != nil {
		return nil, errConnectPlatform
	}
	return platform, nil
}

// journalPresent reports whether the platform database has the project-delete
// cleanup journal (shared/0161). An older platform has none, and then no id is
// journal-owned.
func journalPresent(ctx context.Context, platform *pgx.Conn) (bool, error) {
	var present bool
	err := platform.QueryRow(ctx, `SELECT to_regclass('centry.project_deletions') IS NOT NULL`).Scan(&present)
	return present, err
}

// stillOrphaned re-checks one id against the platform database right before
// its drop. It answers "" when the id is still an orphan, or why it is not.
func stillOrphaned(ctx context.Context, platform *pgx.Conn, projectID int64) (string, error) {
	var hasRow bool
	if err := platform.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID).Scan(&hasRow); err != nil {
		return "", err
	}
	if hasRow {
		return "it has a centry.project row now", nil
	}
	journal, err := journalPresent(ctx, platform)
	if err != nil {
		return "", err
	}
	if !journal {
		return "", nil
	}
	var pending bool
	if err := platform.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project_deletions WHERE project_id = $1 AND completed_at IS NULL)`,
		projectID).Scan(&pending); err != nil {
		return "", err
	}
	if pending {
		return "its delete has cleanup pending in centry.project_deletions, which drops it", nil
	}
	return "", nil
}

func findOrphans(ctx context.Context, platform *pgx.Conn, adminConfig *pgx.ConnConfig) (report, error) {
	var rep report
	rep.AdminDatabase = adminConfig.Database
	if err := platform.QueryRow(ctx, `SELECT current_database()`).Scan(&rep.CurrentDatabase); err != nil {
		return rep, err
	}
	rows, err := platform.Query(ctx, `SELECT id FROM centry.project`)
	if err != nil {
		return rep, err
	}
	projects := map[int64]struct{}{}
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return rep, err
		}
		projects[int64(id)] = struct{}{}
		if len(projects) == 1 || int64(id) < rep.MinProjectID {
			rep.MinProjectID = int64(id)
		}
		if int64(id) > rep.MaxProjectID {
			rep.MaxProjectID = int64(id)
		}
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	rep.ProjectCount = len(projects)

	// Ids whose delete is still cleaning up belong to the cleanup journal.
	journalOwned := map[int64]struct{}{}
	if present, err := journalPresent(ctx, platform); err != nil {
		return rep, err
	} else if present {
		pending, err := platform.Query(ctx, `SELECT project_id FROM centry.project_deletions WHERE completed_at IS NULL`)
		if err != nil {
			return rep, err
		}
		ids, err := pgx.CollectRows(pending, pgx.RowTo[int64])
		if err != nil {
			return rep, err
		}
		for _, id := range ids {
			journalOwned[id] = struct{}{}
		}
	}

	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		return rep, errConnectAdmin
	}
	defer func() { _ = admin.Close(context.Background()) }()
	databases, err := queryNames(ctx, admin, `SELECT datname FROM pg_catalog.pg_database WHERE datname LIKE 'project\_%'`)
	if err != nil {
		return rep, err
	}
	roles, err := queryNames(ctx, admin, `SELECT rolname FROM pg_catalog.pg_roles WHERE rolname LIKE 'project\_%'`)
	if err != nil {
		return rep, err
	}
	rep.Orphans, rep.JournalOwned = computeOrphans(projects, journalOwned, databases, roles)
	rep.ServerProjects = serverProjectCount(databases, roles)
	return rep, nil
}

// serverProjectCount is the number of distinct canonical project ids named by
// the project_* databases and roles, whether or not the project has a row.
func serverProjectCount(databases, roles []string) int {
	ids := map[int64]struct{}{}
	for _, name := range databases {
		if id, ok := pgvector.ParseProjectDatabaseName(name); ok {
			ids[id] = struct{}{}
		}
	}
	for _, name := range roles {
		if id, ok := pgvector.ParseProjectRoleName(name); ok {
			ids[id] = struct{}{}
		}
	}
	return len(ids)
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
// project_<id>_user for a canonical id that has no project row. Ids the cleanup
// journal owns are returned apart (journalOwned), never as orphans.
func computeOrphans(projects, journal map[int64]struct{}, databases, roles []string) (orphans []orphan, journalOwned []int64) {
	byID := map[int64]*orphan{}
	owned := map[int64]struct{}{}
	entry := func(id int64) *orphan {
		if byID[id] == nil {
			byID[id] = &orphan{ProjectID: id}
		}
		return byID[id]
	}
	consider := func(id int64) bool {
		if _, live := projects[id]; live {
			return false
		}
		if _, pending := journal[id]; pending {
			owned[id] = struct{}{}
			return false
		}
		return true
	}
	for _, name := range databases {
		if id, ok := pgvector.ParseProjectDatabaseName(name); ok && consider(id) {
			entry(id).Database = name
		}
	}
	for _, name := range roles {
		if id, ok := pgvector.ParseProjectRoleName(name); ok && consider(id) {
			entry(id).Role = name
		}
	}
	orphans = make([]orphan, 0, len(byID))
	for _, o := range byID {
		orphans = append(orphans, *o)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].ProjectID < orphans[j].ProjectID })
	for id := range owned {
		journalOwned = append(journalOwned, id)
	}
	sort.Slice(journalOwned, func(i, j int) bool { return journalOwned[i] < journalOwned[j] })
	return orphans, journalOwned
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
