//go:build conformance

package nativeclient_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
)

// ── 10. Client contract 1.4, over a native access token ────────────────────
//
// The Chats list's row actions (agent-zefir issue 6): rename (updateConversation),
// pin and unpin (pinEntity / unpinEntity on a conversation), the row's
// `is_pinned`, and that each change reaches another device through the
// conversation list's `changes_since` delta; a delete reaches it as one
// `deleted` tombstone. Called the way the app calls them: the device's
// `elnat_` bearer and its X-Client-Version.
func (s *suite) contract14(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)
	api := s.main.api
	project := s.projectID

	discovery := mustJSON(t, need(t)(api.Get(ctx, "/.well-known/elitea-client")), http.StatusOK)
	if !speaksAtLeast(discovery["client_contract"], 1, 4) {
		t.Fatalf("discovery client_contract = %v, want 1.4 or later", discovery["client_contract"])
	}

	list := client.ConversationPath(project)
	id, _ := s.createConversation(ctx, t, "native conformance 1.4")
	item := client.ConversationItemPath(project, id)
	// "Another device": a cursor taken after the create, read again after
	// each change. A row changed inside the settle window is re-delivered, so
	// every read from this cursor shows the row's current state.
	base, err := api.Sync(ctx, list, "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	row := client.RowsByID(base.Rows)[id]
	if row == nil {
		t.Fatalf("a full sync misses the new conversation %s", id)
	}
	if row["is_pinned"] != false {
		t.Errorf("a new conversation's is_pinned = %#v, want false", row["is_pinned"])
	}
	cursor := base.Cursor
	current := func(label string) map[string]any {
		t.Helper()
		delta, err := api.Sync(ctx, list, cursor, 0)
		if err != nil {
			t.Fatal(err)
		}
		got := client.RowsByID(delta.Rows)[id]
		if got == nil {
			t.Fatalf("%s: the delta misses conversation %s", label, id)
		}
		return got
	}

	// Rename: trimmed, answered with the stored conversation, and delivered.
	renamed := mustJSON(t, need(t)(api.Do(ctx, http.MethodPut, item, map[string]any{"name": "  renamed natively  "}, nil)), http.StatusOK)
	if renamed["name"] != "renamed natively" || renamed["id"] != id {
		t.Errorf("rename answered %v, want id %s and the trimmed name", renamed, id)
	}
	if got := current("rename"); got["name"] != "renamed natively" {
		t.Errorf("the delta after a rename carries name %#v", got["name"])
	}
	for _, body := range []map[string]any{{"name": ""}, {"name": "   "}, {"name": 7}, {"name": strings.Repeat("n", 256)}} {
		refused := need(t)(api.Do(ctx, http.MethodPut, item, body, nil))
		if refused.Status != http.StatusBadRequest {
			t.Errorf("rename with %v: %s, want 400", body, refused)
		}
	}
	if got := current("refused renames"); got["name"] != "renamed natively" {
		t.Errorf("a refused rename changed the name to %#v", got["name"])
	}

	// Pin and unpin: the shared project pin the web app sets.
	pin := fmt.Sprintf("/api/v2/social/pin/prompt_lib/%d/conversation/%s", project, id)
	mustJSON(t, need(t)(api.Do(ctx, http.MethodPost, pin, nil, nil)), http.StatusOK)
	if got := current("pin"); got["is_pinned"] != true {
		t.Errorf("the delta after a pin carries is_pinned %#v", got["is_pinned"])
	}
	legacy := mustJSON(t, need(t)(api.Get(ctx, list+"?limit=100")), http.StatusOK)
	rows, _ := legacy["rows"].([]any)
	foundPinned := false
	for _, raw := range rows {
		if r, _ := raw.(map[string]any); r != nil && client.RowID(r) == id {
			foundPinned = r["is_pinned"] == true
		}
	}
	if !foundPinned {
		t.Errorf("the list page does not show conversation %s pinned", id)
	}
	mustJSON(t, need(t)(api.Do(ctx, http.MethodDelete, pin, nil, nil)), http.StatusOK)
	if got := current("unpin"); got["is_pinned"] != false {
		t.Errorf("the delta after an unpin carries is_pinned %#v", got["is_pinned"])
	}
	// A pin of a conversation that does not exist is refused, not stored.
	if missing := need(t)(api.Do(ctx, http.MethodPost, fmt.Sprintf("/api/v2/social/pin/prompt_lib/%d/conversation/2147483000", project), nil, nil)); missing.Status != http.StatusNotFound {
		t.Errorf("pinning a missing conversation: %s, want 404", missing)
	}

	// Delete: one `deleted` tombstone, and the row does not come back.
	mustJSON(t, need(t)(api.Do(ctx, http.MethodPost, pin, nil, nil)), http.StatusOK)
	deleted := need(t)(api.Do(ctx, http.MethodDelete, item, nil, nil))
	if deleted.Status != http.StatusNoContent && deleted.Status != http.StatusOK {
		t.Fatalf("delete conversation: %s", deleted)
	}
	after, err := api.Sync(ctx, list, cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if client.RowsByID(after.Rows)[id] != nil {
		t.Errorf("the deleted conversation %s came back as a row", id)
	}
	if tomb, ok := client.TombstoneFor(after.Tombstones, id); !ok || tomb.Reason != "deleted" {
		t.Errorf("the delta after a delete has no `deleted` tombstone for %s: %+v", id, after.Tombstones)
	}
	s.done(t)
}

// speaksAtLeast reports whether a discovery `client_contract` value
// ("MAJOR.MINOR") is at least major.minor within the same major.
func speaksAtLeast(value any, major, minor int) bool {
	text, _ := value.(string)
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(text, "%d.%d", &gotMajor, &gotMinor); err != nil {
		return false
	}
	return gotMajor == major && gotMinor >= minor
}
