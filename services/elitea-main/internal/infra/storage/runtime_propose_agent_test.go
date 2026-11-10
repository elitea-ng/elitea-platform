package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
)

type fakeProposeSink struct {
	run     ProposeAgentRun
	writes  []ProposalWrite
	outcome ProposalOutcome
}

func (f *fakeProposeSink) ResolveProposeAgentRun(context.Context, int64, string, uint64) (ProposeAgentRun, error) {
	return f.run, nil
}

func (f *fakeProposeSink) Propose(_ context.Context, _ ProposeAgentRun, write ProposalWrite) (ProposalOutcome, error) {
	f.writes = append(f.writes, write)
	return f.outcome, nil
}

type fakeCreatePermissions struct {
	held   []string
	userID int64
	err    error
	asked  []string
}

func (f *fakeCreatePermissions) ResolvePermissions(_ context.Context, principal auth.User, _ string, projectID string) (auth.PermissionResolution, error) {
	f.asked = append(f.asked, principal.ID+"@"+projectID)
	if f.err != nil {
		return auth.PermissionResolution{}, f.err
	}
	return auth.PermissionResolution{UserID: f.userID, Permissions: f.held}, nil
}

func proposeService(t *testing.T, sink *fakeProposeSink, permissions *fakeCreatePermissions) *RuntimeProposeAgentService {
	t.Helper()
	authorizer := agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{ResourceProjectID: 7, ActorID: "3", ConversationID: "c-1"}, nil
	})
	service, err := NewRuntimeProposeAgentService(authorizer, sink, permissions)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func proposingRun() ProposeAgentRun {
	return ProposeAgentRun{ProjectID: 7, ExecutionID: "e-1", ConversationUUID: "c-1", ApplicationID: 11, ActorID: 3, CanPropose: true}
}

func proposeRequest() agentproposal.Request {
	return agentproposal.Request{
		EphemeralAgentID: "a_0000002a",
		Rationale:        "It found the bug fast.",
		Profile:          agentproposal.Profile{DisplayName: "Ada", Soul: "Precise."},
	}
}

func TestRuntimeProposeAgentStoresADraftAtTierLowWithTheRuntimeDigest(t *testing.T) {
	sink := &fakeProposeSink{run: proposingRun(), outcome: ProposalOutcome{Status: agentproposal.OutcomeProposed, ProposalID: 5}}
	permissions := &fakeCreatePermissions{held: []string{agentproposal.CreatePermission}, userID: 3}
	got, err := proposeService(t, sink, permissions).Propose(context.Background(), ContentClaim{ExecutionID: "e-1", Generation: 1}, proposeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentproposal.OutcomeProposed || got.ProposalID != "5" || got.SchemaVersion != RuntimeProposeAgentSchemaVersion {
		t.Fatalf("result %+v", got)
	}
	if len(sink.writes) != 1 {
		t.Fatalf("writes %d", len(sink.writes))
	}
	write := sink.writes[0]
	if write.Draft["risk_tier"] != "low" || write.SpecDigest != agentproposal.SpecDigest(write.Request.Profile) {
		t.Fatalf("write %+v", write)
	}
	if len(permissions.asked) != 1 || permissions.asked[0] != "3@7" {
		t.Fatalf("the person the run served is resolved in the run's project: %v", permissions.asked)
	}
}

func TestRuntimeProposeAgentRefusesWithoutTheCapabilityOrThePermission(t *testing.T) {
	ctx := context.Background()
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}
	cases := map[string]struct {
		run         func(*ProposeAgentRun)
		permissions *fakeCreatePermissions
	}{
		"no capability":  {func(run *ProposeAgentRun) { run.CanPropose = false }, &fakeCreatePermissions{held: []string{agentproposal.CreatePermission}, userID: 3}},
		"ad hoc run":     {func(run *ProposeAgentRun) { run.ApplicationID = 0 }, &fakeCreatePermissions{held: []string{agentproposal.CreatePermission}, userID: 3}},
		"no create":      {func(*ProposeAgentRun) {}, &fakeCreatePermissions{held: []string{"models.applications.applications.list"}, userID: 3}},
		"denied":         {func(*ProposeAgentRun) {}, &fakeCreatePermissions{err: auth.ErrPermissionDenied}},
		"another person": {func(*ProposeAgentRun) {}, &fakeCreatePermissions{held: []string{agentproposal.CreatePermission}, userID: 4}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			run := proposingRun()
			c.run(&run)
			sink := &fakeProposeSink{run: run}
			got, err := proposeService(t, sink, c.permissions).Propose(ctx, claim, proposeRequest())
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != agentproposal.OutcomeNotAllowed || len(sink.writes) != 0 {
				t.Fatalf("got %+v, writes %d", got, len(sink.writes))
			}
		})
	}
}

func TestRuntimeProposeAgentTakesTheRunFromTheClaimNotTheWorker(t *testing.T) {
	ctx := context.Background()
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}
	for name, mutate := range map[string]func(*ProposeAgentRun){
		"another project":      func(run *ProposeAgentRun) { run.ProjectID = 8 },
		"another conversation": func(run *ProposeAgentRun) { run.ConversationUUID = "c-2" },
		"another person":       func(run *ProposeAgentRun) { run.ActorID = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			run := proposingRun()
			mutate(&run)
			sink := &fakeProposeSink{run: run}
			_, err := proposeService(t, sink, &fakeCreatePermissions{held: []string{agentproposal.CreatePermission}, userID: 3}).
				Propose(ctx, claim, proposeRequest())
			if !errors.Is(err, ErrContentUnauthorized) || len(sink.writes) != 0 {
				t.Fatalf("err %v, writes %d", err, len(sink.writes))
			}
		})
	}
}

func TestRuntimeProposeAgentAnswersAnInvalidRequestAsAToolResult(t *testing.T) {
	sink := &fakeProposeSink{run: proposingRun()}
	request := proposeRequest()
	request.EphemeralAgentID = "nope"
	got, err := proposeService(t, sink, &fakeCreatePermissions{}).Propose(context.Background(), ContentClaim{ExecutionID: "e-1", Generation: 1}, request)
	if err != nil || got.Status != agentproposal.OutcomeInvalid || len(sink.writes) != 0 {
		t.Fatalf("got %+v err %v", got, err)
	}
}
