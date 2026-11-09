package control

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// FU-275: a lease renewal, claim or settlement that meets a restarting
// database is retryable, so the Worker retries within its lease instead of
// giving the turn up.
func TestRuntimeControlAnswersADatabaseOutageAsRetryable(t *testing.T) {
	outage := fmt.Errorf("renew lease: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"})
	if code, _, retryable := safeRuntimeError(outage); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE || !retryable {
		t.Fatalf("safeRuntimeError(outage) = %v retryable=%v, want retryable DEPENDENCY_UNAVAILABLE", code, retryable)
	}
	if code, _, retryable := safeRuntimeError(errors.New("unexpected")); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL || retryable {
		t.Fatalf("safeRuntimeError(programming error) = %v retryable=%v, want INTERNAL, not retryable", code, retryable)
	}
}
