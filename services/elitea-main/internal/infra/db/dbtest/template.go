// Package dbtest builds one migrated PostgreSQL template database and copies
// it for each integration test.
//
// Before this package every test that needed a migrated database replayed the
// full ledgered migration corpus into a new database. One replay costs about
// 1.5 s to 3 s. The repository suite did 78 of them in one package run, which
// pushed the package past the 600 s Go default timeout and made slow runs look
// like migration defects (#409, #425).
//
// The template holds the same schema, so a test gets its database from
// "CREATE DATABASE ... TEMPLATE ...". PostgreSQL copies the template files,
// which is much cheaper than a replay, and each test still gets a private
// database.
//
// The template is built from the ledgered migration corpus with the same
// migrate.Runner that a deployment uses. It is never built from a captured
// dump. A dump can drift away from the corpus, and a test database that does
// not match a deployment is worse than a slow one.
//
// The template name carries a fingerprint of everything that decides the
// schema: the scope, version, path, and SHA-256 checksum of every migration,
// the seed SQL, and the tenant list. A new migration therefore makes a new
// template name, and the old template can never be used by mistake.
//
// # Lifetime
//
// The template stays on the server after the package ends. This is deliberate.
// `go test ./...` starts one process for each package, so a template that dies
// with its package would be rebuilt by every package that wants one. A kept
// template is built one time for a whole workspace run, and again never on a
// developer machine until a migration lands.
//
// A CI database container is new for each job, so nothing accumulates there. On
// a developer machine one template of about 10 MB stays for each migration
// corpus. Remove the old ones with:
//
//	psql -Atc "SELECT 'DROP DATABASE ' || quote_ident(datname) || ';'
//	           FROM pg_database WHERE starts_with(datname, 'elitea_tmpl_')" | psql
package dbtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
)

// buildDeadline bounds one template build: the scratch database, the seed, the
// migration replay, and the rename. The build happens one time for each
// fingerprint, so this deadline is not on the path of an ordinary test.
const buildDeadline = 180 * time.Second

// staleScratchAge is the age at which a scratch database counts as the residue
// of a build that died. It must stay well above buildDeadline, because a build
// that still runs also owns a scratch database.
const staleScratchAge = 15 * time.Minute

// templateLockKey derives the advisory lock that serializes the build of ONE
// template. Two packages that want different templates build at the same time.
//
// PostgreSQL advisory locks belong to the database that took them. Every
// caller connects to the maintenance database in ELITEA_TEST_DATABASE_URL, so
// every caller shares one lock namespace. The migration runner takes its own
// advisory locks inside the database it migrates, which is a different
// namespace, so the two cannot block each other.
func templateLockKey(templateName string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("elitea-platform:dbtest:template:"))
	_, _ = hash.Write([]byte(templateName))
	return int64(hash.Sum64()) // PostgreSQL advisory locks use the bit pattern.
}

// namePrefix marks the finished templates. buildPrefix marks the scratch
// databases that a build writes into before it renames them.
const (
	namePrefix  = "elitea_tmpl_"
	buildPrefix = "elitea_tmplbuild_"
)

// Spec describes one template variant. Two specs that produce the same
// fingerprint produce the same schema, so they share one template database.
type Spec struct {
	// Files is the ledgered migration corpus. Give the embedded corpus, not a
	// dump.
	Files fs.FS
	// Seed is the SQL that runs before the migrations. The legacy project
	// schemas that the tenant history alters live here.
	Seed string
	// Tenants lists the project IDs whose tenant history must be applied. The
	// seed must create the matching centry.project rows and p_<id> schemas.
	Tenants []int64
}

// Fingerprint returns the content identity of the schema that a build
// produces. Two specs with the same fingerprint give the same schema.
func (s Spec) Fingerprint() (string, error) {
	material, err := s.fingerprintMaterial()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:12]), nil
}

