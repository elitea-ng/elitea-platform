package localturn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type unattendedSettingsStub struct {
	settings platformconfig.UnattendedSettings
}

func (s unattendedSettingsStub) Get(context.Context) (platformconfig.UnattendedSettings, error) {
	return s.settings, nil
}

type grantFakeStore struct {
	commitFakeStore
	started []StartRecord
}

func (s *grantFakeStore) StartLocalTurn(ctx context.Context, record StartRecord) (StartedTurn, error) {
	record, err := record.Admit(ctx, func() (ConversationFloor, error) { return ConversationFloor{}, nil })
	if err != nil {
		return StartedTurn{}, err
	}
	s.started = append(s.started, record)
	return StartedTurn{ExecutionID: record.ExecutionID, Created: true}, nil
}

func grantStartRequest(chainRoot string, maxContinuations int64) StartRequest {
	return StartRequest{
		ProjectID: 1, ActorUserID: 2, TokenID: "70", ConversationUUID: "3c5e7a9b-1d2f-4a6b-8c0d-2e4f6a8b0c1d",
		QuestionID: "4d6f8b0c-2e3a-4b7c-9d1e-3f5a7b9c1d2e", ParticipantID: 5, UserInput: "go",
		Unattended: &unattended.Request{
			GrantID: "5e7a9c1d-3f4b-4c8d-8e2f-4a6b8c0d2e3f", ChainRootExecutionID: chainRoot,
			ContinueStepLimit: true,
			Chain:             unattended.ChainRequest{MaxContinuations: &maxContinuations},
		},
	}
}

func grantService(t *testing.T, settings platformconfig.UnattendedSettings) (*Service, *grantFakeStore) {
	t.Helper()
	store := &grantFakeStore{}
	service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithUnattended(unattendedSettingsStub{settings}), store
}

// ADR-0045: a continuation keeps the chain's stored limits. An administrator
// who lowers the maximum mid-chain does not refuse the next turn of a chain
// whose client sends the old limits again.
func TestContinuationIgnoresClientChainLimits(t *testing.T) {
	settings := platformconfig.DefaultUnattendedSettings()
	settings.MaxContinuations = 3
	service, store := grantService(t, settings)
	if _, err := service.Start(t.Context(), grantStartRequest("", 10)); err == nil {
		t.Fatal("a first turn over the lowered maximum was admitted")
	}
	if _, err := service.Start(t.Context(), grantStartRequest(strings.Repeat("a", 32), 10)); err != nil {
		t.Fatalf("continuation = %v", err)
	}
	if len(store.started) != 1 {
		t.Fatalf("starts = %d", len(store.started))
	}
	grant := store.started[0].Grant
	if grant == nil || grant.ChainRootExecutionID != strings.Repeat("a", 32) || grant.Limits != (unattended.Limits{}) {
		t.Fatalf("continuation grant = %+v", grant)
	}
}

// ADR-0045: with grants off, a desktop continuation is refused as disabled.
func TestContinuationRefusedWhenGrantsAreOff(t *testing.T) {
	settings := platformconfig.DefaultUnattendedSettings()
	settings.Enabled = false
	service, store := grantService(t, settings)
	_, err := service.Start(t.Context(), grantStartRequest(strings.Repeat("a", 32), 1))
	var refusal *unattended.RefusalError
	if !errors.As(err, &refusal) || refusal.Reason != unattended.RefusedDisabled || len(store.started) != 0 {
		t.Fatalf("continuation with grants off = %v, starts = %d", err, len(store.started))
	}
}

// ADR-0045 decision 10: with grants off, a first turn whose grant only
// continues (the default) starts with no grant; a grant with an opt-in is
// refused as disabled.
func TestDefaultGrantStartsWithNoGrantWhenGrantsAreOff(t *testing.T) {
	settings := platformconfig.DefaultUnattendedSettings()
	settings.Enabled = false
	service, store := grantService(t, settings)
	if _, err := service.Start(t.Context(), grantStartRequest("", 1)); err != nil {
		t.Fatalf("default grant with grants off = %v", err)
	}
	if len(store.started) != 1 || store.started[0].Grant != nil {
		t.Fatalf("starts = %+v", store.started)
	}
	request := grantStartRequest("", 1)
	request.Unattended.HookAsks = true
	var refusal *unattended.RefusalError
	if _, err := service.Start(t.Context(), request); !errors.As(err, &refusal) || refusal.Reason != unattended.RefusedDisabled {
		t.Fatalf("hook asks with grants off = %v", err)
	}
}

// ADR-0045 decisions 7 and 11: a desktop continuation names its link kind
// (step_limit, tool_call_cap, goal_not_met or resume); absent is continue,
// and any other kind, or a kind on a first turn, is refused as invalid.
func TestContinuationKind(t *testing.T) {
	service, store := grantService(t, platformconfig.DefaultUnattendedSettings())
	root := strings.Repeat("a", 32)
	for kind, want := range map[string]string{
		"": unattended.LinkContinue, unattended.LinkStepLimit: unattended.LinkStepLimit,
		unattended.LinkToolCallCap: unattended.LinkToolCallCap, unattended.LinkGoalNotMet: unattended.LinkGoalNotMet,
		unattended.LinkResume: unattended.LinkResume,
	} {
		request := grantStartRequest(root, 1)
		request.Unattended.Kind = kind
		if _, err := service.Start(t.Context(), request); err != nil {
			t.Fatalf("kind %q = %v", kind, err)
		}
		if got := store.started[len(store.started)-1].Grant.Kind; got != want {
			t.Fatalf("kind %q stored as %q", kind, got)
		}
	}
	refused := []StartRequest{grantStartRequest(root, 1), grantStartRequest(root, 1), grantStartRequest("", 1)}
	refused[0].Unattended.Kind = unattended.LinkRoot
	refused[1].Unattended.Kind = unattended.LinkOutputLimit
	refused[2].Unattended.Kind = unattended.LinkGoalNotMet
	for _, request := range refused {
		var refusal *unattended.RefusalError
		if _, err := service.Start(t.Context(), request); !errors.As(err, &refusal) ||
			refusal.Field != "kind" || refusal.Reason != unattended.RefusedInvalid {
			t.Fatalf("kind %q = %v", request.Unattended.Kind, err)
		}
	}
}

// ADR-0045 decision 11: the first turn's brief state follows goal_check and
// approvals.run_brief.
func TestFirstTurnBriefState(t *testing.T) {
	for _, tc := range []struct {
		goalCheck, runBrief bool
		want                string
	}{
		{true, true, unattended.BriefPending},
		{true, false, unattended.BriefOff},
		{false, true, unattended.BriefNone},
	} {
		settings := platformconfig.DefaultUnattendedSettings()
		settings.RunBrief = tc.runBrief
		service, store := grantService(t, settings)
		request := grantStartRequest("", 1)
		request.Unattended.GoalCheck = tc.goalCheck
		if _, err := service.Start(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		grant := store.started[0].Grant
		if grant.BriefState != tc.want || grant.Grant.GoalCheck != tc.goalCheck {
			t.Fatalf("goal_check %v, run_brief %v = %+v", tc.goalCheck, tc.runBrief, grant)
		}
	}
}
