package localturns

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

type fakeHands struct {
	request  localturn.HandRequest
	result   localturn.HandResult
	err      error
	skipCode string
	skipped  []string
}

func (f *fakeHands) Hand(_ context.Context, request localturn.HandRequest) (localturn.HandResult, error) {
	f.request = request
	return f.result, f.err
}

func (f *fakeHands) Skip(_ context.Context, _, _ int64, _ string, _ []string, code string) ([]string, error) {
	f.skipCode = code
	return f.skipped, f.err
}

const (
	handURL = "/api/v2/elitea_core/local_turn_hand_floor/prompt_lib/1/0123456789abcdef0123456789abcdef"
	skipURL = "/api/v2/elitea_core/local_turn_hand_floor_skip/prompt_lib/1/6f1c2d3e-4a5b-4c6d-8e7f-901234567890"
)

func TestHandFloorPassesTheTurnAndAnswersTheWorkersShape(t *testing.T) {
	hands := &fakeHands{result: localturn.HandResult{
		HandedTo: []localturn.HandedAgent{{ParticipantID: 88, Name: "CI Doctor", HandID: "5d6f0c1e-4c2a-4c1b-9a51-0c2d1d6b2a29"}},
		Message:  "Handed the floor.",
	}}
	h := &handler{hands: hands}
	response := serve(t, h.handFloor, tokenUser(), handURL, HandFloorPath,
		`{"participant_ids":[88],"message":"take lint","consults":[{"from":77,"to":99}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body)
	}
	if hands.request.ExecutionID != "0123456789abcdef0123456789abcdef" || hands.request.Credential.TokenID != "70" ||
		hands.request.ActorUserID != 7 || len(hands.request.Consults) != 1 ||
		hands.request.Consults[0] != (turntaking.Turn{From: 77, To: 99}) {
		t.Fatalf("request = %+v", hands.request)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["schema_version"] != "elitea.runtime.hand-floor.v1" ||
		body["stopped"] != "" || len(body["handed_to"].([]any)) != 1 {
		t.Fatalf("body = %s", response.Body)
	}
}

func TestHandFloorErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		h      *handler
		user   *auth.User
		body   string
		status int
		code   string
	}{
		{"unconfigured", &handler{}, tokenUser(), `{}`, http.StatusServiceUnavailable, "local_turn_unavailable"},
		{"a session", &handler{hands: &fakeHands{}}, &auth.User{ID: "7", UserID: "7", AuthType: "session"}, `{}`,
			http.StatusForbidden, "local_turn_requires_token"},
		{"not live", &handler{hands: &fakeHands{err: localturn.ErrNotLive}}, tokenUser(), `{}`, http.StatusConflict, "local_turn_not_live"},
		{"a cloud member", &handler{hands: &fakeHands{err: localturn.ErrFloorTurn}}, tokenUser(), `{}`, http.StatusConflict, "floor_turn_refused"},
		{"superseded", &handler{hands: &fakeHands{err: localturn.ErrFloorSuperseded}}, tokenUser(), `{}`, http.StatusConflict, "floor_turn_superseded"},
		{"not found", &handler{hands: &fakeHands{err: localturn.ErrNotFound}}, tokenUser(), `{}`, http.StatusNotFound, "local_turn_not_found"},
		{"invalid", &handler{hands: &fakeHands{err: localturn.ErrInvalid}}, tokenUser(), `{}`, http.StatusBadRequest, "invalid_local_turn"},
		{"disabled", &handler{hands: &fakeHands{err: localturn.ErrLocalWorkDisabled}}, tokenUser(), `{}`, http.StatusForbidden, "local_work_disabled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			response := serve(t, c.h.handFloor, c.user, handURL, HandFloorPath, c.body)
			if response.Code != c.status || !containsCode(response.Body.Bytes(), c.code) {
				t.Fatalf("= %d %s, want %d %s", response.Code, response.Body, c.status, c.code)
			}
		})
	}
}

func TestSkipHandsAnswersAnArray(t *testing.T) {
	hands := &fakeHands{}
	h := &handler{hands: hands}
	response := serve(t, h.skipHands, tokenUser(), skipURL, HandSkipPath,
		`{"hand_ids":["5d6f0c1e-4c2a-4c1b-9a51-0c2d1d6b2a29"],"code":"skipped_by_person"}`)
	if response.Code != http.StatusOK || response.Body.String() != "{\"skipped\":[]}\n" || hands.skipCode != "skipped_by_person" {
		t.Fatalf("= %d %q code %q", response.Code, response.Body, hands.skipCode)
	}
	response = serve(t, (&handler{}).skipHands, tokenUser(), skipURL, HandSkipPath, `{}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d", response.Code)
	}
}
