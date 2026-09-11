package drafts_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	draftsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
)

type draftVersionReader struct {
	project, skill, version string
	calls                   int
	err                     error
}

func (r *draftVersionReader) GetVersion(_ context.Context, project, skill, version string) (skillsapi.Skill, error) {
	r.project, r.skill, r.version = project, skill, version
	r.calls++
	return skillsapi.Skill{Name: "stored-name", Description: "Stored description", Instructions: "Selected version rules", Tags: []string{"selected"}}, r.err
}

func TestSkillEditUsesScopedSelectedVersionAndSourceContract(t *testing.T) {
	reader := &draftVersionReader{}
	completer := &stubCompleter{content: routes[0].modelAnswer}
	h := draftsapi.NewHandler(completer, draftsapi.WithSkillVersions(reader), draftsapi.WithPermissions(draftPermissions{}))
	mux := chi.NewRouter()
	mux.Use(draftActor)
	mux.Post("/draft/{projectID}", h.GenerateSkillDraft)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/draft/7", strings.NewReader(`{"user_description":"Improve clarity", "skill_id":3,"version_id":"12"}`)))
	if response.Code != http.StatusOK || reader.project != "7" || reader.skill != "3" || reader.version != "12" {
		t.Fatalf("response=%d reader=%+v", response.Code, reader)
	}
	prompt := completer.got.Messages[0].Content
	for _, expected := range []string{"stored-name", "Stored description", "Selected version rules", "selected", "complete draft", "5000", "2304"} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("prompt lacks %q", expected)
		}
	}
	if completer.got.Messages[1].Content != "Improve clarity" {
		t.Fatalf("user request changed")
	}
}

func TestSkillEditValidationAndSafeReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name, ids     string
		err           error
		status, reads int
	}{
		{"half pair", `"skill_id":3`, nil, 400, 0},
		{"zero", `"skill_id":0,"version_id":2`, nil, 400, 0},
		{"fraction", `"skill_id":3.5,"version_id":2`, nil, 400, 0},
		{"bool", `"skill_id":true,"version_id":2`, nil, 400, 0},
		{"overflow", `"skill_id":2147483648,"version_id":2`, nil, 400, 0},
		{"missing", `"skill_id":3,"version_id":2`, apierr.NotFound("private stored name"), 404, 1},
		{"database", `"skill_id":3,"version_id":2`, errors.New("password=private"), 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &draftVersionReader{err: tc.err}
			completer := &stubCompleter{}
			h := draftsapi.NewHandler(completer, draftsapi.WithSkillVersions(reader), draftsapi.WithPermissions(draftPermissions{}))
			mux := chi.NewRouter()
			mux.Use(draftActor)
			mux.Post("/draft/{projectID}", h.GenerateSkillDraft)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/draft/7", strings.NewReader(`{"user_description":"Improve",`+tc.ids+`}`)))
			if response.Code != tc.status || reader.calls != tc.reads || completer.called != 0 {
				t.Fatalf("status=%d reads=%d completions=%d", response.Code, reader.calls, completer.called)
			}
			if strings.Contains(response.Body.String(), "private") {
				t.Fatal("internal error disclosed")
			}
		})
	}
}

type draftPermissions struct{ denied bool }

func (p draftPermissions) ResolvePermissions(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
	permissions := []string{"models.applications.skills.details"}
	if p.denied {
		permissions = nil
	}
	return auth.PermissionResolution{UserID: 1, Permissions: permissions}, nil
}
func draftActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1", UserID: "1"})))
	})
}
func TestSkillEditRefusesCreateOnlyCallerBeforeStoredRead(t *testing.T) {
	reader := &draftVersionReader{}
	completer := &stubCompleter{}
	h := draftsapi.NewHandler(completer, draftsapi.WithSkillVersions(reader), draftsapi.WithPermissions(draftPermissions{denied: true}))
	mux := chi.NewRouter()
	mux.Use(draftActor)
	mux.Post("/draft/{projectID}", h.GenerateSkillDraft)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/draft/7", strings.NewReader(`{"user_description":"Show rules","skill_id":3,"version_id":12}`)))
	if response.Code != 403 || reader.calls != 0 || completer.called != 0 {
		t.Fatalf("status=%d reads=%d completions=%d", response.Code, reader.calls, completer.called)
	}
}
