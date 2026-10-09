package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type currentToolkitSchemaCatalogStub struct {
	schemas map[string]CurrentToolkitSchema
	err     error
	calls   []string
}

func (s *currentToolkitSchemaCatalogStub) FindEffectiveToolkitSchema(
	_ context.Context,
	projectID int32,
	userID int32,
	toolkitType string,
) (CurrentToolkitSchema, bool, error) {
	if projectID != 7 || userID != 42 {
		return CurrentToolkitSchema{}, false, errors.New("unexpected schema scope")
	}
	s.calls = append(s.calls, toolkitType)
	if s.err != nil {
		return CurrentToolkitSchema{}, false, s.err
	}
	schema, found := s.schemas[toolkitType]
	return schema, found, nil
}

type currentNestedToolkitReaderStub struct {
	toolkits map[int32]CurrentNestedToolkit
	err      error
	calls    []int32
}

func (s *currentNestedToolkitReaderStub) GetCurrentNestedToolkit(
	_ context.Context,
	projectID int32,
	userID int32,
	toolkitID int32,
) (CurrentNestedToolkit, bool, error) {
	if projectID != 7 || userID != 42 {
		return CurrentNestedToolkit{}, false, errors.New("unexpected toolkit scope")
	}
	s.calls = append(s.calls, toolkitID)
	if s.err != nil {
		return CurrentNestedToolkit{}, false, s.err
	}
	toolkit, found := s.toolkits[toolkitID]
	return toolkit, found, nil
}

type currentConfigurationExpanderStub struct {
	fn    func(CurrentExpansionRequest) (map[string]any, error)
	calls []CurrentExpansionRequest
}

func (s *currentConfigurationExpanderStub) Expand(
	_ context.Context,
	request CurrentExpansionRequest,
) (map[string]any, error) {
	s.calls = append(s.calls, CurrentExpansionRequest{
		Payload:          cloneCurrentJSONObject(request.Payload),
		CurrentProjectID: request.CurrentProjectID,
		UserID:           request.UserID,
		Unsecret:         request.Unsecret,
	})
	if s.fn == nil {
		return cloneCurrentJSONObject(request.Payload), nil
	}
	return s.fn(request)
}

type currentModelVisibilityStub struct {
	visible bool
	err     error
	calls   []currentModelVisibilityCall
}

type currentModelVisibilityCall struct {
	section string
	name    string
}

func (s *currentModelVisibilityStub) IsCurrentModelVisible(
	_ context.Context,
	projectID int32,
	section string,
	name string,
) (bool, error) {
	if projectID != 7 {
		return false, errors.New("unexpected model scope")
	}
	s.calls = append(s.calls, currentModelVisibilityCall{section: section, name: name})
	return s.visible, s.err
}

type currentToolkitUnsecreterStub struct {
	fn    func(map[string]any) (map[string]any, error)
	calls []map[string]any
}

func (s *currentToolkitUnsecreterStub) Unsecret(
	_ context.Context,
	projectID int32,
	data map[string]any,
) (map[string]any, error) {
	if projectID != 7 {
		return nil, errors.New("unexpected vault scope")
	}
	s.calls = append(s.calls, cloneCurrentJSONObject(data))
	if s.fn == nil {
		return cloneCurrentJSONObject(data), nil
	}
	return s.fn(data)
}

