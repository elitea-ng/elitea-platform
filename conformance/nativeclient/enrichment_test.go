//go:build conformance

package nativeclient_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
)

// ── 8. Client contract 1.1: chat enrichment, over a native access token ─────
//
// Every operation contract 1.1 added, called the way a native client calls
// it: with the device's `elnat_` bearer and its X-Client-Version. Each answer
// is checked for the shape the client reads, so a route that is mounted but
// refuses a native token — or answers a shape the spec does not promise —
// fails here rather than in the app.
func (s *suite) chatEnrichment(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)
	if s.conversationUUID == "" {
		t.Fatal("the chat scenario created no conversation")
	}
	api := s.main.api

	// Discovery carries the attachment policy (1.1); its version is checked
	// by the contract_1_3 scenario.
	discovery := mustJSON(t, need(t)(api.Get(ctx, "/.well-known/elitea-client")), http.StatusOK)
	policy, _ := discovery["attachments"].(map[string]any)
	if chunk, _ := asInt64(policy["chunk_bytes"]); chunk <= 0 || policy["inline_image_downscale"] != true {
		t.Errorf("discovery attachments policy = %v", policy)
	}

	// Who am I (1.2): the caller's identity and the personal project the chat
	// scenario found in the project list, over the native bearer.
	me := mustJSON(t, need(t)(api.Get(ctx, "/api/v2/social/author")), http.StatusOK)
	if id, _ := me["id"].(string); id == "" {
		t.Errorf("current user has no id: %v", me)
	}
	for _, key := range []string{"name", "email", "avatar"} {
		if _, ok := me[key].(string); !ok {
			t.Errorf("current user %s = %#v, want a string", key, me[key])
		}
	}
	if me["personal_project_id"] != strconv.FormatInt(s.projectID, 10) {
		t.Errorf("current user personal_project_id = %v, want %d", me["personal_project_id"], s.projectID)
	}

	// Participants: add (single object), retry answers the same row, read
	// back on the conversation, remove.
	participants := fmt.Sprintf("/api/v2/elitea_core/participants/prompt_lib/%d/%s", s.projectID, s.conversationUUID)
	add := map[string]any{"entity_name": "llm", "entity_meta": map[string]any{"model_name": s.cfg.Model}}
	first := participantRows(t, need(t)(api.Do(ctx, http.MethodPost, participants, add, nil)))
	retry := participantRows(t, need(t)(api.Do(ctx, http.MethodPost, participants, add, nil)))
	if len(first) != 1 || len(retry) != 1 || first[0]["id"] != retry[0]["id"] {
		t.Fatalf("add then retry answered %v then %v, want the same single row", first, retry)
	}
	participantID, _ := asInt64(first[0]["id"])
	conversation := mustJSON(t, need(t)(api.Get(ctx, client.ConversationItemPath(s.projectID, s.conversationUUID))), http.StatusOK)
	if !hasParticipant(conversation, participantID) {
		t.Fatalf("participant %d is not on the conversation: %v", participantID, conversation["participants"])
	}

	// Candidates: the caller is a member and already a participant.
	candidates := mustJSON(t, need(t)(api.Get(ctx, fmt.Sprintf(
		"/api/v2/elitea_core/participant_candidates/prompt_lib/%d/%s?limit=100", s.projectID, s.conversationUUID))), http.StatusOK)
	rows, _ := candidates["rows"].([]any)
	if _, ok := candidates["has_more"].(bool); !ok || len(rows) == 0 {
		t.Fatalf("participant candidates = %v", candidates)
	}
	sawSelf := false
	for _, raw := range rows {
		if row, _ := raw.(map[string]any); row["already_participant"] == true {
			sawSelf = true
		}
	}
	if !sawSelf {
		t.Errorf("no candidate is already a participant (the caller is): %v", rows)
	}

	// Agents, toolkits and tool history: a native token reads them.
	for _, path := range []string{
		fmt.Sprintf("/api/v2/elitea_core/applications/prompt_lib/%d", s.projectID),
		fmt.Sprintf("/api/v2/elitea_core/tools/prompt_lib/%d", s.projectID),
		fmt.Sprintf("/api/v2/elitea_core/message_traces/prompt_lib/%d/%s", s.projectID, s.conversationID),
	} {
		if response := need(t)(api.Get(ctx, path)); response.Status != http.StatusOK {
			t.Errorf("GET %s with a native token: %s", path, response)
		}
	}

	// Attachments: upload, download the same bytes as a download, and an
	// SVG is never served as itself.
	text := []byte("native conformance attachment " + newUUID())
	name := "conformance-" + newUUID()[:8] + ".txt"
	filepath := uploadAttachment(ctx, t, api, s.projectID, s.conversationUUID, name, text)
	if !strings.HasPrefix(filepath, "/") || !strings.HasSuffix(filepath, "/"+s.conversationUUID+"/"+name) {
		t.Fatalf("upload answered filepath %q", filepath)
	}
	download := need(t)(api.Get(ctx, attachmentPath(s.projectID, s.conversationUUID, name)))
	if download.Status != http.StatusOK || !bytes.Equal(download.Body, text) {
		t.Fatalf("download: %s", download)
	}
	if download.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(download.Header.Get("Content-Disposition"), "attachment;") ||
		!strings.Contains(download.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("download headers = %v", download.Header)
	}
	svg := "conformance-" + newUUID()[:8] + ".svg"
	uploadAttachment(ctx, t, api, s.projectID, s.conversationUUID, svg,
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	served := need(t)(api.Get(ctx, attachmentPath(s.projectID, s.conversationUUID, svg)))
	if served.Status != http.StatusOK || served.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("an SVG attachment was served as %q (%d), want application/octet-stream", served.Header.Get("Content-Type"), served.Status)
	}
	// A traversal name never reaches a file: 400 from elitea-main, or a
	// refusal from the edge in front of it — never the bytes.
	traversal := need(t)(api.Get(ctx, fmt.Sprintf("/api/v2/elitea_core/attachments/prompt_lib/%d/%s/..%%2F%s",
		s.projectID, s.conversationUUID, name)))
	if traversal.Status == http.StatusOK || bytes.Equal(traversal.Body, text) {
		t.Errorf("a traversal name was served: %s", traversal)
	}

	// Remove the participant added above.
	remove := fmt.Sprintf("/api/v2/elitea_core/participant/prompt_lib/%d/%s/%d", s.projectID, s.conversationUUID, participantID)
	if response := need(t)(api.Do(ctx, http.MethodDelete, remove, nil, nil)); response.Status != http.StatusNoContent {
		t.Fatalf("remove participant: %s", response)
	}
	s.done(t)
}

