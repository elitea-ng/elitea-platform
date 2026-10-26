package workspacehandoff

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const forgeThread = "6f1c2d3e-4a5b-4c6d-8e7f-901234567890"

// forgeRepo answers one handoff row; nothing else of the repository is used.
type forgeRepo struct {
	Repository
	row   Handoff
	found bool
}

func (r *forgeRepo) Read(context.Context, Thread) (Handoff, bool, error) { return r.row, r.found, nil }

type forgeStore struct{ stored []*ForgeCredential }

func (s *forgeStore) SetForgeCredential(_ context.Context, _ Thread, _ int64, credential *ForgeCredential) (Handoff, error) {
	s.stored = append(s.stored, credential)
	return Handoff{ForgeCredential: credential}, nil
}

type forgeResolver struct {
	calls []struct {
		user     int64
		unsecret bool
	}
	err error
}

func (r *forgeResolver) ResolveForgeCredential(_ context.Context, _, userID int64, _ ForgeCredential, unsecret bool) (map[string]any, error) {
	r.calls = append(r.calls, struct {
		user     int64
		unsecret bool
	}{userID, unsecret})
	if r.err != nil {
		return nil, r.err
	}
	return map[string]any{"base_url": "https://api.github.com", "access_token": "ghp_x"}, nil
}

type fixedPolicy Policy

func (p fixedPolicy) Effective(context.Context, int64) (Policy, error) { return Policy(p), nil }

type auditLog struct{ actions []string }

func (a *auditLog) Record(_ context.Context, event audit.Event) {
	a.actions = append(a.actions, event.Action)
}

