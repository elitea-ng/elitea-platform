package eliteacore_test

// An imported agent runs on a model of THIS install (issue 3143). A blueprint
// names no model, and a file from another install names that install's model
// and project. Stored as they came, both resolved at every run, silently, to
// the project's default model: on an install whose default is a 256k model,
// every imported agent ran capped at 256k although a 1M model was
// configured. The import now pins the model the file names when the
// catalogue holds it, else the project's default, and its answer says which
// model each agent got, with that model's context window.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

const (
	importModelPublicProject = 1
	importModelDefault       = "local-qwen-256k"
	importModelLarge         = "frontier-1m"
)

// importModelCatalog answers the project's catalogue: a 256k default of the
// project and a shared 1M model of the public project.
type importModelCatalog struct{ reads int }

func (catalog *importModelCatalog) Get(_ context.Context, query configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
	catalog.reads++
	small, large := 262_144, 1_000_000
	name, project := importModelDefault, query.ProjectID
	return configurationapp.CurrentModelCatalogResponse{
		Items: []configurationapp.CurrentModelCatalogItem{
			{Name: importModelDefault, ProjectID: query.ProjectID, ContextWindow: &small},
			{Name: importModelLarge, ProjectID: importModelPublicProject, Shared: true, ContextWindow: &large},
		},
		DefaultModelName: &name, DefaultModelProjectID: &project,
	}, nil
}

func modelAgent(uuid, name string, llm map[string]any) map[string]any {
	agent := teamAgent(uuid, name)
	if llm != nil {
		agent["versions"].([]map[string]any)[0]["llm_settings"] = llm
	}
	return agent
}

type importedModelAnswer struct {
	Result struct {
		Agents []struct {
			Name  string `json:"name"`
			Model *struct {
				Name          string `json:"name"`
				ProjectID     int32  `json:"project_id"`
				ContextWindow *int   `json:"context_window"`
				Defaulted     bool   `json:"defaulted"`
				Requested     string `json:"requested"`
			} `json:"model"`
		} `json:"agents"`
	} `json:"result"`
}

func storedModel(t *testing.T, pool *pgxpool.Pool, name string) (string, json.Number) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT v.llm_settings::text FROM p_1.application_versions v
		JOIN p_1.applications a ON a.id = v.application_id WHERE a.name = $1`, name).Scan(&raw); err != nil {
		t.Fatalf("read %s's llm_settings: %v", name, err)
	}
	var settings struct {
		ModelName      string      `json:"model_name"`
		ModelProjectID json.Number `json:"model_project_id"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("decode %s's llm_settings %s: %v", name, raw, err)
	}
	return settings.ModelName, settings.ModelProjectID
}

