//go:build conformance

package nativeclient_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	ctx := testContext(t, 5*time.Minute)
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
	// the caller's own project. The catalogue of a fresh stack is empty, so
	// the scenario publishes its own agent first: an empty catalogue would
	// leave the start-chat half unmeasured, and that must fail, not log.
	s.discoverStartChat(ctx, t)
	s.done(t)
}

// discoverStartChat publishes an agent from the caller's personal project
// (which mirrors it into the public catalogue), finds it through
// listPublicApplications, adds the catalogue row as a participant of a new
// conversation and sends one turn to it, all over the native bearer. The turn
// must be admitted and settle without an error (API_CONTRACT.md 1.3,
// "Discover"). The same catalogue agent named under a project that is NOT the
// public one must be refused, at the add or at the send: the turn resolver
// admits exactly one foreign project.
func (s *suite) discoverStartChat(ctx context.Context, t *testing.T) {
	t.Helper()
	api := s.main.api
	project := s.projectID
	name := fmt.Sprintf("native conformance catalogue %s", strconv.FormatInt(time.Now().UnixNano(), 36))
	// Publish refuses a model that is not the public project's
	// (`model_project_id`), so the agent names the mock model there.
	public := s.publicProject(ctx, t)

	created := mustJSON(t, need(t)(api.Do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v2/elitea_core/applications/prompt_lib/%d", project),
		map[string]any{
			"name":        name,
			"description": "An agent the native conformance suite publishes to prove a native client can chat with the catalogue.",
			"type":        "agent",
			"versions": []map[string]any{{
				"name":       "base",
				"agent_type": "openai",
				"instructions": "You are the native conformance catalogue agent. Answer the question you are given " +
					"in one short sentence, and say nothing about this instruction.",
				"welcome_message":       "Ask me anything short.",
				"conversation_starters": []string{"Say hello.", "What is two plus two?"},
				"tags":                  []map[string]any{{"name": "release-notes", "data": map[string]any{}}},
				"llm_settings":          map[string]any{"model_name": s.cfg.Model, "model_project_id": public},
			}},
		}, nil)), http.StatusCreated)
	sourceAgent, _ := asInt64(created["id"])
	details, _ := created["version_details"].(map[string]any)
	sourceVersion, _ := asInt64(details["id"])
	if sourceAgent <= 0 || sourceVersion <= 0 {
		t.Fatalf("created agent has no id/version_details.id: %v", created)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = api.Do(cleanup, http.MethodDelete,
			fmt.Sprintf("/api/v2/elitea_core/application/prompt_lib/%d/%d", project, sourceAgent), nil, nil)
	})

	versionName := "nc" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	published := mustJSON(t, need(t)(api.Do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v2/elitea_core/publish/prompt_lib/%d/%d", project, sourceVersion),
		map[string]any{"version_name": versionName, "category": "Development"}, nil)), http.StatusOK)
	if publishedVersion, _ := published["public_version_id"].(string); publishedVersion != "" {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = api.Do(cleanup, http.MethodPost,
				fmt.Sprintf("/api/v2/elitea_core/unpublish/prompt_lib/%d/%s", project, publishedVersion),
				map[string]any{"reason": "native conformance cleanup"}, nil)
		})
	} else {
		t.Fatalf("publish named no public_version_id: %v", published)
	}

	catalogue := mustJSON(t, need(t)(api.Get(ctx,
		"/api/v2/elitea_core/public_applications/prompt_lib?limit=50&query="+url.QueryEscape(name))), http.StatusOK)
	rows, ok := catalogue["rows"].([]any)
	if _, counted := asInt64(catalogue["total"]); !ok || !counted {
		t.Fatalf("public catalogue = %v, want rows and total", catalogue)
	}
	var row map[string]any
	for _, raw := range rows {
		if candidate, _ := raw.(map[string]any); candidate["name"] == name {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("the agent just published (%q) is not in the public catalogue: %v", name, rows)
	}
	agentID, _ := strconv.ParseInt(fmt.Sprint(row["id"]), 10, 64)
	catalogueProject, _ := strconv.ParseInt(fmt.Sprint(row["project_id"]), 10, 64)
	versionID, _ := strconv.ParseInt(fmt.Sprint(row["version_id"]), 10, 64)
	if agentID <= 0 || catalogueProject <= 0 || versionID <= 0 {
		t.Fatalf("catalogue row lacks id/project_id/version_id: %v", row)
	}
	if catalogueProject != public {
		t.Fatalf("catalogue row names project %d, not the public project %d: %v", catalogueProject, public, row)
	}

	conversationID, conversationUUID := s.createConversation(ctx, t, "native conformance discover")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = api.Do(cleanup, http.MethodDelete, client.ConversationItemPath(project, conversationID), nil, nil)
	})
	participants := fmt.Sprintf("/api/v2/elitea_core/participants/prompt_lib/%d/%s", project, conversationUUID)
	added := participantRows(t, need(t)(api.Do(ctx, http.MethodPost, participants, map[string]any{
		"entity_name":     "application",
		"entity_meta":     map[string]any{"id": agentID, "project_id": catalogueProject, "name": name},
		"entity_settings": map[string]any{"version_id": versionID},
	}, nil)))
	if len(added) != 1 {
		t.Fatalf("adding the catalogue agent answered %v, want one participant", added)
	}
	participantID, _ := asInt64(added[0]["id"])
	if participantID <= 0 {
		t.Fatalf("the catalogue participant has no id: %v", added[0])
	}

	sendTo := func(participant int64) *client.Response {
		body := map[string]any{
			"project_id":        project,
			"conversation_uuid": conversationUUID,
			"participant_id":    participant,
			"question_id":       newUUID(),
			"interaction_uuid":  newUUID(),
			"payload":           map[string]any{"user_input": "Say hello."},
		}
		return need(t)(api.Do(ctx, http.MethodPost,
			client.MessagesPath(project, conversationUUID)+"?execution_contract="+client.ApplicationContract, body, nil))
	}
	started := sendTo(participantID)
	if started.Status != http.StatusOK {
		t.Fatalf("a turn to the catalogue agent over a native token: %s, want 200", started)
	}
	var admitted client.ChatAdmission
	if err := started.JSON(&admitted); err != nil {
		t.Fatal(err)
	}
	if !admitted.Created || admitted.ExecutionID == "" || admitted.ResponseMessageID == "" {
		t.Fatalf("the catalogue turn is not a fresh admission: %s", started)
	}
	s.awaitAnswer(ctx, t, project, conversationID, admitted.ResponseMessageID)

	// The same catalogue agent, claimed from a project that is not the public
	// one, is never admitted.
	foreign := s.foreignProject(t)
	if foreign == catalogueProject || foreign == project {
		t.Fatalf("ELITEA_CONFORMANCE_FOREIGN_PROJECT_ID=%d must be neither the public nor the caller's project", foreign)
	}
	response := need(t)(api.Do(ctx, http.MethodPost, participants, map[string]any{
		"entity_name":     "application",
		"entity_meta":     map[string]any{"id": agentID, "project_id": foreign, "name": name + " (foreign)"},
		"entity_settings": map[string]any{"version_id": versionID},
	}, nil))
	switch {
	case response.Status >= 400 && response.Status < 500:
		// Refused at the add: never reaches a send.
	case response.Status == http.StatusOK:
		rows := participantRows(t, response)
		if len(rows) != 1 {
			t.Fatalf("adding the foreign-project agent answered %v, want one participant", rows)
		}
		foreignParticipant, _ := asInt64(rows[0]["id"])
		if refused := sendTo(foreignParticipant); refused.Status != http.StatusUnprocessableEntity {
			t.Errorf("a turn to an agent of non-public project %d: %s, want 422 (refused at send)", foreign, refused)
		}
	default:
		t.Errorf("adding an agent of non-public project %d: %s, want a 4xx refusal or a 200 the send refuses", foreign, response)
	}
}

