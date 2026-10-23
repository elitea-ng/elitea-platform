package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type currentToolkitReaderStub struct {
	toolkit CurrentToolkitSnapshot
	found   bool
	err     error
	calls   int
}

func (s *currentToolkitReaderStub) GetCurrentToolkit(
	_ context.Context,
	projectID int32,
	userID int32,
	toolkitID int32,
) (CurrentToolkitSnapshot, bool, error) {
	s.calls++
	if projectID != 7 || userID != 42 || toolkitID != 19 {
		return CurrentToolkitSnapshot{}, false, errors.New("unexpected toolkit scope")
	}
	return s.toolkit, s.found, s.err
}

type currentModelCatalogStub struct {
	response configurationapp.CurrentModelCatalogResponse
	err      error
	calls    int
	query    configurationapp.CurrentModelCatalogQuery
	get      func(configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

type currentToolkitSettingsValidatorStub struct {
	result map[string]any
	err    error
	calls  []configurationapp.CurrentToolkitSettingsRequest
}

type currentEmbeddingBindingSelectorStub struct {
	binding EmbeddingBinding
	err     error
	calls   []struct {
		projectID          int32
		modelName          string
		preferredProjectID *int32
	}
}

func (s *currentEmbeddingBindingSelectorStub) Resolve(
	_ context.Context,
	projectID int32,
	modelName string,
	preferredProjectID *int32,
) (EmbeddingBinding, error) {
	var preferredCopy *int32
	if preferredProjectID != nil {
		value := *preferredProjectID
		preferredCopy = &value
	}
	s.calls = append(s.calls, struct {
		projectID          int32
		modelName          string
		preferredProjectID *int32
	}{projectID: projectID, modelName: modelName, preferredProjectID: preferredCopy})
	if s.err != nil {
		return EmbeddingBinding{}, s.err
	}
	if s.binding.SchemaVersion != "" {
		return s.binding.Clone(), nil
	}
	binding := currentEmbeddingBindingForTest(projectID, modelName)
	if preferredProjectID != nil {
		binding.ModelProjectID = *preferredProjectID
		binding.ConfigurationProjectID = *preferredProjectID
	}
	return binding, nil
}

func (s *currentToolkitSettingsValidatorStub) Resolve(
	_ context.Context,
	request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	s.calls = append(s.calls, configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: request.ToolkitType,
		Settings:    cloneCurrentResolverObject(request.Settings),
		ProjectID:   request.ProjectID,
		UserID:      request.UserID,
		Mode:        request.Mode,
	})
	if s.err != nil {
		return nil, s.err
	}
	if s.result != nil {
		return cloneCurrentResolverObject(s.result), nil
	}
	return cloneCurrentResolverObject(request.Settings), nil
}

func (s *currentModelCatalogStub) Get(
	_ context.Context,
	query configurationapp.CurrentModelCatalogQuery,
) (configurationapp.CurrentModelCatalogResponse, error) {
	s.calls++
	s.query = query
	if s.get != nil {
		return s.get(query)
	}
	return s.response, s.err
}

func TestCurrentAuthoritativeInputResolverLoadsSavedToolkitAndConfigurationModelMetadata(t *testing.T) {
	openAICompatible := true
	toolkits := &currentToolkitReaderStub{found: true, toolkit: CurrentToolkitSnapshot{
		ID:   19,
		Type: "confluence",
		Name: "Wiki.Main !",
		Settings: map[string]any{
			"confluence_configuration": map[string]any{"elitea_title": "wiki-prod"},
			"pgvector_configuration":   map[string]any{"elitea_title": "project-vectorstore"},
			"large_integer":            json.Number("9007199254740993"),
		},
	}}
	models := &currentModelCatalogStub{response: configurationapp.CurrentModelCatalogResponse{
		Items: []configurationapp.CurrentModelCatalogItem{
			{Name: "claude-through-openai", ProjectID: 1, Shared: true, OpenAICompatible: &openAICompatible},
		},
	}}
	settingsValidator := &currentToolkitSettingsValidatorStub{result: map[string]any{
		"confluence_configuration": map[string]any{
			configurationapp.CurrentFrozenConfigurationMarker: true,
			"elitea_title":             "wiki-prod",
			"private":                  false,
			"url":                      "https://wiki.example",
			"token":                    "{{secret.CONFLUENCE_TOKEN}}",
			"configuration_uuid":       "configuration-confluence",
			"configuration_project_id": int32(1),
			"configuration_type":       "confluence",
		},
		"pgvector_configuration": map[string]any{
			configurationapp.CurrentFrozenConfigurationMarker: true,
			"elitea_title":             "project-vectorstore",
			"private":                  true,
			"connection_string":        "{{secret.PGVECTOR_CONNECTION}}",
			"configuration_uuid":       "configuration-pgvector",
			"configuration_project_id": int32(700),
			"configuration_type":       "pgvector",
		},
		"large_integer": json.Number("9007199254740993"),
	}}
	resolver, err := NewCurrentAuthoritativeInputResolver(
		toolkits,
		models,
		settingsValidator,
		&currentEmbeddingBindingSelectorStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	model := "claude-through-openai"
	request := StartRequest{
		ProjectID:            7,
		ActorUserID:          42,
		ToolkitID:            19,
		ToolParameters:       json.RawMessage(`{"index_name":"docs","source":"all"}`),
		RequestedLLMModel:    &model,
		RequestedLLMSettings: json.RawMessage(`{"temperature":0.1,"seed":9007199254740993}`),
	}

	inputs, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if toolkits.calls != 1 || models.calls != 2 || models.query != (configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1, IncludeShared: true,
	}) {
		t.Fatalf("toolkit calls=%d model calls=%d query=%+v", toolkits.calls, models.calls, models.query)
	}
	if len(settingsValidator.calls) != 1 || settingsValidator.calls[0].ToolkitType != "confluence" ||
		settingsValidator.calls[0].ProjectID != 7 || settingsValidator.calls[0].UserID != 42 ||
		settingsValidator.calls[0].Mode != configurationapp.CurrentToolkitSettingsReferenceMode {
		t.Fatalf("settings validation calls=%+v", settingsValidator.calls)
	}

	toolkit, err := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if toolkit["id"] != json.Number("19") || toolkit["type"] != "confluence" || toolkit["toolkit_name"] != "Wiki_Main" {
		t.Fatalf("toolkit identity=%#v", toolkit)
	}
	settings := toolkit["settings"].(map[string]any)
	confluence := settings["confluence_configuration"].(map[string]any)
	pgvector := settings["pgvector_configuration"].(map[string]any)
	if settings["large_integer"] != json.Number("9007199254740993") ||
		confluence["configuration_uuid"] != "configuration-confluence" ||
		confluence["configuration_project_id"] != json.Number("1") ||
		confluence["token"] != "{{secret.CONFLUENCE_TOKEN}}" ||
		pgvector["configuration_uuid"] != "configuration-pgvector" ||
		pgvector["configuration_project_id"] != json.Number("700") ||
		pgvector["connection_string"] != "{{secret.PGVECTOR_CONNECTION}}" {
		t.Fatalf("sealed expansion was not frozen at admission: %#v", settings)
	}
	if strings.Contains(string(inputs.ToolkitConfiguration), "confluence-secret") ||
		strings.Contains(string(inputs.ToolkitConfiguration), "postgresql://") {
		t.Fatal("resolver materialized credential plaintext during admission")
	}

	llm, err := decodeCurrentResolverObject(inputs.LLMConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if llm["openai_compatible"] != true || llm["seed"] != json.Number("9007199254740993") || inputs.LLMModel == nil || *inputs.LLMModel != model {
		t.Fatalf("LLM resolution model=%v settings=%#v", inputs.LLMModel, llm)
	}
	if inputs.MCPReferences != nil {
		t.Fatalf("unexpected invented MCP credential input: %s", inputs.MCPReferences)
	}
}

func TestCurrentAuthoritativeInputResolverRejectsDatabaseIdentityOverflowBeforeDependencies(
	t *testing.T,
) {
	for name, mutate := range map[string]func(*StartRequest){
		"project": func(request *StartRequest) {
			request.ProjectID = math.MaxInt32 + 1
		},
		"actor": func(request *StartRequest) {
			request.ActorUserID = math.MaxInt32 + 1
		},
		"toolkit": func(request *StartRequest) {
			request.ToolkitID = math.MaxInt32 + 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := validCurrentStartRequest(nil, `{}`)
			mutate(&request)
			toolkits := validCurrentToolkitReader()
			settings := &currentToolkitSettingsValidatorStub{}
			resolver := newCurrentAuthoritativeResolverWithSettingsForTest(
				t,
				toolkits,
				&currentModelCatalogStub{},
				settings,
			)

			if _, err := resolver.Resolve(
				context.Background(),
				request,
			); !errors.Is(err, ErrInvalidIndexStart) {
				t.Fatalf("Resolve() error=%v", err)
			}
			if _, err := resolver.ResolveScheduled(
				context.Background(),
				request,
				toolkits.toolkit,
			); !errors.Is(err, ErrInvalidIndexStart) {
				t.Fatalf("ResolveScheduled() error=%v", err)
			}
			if toolkits.calls != 0 || len(settings.calls) != 0 {
				t.Fatalf(
					"invalid request crossed dependencies: toolkits=%d settings=%d",
					toolkits.calls,
					len(settings.calls),
				)
			}
		})
	}
}

func TestCurrentAuthoritativeInputResolverUsesConfigurationsDefaultWithoutCopyingCredentials(t *testing.T) {
	compatible := false
	defaultName := "configured-default"
	defaultProjectID := int32(7)
	toolkits := validCurrentToolkitReader()
	models := &currentModelCatalogStub{get: func(
		query configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		if query.Section == configurationapp.CurrentModelSectionEmbedding {
			return configurationapp.CurrentModelCatalogResponse{}, nil
		}
		return configurationapp.CurrentModelCatalogResponse{
			DefaultModelName:      &defaultName,
			DefaultModelProjectID: &defaultProjectID,
			Items: []configurationapp.CurrentModelCatalogItem{{
				Name: defaultName, ProjectID: 7, OpenAICompatible: &compatible,
			}},
		}, nil
	}}
	resolver := newCurrentAuthoritativeResolverForTest(t, toolkits, models)

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if inputs.LLMModel == nil || *inputs.LLMModel != defaultName {
		t.Fatalf("model=%v, want Configurations default", inputs.LLMModel)
	}
	settings, _ := decodeCurrentResolverObject(inputs.LLMConfiguration)
	if settings["openai_compatible"] != false || len(settings) != 1 {
		t.Fatalf("settings=%#v", settings)
	}
}

func TestCurrentAuthoritativeInputResolverFreezesScheduledOverrideWithoutReloadingToolkit(t *testing.T) {
	toolkits := validCurrentToolkitReader()
	models := &currentModelCatalogStub{}
	settings := &currentToolkitSettingsValidatorStub{}
	resolver := newCurrentAuthoritativeResolverWithSettingsForTest(
		t,
		toolkits,
		models,
		settings,
	)
	snapshot := CurrentToolkitSnapshot{
		ID:   19,
		Type: "github",
		Name: "scheduled",
		Settings: map[string]any{
			"github_configuration": map[string]any{
				"elitea_title": "schedule-owner-github",
				"private":      true,
			},
			"pgvector_configuration": map[string]any{
				"elitea_title": "project-vectorstore",
				"private":      false,
			},
		},
	}

	inputs, err := resolver.ResolveScheduled(
		context.Background(),
		validCurrentStartRequest(nil, `{}`),
		snapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if toolkits.calls != 0 {
		t.Fatalf("scheduled snapshot was reloaded %d times", toolkits.calls)
	}
	if len(settings.calls) != 1 ||
		settings.calls[0].Mode != configurationapp.CurrentToolkitSettingsReferenceMode ||
		settings.calls[0].Settings["github_configuration"].(map[string]any)["elitea_title"] != "schedule-owner-github" {
		t.Fatalf("scheduled settings call=%+v", settings.calls)
	}
	toolkit, err := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	frozen := toolkit["settings"].(map[string]any)
	if frozen["github_configuration"].(map[string]any)["elitea_title"] != "schedule-owner-github" {
		t.Fatalf("scheduled override was not frozen: %#v", frozen)
	}
}

func TestCurrentAuthoritativeInputResolverAppliesConfigurationsEmbeddingDefault(t *testing.T) {
	defaultEmbedding := "configured-embedding"
	defaultProjectID := int32(7)
	toolkits := validCurrentToolkitReader()
	toolkits.toolkit.Settings["embedding_model"] = ""
	var sections []configurationapp.CurrentModelSection
	models := &currentModelCatalogStub{get: func(
		query configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		sections = append(sections, query.Section)
		if query.Section == configurationapp.CurrentModelSectionEmbedding {
			return configurationapp.CurrentModelCatalogResponse{
				DefaultModelName:      &defaultEmbedding,
				DefaultModelProjectID: &defaultProjectID,
			}, nil
		}
		return configurationapp.CurrentModelCatalogResponse{}, nil
	}}
	embeddings := &currentEmbeddingBindingSelectorStub{}
	resolver := newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
		t,
		toolkits,
		models,
		&currentToolkitSettingsValidatorStub{},
		embeddings,
	)

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	toolkit, err := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	settings := toolkit["settings"].(map[string]any)
	if settings["embedding_model"] != defaultEmbedding {
		t.Fatalf("embedding_model=%#v, want Configurations default", settings["embedding_model"])
	}
	if inputs.EmbeddingBinding == nil ||
		inputs.EmbeddingBinding.ModelName != defaultEmbedding ||
		inputs.EmbeddingBinding.ResolvedModelGroup != "7_"+defaultEmbedding ||
		len(embeddings.calls) != 1 ||
		embeddings.calls[0].projectID != 7 ||
		embeddings.calls[0].modelName != defaultEmbedding ||
		embeddings.calls[0].preferredProjectID == nil ||
		*embeddings.calls[0].preferredProjectID != defaultProjectID {
		t.Fatalf("embedding binding=%+v calls=%+v", inputs.EmbeddingBinding, embeddings.calls)
	}
	if !reflect.DeepEqual(sections, []configurationapp.CurrentModelSection{
		configurationapp.CurrentModelSectionEmbedding,
		configurationapp.CurrentModelSectionLLM,
	}) {
		t.Fatalf("catalog sections=%v", sections)
	}
}

func TestCurrentAuthoritativeInputResolverFreezesConfigurationsEmbeddingDefaultForOmittedUIShape(t *testing.T) {
	defaultEmbedding := "configured-embedding"
	defaultProjectID := int32(7)
	toolkits := validCurrentToolkitReader()
	models := &currentModelCatalogStub{get: func(
		query configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		if query.Section == configurationapp.CurrentModelSectionEmbedding {
			return configurationapp.CurrentModelCatalogResponse{
				DefaultModelName:      &defaultEmbedding,
				DefaultModelProjectID: &defaultProjectID,
			}, nil
		}
		return configurationapp.CurrentModelCatalogResponse{}, nil
	}}
	embeddings := &currentEmbeddingBindingSelectorStub{}
	resolver := newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
		t,
		toolkits,
		models,
		&currentToolkitSettingsValidatorStub{},
		embeddings,
	)

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	toolkit, err := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	settings := toolkit["settings"].(map[string]any)
	if settings["embedding_model"] != defaultEmbedding ||
		inputs.EmbeddingBinding == nil ||
		inputs.EmbeddingBinding.ResolvedModelGroup != "7_"+defaultEmbedding ||
		len(embeddings.calls) != 1 {
		t.Fatalf("settings=%#v binding=%+v calls=%+v", settings, inputs.EmbeddingBinding, embeddings.calls)
	}
}

