package configurations

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCurrentExpansionUsesOneProviderNeutralPath(t *testing.T) {
	tests := []struct {
		name      string
		typeName  string
		titleKey  string
		title     string
		dataField string
	}{
		{name: "github", typeName: "github", titleKey: "elitea_title", title: "github-team", dataField: "token"},
		{name: "openapi", typeName: "openapi", titleKey: "elitea_title", title: "payments-api", dataField: "spec"},
		{name: "custom provider and alita fallback", typeName: "company_custom", titleKey: "alita_title", title: "internal", dataField: "arbitrary"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 7, title: test.title}: {
					UUID:      "33333333-3333-3333-3333-333333333333",
					ProjectID: 7,
					Type:      test.typeName,
					Data: map[string]any{
						test.dataField: "provider-owned-shape",
						"nested":       map[string]any{"enabled": true},
					},
				},
			}}
			unsecreter := &currentExpansionUnsecreterStub{}
			service := newCurrentExpansionTestService(t, finder, unsecreter)
			payload := map[string]any{
				test.titleKey: test.title,
				"private":     false,
				"input":       "preserved",
			}

			result, err := service.Expand(context.Background(), CurrentExpansionRequest{
				Payload: payload, CurrentProjectID: 7,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result[test.dataField] != "provider-owned-shape" ||
				result["configuration_type"] != test.typeName ||
				result["configuration_project_id"] != int32(7) ||
				result["input"] != "preserved" {
				t.Fatalf("unexpected provider-neutral result: %#v", result)
			}
			if len(finder.calls) != 1 || finder.calls[0] != (currentExpansionLookup{projectID: 7, title: test.title}) {
				t.Fatalf("lookup path differed by provider: %#v", finder.calls)
			}
			if len(unsecreter.calls) != 0 {
				t.Fatalf("raw expansion unexpectedly unsecreted data: %#v", unsecreter.calls)
			}

			result["nested"].(map[string]any)["enabled"] = false
			if finder.rows[currentExpansionLookup{projectID: 7, title: test.title}].Data["nested"].(map[string]any)["enabled"] != true {
				t.Fatal("result aliases repository-owned provider data")
			}
			if !reflect.DeepEqual(payload, map[string]any{test.titleKey: test.title, "private": false, "input": "preserved"}) {
				t.Fatalf("caller payload was modified: %#v", payload)
			}
		})
	}
}

