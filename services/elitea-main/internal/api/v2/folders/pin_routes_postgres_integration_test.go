package folders_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The server injects a test identity, then uses the public handlers and project gate.
// PostgreSQL remains the authority for project membership and private chat access.
func newPinRouteServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.ContextWithUser(r.Context(), auth.User{ID: r.Header.Get("X-Test-Actor")})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	router.Mount("/api/v2/social", social.NewHandler(pool).Routes())
	router.Group(func(r chi.Router) {
		r.Use(apimw.RequireProjectAccess(pool))
		core := eliteacore.NewHandler(pool)
		r.Post("/api/v2/elitea_core/pin/prompt_lib/{projectID}/{entityType}/{entityID}", core.Pin)
		r.Delete("/api/v2/elitea_core/pin/prompt_lib/{projectID}/{entityType}/{entityID}", core.Unpin)
		r.Route("/api/v2/projects/{projectID}/folders", func(r chi.Router) {
			r.Mount("/", folders.NewHandler(&mockFolderRepo{}).WithPool(pool).Routes())
		})
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func pinRouteRequest(t *testing.T, server *httptest.Server, method, route, actor string, status int, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, server.URL+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Test-Actor", actor)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if response.StatusCode != status {
		var body any
		_ = json.NewDecoder(response.Body).Decode(&body)
		t.Fatalf("%s %s as %s: status=%d body=%v, want %d", method, route, actor, response.StatusCode, body, status)
	}
	if result != nil {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			t.Fatal(err)
		}
	}
}

func assertPinRow(t *testing.T, pool *pgxpool.Pool, conversationID, count, lastPinner int) int {
	t.Helper()
	var gotCount, gotID, gotPinner int
	err := pool.QueryRow(t.Context(), `SELECT count(*), COALESCE(min(id),0), COALESCE(min(user_id),0)
	 FROM centry.social_pins WHERE entity='conversation' AND project_id=1 AND entity_id=$1`, conversationID).
		Scan(&gotCount, &gotID, &gotPinner)
	if err != nil {
		t.Fatal(err)
	}
	if gotCount != count || gotPinner != lastPinner {
		t.Fatalf("pin rows=%d last pinner=%d, want rows=%d last pinner=%d", gotCount, gotPinner, count, lastPinner)
	}
	return gotID
}

func assertHTTPPinned(t *testing.T, server *httptest.Server, actor string, conversationID int) {
	t.Helper()
	var listing groupedListing
	pinRouteRequest(t, server, http.MethodGet, "/api/v2/projects/1/folders/?grouped=true", actor, http.StatusOK, &listing)
	if conversationID == 0 {
		if len(listing.Pinned.Conversations) != 0 {
			t.Fatalf("reader %s sees pins %v, want none", actor, listing.Pinned.Conversations)
		}
		return
	}
	if len(listing.Pinned.Conversations) != 1 || listing.Pinned.Conversations[0].ID != conversationID {
		t.Fatalf("reader %s sees pins %v, want exactly %d", actor, listing.Pinned.Conversations, conversationID)
	}
	for _, group := range listing.DateGroups {
		for _, conversation := range group.Conversations {
			if conversation.ID == conversationID {
				t.Fatal("the pinned conversation also appears in a date group")
			}
		}
	}
}

