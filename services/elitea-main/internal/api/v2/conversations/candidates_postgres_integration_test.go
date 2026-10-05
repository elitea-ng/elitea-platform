package conversations_test

// Real-PostgreSQL coverage for listParticipantCandidates (client contract
// 1.1): who is offered, how `q` filters, that paging is stable, and that a
// caller outside the project or the conversation is refused.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type candidatesPage struct {
	Rows       []conversations.ParticipantCandidate `json:"rows"`
	NextCursor *string                              `json:"next_cursor"`
	HasMore    bool                                 `json:"has_more"`
}

func candidatesFixture(t *testing.T) (*pgxpool.Pool, http.Handler, string) {
	t.Helper()
	pool := newChatAuthorityPool(t)
	// Project 1's members are users 7, 8 and 9 (newChatAuthorityPool). Add a
	// platform system account, a suspended member, a fourth ordinary member
	// with no name, and a user of another project only.
	if _, err := pool.Exec(context.Background(), `
CREATE TABLE public.auth_core__user(id integer PRIMARY KEY, email text, name text, suspended boolean NOT NULL DEFAULT false);
INSERT INTO public.auth_core__user VALUES
  (7, 'alice@example.com', 'Alice Smith', false),
  (8, 'bob@example.com', 'Bob Jones', false),
  (9, 'anna@example.com', 'Anna Lee', false),
  (10, 'system_1@centry.user', 'system', false),
  (11, 'gone@example.com', 'Anne Gone', true),
  (12, 'zed@example.com', '', false),
  (13, 'other@example.com', 'Anders Other', false);
INSERT INTO public.auth_core__project_user_role VALUES (1,10,2),(1,11,2),(1,12,2),(2,13,3);
`); err != nil {
		t.Fatal(err)
	}
	repo := repos.NewConversationsRepo(pool)
	handler := conversations.NewHandler(repo).WithPool(pool).
		WithParticipantCandidates(repos.NewParticipantCandidatesRepo(pool))
	router := chi.NewRouter()
	router.Post("/{projectID}/conversations/{conversationID}/participants", handler.AddParticipant)
	router.Get("/{projectID}/conversations/{conversationID}/candidates", handler.ListParticipantCandidates)

	created, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Candidates"})
	if err != nil {
		t.Fatal(err)
	}
	return pool, router, created.ID
}

func listCandidates(t *testing.T, router http.Handler, actor, conversationID string, query url.Values, want int) candidatesPage {
	t.Helper()
	path := fmt.Sprintf("/1/conversations/%s/candidates?%s", conversationID, query.Encode())
	recorder := callChatAuthority(t, router, actor, http.MethodGet, path, "", want)
	var page candidatesPage
	if want == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
	}
	return page
}

func candidateIDs(page candidatesPage) []int64 {
	ids := make([]int64, 0, len(page.Rows))
	for _, row := range page.Rows {
		ids = append(ids, row.UserID)
	}
	return ids
}

func TestParticipantCandidatesOfferProjectMembersOnly(t *testing.T) {
	_, router, conversationID := candidatesFixture(t)

	page := listCandidates(t, router, "7", conversationID, url.Values{}, http.StatusOK)
	// Ordered by lower(name), the e-mail standing in for an empty name: Alice,
	// Anna, Bob, then zed@example.com. The system account (10), the suspended
	// member (11) and the other project's user (13) are not offered.
	if got := fmt.Sprint(candidateIDs(page)); got != "[7 9 8 12]" {
		t.Fatalf("candidates = %s, want [7 9 8 12]", got)
	}
	if page.HasMore || page.NextCursor != nil {
		t.Fatalf("a single page reports has_more=%v next_cursor=%v", page.HasMore, page.NextCursor)
	}
	// The author is already a participant; nobody else is.
	for _, row := range page.Rows {
		if row.AlreadyParticipant != (row.UserID == 7) {
			t.Fatalf("row %+v: already_participant wrong", row)
		}
	}

	// Adding Bob flips his flag.
	callChatAuthority(t, router, "7", http.MethodPost,
		fmt.Sprintf("/1/conversations/%s/participants", conversationID),
		`{"entity_name":"user","entity_meta":{"id":8,"user_name":"Bob Jones"}}`, http.StatusOK)
	page = listCandidates(t, router, "7", conversationID, url.Values{"q": {"bob"}}, http.StatusOK)
	if len(page.Rows) != 1 || page.Rows[0].UserID != 8 || !page.Rows[0].AlreadyParticipant {
		t.Fatalf("after adding Bob, q=bob answered %+v", page.Rows)
	}
}

func TestParticipantCandidatesFilterByNameOrEmail(t *testing.T) {
	_, router, conversationID := candidatesFixture(t)

	cases := map[string]string{
		"AN":          "[9]", // name substring, case-insensitive: Anna (Anne is suspended)
		"example.com": "[7 9 8 12]",
		"zed@":        "[12]", // e-mail substring
		"%":           "[]",   // a LIKE wildcard is matched literally
		"_":           "[]",
	}
	for q, want := range cases {
		page := listCandidates(t, router, "7", conversationID, url.Values{"q": {q}}, http.StatusOK)
		if got := fmt.Sprint(candidateIDs(page)); got != want {
			t.Errorf("q=%q answered %s, want %s", q, got, want)
		}
	}
}

func TestParticipantCandidatesPageStably(t *testing.T) {
	_, router, conversationID := candidatesFixture(t)

	var seen []int64
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
		query := url.Values{"limit": {"1"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		page := listCandidates(t, router, "7", conversationID, query, http.StatusOK)
		seen = append(seen, candidateIDs(page)...)
		if !page.HasMore {
			if page.NextCursor != nil {
				t.Fatalf("the last page carries a cursor %q", *page.NextCursor)
			}
			break
		}
		if page.NextCursor == nil || len(page.Rows) != 1 {
			t.Fatalf("a non-final page: %+v", page)
		}
		cursor = *page.NextCursor
	}
	if got := fmt.Sprint(seen); got != "[7 9 8 12]" {
		t.Fatalf("paging one at a time saw %s, want [7 9 8 12] with no repeat or gap", got)
	}
}

func TestParticipantCandidatesRefuseBadInputAndOutsiders(t *testing.T) {
	_, router, conversationID := candidatesFixture(t)

	for _, query := range []url.Values{
		{"type": {"application"}},
		{"limit": {"0"}},
		{"limit": {"101"}},
		{"cursor": {"not-a-cursor"}},
	} {
		listCandidates(t, router, "7", conversationID, query, http.StatusBadRequest)
	}
	// The conversation is private to its author: another project member
	// cannot see it, so it does not exist for them.
	listCandidates(t, router, "8", conversationID, url.Values{}, http.StatusNotFound)
	// A user who is not a member of the project at all.
	recorder := callChatAuthorityAnyStatus(t, router, "13", http.MethodGet,
		fmt.Sprintf("/1/conversations/%s/candidates", conversationID))
	if recorder != http.StatusForbidden && recorder != http.StatusNotFound {
		t.Fatalf("a non-member answered %d, want 403 or 404", recorder)
	}
}

func callChatAuthorityAnyStatus(t *testing.T, router http.Handler, actor, method, path string) int {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request = request.WithContext(chatActor(actor))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code
}
