package output

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// FU-275: a database that is restarting or recovering made output ingest
// answer INTERNAL, not retryable, so the Worker gave up the turn and it re-ran
// from its checkpoint after the lease expired. A failure that heals on its own
// is retryable DEPENDENCY_UNAVAILABLE, so the Worker resends the exact frame.
func TestOutputIngestAnswersADatabaseOutageAsRetryable(t *testing.T) {
	cases := map[string]error{
		"PgBouncer login retry": fmt.Errorf("begin output transaction: %w", &pgconn.PgError{Severity: "FATAL", Code: "08P01", Message: "server login has been failing, cached error: the database system is shutting down (server_login_retry)"}),
		"recovery mode":         fmt.Errorf("begin output transaction: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is in recovery mode"}),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			code, message, retryable := safeOutputError(err)
			if code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE || !retryable {
				t.Fatalf("safeOutputError = %v %q retryable=%v, want retryable DEPENDENCY_UNAVAILABLE", code, message, retryable)
			}
		})
	}
	for name, err := range map[string]error{
		"programming error": errors.New("unexpected"),
		"wrong password":    &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, retryable := safeOutputError(err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL || retryable {
				t.Fatalf("safeOutputError(%s) = %v retryable=%v, want INTERNAL, not retryable", name, code, retryable)
			}
		})
	}
}
