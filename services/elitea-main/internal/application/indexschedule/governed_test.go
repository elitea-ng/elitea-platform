package indexschedule

import (
	"context"
	"strings"
	"testing"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

type scheduleMandatesStub map[string]string

func (stub scheduleMandatesStub) IndexScheduleMandate(_ context.Context, _, _ int64, indexName string) (string, error) {
	return stub[indexName], nil
}

// contextStarter records the context the admission runs in.
type contextStarter struct {
	*scheduledStarterStub
	authority securitycontext.RunAuthority
	found     bool
}

func (starter *contextStarter) StartScheduledIndexData(
	ctx context.Context, request indexingapp.ScheduledStartRequest,
) (indexingapp.StartOutcome, error) {
	starter.authority, starter.found = securitycontext.RunAuthorityFrom(ctx)
	return starter.scheduledStarterStub.StartScheduledIndexData(ctx, request)
}

func governedExecutor(t *testing.T, attached string) (*CurrentExecutor, *currentExecutorDependencies, *contextStarter, *runmandatetest.Store) {
	t.Helper()
	candidate := scheduledExecutorCandidate(-1)
	executor, dependencies := validCurrentExecutor(t, candidate)
	starter := &contextStarter{scheduledStarterStub: dependencies.start}
	executor.start = starter
	store := runmandatetest.New()
	store.PutBinding("binding-1", candidate.ProjectID, 0)
	store.PutMandate("auto", candidate.ProjectID, mandate.KindAutonomous, "binding-1", 0)
	executor.WithMandates(scheduleMandatesStub{"docs": attached}, store.Service())
	return executor, dependencies, starter, store
}

func TestGovernedIndexScheduleNeverIssuesTheProjectToken(t *testing.T) {
	executor, dependencies, starter, _ := governedExecutor(t, "auto")
	outcome, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "index-schedule-v1:governed")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted || dependencies.start.calls != 1 {
		t.Fatalf("outcome=%+v start calls=%d", outcome, dependencies.start.calls)
	}
	// The project-system identity check is what calls IssueProjectToken.
	if dependencies.system.calls != 0 {
		t.Fatalf("CheckProjectSystemIdentity called %d times on a governed run", dependencies.system.calls)
	}
	if !starter.found || starter.authority.MandateID != "auto" || starter.authority.SubjectUserID != 0 {
		t.Fatalf("admission context authority=%+v found=%v", starter.authority, starter.found)
	}
}

func TestGovernedIndexScheduleWithoutAMandateIsRefused(t *testing.T) {
	executor, dependencies, _, store := governedExecutor(t, "")
	outcome, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "index-schedule-v1:none")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionInitializationFailed ||
		!strings.HasPrefix(outcome.SafeReason, runmandate.CodeMandateRequired+":") {
		t.Fatalf("outcome=%+v", outcome)
	}
	if dependencies.start.calls != 0 || dependencies.system.calls != 0 || dependencies.settings.calls != nil {
		t.Fatalf("a refused occurrence reached start=%d system=%d settings=%d",
			dependencies.start.calls, dependencies.system.calls, len(dependencies.settings.calls))
	}
	refusals := store.Refusals()
	if len(refusals) != 1 || refusals[0].Code != runmandate.CodeMandateRequired ||
		refusals[0].Target.Origin != runmandate.OriginIndexSchedule || refusals[0].Target.ResourceID != "7:docs" {
		t.Fatalf("refusals=%+v", refusals)
	}
}

func TestGovernedIndexScheduleRevokedMandateIsRefused(t *testing.T) {
	executor, dependencies, _, store := governedExecutor(t, "auto")
	store.Update("auto", func(m *mandate.Mandate) { m.State = mandate.StateRevoked })
	outcome, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "index-schedule-v1:revoked")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionInitializationFailed ||
		!strings.HasPrefix(outcome.SafeReason, runmandate.CodeMandateNotUsable+":") ||
		dependencies.start.calls != 0 || dependencies.system.calls != 0 {
		t.Fatalf("outcome=%+v start=%d system=%d", outcome, dependencies.start.calls, dependencies.system.calls)
	}
}

func TestWithMandatesNeedsBoth(t *testing.T) {
	executor, dependencies := validCurrentExecutor(t, scheduledExecutorCandidate(-1))
	executor.WithMandates(scheduleMandatesStub{}, nil)
	if _, err := executor.ExecuteScheduled(context.Background(), scheduledExecutorCandidate(-1), time.Now(), "k"); err != nil {
		t.Fatal(err)
	}
	if dependencies.system.calls != 1 {
		t.Fatalf("an ungoverned executor must keep the system identity check, calls=%d", dependencies.system.calls)
	}
}
