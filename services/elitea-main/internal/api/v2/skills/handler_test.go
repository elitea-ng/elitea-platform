package skills_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// mockSkillRepo implements skills.Repository.
type mockSkillRepo struct {
	skills []handler.Skill
	err    error

	// lastListParams records the ListParams passed to the most recent List
	// call so tests can assert query/sort_by/sort_order are actually wired
	// through from the HTTP layer.
	lastListParams handler.ListParams

	// attachCalls/detachCalls record the relation writes, and updateCalls
	// records the plain skill updates, so a test can prove which of the two
	// operations the overloaded PATCH selected (#38).
	attachCalls []attachCall
	detachCalls []attachCall
	updateCalls int
	// relationErr is the error AttachSkill and DetachSkill return.
	relationErr error

	// #874 version-machinery call recorders, so a test can assert what the
	// repository was asked to do without a real database.
	createVersionCalls int
	updateVersionCalls int
	deleteVersionCalls []string
	restoreCalls       []string
	setDefaultCalls    []string
}

func (m *mockSkillRepo) List(_ context.Context, _ string, params handler.ListParams) (handler.ListResponse, error) {
	m.lastListParams = params
	if m.err != nil {
		return handler.ListResponse{}, m.err
	}
	total := len(m.skills)
	totalPages := 1
	if params.PageSize > 0 && total > 0 {
		totalPages = (total + params.PageSize - 1) / params.PageSize
	}
	return handler.ListResponse{
		Items:      m.skills,
		Total:      total,
		Page:       params.Page,
		PageSize:   params.PageSize,
		TotalPages: totalPages,
	}, nil
}

func (m *mockSkillRepo) Get(_ context.Context, _, skillID string) (handler.Skill, error) {
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	for _, s := range m.skills {
		if s.ID == skillID {
			return s, nil
		}
	}
	return handler.Skill{}, apierr.NotFound("skill not found")
}

func (m *mockSkillRepo) GetByName(_ context.Context, _, name string) (handler.Skill, bool, error) {
	if m.err != nil {
		return handler.Skill{}, false, m.err
	}
	for _, s := range m.skills {
		if s.Name == name {
			return s, true, nil
		}
	}
	return handler.Skill{}, false, nil
}

func (m *mockSkillRepo) Create(_ context.Context, projectID string, skill handler.Skill) (handler.Skill, error) {
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	skill.ID = "new-skill-id"
	skill.ProjectID = projectID
	skill.CreatedAt = time.Now()
	skill.UpdatedAt = time.Now()
	m.skills = append(m.skills, skill)
	return skill, nil
}

func (m *mockSkillRepo) Update(_ context.Context, projectID, skillID string, skill handler.Skill) (handler.Skill, error) {
	m.updateCalls++
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	skill.ID = skillID
	skill.ProjectID = projectID
	skill.UpdatedAt = time.Now()
	return skill, nil
}

func (m *mockSkillRepo) Delete(_ context.Context, _, _ string) error {
	return m.err
}

// ---- #874 version machinery ---------------------------------------------

func (m *mockSkillRepo) GetVersion(_ context.Context, _, skillID, versionID string) (handler.Skill, error) {
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	for _, s := range m.skills {
		if s.ID != skillID {
			continue
		}
		for _, v := range s.Versions {
			if v.ID == versionID {
				out := s
				picked := v
				out.VersionDetails = &picked
				out.Instructions = picked.Instructions
				out.Tags = picked.Tags
				return out, nil
			}
		}
		return handler.Skill{}, apierr.NotFound("skill version not found")
	}
	return handler.Skill{}, apierr.NotFound("skill not found")
}

func (m *mockSkillRepo) CreateVersion(_ context.Context, _, skillID string, input handler.VersionCreateInput) (handler.Skill, error) {
	m.createVersionCalls++
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	for i, s := range m.skills {
		if s.ID != skillID {
			continue
		}
		for _, existing := range s.Versions {
			if existing.Name == input.Name {
				return handler.Skill{}, apierr.Conflict("a version with that name already exists")
			}
		}
		v := handler.SkillVersion{ID: "new-version-id", Name: input.Name, Instructions: input.Instructions, Tags: input.Tags}
		m.skills[i].Versions = append(m.skills[i].Versions, v)
		out := m.skills[i]
		out.VersionDetails = &v
		out.Instructions = v.Instructions
		out.Tags = v.Tags
		return out, nil
	}
	return handler.Skill{}, apierr.NotFound("skill not found")
}

