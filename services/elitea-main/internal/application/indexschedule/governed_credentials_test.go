package indexschedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
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

// governedInputs records whether the frozen inputs were resolved under a
// governed context, and the settings they were resolved from.
type governedInputs struct {
	*scheduledInputResolverStub
	governed bool
}

func (inputs *governedInputs) ResolveScheduled(
	ctx context.Context, request indexingapp.StartRequest, toolkit indexingapp.CurrentToolkitSnapshot,
) (indexingapp.AuthoritativeInputs, error) {
	inputs.governed = configurationapp.IsGovernedRun(ctx)
	return inputs.scheduledInputResolverStub.ResolveScheduled(ctx, request, toolkit)
}

// Review of #1556: after the freeze, the fire runs under
// configurations.WithGovernedRun, so the legacy title expansion refuses to
// run (ErrLegacyResolverInGovernedRun) and the frozen inputs resolve the
// toolkit's governed reference instead. The claim-mode preflight, which
// decrypts the configuration's secrets in Main, is not called at all, and a
// schedule's credential title does not replace the toolkit's governed
// reference.
func TestGovernedIndexScheduleNeverRunsTheLegacyResolver(t *testing.T) {
	executor, dependencies, _, _ := governedExecutor(t, "auto")
	reference := map[string]any{
		"elitea_title": "project-github", "private": false,
		configurationapp.CredentialReferenceIDField: "11111111-1111-4111-8111-111111111111",
	}
	dependencies.toolkits.toolkit.Settings = map[string]any{"github_configuration": reference}
	inputs := &governedInputs{scheduledInputResolverStub: dependencies.inputs}
	executor.inputs = inputs
	executor.WithCredentialFreezer(&freezerStub{freeze: &agentexecutionapp.GovernedCredentialFreeze{
		AgentBindingID: "binding-1",
		References: []agentexecutionapp.GovernedCredentialReference{{
			ToolkitID: 7, CredentialReferenceID: "11111111-1111-4111-8111-111111111111",
		}},
	}})
	candidate := scheduledExecutorCandidate(-1)
	candidate.Schedule.Credentials = &Credentials{EliteaTitle: "another-title"}

	outcome, err := executor.ExecuteScheduled(context.Background(), candidate, time.Now(), "index-schedule-v1:governed-ctx")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(dependencies.settings.calls) != 0 {
		t.Fatalf("the claim-mode preflight ran %d times on a governed run", len(dependencies.settings.calls))
	}
	if inputs.calls != 1 || !inputs.governed {
		t.Fatalf("frozen inputs calls=%d governed=%v", inputs.calls, inputs.governed)
	}
	got, ok := inputs.toolkit.Settings["github_configuration"].(map[string]any)
	if !ok || got[configurationapp.CredentialReferenceIDField] != "11111111-1111-4111-8111-111111111111" || got["elitea_title"] != "project-github" {
		t.Fatalf("the frozen inputs read %v, not the toolkit's governed reference", inputs.toolkit.Settings)
	}
	// The guard the mark arms: the legacy expansion refuses this context.
	expansion, err := configurationapp.NewCurrentExpansionService(legacyScopeStub{}, legacyFinderStub{t: t}, legacyUnsecreterStub{t: t})
	if err != nil {
		t.Fatal(err)
	}
	_, err = expansion.Expand(configurationapp.WithGovernedRun(context.Background()), configurationapp.CurrentExpansionRequest{
		Payload: reference, CurrentProjectID: 42,
	})
	if !errors.Is(err, configurationapp.ErrLegacyResolverInGovernedRun) {
		t.Fatalf("legacy expansion under a governed run: %v", err)
	}
}

// The legacy expansion's dependencies: any title lookup or secret
// redemption fails the test.
type legacyScopeStub struct{}

func (legacyScopeStub) PublicProjectID(context.Context) (int32, error) { return 1, nil }
func (legacyScopeStub) PersonalProjectID(context.Context, int32) (int32, error) {
	return 2, nil
}

type legacyFinderStub struct{ t *testing.T }

func (stub legacyFinderStub) FindByEliteaTitle(context.Context, int32, string, bool) (configurationapp.CurrentExpansionConfiguration, bool, error) {
	stub.t.Error("the legacy title lookup ran for a governed run")
	return configurationapp.CurrentExpansionConfiguration{}, false, nil
}

type legacyUnsecreterStub struct{ t *testing.T }

func (stub legacyUnsecreterStub) Unsecret(context.Context, int32, map[string]any) (map[string]any, error) {
	stub.t.Error("a secret was decrypted in Main for a governed run")
	return nil, nil
}