func TestCurrentToolkitSettingsResolverPortsSchemaDrivenExpansion(t *testing.T) {
	createdAt := "2026-07-22T10:11:12+00:00"
	authorID := int32(55)
	nestedSource := map[string]any{
		"nested_credential": map[string]any{"elitea_title": "nested-title", "private": false},
	}
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		"root": {Properties: map[string]any{
			"credential": map[string]any{
				"configuration_types": []any{"root-config"},
				"toolkit_types":       []any{"must-not-run"},
				"secret":              true,
				"configuration_model": "must-not-run",
			},
			"related":         map[string]any{"toolkit_types": []any{"nested"}},
			"api_key":         map[string]any{"secret": true},
			"embedding_model": map[string]any{"configuration_model": "embedding"},
			"plain":           map[string]any{},
		}},
		"nested": {Properties: map[string]any{
			"nested_credential": map[string]any{"configuration_types": []any{"nested-config"}},
		}},
	}}
	toolkits := &currentNestedToolkitReaderStub{toolkits: map[int32]CurrentNestedToolkit{
		9: {
			ID: 9, ToolkitName: "nested_name", Type: "nested",
			Settings: nestedSource, AuthorID: &authorID, CreatedAt: &createdAt,
		},
	}}
	configurations := &currentConfigurationExpanderStub{fn: func(request CurrentExpansionRequest) (map[string]any, error) {
		title, _ := request.Payload["elitea_title"].(string)
		typeByTitle := map[string]string{"root-title": "root-config", "nested-title": "nested-config"}
		return map[string]any{
			"configuration_type": typeByTitle[title],
			"expanded":           title,
			"sealed":             "{{secret.CONFIG}}",
		}, nil
	}}
	models := &currentModelVisibilityStub{visible: true}
	unsecreter := &currentToolkitUnsecreterStub{fn: func(data map[string]any) (map[string]any, error) {
		if !reflect.DeepEqual(data, map[string]any{"api_key": "{{secret.API_KEY}}"}) {
			return nil, errors.New("unexpected secret selection")
		}
		return map[string]any{"api_key": "redeemed-api-key"}, nil
	}}
	resolver := newCurrentToolkitSettingsResolverForTest(t, schemas, toolkits, configurations, models, unsecreter)
	source := map[string]any{
		"credential":      map[string]any{"elitea_title": "root-title", "private": false},
		"related":         json.Number("9"),
		"api_key":         "{{secret.API_KEY}}",
		"embedding_model": "visible-model",
		"plain":           map[string]any{"unchanged": true},
	}
	original := cloneCurrentJSONObject(source)

	result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: "root",
		Settings:    source,
		ProjectID:   7,
		UserID:      42,
		Mode:        CurrentToolkitSettingsClaimMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, original) || !reflect.DeepEqual(nestedSource, map[string]any{
		"nested_credential": map[string]any{"elitea_title": "nested-title", "private": false},
	}) {
		t.Fatalf("resolver mutated an input: source=%#v nested=%#v", source, nestedSource)
	}
	if len(configurations.calls) != 2 || !configurations.calls[0].Unsecret || !configurations.calls[1].Unsecret {
		t.Fatalf("configuration calls=%#v, want two claim-time expansions", configurations.calls)
	}
	for _, call := range configurations.calls {
		if call.CurrentProjectID != 7 || call.UserID == nil || *call.UserID != 42 {
			t.Fatalf("configuration call scope=%#v", call)
		}
	}
	if !reflect.DeepEqual(schemas.calls, []string{"root", "nested"}) || !reflect.DeepEqual(toolkits.calls, []int32{9}) {
		t.Fatalf("schema calls=%v toolkit calls=%v", schemas.calls, toolkits.calls)
	}
	if !reflect.DeepEqual(models.calls, []currentModelVisibilityCall{{section: "embedding", name: "visible-model"}}) {
		t.Fatalf("model calls=%#v", models.calls)
	}
	if len(unsecreter.calls) != 1 || result["api_key"] != "redeemed-api-key" {
		t.Fatalf("unsecret calls=%#v result=%#v", unsecreter.calls, result)
	}

	credential := result["credential"].(map[string]any)
	if credential["configuration_type"] != "root-config" || credential["sealed"] != "{{secret.CONFIG}}" {
		t.Fatalf("credential=%#v", credential)
	}
	related := result["related"].(map[string]any)
	if related["id"] != int32(9) || related["toolkit_name"] != "nested_name" || related["type"] != "nested" ||
		related["author_id"] != authorID || related["created_at"] != createdAt {
		t.Fatalf("nested identity=%#v", related)
	}
	nestedSettings := related["settings"].(map[string]any)
	if nestedSettings["nested_credential"].(map[string]any)["configuration_type"] != "nested-config" {
		t.Fatalf("nested settings=%#v", nestedSettings)
	}
}