func TestCurrentAuthoritativeInputResolverKeepsOmittedEmbeddingOptionalWhenCatalogHasNoDefault(t *testing.T) {
	embeddings := &currentEmbeddingBindingSelectorStub{}
	resolver := newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
		t,
		validCurrentToolkitReader(),
		&currentModelCatalogStub{},
		&currentToolkitSettingsValidatorStub{},
		embeddings,
	)

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if inputs.EmbeddingBinding != nil || len(embeddings.calls) != 0 {
		t.Fatalf("binding=%+v calls=%+v", inputs.EmbeddingBinding, embeddings.calls)
	}
}

func TestCurrentAuthoritativeInputResolverFailsClosedForRequiredEmbeddingWithoutBinding(t *testing.T) {
	for name, configure := range map[string]func(
		*currentToolkitReaderStub,
		*currentModelCatalogStub,
		*currentEmbeddingBindingSelectorStub,
	){
		"empty without catalog default": func(
			toolkits *currentToolkitReaderStub,
			_ *currentModelCatalogStub,
			_ *currentEmbeddingBindingSelectorStub,
		) {
			toolkits.toolkit.Settings["embedding_model"] = ""
		},
		"runtime mismatch": func(
			toolkits *currentToolkitReaderStub,
			_ *currentModelCatalogStub,
			embeddings *currentEmbeddingBindingSelectorStub,
		) {
			toolkits.toolkit.Settings["embedding_model"] = "saved-embedding"
			embeddings.binding = currentEmbeddingBindingForTest(7, "other-embedding")
		},
		"runtime unavailable": func(
			toolkits *currentToolkitReaderStub,
			_ *currentModelCatalogStub,
			embeddings *currentEmbeddingBindingSelectorStub,
		) {
			toolkits.toolkit.Settings["embedding_model"] = "saved-embedding"
			embeddings.err = errors.New("upstream secret detail")
		},
		"embedding catalog unavailable": func(
			toolkits *currentToolkitReaderStub,
			models *currentModelCatalogStub,
			_ *currentEmbeddingBindingSelectorStub,
		) {
			toolkits.toolkit.Settings["embedding_model"] = ""
			models.err = errors.New("catalog secret detail")
		},
	} {
		t.Run(name, func(t *testing.T) {
			toolkits := validCurrentToolkitReader()
			models := &currentModelCatalogStub{}
			embeddings := &currentEmbeddingBindingSelectorStub{}
			configure(toolkits, models, embeddings)
			resolver := newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
				t,
				toolkits,
				models,
				&currentToolkitSettingsValidatorStub{},
				embeddings,
			)
			_, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
			if !errors.Is(err, ErrCurrentEmbeddingBindingUnavailable) &&
				!errors.Is(err, ErrCurrentEmbeddingBindingAmbiguous) {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe embedding error=%v", err)
			}
		})
	}
}

