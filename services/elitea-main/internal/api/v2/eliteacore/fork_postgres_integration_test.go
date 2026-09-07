package eliteacore_test

// Real-PostgreSQL coverage for the FORK path — issue #505.
//
// `Handler.Fork` had no route. `POST /elitea_core/fork/prompt_lib/{projectID}`
// was registered on `ExportImportPost`, so the fork button ran the import.
// internal/api/router_fork_handler_test.go proves the registration; this file
// proves that the handler behind it does what the import cannot, and that the
// faults it used to answer with a bare `continue` now reach the caller.
//
// The body is the one the wizard sends: `{"applications": [...]}` built from an
// export fetched with `?fork=true`
// (apps/elitea-ui .../ImportWizardModal/IWModalForkButton.jsx).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// forkBody is one agent as the fork button sends it. `id` and `owner_id` are
// the source application and the source PROJECT, which only `?fork=true` adds
// to an export and only the fork handler reads.
func forkBody(variableName string) map[string]any {
	return map[string]any{
		"applications": []any{
			map[string]any{
				"id": "77", "owner_id": "9",
				"name": "forked agent", "description": "copied",
				"versions": []any{
					map[string]any{
						"name": "latest", "agent_type": "openai",
						"instructions":    "do the thing",
						"welcome_message": "hello",
						// The source project, which the fork must replace.
						"llm_settings": map[string]any{"model_name": "gpt-4o", "model_project_id": "9"},
						"variables":    []any{map[string]any{"name": variableName, "value": "secret-token"}},
						"tags":         []any{map[string]any{"name": "copied tag", "data": map[string]any{"colour": "blue"}}},
					},
				},
			},
		},
	}
}