// fingerprintMaterial writes every input that decides the schema.
func (s Spec) fingerprintMaterial() (string, error) {
	var material strings.Builder
	// strings.Builder never fails, so the error is discarded on purpose.
	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(&material, format, args...)
	}

	// A version tag lets a later change to the build steps invalidate every
	// template that an older revision left on a developer machine.
	write("elitea-dbtest-template-v1\n")

	for _, scope := range []migrate.Scope{migrate.ScopeShared, migrate.ScopeTenant} {
		manifest, err := migrate.LoadManifest(s.Files, scope)
		if err != nil {
			return "", fmt.Errorf("dbtest: fingerprint %s history: %w", scope, err)
		}
		write("scope %s count %d\n", scope, len(manifest))
		for _, migration := range manifest {
			write("%s\t%d\t%s\t%s\n",
				migration.Scope, migration.Version, migration.Path,
				hex.EncodeToString(migration.Checksum[:]))
		}
	}

	tenants := append([]int64(nil), s.Tenants...)
	sort.Slice(tenants, func(i, j int) bool { return tenants[i] < tenants[j] })
	write("tenants %d\n", len(tenants))
	for _, tenant := range tenants {
		write("tenant %d\n", tenant)
	}

	write("seed %d\n", len(s.Seed))
	material.WriteString(s.Seed)
	return material.String(), nil
}

// TemplateName returns the database name that holds this spec's schema.
func (s Spec) TemplateName() (string, error) {
	fingerprint, err := s.Fingerprint()
	if err != nil {
		return "", err
	}
	return namePrefix + fingerprint, nil
}

var (
	ensured   = map[string]struct{}{}
	ensuredMu sync.Mutex
)

// EnsureTemplate makes sure a template database for spec exists, and returns
// its name. It is safe for concurrent processes: the build runs under a
// PostgreSQL advisory lock, and the caller that loses the race finds the
// finished template.
func EnsureTemplate(ctx context.Context, adminPool *pgxpool.Pool, spec Spec) (string, error) {
	templateName, err := spec.TemplateName()
	if err != nil {
		return "", err
	}

	ensuredMu.Lock()
	defer ensuredMu.Unlock()
	if _, done := ensured[templateName]; done {
		return templateName, nil
	}

	exists, err := databaseExists(ctx, adminPool, templateName)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := buildTemplate(ctx, adminPool, spec, templateName); err != nil {
			return "", err
		}
	}
	ensured[templateName] = struct{}{}
	return templateName, nil
}

// buildTemplate creates the template under the advisory lock.
//
// The lock is transaction-scoped, held on a dedicated pooled connection for
// the life of the build: a session-scoped lock can outlive the process that
// took it. Once the owning process dies, a pooler returns the locked server
// connection to its pool, and the lock sits there until the server connection
// is recycled, blocking every template build that goes through the pooler.
func buildTemplate(ctx context.Context, adminPool *pgxpool.Pool, spec Spec, templateName string) error {
	lockKey := templateLockKey(templateName)
	lockConn, err := adminPool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("dbtest: acquire lock connection: %w", err)
	}
	defer lockConn.Release()
	lockTx, err := lockConn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("dbtest: begin lock transaction: %w", err)
	}
	// Roll back before releasing the connection: the transaction end is what
	// releases the lock.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = lockTx.Rollback(unlockCtx)
	}()
	if _, err := lockTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
		return fmt.Errorf("dbtest: take template build lock: %w", err)
	}

	// Another process may have finished the build while this one waited.
	exists, err := databaseExists(ctx, adminPool, templateName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	dropAbandonedScratchDatabases(ctx, adminPool)

	scratchName := fmt.Sprintf("%s%d_%d", buildPrefix, os.Getpid(), time.Now().UnixNano())
	quotedScratch := pgx.Identifier{scratchName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedScratch); err != nil {
		return fmt.Errorf("dbtest: create template build database: %w", err)
	}
	dropScratch := func() {
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(dropCtx, "DROP DATABASE "+quotedScratch+" WITH (FORCE)")
	}

	if err := applySchema(ctx, adminPool, scratchName, spec); err != nil {
		dropScratch()
		return err
	}

	// The rename needs the scratch database to be free of sessions. applySchema
	// closes its pool, but a backend can take a moment to exit.
	if err := waitForNoConnections(ctx, adminPool, scratchName); err != nil {
		dropScratch()
		return err
	}
	quotedTemplate := pgx.Identifier{templateName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "ALTER DATABASE "+quotedScratch+" RENAME TO "+quotedTemplate); err != nil {
		dropScratch()
		return fmt.Errorf("dbtest: publish template database: %w", err)
	}

	// A template that refuses connections can never report "source database is
	// being accessed by other users" to a copy. The setting needs the database
	// owner, so treat a refusal as acceptable rather than as a failure.
	_, _ = adminPool.Exec(ctx, "ALTER DATABASE "+quotedTemplate+" WITH ALLOW_CONNECTIONS false")
	return nil
}

