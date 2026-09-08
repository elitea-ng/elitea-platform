package folders_test

// The grouped listing pages each bucket, and says how much is behind the page.
//
// DEFECT #852. Every bucket was emitted whole with `total = offset =
// len(conversations)`. `total` is the ONLY thing the rail's load-more sentinel
// compares against the rows it already holds, so a total that always equalled
// the delivered count made "there is more" impossible to express: the sentinel
// never mounted, the follow-up `?date_group=…&offset=…` read — served since
// #128 — never had a caller, and a project with more conversations than one
// bucket page could hold simply never showed the rest.
//
// AGAINST A REAL DATABASE, because the thing under test is the boundary
// between one SQL read and the partitioning that follows it: a handler-level
// fake would let the fixture decide how many rows exist, which is the number
// this test exists to measure. Fixture schema is the same hand-cut subset the
// pinned-listing test next door cuts, and this file reuses its pool, its
// seeder and its repository stub.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// pagedConversation is one row as the rail reads it.
type pagedConversation struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// pagedBucket is a date group or a folder: both carry the same three fields,
// because the client reads them through the same merge helper.
type pagedBucket struct {
	Name          string              `json:"name"`
	Conversations []pagedConversation `json:"conversations"`
	Total         int                 `json:"total"`
	Offset        int                 `json:"offset"`
}

type pagedListing struct {
	DateGroups []pagedBucket `json:"date_groups"`
	Folders    []pagedBucket `json:"folders"`
}

// pagedPage is one follow-up page, the shape ?date_group= and ?folder_id=
// answer with.
type pagedPage struct {
	Conversations []pagedConversation `json:"conversations"`
	Total         int                 `json:"total"`
	Limit         int                 `json:"limit"`
	Offset        int                 `json:"offset"`
}

// callFolders serves one GET against the real router mount, with `folders` as
// the folder set the repository reports.
func callFolders(t *testing.T, pool *pgxpool.Pool, folders []handler.Folder, query string) []byte {
	t.Helper()
	h := handler.NewHandler(&mockFolderRepo{folders: folders}).WithPool(pool)
	router := chi.NewRouter()
	router.Route("/api/v2/projects/{projectID}/folders", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	request := httptest.NewRequest(http.MethodGet, "/api/v2/projects/1/folders/"+query, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "7"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", query, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.Bytes()
}

func readPagedListing(t *testing.T, pool *pgxpool.Pool, folders []handler.Folder, query string) pagedListing {
	t.Helper()
	var listing pagedListing
	if err := json.Unmarshal(callFolders(t, pool, folders, query), &listing); err != nil {
		t.Fatalf("decode the grouped listing: %v", err)
	}
	return listing
}

func readPagedPage(t *testing.T, pool *pgxpool.Pool, folders []handler.Folder, query string) pagedPage {
	t.Helper()
	var page pagedPage
	if err := json.Unmarshal(callFolders(t, pool, folders, query), &page); err != nil {
		t.Fatalf("decode the follow-up page: %v", err)
	}
	return page
}

// bucketNamed returns the named bucket, failing when the listing has none.
func bucketNamed(t *testing.T, buckets []pagedBucket, name string) pagedBucket {
	t.Helper()
	for _, bucket := range buckets {
		if bucket.Name == name {
			return bucket
		}
	}
	t.Fatalf("the listing carries no bucket named %q (it has %d)", name, len(buckets))
	return pagedBucket{}
}

// seedTodayConversations seeds n conversations that all land in the "Today"
// bucket, newest last, and returns their ids in the order the rail must serve
// them: newest first.
func seedTodayConversations(t *testing.T, pool *pgxpool.Pool, prefix string, n int) []int {
	t.Helper()
	base := time.Now().UTC().Add(-2 * time.Hour)
	ids := make([]int, 0, n)
	for i := range n {
		id := seedPinnedListingConversationAt(t, pool, fmt.Sprintf("%s_%02d", prefix, i), base.Add(time.Duration(i)*time.Minute), nil)
		ids = append(ids, id)
	}
	// Newest first.
	newestFirst := make([]int, 0, n)
	for i := len(ids) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, ids[i])
	}
	return newestFirst
}

// A bucket bigger than one page must say so, and the page it delivers must be
// the FIRST page of the order it claims — not a random slice.
func TestGroupedListingPagesADateBucketAndReportsTheRemainder(t *testing.T) {
	pool := newPinnedListingPool(t)
	newestFirst := seedTodayConversations(t, pool, "autotest_page", 14)

	today := bucketNamed(t, readPagedListing(t, pool, nil, "?grouped=true").DateGroups, "Today")
	if len(today.Conversations) != 10 {
		t.Fatalf("the bucket delivered %d rows, want the default page of 10", len(today.Conversations))
	}
	if today.Total != 14 {
		t.Errorf("total=%d, want 14 — total is the size of the bucket, not of the page", today.Total)
	}
	if today.Offset != 10 {
		t.Errorf("offset=%d, want 10 — offset is where the next page starts", today.Offset)
	}
	for i, conversation := range today.Conversations {
		if conversation.ID != newestFirst[i] {
			t.Fatalf("row %d is conversation %d, want %d — the page must be the newest ten, in order", i, conversation.ID, newestFirst[i])
		}
	}
}

