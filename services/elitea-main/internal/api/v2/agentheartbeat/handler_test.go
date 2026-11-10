package agentheartbeat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/heartbeat"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakeStore struct {
	platform   platformconfig.ProactivitySettings
	governance repos.HeartbeatGovernance
	release    *heartbeat.Release
	saved      []repos.HeartbeatSettingsUpdate
	// stored is a row saved by somebody else before the test.
	stored *repos.HeartbeatSettings
}

func (f *fakeStore) GetHeartbeatSettings(context.Context, int64, int64) (repos.HeartbeatSettings, error) {
	if len(f.saved) == 0 {
		if f.stored != nil {
			return *f.stored, nil
		}
		return repos.HeartbeatSettings{}, repos.ErrHeartbeatNotFound
	}
	last := f.saved[len(f.saved)-1]
	return repos.HeartbeatSettings{ApplicationID: 31, Enabled: last.Enabled, OwnerUserID: last.OwnerUserID, MandateID: last.MandateID}, nil
}

func (f *fakeStore) SaveHeartbeatSettings(_ context.Context, _, _ int64, update repos.HeartbeatSettingsUpdate) (repos.HeartbeatSettings, error) {
	f.saved = append(f.saved, update)
	return repos.HeartbeatSettings{}, nil
}

func (f *fakeStore) ListHeartbeats(context.Context, int64, int64, int, int) ([]repos.HeartbeatEntry, int, error) {
	return []repos.HeartbeatEntry{}, 0, nil
}

func (f *fakeStore) HeartbeatGovernanceOf(context.Context, int64, int64, int64) (repos.HeartbeatGovernance, error) {
	return f.governance, nil
}

func (f *fakeStore) CurrentRelease(context.Context, heartbeat.Agent) (heartbeat.Release, bool, error) {
	if f.release == nil {
		return heartbeat.Release{}, false, nil
	}
	return *f.release, true, nil
}

func (f *fakeStore) ProjectEnabled(context.Context, int64) (bool, error)         { return true, nil }
func (f *fakeStore) SetProjectEnabled(context.Context, int64, int64, bool) error { return nil }
func (f *fakeStore) Settings(context.Context) (platformconfig.ProactivitySettings, error) {
	return f.platform, nil
}

func put(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, "/agent_heartbeat/prompt_lib/1/31", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", "1")
	route.URLParams.Add("applicationID", "31")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: "11", UserID: "11"})
	recorder := httptest.NewRecorder()
	h.Put(recorder, request.WithContext(ctx))
	return recorder
}

func heartbeatRelease() *heartbeat.Release {
	return &heartbeat.Release{ReleaseID: 5, VersionID: 42, Reviewed: true,
		Meta: map[string]any{"proactivity": map[string]any{"mode": "heartbeat"}}}
}

func TestPutRefusesALowTierTeamAgent(t *testing.T) {
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierLow}, release: heartbeatRelease()}
	recorder := put(t, NewHandler(store, nil), `{"enabled":true}`)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), CodeTierTooLow) {
		t.Fatalf("answer = %d %s", recorder.Code, recorder.Body.String())
	}
	if len(store.saved) != 0 {
		t.Fatal("a refused save was stored")
	}
}

func TestPutLetsAPersonalProjectSelfApprove(t *testing.T) {
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierLow, Personal: true}, release: heartbeatRelease()}
	recorder := put(t, NewHandler(store, nil), `{"enabled":true}`)
	if recorder.Code != http.StatusOK || len(store.saved) != 1 || store.saved[0].OwnerUserID != 11 {
		t.Fatalf("answer = %d %s, saved %+v", recorder.Code, recorder.Body.String(), store.saved)
	}
	if !strings.Contains(recorder.Body.String(), `"acts_per_day":2`) {
		t.Fatalf("the personal act ceiling is not shown: %s", recorder.Body.String())
	}
	// Off on the platform: refused.
	store.platform.PersonalSelfApprove = false
	if recorder := put(t, NewHandler(store, nil), `{"enabled":true}`); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("self-approve off = %d", recorder.Code)
	}
}

func TestPutRefusesWithoutAHeartbeatRelease(t *testing.T) {
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierMedium}}
	if recorder := put(t, NewHandler(store, nil), `{"enabled":true}`); !strings.Contains(recorder.Body.String(), CodeNoRelease) {
		t.Fatalf("answer = %s", recorder.Body.String())
	}
	store.release = &heartbeat.Release{ReleaseID: 5, VersionID: 42, Reviewed: true, Meta: map[string]any{}}
	if recorder := put(t, NewHandler(store, nil), `{"enabled":true}`); !strings.Contains(recorder.Body.String(), CodeBlockOff) {
		t.Fatalf("answer = %s", recorder.Body.String())
	}
	// Turning it off is always allowed.
	if recorder := put(t, NewHandler(store, nil), `{"enabled":false}`); recorder.Code != http.StatusOK {
		t.Fatalf("disable = %d", recorder.Code)
	}
}

