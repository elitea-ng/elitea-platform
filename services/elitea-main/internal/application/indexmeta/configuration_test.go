package indexmeta

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

type configurationWriterStub struct {
	target        ResolvedTarget
	indexName     string
	configuration json.RawMessage
	calls         int
	err           error
}

func (s *configurationWriterStub) SaveConfiguration(
	_ context.Context,
	target ResolvedTarget,
	indexName string,
	configuration json.RawMessage,
) error {
	s.calls++
	s.target = target
	s.indexName = indexName
	s.configuration = configuration
	return s.err
}

func newConfigurationFixture(writer *configurationWriterStub) (*ConfigurationService, *currentToolkitStub, *currentSettingsStub, error) {
	toolkits := &currentToolkitStub{
		found: true,
		toolkit: indexingapp.CurrentToolkitSnapshot{
			ID: 19, Type: "github", Settings: map[string]any{
				"pgvector_configuration": map[string]any{"elitea_title": "project-pgvector"},
				"github_configuration":   map[string]any{"token": "must-not-expand"},
			},
		},
	}
	settings := &currentSettingsStub{
		result: map[string]any{
			"pgvector_configuration": map[string]any{
				"connection_string": "postgresql://secret-canary@project/vector",
			},
		},
	}
	service, err := NewConfigurationService(toolkits, settings, writer)
	return service, toolkits, settings, err
}

func TestSaveConfigurationResolvesTheSavedTargetAndWritesTheConfiguration(t *testing.T) {
	t.Parallel()

	writer := &configurationWriterStub{}
	service, toolkits, settings, err := newConfigurationFixture(writer)
	if err != nil {
		t.Fatal(err)
	}
	configuration := json.RawMessage(`{"index_name":"docs","progress_step":75}`)
	if err := service.SaveConfiguration(context.Background(), ConfigurationRequest{
		ProjectID: 7, ActorUserID: 11, ToolkitID: 19,
		IndexName: "docs", Configuration: configuration,
	}); err != nil {
		t.Fatal(err)
	}

	if toolkits.projectID != 7 || toolkits.userID != 11 || toolkits.toolkitID != 19 {
		t.Fatalf("toolkit lookup project=%d user=%d toolkit=%d", toolkits.projectID, toolkits.userID, toolkits.toolkitID)
	}
	// Only the pgvector reference is ever expanded: the toolkit's own
	// provider credential must not be claimed by a configuration save.
	if len(settings.request.Settings) != 1 || settings.request.Settings["pgvector_configuration"] == nil {
		t.Fatalf("settings request=%+v", settings.request)
	}
	if settings.request.Mode != configurationapp.CurrentToolkitSettingsClaimMode {
		t.Fatalf("settings mode=%v", settings.request.Mode)
	}
	if writer.calls != 1 || writer.indexName != "docs" ||
		writer.target.ConnectionString != "postgresql://secret-canary@project/vector" ||
		writer.target.SchemaID != 19 {
		t.Fatalf("writer calls=%d index=%q target=%+v", writer.calls, writer.indexName, writer.target)
	}
	if string(writer.configuration) != string(configuration) {
		t.Fatalf("configuration=%s", writer.configuration)
	}
}

func TestSaveConfigurationRefusesEverythingThatIsNotABoundedJSONObject(t *testing.T) {
	t.Parallel()

	cases := map[string]json.RawMessage{
		"absent":       nil,
		"array":        json.RawMessage(`[{"a":1}]`),
		"bare string":  json.RawMessage(`"progress_step"`),
		"number":       json.RawMessage(`75`),
		"null":         json.RawMessage(`null`),
		"invalid json": json.RawMessage(`{"a":`),
		"oversized":    json.RawMessage(`{"a":"` + strings.Repeat("x", MaxCurrentIndexConfigurationBytes) + `"}`),
	}
	for name, configuration := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &configurationWriterStub{}
			service, _, _, err := newConfigurationFixture(writer)
			if err != nil {
				t.Fatal(err)
			}
			err = service.SaveConfiguration(context.Background(), ConfigurationRequest{
				ProjectID: 7, ActorUserID: 11, ToolkitID: 19,
				IndexName: "docs", Configuration: configuration,
			})
			if !errors.Is(err, ErrCurrentIndexConfigurationInvalid) {
				t.Fatalf("err=%v, want ErrCurrentIndexConfigurationInvalid", err)
			}
			// The refusal happens BEFORE the toolkit is resolved, so a
			// malformed body never claims a credential.
			if writer.calls != 0 {
				t.Fatalf("writer was called %d times for a refused configuration", writer.calls)
			}
		})
	}
}

