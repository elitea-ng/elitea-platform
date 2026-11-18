package conversations_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type reviewerFake struct {
	review delegationnotice.Review
	asked  [][3]int64
}

func (r *reviewerFake) DelegationReview(_ context.Context, project, user, caller int64) (delegationnotice.Review, error) {
	r.asked = append(r.asked, [3]int64{project, user, caller})
	return r.review, nil
}

type servedMessages struct {
	Items []struct {
		Metadata map[string]any `json:"metadata"`
	} `json:"items"`
}

// A reloaded transcript serves each delegation notice with what the viewer may
// do about it (#2161): can_review for an editor, the people to ask otherwise,
// one question per calling agent, and the stored notice is left as it was.
func TestListMessagesStampsTheViewersReviewOnDelegationNotices(t *testing.T) {
	notice := func(id string) map[string]any {
		return map[string]any{
			"id": id, "kind": "refused", "reason": "no_grant",
			"edge": map[string]any{"caller_id": float64(2), "callee_name": "Agent1", "kind": "agent_tool"},
		}
	}
	for name, c := range map[string]struct {
		review  delegationnotice.Review
		want    bool
		editors int
	}{
		"an editor": {review: delegationnotice.Review{CanReview: true}, want: true},
		"a reader":  {review: delegationnotice.Review{Editors: []string{"Ada"}}, editors: 1},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &mockRepo{
				listMessagesFn: func(context.Context, string, string, conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
					return conversations.MessagesListResponse{Items: []conversations.Message{
						{ID: "m1", Metadata: map[string]any{delegationnotice.MetaKey: []any{notice("a"), notice("b")}}},
						{ID: "m2", Metadata: map[string]any{"grounding": true}},
						{ID: "m3"},
					}}, nil
				},
			}
			reviewer := &reviewerFake{review: c.review}
			router := newRouter(conversations.NewHandler(repo).WithDelegationReviewer(reviewer))
			request := httptest.NewRequest(http.MethodGet, "/projects/11/conversations/conv-1/messages", nil)
			request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "7", UserID: "7"}))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			var body servedMessages
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			stamped := body.Items[0].Metadata[delegationnotice.MetaKey].([]any)
			if len(stamped) != 2 {
				t.Fatalf("notices = %v", stamped)
			}
			for _, entry := range stamped {
				got := entry.(map[string]any)
				editors, _ := got["editors"].([]any)
				if got["can_review"] != c.want || len(editors) != c.editors {
					t.Fatalf("notice = %v", got)
				}
			}
			if len(reviewer.asked) != 1 || reviewer.asked[0] != [3]int64{11, 7, 2} {
				t.Fatalf("asked = %v", reviewer.asked)
			}
			if body.Items[1].Metadata["grounding"] != true || body.Items[2].Metadata != nil {
				t.Fatalf("other messages changed: %+v", body.Items[1:])
			}
		})
	}
}

// Without a reviewer, or a viewer, the notice is served as stored.
func TestListMessagesServesNoticesUnstampedWithoutAReviewerOrViewer(t *testing.T) {
	repo := &mockRepo{
		listMessagesFn: func(context.Context, string, string, conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
			return conversations.MessagesListResponse{Items: []conversations.Message{
				{ID: "m1", Metadata: map[string]any{delegationnotice.MetaKey: []any{map[string]any{"id": "a"}}}},
			}}, nil
		},
	}
	for name, handler := range map[string]*conversations.Handler{
		"no reviewer": conversations.NewHandler(repo),
		"no viewer":   conversations.NewHandler(repo).WithDelegationReviewer(&reviewerFake{}),
	} {
		response := httptest.NewRecorder()
		newRouter(handler).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/projects/11/conversations/c/messages", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, response.Code)
		}
		var body servedMessages
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		first := body.Items[0].Metadata[delegationnotice.MetaKey].([]any)[0].(map[string]any)
		if _, stamped := first["can_review"]; stamped {
			t.Fatalf("%s: stamped", name)
		}
	}
}