func TestPutChecksTheMandate(t *testing.T) {
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierMedium}, release: heartbeatRelease()}
	mandates := runmandatetest.New()
	mandates.PutBinding("binding-1", 1, 42)
	mine, other := "6b0f9e4c-2a1d-4c3b-9e8f-7a6b5c4d3e2f", "7d1f6a52-0c55-4d5e-9a3e-6f0b1c2d3e4f"
	mandates.PutMandate(mine, 1, mandate.KindDelegatedBackground, "binding-1", 11)
	mandates.PutMandate(other, 1, mandate.KindAutonomous, "binding-1", 0)
	h := NewHandler(store, mandates.Service())
	if recorder := put(t, h, `{"enabled":true,"mandate_id":"`+other+`"}`); !strings.Contains(recorder.Body.String(), CodeMandateNotOwner) {
		t.Fatalf("an autonomous mandate = %s", recorder.Body.String())
	}
	if recorder := put(t, h, `{"enabled":true,"mandate_id":"`+mine+`"}`); recorder.Code != http.StatusOK {
		t.Fatalf("the owner's mandate = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := put(t, NewHandler(store, nil), `{"enabled":true,"mandate_id":"`+mine+`"}`); !strings.Contains(recorder.Body.String(), CodeMandateRefused) {
		t.Fatalf("governance off = %s", recorder.Body.String())
	}
}

// The save applies the scan's review rule: a team project's release that
// did not pass a reviewed change request cannot turn the heartbeat on,
// which would otherwise read as on and never wake.
func TestPutRefusesAnUnreviewedTeamRelease(t *testing.T) {
	release := heartbeatRelease()
	release.Reviewed = false
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierMedium}, release: release}
	recorder := put(t, NewHandler(store, nil), `{"enabled":true}`)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), CodeNotReviewed) {
		t.Fatalf("answer = %d %s", recorder.Code, recorder.Body.String())
	}
	// A personal project self-approves: no review needed.
	store.governance.Personal = true
	if recorder := put(t, NewHandler(store, nil), `{"enabled":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("personal = %d %s", recorder.Code, recorder.Body.String())
	}
}

// Another person cannot take over a heartbeat that is on (and its private
// home thread) unless they own the agent; anyone may turn it off.
func TestPutKeepsAnEnabledHeartbeatWithItsOwner(t *testing.T) {
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierMedium}, release: heartbeatRelease(),
		stored: &repos.HeartbeatSettings{ApplicationID: 31, Enabled: true, OwnerUserID: 99}}
	recorder := put(t, NewHandler(store, nil), `{"enabled":true}`)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), CodeNotOwner) {
		t.Fatalf("takeover = %d %s", recorder.Code, recorder.Body.String())
	}
	if len(store.saved) != 0 {
		t.Fatal("a refused takeover was stored")
	}
	if recorder := put(t, NewHandler(store, nil), `{"enabled":false}`); recorder.Code != http.StatusOK ||
		len(store.saved) != 1 || store.saved[0].OwnerUserID != 99 {
		t.Fatalf("disable = %d, saved %+v", recorder.Code, store.saved)
	}
	store.saved = nil
	store.governance.Owner = true
	if recorder := put(t, NewHandler(store, nil), `{"enabled":true}`); recorder.Code != http.StatusOK ||
		len(store.saved) != 1 || store.saved[0].OwnerUserID != 11 {
		t.Fatalf("an agent owner's takeover = %d, saved %+v", recorder.Code, store.saved)
	}
}

// A mandate binds the released version: with no release it is refused,
// not checked against version 0.
func TestPutRefusesAMandateWithoutARelease(t *testing.T) {
	store := &fakeStore{platform: platformconfig.DefaultProactivitySettings(),
		governance: repos.HeartbeatGovernance{Tier: agentlifecycle.TierMedium}}
	mandates := runmandatetest.New()
	recorder := put(t, NewHandler(store, mandates.Service()), `{"enabled":false,"mandate_id":"6b0f9e4c-2a1d-4c3b-9e8f-7a6b5c4d3e2f"}`)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), CodeNoRelease) {
		t.Fatalf("answer = %d %s", recorder.Code, recorder.Body.String())
	}
}