func TestCurrentAuthoritativeInputResolverPreservesExplicitEmbeddingModel(t *testing.T) {
	toolkits := validCurrentToolkitReader()
	toolkits.toolkit.Settings["embedding_model"] = "saved-embedding"
	models := &currentModelCatalogStub{}
	embeddings := &currentEmbeddingBindingSelectorStub{}
	resolver := newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
		t,
		toolkits,
		models,
		&currentToolkitSettingsValidatorStub{},
		embeddings,
	)

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	toolkit, err := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	settings := toolkit["settings"].(map[string]any)
	if settings["embedding_model"] != "saved-embedding" {
		t.Fatalf("embedding_model=%#v, want saved value", settings["embedding_model"])
	}
	if inputs.EmbeddingBinding == nil ||
		inputs.EmbeddingBinding.ResolvedModelGroup != "7_saved-embedding" ||
		len(embeddings.calls) != 1 {
		t.Fatalf("embedding binding=%+v calls=%+v", inputs.EmbeddingBinding, embeddings.calls)
	}
	if models.calls != 1 || models.query.Section != configurationapp.CurrentModelSectionLLM {
		t.Fatalf("unexpected model catalog calls=%d query=%+v", models.calls, models.query)
	}
}

func TestCurrentAuthoritativeInputResolverOverwritesCallerCompatibilityFromConfigurations(t *testing.T) {
	compatible := false
	models := &currentModelCatalogStub{response: configurationapp.CurrentModelCatalogResponse{Items: []configurationapp.CurrentModelCatalogItem{{
		Name: "explicit-model", ProjectID: 7, OpenAICompatible: &compatible,
	}}}}
	resolver := newCurrentAuthoritativeResolverForTest(t, validCurrentToolkitReader(), models)
	model := "explicit-model"

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(&model, `{"openai_compatible":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if models.calls != 2 {
		t.Fatalf("authoritative compatibility queried catalog %d times", models.calls)
	}
	settings, _ := decodeCurrentResolverObject(inputs.LLMConfiguration)
	if settings["openai_compatible"] != false {
		t.Fatalf("settings=%#v", settings)
	}
}

func TestCurrentAuthoritativeInputResolverFailsClosedWhenConfigurationsModelCatalogIsUnavailable(t *testing.T) {
	models := &currentModelCatalogStub{get: func(
		query configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		if query.Section == configurationapp.CurrentModelSectionEmbedding {
			return configurationapp.CurrentModelCatalogResponse{}, nil
		}
		return configurationapp.CurrentModelCatalogResponse{}, errors.New("catalog details must not escape")
	}}
	resolver := newCurrentAuthoritativeResolverForTest(t, validCurrentToolkitReader(), models)
	model := "explicit-model"
	_, err := resolver.Resolve(context.Background(), validCurrentStartRequest(&model, `{"openai_compatible":true}`))
	if !errors.Is(err, ErrCurrentModelResolutionUnavailable) || err.Error() != ErrCurrentModelResolutionUnavailable.Error() {
		t.Fatalf("error=%v, want redacted model-resolution failure", err)
	}
}

func TestCurrentAuthoritativeInputResolverFreezesSealedExpansionWithoutAliasing(t *testing.T) {
	expansion := map[string]any{
		"credential": map[string]any{
			configurationapp.CurrentFrozenConfigurationMarker: true,
			"elitea_title":             "frozen-title",
			"private":                  false,
			"token":                    "{{secret.FROZEN}}",
			"configuration_uuid":       "frozen-uuid",
			"configuration_project_id": int32(7),
			"configuration_type":       "github",
		},
	}
	validator := &currentToolkitSettingsValidatorStub{result: expansion}
	toolkits := validCurrentToolkitReader()
	resolver := newCurrentAuthoritativeResolverWithSettingsForTest(
		t,
		toolkits,
		&currentModelCatalogStub{},
		validator,
	)

	inputs, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(validator.calls) != 1 || validator.calls[0].Mode != configurationapp.CurrentToolkitSettingsReferenceMode {
		t.Fatalf("settings validation calls=%+v", validator.calls)
	}
	expansion["credential"].(map[string]any)["token"] = "mutated-after-resolve"
	validator.result["credential"].(map[string]any)["elitea_title"] = "changed-title"
	validator.calls[0].Settings["credential"] = "changed-original-reference"
	toolkit, decodeErr := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	credential := toolkit["settings"].(map[string]any)["credential"].(map[string]any)
	if credential["elitea_title"] != "frozen-title" || credential["token"] != "{{secret.FROZEN}}" ||
		credential["configuration_project_id"] != json.Number("7") {
		t.Fatalf("frozen expansion changed through an alias: %#v", credential)
	}
}

func TestCurrentAuthoritativeInputResolverClassifiesSettingsFailuresWithoutDetails(t *testing.T) {
	validation := &configurationapp.CurrentToolkitSettingsValidationError{Violations: []configurationapp.CurrentToolkitSettingsViolation{{
		Field: "credential", Code: configurationapp.CurrentToolkitConfigurationNotFoundCode,
	}}}
	for name, test := range map[string]struct {
		err  error
		want error
	}{
		"invalid saved references": {err: validation, want: ErrInvalidAuthoritativeIndexInput},
		"catalog dependency":       {err: errors.New("schema registry secret detail"), want: ErrCurrentToolkitSettingsResolutionUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := newCurrentAuthoritativeResolverWithSettingsForTest(
				t,
				validCurrentToolkitReader(),
				&currentModelCatalogStub{},
				&currentToolkitSettingsValidatorStub{err: test.err},
			)
			_, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
			if !errors.Is(err, test.want) || err.Error() != test.want.Error() {
				t.Fatalf("error=%v, want safe %v", err, test.want)
			}
		})
	}
}

func TestCurrentAuthoritativeInputResolverDoesNotAliasCallerOrRepositoryValues(t *testing.T) {
	model := "model"
	request := validCurrentStartRequest(&model, `{"nested":{"value":"request"}}`)
	toolkits := validCurrentToolkitReader()
	resolver := newCurrentAuthoritativeResolverForTest(t, toolkits, &currentModelCatalogStub{})

	inputs, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolParameters[2] = 'X'
	request.RequestedLLMSettings[2] = 'X'
	*request.RequestedLLMModel = "changed"
	toolkits.toolkit.Settings["credential"] = "changed"

	toolkit, _ := decodeCurrentResolverObject(inputs.ToolkitConfiguration)
	settings := toolkit["settings"].(map[string]any)
	if settings["credential"] != "{{secret.CREDENTIAL}}" || string(inputs.ToolParameters) != `{"index_name":"docs"}` || *inputs.LLMModel != "model" {
		t.Fatalf("resolver retained an alias: toolkit=%#v params=%s model=%v", toolkit, inputs.ToolParameters, inputs.LLMModel)
	}
	llm, _ := decodeCurrentResolverObject(inputs.LLMConfiguration)
	if !reflect.DeepEqual(llm["nested"], map[string]any{"value": "request"}) {
		t.Fatalf("LLM settings changed through request alias: %#v", llm)
	}
}

func TestCurrentAuthoritativeInputResolverRejectsMissingOrInvalidSavedToolkit(t *testing.T) {
	for name, reader := range map[string]*currentToolkitReaderStub{
		"not found": {found: false},
		"wrong id": {
			found:   true,
			toolkit: CurrentToolkitSnapshot{ID: 20, Type: "github", Name: "repo", Settings: map[string]any{}},
		},
		"missing settings": {
			found:   true,
			toolkit: CurrentToolkitSnapshot{ID: 19, Type: "github", Name: "repo"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := newCurrentAuthoritativeResolverForTest(t, reader, &currentModelCatalogStub{})
			_, err := resolver.Resolve(context.Background(), validCurrentStartRequest(nil, `{}`))
			if name == "not found" {
				if !errors.Is(err, ErrToolkitNotVisible) {
					t.Fatalf("error=%v, want not visible", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidAuthoritativeIndexInput) {
				t.Fatalf("error=%v, want invalid input", err)
			}
		})
	}
}

func TestNewCurrentAuthoritativeInputResolverRejectsIncompleteComposition(t *testing.T) {
	models := &currentModelCatalogStub{}
	toolkits := validCurrentToolkitReader()
	settings := &currentToolkitSettingsValidatorStub{}
	embeddings := &currentEmbeddingBindingSelectorStub{}
	if _, err := NewCurrentAuthoritativeInputResolver(nil, models, settings, embeddings, 1); err == nil {
		t.Fatal("expected missing toolkit reader to fail")
	}
	if _, err := NewCurrentAuthoritativeInputResolver(toolkits, nil, settings, embeddings, 1); err == nil {
		t.Fatal("expected missing model catalog to fail")
	}
	if _, err := NewCurrentAuthoritativeInputResolver(toolkits, models, nil, embeddings, 1); err == nil {
		t.Fatal("expected missing settings validator to fail")
	}
	if _, err := NewCurrentAuthoritativeInputResolver(toolkits, models, settings, nil, 1); err == nil {
		t.Fatal("expected missing embedding binding selector to fail")
	}
	if _, err := NewCurrentAuthoritativeInputResolver(toolkits, models, settings, embeddings, 0); err == nil {
		t.Fatal("expected invalid public project to fail")
	}
}

func validCurrentToolkitReader() *currentToolkitReaderStub {
	return &currentToolkitReaderStub{found: true, toolkit: CurrentToolkitSnapshot{
		ID:       19,
		Type:     "github",
		Name:     "repo",
		Settings: map[string]any{"credential": "{{secret.CREDENTIAL}}"},
	}}
}

func validCurrentStartRequest(model *string, settings string) StartRequest {
	return StartRequest{
		ProjectID:            7,
		ActorUserID:          42,
		ToolkitID:            19,
		ToolParameters:       json.RawMessage(`{"index_name":"docs"}`),
		RequestedLLMModel:    model,
		RequestedLLMSettings: json.RawMessage(settings),
	}
}

func newCurrentAuthoritativeResolverForTest(
	t *testing.T,
	toolkits CurrentToolkitReader,
	models CurrentModelCatalog,
) *CurrentAuthoritativeInputResolver {
	t.Helper()
	return newCurrentAuthoritativeResolverWithSettingsForTest(
		t,
		toolkits,
		models,
		&currentToolkitSettingsValidatorStub{},
	)
}

func newCurrentAuthoritativeResolverWithSettingsForTest(
	t *testing.T,
	toolkits CurrentToolkitReader,
	models CurrentModelCatalog,
	settings CurrentToolkitSettingsValidator,
) *CurrentAuthoritativeInputResolver {
	return newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
		t,
		toolkits,
		models,
		settings,
		&currentEmbeddingBindingSelectorStub{},
	)
}

func newCurrentAuthoritativeResolverWithSettingsAndEmbeddingsForTest(
	t *testing.T,
	toolkits CurrentToolkitReader,
	models CurrentModelCatalog,
	settings CurrentToolkitSettingsValidator,
	embeddings CurrentEmbeddingBindingSelector,
) *CurrentAuthoritativeInputResolver {
	t.Helper()
	resolver, err := NewCurrentAuthoritativeInputResolver(toolkits, models, settings, embeddings, 1)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func currentEmbeddingBindingForTest(projectID int32, modelName string) EmbeddingBinding {
	return EmbeddingBinding{
		SchemaVersion:          CurrentEmbeddingBindingSchema,
		ModelName:              modelName,
		ResolvedModelGroup:     "7_" + modelName,
		Route:                  "project",
		ConfigurationProjectID: projectID,
		ConfigurationUUID:      "00000000-0000-0000-0000-000000000107",
		ConfigurationDigest:    runtimedomain.SHA256([]byte("configuration:" + modelName)),
	}
}
