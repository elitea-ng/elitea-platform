// Package legacypgvector drops what the retired per-project PgVector store
// left on its server: one database, project_<id>, and one login role,
// project_<id>_user, per project that was provisioned before #1359.
//
// Nothing writes there any more (every vector is in Qdrant behind
// elitea-vector), but the databases still hold a deleted project's chunk text
// and embeddings. So a project delete keeps a journal step that drops them
// (projectprovisioning, StepProjectPgvectorDrop), and `elitea-main
// legacy-pgvector-orphans` finds and drops the ones left by projects deleted
// before that step came back.
//
// THE ONLY NAMES IT TOUCHES are derived from an integer project id: exactly
// project_<id> and project_<id>_user, for a canonical positive int4 id. A name
// read from the server is accepted only if it round-trips through that
// derivation, so project_007, project_7_user_old and anything else on a shared
// server are never listed or dropped.
//
// THE ADMIN CONNECTION is the one the old store provisioned with: a role that
// may drop those databases and roles (a superuser, or on managed PostgreSQL a
// CREATEROLE/CREATEDB role that created them). It is configured as a URL
// (ELITEA_LEGACY_PGVECTOR_ADMIN_URL); the stored SQLAlchemy form
// postgresql+psycopg:// is accepted, as the old store accepted it.
package legacypgvector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	databasePrefix = "project_"
	roleSuffix     = "_user"
	// defaultAdminDatabase is where a URL that names no database connects.
	// Every server has it, and it is never a project_<id>, so DROP DATABASE
	// never targets the session's own database.
	defaultAdminDatabase = "postgres"
)

// ErrInvalidAdminURL reports an admin URL that is not a PostgreSQL URL or that
// pgx cannot parse. The message never carries the URL: it holds a password.
var ErrInvalidAdminURL = errors.New("legacypgvector: the admin URL is not a usable PostgreSQL URL")

// DatabaseName is the project's legacy database, "project_<id>".
func DatabaseName(projectID int64) string {
	return databasePrefix + strconv.FormatInt(projectID, 10)
}

// RoleName is the project's legacy login role, "project_<id>_user".
func RoleName(projectID int64) string { return DatabaseName(projectID) + roleSuffix }

// ParseDatabaseName returns the id when name is exactly "project_<id>" in
// canonical form (no sign, no leading zero) and the id fits centry.project's
// int4 key.
func ParseDatabaseName(name string) (int64, bool) { return parseName(name, "") }

// ParseRoleName returns the id when name is exactly "project_<id>_user".
func ParseRoleName(name string) (int64, bool) { return parseName(name, roleSuffix) }

func parseName(name, suffix string) (int64, bool) {
	if !strings.HasPrefix(name, databasePrefix) || !strings.HasSuffix(name, suffix) ||
		len(name) <= len(databasePrefix)+len(suffix) {
		return 0, false
	}
	id, err := strconv.ParseInt(name[len(databasePrefix):len(name)-len(suffix)], 10, 64)
	if err != nil || id <= 0 || id > math.MaxInt32 {
		return 0, false
	}
	// ParseInt accepts a sign and leading zeros; "project_007" is not project
	// 7's database. The round trip rejects both.
	if DatabaseName(id)+suffix != name {
		return 0, false
	}
	return id, true
}

// Dropper drops legacy project databases and roles through one admin URL. It
// opens a connection per call: a project delete drops one project's pair, and
// a pool kept for that would hold a connection to a server nothing else uses.
type Dropper struct {
	config *pgx.ConnConfig
}

// NewDropper parses the admin URL. An URL with no database connects to
// `postgres`; one that names a project_<id> database is refused, since DROP
// DATABASE cannot drop the session's own database and the drop would fail for
// that project only.
func NewDropper(adminURL string) (*Dropper, error) {
	dsn, ok := normalizeURL(strings.TrimSpace(adminURL))
	if !ok {
		return nil, ErrInvalidAdminURL
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, ErrInvalidAdminURL
	}
	if config.Database == "" {
		config.Database = defaultAdminDatabase
	}
	if _, isProject := ParseDatabaseName(config.Database); isProject {
		return nil, fmt.Errorf("%w: it names a project database (%s); name the server's admin database, or none", ErrInvalidAdminURL, config.Database)
	}
	return &Dropper{config: config}, nil
}

