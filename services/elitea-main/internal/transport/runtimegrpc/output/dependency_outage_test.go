package output

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
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

// FU-275: output ingest asks the session registry before ingesting. With the
// registry unreadable (a database restart) the frame is refused, nothing is
// ingested, and the refusal is retryable DEPENDENCY_UNAVAILABLE, bound to the
// frame with no credit and no commit, which is exactly what the Worker
// accepts as "resend this frame" (output_session.rs validate_rejection). Any
// other refusal stays AUTHENTICATION_FAILED, not retryable.
func TestOutputIngestAnswersAnUnreadableSessionStoreAsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		refusal       error
		wantCode      runtimev1.RuntimeErrorCodeV1
		wantRetryable bool
	}{
		{"store unreadable", fmt.Errorf("%w: %w", workloadauth.ErrWorkloadUnauthorized, workloadauth.ErrSessionStoreUnavailable), runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, true},
		{"session refused", workloadauth.ErrWorkloadUnauthorized, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := readCorpusFrame(t, "valid")
			validations := &validationIngestorStub{}
			failures := &failureIngestorStub{}
			server, err := NewServer(ServerConfig{
				OutputSchemaRevision: "elitea.runtime.execution-output.v1",
				MaxFrameBytes:        64 * 1024,
				CreditFrames:         8,
				CreditBytes:          64 * 1024,
			}, &outputAuthorizerStub{err: tc.refusal}, validations, failures)
			if err != nil {
				t.Fatal(err)
			}
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(validations.frames) != 0 || len(failures.frames) != 0 {
				t.Fatalf("a refused frame was ingested: validations=%d failures=%d", len(validations.frames), len(failures.frames))
			}
			ack := stream.acks[len(stream.acks)-1]
			rejection := ack.GetRejection()
			if rejection.GetCode() != tc.wantCode || rejection.GetRetryable() != tc.wantRetryable {
				t.Fatalf("rejection = %v retryable=%v, want %v retryable=%v", rejection.GetCode(), rejection.GetRetryable(), tc.wantCode, tc.wantRetryable)
			}
			if ack.GetStreamId() != frame.GetStreamId() || !proto.Equal(ack.GetIdentity(), frame.GetIdentity()) || !proto.Equal(ack.GetFence(), frame.GetFence()) ||
				ack.GetCommittedContiguousSequence() != 0 || ack.GetCreditFrames() != 0 || ack.GetCreditBytes() != 0 ||
				ack.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_UNSPECIFIED {
				t.Fatalf("the refusal is not bound to the frame the way the Worker requires: %v", ack)
			}
			if strings.Contains(rejection.GetSafeMessage(), frame.GetFence().GetWorkloadSessionId()) {
				t.Fatalf("the refusal carries the session id: %q", rejection.GetSafeMessage())
			}
		})
	}
}