func TestCurrentExpansionPrefersEliteaTitleOverAlitaTitle(t *testing.T) {
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "elitea"}: expansionConfiguration(7, "company_custom", "elitea"),
	}}
	service := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})

	result, err := service.Expand(context.Background(), CurrentExpansionRequest{
		Payload: map[string]any{
			"elitea_title": "elitea",
			"alita_title":  "alita",
		},
		CurrentProjectID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["value"] != "elitea" || len(finder.calls) != 1 || finder.calls[0].title != "elitea" {
		t.Fatalf("result=%#v calls=%#v", result, finder.calls)
	}
}

func TestCurrentExpansionPreservesProjectPrecedence(t *testing.T) {
	userID := int32(44)
	tests := []struct {
		name            string
		payload         map[string]any
		rows            map[currentExpansionLookup]CurrentExpansionConfiguration
		wantProjectID   int32
		wantValue       string
		wantCalls       []currentExpansionLookup
		wantPersonalUse bool
	}{
		{
			name:    "current project wins over public shared",
			payload: map[string]any{"elitea_title": "credential"},
			rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 7, title: "credential"}:                   expansionConfiguration(7, "github", "current"),
				{projectID: 1, title: "credential", sharedOnly: true}: expansionConfiguration(1, "github", "public"),
			},
			wantProjectID: 7,
			wantValue:     "current",
			wantCalls:     []currentExpansionLookup{{projectID: 7, title: "credential"}},
		},
		{
			name:    "missing current project uses public shared only",
			payload: map[string]any{"elitea_title": "credential"},
			rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 1, title: "credential", sharedOnly: true}: expansionConfiguration(1, "github", "public"),
			},
			wantProjectID: 1,
			wantValue:     "public",
			wantCalls: []currentExpansionLookup{
				{projectID: 7, title: "credential"},
				{projectID: 1, title: "credential", sharedOnly: true},
			},
		},
		{
			name:    "personal project wins over public shared",
			payload: map[string]any{"elitea_title": "credential", "private": true},
			rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 99, title: "credential"}:                  expansionConfiguration(99, "github", "personal"),
				{projectID: 1, title: "credential", sharedOnly: true}: expansionConfiguration(1, "github", "public"),
			},
			wantProjectID:   99,
			wantValue:       "personal",
			wantCalls:       []currentExpansionLookup{{projectID: 99, title: "credential"}},
			wantPersonalUse: true,
		},
		{
			name:    "missing personal project uses public shared only",
			payload: map[string]any{"elitea_title": "credential", "private": true},
			rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 1, title: "credential", sharedOnly: true}: expansionConfiguration(1, "github", "public"),
			},
			wantProjectID: 1,
			wantValue:     "public",
			wantCalls: []currentExpansionLookup{
				{projectID: 99, title: "credential"},
				{projectID: 1, title: "credential", sharedOnly: true},
			},
			wantPersonalUse: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := &currentExpansionScopeStub{publicID: 1, personalID: 99}
			finder := &currentExpansionFinderStub{rows: test.rows}
			service, err := NewCurrentExpansionService(scope, finder, &currentExpansionUnsecreterStub{})
			if err != nil {
				t.Fatal(err)
			}
			request := CurrentExpansionRequest{Payload: test.payload, CurrentProjectID: 7}
			if test.wantPersonalUse {
				request.UserID = &userID
			}

			result, err := service.Expand(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result["configuration_project_id"] != test.wantProjectID || result["value"] != test.wantValue {
				t.Fatalf("result=%#v", result)
			}
			if !reflect.DeepEqual(finder.calls, test.wantCalls) {
				t.Fatalf("calls=%#v want=%#v", finder.calls, test.wantCalls)
			}
			if (scope.personalCalls != 0) != test.wantPersonalUse {
				t.Fatalf("personal scope calls=%d", scope.personalCalls)
			}
		})
	}
}

// The pgvector configuration type is retired: a stored row is kept but never
// read, so a reference to one expands to nothing, exactly like a missing row,
// and its secrets are never redeemed. Own project and public shared alike.
func TestCurrentExpansionNeverExpandsTheRetiredPgvectorType(t *testing.T) {
	userID := int32(44)
	for name, test := range map[string]struct {
		rows    map[currentExpansionLookup]CurrentExpansionConfiguration
		payload map[string]any
	}{
		"own project": {
			rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 7, title: "project-vector"}: expansionConfiguration(7, "pgvector", "project"),
			},
			payload: map[string]any{"elitea_title": "project-vector", "private": false},
		},
		"public shared": {
			rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
				{projectID: 1, title: "public-vector", sharedOnly: true}: expansionConfiguration(1, "pgvector", "public"),
			},
			payload: map[string]any{"elitea_title": "public-vector", "private": false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			unsecreter := &currentExpansionUnsecreterStub{}
			service := newCurrentExpansionTestService(t, &currentExpansionFinderStub{rows: test.rows}, unsecreter)
			result, err := service.Expand(context.Background(), CurrentExpansionRequest{
				Payload:          test.payload,
				CurrentProjectID: 7,
				UserID:           &userID,
				Unsecret:         true,
			})
			if result != nil || !errors.Is(err, ErrCurrentExpansionNotFound) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			assertCurrentExpansionError(t, err, CurrentExpansionNotFoundCode)
			if len(unsecreter.calls) != 0 {
				t.Fatalf("a retired row's secrets were redeemed: %#v", unsecreter.calls)
			}
		})
	}
}