// normalizeURL accepts the URL forms the old store accepted: postgres://,
// postgresql:// and SQLAlchemy's postgresql+psycopg://, which pgx rejects.
func normalizeURL(value string) (string, bool) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	switch {
	case strings.HasPrefix(value, "postgresql+psycopg://"):
		return "postgresql://" + strings.TrimPrefix(value, "postgresql+psycopg://"), true
	case strings.HasPrefix(value, "postgresql://"), strings.HasPrefix(value, "postgres://"):
		return value, true
	default:
		return "", false
	}
}

// AdminDatabase is the database the admin connection opens (for a header an
// operator checks before dropping).
func (d *Dropper) AdminDatabase() string { return d.config.Database }

// AdminHost is the server the admin connection reaches, host:port.
func (d *Dropper) AdminHost() string {
	return d.config.Host + ":" + strconv.FormatUint(uint64(d.config.Port), 10)
}

// Dropped reports what one Drop removed, so a no-op is told from a drop.
type Dropped struct {
	Database        string
	Role            string
	DatabaseDropped bool
	RoleDropped     bool
}

// Summary is the one-line account a journal note carries.
func (d Dropped) Summary() string {
	switch {
	case d.DatabaseDropped && d.RoleDropped:
		return "dropped legacy PgVector database " + d.Database + " and role " + d.Role
	case d.DatabaseDropped:
		return "dropped legacy PgVector database " + d.Database + " (no role " + d.Role + ")"
	case d.RoleDropped:
		return "dropped legacy PgVector role " + d.Role + " (no database " + d.Database + ")"
	default:
		return "no legacy PgVector database or role to drop"
	}
}

// Drop removes project_<id> and project_<id>_user. It is idempotent: an absent
// database or role is not an error.
//
// The order is the old store's. The role is locked out first (NOLOGIN), so it
// cannot open a session while the drop runs. A non-superuser admin (managed
// PostgreSQL) may only terminate sessions of roles it has the privileges of,
// and from PG16 creating a role no longer grants them, so the admin grants the
// role to itself when pg_has_role says it lacks USAGE. CONNECT is revoked from
// PUBLIC so nothing else reconnects, then DROP DATABASE ... WITH (FORCE)
// disconnects whatever is left, and the role goes last, once it owns nothing.
func (d *Dropper) Drop(ctx context.Context, projectID int64) (Dropped, error) {
	if projectID <= 0 || projectID > math.MaxInt32 {
		return Dropped{}, fmt.Errorf("legacypgvector: project id %d is out of range", projectID)
	}
	result := Dropped{Database: DatabaseName(projectID), Role: RoleName(projectID)}
	conn, err := pgx.ConnectConfig(ctx, d.config)
	if err != nil {
		return result, fmt.Errorf("legacypgvector: connect to the admin database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	database, role := pgx.Identifier{result.Database}.Sanitize(), pgx.Identifier{result.Role}.Sanitize()
	databaseExists, roleExists, err := exists(ctx, conn, result.Database, result.Role)
	if err != nil {
		return result, err
	}
	if roleExists {
		if _, err := conn.Exec(ctx, "ALTER ROLE "+role+" NOLOGIN"); err != nil {
			return result, fmt.Errorf("legacypgvector: lock out role %s: %w", result.Role, err)
		}
		var privileged bool
		if err := conn.QueryRow(ctx,
			`SELECT pg_catalog.pg_has_role(CURRENT_USER, $1::name, 'USAGE')`, result.Role,
		).Scan(&privileged); err != nil {
			return result, fmt.Errorf("legacypgvector: read the admin's privileges on role %s: %w", result.Role, err)
		}
		if !privileged {
			if _, err := conn.Exec(ctx, "GRANT "+role+" TO CURRENT_USER"); err != nil {
				return result, fmt.Errorf("legacypgvector: grant role %s to the admin: %w", result.Role, err)
			}
		}
	}
	if databaseExists {
		if _, err := conn.Exec(ctx, "REVOKE CONNECT ON DATABASE "+database+" FROM PUBLIC"); err != nil {
			return result, fmt.Errorf("legacypgvector: revoke connect on %s: %w", result.Database, err)
		}
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); err != nil {
			return result, fmt.Errorf("legacypgvector: drop database %s: %w", result.Database, err)
		}
		result.DatabaseDropped = true
	}
	if roleExists {
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+role); err != nil {
			return result, fmt.Errorf("legacypgvector: drop role %s: %w", result.Role, err)
		}
		result.RoleDropped = true
	}
	return result, nil
}

