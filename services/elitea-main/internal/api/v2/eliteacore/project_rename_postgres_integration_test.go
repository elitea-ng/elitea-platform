package eliteacore_test

// PUT project_info renames a project. Every resolver of a personal project
// (the ensurer, /social/author, the project resolver and the settings-route
// ownership check) finds the row by the exact name `project_user_<owner_id>`.
// A renamed personal project is orphaned, and a team project renamed into
// that namespace can pass for somebody's personal project. These tests run
// the handler against the real migration corpus and read the row back.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

const (
	renameTeamProject     = 6201
	renamePersonalProject = 6202
	renameOwner           = 6299
)

func seedRenameProjects(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES ($1, 'team project', $3, true),
       ($2, 'project_user_' || $3::text, $3, true)
ON CONFLICT (id) DO NOTHING`, renameTeamProject, renamePersonalProject, renameOwner); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
}

func projectName(t *testing.T, pool *pgxpool.Pool, projectID int) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT name FROM centry.project WHERE id = $1`, projectID).Scan(&name); err != nil {
		t.Fatalf("read project %d: %v", projectID, err)
	}
	return name
}

func putProjectInfo(t *testing.T, handler *eliteacore.Handler, projectID int, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Put("/project_info/prompt_lib/{projectID}/project-info", handler.UpdateProjectInfo)
	request := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/project_info/prompt_lib/%d/project-info", projectID), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestUpdateProjectInfoRenamesATeamProject(t *testing.T) {
	pool := newImportCorpusPool(t)
	seedRenameProjects(t, pool)
	handler := eliteacore.NewHandler(pool)

	recorder := putProjectInfo(t, handler, renameTeamProject, `{"name":"renamed team"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := projectName(t, pool, renameTeamProject); got != "renamed team" {
		t.Fatalf("name after rename = %q, want %q", got, "renamed team")
	}
}

func TestUpdateProjectInfoRefusesToRenameAPersonalProject(t *testing.T) {
	pool := newImportCorpusPool(t)
	seedRenameProjects(t, pool)
	handler := eliteacore.NewHandler(pool)
	personalName := fmt.Sprintf("project_user_%d", renameOwner)

	recorder := putProjectInfo(t, handler, renamePersonalProject, `{"name":"x"}`)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "personal_project_rename") {
		t.Fatalf("rename of a personal project = %d %s, want 409 personal_project_rename",
			recorder.Code, recorder.Body.String())
	}
	if got := projectName(t, pool, renamePersonalProject); got != personalName {
		t.Fatalf("the personal project was renamed to %q", got)
	}

	// Sending the current name back is a no-op, not a refusal: a client that
	// PUTs the whole form must still be able to save the icon.
	recorder = putProjectInfo(t, handler, renamePersonalProject, `{"name":"`+personalName+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("same-name PUT on a personal project = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
}

func TestUpdateProjectInfoRefusesTheReservedPrefix(t *testing.T) {
	pool := newImportCorpusPool(t)
	seedRenameProjects(t, pool)
	handler := eliteacore.NewHandler(pool)

	for _, name := range []string{"project_user_1", "Project_User_7", "  project_user_x"} {
		recorder := putProjectInfo(t, handler, renameTeamProject, `{"name":"`+name+`"}`)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "reserved_project_name") {
			t.Fatalf("rename to %q = %d %s, want 400 reserved_project_name", name, recorder.Code, recorder.Body.String())
		}
	}
	if got := projectName(t, pool, renameTeamProject); got != "team project" {
		t.Fatalf("a refused rename changed the name to %q", got)
	}
}

func TestUpdateProjectInfoAnswers404ForAnUnknownProject(t *testing.T) {
	pool := newImportCorpusPool(t)
	handler := eliteacore.NewHandler(pool)

	recorder := putProjectInfo(t, handler, 6299999, `{"name":"anything"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("rename of an unknown project = %d %s, want 404", recorder.Code, recorder.Body.String())
	}
}