func (m *mockSkillRepo) UpdateVersion(_ context.Context, _, skillID, versionID string, skill handler.Skill) (handler.Skill, error) {
	m.updateVersionCalls++
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	for i, s := range m.skills {
		if s.ID != skillID {
			continue
		}
		m.skills[i].Name = skill.Name
		m.skills[i].Description = skill.Description
		for j, v := range s.Versions {
			if v.ID == versionID {
				m.skills[i].Versions[j].Instructions = skill.Instructions
				m.skills[i].Versions[j].Tags = skill.Tags
				out := m.skills[i]
				picked := m.skills[i].Versions[j]
				out.VersionDetails = &picked
				out.Instructions = picked.Instructions
				out.Tags = picked.Tags
				return out, nil
			}
		}
		return handler.Skill{}, apierr.NotFound("skill version not found")
	}
	return handler.Skill{}, apierr.NotFound("skill not found")
}

func (m *mockSkillRepo) DeleteVersion(_ context.Context, _, skillID, versionID string) error {
	m.deleteVersionCalls = append(m.deleteVersionCalls, versionID)
	if m.err != nil {
		return m.err
	}
	for i, s := range m.skills {
		if s.ID != skillID {
			continue
		}
		for j, v := range s.Versions {
			if v.ID == versionID {
				if v.Name == "base" {
					return apierr.BadRequest(`cannot delete the "base" version`)
				}
				m.skills[i].Versions = append(s.Versions[:j:j], s.Versions[j+1:]...)
				return nil
			}
		}
		return apierr.NotFound("skill version not found")
	}
	return apierr.NotFound("skill not found")
}

func (m *mockSkillRepo) RestoreVersion(_ context.Context, _, skillID, versionID string) (handler.Skill, error) {
	m.restoreCalls = append(m.restoreCalls, versionID)
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	for i, s := range m.skills {
		if s.ID != skillID {
			continue
		}
		var source *handler.SkillVersion
		for _, v := range s.Versions {
			if v.ID == versionID {
				picked := v
				source = &picked
			}
		}
		if source == nil {
			return handler.Skill{}, apierr.NotFound("skill version not found")
		}
		for j, v := range m.skills[i].Versions {
			if v.Name == "base" {
				m.skills[i].Versions[j].Instructions = source.Instructions
				m.skills[i].Versions[j].Tags = source.Tags
				out := m.skills[i]
				picked := m.skills[i].Versions[j]
				out.VersionDetails = &picked
				out.Instructions = picked.Instructions
				out.Tags = picked.Tags
				return out, nil
			}
		}
		return handler.Skill{}, apierr.NotFound("base version not found")
	}
	return handler.Skill{}, apierr.NotFound("skill not found")
}

func (m *mockSkillRepo) SetDefaultVersion(_ context.Context, _, skillID, versionID string) (handler.Skill, error) {
	m.setDefaultCalls = append(m.setDefaultCalls, versionID)
	if m.err != nil {
		return handler.Skill{}, m.err
	}
	for i, s := range m.skills {
		if s.ID != skillID {
			continue
		}
		m.skills[i].DefaultVersionID = versionID
		return m.skills[i], nil
	}
	return handler.Skill{}, apierr.NotFound("skill not found")
}

// attachCall records one AttachSkill or DetachSkill call. The relation form of
// PATCH used to decode into `createRequest`, which names none of the four
// relation keys, so it reached the repository as a name/description update and
// answered 200. A test that reads only the status code cannot see that. These
// fields let a test assert what the repository was asked to do.
type attachCall struct {
	projectID string
	skillID   string
	relation  handler.SkillRelation
}

func (m *mockSkillRepo) AttachSkill(
	_ context.Context,
	projectID, skillID string,
	relation handler.SkillRelation,
) (handler.SkillAttachment, error) {
	m.attachCalls = append(m.attachCalls, attachCall{projectID, skillID, relation})
	if m.relationErr != nil {
		return handler.SkillAttachment{}, m.relationErr
	}
	return handler.SkillAttachment{
		SkillID:     1,
		SkillName:   "Reviewer",
		VersionName: "base",
	}, nil
}

