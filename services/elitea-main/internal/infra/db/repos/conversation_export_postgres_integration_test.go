package repos

// Conversation export (#851) — the document a user downloads.
//
// The rail's row menu offered an "Export" entry that was disabled and whose
// two options were labelled `Option1`/`Option2`, because there was no route
// behind either of them. This covers the route that closes that gap.
//
// AGAINST A REAL, MIGRATED DATABASE and THROUGH THE HANDLER, because the
// question is not "does the renderer render" — that is a pure function with
// its own unit test next to it — but "does the document hold the conversation
// that was actually stored". The transcript is spread over four tables
// (chat_message_group, chat_message_items, chat_messages_text,
// chat_participants), the handler reads it in pages, and a fake repository
// would be free to answer with whatever the fixture said. So the sequence
// below is the one the browser performs: seed the conversation, then GET the
// export route and read the file it returns.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// newConversationExportRouter mounts the export route on the same pattern
// internal/api/router.go registers it under.
func newConversationExportRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	handler := conversations.NewHandler(NewConversationsRepo(pool)).WithPool(pool)
	r := chi.NewRouter()
	r.Get("/conversation_export/prompt_lib/{projectID}/{conversationID}", handler.Export)
	return r
}

// seedExportTranscript writes a conversation with `count` message groups whose
// timestamps are distinct and strictly increasing, and returns its numeric id,
// its UUID and the message contents oldest-first.
//
// The timestamps are set explicitly. `now()` is transaction-scoped in
// PostgreSQL, so rows written by one transaction share a timestamp to the
// microsecond, and an ordering assertion made on them would be decided by the
// id tiebreaker alone — it would pass even if the export ignored `created_at`.
func seedExportTranscript(t *testing.T, pool *pgxpool.Pool, name string, count int) (numericID, conversationUUID string, oldestFirst []string) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), $1, 7, 'agent')
RETURNING id::text, uuid::text`, name).Scan(&numericID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	// ONE participant, MAPPED to the conversation. The mapping row is what
	// the participants read joins on, so a participant written without it is
	// invisible to the export and every message comes back unattributed —
	// which is the state this fixture used to be in, and the reason
	// attribution is asserted below rather than assumed.
	var participantID int
	if err := pool.QueryRow(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 42, "project_id": 1, "name": "autotest_author"}'::jsonb)
    RETURNING id
), mapping AS (
    INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id)
    SELECT $1::int, participant.id FROM participant
    RETURNING participant_id
)
SELECT participant_id FROM mapping`, numericID).Scan(&participantID); err != nil {
		t.Fatalf("seed participant: %v", err)
	}

	for i := range count {
		content := fmt.Sprintf("autotest export line %d", i)
		if _, err := pool.Exec(ctx, `
WITH grp AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
    VALUES (gen_random_uuid(), $4::int, $1::int,
            TIMESTAMP '2026-01-01 00:00:00' + ($2::int * INTERVAL '1 minute'))
    RETURNING id
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, grp.id FROM grp
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT item.id, $3 FROM item`, numericID, i, content, participantID); err != nil {
			t.Fatalf("seed message group %d: %v", i, err)
		}
		oldestFirst = append(oldestFirst, content)
	}
	return numericID, conversationUUID, oldestFirst
}

func callExport(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// The Markdown document names the conversation and carries every message, in
// the order they were written.
func TestConversationExportMarkdownCarriesTheWholeTranscript(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)
	id, _, oldestFirst := seedExportTranscript(t, pool, "autotest_export_markdown", 4)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/"+id+"?format=md")
	if response.Code != http.StatusOK {
		t.Fatalf("export answered %d: %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/markdown") {
		t.Errorf("Content-Type is %q, want text/markdown", contentType)
	}

	// A DOWNLOAD, not a page. Without this header the browser renders the
	// transcript in the tab instead of saving it, which is not an export.
	disposition := response.Header().Get("Content-Disposition")
	mediaType, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		t.Fatalf("parse Content-Disposition %q: %v", disposition, err)
	}
	if mediaType != "attachment" {
		t.Errorf("Content-Disposition is %q, want an attachment", mediaType)
	}
	if want := "autotest_export_markdown.md"; params["filename"] != want {
		t.Errorf("filename is %q, want %q — the name comes from the conversation", params["filename"], want)
	}

	body := response.Body.String()
	if !strings.Contains(body, "# autotest_export_markdown") {
		t.Errorf("the document does not open with the conversation's name:\n%s", body)
	}
	// Every message, in order. `strings.Index` walking forward is what makes
	// this an ORDER assertion rather than four independent Contains checks.
	position := 0
	for _, content := range oldestFirst {
		found := strings.Index(body[position:], content)
		if found < 0 {
			t.Fatalf("the document is missing %q, or has it out of order:\n%s", content, body)
		}
		position += found + len(content)
	}
	if !strings.Contains(body, "- Messages: 4") {
		t.Errorf("the header block does not report 4 messages:\n%s", body)
	}
}

