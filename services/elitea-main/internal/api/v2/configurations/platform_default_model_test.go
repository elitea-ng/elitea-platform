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

func recordedFrom(row configurationapp.PlatformModelDefaultModelRow) recordedRelease {
	return recordedRelease{row.OwnerProjectID, row.Section, row.Name}
}

type fakeModelDefaults struct {
	view     configurationapp.PlatformModelDefaultView
	getErr   error
	setErr   error
	sets     []string
	clears   []configurationapp.CurrentModelSection
	usage    configurationapp.PlatformModelDefaultUsage
	usageErr error
	usages   []configurationapp.PlatformModelDefaultModelRow
	// fanOut is what the first release step answers.
	fanOut   bool
	releases []configurationapp.PlatformModelDefaultModelRow
	fanOuts  []configurationapp.PlatformModelDefaultModelRow
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
	_ context.Context, row configurationapp.PlatformModelDefaultModelRow,
) (configurationapp.PlatformModelDefaultUsage, error) {
	f.usages = append(f.usages, row)
	return f.usage, f.usageErr
}

func (f *fakeModelDefaults) ReleaseDeletedPlatformModelDefault(
	_ context.Context, row configurationapp.PlatformModelDefaultModelRow,
) (bool, error) {
	f.releases = append(f.releases, row)
	return f.fanOut, nil
}

func (f *fakeModelDefaults) ReleaseDeletedProjectModelDefaults(
	_ context.Context, row configurationapp.PlatformModelDefaultModelRow,
) error {
	f.fanOuts = append(f.fanOuts, row)
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
			return deletedModelRowFrom(5, "llm", "GPT all", "gpt-all", true), true, nil
		case "6":
			return deletedModelRowFrom(6, "llm", "own", "own", false), true, nil
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
	wantRow := configurationapp.PlatformModelDefaultModelRow{
		OwnerProjectID: 1, RowID: 5, Section: configurationapp.CurrentModelSectionLLM, Name: "gpt-all", Shared: true,
	}
	if len(manager.usages) != 1 || manager.usages[0] != wantRow {
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
	handler := NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager))
	admin := withPlatformModelDelete(context.Background())

	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom(3, "llm", "Title", "gpt-own", false))
	handler.releaseDeletedModelDefault(admin, "1", deletedModelRowFrom(4, "vectorstorage", "pgvector", "", true))
	// Not a model section, no name, or a malformed project: nothing to release.
	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom(5, "ai_credentials", "openai", "", false))
	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom(6, "llm", "Title", "", false))
	handler.releaseDeletedModelDefault(context.Background(), "x", deletedModelRowFrom(7, "llm", "Title", "gpt", false))

	want := []configurationapp.PlatformModelDefaultModelRow{
		{OwnerProjectID: 7, Section: configurationapp.CurrentModelSectionLLM, Name: "gpt-own"},
		{OwnerProjectID: 1, Section: configurationapp.CurrentModelSectionVectorStorage, Name: "pgvector", Shared: true},
	}
	if len(manager.releases) != len(want) {
		t.Fatalf("releases = %+v, want %+v", manager.releases, want)
	}
	for index := range want {
		// RowID is 0: the row is gone, so the survivor check leaves no row out.
		if manager.releases[index] != want[index] {
			t.Fatalf("release %d = %+v, want %+v", index, manager.releases[index], want[index])
		}
	}

	// Without a service the hook is a no-op rather than a panic.
	NewHandler(nil).releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom(1, "llm", "T", "gpt", false))
}

func TestReleaseDeletedModelDefaultRunsTheFanOutAfterThePlatformStep(t *testing.T) {
	manager := &fakeModelDefaults{fanOut: true}
	handler := NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager))
	var deferred []func()
	handler.modelDefaultRelease = func(release func()) { deferred = append(deferred, release) }

	handler.releaseDeletedModelDefault(withPlatformModelDelete(context.Background()), "1",
		deletedModelRowFrom(9, "llm", "GPT", "gpt-all", true))
	if len(manager.releases) != 1 || len(manager.fanOuts) != 0 || len(deferred) != 1 {
		t.Fatalf("releases=%d fanOuts=%d deferred=%d; the fan-out must not run inside the request",
			len(manager.releases), len(manager.fanOuts), len(deferred))
	}
	deferred[0]()
	if len(manager.fanOuts) != 1 || recordedFrom(manager.fanOuts[0]) != (recordedRelease{1, configurationapp.CurrentModelSectionLLM, "gpt-all"}) {
		t.Fatalf("fanOuts = %+v", manager.fanOuts)
	}

	// No fan-out is scheduled when the first step says there is none.
	manager = &fakeModelDefaults{}
	handler = NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager))
	handler.modelDefaultRelease = func(func()) { t.Fatal("a fan-out was scheduled") }
	handler.releaseDeletedModelDefault(context.Background(), "7", deletedModelRowFrom(9, "llm", "GPT", "own", false))
}

func TestProjectRouteDeleteOfAPublicRowReleasesNoPlatformDefault(t *testing.T) {
	manager := &fakeModelDefaults{fanOut: true}
	handler := NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager))
	handler.modelDefaultRelease = func(func()) { t.Fatal("a fan-out was scheduled") }

	// The project-scoped DELETE /configuration/{public}/{id} carries no mark:
	// it is gated by the project permission, not configuration.governance.
	handler.releaseDeletedModelDefault(context.Background(), "1", deletedModelRowFrom(9, "llm", "GPT", "gpt-all", true))
	if len(manager.releases) != 0 {
		t.Fatalf("a project-route delete released platform-wide defaults: %+v", manager.releases)
	}
}

func TestGlobalModelDefaultUsageAnswers404ForAnOutOfRangeID(t *testing.T) {
	manager := &fakeModelDefaults{}
	handler := NewHandler(nil, WithPublicProjectID(1), WithModelDefaults(manager))
	handler.modelRowLookup = func(_ context.Context, _ int, configID string) (deletedModelRow, bool, error) {
		if configurationIDColumn(configID) != "uuid::text" {
			t.Fatalf("an out-of-range id %q was bound to the integer id column", configID)
		}
		return deletedModelRow{}, false, nil
	}
	recorder := servePlatformDefault(t, platformDefaultRouter(handler), http.MethodGet,
		"/gateway/platform_models/99999999999/default_usage", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("usage of an out-of-range id = %d, want 404", recorder.Code)
	}
}
