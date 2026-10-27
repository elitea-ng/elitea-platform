package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

type recordingPinExecutor struct {
	calls [][]any
}

func (r *recordingPinExecutor) Exec(_ context.Context, _ string, arguments ...any) (pgconn.CommandTag, error) {
	r.calls = append(r.calls, arguments)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func governedTestAdmission() profile.Admission {
	return profile.GovernedAdmission(profile.PolicyRevision{ID: "r1", Digest: [32]byte{1}})
}

// TestPinExecutionAdmissionFlagOffSendsNothing is flag-off contract item 6:
// a deployment that never ran governed sends no statement at all, for fresh
// admissions and continuations alike.
func TestPinExecutionAdmissionFlagOffSendsNothing(t *testing.T) {
	ctx := context.Background()
	for _, pin := range []admissionPin{
		{executionID: "e1"},
		{executionID: "e2", continuationScope: "agent-conversation:c", continuation: true},
	} {
		executor := &recordingPinExecutor{}
		if err := pinExecutionAdmission(ctx, executor, profile.LegacyAdmission(false), pin); err != nil {
			t.Fatal(err)
		}
		if len(executor.calls) != 0 {
			t.Fatalf("flag off sent %d statements for %+v", len(executor.calls), pin)
		}
	}
}

func TestPinExecutionAdmissionGuardOnlyLooksUpContinuations(t *testing.T) {
	ctx := context.Background()
	guarded := profile.LegacyAdmission(true)

	fresh := &recordingPinExecutor{}
	if err := pinExecutionAdmission(ctx, fresh, guarded, admissionPin{executionID: "e1", continuationScope: "agent-conversation:c"}); err != nil {
		t.Fatal(err)
	}
	if len(fresh.calls) != 0 {
		t.Fatal("a fresh legacy admission must not read or write pins")
	}

	resume := &recordingPinExecutor{}
	if err := pinExecutionAdmission(ctx, resume, guarded, admissionPin{executionID: "e2", continuationScope: "agent-conversation:c", continuation: true}); err != nil {
		t.Fatal(err)
	}
	if len(resume.calls) != 1 {
		t.Fatal("a guarded continuation must look up its predecessor")
	}
	args := resume.calls[0]
	if args[1] != nil || args[2] != nil || args[4] != true {
		t.Fatalf("a legacy continuation may only copy a predecessor: %v", args)
	}
}

func TestPinExecutionAdmissionGoverned(t *testing.T) {
	executor := &recordingPinExecutor{}
	if err := pinExecutionAdmission(context.Background(), executor, governedTestAdmission(), admissionPin{executionID: "e1"}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || executor.calls[0][0] != "e1" || executor.calls[0][1] != "r1" {
		t.Fatalf("calls = %v", executor.calls)
	}
	if err := pinExecutionAdmission(context.Background(), executor, governedTestAdmission(), admissionPin{}); err == nil {
		t.Fatal("an empty execution id must be refused")
	}
}

func TestDispatchPolicyRefusesMissingCredentialProfile(t *testing.T) {
	policy := testDispatchPolicy()
	policy.CredentialProfile = profile.Admission{}
	if err := policy.validate(); err == nil {
		t.Fatal("a dispatch policy without a credential profile must be refused")
	}
}