func TestCurrentToolkitSettingsResolverKeepsSecretsSealedOutsideClaimMode(t *testing.T) {
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		"root": {Properties: map[string]any{
			"credential": map[string]any{"configuration_sections": []any{"credentials"}},
			"optional":   map[string]any{"configuration_sections": []any{"credentials"}},
			"token":      map[string]any{"secret": true},
		}},
	}}
	configurations := &currentConfigurationExpanderStub{fn: func(request CurrentExpansionRequest) (map[string]any, error) {
		return map[string]any{"configuration_type": "any", "token": "{{secret.CONFIG}}"}, nil
	}}
	unsecreter := &currentToolkitUnsecreterStub{fn: func(map[string]any) (map[string]any, error) {
		return nil, errors.New("must not be called")
	}}
	resolver := newCurrentToolkitSettingsResolverForTest(
		t,
		schemas,
		&currentNestedToolkitReaderStub{},
		configurations,
		&currentModelVisibilityStub{},
		unsecreter,
	)

	result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: "root",
		Settings: map[string]any{
			"credential": map[string]any{"elitea_title": "title", "private": false},
			"token":      "{{secret.TOKEN}}",
		},
		ProjectID: 7,
		UserID:    42,
		Mode:      CurrentToolkitSettingsReferenceMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(configurations.calls) != 1 || configurations.calls[0].Unsecret || len(unsecreter.calls) != 0 {
		t.Fatalf("configuration calls=%#v unsecret calls=%#v", configurations.calls, unsecreter.calls)
	}
	if result["token"] != "{{secret.TOKEN}}" {
		t.Fatalf("token=%#v", result["token"])
	}
	credential := result["credential"].(map[string]any)
	if credential[CurrentFrozenConfigurationMarker] != true {
		t.Fatalf("configuration admission marker missing: %#v", credential)
	}
	// An absent configuration field is normalized to the EMPTY OBJECT, not to
	// null. It used to be null, and the native worker refuses a non-object
	// there — see the note in resolveConfigurationField for the two files that
	// measured it. The type assertion is the assertion: `nil` is not a
	// `map[string]any`, so a regression fails here rather than reading as an
	// empty map.
	optional, exists := result["optional"]
	if !exists {
		t.Fatalf("missing configuration field was dropped instead of normalized: %#v", result)
	}
	empty, ok := optional.(map[string]any)
	if !ok || len(empty) != 0 {
		t.Fatalf("missing configuration field = %#v, want an empty object", optional)
	}
	// No marker: nothing was expanded, so the claim-time materializer must read
	// it as an ordinary map and not as a configuration it has to own.
	if _, marked := empty[CurrentFrozenConfigurationMarker]; marked {
		t.Fatalf("the empty configuration carries the frozen marker: %#v", empty)
	}
}

