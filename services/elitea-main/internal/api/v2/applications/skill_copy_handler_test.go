package applications_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

type skillCopyCaptureRepo struct {
	mockRepo
	input applications.Version
}

func (r *skillCopyCaptureRepo) CreateVersion(ctx context.Context, project, application string, v applications.Version) (applications.Version, error) {
	r.input = v
	return r.mockRepo.CreateVersion(ctx, project, application, v)
}

func TestCreateVersionForwardsSkillSourceWithoutExposingIt(t *testing.T) {
	repo := &skillCopyCaptureRepo{}
	router := setupRouter(repo)
	router.Post("/api/v2/projects/{projectID}/applications/{applicationID}/versions", handler.NewHandler(repo).CreateVersion)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/projects/1/applications/1/versions",
		strings.NewReader(`{"name":"copy","copy_skills_from_version_id":"42"}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || repo.input.CopySkillsFromVersionID != 42 {
		t.Fatalf("status=%d source=%d body=%s", recorder.Code, repo.input.CopySkillsFromVersionID, recorder.Body)
	}
	wire, err := json.Marshal(repo.input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "copy_skills") || strings.Contains(recorder.Body.String(), "copy_skills") {
		t.Fatal("request-only copy option leaked into the version response or metadata")
	}
}
