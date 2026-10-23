// Package tenantschema builds the PostgreSQL identifier of a project's tenant
// schema.
//
// Every project keeps its rows in a schema named p_{project_id}, where
// project_id is centry.project.id. A query against such a schema cannot bind
// the schema name as a parameter, because PostgreSQL binds values and not
// identifiers. The name must therefore be interpolated into the statement
// text, and it must be quoted with SQL rules.
//
// # Why fmt %q is not an identifier quoter
//
// Callers used to interpolate the schema with %q. %q applies Go string-literal
// rules: it escapes an embedded double quote as \" . PostgreSQL applies
// different rules: inside a quoted identifier a backslash is an ordinary
// character and a double quote closes the identifier, so a doubled "" is the
// only way to keep one inside. The two rules disagree, and the disagreement
// lets caller text leave the identifier:
//
//	project id  1".configuration, centry.project x --
//	%q          FROM "p_1\".configuration, centry.project x --".configuration
//	Sanitize    FROM "p_1"".configuration, centry.project x --".configuration
//
// The %q line is one statement that PostgreSQL parses: the identifier stops at
// the quote after the backslash, and the rest of the caller's text becomes SQL.
// It fails today only because the backslash makes the relation name unknown.
// That is a property of %q, not a decision, and it disappears if the name is
// built differently. The Sanitize line keeps the whole payload inside one
// identifier, where it is inert.
//
// # How to use this package
//
// Call Quote and interpolate the result with %s. Do not interpolate the result
// with %q: the value is already quoted, and %q would quote it a second time.
// Quote refuses a project id that is not a plain decimal number, so hostile
// text stops at the handler and never reaches the statement at all.
package tenantschema

import (
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Prefix starts every tenant schema name. It agrees with create_tenant_schema
// in migrations/001_initial.sql.
const Prefix = "p_"

// MaxProjectIDDigits bounds a project id above any real BIGSERIAL value and
// refuses pathological input outright.
const MaxProjectIDDigits = 19

// ErrInvalidProjectID is the answer to a project id that is not a plain
// decimal number. It is an *apierr.APIError, so apierr.Write answers 400 and
// discloses nothing about the database.
var ErrInvalidProjectID = apierr.BadRequest("invalid project id")

// Valid reports whether projectID is a plain decimal project id.
//
// A tenant schema is named after centry.project.id, a BIGSERIAL that starts at
// 1. A project id is therefore a run of ASCII digits that starts with a
// non-zero digit: "0" identifies no project, and a leading zero would give two
// spellings ("7" and "07") of one schema name. Nothing else is a project id,
// so anything else identifies no schema and must stop the request.
func Valid(projectID string) bool {
	if projectID == "" || len(projectID) > MaxProjectIDDigits {
		return false
	}
	if projectID[0] < '1' || projectID[0] > '9' {
		return false
	}
	for i := 1; i < len(projectID); i++ {
		if projectID[i] < '0' || projectID[i] > '9' {
			return false
		}
	}
	return true
}

// Name returns the UNQUOTED schema name, for a caller that must compare the
// name against a catalogue column such as information_schema.schemata
// .schema_name. Bind the result as a parameter. Do not interpolate it.
func Name(projectID string) (string, error) {
	if !Valid(projectID) {
		return "", ErrInvalidProjectID
	}
	return Prefix + projectID, nil
}

// Quote returns the schema as a quoted PostgreSQL identifier, for example
// `"p_1"`. Interpolate the result with %s.
func Quote(projectID string) (string, error) {
	name, err := Name(projectID)
	if err != nil {
		return "", err
	}
	return pgx.Identifier{name}.Sanitize(), nil
}

// QuoteInt is Quote for a project id that is already an integer.
func QuoteInt(projectID int64) (string, error) {
	if projectID <= 0 {
		return "", ErrInvalidProjectID
	}
	return Quote(strconv.FormatInt(projectID, 10))
}
