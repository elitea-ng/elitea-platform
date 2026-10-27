package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
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

// TestAgentAdmissionPinScopesToTheResponse is the review fix for #1456: a
// resume of turn 1 must not inherit the pin of a later turn 2 in the same
// conversation, so the scope is the response message, not the conversation.
func TestAgentAdmissionPinScopesToTheResponse(t *testing.T) {
	const conversation = "11111111-1111-4111-8111-111111111111"
	turnOne := agentexecutionapp.Admission{CurrentTurn: &agentexecutionapp.CurrentApplicationTurn{
		ConversationUUID: conversation, ResponseMessageID: "22222222-2222-4222-8222-222222222222",
	}}
	turnTwo := agentexecutionapp.Admission{CurrentTurn: &agentexecutionapp.CurrentApplicationTurn{
		ConversationUUID: conversation, ResponseMessageID: "33333333-3333-4333-8333-333333333333",
	}}
	resumeOne := agentexecutionapp.Admission{CurrentContinueTurn: &agentexecutionapp.CurrentContinueTurn{
		ConversationUUID: conversation, ResponseMessageID: "22222222-2222-4222-8222-222222222222",
	}}
	one, two, resume := agentAdmissionPin(turnOne), agentAdmissionPin(turnTwo), agentAdmissionPin(resumeOne)
	if one.continuation || !resume.continuation {
		t.Fatal("only the continue turn is a continuation")
	}
	if resume.continuationScope != one.continuationScope || one.continuationScope == two.continuationScope {
		t.Fatalf("scopes: turn1=%q turn2=%q resume1=%q", one.continuationScope, two.continuationScope, resume.continuationScope)
	}
}

// A parked fan-out's wake resumes the same response: it is a continuation in
// that response's scope, so a governed run never wakes as legacy.
func TestAgentAdmissionPinTreatsAFanoutWakeAsAContinuationOfItsResponse(t *testing.T) {
	const conversation, response = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	turn := agentAdmissionPin(agentexecutionapp.Admission{CurrentTurn: &agentexecutionapp.CurrentApplicationTurn{
		ConversationUUID: conversation, ResponseMessageID: response,
	}})
	wake := agentAdmissionPin(agentexecutionapp.Admission{CurrentFanoutWakeTurn: &agentexecutionapp.CurrentFanoutWakeTurn{
		ConversationUUID: conversation, ResponseMessageID: response,
	}})
	if !wake.continuation || wake.continuationScope != turn.continuationScope || wake.continuationScope == "" {
		t.Fatalf("wake pin %+v, turn scope %q", wake, turn.continuationScope)
	}
}
