package runtimecomposition

import (
	"context"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

type fixedSuspensions []safety.Suspension

func (f fixedSuspensions) LoadActiveSuspensions(context.Context) ([]safety.Suspension, error) {
	return f, nil
}

type countingStores struct {
	agent, index, execute, callTool int
	// subjects are the fence subjects each admitted call carried on its
	// context, for the store's in-transaction re-check.
	subjects []safety.Subject
}

func (c *countingStores) record(ctx context.Context) {
	if subject, ok := safety.AdmissionSubject(ctx); ok {
		c.subjects = append(c.subjects, subject)
	}
}

func (c *countingStores) AdmitAgentExecution(ctx context.Context, _ agentexecutionapp.Admission) (executionapp.AdmissionOutcome, error) {
	c.agent++
	c.record(ctx)
	return executionapp.AdmissionOutcome{}, nil
}

func (c *countingStores) AdmitIndexIngest(ctx context.Context, _ indexingapp.Admission) (indexingapp.AdmissionOutcome, error) {
	c.index++
	c.record(ctx)
	return indexingapp.AdmissionOutcome{}, nil
}

func (c *countingStores) AdmitToolkitExecuteRead(ctx context.Context, _ toolkitexecutionapp.Admission) (executionapp.AdmissionOutcome, error) {
	c.execute++
	c.record(ctx)
	return executionapp.AdmissionOutcome{}, nil
}

func (c *countingStores) AdmitToolkitCallTool(ctx context.Context, _ toolkitcalltoolapp.Admission) (executionapp.AdmissionOutcome, error) {
	c.callTool++
	c.record(ctx)
	return executionapp.AdmissionOutcome{}, nil
}

func job(project string) executiondomain.Admission {
	return executiondomain.Admission{Job: executiondomain.Job{ResourceProjectID: project}}
}

// Each admission store refuses a covered run BEFORE it writes, with the
// typed refusal, and admits what no suspension covers.
func TestSuspensionGuardsRefuseCoveredRunsBeforeTheStore(t *testing.T) {
	t.Parallel()
	gate, err := safety.NewGate(fixedSuspensions{
		{ID: 1, Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: 7, ApplicationID: 3}},
		{ID: 2, Target: safety.Target{Scope: safety.ScopeProject, ProjectID: 9}},
		{ID: 3, Target: safety.Target{Scope: safety.ScopeToolkitType, ToolkitType: "github"}},
	}, safety.GateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stores := &countingStores{}
	agents := guardAgentAdmissions(stores, gate)
	index := guardIndexAdmissions(stores, gate)
	execute := guardToolkitExecute(stores, gate)
	callTool := guardToolkitCallTool(stores, gate)

	refused := func(name string, err error, id int64) {
		t.Helper()
		refusal, ok := safety.AsSuspended(err)
		if !ok || refusal.SuspensionID != id {
			t.Errorf("%s: err = %v, want suspension %d", name, err, id)
		}
	}

	_, err = agents.AdmitAgentExecution(ctx, agentexecutionapp.Admission{
		Record: job("7"), CurrentTurn: &agentexecutionapp.CurrentApplicationTurn{ApplicationID: 3},
	})
	refused("agent start", err, 1)
	_, err = agents.AdmitAgentExecution(ctx, agentexecutionapp.Admission{
		Record: job("7"), CurrentContinueTurn: &agentexecutionapp.CurrentContinueTurn{ApplicationID: 3},
	})
	refused("agent continue", err, 1)
	_, err = agents.AdmitAgentExecution(ctx, agentexecutionapp.Admission{
		Record: job("7"), CurrentRegenerateTurn: &agentexecutionapp.CurrentRegenerateTurn{ApplicationID: 3},
	})
	refused("agent regenerate", err, 1)
	if _, err := agents.AdmitAgentExecution(ctx, agentexecutionapp.Admission{
		Record: job("7"), CurrentTurn: &agentexecutionapp.CurrentApplicationTurn{ApplicationID: 4},
	}); err != nil {
		t.Errorf("another agent of the project: %v", err)
	}
	_, err = agents.AdmitAgentExecution(ctx, agentexecutionapp.Admission{Record: job("9")})
	refused("ad hoc chat in a suspended project", err, 2)

	_, err = index.AdmitIndexIngest(ctx, indexingapp.Admission{Record: job("9")})
	refused("index ingest", err, 2)
	if _, err := index.AdmitIndexIngest(ctx, indexingapp.Admission{Record: job("7")}); err != nil {
		t.Errorf("index ingest of project 7: %v", err)
	}

	_, err = execute.AdmitToolkitExecuteRead(ctx, toolkitexecutionapp.Admission{Record: job("9")})
	refused("direct read", err, 2)

	_, err = callTool.AdmitToolkitCallTool(ctx, toolkitcalltoolapp.Admission{
		Record: job("1"), Binding: executiondomain.ToolkitCallToolBinding{ToolkitType: "GitHub"},
	})
	refused("tool run of a suspended type", err, 3)
	if _, err := callTool.AdmitToolkitCallTool(ctx, toolkitcalltoolapp.Admission{
		Record: job("1"), Binding: executiondomain.ToolkitCallToolBinding{ToolkitType: "jira"},
	}); err != nil {
		t.Errorf("tool run of another type: %v", err)
	}

	if stores.agent != 1 || stores.index != 1 || stores.execute != 0 || stores.callTool != 1 {
		t.Fatalf("store calls = %+v: a refused run must write nothing", *stores)
	}
	// Every admitted call carries its subject to the store, which re-checks
	// it inside the admission transaction (safety/fence.go).
	want := []safety.Subject{
		{ProjectID: 7, ApplicationID: 4},
		{ProjectID: 7},
		{ProjectID: 1, ToolkitType: "jira"},
	}
	if len(stores.subjects) != len(want) {
		t.Fatalf("fenced subjects = %+v, want %+v", stores.subjects, want)
	}
	for i := range want {
		if stores.subjects[i] != want[i] {
			t.Fatalf("fenced subjects = %+v, want %+v", stores.subjects, want)
		}
	}
}

// Without a gate the stores are returned as they are.
func TestSuspensionGuardsAreInertWithoutAGate(t *testing.T) {
	t.Parallel()
	stores := &countingStores{}
	if guardAgentAdmissions(stores, nil) != agentexecutionapp.AtomicAdmissionStore(stores) ||
		guardIndexAdmissions(stores, nil) != indexingapp.AtomicAdmissionStore(stores) ||
		guardToolkitExecute(stores, nil) != toolkitexecutionapp.AtomicAdmissionStore(stores) ||
		guardToolkitCallTool(stores, nil) != toolkitcalltoolapp.AtomicAdmissionStore(stores) {
		t.Fatal("a nil gate must leave the store unwrapped")
	}
}