func TestSaveConfigurationRefusesAnUnusableIdentity(t *testing.T) {
	t.Parallel()

	valid := ConfigurationRequest{ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexName: "docs", Configuration: json.RawMessage(`{}`)}
	cases := map[string]func(ConfigurationRequest) ConfigurationRequest{
		"no project":        func(r ConfigurationRequest) ConfigurationRequest { r.ProjectID = 0; return r },
		"no actor":          func(r ConfigurationRequest) ConfigurationRequest { r.ActorUserID = 0; return r },
		"no toolkit":        func(r ConfigurationRequest) ConfigurationRequest { r.ToolkitID = -1; return r },
		"no index name":     func(r ConfigurationRequest) ConfigurationRequest { r.IndexName = ""; return r },
		"newline in name":   func(r ConfigurationRequest) ConfigurationRequest { r.IndexName = "docs\nmore"; return r },
		"oversized name":    func(r ConfigurationRequest) ConfigurationRequest { r.IndexName = strings.Repeat("d", MaxCurrentIndexMetaCollectionBytes+1); return r },
		"project overflows": func(r ConfigurationRequest) ConfigurationRequest { r.ProjectID = 1 << 40; return r },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &configurationWriterStub{}
			service, _, _, err := newConfigurationFixture(writer)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.SaveConfiguration(context.Background(), mutate(valid)); !errors.Is(err, ErrInvalidCurrentIndexMetaRequest) {
				t.Fatalf("err=%v, want ErrInvalidCurrentIndexMetaRequest", err)
			}
			if writer.calls != 0 {
				t.Fatalf("writer was called %d times for a refused request", writer.calls)
			}
		})
	}
}

func TestSaveConfigurationPreservesTheNotFoundAndRedactsEverythingElse(t *testing.T) {
	t.Parallel()

	t.Run("not found travels", func(t *testing.T) {
		t.Parallel()
		writer := &configurationWriterStub{err: ErrCurrentIndexMetaNotFound}
		service, _, _, err := newConfigurationFixture(writer)
		if err != nil {
			t.Fatal(err)
		}
		err = service.SaveConfiguration(context.Background(), ConfigurationRequest{
			ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexName: "docs", Configuration: json.RawMessage(`{}`),
		})
		if !errors.Is(err, ErrCurrentIndexMetaNotFound) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("a storage failure never carries the DSN", func(t *testing.T) {
		t.Parallel()
		writer := &configurationWriterStub{
			err: errors.New(`failed to connect to "user=vector database=vector": postgresql://secret-canary@project/vector`),
		}
		service, _, _, err := newConfigurationFixture(writer)
		if err != nil {
			t.Fatal(err)
		}
		err = service.SaveConfiguration(context.Background(), ConfigurationRequest{
			ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexName: "docs", Configuration: json.RawMessage(`{}`),
		})
		if !errors.Is(err, ErrCurrentIndexConfigurationUnavailable) {
			t.Fatalf("err=%v, want ErrCurrentIndexConfigurationUnavailable", err)
		}
		if strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("the returned error leaks the connection string: %v", err)
		}
	})
}

func TestSaveConfigurationRefusesAToolkitWithoutAPgVectorTarget(t *testing.T) {
	t.Parallel()

	writer := &configurationWriterStub{}
	toolkits := &currentToolkitStub{
		found:   true,
		toolkit: indexingapp.CurrentToolkitSnapshot{ID: 19, Type: "github", Settings: map[string]any{}},
	}
	service, err := NewConfigurationService(toolkits, &currentSettingsStub{}, writer)
	if err != nil {
		t.Fatal(err)
	}
	err = service.SaveConfiguration(context.Background(), ConfigurationRequest{
		ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexName: "docs", Configuration: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrCurrentIndexMetaTargetMissing) {
		t.Fatalf("err=%v, want ErrCurrentIndexMetaTargetMissing", err)
	}
	if writer.calls != 0 {
		t.Fatalf("writer was called %d times without a resolved target", writer.calls)
	}
}

func TestNewConfigurationServiceRefusesMissingDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewConfigurationService(nil, &currentSettingsStub{}, &configurationWriterStub{}); err == nil {
		t.Fatal("a service without a toolkit reader was constructed")
	}
	if _, err := NewConfigurationService(&currentToolkitStub{}, nil, &configurationWriterStub{}); err == nil {
		t.Fatal("a service without a settings resolver was constructed")
	}
	if _, err := NewConfigurationService(&currentToolkitStub{}, &currentSettingsStub{}, nil); err == nil {
		t.Fatal("a service without an external writer was constructed")
	}
}
