package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type embeddingConfigurationReaderStub struct {
	configurations map[int32]CurrentEmbeddingConfiguration
	calls          []embeddingConfigurationCall
	err            error
}

type embeddingConfigurationCall struct {
	projectID  int32
	modelName  string
	sharedOnly bool
}

func (s *embeddingConfigurationReaderStub) FindCurrentEmbeddingConfiguration(
	_ context.Context,
	projectID int32,
	modelName string,
	sharedOnly bool,
) (CurrentEmbeddingConfiguration, bool, error) {
	s.calls = append(s.calls, embeddingConfigurationCall{
		projectID: projectID, modelName: modelName, sharedOnly: sharedOnly,
	})
	if s.err != nil {
		return CurrentEmbeddingConfiguration{}, false, s.err
	}
	configuration, found := s.configurations[projectID]
	return configuration, found, nil
}

// TestCurrentEmbeddingBindingResolverBindsTheConfigurationRowTheGatewayReads
// pins the post-LiteLLM contract. The binding always carries the plain model
// name (route "raw"), because the gateway derives the project from the edge's
// signed identity rather than from a `{projectID}_` group prefix; the
// project -> public scope decision survives, but is now visible only in the
// bound configuration identity and in the exact reader calls made.
func TestCurrentEmbeddingBindingResolverBindsTheConfigurationRowTheGatewayReads(t *testing.T) {
	projectLocal := currentEmbeddingConfiguration(
		7,
		"00000000-0000-0000-0000-000000000701",
		"embed",
		false,
	)
	publicShared := currentEmbeddingConfiguration(
		1,
		"00000000-0000-0000-0000-000000000101",
		"embed",
		true,
	)
	tests := []struct {
		name       string
		configs    map[int32]CurrentEmbeddingConfiguration
		wantCalls  []embeddingConfigurationCall
		wantConfig *CurrentEmbeddingConfiguration
	}{
		{
			// The caller's own row must win over an identically named shared
			// row: if the public project were searched first (or at all), the
			// bound uuid and digest would be the public one.
			name: "project local wins over identically named shared model",
			configs: map[int32]CurrentEmbeddingConfiguration{
				7: projectLocal,
				1: publicShared,
			},
			wantCalls:  []embeddingConfigurationCall{{projectID: 7, modelName: "embed"}},
			wantConfig: &projectLocal,
		},
		{
			// The public project is consulted only after the caller's project
			// misses, and only with sharedOnly: a private public-project row
			// must stay invisible to another tenant.
			name: "public fallback is consulted second and shared-only",
			configs: map[int32]CurrentEmbeddingConfiguration{
				1: publicShared,
			},
			wantCalls: []embeddingConfigurationCall{
				{projectID: 7, modelName: "embed"},
				{projectID: 1, modelName: "embed", sharedOnly: true},
			},
			wantConfig: &publicShared,
		},
		// A model that no row holds used to be a third case here, and it
		// expected a binding with no configuration identity. It is now a
		// refusal, and it has its own test below.
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configurations := &embeddingConfigurationReaderStub{configurations: test.configs}
			resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := resolver.Resolve(context.Background(), 7, "embed", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := EmbeddingBinding{
				SchemaVersion:      CurrentEmbeddingBindingSchema,
				ModelName:          "embed",
				ResolvedModelGroup: "embed",
				Route:              "raw",
			}
			if test.wantConfig != nil {
				digest, digestErr := currentEmbeddingConfigurationDigest(*test.wantConfig, "embed")
				if digestErr != nil {
					t.Fatal(digestErr)
				}
				want.ConfigurationProjectID = test.wantConfig.ProjectID
				want.ConfigurationUUID = test.wantConfig.UUID
				want.ConfigurationDigest = digest
			}
			if !reflect.DeepEqual(binding, want) {
				t.Fatalf("binding=%+v want=%+v", binding, want)
			}
			if !reflect.DeepEqual(configurations.calls, test.wantCalls) {
				t.Fatalf("configuration calls=%+v want=%+v", configurations.calls, test.wantCalls)
			}
			encoded, err := binding.MarshalCanonical()
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"api_key", "token", "credential_title", "deployment_id", "endpoint"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("binding leaked %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

// TestCurrentEmbeddingBindingResolverRefusesAModelNameNoRowHolds is the
// negative direction of issue #470, and it is the admission half of issue #468.
//
// The toolkit names its embedding model in `settings.embedding_model`. When
// that name matches no `p_{project}.configuration` row, the run must not start.
// It used to start: the resolver returned a valid binding with no configuration
// identity, the platform made the admission durable, and the worker then got
// 404 model_not_found from the gateway, which reads the same rows.
//
// Issue #468 produces exactly this state on a correctly built stack. Two seed
// steps write one row from two different variables. When the step that writes
// the toolkit setting runs first, the configuration row afterwards holds a
// different model name, and no row holds the name the worker sends.
//
// Two things are asserted, and the second one matters as much as the first:
//
//  1. the resolver refuses, with the unavailable sentinel the start route maps
//     to 503 and a message that names the embedding model; and
//  2. the resolver searches both scopes and then substitutes NOTHING. A silent
//     fallback to some other embedding row would give a green run against a
//     model the operator did not ask for.
func TestCurrentEmbeddingBindingResolverRefusesAModelNameNoRowHolds(t *testing.T) {
	configurations := &embeddingConfigurationReaderStub{
		configurations: map[int32]CurrentEmbeddingConfiguration{},
	}
	resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolver.Resolve(context.Background(), 7, "embed", nil)
	if !errors.Is(err, ErrCurrentEmbeddingBindingUnavailable) {
		t.Fatalf("err=%v want=%v", err, ErrCurrentEmbeddingBindingUnavailable)
	}
	if !reflect.DeepEqual(binding, EmbeddingBinding{}) {
		t.Fatalf("a refused resolution must return no binding, got %+v", binding)
	}
	// Both scopes were searched before the refusal. Refusing before the public
	// scope is read would break the shared-model case above.
	wantCalls := []embeddingConfigurationCall{
		{projectID: 7, modelName: "embed"},
		{projectID: 1, modelName: "embed", sharedOnly: true},
	}
	if !reflect.DeepEqual(configurations.calls, wantCalls) {
		t.Fatalf("configuration calls=%+v want=%+v", configurations.calls, wantCalls)
	}

	// The same refusal when the caller's project DOES hold an embedding row,
	// but under another name. This is the half that proves no substitution: a
	// resolver that fell back to "any embedding row of this project" would bind
	// this row and admit the run against a model the operator did not ask for.
	withOther := &embeddingNameAwareReaderStub{
		rows: map[string]CurrentEmbeddingConfiguration{
			"some-other-embedding": currentEmbeddingConfiguration(
				7,
				"00000000-0000-0000-0000-000000000702",
				"some-other-embedding",
				false,
			),
		},
	}
	resolverWithOther, err := NewCurrentEmbeddingBindingResolver(withOther, 1)
	if err != nil {
		t.Fatal(err)
	}
	binding, err = resolverWithOther.Resolve(context.Background(), 7, "embed", nil)
	if !errors.Is(err, ErrCurrentEmbeddingBindingUnavailable) {
		t.Fatalf("err=%v want=%v", err, ErrCurrentEmbeddingBindingUnavailable)
	}
	if binding.ModelName != "" {
		t.Fatalf("a refused resolution must name no model, got %q", binding.ModelName)
	}

	// The control: the very same reader admits the name it does hold. Without
	// this line the assertions above would also pass against a resolver that
	// refused every request.
	admitted, err := resolverWithOther.Resolve(context.Background(), 7, "some-other-embedding", nil)
	if err != nil {
		t.Fatalf("the reader's own row must still bind: %v", err)
	}
	if admitted.ModelName != "some-other-embedding" || admitted.ConfigurationUUID == "" {
		t.Fatalf("binding=%+v want the row's own name and identity", admitted)
	}
}

// embeddingNameAwareReaderStub answers by MODEL NAME, which the shared stub
// above does not: that one keys on project alone and returns its row for any
// name asked. Only a name-aware reader can express "this project holds an
// embedding row, but not this one".
type embeddingNameAwareReaderStub struct {
	rows map[string]CurrentEmbeddingConfiguration
}

func (s *embeddingNameAwareReaderStub) FindCurrentEmbeddingConfiguration(
	_ context.Context,
	_ int32,
	modelName string,
	_ bool,
) (CurrentEmbeddingConfiguration, bool, error) {
	configuration, found := s.rows[modelName]
	return configuration, found, nil
}

// TestCurrentEmbeddingBindingResolverRejectsDuplicateMutableDefinitions keeps
// the LIMIT 2 duplicate sentinel of FindCurrentEmbeddingConfigurations a
// distinct admission outcome. Collapsing it into "unavailable" would hide an
// ambiguous catalog behind a transient-looking dependency failure.
func TestCurrentEmbeddingBindingResolverRejectsDuplicateMutableDefinitions(t *testing.T) {
	configurations := &embeddingConfigurationReaderStub{
		err: ErrCurrentEmbeddingBindingAmbiguous,
	}
	resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(
		context.Background(),
		7,
		"embed",
		nil,
	); !errors.Is(err, ErrCurrentEmbeddingBindingAmbiguous) {
		t.Fatalf("duplicate embedding definitions error=%v", err)
	}
	if len(configurations.calls) != 1 {
		t.Fatalf("ambiguity did not stop the search: %+v", configurations.calls)
	}
}

func TestCurrentEmbeddingBindingResolverRequiresConfigurationReader(t *testing.T) {
	if _, err := NewCurrentEmbeddingBindingResolver(nil, 1); err == nil {
		t.Fatal("resolver composed without a configuration reader")
	}
	if _, err := NewCurrentEmbeddingBindingResolver(
		&embeddingConfigurationReaderStub{},
		0,
	); err == nil {
		t.Fatal("resolver composed without a public project")
	}
}

func TestCurrentEmbeddingBindingResolverRetainsAuthoritativeDefaultTupleWithDuplicateName(t *testing.T) {
	public := currentEmbeddingConfiguration(
		1,
		"00000000-0000-0000-0000-000000000101",
		"embed",
		true,
	)
	configurations := &embeddingConfigurationReaderStub{
		configurations: map[int32]CurrentEmbeddingConfiguration{
			7: currentEmbeddingConfiguration(
				7,
				"00000000-0000-0000-0000-000000000701",
				"embed",
				false,
			),
			1: public,
		},
	}
	resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
	if err != nil {
		t.Fatal(err)
	}
	preferredProjectID := int32(1)
	first, err := resolver.Resolve(context.Background(), 7, "embed", &preferredProjectID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background(), 7, "embed", &preferredProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("restart changed binding: first=%+v second=%+v", first, second)
	}
	if first.ModelProjectID != 1 ||
		first.ConfigurationProjectID != 1 ||
		first.ConfigurationUUID != public.UUID ||
		first.ConfigurationDigest.IsZero() {
		t.Fatalf("default tuple bound the wrong duplicate: %+v", first)
	}
	// The authoritative default tuple selected the public duplicate, and the
	// wire name stays the plain model name: the gateway resolves that project's
	// own configuration row from the edge identity, so a project-prefixed group
	// would not resolve at all.
	if first.Route != "raw" || first.ResolvedModelGroup != "embed" {
		t.Fatalf("current route drifted: %+v", first)
	}
	if !reflect.DeepEqual(configurations.calls, []embeddingConfigurationCall{
		{projectID: 1, modelName: "embed", sharedOnly: true},
		{projectID: 1, modelName: "embed", sharedOnly: true},
	}) {
		t.Fatalf("default configuration lookup=%+v", configurations.calls)
	}
}

func TestCurrentEmbeddingBindingResolverRejectsDefaultFromUnrelatedProject(t *testing.T) {
	resolver, err := NewCurrentEmbeddingBindingResolver(
		&embeddingConfigurationReaderStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedProjectID := int32(8)
	if _, err := resolver.Resolve(
		context.Background(),
		7,
		"embed",
		&unrelatedProjectID,
	); !errors.Is(err, ErrInvalidCurrentEmbeddingBinding) {
		t.Fatalf("error=%v", err)
	}
}

func TestCurrentEmbeddingBindingConfigurationChangeChangesDigest(t *testing.T) {
	first := currentEmbeddingConfiguration(
		7,
		"00000000-0000-0000-0000-000000000701",
		"embed",
		false,
	)
	second := first
	second.Data = json.RawMessage(`{"name":"embed","ai_credentials":{"elitea_title":"credential-v2","private":true}}`)

	firstDigest, err := currentEmbeddingConfigurationDigest(first, "embed")
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := currentEmbeddingConfigurationDigest(second, "embed")
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("configuration reference change retained the old digest")
	}
}

func TestCurrentEmbeddingBindingAcceptsCurrentOptionalCredentialReference(t *testing.T) {
	configuration := CurrentEmbeddingConfiguration{
		UUID:      "00000000-0000-0000-0000-000000000701",
		ProjectID: 7,
		Type:      "embedding_model",
		Section:   "embedding",
		Data:      json.RawMessage(`{"name":"external-embedding","ai_credentials":null}`),
	}
	first, err := currentEmbeddingConfigurationDigest(configuration, "external-embedding")
	if err != nil || first.IsZero() {
		t.Fatalf("optional credential reference: digest=%s err=%v", first, err)
	}
	configuration.Data = json.RawMessage(`{"name":"external-embedding"}`)
	second, err := currentEmbeddingConfigurationDigest(configuration, "external-embedding")
	if err != nil || second != first {
		t.Fatalf("missing and null optional references diverged: first=%s second=%s err=%v", first, second, err)
	}
}

func TestCurrentEmbeddingBindingResolverRedactsDependencyFailuresAndPreservesCancellation(t *testing.T) {
	resolver, err := NewCurrentEmbeddingBindingResolver(
		&embeddingConfigurationReaderStub{err: errors.New("credential-details")},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), 7, "embed", nil)
	if !errors.Is(err, ErrCurrentEmbeddingBindingUnavailable) ||
		strings.Contains(err.Error(), "credential-details") {
		t.Fatalf("error=%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = resolver.Resolve(ctx, 7, "embed", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestEmbeddingBindingValidationContract(t *testing.T) {
	valid := EmbeddingBinding{
		SchemaVersion:          CurrentEmbeddingBindingSchema,
		ModelName:              "embed",
		ResolvedModelGroup:     "1_embed",
		Route:                  "public",
		ModelProjectID:         1,
		ConfigurationProjectID: 1,
		ConfigurationUUID:      "00000000-0000-0000-0000-000000000101",
		ConfigurationDigest:    currentEmbeddingDigest("valid"),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []EmbeddingBinding{
		func() EmbeddingBinding { value := valid; value.Route = "raw"; return value }(),
		func() EmbeddingBinding { value := valid; value.ResolvedModelGroup = "embed"; return value }(),
		func() EmbeddingBinding { value := valid; value.ModelProjectID = -1; return value }(),
		func() EmbeddingBinding { value := valid; value.ConfigurationUUID = ""; return value }(),
		func() EmbeddingBinding { value := valid; value.ConfigurationProjectID = 7; return value }(),
		func() EmbeddingBinding {
			value := valid
			value.ModelName = strings.Repeat("x", maxEmbeddingBindingIdentity+1)
			return value
		}(),
	}
	for index, invalid := range tests {
		if !errors.Is(invalid.Validate(), ErrInvalidCurrentEmbeddingBinding) {
			t.Fatalf("case %d accepted: %+v", index, invalid)
		}
	}
}

func TestEmbeddingBindingValidationSharedGoPythonFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/embedding_binding_validation_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string `json:"name"`
		Valid    bool   `json:"valid"`
		Document struct {
			SchemaVersion          string `json:"schema_version"`
			ModelName              string `json:"model_name"`
			ResolvedModelGroup     string `json:"resolved_model_group"`
			Route                  string `json:"route"`
			ModelProjectID         int32  `json:"model_project_id"`
			ConfigurationProjectID int32  `json:"configuration_project_id"`
			ConfigurationUUID      string `json:"configuration_uuid"`
			ConfigurationDigest    string `json:"configuration_digest"`
		} `json:"document"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			var digest runtimedomain.Digest
			if test.Document.ConfigurationDigest != "" {
				var parseErr error
				digest, parseErr = runtimedomain.ParseDigest(test.Document.ConfigurationDigest)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
			}
			binding := EmbeddingBinding{
				SchemaVersion:          test.Document.SchemaVersion,
				ModelName:              test.Document.ModelName,
				ResolvedModelGroup:     test.Document.ResolvedModelGroup,
				Route:                  test.Document.Route,
				ModelProjectID:         test.Document.ModelProjectID,
				ConfigurationProjectID: test.Document.ConfigurationProjectID,
				ConfigurationUUID:      test.Document.ConfigurationUUID,
				ConfigurationDigest:    digest,
			}
			if got := binding.Validate() == nil; got != test.Valid {
				t.Fatalf("valid=%t want=%t binding=%+v", got, test.Valid, binding)
			}
		})
	}
}

func currentEmbeddingConfiguration(
	projectID int32,
	uuid string,
	modelName string,
	shared bool,
) CurrentEmbeddingConfiguration {
	private := !shared
	data, _ := json.Marshal(map[string]any{
		"name": modelName,
		"ai_credentials": map[string]any{
			"elitea_title": "credential-current",
			"private":      private,
		},
	})
	return CurrentEmbeddingConfiguration{
		UUID:      uuid,
		ProjectID: projectID,
		Type:      "embedding_model",
		Section:   "embedding",
		Data:      data,
		Shared:    shared,
	}
}

func currentEmbeddingDigest(value string) runtimedomain.Digest {
	return runtimedomain.SHA256([]byte(value))
}
