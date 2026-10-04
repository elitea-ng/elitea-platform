package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

type recordedRelease struct {
	owner   int32
	section configurationapp.CurrentModelSection
	name    string
}

type fakeModelDefaults struct {
	view     configurationapp.PlatformModelDefaultView
	getErr   error
	setErr   error
	sets     []string
	clears   []configurationapp.CurrentModelSection
	usage    configurationapp.PlatformModelDefaultUsage
	usageErr error
	usages   []recordedRelease
	releases []recordedRelease
	sections []configurationapp.CurrentModelSection
}

func (f *fakeModelDefaults) Get(_ context.Context, section configurationapp.CurrentModelSection) (configurationapp.PlatformModelDefaultView, error) {
	f.sections = append(f.sections, section)
	return f.view, f.getErr
}

func (f *fakeModelDefaults) Set(_ context.Context, _ configurationapp.CurrentModelSection, name string) error {
	f.sets = append(f.sets, name)
	return f.setErr
}

func (f *fakeModelDefaults) Clear(_ context.Context, section configurationapp.CurrentModelSection) error {
	f.clears = append(f.clears, section)
	return nil
}

func (f *fakeModelDefaults) Usage(
	_ context.Context, owner int32, section configurationapp.CurrentModelSection, name string,
) (configurationapp.PlatformModelDefaultUsage, error) {
	f.usages = append(f.usages, recordedRelease{owner, section, name})
	return f.usage, f.usageErr
}

func (f *fakeModelDefaults) ReleaseDeletedModelDefault(
	_ context.Context, owner int32, section configurationapp.CurrentModelSection, name string,
) error {
	f.releases = append(f.releases, recordedRelease{owner, section, name})
	return nil
}

func platformDefaultRouter(handler *Handler) chi.Router {
	r := chi.NewRouter()
	r.Route("/gateway", func(r chi.Router) {
		r.Mount("/default_model", handler.PlatformDefaultModelRoutes())
		r.Mount("/platform_models", handler.GlobalModelRoutes())
	})
	return r
}

func servePlatformDefault(t *testing.T, router chi.Router, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestPlatformDefaultModelRoutesAnswer503WithoutAService(t *testing.T) {
	router := platformDefaultRouter(NewHandler(nil, WithPublicProjectID(1)))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		recorder := servePlatformDefault(t, router, method, "/gateway/default_model/", `{"model_name":"x"}`)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without a service = %d, want 503", method, recorder.Code)
		}
	}
	recorder := servePlatformDefault(t, router, http.MethodGet, "/gateway/platform_models/5/default_usage", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("default_usage without a service = %d, want 503", recorder.Code)
	}
}

func TestGetPlatformDefaultModelReportsTheView(t *testing.T) {
	projectID := int32(1)
	manager := &fakeModelDefaults{view: configurationapp.PlatformModelDefaultView{
		Section: "llm", ModelName: "gpt-all", ModelProjectID: &projectID, Available: true,
		Candidates: []configurationapp.PlatformModelDefaultCandidate{{Name: "gpt-all", DisplayName: "GPT"}},
	}}
	router := platformDefaultRouter(NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager)))

	recorder := servePlatformDefault(t, router, http.MethodGet, "/gateway/default_model/", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["model_name"] != "gpt-all" || body["model_project_id"] != float64(1) || body["available"] != true {
		t.Fatalf("body = %v", body)
	}
	if len(manager.sections) != 1 || manager.sections[0] != configurationapp.CurrentModelSectionLLM {
		t.Fatalf("an omitted section must read llm: %v", manager.sections)
	}
}

func TestPlatformDefaultModelRejectsAnUnknownSection(t *testing.T) {
	manager := &fakeModelDefaults{}
	router := platformDefaultRouter(NewHandler(nil, WithModelDefaults(manager)))
	recorder := servePlatformDefault(t, router, http.MethodGet, "/gateway/default_model/?section=bogus", "")
	if recorder.Code != http.StatusBadRequest || len(manager.sections) != 0 {
		t.Fatalf("unknown section = %d calls=%v", recorder.Code, manager.sections)
	}
}