// The freeze shape the native worker's OpenAPI family actually admits.
//
// The `openapi` toolkit is the one built-in whose credential is genuinely
// optional: an anonymous API needs none, and the create form saves none unless
// the user picks one, so this is the shape EVERY toolkit that form produces
// arrives in. `merged_auth_settings`
// (libs/rust/agent-runtime/src/toolkits/families/openapi/config.rs) reads
// the field with `as_object()` and refuses anything else, which ended the whole
// turn with `native_agent.invalid_configuration` while the settings beside it —
// spec and selected_tools — were correct and untouched.
//
// Both modes are exercised: the worker reads the CLAIM freeze, and admission
// reads the REFERENCE one, so a fix that only reached one of them would leave
// the turn refused or the toolkit unsavable.
func TestCurrentToolkitSettingsResolverFreezesAnAbsentOpenApiCredentialAsAnEmptyObject(t *testing.T) {
	for _, mode := range []CurrentToolkitSettingsMode{
		CurrentToolkitSettingsReferenceMode,
		CurrentToolkitSettingsClaimMode,
	} {
		t.Run(string(mode), func(t *testing.T) {
			schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
				// The pinned SDK snapshot's whole `openapi` properties block
				// (internal/runtimecomposition/current_toolkit_schema_snapshot.json).
				"openapi": {Properties: map[string]any{
					"openapi_configuration": map[string]any{
						"configuration_types": []any{"openapi"},
					},
				}},
			}}
			configurations := &currentConfigurationExpanderStub{fn: func(CurrentExpansionRequest) (map[string]any, error) {
				return nil, errors.New("no configuration is referenced, so nothing may be expanded")
			}}
			resolver := newCurrentToolkitSettingsResolverForTest(
				t,
				schemas,
				&currentNestedToolkitReaderStub{},
				configurations,
				&currentModelVisibilityStub{},
				&currentToolkitUnsecreterStub{},
			)

			result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
				ToolkitType: "openapi",
				// Exactly what the create form stores today: a specification, a
				// selection, and no credential key at all.
				Settings: map[string]any{
					"spec":           "openapi: 3.0.0\npaths: {}\n",
					"selected_tools": []any{"readStatus"},
				},
				ProjectID: 7,
				UserID:    42,
				Mode:      mode,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(configurations.calls) != 0 {
				t.Fatalf("an absent credential must not be expanded: %#v", configurations.calls)
			}
			configuration, exists := result["openapi_configuration"]
			if !exists {
				t.Fatalf("openapi_configuration was not frozen at all: %#v", result)
			}
			// `as_object()` on the worker side. nil fails it; an empty map is
			// merged and leaves parse_auth at OpenApiAuth::Anonymous.
			empty, ok := configuration.(map[string]any)
			if !ok || len(empty) != 0 {
				t.Fatalf("openapi_configuration = %#v, want an empty object", configuration)
			}
			if result["spec"] != "openapi: 3.0.0\npaths: {}\n" {
				t.Fatalf("the specification did not survive the freeze: %#v", result["spec"])
			}
		})
	}
}

func TestCurrentToolkitSettingsResolverRejectsUnmigratedPlaintextSecretsInReferenceMode(t *testing.T) {
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		"root": {Properties: map[string]any{
			"api_key": map[string]any{"secret": true},
		}},
	}}
	unsecreter := &currentToolkitUnsecreterStub{}
	resolver := newCurrentToolkitSettingsResolverForTest(
		t,
		schemas,
		&currentNestedToolkitReaderStub{},
		&currentConfigurationExpanderStub{},
		&currentModelVisibilityStub{},
		unsecreter,
	)

	result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: "root",
		Settings:    map[string]any{"api_key": "plaintext-that-must-not-be-frozen"},
		ProjectID:   7,
		UserID:      42,
		Mode:        CurrentToolkitSettingsReferenceMode,
	})
	if result != nil {
		t.Fatalf("result=%#v, want no partially frozen settings", result)
	}
	var validationErr *CurrentToolkitSettingsValidationError
	if !errors.As(err, &validationErr) || !reflect.DeepEqual(validationErr.Violations, []CurrentToolkitSettingsViolation{{
		Field: "api_key", Code: CurrentToolkitSecretNotSealedCode,
	}}) {
		t.Fatalf("error=%v violations=%#v", err, validationErr)
	}
	if strings.Contains(err.Error(), "plaintext-that-must-not-be-frozen") || len(unsecreter.calls) != 0 {
		t.Fatalf("unsafe error or claim-time redemption: error=%v calls=%#v", err, unsecreter.calls)
	}
}

