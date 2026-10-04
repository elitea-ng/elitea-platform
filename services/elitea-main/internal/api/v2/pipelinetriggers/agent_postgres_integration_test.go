package pipelinetriggers_test

// Legacy issue 6656: the inbound trigger starts an ordinary AGENT, not only a
// pipeline. Each test drives the HTTP route against real rows. The shared
// harness is in pipelinetriggers_postgres_integration_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
)

// seedAgent writes one application and one ordinary AGENT version that
// declares the given variables, and returns the version id.
func seedAgent(t *testing.T, pool *pgxpool.Pool, schema, name string, owner int64, variables ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var applicationID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s.applications (name, description, owner_id) VALUES ($1, '', $2) RETURNING id`, schema),
		name, owner).Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	rows := make([]map[string]string, 0, len(variables))
	for _, variable := range variables {
		rows = append(rows, map[string]string{"name": variable, "value": ""})
	}
	meta, err := json.Marshal(map[string]any{"variables": rows})
	if err != nil {
		t.Fatal(err)
	}
	var versionID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions (application_id, name, status, author_id, agent_type, llm_settings, meta)
VALUES ($1, 'latest', 'published', $2, 'openai', '{}'::jsonb, $3::jsonb)
RETURNING id`, schema), applicationID, owner, string(meta)).Scan(&versionID); err != nil {
		t.Fatalf("seed agent version: %v", err)
	}
	return versionID
}

// mintAgentTrigger creates an agent version and its trigger in the given mode,
// and returns the version id, the secret and the url the settings route
// handed out.
func (h *harness) mintAgentTrigger(t *testing.T, name, modeBody string, variables ...string) (int64, string, string) {
	t.Helper()
	versionID := seedAgent(t, h.pool, homeSchema, name, ownerUserID, variables...)
	response := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), modeBody, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint agent trigger: status = %d, body = %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	secret, _ := body["secret"].(string)
	url, _ := body["url"].(string)
	if secret == "" || url == "" {
		t.Fatalf("mint agent trigger returned no usable trigger: %v", body)
	}
	return versionID, secret, url
}

// mappingSettings reads the agent participant's mapping for the dispatched
// conversation — the row ResolveCurrentApplicationTurn reads.
func (h *harness) mappingSettings(t *testing.T, conversationUUID string, participantID int64) map[string]any {
	t.Helper()
	var raw string
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(`
SELECT mapping.entity_settings::text
  FROM %[1]s.chat_participant_mapping AS mapping
  JOIN %[1]s.chat_conversations AS conversation ON conversation.id = mapping.conversation_id
 WHERE conversation.uuid = $1::uuid AND mapping.participant_id = $2`, homeSchema),
		conversationUUID, participantID).Scan(&raw); err != nil {
		t.Fatalf("read the agent mapping: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

// recordingEvents keeps every emitted event type.
type recordingEvents struct {
	mu    sync.Mutex
	types []string
}

func (e *recordingEvents) Emit(_ context.Context, _, eventType string, _ any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.types = append(e.types, eventType)
}

func TestAnAgentTriggerStartsAnAgentRunWithItsInputAndVariables(t *testing.T) {
	h := newHarness(t)
	events := &recordingEvents{}
	h.handler = pipelinetriggers.NewPlatformHandler(
		h.pool, h.start, h.vault,
		fixedPermissions{userID: ownerUserID, projectID: homeProject, permissions: []string{pipelinetriggers.RunPermission}},
		h.recorder, nil, events, nil,
	)
	h.router = mountRoutes(h.handler)

	versionID, secret, url := h.mintAgentTrigger(t, "Triage agent", "", "topic", "tone")
	// The SAME inbound route a pipeline uses: a sender's URL has one shape.
	if !strings.HasPrefix(url, "/api/v2/pipeline_trigger/"+homeProject+"/") {
		t.Fatalf("url = %q, want the inbound trigger route", url)
	}

	response := h.do(t, http.MethodPost, url,
		`{"input":"triage issue 42","variables":{"topic":"billing","undeclared":"ignored","tone":7}}`,
		map[string]string{"Authorization": "Bearer " + secret})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	if body := decode(t, response); body["version_id"] != float64(versionID) {
		t.Fatalf("version_id = %v, want %d", body["version_id"], versionID)
	}

	request, ok := h.start.last()
	if !ok {
		t.Fatal("nothing was dispatched")
	}
	if request.UserInput != "triage issue 42" {
		t.Fatalf("UserInput = %q, want the body's input", request.UserInput)
	}
	if request.AllowEmptyUserInput {
		t.Fatal("an agent run must keep the use case's refusal of an empty turn")
	}
	if request.ActorUserID != ownerUserID {
		t.Fatalf("ActorUserID = %d, want the trigger's creator", request.ActorUserID)
	}

	settings := h.mappingSettings(t, request.ConversationUUID, request.TargetParticipantID)
	if settings["version_id"] != float64(versionID) {
		t.Fatalf("mapped version = %v, want %d", settings["version_id"], versionID)
	}
	encoded, _ := json.Marshal(settings["variables"])
	if string(encoded) != `[{"name":"topic","value":"billing"},{"name":"tone","value":"7"}]` {
		t.Fatalf("mapped variables = %s — only DECLARED names, in declaration order", encoded)
	}

	// The pipeline vocabulary is not used for an agent run.
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.types) != 0 {
		t.Fatalf("an agent run emitted %v; pipeline.run.* describes pipelines only", events.types)
	}
}

func TestAnAgentTriggerReadsTheProviderPayloadWhenThereIsNoInput(t *testing.T) {
	h := newHarness(t)
	_, secret, url := h.mintAgentTrigger(t, "Repository reviewer", `{"type":"github"}`)
	if !strings.HasSuffix(url, "/github") {
		t.Fatalf("url = %q, want the GitHub preset suffix", url)
	}

	body := `{"action":"opened","pull_request":{"number":7,"title":"Fix the build"}}`
	response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	request, ok := h.start.last()
	if !ok {
		t.Fatal("nothing was dispatched")
	}
	if request.UserInput != body {
		t.Fatalf("UserInput = %q, want the signed payload", request.UserInput)
	}

	// REPLAY: the same signed delivery starts no second agent run, and is
	// answered with the first run.
	replay := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)})
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202; body = %s", replay.Code, replay.Body.String())
	}
	if decode(t, replay)["execution_id"] != decode(t, response)["execution_id"] {
		t.Fatal("the replay was not answered with the first run")
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}
}