// applySchema opens a pool on the scratch database, applies the seed and the
// ledgered migrations, then closes the pool.
func applySchema(ctx context.Context, adminPool *pgxpool.Pool, scratchName string, spec Spec) error {
	config := adminPool.Config().Copy()
	config.ConnConfig.Database = scratchName
	config.MinConns = 0
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("dbtest: open template build database: %w", err)
	}
	defer pool.Close()

	if spec.Seed != "" {
		if _, err := pool.Exec(ctx, spec.Seed); err != nil {
			return fmt.Errorf("dbtest: apply template seed: %w", err)
		}
	}

	runner := migrate.New(pool, spec.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		return fmt.Errorf("dbtest: apply embedded shared migrations: %w", err)
	}
	for _, tenant := range spec.Tenants {
		if err := runner.ApplyTenant(ctx, tenant); err != nil {
			return fmt.Errorf("dbtest: apply embedded tenant migrations for project %d: %w", tenant, err)
		}
	}
	if err := runner.CheckHead(ctx, migrate.ScopeShared, "platform"); err != nil {
		return fmt.Errorf("dbtest: verify shared migration head: %w", err)
	}
	for _, tenant := range spec.Tenants {
		if err := runner.CheckHead(ctx, migrate.ScopeTenant, strconv.FormatInt(tenant, 10)); err != nil {
			return fmt.Errorf("dbtest: verify tenant migration head for project %d: %w", tenant, err)
		}
	}
	return nil
}

// CreateFromTemplate creates databaseName as a copy of templateName.
func CreateFromTemplate(ctx context.Context, adminPool *pgxpool.Pool, templateName, databaseName string) error {
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	quotedTemplate := pgx.Identifier{templateName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase+" TEMPLATE "+quotedTemplate); err != nil {
		return fmt.Errorf("dbtest: copy template %s: %w", templateName, err)
	}
	return nil
}

func databaseExists(ctx context.Context, adminPool *pgxpool.Pool, name string) (bool, error) {
	var exists bool
	if err := adminPool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)", name,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("dbtest: look up database %s: %w", name, err)
	}
	return exists, nil
}

func waitForNoConnections(ctx context.Context, adminPool *pgxpool.Pool, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var backends int
		if err := adminPool.QueryRow(ctx,
			"SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname = $1", name,
		).Scan(&backends); err != nil {
			return fmt.Errorf("dbtest: count sessions on %s: %w", name, err)
		}
		if backends == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dbtest: %d sessions still hold %s", backends, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// dropAbandonedScratchDatabases removes the scratch databases of builds that
// did not finish.
//
// A build that still runs also owns a scratch database, and builds of
// different templates run at the same time. So age decides, not the lock: the
// name carries the nanosecond clock of its build, and only a name older than
// staleScratchAge is removed. buildDeadline is 180 s, so a live build can
// never reach that age.
func dropAbandonedScratchDatabases(ctx context.Context, adminPool *pgxpool.Pool) {
	// starts_with, not LIKE. An underscore is a LIKE wildcard, and the prefix
	// holds two of them, so LIKE would also match a name this package does not
	// own.
	rows, err := adminPool.Query(ctx,
		"SELECT datname FROM pg_catalog.pg_database WHERE starts_with(datname, $1)", buildPrefix)
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return
		}
		names = append(names, name)
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	oldest := time.Now().Add(-staleScratchAge).UnixNano()
	for _, name := range names {
		created, ok := scratchCreatedAt(name)
		if !ok || created > oldest {
			continue
		}
		_, _ = adminPool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}
}

// scratchCreatedAt reads the nanosecond clock that scratch names carry.
func scratchCreatedAt(name string) (int64, bool) {
	rest, found := strings.CutPrefix(name, buildPrefix)
	if !found {
		return 0, false
	}
	underscore := strings.LastIndex(rest, "_")
	if underscore < 0 {
		return 0, false
	}
	created, err := strconv.ParseInt(rest[underscore+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return created, true
}

// BuildContext returns a context with the template build deadline. Callers use
// it for EnsureTemplate, which is the only step that can pay a migration
// replay.
func BuildContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, buildDeadline)
}