func TestCurrentToolkitSettingsResolverAggregatesSafeFieldViolations(t *testing.T) {
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		"root": {Properties: map[string]any{
			"bad_credential": map[string]any{"configuration_types": []any{"expected-private-type"}},
			"credential":     map[string]any{"configuration_types": []any{"expected-private-type"}},
			"model":          map[string]any{"configuration_model": "private-section"},
			"references":     map[string]any{"toolkit_types": []any{"private-toolkit"}},
		}},
	}}
	configurations := &currentConfigurationExpanderStub{fn: func(CurrentExpansionRequest) (map[string]any, error) {
		return map[string]any{"configuration_type": "actual-private-type"}, nil
	}}
	models := &currentModelVisibilityStub{visible: false}
	resolver := newCurrentToolkitSettingsResolverForTest(
		t,
		schemas,
		&currentNestedToolkitReaderStub{toolkits: map[int32]CurrentNestedToolkit{}},
		configurations,
		models,
		&currentToolkitUnsecreterStub{},
	)

	result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: "root",
		Settings: map[string]any{
			"bad_credential": map[string]any{"elitea_title": "private-title", "private": false, "extra": true},
			"credential":     map[string]any{"elitea_title": "private-title", "private": false},
			"model":          "private-model-name",
			"references":     []any{"private-id", int32(404)},
		},
		ProjectID: 7,
		UserID:    42,
		Mode:      CurrentToolkitSettingsClaimMode,
	})
	if result != nil || !errors.Is(err, ErrCurrentToolkitSettingsValidation) {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	var validationErr *CurrentToolkitSettingsValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error=%T, want CurrentToolkitSettingsValidationError", err)
	}
	want := []CurrentToolkitSettingsViolation{
		{Field: "bad_credential", Code: CurrentToolkitConfigurationReferenceInvalidCode},
		{Field: "credential", Code: CurrentToolkitConfigurationTypeMismatchCode},
		{Field: "references[0]", Code: CurrentToolkitReferenceInvalidCode},
		{Field: "references[1]", Code: CurrentToolkitReferenceNotFoundCode},
		{Field: "model", Code: CurrentToolkitModelNotFoundCode},
	}
	if !reflect.DeepEqual(validationErr.Violations, want) {
		t.Fatalf("violations=%#v, want %#v", validationErr.Violations, want)
	}
	for _, sensitive := range []string{"private-title", "private-model-name", "private-section", "expected-private-type", "actual-private-type", "404"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("safe error leaked %q: %v", sensitive, err)
		}
	}
}

func TestCurrentToolkitSettingsResolverSupportsNestedScalarListAndExpandedValues(t *testing.T) {
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		"root":  {Properties: map[string]any{"references": map[string]any{"toolkit_types": []any{"child"}}}},
		"child": {Properties: map[string]any{}},
	}}
	toolkits := &currentNestedToolkitReaderStub{toolkits: map[int32]CurrentNestedToolkit{
		9:  {ID: 9, ToolkitName: "nine", Type: "schema-unavailable", Settings: map[string]any{"sealed": "{{secret.NINE}}"}},
		10: {ID: 10, ToolkitName: "ten", Type: "child", Settings: map[string]any{}},
	}}
	resolver := newCurrentToolkitSettingsResolverForTest(
		t,
		schemas,
		toolkits,
		&currentConfigurationExpanderStub{},
		&currentModelVisibilityStub{},
		&currentToolkitUnsecreterStub{},
	)
	alreadyExpanded := map[string]any{"id": json.Number("77"), "type": "existing"}
	source := map[string]any{
		"references": []any{json.Number("9"), []any{float64(10)}, alreadyExpanded, nil, ""},
	}

	result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: "root", Settings: source, ProjectID: 7, UserID: 42,
		Mode: CurrentToolkitSettingsReferenceMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	references := result["references"].([]any)
	nine := references[0].(map[string]any)
	if nine["id"] != int32(9) || nine["settings"].(map[string]any)["sealed"] != "{{secret.NINE}}" {
		t.Fatalf("schema-fallback toolkit=%#v", nine)
	}
	ten := references[1].([]any)[0].(map[string]any)
	if ten["id"] != int32(10) || ten["toolkit_name"] != "ten" {
		t.Fatalf("nested-list toolkit=%#v", ten)
	}
	if !reflect.DeepEqual(references[2], alreadyExpanded) || references[3] != nil || references[4] != "" {
		t.Fatalf("preserved references=%#v", references)
	}
	alreadyExpanded["type"] = "mutated-after-resolve"
	if references[2].(map[string]any)["type"] != "existing" {
		t.Fatal("result aliases source expanded toolkit map")
	}
	if !reflect.DeepEqual(toolkits.calls, []int32{9, 10}) {
		t.Fatalf("toolkit calls=%v", toolkits.calls)
	}
}

