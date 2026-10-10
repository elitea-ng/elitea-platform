package pgvector

import (
	"context"
	"errors"
	"strings"
)

const (
	projectLockNamespace     int32 = 0x454c4954 // ASCII "ELIT"
	acquireProjectLockSQL          = `SELECT pg_catalog.pg_advisory_lock($1, $2)`
	tryProjectLockSQL              = `SELECT pg_catalog.pg_try_advisory_lock($1, $2)`
	roleExistsSQL                  = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`
	databaseExistsSQL              = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)`
	schemaExistsSQL                = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`
	createVectorExtensionSQL       = `CREATE EXTENSION IF NOT EXISTS vector`
)

func acquireProjectLock(ctx context.Context, connection Connection, projectID int64) error {
	return exec(
		ctx,
		connection,
		"acquire project advisory lock",
		acquireProjectLockSQL,
		projectLockNamespace,
		int32(projectID),
	)
}

func projectRoleExists(ctx context.Context, connection Connection, role string) (bool, error) {
	return queryBool(ctx, connection, "check project role", roleExistsSQL, role)
}

func ensureRole(ctx context.Context, connection Connection, role string, password string) (bool, error) {
	exists, err := projectRoleExists(ctx, connection, role)
	if err != nil {
		return false, err
	}
	if exists {
		return true, exec(ctx, connection, "reset project role password", alterRoleSQL(role, password))
	}

	if err := exec(ctx, connection, "create project role", createRoleSQL(role, password)); err != nil {
		// CREATE ROLE has no IF NOT EXISTS. Rechecking handles a concurrent
		// creator and an unknown-success retry without masking cancellation.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		exists, checkErr := queryBool(ctx, connection, "recheck project role", roleExistsSQL, role)
		if checkErr != nil {
			return false, checkErr
		}
		if !exists {
			return false, err
		}
		if alterErr := exec(ctx, connection, "converge project role password", alterRoleSQL(role, password)); alterErr != nil {
			return false, alterErr
		}
		return true, nil
	}
	return false, nil
}

func ensureDatabase(ctx context.Context, connection Connection, database string) error {
	exists, err := queryBool(ctx, connection, "check project database", databaseExistsSQL, database)
	if err != nil || exists {
		return err
	}
	if err := exec(ctx, connection, "create project database", createDatabaseSQL(database)); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		exists, checkErr := queryBool(ctx, connection, "recheck project database", databaseExistsSQL, database)
		if checkErr != nil {
			return checkErr
		}
		if !exists {
			return err
		}
	}
	return nil
}

func ensureSchema(ctx context.Context, connection Connection, schema string) error {
	exists, err := queryBool(ctx, connection, "check project schema", schemaExistsSQL, schema)
	if err != nil || exists {
		return err
	}
	if err := exec(ctx, connection, "create project schema", createSchemaSQL(schema)); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		exists, checkErr := queryBool(ctx, connection, "recheck project schema", schemaExistsSQL, schema)
		if checkErr != nil {
			return checkErr
		}
		if !exists {
			return err
		}
	}
	return nil
}

func grantDatabase(ctx context.Context, connection Connection, database string, role string) error {
	statement := "GRANT ALL PRIVILEGES ON DATABASE " + quoteIdentifier(database) + " TO " + quoteIdentifier(role)
	return exec(ctx, connection, "grant project database", statement)
}

// grantPublicSchema gives role full use of the project database's public
// schema, for the objects that exist now AND for the ones the provisioning
// administrator creates later.
//
// #6642: "GRANT ... ON ALL TABLES" covers only tables that already exist. When
// LangGraph's PostgresSaver.setup() later ran as the administrator, it created
// checkpoint_migrations, checkpoints, checkpoint_blobs and checkpoint_writes
// with no grant to the project role, and every prediction in that project then
// failed with "permission denied for table checkpoint_migrations".
//
// The default privileges are set FIRST, so a table the administrator creates
// between these statements is covered by one or the other. ALTER DEFAULT
// PRIVILEGES without FOR ROLE applies to objects created by the executing
// role, which is the administrator this connection authenticates as.
//
// WHAT THIS DOES AND DOES NOT REACH. It runs only when Provisioner.provision
// runs. In production that is project creation: the one caller
// (runtimecomposition.ProjectVectorStore) asks for ProvisionIfMissing, and
// ProjectPgvectorService returns before any SQL when the project's vault
// already holds complete material. Nothing calls ProvisionRepair or
// ProvisionForceRecreate today. So:
//
//   - a project created from now on gets the default privileges. In the
//     database-role mode the worker connects as the project role, which then
//     owns the checkpoint* tables it creates; the default privileges matter
//     for tables an administrator creates in that database (a pylon-era
//     worker, a manual setup, or the existing-admin-user mode).
//
//   - an EXISTING project, including every project migrated from pylon, is
//     NOT re-granted: there is no wired reprovision path. A deployment that
//     already shows "permission denied for table checkpoint_migrations" must
//     be repaired by hand, connected to the project's database
//     ("project_<id>") as the role that owns the checkpoint* tables:
//
//     GRANT ALL ON ALL TABLES IN SCHEMA public TO "project_<id>_user";
//     GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "project_<id>_user";
//     ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO "project_<id>_user";
//     ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO "project_<id>_user";
//
// They are this function's statements minus the schema grant, all idempotent.
func grantPublicSchema(ctx context.Context, connection Connection, role string) error {
	quotedRole := quoteIdentifier(role)
	statements := [...]string{
		"GRANT ALL ON SCHEMA public TO " + quotedRole,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO " + quotedRole,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO " + quotedRole,
		"GRANT ALL ON ALL TABLES IN SCHEMA public TO " + quotedRole,
		"GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO " + quotedRole,
	}
	for _, statement := range statements {
		if err := exec(ctx, connection, "grant public schema", statement); err != nil {
			return err
		}
	}
	return nil
}

func createRoleSQL(role string, password string) string {
	return "CREATE USER " + quoteIdentifier(role) + " WITH PASSWORD " + quoteLiteral(password)
}

func alterRoleSQL(role string, password string) string {
	return "ALTER USER " + quoteIdentifier(role) + " WITH PASSWORD " + quoteLiteral(password)
}

func createDatabaseSQL(database string) string {
	return "CREATE DATABASE " + quoteIdentifier(database)
}

func createSchemaSQL(schema string) string {
	return "CREATE SCHEMA " + quoteIdentifier(schema)
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	// An explicit escape string is independent of standard_conforming_strings.
	// Both backslashes and quotes are escaped so passwords cannot change the
	// statement structure. validateRequest rejects NUL and invalid UTF-8 first.
	var builder strings.Builder
	builder.Grow(len(value) + 3)
	builder.WriteString("E'")
	for _, character := range value {
		switch character {
		case '\\':
			builder.WriteString(`\\`)
		case '\'':
			builder.WriteString("''")
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('\'')
	return builder.String()
}

func queryBool(
	ctx context.Context,
	connection Connection,
	operation string,
	statement string,
	args ...any,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	value, err := connection.QueryBool(ctx, statement, args...)
	if err != nil {
		return false, operationError(ctx, operation, err)
	}
	return value, nil
}

func exec(ctx context.Context, connection Connection, operation string, statement string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := connection.Exec(ctx, statement, args...); err != nil {
		return operationError(ctx, operation, err)
	}
	return nil
}
