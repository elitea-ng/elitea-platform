package repos

// DEFECT #603: GET /messages/prompt_lib/{projectID}/{conversationID} ignored
// every pagination and ordering parameter its callers send.
//
// The repository computed its window from `page`/`page_size` and hardcoded
// `ORDER BY mg.created_at DESC`, while the web client has always sent pylon's
// `limit`/`offset`/`sort_by`/`sort_order`
// (apps/elitea-web/src/entities/conversation/api/messageApi.ts:59-63,
// legacy/plugins/elitea_core/api/v2/messages.py:71-107). Neither `page` nor
// `page_size` was ever on the wire, so every request collapsed to page 1 of
// 50, newest first: scrolling back re-fetched the same rows forever and a
// client that asked for oldest-first got newest-first.
//
// WHAT THE RED RUN SHOWS. Against the unchanged query — offset fixed at
// (1-1)*50 and ORDER BY created_at DESC — TestListMessagesWindowsByLimitAndOffset
// fails on its first assertion (`sort_order=asc, limit=2, offset=2` returns
// groups 1 and 2 newest-first instead of the third and fourth oldest), and
// TestListMessagesOffsetAdvances fails reporting that all four offsets
// returned an identical row. Both are behavioural: the old code answered 200
// with a well-formed envelope every time, which is why no existing test could
// see this.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// seedOrderedTranscript writes `count` message groups whose created_at values
// are distinct and strictly increasing, and returns the conversation UUID plus
// the group contents in OLDEST-FIRST order.
//
// The timestamps are set explicitly rather than left to the column default.
// `now()` in Postgres is transaction-scoped, so groups inserted by one
// transaction share a timestamp to the microsecond — which is the very reason
// pylon sorts by `id` as a tiebreaker (messages.py:94-97). Under a shared
// timestamp an ordering assertion would be decided by the id tiebreaker alone
// and would pass even if the created_at ordering were ignored. Spacing the
// rows a minute apart makes the sort key the thing under test.
func seedOrderedTranscript(t *testing.T, repo *ConversationsRepo, count int) (conversationUUID string, oldestFirst []string) {
	t.Helper()
	ctx := context.Background()

	var numericID string
	if err := repo.pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'ordered transcript', 7, 'agent')
RETURNING id::text, uuid::text`).Scan(&numericID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	for i := 0; i < count; i++ {
		content := fmt.Sprintf("group %d", i)
		if _, err := repo.pool.Exec(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
), grp AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
    SELECT gen_random_uuid(), participant.id, $1::int,
           TIMESTAMP '2026-01-01 00:00:00' + ($2::int * INTERVAL '1 minute')
    FROM participant
    RETURNING id
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, grp.id FROM grp
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT item.id, $3 FROM item`, numericID, i, content); err != nil {
			t.Fatalf("seed message group %d: %v", i, err)
		}
		oldestFirst = append(oldestFirst, content)
	}

	return conversationUUID, oldestFirst
}

// wholeTranscript is the window the sibling #599 tests
// (conversation_messages_postgres_integration_test.go) ask for: everything they
// seeded. They were written when ListMessages took a (page, pageSize) pair and
// passed (1, 50). #603 replaced that pair with the limit/offset/sort window the
// route actually receives, so those call sites name the same intent through
// this helper rather than restating a struct whose fields are beside the point
// in a file about conversation-identifier resolution.
func wholeTranscript() conversations.MessagesQuery {
	return conversations.MessagesQuery{Limit: 50, SortBy: "created_at", SortOrder: "desc"}
}

func listedContents(items []conversations.Message) []string {
	contents := make([]string, 0, len(items))
	for _, m := range items {
		contents = append(contents, m.Content)
	}
	return contents
}