func TestCurrentExpansionRecursesOnlyThroughDictionaryValues(t *testing.T) {
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "github"}: {
			UUID: "11111111-1111-1111-1111-111111111111", ProjectID: 7, Type: "github",
			Data: map[string]any{"child": map[string]any{"alita_title": "openapi"}},
		},
		{projectID: 7, title: "openapi"}: expansionConfiguration(7, "openapi", "nested"),
		{projectID: 7, title: "custom"}:  expansionConfiguration(7, "company_custom", "sibling"),
	}}
	service := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})
	payload := map[string]any{
		"top":     map[string]any{"elitea_title": "github"},
		"sibling": map[string]any{"elitea_title": "custom"},
		"array": []any{
			map[string]any{"elitea_title": "must-not-expand"},
		},
	}

	result, err := service.Expand(context.Background(), CurrentExpansionRequest{Payload: payload, CurrentProjectID: 7})
	if err != nil {
		t.Fatal(err)
	}
	child := result["top"].(map[string]any)["child"].(map[string]any)
	if child["configuration_type"] != "openapi" || child["value"] != "nested" {
		t.Fatalf("nested result=%#v", child)
	}
	if result["sibling"].(map[string]any)["configuration_type"] != "company_custom" {
		t.Fatalf("sibling result=%#v", result["sibling"])
	}
	arrayItem := result["array"].([]any)[0].(map[string]any)
	if _, expanded := arrayItem["configuration_uuid"]; expanded {
		t.Fatalf("array-contained dictionary was expanded: %#v", arrayItem)
	}
	for _, call := range finder.calls {
		if call.title == "must-not-expand" {
			t.Fatalf("array-contained reference reached finder: %#v", finder.calls)
		}
	}
}

func TestCurrentExpansionRejectsRepeatedTitleAcrossWholePayload(t *testing.T) {
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "repeated-sensitive-title"}: expansionConfiguration(7, "github", "ok"),
	}}
	service := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})
	payload := map[string]any{
		"first":  map[string]any{"elitea_title": "repeated-sensitive-title"},
		"second": map[string]any{"elitea_title": "repeated-sensitive-title"},
	}
	expected := cloneCurrentJSONObject(payload)

	result, err := service.Expand(context.Background(), CurrentExpansionRequest{Payload: payload, CurrentProjectID: 7})
	if result != nil || !errors.Is(err, ErrCurrentExpansionRecursion) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertCurrentExpansionError(t, err, CurrentExpansionRecursionCode)
	if len(finder.calls) != 1 {
		t.Fatalf("repeated title performed %d lookups", len(finder.calls))
	}
	if strings.Contains(err.Error(), "repeated-sensitive-title") {
		t.Fatalf("recursion error leaked title: %v", err)
	}
	if !reflect.DeepEqual(payload, expected) {
		t.Fatalf("failed expansion partially modified input: %#v", payload)
	}
}