func (m *mockSkillRepo) DetachSkill(
	_ context.Context,
	projectID, skillID string,
	relation handler.SkillRelation,
) error {
	m.detachCalls = append(m.detachCalls, attachCall{projectID, skillID, relation})
	return m.relationErr
}

func setupSkillsRouter(repo handler.Repository) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "41", UserID: "41"})))
		})
	})
	h := handler.NewHandler(repo)
	r.Route("/api/v2/projects/{projectID}/skills", func(r chi.Router) {
		r.Mount("/", h.Routes())
		r.Post("/import", h.Import)
		r.Get("/export/{skillID}", h.Export)
	})
	return r
}

func multipartFileBody(t *testing.T, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf, w.FormDataContentType()
}

// ---- List -------------------------------------------------------------------

func TestSkillList_Success(t *testing.T) {
	repo := &mockSkillRepo{
		skills: []handler.Skill{
			{ID: "s-1", ProjectID: "proj-1", Name: "Skill A", Type: "tool"},
			{ID: "s-2", ProjectID: "proj-1", Name: "Skill B", Type: "tool"},
		},
	}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/?page=1&page_size=20", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp handler.ListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("expected 2 skills, got %d", len(resp.Items))
	}
	if resp.Total != 2 {
		t.Errorf("expected total=2, got %d", resp.Total)
	}
}

func TestSkillList_DefaultPagination(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{}}
	r := setupSkillsRouter(repo)

	// No page/page_size query params → defaults applied in handler (page=1, pageSize=20)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp handler.ListResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Page != 1 {
		t.Errorf("expected default page=1, got %d", resp.Page)
	}
	if resp.PageSize != 20 {
		t.Errorf("expected default page_size=20, got %d", resp.PageSize)
	}
}

func TestSkillList_Error(t *testing.T) {
	repo := &mockSkillRepo{err: errors.New("db failure")}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Get --------------------------------------------------------------------

func TestSkillGet_Success(t *testing.T) {
	repo := &mockSkillRepo{
		skills: []handler.Skill{
			{ID: "s-1", ProjectID: "proj-1", Name: "Skill A", Type: "tool"},
		},
	}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/s-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var skill handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&skill); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if skill.ID != "s-1" {
		t.Errorf("expected ID 's-1', got %q", skill.ID)
	}
}

func TestSkillGet_NotFound(t *testing.T) {
	repo := &mockSkillRepo{skills: nil}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/nonexistent", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillGet_Error(t *testing.T) {
	repo := &mockSkillRepo{err: apierr.Internal("db error")}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/s-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Create -----------------------------------------------------------------

func TestSkillCreate_Success(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	payload, _ := json.Marshal(handler.Skill{Name: "New Skill", Type: "tool"})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var skill handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&skill); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if skill.ID != "new-skill-id" {
		t.Errorf("expected ID 'new-skill-id', got %q", skill.ID)
	}
	if skill.Name != "New Skill" {
		t.Errorf("expected Name 'New Skill', got %q", skill.Name)
	}
}

