package configurations_test

// The platform default model's two database-facing hooks (#6826), against a
// real PostgreSQL: the default_usage read names the row's provider model, and
// the delete reports the deleted row so its stored defaults are released.
// The release itself is unit-tested in the application package; this file
// proves the handler hands it the right identity from the row it deleted.

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

type recordedModelDefaultCall struct {
	owner   int32
	section configurationapp.CurrentModelSection
	name    string
}

type recordingModelDefaults struct {
	mu       sync.Mutex
	usages   []recordedModelDefaultCall
	releases []recordedModelDefaultCall
}

func (*recordingModelDefaults) Get(context.Context, configurationapp.CurrentModelSection) (configurationapp.PlatformModelDefaultView, error) {
	return configurationapp.PlatformModelDefaultView{}, nil
}

func (*recordingModelDefaults) Set(context.Context, configurationapp.CurrentModelSection, string) error {
	return nil
}

func (*recordingModelDefaults) Clear(context.Context, configurationapp.CurrentModelSection) error {
	return nil
}

func (r *recordingModelDefaults) Usage(
	_ context.Context, owner int32, section configurationapp.CurrentModelSection, name string,
) (configurationapp.PlatformModelDefaultUsage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usages = append(r.usages, recordedModelDefaultCall{owner, section, name})
	return configurationapp.PlatformModelDefaultUsage{ModelName: name, Projects: 2}, nil
}

func (r *recordingModelDefaults) ReleaseDeletedModelDefault(
	_ context.Context, owner int32, section configurationapp.CurrentModelSection, name string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = append(r.releases, recordedModelDefaultCall{owner, section, name})
	return nil
}

func platformDefaultIntegrationRouter(pool *pgxpool.Pool, defaults configurations.ModelDefaultsManager) chi.Router {
	handler := configurations.NewHandler(pool,
		configurations.WithPublicProjectID(globalScopeProject),
		configurations.WithModelDefaults(defaults))
	r := chi.NewRouter()
	r.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes())
		r.Mount("/platform_models", handler.GlobalModelRoutes())
	})
	return r
}

func TestPlatformModelDeleteReleasesItsStoredDefaults(t *testing.T) {
	pool := newCreateEchoPool(t)
	defaults := &recordingModelDefaults{}
	router := platformDefaultIntegrationRouter(pool, defaults)
	provider := platformLinkProvider(t, router, "autotest_default_provider")

	const title = "autotest_default_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models", map[string]any{
		"elitea_title": title,
		"type":         "llm_model",
		"data": map[string]any{
			"name":           "autotest-default-wire",
			"ai_credentials": map[string]any{"elitea_title": provider},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d; body = %s", created.Code, created.Body.String())
	}
	id := readPlatformModel(t, router, title).ID

	usage := platformLinkSend(t, router, http.MethodGet,
		"/gateway/platform_models/"+strconv.Itoa(id)+"/default_usage", nil)
	if usage.Code != http.StatusOK {
		t.Fatalf("default_usage = %d; body = %s", usage.Code, usage.Body.String())
	}
	want := recordedModelDefaultCall{int32(globalScopeProject), configurationapp.CurrentModelSectionLLM, "autotest-default-wire"}
	if len(defaults.usages) != 1 || defaults.usages[0] != want {
		t.Fatalf("usage calls = %+v, want %+v (the provider model name, not the title)", defaults.usages, want)
	}

	deleted := platformLinkSend(t, router, http.MethodDelete,
		"/gateway/platform_models/"+strconv.Itoa(id), nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d; body = %s", deleted.Code, deleted.Body.String())
	}
	if len(defaults.releases) != 1 || defaults.releases[0] != want {
		t.Fatalf("release calls = %+v, want %+v", defaults.releases, want)
	}

	// A second delete finds no row: 404, and nothing more to release.
	again := platformLinkSend(t, router, http.MethodDelete,
		"/gateway/platform_models/"+strconv.Itoa(id), nil)
	if again.Code != http.StatusNotFound || len(defaults.releases) != 1 {
		t.Fatalf("second delete = %d releases=%+v", again.Code, defaults.releases)
	}
	gone := platformLinkSend(t, router, http.MethodGet,
		"/gateway/platform_models/"+strconv.Itoa(id)+"/default_usage", nil)
	if gone.Code != http.StatusNotFound {
		t.Fatalf("default_usage of a deleted model = %d, want 404", gone.Code)
	}
}
