package pgvector

import (
	"context"
	"errors"
	"fmt"
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
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 || id > math.MaxInt32 {
		return 0, false
	}
	// Canonical form only: "project_007" is not project 7's database.
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
//   - ModeDatabaseRole: DROP DATABASE project_<id> WITH (FORCE), then DROP ROLE
//     project_<id>_user. The database goes first so the role no longer owns or
//     holds grants on anything. FORCE disconnects a worker still attached;
//     callers that care must settle index runs first.
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
	if err := acquireProjectLockBounded(ctx, admin, request.ProjectID); err != nil {
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
		if exists {
			if err := exec(ctx, admin, "drop project database", dropDatabaseSQL(database)); err != nil {
				closeBestEffort(ctx, admin)
				return DropResult{}, err
			}
			result.DatabaseDropped = true
		}
		roleExists, err := projectRoleExists(ctx, admin, role)
		if err != nil {
			closeBestEffort(ctx, admin)
			return DropResult{}, err
		}
		if roleExists {
			if err := exec(ctx, admin, "drop project role", dropRoleSQL(role)); err != nil {
				closeBestEffort(ctx, admin)
				return DropResult{}, err
			}
			result.RoleDropped = true
		}
	}
	// Closing the admin session releases the advisory lock.
	if err := closeOwned(ctx, admin); err != nil {
		return DropResult{}, err
	}
	return result, nil
}

// Lock-wait bounds for Drop. The wait is independent of the request context:
// Deprovision runs under context.WithoutCancel, so without its own deadline a
// provision holding the same project's lock would block the delete for ever.
// Variables, not constants, so a test can shrink them.
var (
	dropLockTimeout     = 30 * time.Second
	dropLockPollInitial = 50 * time.Millisecond
	dropLockPollMax     = 1 * time.Second
)

// ErrDropLockTimeout means the per-project advisory lock stayed held by another
// session for the whole bounded wait. Nothing was dropped.
var ErrDropLockTimeout = errors.New("pgvector: timed out waiting for the project advisory lock")

// acquireProjectLockBounded takes the same advisory lock as Provision, but with
// pg_try_advisory_lock in a bounded retry loop instead of blocking.
func acquireProjectLockBounded(ctx context.Context, connection Connection, projectID int64) error {
	deadline := time.Now().Add(dropLockTimeout)
	delay := dropLockPollInitial
	for {
		// The query itself is not bounded by the request context's absence:
		// it returns immediately either way.
		got, err := queryBool(ctx, connection, "try project advisory lock",
			tryProjectLockSQL, projectLockNamespace, int32(projectID))
		if err != nil {
			return err
		}
		if got {
			return nil
		}
		if time.Now().Add(delay).After(deadline) {
			return ErrDropLockTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > dropLockPollMax {
			delay = dropLockPollMax
		}
	}
}

func validateDropRequest(request DropRequest) (database string, role string, err error) {
	if request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		(request.Mode != ModeDatabaseRole && request.Mode != ModeSchema) ||
		!validPostgresName(request.Admin.Database) {
		return "", "", ErrInvalidRequest
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

func dropRoleSQL(role string) string {
	return "DROP ROLE " + quoteIdentifier(role)
}

func dropSchemaSQL(schema string) string {
	return "DROP SCHEMA " + quoteIdentifier(schema) + " CASCADE"
}