func TestCurrentToolkitSettingsResolverPreservesCancellationAndRedactsDependencies(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, &currentNestedToolkitReaderStub{}, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := resolver.Resolve(ctx, validCurrentToolkitSettingsRequest(map[string]any{}))
		if !errors.Is(err, context.Canceled) || len(schemas.calls) != 0 {
			t.Fatalf("error=%v schema calls=%v", err, schemas.calls)
		}
	})

	t.Run("catalog failure", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{err: errors.New("sensitive catalog detail")}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, &currentNestedToolkitReaderStub{}, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{}))
		if !errors.Is(err, ErrCurrentToolkitSettingsDependency) || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("unsecreter failure", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
			"root": {Properties: map[string]any{"token": map[string]any{"secret": true}}},
		}}
		unsecreter := &currentToolkitUnsecreterStub{fn: func(map[string]any) (map[string]any, error) {
			return nil, errors.New("raw vault detail")
		}}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, &currentNestedToolkitReaderStub{}, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, unsecreter,
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{
			"token": "{{secret.PRIVATE}}",
		}))
		if !errors.Is(err, ErrCurrentToolkitSettingsDependency) || strings.Contains(err.Error(), "vault") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("dependency deadline", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{err: context.DeadlineExceeded}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, &currentNestedToolkitReaderStub{}, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{}))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCurrentToolkitSettingsResolverBoundsInputAndToolkitRecursion(t *testing.T) {
	t.Run("reserved frozen marker", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, &currentNestedToolkitReaderStub{}, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{
			CurrentFrozenConfigurationMarker: true,
		}))
		if !errors.Is(err, ErrInvalidCurrentToolkitSettings) || len(schemas.calls) != 0 {
			t.Fatalf("error=%v schema calls=%v", err, schemas.calls)
		}
	})

	t.Run("string bytes", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, &currentNestedToolkitReaderStub{}, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{
			"value": strings.Repeat("x", MaxCurrentToolkitSettingsStringBytes+1),
		}))
		if !errors.Is(err, ErrInvalidCurrentToolkitSettings) || len(schemas.calls) != 0 {
			t.Fatalf("error=%v schema calls=%v", err, schemas.calls)
		}
	})

	t.Run("nodes", func(t *testing.T) {
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, &currentToolkitSchemaCatalogStub{}, &currentNestedToolkitReaderStub{},
			&currentConfigurationExpanderStub{}, &currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{
			"values": make([]any, MaxCurrentToolkitSettingsNodes),
		}))
		if !errors.Is(err, ErrInvalidCurrentToolkitSettings) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("nested toolkit cycle", func(t *testing.T) {
		schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
			"root":  {Properties: map[string]any{"peer": map[string]any{"toolkit_types": []any{"child"}}}},
			"child": {Properties: map[string]any{"peer": map[string]any{"toolkit_types": []any{"child"}}}},
		}}
		toolkits := &currentNestedToolkitReaderStub{toolkits: map[int32]CurrentNestedToolkit{
			9: {ID: 9, ToolkitName: "cycle", Type: "child", Settings: map[string]any{"peer": int32(9)}},
		}}
		resolver := newCurrentToolkitSettingsResolverForTest(
			t, schemas, toolkits, &currentConfigurationExpanderStub{},
			&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
		)
		_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{"peer": int32(9)}))
		var validationErr *CurrentToolkitSettingsValidationError
		if !errors.As(err, &validationErr) || len(validationErr.Violations) != 1 ||
			validationErr.Violations[0].Field != "peer.settings.peer" ||
			validationErr.Violations[0].Code != CurrentToolkitReferenceRecursionCode {
			t.Fatalf("error=%v violations=%#v", err, validationErr)
		}
	})
}

