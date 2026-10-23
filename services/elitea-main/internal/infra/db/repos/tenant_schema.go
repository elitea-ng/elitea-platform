package repos

import (
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// invalidTenantSchema is the identifier substituted for a project id that is
// not a plain decimal number. `!` cannot appear in a schema created by
// create_tenant_schema (migrations/001_initial.sql), so any query built with
// it fails closed with "relation does not exist" instead of touching data.
// It is quoted by the same rule as every other identifier here.
var invalidTenantSchema = pgx.Identifier{"p_!invalid"}.Sanitize()

// schema returns the project's tenant schema as a QUOTED PostgreSQL
// identifier, for example `"p_1"`, ready to interpolate with %s.
//
// It returns a fail-closed sentinel rather than an error, for the older
// callers that have no error path. New code should use tenantSchema, which
// returns an error instead.
//
// The callers used to interpolate an UNQUOTED name with %q. %q is a Go string
// quoter, not a PostgreSQL identifier quoter: it writes an embedded quote as
// \" where PostgreSQL wants it doubled, and PostgreSQL ends the identifier at
// that quote, so caller text left the identifier and became SQL (issue #543).
func schema(projectID string) string {
	quoted, err := tenantschema.Quote(projectID)
	if err != nil {
		return invalidTenantSchema
	}
	return quoted
}

// tenantSchema returns the project's tenant schema as a fully quoted
// PostgreSQL identifier (e.g. `"p_1"`), ready to interpolate with %s, or a
// 400 when projectID is not a plain decimal project id.
func tenantSchema(projectID string) (string, error) {
	return tenantschema.Quote(projectID)
}

// isNumericRowID reports whether an id path parameter is a plain decimal row
// id. Row ids reach SQL as bind parameters, so a non-numeric one is not an
// injection risk — it is simply an id that cannot exist, and PostgreSQL would
// answer it with "invalid input syntax for type integer" (a 500) instead of a
// 404.
func isNumericRowID(id string) bool { return tenantschema.Valid(id) }