func TestCurrentExpansionUnsecretsWithConfigurationOwner(t *testing.T) {
	rowData := map[string]any{
		"token":  "{{secret.internal_token}}",
		"nested": map[string]any{"endpoint": "https://provider.invalid"},
	}
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 1, title: "shared", sharedOnly: true}: {
			UUID: "22222222-2222-2222-2222-222222222222", ProjectID: 1, Type: "company_custom", Data: rowData,
		},
	}}
	unsecreter := &currentExpansionUnsecreterStub{fn: func(_ context.Context, projectID int32, data map[string]any) (map[string]any, error) {
		if projectID != 1 {
			t.Fatalf("unsecret project=%d, want configuration owner 1", projectID)
		}
		data["token"] = "redeemed-value"
		data["nested"].(map[string]any)["endpoint"] = "https://redeemed.invalid"
		return data, nil
	}}
	service := newCurrentExpansionTestService(t, finder, unsecreter)
	payload := map[string]any{"elitea_title": "shared", "private": false}

	result, err := service.Expand(context.Background(), CurrentExpansionRequest{
		Payload: payload, CurrentProjectID: 7, Unsecret: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["token"] != "redeemed-value" || result["configuration_project_id"] != int32(1) {
		t.Fatalf("result=%#v", result)
	}
	if len(unsecreter.calls) != 1 || unsecreter.calls[0].projectID != 1 {
		t.Fatalf("unsecret calls=%#v", unsecreter.calls)
	}
	if rowData["token"] != "{{secret.internal_token}}" || rowData["nested"].(map[string]any)["endpoint"] != "https://provider.invalid" {
		t.Fatalf("unsecret dependency received repository-owned data: %#v", rowData)
	}
	if !reflect.DeepEqual(payload, map[string]any{"elitea_title": "shared", "private": false}) {
		t.Fatalf("caller payload changed: %#v", payload)
	}
}

func TestCurrentExpansionReturnsNoPartialValueOrSensitiveDependencyError(t *testing.T) {
	dependencyFailure := errors.New("vault failed for sensitive-title with raw-secret-value in project 7")
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "first"}:           expansionConfiguration(7, "github", "first"),
		{projectID: 7, title: "sensitive-title"}: expansionConfiguration(7, "openapi", "raw-secret-value"),
	}}
	unsecreter := &currentExpansionUnsecreterStub{fn: func(_ context.Context, _ int32, data map[string]any) (map[string]any, error) {
		if data["value"] == "raw-secret-value" {
			return nil, dependencyFailure
		}
		return data, nil
	}}
	service := newCurrentExpansionTestService(t, finder, unsecreter)
	payload := map[string]any{
		"a": map[string]any{"elitea_title": "first"},
		"b": map[string]any{"elitea_title": "sensitive-title"},
	}
	expected := cloneCurrentJSONObject(payload)

	result, err := service.Expand(context.Background(), CurrentExpansionRequest{
		Payload: payload, CurrentProjectID: 7, Unsecret: true,
	})
	if result != nil || !errors.Is(err, ErrCurrentExpansionDependency) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertCurrentExpansionError(t, err, CurrentExpansionDependencyCode)
	for _, secret := range []string{"sensitive-title", "raw-secret-value", "project 7"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("dependency error leaked %q: %v", secret, err)
		}
	}
	if !reflect.DeepEqual(payload, expected) {
		t.Fatalf("failed expansion partially modified input: %#v", payload)
	}
}

