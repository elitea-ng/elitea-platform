package eliteacore_test

// The team import (issue 3010) on the production migration chain: a set of
// agents imported as one saved team, all or nothing. And the plain import of
// the shape the web sends, `{applications, toolkits, skills}`, whose
// top-level toolkits the server used to ignore (every agent with a toolkit
// answered 207 "unable to link tools").
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func teamImportRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			user := auth.User{ID: strconv.Itoa(importLinkPrincipal)}
			next.ServeHTTP(w, request.WithContext(auth.ContextWithUser(request.Context(), user)))
		})
	})
	router.Post("/elitea_core/import_wizard/prompt_lib/{projectID}", handler.ExportImportPost)
	router.Post("/elitea_core/team_import/prompt_lib/{projectID}", handler.TeamImportPost)
	router.Get("/elitea_core/export_import/prompt_lib/{projectID}/{entityID}", handler.ExportImportGet)
	return router
}

func teamImportPost(t *testing.T, router chi.Router, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// teamAgent is one agent entry the way the web reader writes an .agent.md.
func teamAgent(uuid, name string, toolRefs ...map[string]any) map[string]any {
	tools := []map[string]any{}
	tools = append(tools, toolRefs...)
	return map[string]any{
		"entity":      "agents",
		"import_uuid": uuid,
		"name":        name,
		"description": name + " agent",
		"versions": []map[string]any{{
			"name":                "base",
			"agent_type":          "openai",
			"instructions":        "Work as " + name + ".",
			"import_version_uuid": uuid + "-v",
			"tools":               tools,
		}},
	}
}

func teamToolkit(uuid string) map[string]any {
	return map[string]any{
		"import_uuid": uuid,
		"name":        "repo toolkit",
		"type":        "github",
		"description": "the repository",
		"settings":    map[string]any{"url": "https://example.invalid"},
	}
}

func teamDocument(team map[string]any, agents []map[string]any, toolkits []map[string]any) map[string]any {
	return map[string]any{"team": team, "applications": agents, "toolkits": toolkits}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	return importLinkCount(t, pool, query)
}

func assertNothingImported(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for label, query := range map[string]string{
		"agents":   `SELECT count(*) FROM p_1.applications WHERE name LIKE 'Dev %'`,
		"toolkits": `SELECT count(*) FROM p_1.elitea_tools WHERE name = 'repo toolkit'`,
		"teams":    `SELECT count(*) FROM elitea_chat.agent_teams WHERE project_id = 1 AND name = 'Dev team'`,
	} {
		if count := countRows(t, pool, query); count != 0 {
			t.Errorf("a refused team import kept %d %s", count, label)
		}
	}
}

func TestTeamImportCreatesTheAgentsTheirToolkitAndTheTeam(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))

	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", teamDocument(
		map[string]any{"name": "Dev team", "description": "Builds the platform", "lead": "md-lead", "members": []string{"md-lead", "md-impl"}},
		[]map[string]any{
			teamAgent("md-lead", "Dev Coordinator"),
			teamAgent("md-impl", "Dev Implementer", map[string]any{"import_uuid": "tool-1", "selected_tools": []string{"get_issue"}}),
		},
		[]map[string]any{teamToolkit("tool-1")},
	))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("team import: got %d: %s", recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Team struct {
			ID           int64  `json:"id"`
			FloorPolicy  string `json:"floor_policy"`
			LeadPosition *int   `json:"lead_position"`
			Members      []struct {
				ApplicationID int64  `json:"application_id"`
				VersionID     int64  `json:"version_id"`
				Name          string `json:"name"`
			} `json:"members"`
		} `json:"team"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer.Team.FloorPolicy != "lead" || answer.Team.LeadPosition == nil || *answer.Team.LeadPosition != 1 {
		t.Errorf("the team's floor = %q lead %v, want lead 1", answer.Team.FloorPolicy, answer.Team.LeadPosition)
	}
	if len(answer.Team.Members) != 2 || answer.Team.Members[0].Name != "Dev Coordinator" || answer.Team.Members[1].Name != "Dev Implementer" {
		t.Fatalf("the team's members = %+v", answer.Team.Members)
	}
	// Each member is the agent the import wrote, at its version.
	for _, member := range answer.Team.Members {
		if countRows(t, pool, `SELECT count(*) FROM p_1.application_versions WHERE id = `+strconv.FormatInt(member.VersionID, 10)+
			` AND application_id = `+strconv.FormatInt(member.ApplicationID, 10)) != 1 {
			t.Errorf("member %q names no imported version", member.Name)
		}
	}
	if countRows(t, pool, `SELECT count(*) FROM elitea_chat.agent_team_members WHERE team_id = `+strconv.FormatInt(answer.Team.ID, 10)) != 2 {
		t.Error("the team's roster rows are missing")
	}
	// The toolkit is linked to the implementer's version.
	if countRows(t, pool, `SELECT count(*) FROM p_1.entity_tool_mapping m JOIN p_1.elitea_tools t ON t.id = m.tool_id
		WHERE t.name = 'repo toolkit' AND m.entity_version_id = `+strconv.FormatInt(answer.Team.Members[1].VersionID, 10)) != 1 {
		t.Error("the implementer's toolkit is not linked")
	}
}

func TestTeamImportRefusesATakenAgentNameAndKeepsNothing(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))
	if recorder := teamImportPost(t, router, "/elitea_core/import_wizard/prompt_lib/1",
		map[string]any{"applications": []map[string]any{teamAgent("old", "dev implementer")}}); recorder.Code != http.StatusCreated {
		t.Fatalf("seed the existing agent: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", teamDocument(
		map[string]any{"name": "Dev team", "members": []string{"md-lead", "md-impl"}},
		[]map[string]any{teamAgent("md-lead", "Dev Coordinator"), teamAgent("md-impl", "Dev Implementer")},
		nil,
	))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "agent_name_taken") ||
		!strings.Contains(recorder.Body.String(), "Dev Implementer") {
		t.Fatalf("a taken agent name: got %d: %s", recorder.Code, recorder.Body.String())
	}
	assertNothingImported(t, pool)
}

func TestTeamImportRefusesAMissingToolkitAndKeepsNothing(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))

	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", teamDocument(
		map[string]any{"name": "Dev team", "members": []string{"md-lead", "md-impl"}},
		[]map[string]any{
			teamAgent("md-lead", "Dev Coordinator"),
			teamAgent("md-impl", "Dev Implementer", map[string]any{"import_uuid": "tool-absent", "name": "jira"}),
		},
		nil,
	))
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "toolkit_missing") ||
		!strings.Contains(recorder.Body.String(), "Dev Implementer: jira") {
		t.Fatalf("a missing toolkit: got %d: %s", recorder.Code, recorder.Body.String())
	}
	assertNothingImported(t, pool)
}

func TestTeamImportRefusesATakenTeamNameAndKeepsNothing(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_chat.agent_teams (project_id, name, created_by) VALUES (1, 'DEV TEAM', 7)`); err != nil {
		t.Fatalf("seed the team: %v", err)
	}

	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", teamDocument(
		map[string]any{"name": "Dev team", "members": []string{"md-lead", "md-impl"}},
		[]map[string]any{teamAgent("md-lead", "Dev Coordinator"), teamAgent("md-impl", "Dev Implementer")},
		nil,
	))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "team_name_taken") {
		t.Fatalf("a taken team name: got %d: %s", recorder.Code, recorder.Body.String())
	}
	assertNothingImported(t, pool)
}

