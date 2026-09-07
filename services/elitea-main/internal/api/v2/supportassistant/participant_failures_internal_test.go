package supportassistant

// THE FAILURE DIRECTIONS.
//
// Every write in participants.go can fail, and each failure has one correct
// direction. The rule is the same everywhere: a turn whose participant rows are
// not right must be REFUSED, never started. A run addressed to a participant
// that was never attached, or to a mapping the resolver will reject, produces a
// 502 from three packages away instead of one sentence here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestAFailedParticipantWriteStopsTheTurn(t *testing.T) {
	settings := platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}

	t.Run("the author cannot be attached", func(t *testing.T) {
		handler := NewHandler(nil)
		handler.chat = &fakeChatStore{addErr: errors.New("write refused")}
		if _, err := handler.ensureTurnParticipants(context.Background(), settings, 1, 11); err == nil {
			t.Fatal("a failed author attach was survived")
		}
	})

	t.Run("the participants read fails", func(t *testing.T) {
		handler := NewHandler(nil)
		handler.chat = &fakeChatStore{listErr: errors.New("read failed")}
		if err := handler.ensureUserParticipant(context.Background(), "7", "1", 7, 11); err == nil {
			t.Fatal("a failed read reported success")
		}
	})

	t.Run("the agent cannot be attached", func(t *testing.T) {
		handler := NewHandler(nil)
		handler.chat = &fakeChatStore{addErr: errors.New("write refused")}
		if _, err := handler.attachAgentParticipant(context.Background(), settings, "1",
			agentVersion{ID: 41, AgentType: "openai"}); err == nil {
			t.Fatal("a failed agent attach was survived")
		}
	})
}

// A REPAIR THAT CANNOT BE WRITTEN REFUSES THE TURN.
func TestAFailedRepairRefusesTheTurn(t *testing.T) {
	handler := NewHandler(nil)
	handler.chat = &fakeChatStore{
		participants: []conversations.Participant{
			{ID: 4, EntityName: applicationEntityName,
				EntityMeta: map[string]any{"id": float64(31), "project_id": float64(7)}},
		},
		updateErr: errors.New("update refused"),
	}
	if _, err := handler.attachAgentParticipant(context.Background(),
		platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}, "1",
		agentVersion{ID: 41, AgentType: "openai"}); err == nil {
		t.Fatal("a failed repair still started the turn")
	}
}

// THE REPAIR TARGETS THE PARTICIPANT IT FOUND, and attaches nothing new.
func TestTheRepairTargetsTheParticipantItFound(t *testing.T) {
	handler := NewHandler(nil)
	store := &fakeChatStore{participants: []conversations.Participant{
		{ID: 4, EntityName: applicationEntityName,
			EntityMeta:     map[string]any{"id": float64(31), "project_id": float64(7)},
			EntitySettings: map[string]any{}},
	}}
	handler.chat = store

	id, err := handler.attachAgentParticipant(context.Background(),
		platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}, "1",
		agentVersion{ID: 41, AgentType: "openai"})
	if err != nil || id != 4 {
		t.Fatalf("attach = (%d, %v), want (4, nil)", id, err)
	}
	if len(store.added) != 0 {
		t.Fatal("an existing participant was attached a second time")
	}
	if len(store.updatedIDs) != 1 || store.updatedIDs[0] != "4" {
		t.Fatalf("the repair updated %v, want participant 4", store.updatedIDs)
	}
	if store.updated[0]["version_id"] != "41" {
		t.Fatalf("the repair wrote %+v", store.updated[0])
	}
}

// AN ATTACH THAT REPORTS SUCCESS AND LEAVES NO ROW REFUSES. A turn addressed to
// participant 0 would run the question against nothing.
func TestAnAttachThatLeavesNoRowRefuses(t *testing.T) {
	handler := NewHandler(nil)
	handler.chat = &vanishingChatStore{}
	if _, err := handler.attachAgentParticipant(context.Background(),
		platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}, "1",
		agentVersion{ID: 41, AgentType: "openai"}); err == nil {
		t.Fatal("a participant that is not there was accepted")
	}
}

// vanishingChatStore accepts an attach and then reports no participants, which
// is what a lost transaction looks like from here.
type vanishingChatStore struct{ fakeChatStore }

func (v *vanishingChatStore) AddParticipant(_ context.Context, _, _ string, _ map[string]any) error {
	return nil
}

// THE VERSION READ REFUSES WITHOUT A POOL rather than dereferencing nil.
func TestTheVersionReadRefusesWithoutAPool(t *testing.T) {
	s := &store{}
	if _, err := s.agentVersionOf(context.Background(), 7, 31); !errors.Is(err, errAgentVersionNotFound) {
		t.Fatalf("err = %v, want errAgentVersionNotFound", err)
	}
}

// AN EMPTY PAGE CONTEXT ADDS NO FENCE. A widget that sends `{}` must not make
// every question carry a marker the agent has to ignore.
func TestAnEmptyPageContextAddsNothing(t *testing.T) {
	handler := NewHandler(nil)
	if got := handler.composeUserInput(PredictRequest{Content: "why?", Context: &AssistantContext{}}); got != "why?" {
		t.Fatalf("composeUserInput = %q, want %q", got, "why?")
	}
}

// THE CONFIG ROUTE ANSWERS `false` FOR A PRINCIPAL THAT OWNS NO USER.
func TestConfigAnswersDisabledForAPrincipalWithNoOwningUser(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil).Config(recorder,
		withUser(httptest.NewRequest(http.MethodGet, "/config/", nil), auth.User{ID: "", UserID: ""}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"enabled":false`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

// THE MIDDLEWARE REFUSES AN UNAUTHENTICATED CALLER BEFORE IT READS ANYTHING.
// `resolve` runs first in the chain, so this is the outermost refusal.
func TestResolveRefusesAnUnauthenticatedCaller(t *testing.T) {
	routes := NewHandler(nil, WithPermissionResolver(stubResolver{})).Routes()
	for name, request := range map[string]*http.Request{
		"no user": httptest.NewRequest(http.MethodGet, "/conversations/", nil),
		"no owning user": withUser(httptest.NewRequest(http.MethodGet, "/conversations/", nil),
			auth.User{ID: "", UserID: ""}),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			routes.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// A JSON NUMBER READS AS A NUMBER, and a value that is not a number does not.
func TestAJSONNumberIsReadAsANumber(t *testing.T) {
	value, ok := metaValueInt(json.Number("31"))
	if !ok || value != 31 {
		t.Fatalf("metaValueInt(json.Number) = (%d, %v), want (31, true)", value, ok)
	}
	if _, ok := metaValueInt(json.Number("not a number")); ok {
		t.Fatal("an unparseable json.Number was accepted")
	}
	if _, ok := metaValueInt(true); ok {
		t.Fatal("a boolean was read as a number")
	}
}