func participantRows(t *testing.T, response *client.Response) []map[string]any {
	t.Helper()
	if response.Status != http.StatusOK {
		t.Fatalf("add participant: %s", response)
	}
	var rows []map[string]any
	if err := response.JSON(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func hasParticipant(conversation map[string]any, id int64) bool {
	participants, _ := conversation["participants"].([]any)
	for _, raw := range participants {
		participant, _ := raw.(map[string]any)
		if got, ok := asInt64(participant["id"]); ok && got == id {
			return true
		}
	}
	return false
}

func attachmentPath(projectID int64, conversationUUID, name string) string {
	return fmt.Sprintf("/api/v2/elitea_core/attachments/prompt_lib/%d/%s/%s", projectID, conversationUUID, url.PathEscape(name))
}

// uploadAttachment sends one single-shot multipart upload and answers the
// stored filepath.
func uploadAttachment(ctx context.Context, t *testing.T, api *client.Client, projectID int64, conversationUUID, name string, content []byte) string {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("file_name", name); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := api.NewRequest(ctx, http.MethodPost,
		fmt.Sprintf("/api/v2/elitea_core/attachments/prompt_lib/%d/%s", projectID, conversationUUID), body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := api.HTTP.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", name, response.StatusCode, raw)
	}
	var created []struct {
		Filepath string `json:"filepath"`
	}
	if err := (&client.Response{Status: response.StatusCode, Body: raw}).JSON(&created); err != nil || len(created) != 1 {
		t.Fatalf("upload %s answered %s (%v)", name, raw, err)
	}
	return created[0].Filepath
}