func TestSkillCreate_InvalidBody(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillCreate_RepoError(t *testing.T) {
	repo := &mockSkillRepo{err: apierr.Internal("create failed")}
	r := setupSkillsRouter(repo)

	payload, _ := json.Marshal(handler.Skill{Name: "X", Type: "tool"})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Update -----------------------------------------------------------------

func TestSkillUpdate_Success(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	payload, _ := json.Marshal(handler.Skill{Name: "Updated Skill", Type: "tool"})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/skills/s-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var skill handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&skill); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if skill.ID != "s-1" {
		t.Errorf("expected ID 's-1', got %q", skill.ID)
	}
	if skill.Name != "Updated Skill" {
		t.Errorf("expected updated name, got %q", skill.Name)
	}
}

func TestSkillUpdate_NotFound(t *testing.T) {
	repo := &mockSkillRepo{err: apierr.NotFound("skill not found")}
	r := setupSkillsRouter(repo)

	payload, _ := json.Marshal(handler.Skill{Name: "X", Type: "tool"})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/skills/missing", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillUpdate_InvalidBody(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/skills/s-1", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Delete -----------------------------------------------------------------

func TestSkillDelete_Success(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/skills/s-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillDelete_NotFound(t *testing.T) {
	repo := &mockSkillRepo{err: apierr.NotFound("skill not found")}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/skills/missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillDelete_Error(t *testing.T) {
	repo := &mockSkillRepo{err: apierr.Internal("db error")}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/skills/s-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Content-Type -----------------------------------------------------------

func TestSkillHandlers_ContentTypeJSON(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		t.Error("expected Content-Type header to be set")
	}
}

// ---- Search / sort passthrough -----------------------------------------------

func TestSkillList_QueryAndSortPassthrough(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/?query=%20foo%20&sort_by=name&sort_order=asc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.lastListParams.Query != "foo" {
		t.Errorf("expected query 'foo' (trimmed), got %q", repo.lastListParams.Query)
	}
	if repo.lastListParams.SortBy != "name" {
		t.Errorf("expected sort_by 'name', got %q", repo.lastListParams.SortBy)
	}
	if repo.lastListParams.SortOrder != "asc" {
		t.Errorf("expected sort_order 'asc', got %q", repo.lastListParams.SortOrder)
	}
}

// ---- Instructions / tags on Create & Update ----------------------------------

func TestSkillCreate_VersionsShapePopulatesInstructionsAndTags(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	// Matches skillsApi.ts createSkill(): {name, description, versions: [{name, instructions, tags}]}.
	payload, _ := json.Marshal(map[string]any{
		"name":        "New Skill",
		"description": "desc",
		"versions": []map[string]any{
			{"name": "base", "instructions": "Be helpful", "tags": []string{"quality", "support"}},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var skill handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&skill); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if skill.Instructions != "Be helpful" {
		t.Errorf("expected instructions from versions[0], got %q", skill.Instructions)
	}
	if len(skill.Tags) != 2 || skill.Tags[0] != "quality" || skill.Tags[1] != "support" {
		t.Errorf("expected tags from versions[0], got %v", skill.Tags)
	}
}

func TestSkillUpdate_FlatShapePopulatesInstructionsAndTags(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	// Matches skillsApi.ts updateSkill(): {name, description, instructions, tags} flat.
	payload, _ := json.Marshal(map[string]any{
		"name":         "Updated Skill",
		"description":  "desc",
		"instructions": "Stay on topic",
		"tags":         []string{"focus"},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/skills/s-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var skill handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&skill); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if skill.Instructions != "Stay on topic" {
		t.Errorf("expected instructions 'Stay on topic', got %q", skill.Instructions)
	}
	if len(skill.Tags) != 1 || skill.Tags[0] != "focus" {
		t.Errorf("expected tags ['focus'], got %v", skill.Tags)
	}
}

// ---- Import -------------------------------------------------------------------

const validSkillMD = "---\nname: Code Reviewer\ndescription: Reviews code for bugs\ntags:\n  - quality\n  - review\n---\nAlways check for security issues."

func TestSkillImport_MultipartSuccess(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	body, contentType := multipartFileBody(t, "reviewer.md", validSkillMD)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/import", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var skill handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&skill); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if skill.Name != "Code Reviewer" || skill.Description != "Reviews code for bugs" {
		t.Errorf("unexpected skill: %+v", skill)
	}
	if skill.Instructions != "Always check for security issues." {
		t.Errorf("unexpected instructions: %q", skill.Instructions)
	}
	if len(skill.Tags) != 2 {
		t.Errorf("expected 2 tags, got %v", skill.Tags)
	}
}

func TestSkillImport_JSONBodySuccess(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	payload, _ := json.Marshal(map[string]string{"content": validSkillMD, "filename": "reviewer.md"})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillImport_WrongExtension(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	body, contentType := multipartFileBody(t, "reviewer.txt", validSkillMD)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/import", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillImport_MissingFrontmatter(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	body, contentType := multipartFileBody(t, "reviewer.md", "just some text with no frontmatter")
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/import", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillImport_DuplicateNameReusesAndNotices(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{
		{ID: "existing-1", Name: "Code Reviewer", Description: "old desc"},
	}}
	r := setupSkillsRouter(repo)

	body, contentType := multipartFileBody(t, "reviewer.md", validSkillMD)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/import", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (reused, not created), got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID     string `json:"id"`
		Notice string `json:"notice"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID != "existing-1" {
		t.Errorf("expected the existing skill to be reused, got id %q", resp.ID)
	}
	if resp.Notice == "" {
		t.Error("expected a notice explaining the skill was reused")
	}
}

// ---- Export -------------------------------------------------------------------

func TestSkillExport_Success(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{
		{
			ID: "s-1", Name: "Code Reviewer", Description: "Reviews code for bugs",
			Instructions: "Always check for security issues.", Tags: []string{"quality", "review"},
		},
	}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/export/s-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown" {
		t.Errorf("expected text/markdown content type, got %q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if cd == "" || !bytes.Contains([]byte(cd), []byte("Code Reviewer.md")) {
		t.Errorf("expected Content-Disposition with a Code Reviewer.md filename, got %q", cd)
	}

	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte("name: Code Reviewer")) {
		t.Errorf("expected frontmatter with skill name, got: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte("Always check for security issues.")) {
		t.Errorf("expected instructions in body, got: %s", body)
	}
}

func TestSkillExport_NotFound(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/export/missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// NOTE(#395): three ListForApplication tests stood here —
// _ReturnsOnlyThatVersionsSkills, _RefusesAMalformedVersionID and
// _FailsLoudlyOnRepositoryError. They covered the PROTOTYPE fallback for
// GET /application_skills/{mode}/{projectID}/{appVersionID}, which #395
// deleted.
//
// internal/api/v2/applicationskills answers that path now, and its own suite
// keeps all three guarantees: TestCurrentApplicationSkillsRoutePostgresContractAndTenantIsolation
// proves the answer is scoped to ONE agent version against a real database,
// and its handler tests cover a malformed id and a repository failure.

// ---- Relation PATCH (#38) ----------------------------------------------------

// setupSkillRelationRouter mirrors the registration internal/api/router.go
// makes: PUT and PATCH on ONE path, both on Update. The overload lives in the
// body, so a test router that mounts only PATCH cannot show that the plain
// update still works on the same URL.
func setupSkillRelationRouter(repo handler.Repository) *chi.Mux {
	r := chi.NewRouter()
	h := handler.NewHandler(repo)
	r.Put("/elitea_core/skill/{mode}/{projectID}/{skillID}", h.Update)
	r.Patch("/elitea_core/skill/{mode}/{projectID}/{skillID}", h.Update)
	return r
}

func patchSkill(t *testing.T, r *chi.Mux, skillID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPatch, "/elitea_core/skill/prompt_lib/7/"+skillID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestSkillRelation_AttachReachesTheRepository is the acceptance test for the
// attach half of #38.
//
// The assertion is on WHAT THE REPOSITORY WAS ASKED FOR, never on the status
// code. Before this change the same request answered 200 and reached
// repo.Update with an empty name — a status code cannot tell the two apart.
func TestSkillRelation_AttachReachesTheRepository(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillRelationRouter(repo)

	rec := patchSkill(t, r, "11", `{
		"has_relation": true,
		"entity_version_id": 42,
		"skill_version_id": 5,
		"entity_type": "agent"
	}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("attach status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if repo.updateCalls != 0 {
		t.Errorf("a relation body reached repo.Update %d times; it must reach AttachSkill only", repo.updateCalls)
	}
	if len(repo.attachCalls) != 1 {
		t.Fatalf("AttachSkill calls = %d, want 1", len(repo.attachCalls))
	}
	got := repo.attachCalls[0]
	want := attachCall{
		projectID: "7",
		skillID:   "11",
		relation: handler.SkillRelation{
			EntityVersionID: "42",
			EntityType:      "agent",
			SkillVersionID:  "5",
		},
	}
	if got != want {
		t.Errorf("AttachSkill called with %+v, want %+v", got, want)
	}

	// The body is pylon's four-key attachment, not an echo of the request.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode attach body: %v", err)
	}
	for _, key := range []string{"skill_id", "skill_version_id", "skill_name", "version_name"} {
		if _, ok := body[key]; !ok {
			t.Errorf("attach body is missing %q: %s", key, rec.Body.String())
		}
	}
}

// TestSkillRelation_DetachReachesTheRepository is the detach half.
//
// `skill_version_id` is absent, exactly as the old app's useDetachSkill hook
// sends it. It is not part of the mapping key, so a handler that demanded it
// would refuse every real detach.
func TestSkillRelation_DetachReachesTheRepository(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillRelationRouter(repo)

	rec := patchSkill(t, r, "11", `{"has_relation": false, "entity_version_id": 42}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("detach status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if repo.updateCalls != 0 {
		t.Errorf("a relation body reached repo.Update %d times", repo.updateCalls)
	}
	if len(repo.detachCalls) != 1 {
		t.Fatalf("DetachSkill calls = %d, want 1", len(repo.detachCalls))
	}
	got := repo.detachCalls[0]
	want := attachCall{
		projectID: "7",
		skillID:   "11",
		relation:  handler.SkillRelation{EntityVersionID: "42", EntityType: "agent"},
	}
	if got != want {
		t.Errorf("DetachSkill called with %+v, want %+v", got, want)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Errorf("detach body = %s, want {\"ok\":true}", rec.Body.String())
	}
}

// TestSkillRelation_StringIdsAreAccepted keeps the JSON-string form working.
// The old app reads these ids out of the redux store, where a version id can be
// a string.
func TestSkillRelation_StringIdsAreAccepted(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillRelationRouter(repo)

	rec := patchSkill(t, r, "11", `{
		"has_relation": true, "entity_version_id": "42", "skill_version_id": "5"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("attach status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if len(repo.attachCalls) != 1 || repo.attachCalls[0].relation.EntityVersionID != "42" {
		t.Fatalf("AttachSkill calls = %+v", repo.attachCalls)
	}
	// Absent entity_type defaults to "agent"; both readers of the row filter
	// on that literal, so a blank one writes a row nothing reads.
	if repo.attachCalls[0].relation.EntityType != "agent" {
		t.Errorf("entity_type = %q, want \"agent\"", repo.attachCalls[0].relation.EntityType)
	}
}

// TestSkillRelation_PlainUpdateStillWorks proves the overload did not take the
// URL away from the operation that already used it.
func TestSkillRelation_PlainUpdateStillWorks(t *testing.T) {
	repo := &mockSkillRepo{}
	r := setupSkillRelationRouter(repo)

	rec := patchSkill(t, r, "11", `{"name": "Renamed", "description": "d", "instructions": "i"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if repo.updateCalls != 1 {
		t.Errorf("repo.Update calls = %d, want 1", repo.updateCalls)
	}
	if len(repo.attachCalls)+len(repo.detachCalls) != 0 {
		t.Errorf("a plain update reached the relation path")
	}

	var updated handler.Skill
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update body: %v", err)
	}
	if updated.Name != "Renamed" {
		t.Errorf("updated name = %q, want %q", updated.Name, "Renamed")
	}
}

// TestSkillRelation_RefusesMalformedRequests covers every input the handler
// refuses before it reaches the repository. Each case asserts that NOTHING was
// written, because a 400 over a completed write is the failure this repository
// keeps finding.
func TestSkillRelation_RefusesMalformedRequests(t *testing.T) {
	cases := []struct {
		name    string
		skillID string
		body    string
	}{
		{"has_relation is a string", "11", `{"has_relation": "true", "entity_version_id": 42}`},
		{"has_relation is null", "11", `{"has_relation": null, "entity_version_id": 42}`},
		{"entity_version_id missing", "11", `{"has_relation": true, "skill_version_id": 5}`},
		{"entity_version_id is zero", "11", `{"has_relation": true, "entity_version_id": 0, "skill_version_id": 5}`},
		{"entity_version_id is a word", "11", `{"has_relation": false, "entity_version_id": "abc"}`},
		{"entity_version_id overflows int32", "11", `{"has_relation": false, "entity_version_id": 4294967296}`},
		{"skill_version_id missing on attach", "11", `{"has_relation": true, "entity_version_id": 42}`},
		{"skill_version_id is null on attach", "11", `{"has_relation": true, "entity_version_id": 42, "skill_version_id": null}`},
		{"entity_type is not agent", "11", `{"has_relation": true, "entity_version_id": 42, "skill_version_id": 5, "entity_type": "pipeline"}`},
		{"skill id is not a number", "abc", `{"has_relation": true, "entity_version_id": 42, "skill_version_id": 5}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockSkillRepo{}
			r := setupSkillRelationRouter(repo)

			rec := patchSkill(t, r, tc.skillID, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			if n := len(repo.attachCalls) + len(repo.detachCalls) + repo.updateCalls; n != 0 {
				t.Errorf("a refused request still reached the repository %d times", n)
			}
		})
	}
}

// TestSkillRelation_CarriesTheRepositoryStatus proves the handler does not
// flatten the repository's refusals into one code. The old app's version
// selector reads the 409 to decide whether to re-attach.
func TestSkillRelation_CarriesTheRepositoryStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"already attached", apierr.Conflict("Skill 11 is already attached to agent version 42"), http.StatusConflict},
		{"unknown skill", apierr.NotFound("Skill with id 11 not found"), http.StatusNotFound},
		{"skill limit", apierr.BadRequest("Agent version 42 already has 5 skills attached."), http.StatusBadRequest},
		{"unknown failure", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockSkillRepo{relationErr: tc.err}
			r := setupSkillRelationRouter(repo)

			rec := patchSkill(t, r, "11", `{
				"has_relation": true, "entity_version_id": 42, "skill_version_id": 5
			}`)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// ---- #874 version machinery (HTTP layer) --------------------------------

func TestSkillCreateVersion_RejectsEmptyAndReservedNames(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{ID: "s-1", Name: "Reviewer"}}}
	r := setupSkillsRouter(repo)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty name", `{"instructions":"i"}`, http.StatusBadRequest},
		{"blank name", `{"name":"   ","instructions":"i"}`, http.StatusBadRequest},
		{"reserved base", `{"name":"base","instructions":"i"}`, http.StatusBadRequest},
		{"valid", `{"name":"v2","instructions":"i"}`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/s-1/versions",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if repo.createVersionCalls != 1 {
		t.Errorf("repo.CreateVersion called %d times, want exactly 1 (only the valid case)", repo.createVersionCalls)
	}
}

func TestSkillCreateVersion_ReachesRepositoryWithTheRightSkillID(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{ID: "s-1", Name: "Reviewer"}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/s-1/versions",
		strings.NewReader(`{"name":"v2","instructions":"new content","tags":["a","b"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var got handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VersionDetails == nil || got.VersionDetails.Name != "v2" || got.VersionDetails.Instructions != "new content" {
		t.Errorf("version_details=%+v", got.VersionDetails)
	}
	// The route used to be bound to Create, which ignores {skillID} — this
	// is the regression #874 fixes: the version must land on THIS skill.
	if len(repo.skills[0].Versions) != 1 || repo.skills[0].Versions[0].Name != "v2" {
		t.Errorf("repo.skills[0].Versions=%+v, want the new version attached to s-1", repo.skills[0].Versions)
	}
	if len(repo.skills) != 1 {
		t.Errorf("repo.skills=%+v, want CreateVersion to add NO new skill row (the pre-#874 bug created one)", repo.skills)
	}
}

func TestSkillGet_VersionScopedRouteReturnsTheNamedVersion(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{
			{ID: "v-base", Name: "base", Instructions: "base content"},
			{ID: "v-2", Name: "v2", Instructions: "v2 content"},
		},
	}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/s-1/v-2", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var got handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VersionDetails == nil || got.VersionDetails.ID != "v-2" || got.Instructions != "v2 content" {
		t.Errorf("got=%+v, want version v-2's content", got)
	}
}

func TestSkillUpdate_VersionScopedRouteEditsTheNamedVersion(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{
			{ID: "v-base", Name: "base", Instructions: "base content"},
			{ID: "v-2", Name: "v2", Instructions: "old v2 content"},
		},
	}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/skills/s-1/v-2",
		strings.NewReader(`{"name":"Reviewer","description":"d","instructions":"new v2 content","tags":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if repo.updateVersionCalls != 1 {
		t.Errorf("repo.UpdateVersion called %d times, want 1", repo.updateVersionCalls)
	}
	if repo.updateCalls != 0 {
		t.Errorf("repo.Update (the unversioned/base path) called %d times, want 0 — a versioned PUT must not touch base", repo.updateCalls)
	}
}

func TestSkillDelete_VersionScopedRouteDeletesOnlyTheNamedVersion(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{
			{ID: "v-base", Name: "base"},
			{ID: "v-2", Name: "v2"},
		},
	}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/skills/s-1/v-2", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if len(repo.deleteVersionCalls) != 1 || repo.deleteVersionCalls[0] != "v-2" {
		t.Errorf("deleteVersionCalls=%v, want [v-2]", repo.deleteVersionCalls)
	}
}

func TestSkillDelete_VersionScopedRouteRefusesBase(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{{ID: "v-base", Name: "base"}},
	}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/skills/s-1/v-base", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (base refused); body: %s", rec.Code, rec.Body.String())
	}
}