func TestSetPlatformDefaultModelWritesAndAnswersTheNewView(t *testing.T) {
	manager := &fakeModelDefaults{view: configurationapp.PlatformModelDefaultView{Section: "llm", ModelName: "gpt-all", Available: true}}
	router := platformDefaultRouter(NewHandler(nil, WithModelDefaults(manager)))

	recorder := servePlatformDefault(t, router, http.MethodPut, "/gateway/default_model/", `{"model_name":" gpt-all "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(manager.sets) != 1 || manager.sets[0] != "gpt-all" {
		t.Fatalf("sets = %v", manager.sets)
	}
	if !strings.Contains(recorder.Body.String(), `"model_name":"gpt-all"`) {
		t.Fatalf("PUT did not answer the stored view: %s", recorder.Body.String())
	}
}

func TestSetPlatformDefaultModelRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		setErr error
		want   int
	}{
		{name: "missing name", body: `{}`, want: http.StatusBadRequest},
		{name: "blank name", body: `{"model_name":"  "}`, want: http.StatusBadRequest},
		{name: "not a string", body: `{"model_name":3}`, want: http.StatusBadRequest},
		{name: "ineligible", body: `{"model_name":"narrow"}`, setErr: configurationapp.ErrPlatformModelDefaultIneligible, want: http.StatusBadRequest},
		{name: "storage failure", body: `{"model_name":"gpt-all"}`, setErr: errors.New("vault down"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &fakeModelDefaults{setErr: tc.setErr}
			router := platformDefaultRouter(NewHandler(nil, WithModelDefaults(manager)))
			recorder := servePlatformDefault(t, router, http.MethodPut, "/gateway/default_model/", tc.body)
			if recorder.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d; body=%s", tc.body, recorder.Code, tc.want, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "vault down") {
				t.Fatalf("a raw storage error crossed the boundary: %s", recorder.Body.String())
			}
		})
	}
}

func TestClearPlatformDefaultModel(t *testing.T) {
	manager := &fakeModelDefaults{}
	router := platformDefaultRouter(NewHandler(nil, WithModelDefaults(manager)))
	recorder := servePlatformDefault(t, router, http.MethodDelete, "/gateway/default_model/?section=embedding", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", recorder.Code)
	}
	if len(manager.clears) != 1 || manager.clears[0] != configurationapp.CurrentModelSectionEmbedding {
		t.Fatalf("clears = %v", manager.clears)
	}
}

func TestGlobalModelDefaultUsageCountsThePlatformModel(t *testing.T) {
	manager := &fakeModelDefaults{usage: configurationapp.PlatformModelDefaultUsage{
		ModelName: "gpt-all", PlatformDefault: true, Projects: 3,
	}}
	handler := NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager))
	handler.modelRowLookup = func(_ context.Context, projectID int, configID string) (deletedModelRow, bool, error) {
		if projectID != 1 {
			t.Fatalf("the usage read project %d, want the public project", projectID)
		}
		switch configID {
		case "5":
			return deletedModelRowFrom("llm", "GPT all", "gpt-all", true), true, nil
		case "6":
			return deletedModelRowFrom("llm", "own", "own", false), true, nil
		default:
			return deletedModelRow{}, false, nil
		}
	}
	router := platformDefaultRouter(handler)

	recorder := servePlatformDefault(t, router, http.MethodGet, "/gateway/platform_models/5/default_usage", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body configurationapp.PlatformModelDefaultUsage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body != manager.usage {
		t.Fatalf("usage body = %+v", body)
	}
	if len(manager.usages) != 1 || manager.usages[0] != (recordedRelease{1, configurationapp.CurrentModelSectionLLM, "gpt-all"}) {
		t.Fatalf("usage request = %+v; it must count the provider model name, not the title", manager.usages)
	}

	for _, configID := range []string{"6", "99"} {
		recorder = servePlatformDefault(t, router, http.MethodGet, "/gateway/platform_models/"+configID+"/default_usage", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("usage of %s = %d, want 404 (not a platform row)", configID, recorder.Code)
		}
	}
}

func TestReleaseDeletedModelDefaultCallsTheServiceForModelRows(t *testing.T) {
	manager := &fakeModelDefaults{}
	handler := NewHandler(nil, WithModelDefaults(manager))

	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom("llm", "Title", "gpt-own", false))
	handler.releaseDeletedModelDefault(context.Background(), "1", deletedModelRowFrom("vectorstorage", "pgvector", "", true))
	// Not a model section, no name, or a malformed project: nothing to release.
	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom("ai_credentials", "openai", "", false))
	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom("llm", "Title", "", false))
	handler.releaseDeletedModelDefault(context.Background(), "x", deletedModelRowFrom("llm", "Title", "gpt", false))

	want := []recordedRelease{
		{7, configurationapp.CurrentModelSectionLLM, "gpt-own"},
		{1, configurationapp.CurrentModelSectionVectorStorage, "pgvector"},
	}
	if len(manager.releases) != len(want) {
		t.Fatalf("releases = %+v, want %+v", manager.releases, want)
	}
	for index := range want {
		if manager.releases[index] != want[index] {
			t.Fatalf("release %d = %+v, want %+v", index, manager.releases[index], want[index])
		}
	}

	// Without a service the hook is a no-op rather than a panic.
	NewHandler(nil).releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom("llm", "T", "gpt", false))
}