func TestPublicPinRoutesShareCanonicalRowsAndEnforceChatAuthority(t *testing.T) {
	for _, family := range []string{"social", "elitea_core"} {
		t.Run(family, func(t *testing.T) {
			pool := newPinnedListingPool(t)
			conversation := seedPinnedListingConversation(t, pool, "shared pin", true)
			server := newPinRouteServer(t, pool)
			route := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/1/conversation/%d", family, conversation)
			if _, err := pool.Exec(t.Context(), `UPDATE p_1.chat_conversations SET meta='{"is_pinned":true}' WHERE id=$1`, conversation); err != nil {
				t.Fatal(err)
			}
			// Conversation metadata is not a second pin authority.
			assertHTTPPinned(t, server, "7", 0)
			var action struct {
				OK bool `json:"ok"`
			}
			pinRouteRequest(t, server, http.MethodPost, route, "7", http.StatusOK, &action)
			if !action.OK {
				t.Fatal("the pin did not report success")
			}
			pinID := assertPinRow(t, pool, conversation, 1, 7)
			for _, actor := range []string{"7", "8", "8"} {
				pinRouteRequest(t, server, http.MethodPost, route, actor, http.StatusOK, &action)
				if id := assertPinRow(t, pool, conversation, 1, mustPinActor(t, actor)); id != pinID {
					t.Fatal("a repeat pin changed the canonical row identity")
				}
				assertHTTPPinned(t, server, "7", conversation)
				assertHTTPPinned(t, server, "8", conversation)
			}
			// Actor 7 can remove the shared pin last written by actor 8.
			for repeat := 0; repeat < 2; repeat++ {
				pinRouteRequest(t, server, http.MethodDelete, route, "7", http.StatusOK, &action)
				assertPinRow(t, pool, conversation, 0, 0)
				assertHTTPPinned(t, server, "7", 0)
				assertHTTPPinned(t, server, "8", 0)
			}

			// A private chat is still inaccessible to another project member.
			if _, err := pool.Exec(t.Context(), `DELETE FROM p_1.chat_participant_mapping WHERE conversation_id=$1 AND participant_id=8`, conversation); err != nil {
				t.Fatal(err)
			}
			pinRouteRequest(t, server, http.MethodPost, route, "7", http.StatusOK, nil)
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				pinRouteRequest(t, server, method, route, "8", http.StatusNotFound, nil)
				assertPinRow(t, pool, conversation, 1, 7)
				assertHTTPPinned(t, server, "8", 0)
				crossProject := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/2/conversation/%d", family, conversation)
				pinRouteRequest(t, server, method, crossProject, "7", http.StatusForbidden, nil)
				assertPinRow(t, pool, conversation, 1, 7)
			}

			for _, entityID := range []string{"0", "-1", "01", "2147483648", "not-an-id"} {
				invalid := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/1/conversation/%s", family, entityID)
				for _, method := range []string{http.MethodPost, http.MethodDelete} {
					pinRouteRequest(t, server, method, invalid, "7", http.StatusBadRequest, nil)
				}
			}
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				unknown := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/1/unknown/%d", family, conversation)
				pinRouteRequest(t, server, method, unknown, "7", http.StatusBadRequest, nil)
				missing := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/1/conversation/2147483647", family)
				pinRouteRequest(t, server, method, missing, "7", http.StatusNotFound, nil)
				alias := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/01/conversation/%d", family, conversation)
				pinRouteRequest(t, server, method, alias, "7", http.StatusBadRequest, nil)
			}
			assertPinRow(t, pool, conversation, 1, 7)

			// Equal entity IDs in two tenants must remain separate shared pins.
			if _, err := pool.Exec(t.Context(), `CREATE SCHEMA p_2;
CREATE TABLE p_2.chat_conversations(id integer PRIMARY KEY,is_private boolean,meta jsonb);
CREATE TABLE p_2.chat_participants(id integer PRIMARY KEY,entity_name text,entity_meta jsonb);
CREATE TABLE p_2.chat_participant_mapping(conversation_id integer,participant_id integer);
INSERT INTO p_2.chat_participants VALUES (9,'user','{"id":9}');`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO p_2.chat_conversations VALUES ($1,TRUE,'{}')`, conversation); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO p_2.chat_participant_mapping VALUES ($1,9)`, conversation); err != nil {
				t.Fatal(err)
			}
			other := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/2/conversation/%d", family, conversation)
			pinRouteRequest(t, server, http.MethodPost, other, "9", http.StatusOK, nil)
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				pinRouteRequest(t, server, method, route, "9", http.StatusForbidden, nil)
			}
			pinRouteRequest(t, server, http.MethodDelete, route, "7", http.StatusOK, nil)
			assertPinRow(t, pool, conversation, 0, 0)
			var lastPinner int
			if err := pool.QueryRow(t.Context(), `SELECT user_id FROM centry.social_pins WHERE entity='conversation' AND project_id=2 AND entity_id=$1`, conversation).Scan(&lastPinner); err != nil || lastPinner != 9 {
				t.Fatalf("other project's pin: last pinner=%d error=%v", lastPinner, err)
			}
			pinRouteRequest(t, server, http.MethodDelete, other, "9", http.StatusOK, nil)
		})
	}
}

func mustPinActor(t *testing.T, actor string) int {
	t.Helper()
	id, err := strconv.Atoi(actor)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPublicPinRoutesReportStorageFailure(t *testing.T) {
	pool := newPinnedListingPool(t)
	conversation := seedPinnedListingConversation(t, pool, "storage failure", true)
	server := newPinRouteServer(t, pool)
	if _, err := pool.Exec(t.Context(), `DROP TABLE centry.social_pins`); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"social", "elitea_core"} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			route := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/1/conversation/%d", family, conversation)
			pinRouteRequest(t, server, method, route, "7", http.StatusInternalServerError, nil)
		}
	}
}

func TestPublicPinRoutesConcurrentUpsertsKeepOneSharedRow(t *testing.T) {
	pool := newPinnedListingPool(t)
	conversation := seedPinnedListingConversation(t, pool, "concurrent pin", true)
	server := newPinRouteServer(t, pool)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		family, actor := "social", "7"
		if i%2 == 1 {
			family, actor = "elitea_core", "8"
		}
		go func() {
			route := fmt.Sprintf("/api/v2/%s/pin/prompt_lib/1/conversation/%d", family, conversation)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+route, nil)
			if err != nil {
				results <- err
				return
			}
			request.Header.Set("X-Test-Actor", actor)
			response, err := server.Client().Do(request)
			if err == nil {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
				if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("concurrent pin returned HTTP %d", response.StatusCode)
				}
			}
			results <- err
		}()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	var count, lastPinner int
	if err := pool.QueryRow(t.Context(), `SELECT count(*), min(user_id) FROM centry.social_pins
WHERE entity='conversation' AND project_id=1 AND entity_id=$1`, conversation).Scan(&count, &lastPinner); err != nil {
		t.Fatal(err)
	}
	if count != 1 || (lastPinner != 7 && lastPinner != 8) {
		t.Fatalf("concurrent pin rows=%d last pinner=%d", count, lastPinner)
	}
	assertHTTPPinned(t, server, "7", conversation)
	assertHTTPPinned(t, server, "8", conversation)
}