// TestForkWritesWhatTheImportCannot is the reason the route was moved. Each
// assertion below is a row the import path never writes.
func TestForkWritesWhatTheImportCannot(t *testing.T) {
	pool := newImportLinkPool(t)
	router := forkRouter(eliteacore.NewHandler(pool), true)

	recorder := forkDo(t, router, forkBody("api_token"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var versionID, ownerID int
	var llmText, metaText string
	if err := pool.QueryRow(ctx, `
SELECT v.id, a.owner_id, COALESCE(v.llm_settings::text, ''), COALESCE(v.meta::text, '')
FROM p_1.application_versions v
JOIN p_1.applications a ON a.id = v.application_id`).Scan(&versionID, &ownerID, &llmText, &metaText); err != nil {
		t.Fatalf("the fork wrote no version: %v", err)
	}
	// `applications.owner_id` is the DESTINATION PROJECT, and the destination
	// schema is p_1 (#533). This route wrote the caller here, while the same
	// request reads the SOURCE row's owner_id back as `parent_project_id`
	// below: one column, two kinds of number, in one request.
	if ownerID != 1 {
		t.Errorf("applications.owner_id = %d, want the destination project 1", ownerID)
	}

	var llm map[string]any
	if err := json.Unmarshal([]byte(llmText), &llm); err != nil {
		t.Fatalf("decode llm_settings %q: %v", llmText, err)
	}
	// The copy must run on a model of the DESTINATION project. Serving this
	// route with the import left it pointing at project 9.
	if llm["model_project_id"] != "1" {
		t.Errorf("llm_settings.model_project_id = %v, want the destination project \"1\"", llm["model_project_id"])
	}

	var meta map[string]any
	if err := json.Unmarshal([]byte(metaText), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", metaText, err)
	}
	for key, want := range map[string]any{
		"parent_entity_id":  "77",
		"parent_project_id": "9",
		"parent_author_id":  strconv.Itoa(importLinkPrincipal),
	} {
		if meta[key] != want {
			t.Errorf("meta.%s = %v, want %v", key, meta[key], want)
		}
	}

	// The variable and the tag. The import reads neither key, so a fork served
	// by the import lost both without a word.
	var variableName, variableValue string
	if err := pool.QueryRow(ctx, `
SELECT name, COALESCE(value, '') FROM p_1.application_variables
WHERE application_version_id = $1`, versionID).Scan(&variableName, &variableValue); err != nil {
		t.Fatalf("the fork wrote no variable: %v", err)
	}
	if variableName != "api_token" || variableValue != "secret-token" {
		t.Errorf("variable = %q/%q, want api_token/secret-token", variableName, variableValue)
	}

	var tagName, tagData string
	if err := pool.QueryRow(ctx, `
SELECT t.name, COALESCE(t.data::text, '')
FROM p_1.application_version_tag_association vta
JOIN p_1.tags t ON t.id = vta.tag_id
WHERE vta.version_id = $1`, versionID).Scan(&tagName, &tagData); err != nil {
		t.Fatalf("the fork wrote no tag: %v", err)
	}
	if tagName != "copied tag" {
		t.Errorf("tag name = %q, want %q", tagName, "copied tag")
	}
	if !bytes.Contains([]byte(tagData), []byte(`"colour": "blue"`)) {
		t.Errorf("tag data = %q, want the data the file carried", tagData)
	}
}

// TestForkReportsAFailedVariableInsert breaks the variable statement and
// nothing else. It ran as `_, _ = h.pool.Exec(...)` under the words
// "best-effort insert", so the caller got 201 and an agent that cannot run.
func TestForkReportsAFailedVariableInsert(t *testing.T) {
	pool := newImportLinkPool(t)
	router := forkRouter(eliteacore.NewHandler(pool), true)

	roundTripExec(t, pool, `
ALTER TABLE p_1.application_variables ADD CONSTRAINT variable_must_fail CHECK (name <> 'api_token')`)

	recorder := forkDo(t, router, forkBody("api_token"))
	if recorder.Code == http.StatusCreated {
		t.Fatalf("fork reported success while the variable insert failed: %s", recorder.Body.String())
	}
	// 207: the agent and its version were forked. Only the variable was lost.
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 1 {
		t.Fatalf("errors.agents = %+v, want exactly the lost variable", answer.Errors.Agents)
	}
	// The wizard reads `selectedData[item.index].import_uuid` with no guard, so
	// an entry with no index would throw inside the browser and stop the whole
	// report. The entries this handler used to build carried no index at all.
	if answer.Errors.Agents[0].Index != 0 {
		t.Errorf("reported index = %d, want the agent's position 0", answer.Errors.Agents[0].Index)
	}
	if !bytes.Contains([]byte(answer.Errors.Agents[0].Msg), []byte("unable to fork variable api_token")) {
		t.Errorf("reported msg = %q, want the failed-variable message", answer.Errors.Agents[0].Msg)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.application_variables`); count != 0 {
		t.Errorf("application_variables rows = %d, want 0", count)
	}
}

// TestForkReportsAFailedVersionInsert covers the bare `continue` on the version
// statement. The application row was already written, so the fork answered 201
// and produced an agent with no version at all.
func TestForkReportsAFailedVersionInsert(t *testing.T) {
	pool := newImportLinkPool(t)
	router := forkRouter(eliteacore.NewHandler(pool), true)

	roundTripExec(t, pool, `
ALTER TABLE p_1.application_versions ADD CONSTRAINT version_must_fail CHECK (name <> 'latest')`)

	recorder := forkDo(t, router, forkBody("api_token"))
	if recorder.Code == http.StatusCreated {
		t.Fatalf("fork reported success while the version insert failed: %s", recorder.Body.String())
	}
	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 1 {
		t.Fatalf("errors.agents = %+v, want exactly the failed version", answer.Errors.Agents)
	}
	if !bytes.Contains([]byte(answer.Errors.Agents[0].Msg), []byte("unable to fork version latest")) {
		t.Errorf("reported msg = %q, want the failed-version message", answer.Errors.Agents[0].Msg)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.application_versions`); count != 0 {
		t.Errorf("application_versions rows = %d, want 0", count)
	}
}

// TestForkRefusesWithNoPrincipal is the fork half of the substitution repair.
// The handler read the principal over a `userID := 1` default, so an
// unreadable identifier gave the copy to user 1.
func TestForkRefusesWithNoPrincipal(t *testing.T) {
	pool := newImportLinkPool(t)
	router := forkRouter(eliteacore.NewHandler(pool), false)

	recorder := forkDo(t, router, forkBody("api_token"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 0 {
		t.Errorf("applications rows = %d, want 0 — a refused fork must write nothing", count)
	}
}

// TestForkRefusesABodyWithNoApplications covers the last silent success on this
// route. A body with no `applications` array answered 201 with an empty result,
// which the wizard reads as a completed fork.
func TestForkRefusesABodyWithNoApplications(t *testing.T) {
	pool := newImportLinkPool(t)
	router := forkRouter(eliteacore.NewHandler(pool), true)

	recorder := forkDo(t, router, map[string]any{"agents": []any{map[string]any{"name": "a"}}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 0 {
		t.Errorf("applications rows = %d, want 0", count)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func forkRouter(handler *eliteacore.Handler, withPrincipal bool) chi.Router {
	router := chi.NewRouter()
	if withPrincipal {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				user := auth.User{ID: strconv.Itoa(importLinkPrincipal)}
				next.ServeHTTP(w, request.WithContext(auth.ContextWithUser(request.Context(), user)))
			})
		})
	}
	router.Post("/elitea_core/fork/prompt_lib/{projectID}", handler.Fork)
	return router
}

func forkDo(t *testing.T, router chi.Router, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/elitea_core/fork/prompt_lib/1", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}