func TestTeamImportRefusesAMemberThatIsNotInTheFile(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))

	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", teamDocument(
		map[string]any{"name": "Dev team", "lead": "md-ghost", "members": []string{"md-lead", "md-ghost"}},
		[]map[string]any{teamAgent("md-lead", "Dev Coordinator")},
		nil,
	))
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "team_invalid") {
		t.Fatalf("a member not in the file: got %d: %s", recorder.Code, recorder.Body.String())
	}
	assertNothingImported(t, pool)
}

// The plain import of the web's shape: the top-level toolkit is imported and
// linked, so the answer is 201, not the 207 the dropped array produced.
func TestImportLinksTheTopLevelToolkitsOfTheDocument(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))

	recorder := teamImportPost(t, router, "/elitea_core/import_wizard/prompt_lib/1", map[string]any{
		"applications": []map[string]any{teamAgent("md-impl", "Dev Implementer", map[string]any{"import_uuid": "tool-1"})},
		"toolkits":     []map[string]any{teamToolkit("tool-1")},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import with a top-level toolkit: got %d: %s", recorder.Code, recorder.Body.String())
	}
	if countRows(t, pool, `SELECT count(*) FROM p_1.entity_tool_mapping m JOIN p_1.elitea_tools t ON t.id = m.tool_id WHERE t.name = 'repo toolkit'`) != 1 {
		t.Error("the top-level toolkit was not linked")
	}
}

// A team's export pins each member to its team version, so an export that
// names a version keeps that one version, fork or not. Without fork the id
// was ignored and every version came back.
func TestExportKeepsTheNamedVersionWithoutFork(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))

	agent := teamAgent("md-two", "Dev Two")
	versions := agent["versions"].([]map[string]any)
	second := map[string]any{"name": "second", "agent_type": "openai", "instructions": "Second.", "import_version_uuid": "md-two-v2", "tools": []any{}}
	agent["versions"] = append(versions, second)
	recorder := teamImportPost(t, router, "/elitea_core/import_wizard/prompt_lib/1", map[string]any{"applications": []map[string]any{agent}})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed a two-version agent: %d %s", recorder.Code, recorder.Body.String())
	}
	var appID, secondID int64
	if err := pool.QueryRow(t.Context(), `SELECT a.id, v.id FROM p_1.applications a JOIN p_1.application_versions v ON v.application_id = a.id
		WHERE a.name = 'Dev Two' AND v.name = 'second'`).Scan(&appID, &secondID); err != nil {
		t.Fatalf("read the seeded version: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/elitea_core/export_import/prompt_lib/1/"+strconv.FormatInt(appID, 10)+
		"?follow_version_ids="+strconv.FormatInt(secondID, 10), nil)
	exported := httptest.NewRecorder()
	router.ServeHTTP(exported, request)
	if exported.Code != http.StatusOK {
		t.Fatalf("export: %d %s", exported.Code, exported.Body.String())
	}
	var document struct {
		Applications []struct {
			Versions []struct {
				Name string `json:"name"`
			} `json:"versions"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(exported.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(document.Applications) != 1 || len(document.Applications[0].Versions) != 1 || document.Applications[0].Versions[0].Name != "second" {
		t.Fatalf("export with a named version = %s, want only the version named second", exported.Body.String())
	}
}
