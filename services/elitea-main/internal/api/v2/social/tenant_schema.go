package social

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// tenantSchema returns the project's tenant schema as a quoted PostgreSQL
// identifier, ready to interpolate with %s, and answers 400 when projectID is
// not a plain decimal project id.
//
// A query cannot bind a schema name as a parameter, so the name goes into the
// statement text and must be quoted with SQL rules. %q quotes with GO rules:
// it writes an embedded double quote as \" , which PostgreSQL reads as an
// ordinary backslash followed by the quote that ENDS the identifier. The rest
// of the caller's text then became SQL. See internal/infra/db/tenantschema and
// issue #543.
func tenantSchema(w http.ResponseWriter, projectID string) (string, bool) {
	quoted, err := tenantschema.Quote(projectID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return "", false
	}
	return quoted, true
}