func TestTeamImportPinsEachAgentToAModelOfThisInstall(t *testing.T) {
	pool := newImportLinkPool(t)
	catalog := &importModelCatalog{}
	router := teamImportRouter(eliteacore.NewHandler(pool, eliteacore.WithImportModelCatalog(catalog)))

	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", teamDocument(
		map[string]any{"name": "Dev team", "lead": "m-lead", "members": []string{"m-lead", "m-large", "m-gone"}},
		[]map[string]any{
			// A blueprint member: a reasoning effort, no model.
			modelAgent("m-lead", "Dev Coordinator", map[string]any{"reasoning_effort": "high"}),
			// Exported on another install, where the 1M model lived in project 77.
			modelAgent("m-large", "Dev Implementer", map[string]any{"model_name": importModelLarge, "model_project_id": "77", "max_tokens": 4096}),
			// A model this install does not hold.
			modelAgent("m-gone", "Dev Reviewer", map[string]any{"model_name": "retired-model"}),
		},
		nil,
	))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("team import: got %d: %s", recorder.Code, recorder.Body.String())
	}
	if catalog.reads != 1 {
		t.Errorf("the import read the catalogue %d times, want once", catalog.reads)
	}
	var answer importedModelAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]struct {
		model     string
		project   int32
		window    int
		defaulted bool
		requested string
	}{
		"Dev Coordinator": {importModelDefault, 1, 262_144, true, ""},
		"Dev Implementer": {importModelLarge, importModelPublicProject, 1_000_000, false, importModelLarge},
		"Dev Reviewer":    {importModelDefault, 1, 262_144, true, "retired-model"},
	}
	if len(answer.Result.Agents) != len(want) {
		t.Fatalf("imported %d agents, want %d", len(answer.Result.Agents), len(want))
	}
	for _, agent := range answer.Result.Agents {
		expected := want[agent.Name]
		model := agent.Model
		if model == nil {
			t.Errorf("%s: the answer names no model", agent.Name)
			continue
		}
		if model.Name != expected.model || model.ProjectID != expected.project || model.Defaulted != expected.defaulted ||
			model.Requested != expected.requested || model.ContextWindow == nil || *model.ContextWindow != expected.window {
			t.Errorf("%s: answered model %+v (window %v), want %+v", agent.Name, *model, model.ContextWindow, expected)
		}
		// What the run reads: the stored model, by name and project.
		name, project := storedModel(t, pool, agent.Name)
		if name != expected.model || project.String() != strconv.Itoa(int(expected.project)) {
			t.Errorf("%s: stored model %q in project %s, want %q in %d", agent.Name, name, project, expected.model, expected.project)
		}
	}
}

func TestImportWithoutACatalogueKeepsTheFileModelSettings(t *testing.T) {
	pool := newImportLinkPool(t)
	router := teamImportRouter(eliteacore.NewHandler(pool))

	recorder := teamImportPost(t, router, "/elitea_core/import_wizard/prompt_lib/1", map[string]any{
		"applications": []map[string]any{modelAgent("m-solo", "Dev Solo", map[string]any{"model_name": "retired-model"})},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import: got %d: %s", recorder.Code, recorder.Body.String())
	}
	var answer importedModelAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(answer.Result.Agents) != 1 || answer.Result.Agents[0].Model != nil {
		t.Fatalf("an import with no catalogue answered a model: %s", recorder.Body.String())
	}
	if name, _ := storedModel(t, pool, "Dev Solo"); name != "retired-model" {
		t.Errorf("stored model %q, want the file's", name)
	}
}

// A pipeline runs the models its nodes name: the import leaves its settings
// as the file carries them and answers no model for it (review of #3148).
func TestImportLeavesAPipelinesModelSettingsAlone(t *testing.T) {
	pool := newImportLinkPool(t)
	catalog := &importModelCatalog{}
	router := teamImportRouter(eliteacore.NewHandler(pool, eliteacore.WithImportModelCatalog(catalog)))

	pipeline := modelAgent("m-flow", "Dev Flow", map[string]any{"temperature": 0.2})
	pipeline["versions"].([]map[string]any)[0]["agent_type"] = "pipeline"
	recorder := teamImportPost(t, router, "/elitea_core/import_wizard/prompt_lib/1", map[string]any{
		"applications": []map[string]any{pipeline, modelAgent("m-solo", "Dev Solo", nil)},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import: got %d: %s", recorder.Code, recorder.Body.String())
	}
	var answer importedModelAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := map[string]bool{}
	for _, agent := range answer.Result.Agents {
		models[agent.Name] = agent.Model != nil
	}
	if models["Dev Flow"] || !models["Dev Solo"] {
		t.Fatalf("answered models %v, want only Dev Solo's: %s", models, recorder.Body.String())
	}
	if name, _ := storedModel(t, pool, "Dev Flow"); name != "" {
		t.Errorf("the pipeline's stored model is %q, want none", name)
	}
	if name, _ := storedModel(t, pool, "Dev Solo"); name != importModelDefault {
		t.Errorf("the agent's stored model is %q, want %q", name, importModelDefault)
	}
}