// TestSkillGet_UnversionedRouteStillListsEveryVersion is the "no separate
// listVersions" contract Repository's doc comment states: the plain GET
// /skill/{mode}/{projectID}/{skillID} — the one call the version
// selector/compare UI already makes on page load — carries every version in
// `versions[]`, not just `base`. skills.getSkill has no companion
// skills.listVersions entry in endpoints.manifest.json for exactly this
// reason.
func TestSkillGet_UnversionedRouteStillListsEveryVersion(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{
			{ID: "v-base", Name: "base"},
			{ID: "v-2", Name: "v2"},
		},
	}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/skills/s-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var got handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Versions) != 2 {
		t.Fatalf("versions=%+v, want 2", got.Versions)
	}
}

func TestSkillRestoreVersion_CopiesContentOntoBase(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{
			{ID: "v-base", Name: "base", Instructions: "stale"},
			{ID: "v-2", Name: "v2", Instructions: "the good content"},
		},
	}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/s-1/versions/v-2/restore", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var got handler.Skill
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VersionDetails == nil || got.VersionDetails.Name != "base" || got.VersionDetails.Instructions != "the good content" {
		t.Errorf("got=%+v, want base restored to v-2's content", got)
	}
	if len(repo.restoreCalls) != 1 || repo.restoreCalls[0] != "v-2" {
		t.Errorf("restoreCalls=%v, want [v-2]", repo.restoreCalls)
	}
}