// The remainder is really fetchable: the follow-up read the client issues with
// the offset the listing handed it must return the rows the first page left,
// with no row served twice and none skipped.
func TestDateGroupSecondPageReturnsTheRemainderWithNoOverlap(t *testing.T) {
	pool := newPinnedListingPool(t)
	newestFirst := seedTodayConversations(t, pool, "autotest_second", 14)

	today := bucketNamed(t, readPagedListing(t, pool, nil, "?grouped=true").DateGroups, "Today")
	page := readPagedPage(t, pool, nil, fmt.Sprintf("?grouped=true&date_group=Today&limit=10&offset=%d", today.Offset))

	if page.Total != 14 {
		t.Errorf("the second page reports total=%d, want 14", page.Total)
	}
	if len(page.Conversations) != 4 {
		t.Fatalf("the second page carried %d rows, want the remaining 4", len(page.Conversations))
	}
	seen := map[int]bool{}
	for _, conversation := range today.Conversations {
		seen[conversation.ID] = true
	}
	for i, conversation := range page.Conversations {
		if seen[conversation.ID] {
			t.Errorf("conversation %d was served on both pages", conversation.ID)
		}
		if conversation.ID != newestFirst[10+i] {
			t.Errorf("second-page row %d is conversation %d, want %d", i, conversation.ID, newestFirst[10+i])
		}
		seen[conversation.ID] = true
	}
	if len(seen) != 14 {
		t.Errorf("the two pages together carried %d distinct conversations, want all 14", len(seen))
	}
}

// A bucket that fits in one page still reports the honest pair — the state the
// old code asserted for every bucket, which is why the defect was invisible on
// a small project.
func TestGroupedListingLeavesASmallBucketWhole(t *testing.T) {
	pool := newPinnedListingPool(t)
	seedTodayConversations(t, pool, "autotest_small", 3)

	today := bucketNamed(t, readPagedListing(t, pool, nil, "?grouped=true").DateGroups, "Today")
	if len(today.Conversations) != 3 || today.Total != 3 || today.Offset != 3 {
		t.Fatalf("rows=%d total=%d offset=%d, want 3/3/3", len(today.Conversations), today.Total, today.Offset)
	}
}

// `limit=` chooses the page size, so a client that wants the whole rail in one
// read can still have it.
func TestGroupedListingHonoursAnExplicitLimit(t *testing.T) {
	pool := newPinnedListingPool(t)
	seedTodayConversations(t, pool, "autotest_limit", 14)

	today := bucketNamed(t, readPagedListing(t, pool, nil, "?grouped=true&limit=50").DateGroups, "Today")
	if len(today.Conversations) != 14 || today.Total != 14 || today.Offset != 14 {
		t.Fatalf("rows=%d total=%d offset=%d, want 14/14/14 at limit=50", len(today.Conversations), today.Total, today.Offset)
	}
}

// A SEARCH is answered whole. The load-more fetchers do not carry the term, so
// a paged search would drop matches and then fetch unfiltered rows to replace
// them.
func TestGroupedListingDoesNotPageAFilteredListing(t *testing.T) {
	pool := newPinnedListingPool(t)
	seedTodayConversations(t, pool, "autotest_query", 14)

	today := bucketNamed(t, readPagedListing(t, pool, nil, "?grouped=true&query=autotest_query").DateGroups, "Today")
	if len(today.Conversations) != 14 {
		t.Fatalf("a filtered listing carried %d rows, want all 14 matches", len(today.Conversations))
	}
	if today.Total != 14 || today.Offset != 14 {
		t.Errorf("total=%d offset=%d, want 14/14 — the filtered answer is complete", today.Total, today.Offset)
	}
}

// A FOLDER pages the same way, and for the same reason: its client half is the
// same merge helper reading the same two counters.
func TestGroupedListingPagesAFolderAndReportsTheRemainder(t *testing.T) {
	pool := newPinnedListingPool(t)
	ids := seedTodayConversations(t, pool, "autotest_folder", 14)
	for _, id := range ids {
		if _, err := pool.Exec(context.Background(),
			`UPDATE p_1.chat_conversations SET folder_id = 5 WHERE id = $1`, id); err != nil {
			t.Fatalf("move conversation %d into the folder: %v", id, err)
		}
	}
	folders := []handler.Folder{{ID: "5", ProjectID: "1", Name: "autotest_folder_bucket"}}

	listing := readPagedListing(t, pool, folders, "?grouped=true")
	folder := bucketNamed(t, listing.Folders, "autotest_folder_bucket")
	if len(folder.Conversations) != 10 || folder.Total != 14 || folder.Offset != 10 {
		t.Fatalf("rows=%d total=%d offset=%d, want 10/14/10", len(folder.Conversations), folder.Total, folder.Offset)
	}
	// The date buckets are empty now: every conversation is in the folder.
	if len(listing.DateGroups) != 0 {
		t.Errorf("date_groups=%d, want 0 — every conversation sits in the folder", len(listing.DateGroups))
	}

	page := readPagedPage(t, pool, folders, fmt.Sprintf("?grouped=true&folder_id=5&limit=10&offset=%d", folder.Offset))
	if page.Total != 14 || len(page.Conversations) != 4 {
		t.Fatalf("the folder's second page: total=%d rows=%d, want 14/4", page.Total, len(page.Conversations))
	}
}
