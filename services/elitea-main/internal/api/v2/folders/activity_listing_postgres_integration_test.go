package folders_test

// A message moves a conversation to Today; a folder keeps its own.
//
// Sending a message writes a chat_message_group row and does not revise the
// chat_conversations row, so a listing that bucketed by the row's own
// timestamps kept a conversation someone writes in under the date it was
// created ("Previous 30 days"), below conversations nobody had touched. The
// grouped listing now buckets and orders the date groups by the newest of
// the creation, the revision and the newest message group. Folder and pinned
// rows are not date-bucketed and keep their order.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
)

// localNoonUTC is the middle of the host's "Today" window, in UTC (see
// seedTodayConversations for why the fixture's plain timestamps need it).
func localNoonUTC() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).UTC()
}

func seedMessageAt(t *testing.T, pool *pgxpool.Pool, conversationID int, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.chat_message_group (conversation_id, created_at) VALUES ($1, $2)`, conversationID, at); err != nil {
		t.Fatalf("seed a message in %d: %v", conversationID, err)
	}
}

func TestGroupedListingBucketsByLastMessage(t *testing.T) {
	pool := newPinnedListingPool(t)
	noon := localNoonUTC()
	old := noon.AddDate(0, 0, -20)
	// Two conversations from today, and one from 20 days ago that gets a message today.
	seedPinnedListingConversationAt(t, pool, "autotest_today_quiet", noon.Add(-2*time.Hour), nil)
	seedPinnedListingConversationAt(t, pool, "autotest_today_newer", noon.Add(-time.Hour), nil)
	written := seedPinnedListingConversationAt(t, pool, "autotest_old_written", old, nil)
	seedMessageAt(t, pool, written, old.Add(time.Minute))
	seedMessageAt(t, pool, written, noon.Add(-time.Minute))
	stale := seedPinnedListingConversationAt(t, pool, "autotest_old_stale", old, nil)
	seedMessageAt(t, pool, stale, old.Add(time.Minute))

	listing := readPagedListing(t, pool, nil, "?grouped=true")
	today := bucketNamed(t, listing.DateGroups, "Today")
	names := make([]string, 0, len(today.Conversations))
	for _, conversation := range today.Conversations {
		names = append(names, conversation.Name)
	}
	want := []string{"autotest_old_written", "autotest_today_newer", "autotest_today_quiet"}
	if len(names) != len(want) {
		t.Fatalf("Today = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Today = %v, want %v (the conversation written in last is first)", names, want)
		}
	}
	older := bucketNamed(t, listing.DateGroups, "This month")
	if len(older.Conversations) != 1 || older.Conversations[0].Name != "autotest_old_stale" {
		t.Fatalf("This month = %+v, want only the conversation with no new message", older.Conversations)
	}

	// The follow-up page of the bucket answers in the same order.
	page := readPagedPage(t, pool, nil, "?date_group=Today&limit=10&offset=0")
	if len(page.Conversations) == 0 || page.Conversations[0].Name != "autotest_old_written" {
		t.Fatalf("?date_group=Today = %+v, want the written conversation first", page.Conversations)
	}
}

func TestGroupedListingKeepsAFolderConversationInItsFolder(t *testing.T) {
	pool := newPinnedListingPool(t)
	noon := localNoonUTC()
	old := noon.AddDate(0, 0, -20)
	first := seedPinnedListingConversationAt(t, pool, "autotest_folder_first", old.Add(time.Hour), nil)
	second := seedPinnedListingConversationAt(t, pool, "autotest_folder_second", old, nil)
	for _, id := range []int{first, second} {
		if _, err := pool.Exec(context.Background(), `UPDATE p_1.chat_conversations SET folder_id = 5 WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	// A message today in the older one of the two.
	seedMessageAt(t, pool, second, noon.Add(-time.Minute))

	folders := []handler.Folder{{ID: "5", ProjectID: "1", Name: "autotest_activity_folder"}}
	listing := readPagedListing(t, pool, folders, "?grouped=true")
	for _, group := range listing.DateGroups {
		for _, conversation := range group.Conversations {
			if conversation.ID == first || conversation.ID == second {
				t.Fatalf("folder conversation %q is in the date group %q", conversation.Name, group.Name)
			}
		}
	}
	folder := bucketNamed(t, listing.Folders, "autotest_activity_folder")
	if len(folder.Conversations) != 2 || folder.Conversations[0].ID != first || folder.Conversations[1].ID != second {
		t.Fatalf("folder = %+v, want its own order (first, second) unchanged by the message", folder.Conversations)
	}
}