func TestSkillSetDefaultVersion_ReadsVersionIDAndDoesNotTouchTheSkillName(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{
		ID: "s-1", Name: "Reviewer",
		Versions: []handler.SkillVersion{{ID: "2", Name: "v2"}},
	}}}
	r := setupSkillsRouter(repo)

	// Before #874 this exact body (what setDefaultSkillVersion sends) reached
	// the generic Update handler, which read no "version_id" key and wrote
	// the skill's own name to "". version_id is a row id (relationID/rowID),
	// so it must be numeric — the same leniency AttachSkill's tests use.
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/proj-1/skills/s-1/default_version",
		strings.NewReader(`{"version_id":"2"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setDefaultCalls) != 1 || repo.setDefaultCalls[0] != "2" {
		t.Errorf("setDefaultCalls=%v, want [2]", repo.setDefaultCalls)
	}
	if repo.skills[0].Name != "Reviewer" {
		t.Errorf("skill name=%q, want unchanged %q (regression: used to be wiped to empty)", repo.skills[0].Name, "Reviewer")
	}
}

func TestSkillSetDefaultVersion_RequiresVersionID(t *testing.T) {
	repo := &mockSkillRepo{skills: []handler.Skill{{ID: "s-1", Name: "Reviewer"}}}
	r := setupSkillsRouter(repo)

	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/proj-1/skills/s-1/default_version",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setDefaultCalls) != 0 {
		t.Errorf("setDefaultCalls=%v, want none — repository must not be reached with no version_id", repo.setDefaultCalls)
	}
}

func TestSkillCreateUsesAuthenticatedAuthorAndRejectsMissingPrincipal(t *testing.T) {
	repo := &mockSkillRepo{}
	router := setupSkillsRouter(repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/skills/", strings.NewReader(`{"name":"reviewer","description":"Review rules","author_id":999}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusCreated || len(repo.skills) != 1 || repo.skills[0].AuthorID != 41 {
		t.Fatalf("create status=%d skills=%+v", response.Code, repo.skills)
	}
	anonymous := httptest.NewRecorder()
	handler.NewHandler(repo).Create(anonymous, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"anonymous"}`)))
	if anonymous.Code != http.StatusUnauthorized || len(repo.skills) != 1 {
		t.Fatalf("anonymous status=%d skills=%+v", anonymous.Code, repo.skills)
	}
}
