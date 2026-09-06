package eliteacore

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

// This file holds the only place in the package that turns a project id into a
// schema identifier.
//
// A query cannot bind a schema name as a parameter, so the name is
// interpolated into the statement text. The handlers used to build it with
// fmt.Sprintf("p_%s", projectID) straight from chi.URLParam and interpolate it
// with %q. %q applies Go string-literal rules and writes an embedded double
// quote as \" . PostgreSQL treats a backslash inside a quoted identifier as an
// ordinary character and ENDS the identifier at that quote, so the rest of the
// caller's text left the identifier and became SQL. See
// internal/infra/db/tenantschema for the measurement.
//
// Every schema identifier below is quoted with SQL rules and interpolated with
// %s. Do not interpolate one with %q: the value is already quoted.

// tenantSchema returns the project's tenant schema as a quoted PostgreSQL
// identifier, for example `"p_1"`, ready to interpolate with %s.
//
// It answers 400 and returns false when projectID is not a plain decimal
// project id. The request stops there, so caller text never reaches a
// statement. The answer names no table and carries no SQL error.
func tenantSchema(w http.ResponseWriter, projectID string) (string, bool) {
	quoted, err := tenantschema.Quote(projectID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return "", false
	}
	return quoted, true
}

// publicProjectIDOrDefault returns the id of the PUBLIC project, the project
// whose schema holds the published catalogue.
//
// The id is set by the operator, not by a caller, but it still reaches an
// identifier. internal/publicproject resolves it from ELITEA_AI_PROJECT_ID and
// its deprecated aliases and only ever answers a positive integer, so the
// validity check below can no longer fail — it is kept because this value ends
// up inside a schema name, and a guard on that path must not depend on a
// promise made in another package.
func publicProjectIDOrDefault() string {
	id := publicproject.IDString()
	if !tenantschema.Valid(id) {
		return "1"
	}
	return id
}

// publicTenantSchema returns the PUBLIC project's schema as a quoted
// PostgreSQL identifier. It cannot fail: publicProjectIDOrDefault gives a
// valid id or the default.
func publicTenantSchema() string {
	quoted, err := tenantschema.Quote(publicProjectIDOrDefault())
	if err != nil {
		// Unreachable: publicProjectIDOrDefault only returns a valid id.
		return `"p_1"`
	}
	return quoted
}

// catalogueSchema quotes a schema name that came out of the database
// catalogue, such as the 'p_' || project_id list the author page reads.
//
// The value is the database's own, not a caller's, so it is not refused. It is
// quoted with SQL rules all the same, because a name is quoted by the rule
// that applies to it and not by how much its source is trusted.
func catalogueSchema(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
