package indexschedule

import (
	"context"
	"strings"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

type freezerStub struct {
	calls   int
	binding string
	toolkit int64
	freeze  *agentexecutionapp.GovernedCredentialFreeze
	err     error
}

func (stub *freezerStub) FreezeToolkit(_ context.Context, _ int64, bindingID string, toolkitID int64) (*agentexecutionapp.GovernedCredentialFreeze, error) {
	stub.calls++
	stub.binding, stub.toolkit = bindingID, toolkitID
	return stub.freeze, stub.err
}

// freezeStarter records the freeze the admission context carries.
type freezeStarter struct {
	*contextStarter
	freeze *agentexecutionapp.GovernedCredentialFreeze
}

func (starter *freezeStarter) StartScheduledIndexData(
	ctx context.Context, request indexingapp.ScheduledStartRequest,
) (indexingapp.StartOutcome, error) {
	starter.freeze = agentexecutionapp.UnattendedCredentialFreezeFrom(ctx)
	return starter.contextStarter.StartScheduledIndexData(ctx, request)
}

// A governed index schedule with an active mandate freezes its toolkit's
// credential under the mandate's binding and admits the run with it; the
// project system token is never issued (#1473).
func TestGovernedIndexScheduleFreezesTheToolkitCredential(t *testing.T) {
	executor, dependencies, starter, _ := governedExecutor(t, "auto")
	recorder := &freezeStarter{contextStarter: starter}
	executor.start = recorder
	freeze := &agentexecutionapp.GovernedCredentialFreeze{
		AgentBindingID: "binding-1",
		References: []agentexecutionapp.GovernedCredentialReference{{
			ToolkitID: 7, CredentialReferenceID: "11111111-1111-4111-8111-111111111111",
		}},
	}
	freezer := &freezerStub{freeze: freeze}
	executor.WithCredentialFreezer(freezer)

	outcome, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "index-schedule-v1:frozen")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted || dependencies.start.calls != 1 {
		t.Fatalf("outcome=%+v start calls=%d", outcome, dependencies.start.calls)
	}
	if freezer.calls != 1 || freezer.binding != "binding-1" || freezer.toolkit != 7 {
		t.Fatalf("freezer calls=%d binding=%q toolkit=%d", freezer.calls, freezer.binding, freezer.toolkit)
	}
	if recorder.freeze != freeze {
		t.Fatalf("the admission context carries freeze %+v", recorder.freeze)
	}
	if dependencies.system.calls != 0 {
		t.Fatalf("CheckProjectSystemIdentity called %d times on a governed run", dependencies.system.calls)
	}
}

// A toolkit credential that cannot be frozen refuses the occurrence with a
// typed reason; nothing is admitted.
func TestGovernedIndexScheduleRefusesAnUnfreezableCredential(t *testing.T) {
	executor, dependencies, _, _ := governedExecutor(t, "auto")
	executor.WithCredentialFreezer(&freezerStub{err: &agentexecutionapp.GovernedCredentialAdmissionError{Code: "credential_grant_missing", ToolkitID: 7}})

	outcome, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "index-schedule-v1:unfrozen")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionInitializationFailed ||
		outcome.SafeReason != governedCredentialRefusalPrefix+": credential_grant_missing" {
		t.Fatalf("outcome=%+v", outcome)
	}
	if dependencies.start.calls != 0 || dependencies.system.calls != 0 {
		t.Fatalf("a refused occurrence reached start=%d system=%d", dependencies.start.calls, dependencies.system.calls)
	}
}

// A revoked mandate refuses the occurrence before any credential is frozen.
func TestGovernedIndexScheduleRevokedMandateFreezesNothing(t *testing.T) {
	executor, dependencies, _, store := governedExecutor(t, "auto")
	freezer := &freezerStub{}
	executor.WithCredentialFreezer(freezer)
	store.Update("auto", func(m *mandate.Mandate) { m.State = mandate.StateRevoked })

	outcome, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "index-schedule-v1:revoked-frozen")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionInitializationFailed ||
		!strings.HasPrefix(outcome.SafeReason, runmandate.CodeMandateNotUsable+":") ||
		freezer.calls != 0 || dependencies.start.calls != 0 || dependencies.system.calls != 0 {
		t.Fatalf("outcome=%+v freezer=%d start=%d system=%d", outcome, freezer.calls, dependencies.start.calls, dependencies.system.calls)
	}
}

// An ungoverned executor never freezes, even with a freezer composed.
func TestUngovernedIndexScheduleNeverFreezes(t *testing.T) {
	executor, dependencies := validCurrentExecutor(t, scheduledExecutorCandidate(-1))
	freezer := &freezerStub{}
	executor.WithCredentialFreezer(freezer)
	if _, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "k"); err != nil {
		t.Fatal(err)
	}
	if freezer.calls != 0 || dependencies.system.calls != 1 {
		t.Fatalf("freezer=%d system=%d", freezer.calls, dependencies.system.calls)
	}
}