func TestAnAgentTriggerWithNothingToReadIsA422NamingInput(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"input":""}`, `{"variables":{"topic":"x"}}`} {
		t.Run(fmt.Sprintf("body %q", body), func(t *testing.T) {
			h := newHarness(t)
			_, secret, url := h.mintAgentTrigger(t, "Quiet agent", "", "topic")
			response := h.do(t, http.MethodPost, url, body, map[string]string{"Authorization": "Bearer " + secret})
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "`input`") {
				t.Fatalf("body = %s, want it to name `input`", response.Body.String())
			}
			if h.start.count() != 0 {
				t.Fatalf("dispatches = %d, want 0", h.start.count())
			}
			if n := h.triggerConversations(t); n != 0 {
				t.Fatalf("a refused agent call left %d trigger conversations behind", n)
			}
		})
	}
}

func TestAnAgentTriggerRefusesEveryUnusableCredential(t *testing.T) {
	h := newHarness(t)
	versionID, secret, url := h.mintAgentTrigger(t, "Guarded agent", "")
	body := `{"input":"hello"}`

	refusals := map[string]map[string]string{
		"no credential":    {},
		"wrong secret":     {"Authorization": "Bearer not-the-secret"},
		"secret as header": {pipelinetriggers.TriggerTokenHeader: secret + "x"},
	}
	var refusalBody string
	for name, headers := range refusals {
		response := h.do(t, http.MethodPost, url, body, headers)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401; body = %s", name, response.Code, response.Body.String())
		}
		if refusalBody == "" {
			refusalBody = response.Body.String()
		} else if response.Body.String() != refusalBody {
			t.Fatalf("%s: the refusal differs from the others: %s vs %s", name, response.Body.String(), refusalBody)
		}
	}

	// The creator lost the run permission: the same refusal, no run.
	stripped := pipelinetriggers.NewPlatformHandler(
		h.pool, h.start, h.vault,
		fixedPermissions{userID: ownerUserID, projectID: homeProject, permissions: nil},
		h.recorder, nil, nil, nil,
	)
	router := chi.NewRouter()
	router.Post(pipelinetriggers.InboundPath, stripped.Trigger)
	request := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+secret)
	lost := httptest.NewRecorder()
	router.ServeHTTP(lost, request)
	if lost.Code != http.StatusUnauthorized || lost.Body.String() != refusalBody {
		t.Fatalf("lost permission: status = %d, body = %s; want the one 401 refusal", lost.Code, lost.Body.String())
	}

	// Revoked: the same refusal, no run.
	revoke := h.do(t, http.MethodDelete,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d, body = %s", revoke.Code, revoke.Body.String())
	}
	revoked := h.do(t, http.MethodPost, url, body, map[string]string{"Authorization": "Bearer " + secret})
	if revoked.Code != http.StatusUnauthorized || revoked.Body.String() != refusalBody {
		t.Fatalf("revoked: status = %d, body = %s; want the one 401 refusal", revoked.Code, revoked.Body.String())
	}

	if h.start.count() != 0 {
		t.Fatalf("a refused agent call dispatched %d runs", h.start.count())
	}
}

// TestTheScheduleStaysPipelineOnly pins the boundary: the trigger opened to
// agents, the schedule did not.
func TestTheScheduleStaysPipelineOnly(t *testing.T) {
	h := newHarness(t)
	versionID := seedAgent(t, h.pool, homeSchema, "Not schedulable", ownerUserID)
	response := h.do(t, http.MethodPut,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID),
		`{"cron":"0 * * * *","active":true,"input":"hello"}`, nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", response.Code, response.Body.String())
	}
}