// DropProjectDatabase is Drop for the project delete's journal step
// (projectprovisioning.LegacyVectorDatabases): the summary is the step's note.
func (d *Dropper) DropProjectDatabase(ctx context.Context, projectID int64) (string, error) {
	dropped, err := d.Drop(ctx, projectID)
	if err != nil {
		return "", err
	}
	return dropped.Summary(), nil
}

// Exists reports whether project_<id> or project_<id>_user is on the server.
func (d *Dropper) Exists(ctx context.Context, projectID int64) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, d.config)
	if err != nil {
		return false, fmt.Errorf("legacypgvector: connect to the admin database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	databaseExists, roleExists, err := exists(ctx, conn, DatabaseName(projectID), RoleName(projectID))
	return databaseExists || roleExists, err
}

// Querier is the one method the existence probe needs: a connection, a pool
// or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ExistsOn reports whether the project's legacy database or role is on the
// server q is connected to. The project delete asks the PLATFORM database's
// server with it (no admin URL needed to read the catalogue): a deployment
// whose old store shared that server is caught even when nothing configures
// the admin URL.
func ExistsOn(ctx context.Context, q Querier, projectID int64) (bool, error) {
	databaseExists, roleExists, err := exists(ctx, q, DatabaseName(projectID), RoleName(projectID))
	return databaseExists || roleExists, err
}

func exists(ctx context.Context, q Querier, database, role string) (databaseExists, roleExists bool, err error) {
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1),
       EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $2)`,
		database, role).Scan(&databaseExists, &roleExists); err != nil {
		return false, false, fmt.Errorf("legacypgvector: read the catalogue: %w", err)
	}
	return databaseExists, roleExists, nil
}

// Leftover is one project id with a legacy database and/or role on the server.
type Leftover struct {
	ProjectID int64
	Database  string // empty when the database is gone
	Role      string // empty when the role is gone
}

// List returns every project id that has a legacy database or role on the
// server, by id. Only names that are exactly project_<id> or
// project_<id>_user are read (ParseDatabaseName, ParseRoleName).
func (d *Dropper) List(ctx context.Context) ([]Leftover, error) {
	conn, err := pgx.ConnectConfig(ctx, d.config)
	if err != nil {
		return nil, fmt.Errorf("legacypgvector: connect to the admin database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, `
SELECT datname::text, false FROM pg_catalog.pg_database WHERE datname LIKE 'project\_%'
UNION ALL
SELECT rolname::text, true FROM pg_catalog.pg_roles WHERE rolname LIKE 'project\_%\_user'`)
	if err != nil {
		return nil, fmt.Errorf("legacypgvector: list the catalogue: %w", err)
	}
	type pair struct {
		name string
		role bool
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pair, error) {
		var p pair
		return p, row.Scan(&p.name, &p.role)
	})
	if err != nil {
		return nil, fmt.Errorf("legacypgvector: list the catalogue: %w", err)
	}
	byID := map[int64]*Leftover{}
	var ids []int64
	for _, p := range pairs {
		parse := ParseDatabaseName
		if p.role {
			parse = ParseRoleName
		}
		id, ok := parse(p.name)
		if !ok {
			continue
		}
		leftover := byID[id]
		if leftover == nil {
			leftover = &Leftover{ProjectID: id}
			byID[id] = leftover
			ids = append(ids, id)
		}
		if p.role {
			leftover.Role = p.name
		} else {
			leftover.Database = p.name
		}
	}
	slices.Sort(ids)
	out := make([]Leftover, 0, len(ids))
	for _, id := range ids {
		out = append(out, *byID[id])
	}
	return out, nil
}