func forgeService(t *testing.T, row Handoff, found bool, policy Policy) (*Service, *forgeStore, *forgeResolver, *auditLog) {
	t.Helper()
	recorder := &auditLog{}
	service, err := NewService(&forgeRepo{row: row, found: found}, fixedPolicy(policy), noEraser{}, noCanceller{}, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, resolver := &forgeStore{}, &forgeResolver{}
	service.WithForgeCredentials(store, resolver)
	return service, store, resolver, recorder
}

func cloudRow() Handoff {
	return Handoff{ProjectID: 1, ConversationUUID: forgeThread, OwnerUserID: 7, State: workspacestore.StateCloudIdle, Epoch: 2}
}

var pullRequestsOn = Policy{Enabled: true, PullRequests: true, PullRequestApproval: true}

func TestSetForgeCredentialChecksTheReferenceAndAuditsNoSecret(t *testing.T) {
	service, store, resolver, recorder := forgeService(t, cloudRow(), true, pullRequestsOn)
	thread := Thread{ProjectID: 1, ConversationUUID: forgeThread}
	credential := &ForgeCredential{EliteaTitle: "gh-team", Private: true}
	updated, err := service.SetForgeCredential(context.Background(), SetForgeCredentialRequest{
		Thread: thread, ActorUserID: 7, Credential: credential, AuditRoute: "/route",
	})
	if err != nil || updated.ForgeCredential == nil || len(store.stored) != 1 {
		t.Fatalf("set = %+v %v", updated, err)
	}
	// The reference is checked as the owner, without redeeming its secret.
	if len(resolver.calls) != 1 || resolver.calls[0].user != 7 || resolver.calls[0].unsecret {
		t.Fatalf("resolver calls = %+v", resolver.calls)
	}
	if len(recorder.actions) != 1 || !strings.Contains(recorder.actions[0], "gh-team (private)") ||
		strings.Contains(recorder.actions[0], "ghp_") {
		t.Fatalf("audit = %v", recorder.actions)
	}
	// Clearing needs no policy and no resolver.
	if _, err := service.SetForgeCredential(context.Background(), SetForgeCredentialRequest{Thread: thread, ActorUserID: 7}); err != nil ||
		store.stored[1] != nil || recorder.actions[1] != EventForgeCredentialCleared || len(resolver.calls) != 1 {
		t.Fatalf("clear: %v %v %v", err, store.stored, recorder.actions)
	}
}

func TestSetForgeCredentialRefusals(t *testing.T) {
	thread := Thread{ProjectID: 1, ConversationUUID: forgeThread}
	credential := &ForgeCredential{EliteaTitle: "gh"}
	local := cloudRow()
	local.State = workspacestore.StateLocal
	erased := cloudRow()
	erased.DeletedAt = &time.Time{}
	for name, tc := range map[string]struct {
		row        Handoff
		found      bool
		policy     Policy
		actor      int64
		credential *ForgeCredential
		resolveErr error
		want       error
	}{
		"no handoff":          {found: false, policy: pullRequestsOn, actor: 7, credential: credential, want: ErrNotFound},
		"not the owner":       {row: cloudRow(), found: true, policy: pullRequestsOn, actor: 8, credential: credential, want: ErrNotFound},
		"back on the desktop": {row: local, found: true, policy: pullRequestsOn, actor: 7, credential: credential, want: ErrNotInCloud},
		"erased":              {row: erased, found: true, policy: pullRequestsOn, actor: 7, credential: credential, want: ErrErased},
		"pull requests off":   {row: cloudRow(), found: true, policy: Policy{Enabled: true}, actor: 7, credential: credential, want: ErrPullRequestsDisabled},
		"not a github config": {row: cloudRow(), found: true, policy: pullRequestsOn, actor: 7, credential: credential, resolveErr: ErrForgeCredentialInvalid, want: ErrForgeCredentialInvalid},
		"a blank title":       {row: cloudRow(), found: true, policy: pullRequestsOn, actor: 7, credential: &ForgeCredential{EliteaTitle: " "}, want: ErrInvalid},
		"a control character": {row: cloudRow(), found: true, policy: pullRequestsOn, actor: 7, credential: &ForgeCredential{EliteaTitle: "a\nb"}, want: ErrInvalid},
		"an over-long title":  {row: cloudRow(), found: true, policy: pullRequestsOn, actor: 7, credential: &ForgeCredential{EliteaTitle: strings.Repeat("a", 257)}, want: ErrInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			service, store, resolver, _ := forgeService(t, tc.row, tc.found, tc.policy)
			resolver.err = tc.resolveErr
			_, err := service.SetForgeCredential(context.Background(), SetForgeCredentialRequest{
				Thread: thread, ActorUserID: tc.actor, Credential: tc.credential,
			})
			if !errors.Is(err, tc.want) || len(store.stored) != 0 {
				t.Fatalf("err = %v, stored = %v", err, store.stored)
			}
		})
	}
	// Without the plumbing composed, the call is unavailable, not a panic.
	service, err := NewService(&forgeRepo{row: cloudRow(), found: true}, fixedPolicy(pullRequestsOn), noEraser{}, noCanceller{}, noAudit{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetForgeCredential(context.Background(), SetForgeCredentialRequest{Thread: thread, ActorUserID: 7}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("uncomposed = %v", err)
	}
}

func TestForgeCredentialForTurnRedeemsAsTheOwnerUnderTheCurrentPolicy(t *testing.T) {
	row := cloudRow()
	row.ForgeCredential = &ForgeCredential{EliteaTitle: "gh-team"}
	service, _, resolver, _ := forgeService(t, row, true, pullRequestsOn)
	credential, err := service.ForgeCredentialForTurn(context.Background(), row)
	if err != nil || credential["access_token"] != "ghp_x" || len(resolver.calls) != 1 ||
		resolver.calls[0].user != 7 || !resolver.calls[0].unsecret {
		t.Fatalf("redeem = %v %v %+v", credential, err, resolver.calls)
	}
	if _, err := service.ForgeCredentialForTurn(context.Background(), cloudRow()); !errors.Is(err, ErrNoForgeCredential) {
		t.Fatalf("no credential = %v", err)
	}
	off, _, _, _ := forgeService(t, row, true, Policy{Enabled: true})
	if _, err := off.ForgeCredentialForTurn(context.Background(), row); !errors.Is(err, ErrPullRequestsDisabled) {
		t.Fatalf("policy off = %v", err)
	}
}

// Amendment 2026-10-26: pull requests narrow like any switch, and the
// approval only ever tightens.
func TestNarrowPullRequests(t *testing.T) {
	ceiling := platformconfig.DefaultCloudExecutorSettings()
	ceiling.Enabled, ceiling.ProjectDefaultEnabled = true, true
	if got := Narrow(ceiling, ProjectSetting{}, true, true); !got.PullRequests || !got.PullRequestApproval {
		t.Fatalf("defaults = %+v", got)
	}
	for name, tc := range map[string]struct {
		platformPR, platformApproval bool
		project                      ProjectSetting
		wantPR, wantApproval         bool
	}{
		"the project turns pull requests off":          {true, true, ProjectSetting{PullRequests: boolPtr(false)}, false, true},
		"the project cannot turn them on":              {false, true, ProjectSetting{PullRequests: boolPtr(true)}, false, true},
		"the project cannot waive a required approval": {true, true, ProjectSetting{PullRequestApproval: boolPtr(false)}, true, true},
		"the project may require one":                  {true, false, ProjectSetting{PullRequestApproval: boolPtr(true)}, true, true},
		"no approval when neither asks":                {true, false, ProjectSetting{}, true, false},
	} {
		settings := ceiling
		settings.PullRequests, settings.PullRequestApproval = tc.platformPR, tc.platformApproval
		if got := Narrow(settings, tc.project, true, true); got.PullRequests != tc.wantPR || got.PullRequestApproval != tc.wantApproval {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
