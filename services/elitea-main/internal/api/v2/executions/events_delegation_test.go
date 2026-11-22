package executions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type reviewerStub struct {
	review  delegationnotice.Review
	asked   [][3]int64
	askedBy int
}

func (r *reviewerStub) DelegationReview(_ context.Context, projectID, userID, callerID int64) (delegationnotice.Review, error) {
	r.askedBy++
	r.asked = append(r.asked, [3]int64{projectID, userID, callerID})
	return r.review, nil
}

const delegationFrame = `{"type":"delegation_refused","stream_id":"s","message_id":"m","response_metadata":{"delegation_v1":{"id":"call-1","kind":"refused","reason":"no_grant","code":"DELEGATION_GRANT_MISSING","edge":{"caller_id":2,"callee_id":1,"callee_name":"Agent1","kind":"agent_tool"},"missing":[{"kind":"toolkit","name":"Git"}]}}}`

func delegationRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "11")
	routeContext.URLParams.Add("executionID", "execution-1")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	return request.WithContext(auth.ContextWithAuthenticatedUser(
		ctx, auth.User{ID: "7", UserID: "7", AuthType: "user"}, auth.AuthenticationSourceSession))
}

// The refusal event reaches the SSE stream with the viewer's can_review
// (#2161): an editor gets true, a reader gets false and the names to ask, and
// the frame is never stored with either.
func TestEventHandlerStampsTheViewersReviewOnADelegationNotice(t *testing.T) {
	for name, c := range map[string]struct {
		review   delegationnotice.Review
		wantTrue bool
		editors  []string
	}{
		"an editor": {review: delegationnotice.Review{CanReview: true}, wantTrue: true},
		"a reader":  {review: delegationnotice.Review{Editors: []string{"Ada"}}, editors: []string{"Ada"}},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
				0: {
					{Cursor: 1, Type: nodeEventType, Data: json.RawMessage(delegationFrame)},
					{Cursor: 2, Type: nodeEventType, Data: json.RawMessage(delegationFrame)},
					{Cursor: 3, Type: nodeEventType, Data: json.RawMessage(`{"type":"agent_tool_end","response_metadata":{}}`)},
				},
			}}
			reviewer := &reviewerStub{review: c.review}
			handler, err := NewEventHandler(&eventAuthorizerStub{}, repository, &replayWaiterStub{})
			if err != nil {
				t.Fatal(err)
			}
			handler.WithDelegationReviewer(reviewer)
			response := newStreamingRecorder()
			handler.Stream(response, delegationRequest())

			var notices []map[string]any
			for _, line := range strings.Split(response.Body.String(), "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok {
					continue
				}
				var frame struct {
					Type             string `json:"type"`
					ResponseMetadata struct {
						Notice map[string]any `json:"delegation_v1"`
					} `json:"response_metadata"`
				}
				if err := json.Unmarshal([]byte(data), &frame); err != nil {
					t.Fatalf("%s: %v", data, err)
				}
				if frame.Type == "delegation_refused" {
					notices = append(notices, frame.ResponseMetadata.Notice)
				}
			}
			if len(notices) != 2 {
				t.Fatalf("notices = %d in %s", len(notices), response.Body.String())
			}
			for _, notice := range notices {
				if notice["can_review"] != c.wantTrue {
					t.Fatalf("can_review = %v", notice["can_review"])
				}
				got, _ := notice["editors"].([]any)
				if len(got) != len(c.editors) {
					t.Fatalf("editors = %v", notice["editors"])
				}
			}
			// Asked once for the calling agent, as the viewer, in the project.
			if len(reviewer.asked) != 1 || reviewer.asked[0] != [3]int64{11, 7, 2} {
				t.Fatalf("asked = %v", reviewer.asked)
			}
			if strings.Contains(response.Body.String(), `"type":"agent_tool_end","response_metadata":{"delegation_v1"`) {
				t.Fatal("an unrelated frame was stamped")
			}
		})
	}
}

// Without a reviewer the notice is served as it was stored.
func TestEventHandlerServesADelegationNoticeUnstampedWithoutAReviewer(t *testing.T) {
	repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
		0: {{Cursor: 1, Type: nodeEventType, Data: json.RawMessage(delegationFrame)}},
	}}
	handler, err := NewEventHandler(&eventAuthorizerStub{}, repository, &replayWaiterStub{})
	if err != nil {
		t.Fatal(err)
	}
	response := newStreamingRecorder()
	handler.Stream(response, delegationRequest())
	if strings.Contains(response.Body.String(), "can_review") {
		t.Fatalf("stamped without a reviewer: %s", response.Body.String())
	}
}

// FU-360: an assembly-time pipeline_node delegation_refused event (for both an
// Agent node and a #2235 saved-pipeline node) reaches the SSE stream stamped
// with can_review and the calling pipeline's name BEFORE execution.failed.
func TestEventHandlerStreamsAssemblyPipelineNodeRefusalBeforeExecutionFailed(t *testing.T) {
	const (
		agentNodeFrame = `{"type":"delegation_refused","stream_id":"s","message_id":"m","execution_generation":"g1","response_metadata":{"delegation_v1":{"id":"pipeline_node:delegate","kind":"refused","reason":"no_grant","code":"DELEGATION_GRANT_MISSING","edge":{"caller_id":174,"callee_id":171,"callee_name":"delegate","kind":"pipeline_node"},"missing":[{"kind":"toolkit","name":"c1mockmcp"}]}}}`
		failedFrame    = `{"code":"AUTHORIZATION_FAILED","safe_message":"Execution authorization failed.","retryable":false}`
	)
	repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
		0: {
			{Cursor: 1, Type: nodeEventType, Data: json.RawMessage(agentNodeFrame)},
			{Cursor: 2, Type: "execution.failed", Data: json.RawMessage(failedFrame)},
		},
	}}
	reviewer := &reviewerStub{review: delegationnotice.Review{CanReview: true, CallerName: "P0r refusal"}}
	handler, err := NewEventHandler(&eventAuthorizerStub{}, repository, &replayWaiterStub{})
	if err != nil {
		t.Fatal(err)
	}
	handler.WithDelegationReviewer(reviewer)
	response := newStreamingRecorder()
	handler.Stream(response, delegationRequest())

	var events []string
	var stampedNotice map[string]any
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, ev)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(data, "delegation_v1") {
			var frame struct {
				ResponseMetadata struct {
					Notice map[string]any `json:"delegation_v1"`
				} `json:"response_metadata"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			stampedNotice = frame.ResponseMetadata.Notice
		}
	}
	if len(events) != 2 || events[0] != nodeEventType || events[1] != "execution.failed" {
		t.Fatalf("events = %v, want [%s execution.failed]", events, nodeEventType)
	}
	if stampedNotice == nil || stampedNotice["can_review"] != true {
		t.Fatalf("stamped notice = %+v, want can_review=true", stampedNotice)
	}
	edge, _ := stampedNotice["edge"].(map[string]any)
	if edge["caller_name"] != "P0r refusal" || edge["kind"] != "pipeline_node" {
		t.Fatalf("edge = %+v, want caller_name='P0r refusal' and kind='pipeline_node'", edge)
	}
	if len(reviewer.asked) != 1 || reviewer.asked[0] != [3]int64{11, 7, 174} {
		t.Fatalf("asked = %v, want [[11 7 174]]", reviewer.asked)
	}
}
