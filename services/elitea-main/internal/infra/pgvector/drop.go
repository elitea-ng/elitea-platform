package pgvector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"
)

// ErrInvalidDropTarget means a drop was asked for a name that is not exactly
// the database, role or schema this package derives from the given project id.
// The drop is destructive and irreversible, so it never trusts a name: it
// re-derives the only names it will touch and refuses anything else.
var ErrInvalidDropTarget = errors.New("pgvector: refusing to drop a name that is not this project's own")

const (
	projectDatabasePrefix = "project_"
	projectRoleSuffix     = "_user"
)

// ProjectDatabaseName is the per-project database (ModeDatabaseRole) or schema
// (ModeSchema) name, "project_<id>". It is the single derivation provisioning
// and the drop share.
func ProjectDatabaseName(projectID int64) string {
	return projectDatabasePrefix + strconv.FormatInt(projectID, 10)
}

// ProjectRoleName is the per-project login role, "project_<id>_user".
func ProjectRoleName(projectID int64) string {
	return ProjectDatabaseName(projectID) + projectRoleSuffix
}

// ParseProjectDatabaseName returns the project id when name is exactly
// "project_<id>" in canonical decimal form (no sign, no leading zero, no
// suffix) and the id fits the centry.project int4 key.
func ParseProjectDatabaseName(name string) (int64, bool) {
	return parseProjectName(name, "")
}

// ParseProjectRoleName returns the project id when name is exactly
// "project_<id>_user".
func ParseProjectRoleName(name string) (int64, bool) {
	return parseProjectName(name, projectRoleSuffix)
}

func parseProjectName(name, suffix string) (int64, bool) {
	if len(name) <= len(projectDatabasePrefix)+len(suffix) ||
		name[:len(projectDatabasePrefix)] != projectDatabasePrefix ||
		name[len(name)-len(suffix):] != suffix {
		return 0, false
	}
	digits := name[len(projectDatabasePrefix) : len(name)-len(suffix)]
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 || id > math.MaxInt32 {
		return 0, false
	}
	// Canonical form only: ParseInt accepts a leading sign and leading zeros, and
	// "project_007" is not project 7's database. The round trip rejects both.
	if ProjectDatabaseName(id)+suffix != name {
		return 0, false
	}
	return id, true
}

// DropRequest names one project whose PgVector isolation is to be removed.
// Database and Role are optional echoes of the names the caller believes it is
// dropping (the orphan command passes the exact names the operator confirmed);
// when set they must equal the names derived from ProjectID.
type DropRequest struct {
	ProjectID int64
	Admin     AdminConnection
	Mode      Mode
	Database  string
	Role      string
}

// DropResult reports what the drop actually removed, so an idempotent no-op is
// distinguishable from a real drop.
type DropResult struct {
	DatabaseDropped bool
	RoleDropped     bool
	SchemaDropped   bool
}

