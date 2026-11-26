package localturns

// handLocalFloor and skipLocalFloorHands (ADR-0061 decision 3c, issue
// 3063, client contract 1.68): a desktop agent hands the floor to other
// agents of its thread, and the person stops or skips the queued hand-offs.
// See internal/application/localturn/handoff.go for the rules. External
// client routes (desktop -> Main over public HTTPS), like every local-turn
// operation: no internal call is added.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

const (
	// HandFloorPath is handLocalFloor: {executionID} is the calling live turn.
	HandFloorPath = "/api/v2/elitea_core/local_turn_hand_floor/prompt_lib/{projectID}/{executionID}"
	// HandSkipPath is skipLocalFloorHands: {questionID} is the person's message.
	HandSkipPath = "/api/v2/elitea_core/local_turn_hand_floor_skip/prompt_lib/{projectID}/{questionID}"

	maxHandBody = int64(16 * 1024)
)

// Hands is *localturn.Hands.
type Hands interface {
	Hand(context.Context, localturn.HandRequest) (localturn.HandResult, error)
	Skip(ctx context.Context, projectID, actorUserID int64, questionID string, handIDs []string, code string) ([]string, error)
}

var _ Hands = (*localturn.Hands)(nil)

// WithHands serves handLocalFloor and skipLocalFloorHands. Without it, both
// answer 503 local_turn_unavailable.
func (route *Route) WithHands(hands Hands) *Route {
	if route != nil && route.h != nil && hands != nil {
		route.h.hands = hands
	}
	return route
}

type handBody struct {
	ParticipantIDs []int64 `json:"participant_ids"`
	Message        string  `json:"message"`
	Consults       []struct {
		From int64 `json:"from"`
		To   int64 `json:"to"`
	} `json:"consults"`
}

type skipBody struct {
	HandIDs []string `json:"hand_ids"`
	Code    string   `json:"code"`
}

func (h *handler) handFloor(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return
	}
	user, actor, ok := caller(writer, request)
	if !ok {
		return
	}
	var body handBody
	if !decodeBody(writer, request, maxHandBody, &body) {
		return
	}
	if h.hands == nil {
		writeUseCaseError(request.Context(), writer, "hand_floor", localturn.ErrUnavailable)
		return
	}
	consults := make([]turntaking.Turn, 0, len(body.Consults))
	for _, consult := range body.Consults {
		consults = append(consults, turntaking.Turn{From: consult.From, To: consult.To})
	}
	result, err := h.hands.Hand(request.Context(), localturn.HandRequest{
		ProjectID: projectID, ActorUserID: actor,
		Credential:  localturn.Credential{TokenID: user.TokenID, NativeClientID: user.NativeClientID},
		ExecutionID: chi.URLParam(request, "executionID"), ParticipantIDs: body.ParticipantIDs,
		Message: body.Message, Consults: consults,
	})
	if err != nil {
		writeUseCaseError(request.Context(), writer, "hand_floor", err)
		return
	}
	handed := make([]map[string]any, 0, len(result.HandedTo))
	for _, agent := range result.HandedTo {
		handed = append(handed, map[string]any{
			"participant_id": agent.ParticipantID, "name": agent.Name, "hand_id": agent.HandID,
		})
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{
		"schema_version": localturn.HandFloorSchemaVersion,
		"handed_to":      handed,
		"stopped":        result.Stopped,
		"message":        result.Message,
	})
}

func (h *handler) skipHands(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return
	}
	_, actor, ok := caller(writer, request)
	if !ok {
		return
	}
	var body skipBody
	if !decodeBody(writer, request, maxHandBody, &body) {
		return
	}
	if h.hands == nil {
		writeUseCaseError(request.Context(), writer, "hand_floor_skip", localturn.ErrUnavailable)
		return
	}
	skipped, err := h.hands.Skip(request.Context(), projectID, actor, chi.URLParam(request, "questionID"), body.HandIDs, body.Code)
	if err != nil {
		writeUseCaseError(request.Context(), writer, "hand_floor_skip", err)
		return
	}
	if skipped == nil {
		skipped = []string{}
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{"skipped": skipped})
}