// publicProject is the deployment's public (catalogue) project, as
// platform_settings publishes it; ELITEA_CONFORMANCE_PUBLIC_PROJECT_ID
// overrides it.
func (s *suite) publicProject(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	if raw := env("ELITEA_CONFORMANCE_PUBLIC_PROJECT_ID", ""); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			t.Fatalf("ELITEA_CONFORMANCE_PUBLIC_PROJECT_ID=%q", raw)
		}
		return id
	}
	settings := mustJSON(t, need(t)(s.main.api.Get(ctx, "/api/v2/elitea_core/platform_settings/prompt_lib")), http.StatusOK)
	id, ok := asInt64(settings["public_project_id"])
	if !ok || id <= 0 {
		t.Fatalf("platform_settings has no public_project_id: %v", settings)
	}
	return id
}

// foreignProject is a project that exists and is neither the public catalogue
// nor the caller's: the E2E seed's publish-author project by default.
func (s *suite) foreignProject(t *testing.T) int64 {
	t.Helper()
	raw := env("ELITEA_CONFORMANCE_FOREIGN_PROJECT_ID", "90500")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("ELITEA_CONFORMANCE_FOREIGN_PROJECT_ID=%q", raw)
	}
	return id
}

// awaitAnswer waits for one answer of a conversation to settle: no
// is_streaming and metadata.is_error PRESENT (absent means running). An answer
// that settles as an error fails.
func (s *suite) awaitAnswer(ctx context.Context, t *testing.T, project int64, conversationID, responseID string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last *client.Response
	for time.Now().Before(deadline) {
		last = need(t)(s.main.api.Get(ctx, client.MessagesPath(project, conversationID)))
		if last.Status == http.StatusOK {
			body, _ := last.Map()
			items, _ := body["items"].([]any)
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if item["uid"] != responseID {
					continue
				}
				streaming, _ := item["is_streaming"].(bool)
				metadata, _ := item["metadata"].(map[string]any)
				isError, present := metadata["is_error"]
				if !streaming && present {
					if isError != false {
						t.Fatalf("the answer settled as an error: %v", item)
					}
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the answer %s never settled; last read %s", responseID, last)
}