// Drop removes one project's PgVector isolation under the bootstrap admin
// connection, and is the inverse of Provision.
//
//   - ModeDatabaseRole: lock the role out (see prepareForcedDrop), then DROP
//     DATABASE project_<id> WITH (FORCE), then DROP ROLE project_<id>_user. The
//     database goes first so the role no longer owns or holds grants on
//     anything. FORCE disconnects a worker still attached; callers that care
//     must settle index runs first.
//   - ModeSchema: DROP SCHEMA project_<id> CASCADE in the admin's own database.
//     Provision creates no role in this mode, so none is dropped.
//
// It is idempotent: an absent database, role or schema is a no-op. It takes the
// same per-project advisory lock as Provision, so a drop and a provision of the
// same project cannot interleave.
func (p *Provisioner) Drop(ctx context.Context, request DropRequest) (DropResult, error) {
	if ctx == nil || p == nil || p.connector == nil {
		return DropResult{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return DropResult{}, err
	}
	database, role, err := validateDropRequest(request)
	if err != nil {
		return DropResult{}, err
	}

	admin, err := p.connect(ctx, request.Admin.Database)
	if err != nil {
		return DropResult{}, err
	}
	if err := acquireProjectLockWithTimeout(ctx, admin, request.ProjectID); err != nil {
		closeBestEffort(ctx, admin)
		return DropResult{}, err
	}

	var result DropResult
	if request.Mode == ModeSchema {
		exists, err := queryBool(ctx, admin, "check project schema", schemaExistsSQL, database)
		if err != nil {
			closeBestEffort(ctx, admin)
			return DropResult{}, err
		}
		if exists {
			if err := exec(ctx, admin, "drop project schema", dropSchemaSQL(database)); err != nil {
				closeBestEffort(ctx, admin)
				return DropResult{}, err
			}
			result.SchemaDropped = true
		}
	} else {
		exists, err := queryBool(ctx, admin, "check project database", databaseExistsSQL, database)
		if err != nil {
			closeBestEffort(ctx, admin)
			return DropResult{}, err
		}
		roleExists, err := projectRoleExists(ctx, admin, role)
		if err != nil {
			closeBestEffort(ctx, admin)
			return DropResult{}, err
		}
		if exists {
			if roleExists {
				if err := prepareForcedDrop(ctx, admin, database, role); err != nil {
					closeBestEffort(ctx, admin)
					return DropResult{}, err
				}
			}
			if err := exec(ctx, admin, "drop project database", dropDatabaseSQL(database)); err != nil {
				closeBestEffort(ctx, admin)
				return DropResult{}, err
			}
			result.DatabaseDropped = true
		}
		if roleExists {
			if err := exec(ctx, admin, "drop project role", dropRoleSQL(role)); err != nil {
				closeBestEffort(ctx, admin)
				return DropResult{}, err
			}
			result.RoleDropped = true
		}
	}
	// Closing the admin session releases the advisory lock. Both drops have
	// already happened, so a close that fails does not undo anything: the result
	// is returned and the close error is only logged. (The server ends the
	// session, and with it the lock, when the connection goes away.)
	if err := closeOwned(ctx, admin); err != nil {
		dropLogger().WarnContext(ctx, "pgvector: the admin session did not close cleanly after the drop",
			"project_id", request.ProjectID, "err", err)
	}
	return result, nil
}

// Exists reports whether the project's PgVector isolation is present: its
// database or its login role (ModeDatabaseRole), or its schema (ModeSchema). It
// is read-only and takes no lock. The project delete uses it to find the
// leftovers of a project whose row is already gone (#1211).
func (p *Provisioner) Exists(ctx context.Context, request DropRequest) (bool, error) {
	if ctx == nil || p == nil || p.connector == nil {
		return false, ErrInvalidRequest
	}
	database, role, err := validateDropRequest(request)
	if err != nil {
		return false, err
	}
	admin, err := p.connect(ctx, request.Admin.Database)
	if err != nil {
		return false, err
	}
	defer closeBestEffort(ctx, admin)
	if request.Mode == ModeSchema {
		return queryBool(ctx, admin, "check project schema", schemaExistsSQL, database)
	}
	exists, err := queryBool(ctx, admin, "check project database", databaseExistsSQL, database)
	if err != nil || exists {
		return exists, err
	}
	return projectRoleExists(ctx, admin, role)
}

// dropLogger is where Drop reports a non-fatal close failure. A variable so a
// test can capture it.
var dropLogger = slog.Default

// prepareForcedDrop makes DROP DATABASE ... WITH (FORCE) work for an admin that
// is not a superuser (managed Postgres: RDS, Cloud SQL, Azure).
//
// FORCE terminates the other sessions on the database. From PG16 a non-superuser
// CREATEROLE admin may only terminate a backend whose role it is a member of,
// and creating a role no longer makes the creator a usable member (the grant it
// gets has ADMIN but not INHERIT or SET). So, in order:
//
//  1. ALTER ROLE NOLOGIN: the project role cannot open a new session while the
//     drop runs.
//  2. GRANT <role> TO CURRENT_USER, skipped when the admin already has the
//     role's privileges (a superuser always does). The test is USAGE, not
//     MEMBER: the creator's automatic ADMIN-only grant makes it a MEMBER
//     without INHERIT, which is exactly the state that cannot terminate. The
//     creator has ADMIN OPTION on PG16+, so the grant is allowed; "already a
//     member" is tolerated.
//  3. REVOKE CONNECT ON DATABASE FROM PUBLIC and the role: a session that was
//     not role-based cannot sneak back in either.
//
// The role is about to be dropped, so none of this needs undoing; the grant goes
// with the role.
func prepareForcedDrop(ctx context.Context, connection Connection, database, role string) error {
	if err := exec(ctx, connection, "disable project role login", alterRoleNoLoginSQL(role)); err != nil {
		return err
	}
	member, err := queryBool(ctx, connection, "check admin privileges of project role", roleMemberSQL, role)
	if err != nil {
		return err
	}
	if !member {
		if err := exec(ctx, connection, "grant project role to admin", grantRoleToAdminSQL(role)); err != nil {
			// A concurrent grant is the only benign failure; recheck.
			recheck, checkErr := queryBool(ctx, connection, "recheck admin privileges of project role", roleMemberSQL, role)
			if checkErr != nil || !recheck {
				return err
			}
		}
	}
	return exec(ctx, connection, "revoke project database connect", revokeConnectSQL(database, role))
}

// dropLockTimeout bounds Drop's wait for the per-project advisory lock. The
// wait is independent of the request context: Deprovision runs the drop under
// context.WithoutCancel with its own bound, so without a deadline here a
// provision holding the same project's lock would block the delete for the
// whole of that bound. A variable, not a constant, so a test can shrink it.
var dropLockTimeout = 30 * time.Second

// ErrDropLockTimeout means the per-project advisory lock stayed held by another
// session for the whole bounded wait. Nothing was dropped.
var ErrDropLockTimeout = errors.New("pgvector: timed out waiting for the project advisory lock")

// lockNotAvailable is SQLSTATE 55P03, what a lock wait cut off by lock_timeout
// raises.
const lockNotAvailable = "55P03"

// acquireProjectLockWithTimeout takes the same advisory lock as Provision with
// the same blocking pg_advisory_lock, bounded by the session's lock_timeout
// rather than by a poll loop: the server wakes the waiter the moment the lock is
// free, and raises 55P03 when the timeout passes first, which maps to
// ErrDropLockTimeout. lock_timeout is reset afterwards, so it bounds only this
// wait and not the drop statements.
func acquireProjectLockWithTimeout(ctx context.Context, connection Connection, projectID int64) error {
	if err := exec(ctx, connection, "set lock timeout", setLockTimeoutSQL(dropLockTimeout)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := connection.Exec(ctx, acquireProjectLockSQL, projectLockNamespace, int32(projectID)); err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == lockNotAvailable {
			return ErrDropLockTimeout
		}
		return operationError(ctx, "acquire project advisory lock", err)
	}
	return exec(ctx, connection, "reset lock timeout", resetLockTimeoutSQL)
}

// setLockTimeoutSQL renders SET lock_timeout. SET takes no bind parameters; the
// value is an integer number of milliseconds, never caller input.
func setLockTimeoutSQL(timeout time.Duration) string {
	milliseconds := max(timeout.Milliseconds(), 1)
	return "SET lock_timeout = " + strconv.FormatInt(milliseconds, 10)
}

const resetLockTimeoutSQL = "RESET lock_timeout"

func validateDropRequest(request DropRequest) (database string, role string, err error) {
	if request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 {
		return "", "", fmt.Errorf("%w: project id out of range", ErrInvalidRequest)
	}
	if request.Mode != ModeDatabaseRole && request.Mode != ModeSchema {
		return "", "", fmt.Errorf("%w: unknown mode", ErrInvalidRequest)
	}
	// The drop runs from the admin database, so it has to be named: an empty
	// name would make the connector refuse with no hint. Callers that take a
	// server URL (cmd/pgvector-orphans) fall back to `postgres` before this.
	if request.Admin.Database == "" {
		return "", "", fmt.Errorf("%w: the admin connection names no database", ErrInvalidRequest)
	}
	if !validPostgresName(request.Admin.Database) {
		return "", "", fmt.Errorf("%w: the admin database name is not valid", ErrInvalidRequest)
	}
	database = ProjectDatabaseName(request.ProjectID)
	role = ProjectRoleName(request.ProjectID)
	if !validPostgresName(database) || !validPostgresName(role) {
		return "", "", ErrInvalidRequest
	}
	if request.Database != "" && request.Database != database {
		return "", "", fmt.Errorf("%w: database", ErrInvalidDropTarget)
	}
	if request.Role != "" && request.Role != role {
		return "", "", fmt.Errorf("%w: role", ErrInvalidDropTarget)
	}
	// DROP DATABASE cannot drop the database the session is connected to. The
	// derived name can only collide with a deliberately named bootstrap.
	if request.Mode == ModeDatabaseRole && request.Admin.Database == database {
		return "", "", fmt.Errorf("%w: database is the bootstrap database", ErrInvalidDropTarget)
	}
	return database, role, nil
}

// The identifiers go through the package's one quoter (quoteIdentifier); the
// names have already been re-derived from an integer id, so quoting is defence
// in depth.
func dropDatabaseSQL(database string) string {
	return "DROP DATABASE " + quoteIdentifier(database) + " WITH (FORCE)"
}

const roleMemberSQL = `SELECT pg_catalog.pg_has_role(CURRENT_USER, $1::name, 'USAGE')`

func alterRoleNoLoginSQL(role string) string {
	return "ALTER ROLE " + quoteIdentifier(role) + " NOLOGIN"
}

func grantRoleToAdminSQL(role string) string {
	return "GRANT " + quoteIdentifier(role) + " TO CURRENT_USER"
}

func revokeConnectSQL(database, role string) string {
	return "REVOKE CONNECT ON DATABASE " + quoteIdentifier(database) + " FROM PUBLIC, " + quoteIdentifier(role)
}

func dropRoleSQL(role string) string {
	return "DROP ROLE " + quoteIdentifier(role)
}

func dropSchemaSQL(schema string) string {
	return "DROP SCHEMA " + quoteIdentifier(schema) + " CASCADE"
}