func equalContents(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The assertion the issue asks for: limit=2&offset=2&sort_order=asc must serve
// the THIRD and FOURTH OLDEST groups, and the same window descending must serve
// the third and fourth newest. Six groups is enough for the two windows to be
// disjoint, so a query that ignored either parameter cannot land on the right
// rows by accident.
func TestListMessagesWindowsByLimitAndOffset(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	conversationUUID, oldestFirst := seedOrderedTranscript(t, repo, 6)

	ascending, err := repo.ListMessages(ctx, "1", conversationUUID,
		conversations.MessagesQuery{Limit: 2, Offset: 2, SortBy: "created_at", SortOrder: "asc"})
	if err != nil {
		t.Fatalf("list ascending window: %v", err)
	}
	if want := oldestFirst[2:4]; !equalContents(listedContents(ascending.Items), want) {
		t.Errorf("limit=2&offset=2&sort_order=asc returned %v, want the third and fourth oldest %v",
			listedContents(ascending.Items), want)
	}

	descending, err := repo.ListMessages(ctx, "1", conversationUUID,
		conversations.MessagesQuery{Limit: 2, Offset: 2, SortBy: "created_at", SortOrder: "desc"})
	if err != nil {
		t.Fatalf("list descending window: %v", err)
	}
	if want := []string{oldestFirst[3], oldestFirst[2]}; !equalContents(listedContents(descending.Items), want) {
		t.Errorf("limit=2&offset=2&sort_order=desc returned %v, want the third and fourth newest %v",
			listedContents(descending.Items), want)
	}

	// Total counts the conversation, not the window — the client needs it to
	// know whether another scroll-back page exists.
	if ascending.Total != 6 || descending.Total != 6 {
		t.Errorf("totals %d and %d, want 6 for both windows", ascending.Total, descending.Total)
	}

	// The envelope is derived from the window served, not from a `page` the
	// caller never sent: page_size is the limit, page is the 1-based index of
	// the page holding the first row returned.
	if ascending.PageSize != 2 || ascending.Page != 2 || ascending.TotalPages != 3 {
		t.Errorf("envelope reported page %d size %d of %d pages, want page 2 size 2 of 3",
			ascending.Page, ascending.PageSize, ascending.TotalPages)
	}
}

// The scroll-back break itself (useLoadMoreMessages.ts:96): walking the offset
// in limit-sized steps has to walk the transcript. The old code returned an
// identical first window for every offset, so the chat could never load an
// older message.
func TestListMessagesOffsetAdvances(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	conversationUUID, oldestFirst := seedOrderedTranscript(t, repo, 4)

	var walked []string
	for offset := 0; offset < 4; offset++ {
		resp, err := repo.ListMessages(ctx, "1", conversationUUID,
			conversations.MessagesQuery{Limit: 1, Offset: offset, SortBy: "created_at", SortOrder: "asc"})
		if err != nil {
			t.Fatalf("list at offset %d: %v", offset, err)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("offset %d returned %d items, want 1", offset, len(resp.Items))
		}
		walked = append(walked, resp.Items[0].Content)
	}

	if !equalContents(walked, oldestFirst) {
		t.Errorf("walking offsets 0..3 visited %v, want each group once in order %v", walked, oldestFirst)
	}
}

// An offset past the end is an empty window, not an error and not a wrapped
// window: the scroll-back loop relies on it to stop.
func TestListMessagesOffsetBeyondTheEndIsEmpty(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, _ := seedOrderedTranscript(t, repo, 3)

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID,
		conversations.MessagesQuery{Limit: 10, Offset: 3, SortBy: "created_at", SortOrder: "asc"})
	if err != nil {
		t.Fatalf("list past the end: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("offset past the end returned %d items, want none", len(resp.Items))
	}
	if resp.Total != 3 {
		t.Errorf("total %d, want 3 even for an empty window", resp.Total)
	}
}

// sort_by is concatenated into the statement, so it is validated against an
// allow-list of real chat_message_group columns. A value outside the list must
// fall back to created_at rather than reach Postgres — the assertion that the
// injection this shape would otherwise open is closed.
func TestListMessagesRejectsAnUnknownSortColumn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	conversationUUID, oldestFirst := seedOrderedTranscript(t, repo, 3)

	for _, sortBy := range []string{
		"no_such_column",
		"created_at; DROP TABLE p_1.chat_message_group",
		"(SELECT 1)",
		"",
	} {
		resp, err := repo.ListMessages(ctx, "1", conversationUUID,
			conversations.MessagesQuery{Limit: 10, SortBy: sortBy, SortOrder: "asc"})
		if err != nil {
			t.Fatalf("sort_by %q: %v", sortBy, err)
		}
		if !equalContents(listedContents(resp.Items), oldestFirst) {
			t.Errorf("sort_by %q returned %v, want the created_at fallback order %v",
				sortBy, listedContents(resp.Items), oldestFirst)
		}
	}

	// And the table is still there.
	var groups int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_message_group`).Scan(&groups); err != nil {
		t.Fatalf("count message groups: %v", err)
	}
	if groups != 3 {
		t.Errorf("after the sort_by probes the transcript holds %d groups, want 3", groups)
	}
}

// `id` is in the allow-list, and ordering by it must not emit the `mg.id`
// tiebreaker twice.
func TestListMessagesSortsByID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, oldestFirst := seedOrderedTranscript(t, repo, 3)

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID,
		conversations.MessagesQuery{Limit: 10, SortBy: "id", SortOrder: "desc"})
	if err != nil {
		t.Fatalf("sort by id: %v", err)
	}
	want := []string{oldestFirst[2], oldestFirst[1], oldestFirst[0]}
	if !equalContents(listedContents(resp.Items), want) {
		t.Errorf("sort_by=id&sort_order=desc returned %v, want %v", listedContents(resp.Items), want)
	}
}

// --- The free-text filter, pylon's `query` parameter (messages.py:86-91). ---

// seedTimedTranscript writes one message group per content string, each a
// minute apart, so ordering assertions rest on the timestamps rather than on
// insertion luck — the same reason seedOrderedTranscript sets created_at
// explicitly.
func seedTimedTranscript(t *testing.T, repo *ConversationsRepo, contents ...string) (numericID, conversationUUID string) {
	t.Helper()
	ctx := context.Background()

	if err := repo.pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'searchable transcript', 7, 'agent')
RETURNING id::text, uuid::text`).Scan(&numericID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	for i, content := range contents {
		if _, err := repo.pool.Exec(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
), grp AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
    SELECT gen_random_uuid(), participant.id, $1::int,
           TIMESTAMP '2026-01-01 00:00:00' + ($2::int * INTERVAL '1 minute')
    FROM participant
    RETURNING id
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, grp.id FROM grp
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT item.id, $3 FROM item`, numericID, i, content); err != nil {
			t.Fatalf("seed message group %q: %v", content, err)
		}
	}

	return numericID, conversationUUID
}

func searchTranscript(term string, limit int) conversations.MessagesQuery {
	return conversations.MessagesQuery{Limit: limit, SortBy: "created_at", SortOrder: "asc", Query: term}
}

func TestListMessagesFiltersByQuery(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTimedTranscript(t, repo,
		"the cat sat on the mat", "a DOG barked", "cats and dogs")

	resp, err := repo.ListMessages(ctx, "1", conversationUUID, searchTranscript("cat", 50))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := listedContents(resp.Items)
	want := []string{"the cat sat on the mat", "cats and dogs"}
	if !equalContents(got, want) {
		t.Fatalf("searching for %q returned %v, want %v", "cat", got, want)
	}

	// total must describe the FILTERED set. Report the unfiltered count and
	// total_pages describes a different result than items.
	if resp.Total != 2 {
		t.Errorf("total is %d, want 2 — the filter was not applied to the count", resp.Total)
	}
}

// ILIKE, not LIKE: pylon's filter is case-insensitive.
func TestListMessagesQueryIsCaseInsensitive(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	// The second group is what makes this discriminating: with only a matching
	// group present, a filter that was never applied returns the same one item
	// and the test passes while measuring nothing.
	_, conversationUUID := seedTimedTranscript(t, repo, "a DOG barked", "a cat slept")

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, searchTranscript("dog", 50))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := listedContents(resp.Items); !equalContents(got, []string{"a DOG barked"}) {
		t.Fatalf("case-insensitive search returned %v, want [a DOG barked]", got)
	}
}

// An empty term is "no filter", not "match the empty string". Treat it as a
// pattern and every group matches `%%`, which reads as a working filter right
// up until someone clears the search box.
func TestListMessagesEmptyQueryIsNoFilter(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	_, conversationUUID := seedTimedTranscript(t, repo, "one", "two", "three")

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, searchTranscript("", 50))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Total != 3 || len(resp.Items) != 3 {
		t.Fatalf("an empty query returned total %d and %d items, want 3 and 3", resp.Total, len(resp.Items))
	}
}

// The metacharacter escape. `%` and `_` are ILIKE wildcards, so an unescaped
// term silently matches far more than the user typed — pylon interpolated the
// raw term and had exactly that behaviour. Drop the escaping and every one of
// these searches returns rows it should not.
func TestListMessagesQueryTreatsWildcardsLiterally(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTimedTranscript(t, repo,
		"discount is 50% today", "discount is 5000 today", "a_b", "axb", `back\slash`)

	for _, tc := range []struct {
		term string
		want []string
	}{
		{"50%", []string{"discount is 50% today"}},
		{"a_b", []string{"a_b"}},
		{`back\slash`, []string{`back\slash`}},
	} {
		resp, err := repo.ListMessages(ctx, "1", conversationUUID, searchTranscript(tc.term, 50))
		if err != nil {
			t.Fatalf("search %q: %v", tc.term, err)
		}
		if got := listedContents(resp.Items); !equalContents(got, tc.want) {
			t.Errorf("searching for %q returned %v, want %v", tc.term, got, tc.want)
		}
	}
}

// A group whose text is split across several matching items must appear ONCE.
// Pylon joined the item table instead, so such a group came back duplicated and
// was double-counted in `total`; EXISTS answers per group.
func TestListMessagesQueryReturnsAGroupOnceWhenSeveralItemsMatch(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTimedTranscript(t, repo, "needle one")

	// A second text item on the same group, also matching.
	if _, err := repo.pool.Exec(ctx, `
WITH item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 1, id FROM p_1.chat_message_group LIMIT 1
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content) SELECT id, 'needle two' FROM item`); err != nil {
		t.Fatalf("seed second matching item: %v", err)
	}

	resp, err := repo.ListMessages(ctx, "1", conversationUUID, searchTranscript("needle", 50))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("a group with two matching items came back as total %d / %d items, want 1 and 1",
			resp.Total, len(resp.Items))
	}
}

// The filter and the window compose: paging through search results must skip
// within the filtered set, not within the whole transcript.
func TestListMessagesQueryComposesWithLimitAndOffset(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTimedTranscript(t, repo,
		"match 1", "noise", "match 2", "noise", "match 3")

	window := searchTranscript("match", 2)
	window.Offset = 1
	resp, err := repo.ListMessages(ctx, "1", conversationUUID, window)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := listedContents(resp.Items); !equalContents(got, []string{"match 2", "match 3"}) {
		t.Fatalf("limit=2&offset=1 over the matches returned %v, want [match 2 match 3]", got)
	}
	if resp.Total != 3 {
		t.Errorf("total is %d, want 3 matching groups", resp.Total)
	}
}

// SQL injection through the search term. It is bound, not interpolated — unlike
// sort_by — so this is a guard against a future "optimisation" that inlines it.
func TestListMessagesQueryIsBoundNotInterpolated(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTimedTranscript(t, repo, "harmless")

	if _, err := repo.ListMessages(ctx, "1", conversationUUID,
		searchTranscript("' OR 1=1; DROP TABLE p_1.chat_message_group; --", 50)); err != nil {
		t.Fatalf("a hostile search term errored instead of simply not matching: %v", err)
	}
	var groups int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_message_group`).Scan(&groups); err != nil {
		t.Fatalf("chat_message_group did not survive the search: %v", err)
	}
	if groups != 1 {
		t.Fatalf("%d groups left, want 1", groups)
	}
}

func TestListMessagesPreservesContinuationSeams(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	for _, tc := range []struct{ name, meta, want string }{
		{"ordinary", `{}`, "group 0\n suffix"},
		{"continued", `{"output_limit_sequence":1}`, "group 0 suffix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, _ := seedOrderedTranscript(t, repo, 1)
			_, err := pool.Exec(ctx, `
WITH grp AS (
 UPDATE p_1.chat_message_group SET meta = $2::jsonb
 WHERE conversation_id = (SELECT id FROM p_1.chat_conversations WHERE uuid = $1::uuid)
 RETURNING id
), item AS (
 INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
 SELECT gen_random_uuid(), 'text_message', 1, id FROM grp RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content) SELECT id, ' suffix' FROM item`, id, tc.meta)
			if err != nil {
				t.Fatal(err)
			}
			response, err := repo.ListMessages(ctx, "1", id, wholeTranscript())
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Items) != 1 || response.Items[0].Content != tc.want {
				t.Fatalf("content = %v; want %q", listedContents(response.Items), tc.want)
			}
		})
	}
}
