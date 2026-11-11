package main

import (
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
)

// describeForLog renders the error types whose text can carry connection
// details safely in a log line's cause chain (FU-299): a PostgreSQL answer
// keeps its SQLSTATE and message, a connect failure keeps only its kind.
// It looks at the element's own type, so the wrapping context above it
// stays in the chain.
func describeForLog(err error) (string, bool) {
	switch err.(type) {
	case *pgconn.PgError, *pgconn.ConnectError:
		return depfailure.Describe(err), true
	}
	return "", false
}