// The JSON document is the faithful one: the same messages, as data.
func TestConversationExportJSONCarriesTheWholeTranscript(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)
	id, _, oldestFirst := seedExportTranscript(t, pool, "autotest_export_json", 3)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/"+id+"?format=json")
	if response.Code != http.StatusOK {
		t.Fatalf("export answered %d: %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type is %q, want application/json", contentType)
	}
	_, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("parse Content-Disposition: %v", err)
	}
	if want := "autotest_export_json.json"; params["filename"] != want {
		t.Errorf("filename is %q, want %q", params["filename"], want)
	}

	var document struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ProjectID    string `json:"project_id"`
		MessageCount int    `json:"message_count"`
		Messages     []struct {
			Content string `json:"content"`
			Author  string `json:"author"`
			Role    string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode the export document: %v\n%s", err, response.Body.String())
	}
	if document.Name != "autotest_export_json" || document.ID != id || document.ProjectID != "1" {
		t.Errorf("the document names conversation %q/%q in project %q, want %q/%q in 1",
			document.ID, document.Name, document.ProjectID, id, "autotest_export_json")
	}
	if document.MessageCount != 3 || len(document.Messages) != 3 {
		t.Fatalf("message_count=%d with %d messages, want 3 and 3", document.MessageCount, len(document.Messages))
	}
	for i, message := range document.Messages {
		if message.Content != oldestFirst[i] {
			t.Errorf("message %d is %q, want %q — oldest first", i, message.Content, oldestFirst[i])
		}
		// Attribution: the seeded participant carries a name, so the export
		// must resolve it rather than leaving every message anonymous.
		if message.Author != "autotest_author" {
			t.Errorf("message %d is attributed to %q, want the participant's name", i, message.Author)
		}
	}
}

// A UUID in the path resolves the same conversation. Every deep link in the
// chat surface carries one, so an export that only understood numeric ids
// would fail for exactly the conversation the user is looking at.
func TestConversationExportResolvesAUUIDPath(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)
	_, conversationUUID, oldestFirst := seedExportTranscript(t, pool, "autotest_export_uuid", 2)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/"+conversationUUID+"?format=md")
	if response.Code != http.StatusOK {
		t.Fatalf("export by UUID answered %d: %s", response.Code, response.Body.String())
	}
	for _, content := range oldestFirst {
		if !strings.Contains(response.Body.String(), content) {
			t.Errorf("the UUID export is missing %q", content)
		}
	}
}

// The default format is the readable one, so a plain link exports Markdown.
func TestConversationExportDefaultsToMarkdown(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)
	id, _, _ := seedExportTranscript(t, pool, "autotest_export_default", 1)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/"+id)
	if response.Code != http.StatusOK {
		t.Fatalf("export with no format answered %d: %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/markdown") {
		t.Errorf("Content-Type is %q, want text/markdown by default", contentType)
	}
}

// An unknown format is REFUSED. A client that asked for `pdf` and silently
// received Markdown would save a file with the wrong extension and have no way
// to notice.
func TestConversationExportRefusesAnUnknownFormat(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)
	id, _, _ := seedExportTranscript(t, pool, "autotest_export_format", 1)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/"+id+"?format=pdf")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("an unknown format answered %d, want 400: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Error("a refused export must not offer a download")
	}
}

// A conversation that does not exist is a 404, not an empty document named
// after nothing.
func TestConversationExportRefusesAnUnknownConversation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/99999999?format=json")
	if response.Code != http.StatusNotFound {
		t.Fatalf("an unknown conversation answered %d, want 404: %s", response.Code, response.Body.String())
	}
}

// The transcript is read in pages, so a conversation longer than one page must
// still export WHOLE. This is the assertion that fails if the paging loop ever
// stops after its first read.
func TestConversationExportPagesPastTheFirstReadWindow(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newConversationExportRouter(t, pool)
	// One more than the export's own page size, so the loop must go round
	// twice and the second read must return the tail rather than the head.
	id, _, oldestFirst := seedExportTranscript(t, pool, "autotest_export_long", 101)

	response := callExport(t, router, "/conversation_export/prompt_lib/1/"+id+"?format=json")
	if response.Code != http.StatusOK {
		t.Fatalf("export answered %d: %s", response.Code, response.Body.String())
	}
	var document struct {
		MessageCount int `json:"message_count"`
		Messages     []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode the export document: %v", err)
	}
	if document.MessageCount != 101 || len(document.Messages) != 101 {
		t.Fatalf("message_count=%d with %d messages, want 101 — the export stopped at a page boundary",
			document.MessageCount, len(document.Messages))
	}
	seen := map[string]bool{}
	for _, message := range document.Messages {
		if seen[message.Content] {
			t.Fatalf("message %q appears twice — the paging loop re-read a window", message.Content)
		}
		seen[message.Content] = true
	}
	for _, content := range oldestFirst {
		if !seen[content] {
			t.Errorf("the export is missing %q", content)
		}
	}
}