func TestCurrentExpansionBoundsValidationAndCancellation(t *testing.T) {
	finder := &currentExpansionFinderStub{}
	service := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})

	t.Run("identifier bound", func(t *testing.T) {
		result, err := service.Expand(context.Background(), CurrentExpansionRequest{
			Payload: map[string]any{
				"elitea_title": strings.Repeat("x", MaxCurrentExpansionIdentifierLength+1),
			},
			CurrentProjectID: 7,
		})
		if result != nil || !errors.Is(err, ErrInvalidCurrentExpansion) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("private lookup requires positive user identity", func(t *testing.T) {
		zero := int32(0)
		result, err := service.Expand(context.Background(), CurrentExpansionRequest{
			Payload: map[string]any{
				"elitea_title": "private",
				"private":      true,
			},
			CurrentProjectID: 7,
			UserID:           &zero,
		})
		if result != nil || !errors.Is(err, ErrInvalidCurrentExpansion) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("unused zero user identity does not change public behavior", func(t *testing.T) {
		zero := int32(0)
		publicFinder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
			{projectID: 7, title: "public"}: expansionConfiguration(7, "github", "ok"),
		}}
		publicService := newCurrentExpansionTestService(t, publicFinder, &currentExpansionUnsecreterStub{})
		result, err := publicService.Expand(context.Background(), CurrentExpansionRequest{
			Payload: map[string]any{
				"elitea_title": "public",
				"private":      false,
			},
			CurrentProjectID: 7,
			UserID:           &zero,
		})
		if err != nil || result["value"] != "ok" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("depth bound", func(t *testing.T) {
		payload := map[string]any{}
		cursor := payload
		for depth := 0; depth <= MaxCurrentExpansionDepth; depth++ {
			next := map[string]any{}
			cursor["nested"] = next
			cursor = next
		}
		result, err := service.Expand(context.Background(), CurrentExpansionRequest{Payload: payload, CurrentProjectID: 7})
		if result != nil || !errors.Is(err, ErrInvalidCurrentExpansion) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("node bound", func(t *testing.T) {
		result, err := service.Expand(context.Background(), CurrentExpansionRequest{
			Payload:          map[string]any{"values": make([]any, MaxCurrentExpansionNodes)},
			CurrentProjectID: 7,
		})
		if result != nil || !errors.Is(err, ErrInvalidCurrentExpansion) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("canceled before work", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := service.Expand(ctx, CurrentExpansionRequest{
			Payload: map[string]any{"elitea_title": "unused"}, CurrentProjectID: 7,
		})
		if result != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("dependency deadline identity", func(t *testing.T) {
		deadlineFinder := &currentExpansionFinderStub{err: context.DeadlineExceeded}
		deadlineService := newCurrentExpansionTestService(t, deadlineFinder, &currentExpansionUnsecreterStub{})
		result, err := deadlineService.Expand(context.Background(), CurrentExpansionRequest{
			Payload: map[string]any{"elitea_title": "unused"}, CurrentProjectID: 7,
		})
		if result != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})
}

type currentExpansionLookup struct {
	projectID  int32
	title      string
	sharedOnly bool
}

type currentExpansionScopeStub struct {
	publicID      int32
	personalID    int32
	publicErr     error
	personalErr   error
	publicCalls   int
	personalCalls int
}

func (s *currentExpansionScopeStub) PublicProjectID(context.Context) (int32, error) {
	s.publicCalls++
	return s.publicID, s.publicErr
}

func (s *currentExpansionScopeStub) PersonalProjectID(context.Context, int32) (int32, error) {
	s.personalCalls++
	return s.personalID, s.personalErr
}

type currentExpansionFinderStub struct {
	rows  map[currentExpansionLookup]CurrentExpansionConfiguration
	err   error
	calls []currentExpansionLookup
}

func (s *currentExpansionFinderStub) FindByEliteaTitle(
	_ context.Context,
	projectID int32,
	title string,
	sharedOnly bool,
) (CurrentExpansionConfiguration, bool, error) {
	lookup := currentExpansionLookup{projectID: projectID, title: title, sharedOnly: sharedOnly}
	s.calls = append(s.calls, lookup)
	if s.err != nil {
		return CurrentExpansionConfiguration{}, false, s.err
	}
	configuration, found := s.rows[lookup]
	return configuration, found, nil
}

type currentExpansionUnsecretCall struct {
	projectID int32
	data      map[string]any
}

type currentExpansionUnsecreterStub struct {
	fn    func(context.Context, int32, map[string]any) (map[string]any, error)
	calls []currentExpansionUnsecretCall
}

func (s *currentExpansionUnsecreterStub) Unsecret(
	ctx context.Context,
	projectID int32,
	data map[string]any,
) (map[string]any, error) {
	s.calls = append(s.calls, currentExpansionUnsecretCall{projectID: projectID, data: cloneCurrentJSONObject(data)})
	if s.fn != nil {
		return s.fn(ctx, projectID, data)
	}
	return data, nil
}

func newCurrentExpansionTestService(
	t *testing.T,
	finder CurrentExpansionFinder,
	unsecreter CurrentExpansionUnsecreter,
) *CurrentExpansionService {
	t.Helper()
	service, err := NewCurrentExpansionService(
		&currentExpansionScopeStub{publicID: 1, personalID: 99},
		finder,
		unsecreter,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func expansionConfiguration(projectID int32, configurationType, value string) CurrentExpansionConfiguration {
	return CurrentExpansionConfiguration{
		UUID:      "11111111-1111-1111-1111-111111111111",
		ProjectID: projectID,
		Type:      configurationType,
		Data:      map[string]any{"value": value},
	}
}

func assertCurrentExpansionError(t *testing.T, err error, code CurrentExpansionErrorCode) {
	t.Helper()
	var expansionErr *CurrentExpansionError
	if !errors.As(err, &expansionErr) || expansionErr.Code != code {
		t.Fatalf("error=%T %v, want CurrentExpansionError code %q", err, err, code)
	}
}
