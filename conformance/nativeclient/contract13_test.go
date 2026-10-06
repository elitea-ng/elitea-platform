//go:build conformance

package nativeclient_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
)

// ── 9. Client contract 1.3, over a native access token ─────────────────────
//
// Every operation contract 1.3 tags or describes, called the way a native
// client calls it (the device's `elnat_` bearer and its X-Client-Version), and
// the policy's data controls in discovery. A route that is mounted but
// refuses a native token, or answers a shape the spec does not promise,
// fails here rather than in the app.
func (s *suite) contract13(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)
	if s.conversationUUID == "" || s.admission.ResponseMessageID == "" {
		t.Fatal("the chat scenario left no conversation or answer")
	}
	api := s.main.api
	project := s.projectID

	// Discovery speaks 1.3 and publishes the data controls.
	discovery := mustJSON(t, need(t)(api.Get(ctx, "/.well-known/elitea-client")), http.StatusOK)
	if discovery["client_contract"] != "1.3" {
		t.Errorf("discovery client_contract = %v, want 1.3", discovery["client_contract"])
	}
	policy, _ := discovery["client_policy"].(map[string]any)
	for _, key := range []string{"allow_share_out", "allow_share_in", "allow_cloud_stt", "allow_notification_actions", "allow_system_surfaces"} {
		if _, ok := policy[key].(bool); !ok {
			t.Errorf("discovery client_policy.%s = %#v, want a boolean", key, policy[key])
		}
	}
	if preview := policy["notification_preview"]; preview != "none" && preview != "title" {
		t.Errorf("discovery client_policy.notification_preview = %#v, want none or title", preview)
	}

	// Message feedback: rate the chat scenario's answer, read it back, retract.
	feedback := fmt.Sprintf("/api/v2/elitea_core/message_feedback/prompt_lib/%d/%s", project, s.admission.ResponseMessageID)
	rated := mustJSON(t, need(t)(api.Do(ctx, http.MethodPost, feedback, map[string]any{"rating": 1, "comment": "native conformance"}, nil)), http.StatusOK)
	if likes, _ := asInt64(rated["likes"]); likes < 1 {
		t.Errorf("after a like the summary = %v", rated)
	}
	read := mustJSON(t, need(t)(api.Get(ctx, feedback)), http.StatusOK)
	if mine, _ := read["mine"].(map[string]any); mine == nil {
		t.Errorf("feedback read has no `mine` after rating: %v", read)
	}
	retracted := mustJSON(t, need(t)(api.Do(ctx, http.MethodDelete, feedback, nil, nil)), http.StatusOK)
	if _, mine := retracted["mine"]; mine {
		t.Errorf("feedback still has `mine` after the retraction: %v", retracted)
	}

	// Export: the whole transcript as a download.
	export := need(t)(api.Get(ctx, fmt.Sprintf("/api/v2/elitea_core/conversation_export/prompt_lib/%d/%s?format=md", project, s.conversationUUID)))
	if export.Status != http.StatusOK || len(export.Body) == 0 ||
		!strings.HasPrefix(export.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("export: %s (Content-Disposition %q)", export, export.Header.Get("Content-Disposition"))
	}

	// Stop: the chat scenario's answer has settled, so a stop is refused with
	// 409 (not running), and a malformed id with 400.
	stop := fmt.Sprintf("/api/v2/elitea_core/task/prompt_lib/%d/%s", project, s.admission.ResponseMessageID)
	if response := need(t)(api.Do(ctx, http.MethodDelete, stop, nil, nil)); response.Status != http.StatusConflict {
		t.Errorf("stopping a settled answer: %s, want 409", response)
	}
	malformed := fmt.Sprintf("/api/v2/elitea_core/task/prompt_lib/%d/not-a-uuid", project)
	if response := need(t)(api.Do(ctx, http.MethodDelete, malformed, nil, nil)); response.Status != http.StatusBadRequest {
		t.Errorf("stopping a malformed id: %s, want 400", response)
	}

	// Notifications: a bulk delete of an id that is not the caller's deletes
	// nothing and says so.
	bulk := mustJSON(t, need(t)(api.Do(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v2/notifications/notifications/prompt_lib/%d", project),
		map[string]any{"ids": []int64{2147483000}}, nil)), http.StatusOK)
	if deleted, ok := asInt64(bulk["deleted"]); !ok || deleted != 0 {
		t.Errorf("bulk delete of a foreign id = %v, want deleted 0", bulk)
	}

	// Suggestions, memories, usage and budget: read over the native bearer.
	recommendations := mustJSON(t, need(t)(api.Get(ctx, fmt.Sprintf("/api/v2/elitea_core/recommendations/prompt_lib/%d", project))), http.StatusOK)
	if _, ok := recommendations["applications"].([]any); !ok {
		t.Errorf("recommendations = %v, want an applications array", recommendations)
	}
	memories := mustJSON(t, need(t)(api.Get(ctx, fmt.Sprintf("/api/v2/elitea_core/memories/prompt_lib/%d", project))), http.StatusOK)
	if _, ok := memories["items"].([]any); !ok {
		t.Errorf("memories = %v, want an items array", memories)
	}
	// Deleting a memory that does not exist is idempotent.
	if response := need(t)(api.Do(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v2/elitea_core/memory/prompt_lib/%d/%s", project, newUUID()), nil, nil)); response.Status != http.StatusNoContent {
		t.Errorf("deleting an absent memory: %s, want 204", response)
	}
	for _, path := range []string{
		fmt.Sprintf("/api/v2/elitea_core/usage/prompt_lib/%d/usage?scope=user", project),
		fmt.Sprintf("/api/v2/elitea_core/project_budget/prompt_lib/%d/budget", project),
	} {
		if response := need(t)(api.Get(ctx, path)); response.Status != http.StatusOK {
			t.Errorf("GET %s with a native token: %s", path, response)
		}
	}
	me := mustJSON(t, need(t)(api.Get(ctx, "/api/v2/social/author")), http.StatusOK)
	if id, _ := me["id"].(string); id != "" {
		if response := need(t)(api.Get(ctx, fmt.Sprintf("/api/v2/elitea_core/user_budget/prompt_lib/%d/user_budget/%s", project, id))); response.Status != http.StatusOK {
			t.Errorf("own member budget: %s", response)
		}
	}

	// Discover: the public catalogue, and a chat with one of its agents from
	// the caller's own project (create a conversation, add the agent).
	catalogue := mustJSON(t, need(t)(api.Get(ctx, "/api/v2/elitea_core/public_applications/prompt_lib?limit=5")), http.StatusOK)
	rows, ok := catalogue["rows"].([]any)
	if _, counted := asInt64(catalogue["total"]); !ok || !counted {
		t.Fatalf("public catalogue = %v, want rows and total", catalogue)
	}
	if len(rows) == 0 {
		t.Log("the public catalogue is empty on this deployment; the start-chat half of discover is not exercised")
	} else {
		row, _ := rows[0].(map[string]any)
		agentID, _ := strconv.ParseInt(fmt.Sprint(row["id"]), 10, 64)
		catalogueProject, _ := strconv.ParseInt(fmt.Sprint(row["project_id"]), 10, 64)
		versionID, _ := strconv.ParseInt(fmt.Sprint(row["version_id"]), 10, 64)
		if agentID <= 0 || catalogueProject <= 0 || versionID <= 0 {
			t.Fatalf("catalogue row lacks id/project_id/version_id: %v", row)
		}
		conversationID, conversationUUID := s.createConversation(ctx, t, "native conformance discover")
		added := participantRows(t, need(t)(api.Do(ctx, http.MethodPost,
			fmt.Sprintf("/api/v2/elitea_core/participants/prompt_lib/%d/%s", project, conversationUUID),
			map[string]any{
				"entity_name":     "application",
				"entity_meta":     map[string]any{"id": agentID, "project_id": catalogueProject, "name": row["name"]},
				"entity_settings": map[string]any{"version_id": versionID},
			}, nil)))
		if len(added) != 1 {
			t.Errorf("adding the catalogue agent answered %v, want one participant", added)
		}
		if response := need(t)(api.Do(ctx, http.MethodDelete, client.ConversationItemPath(project, conversationID), nil, nil)); response.Status != http.StatusOK && response.Status != http.StatusNoContent {
			t.Errorf("delete the discover conversation: %s", response)
		}
	}
	s.done(t)
}
