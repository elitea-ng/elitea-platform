package eliteacore

import (
	"fmt"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

// tenantOwnerID converts the project id in an import route's path into the
// integer written to a tenant table's owner_id column. The tenant schema is
// p_<project id>, so a schema this handler can address is by definition a
// project id it can store. A path segment that is not a project id gets an
// error rather than a substitute value: the import path already has one
// silent substitution (a failed fmt.Sscanf on the principal gives every row to
// user 1) and it is a defect, not a pattern to repeat.
//
// The return type is ownership.ProjectID and not int, because the same import
// path also holds a USER id (the caller). Both were plain integers, and #533
// measured what that costs: one number reached the wrong column and the
// database accepted it. The types now refuse the swap at compile time.
func tenantOwnerID(projectID string) (ownership.ProjectID, error) {
	ownerID, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a project id", projectID)
	}
	typed, err := ownership.NewProjectID(ownerID)
	if err != nil {
		return 0, fmt.Errorf("%q is not a project id", projectID)
	}
	return typed, nil
}

// importToolkitInsertSQL builds the elitea_tools INSERT that the toolkit import
// path runs for one tenant schema.
//
// # Why owner_id is here at all
//
// p_<id>.elitea_tools.owner_id is INTEGER NOT NULL with no default
// (internal/infra/db/migrations/001_initial.sql:435-447). The import statement
// named six columns and owner_id was not one of them, so on every deployment
// whose schema comes from this repository's migration corpus each toolkit
// import failed with
//
//	null value in column "owner_id" of relation "elitea_tools"
//	violates not-null constraint (SQLSTATE 23502)
//
// A pylon-made schema has no owner_id column on this table at all, which is why
// the same statement succeeds there and the fault stayed invisible (issue #504).
//
// # Which owner, and why
//
// owner_id is the DESTINATION PROJECT. It is not a user id, so the choice
// between "the caller", "the project's owner" and "the original owner named in
// the export" does not apply to this column. author_id carries the user, and it
// carries the CALLER. The evidence, measured rather than read:
//
//   - One column, two writers, one meaning. The interactive create route writes
//     the project id into this column (internal/api/v2/toolkits/handler.go,
//     createToolkitInsertSQL and CreateToolkit) and
//     create_toolkit_owner_id_test.go asserts it against the corpus schema.
//     ForkToolkit copies owner_id across a same-schema copy, which is only
//     coherent for a property of the project. An import that wrote a user id
//     would put two kinds of number in one column of one table.
//   - The export names no user for a toolkit. ExportImportGet emits id, name,
//     type, import_uuid and settings for each toolkit entry and nothing else,
//     so there is no "original owner named in the export" for this entity to be
//     faithful to.
//   - Where an export DOES carry owner_id — the ?fork=true application entry —
//     the value is a PROJECT id: Fork reads it straight into
//     forkMeta["parent_project_id"].
//   - A user id that comes from another installation cannot be trusted here.
//     author_id has no foreign key in either schema, so an adopted foreign id
//     would be stored without complaint and would attribute the toolkit to
//     whichever local user happens to hold that number. The import therefore
//     reads no user id out of the payload: the principal that performed the
//     import is the author, and a user this deployment does not hold is never
//     invented and never silently turned into user 1.
//
// The full column list is measured against the corpus in
// import_toolkit_owner_postgres_integration_test.go: owner_id was the only
// NOT NULL column with no default that the import path omitted.
func importToolkitInsertSQL(schema string) string {
	return fmt.Sprintf(`
		INSERT INTO %s.elitea_tools (name, type, settings, owner_id, author_id, description, meta)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, '{}'::jsonb) RETURNING id`, schema)
}