func TestCurrentToolkitSettingsResolverRejectsMissingSchemaAndIncompleteComposition(t *testing.T) {
	dependencies := currentToolkitSettingsTestDependencies()
	resolver := newCurrentToolkitSettingsResolverForTest(
		t, dependencies.schemas, dependencies.toolkits, dependencies.configurations, dependencies.models, dependencies.unsecreter,
	)
	_, err := resolver.Resolve(context.Background(), validCurrentToolkitSettingsRequest(map[string]any{}))
	if !errors.Is(err, ErrCurrentToolkitSchemaNotFound) {
		t.Fatalf("error=%v", err)
	}

	if _, err := NewCurrentToolkitSettingsResolver(nil, dependencies.toolkits, dependencies.configurations, dependencies.models, dependencies.unsecreter); err == nil {
		t.Fatal("expected missing schema catalog to fail")
	}
	if _, err := NewCurrentToolkitSettingsResolver(dependencies.schemas, nil, dependencies.configurations, dependencies.models, dependencies.unsecreter); err == nil {
		t.Fatal("expected missing toolkit reader to fail")
	}
	if _, err := NewCurrentToolkitSettingsResolver(dependencies.schemas, dependencies.toolkits, nil, dependencies.models, dependencies.unsecreter); err == nil {
		t.Fatal("expected missing configuration expander to fail")
	}
	if _, err := NewCurrentToolkitSettingsResolver(dependencies.schemas, dependencies.toolkits, dependencies.configurations, nil, dependencies.unsecreter); err == nil {
		t.Fatal("expected missing model visibility to fail")
	}
	if _, err := NewCurrentToolkitSettingsResolver(dependencies.schemas, dependencies.toolkits, dependencies.configurations, dependencies.models, nil); err == nil {
		t.Fatal("expected missing unsecreter to fail")
	}
}

type currentToolkitSettingsTestDependencySet struct {
	schemas        *currentToolkitSchemaCatalogStub
	toolkits       *currentNestedToolkitReaderStub
	configurations *currentConfigurationExpanderStub
	models         *currentModelVisibilityStub
	unsecreter     *currentToolkitUnsecreterStub
}

func currentToolkitSettingsTestDependencies() currentToolkitSettingsTestDependencySet {
	return currentToolkitSettingsTestDependencySet{
		schemas:        &currentToolkitSchemaCatalogStub{},
		toolkits:       &currentNestedToolkitReaderStub{},
		configurations: &currentConfigurationExpanderStub{},
		models:         &currentModelVisibilityStub{},
		unsecreter:     &currentToolkitUnsecreterStub{},
	}
}

func newCurrentToolkitSettingsResolverForTest(
	t *testing.T,
	schemas CurrentToolkitSchemaCatalog,
	toolkits CurrentNestedToolkitReader,
	configurations CurrentConfigurationExpander,
	models CurrentModelVisibility,
	unsecreter CurrentExpansionUnsecreter,
) *CurrentToolkitSettingsResolver {
	t.Helper()
	resolver, err := NewCurrentToolkitSettingsResolver(schemas, toolkits, configurations, models, unsecreter)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func validCurrentToolkitSettingsRequest(settings map[string]any) CurrentToolkitSettingsRequest {
	return CurrentToolkitSettingsRequest{
		ToolkitType: "root",
		Settings:    settings,
		ProjectID:   7,
		UserID:      42,
		Mode:        CurrentToolkitSettingsClaimMode,
	}
}
