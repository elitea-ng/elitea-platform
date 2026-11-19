package configurations

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func normalizeMicrosoft(t *testing.T, typeName string, op CurrentConfigurationNormalizationOperation, data map[string]any) (map[string]any, error) {
	t.Helper()
	fallbackCalled := false
	normalizer := NewCurrentMicrosoftDataNormalizer(currentNormalizerFunc(func(
		context.Context, CurrentConfigurationNormalizationRequest,
	) (CurrentConfigurationNormalizationResult, error) {
		fallbackCalled = true
		return CurrentConfigurationNormalizationResult{}, nil
	}))
	result, err := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: op, Type: typeName, Data: data,
	})
	if fallbackCalled {
		t.Fatalf("%s: a Microsoft type fell through to another owner", typeName)
	}
	if err == nil && !result.Complete {
		t.Fatalf("%s: the result is not complete", typeName)
	}
	return result.Data, err
}

type currentNormalizerFunc func(context.Context, CurrentConfigurationNormalizationRequest) (CurrentConfigurationNormalizationResult, error)

func (f currentNormalizerFunc) Normalize(ctx context.Context, request CurrentConfigurationNormalizationRequest) (CurrentConfigurationNormalizationResult, error) {
	return f(ctx, request)
}

func TestMicrosoftCreateKeepsOnlyTheTypesFieldsAndNeverARefreshToken(t *testing.T) {
	for _, typeName := range []string{"outlook", "teams", "ms_graph"} {
		data, err := normalizeMicrosoft(t, typeName, CurrentConfigurationNormalizationCreate, map[string]any{
			"client_id": "app", "client_secret": "{{secret.ms}}",
			"oauth_discovery_endpoint": "https://login.microsoftonline.com/contoso.com/",
			"refresh_token":            "0.AAAA-planted-refresh-token", "access_token": "planted-access-token",
			"unknown": "x",
		})
		if err != nil {
			t.Fatalf("%s: %v", typeName, err)
		}
		for _, forbidden := range []string{"refresh_token", "access_token", "unknown"} {
			if _, present := data[forbidden]; present {
				t.Errorf("%s: %s reached the credential's data", typeName, forbidden)
			}
		}
		if data["oauth_discovery_endpoint"] != "https://login.microsoftonline.com/contoso.com" || data["client_id"] != "app" {
			t.Errorf("%s: data = %v", typeName, data)
		}
	}
}

func TestMicrosoftCreateNormalizesScopesWithTheDescriptorFunctions(t *testing.T) {
	data, err := normalizeMicrosoft(t, "outlook", CurrentConfigurationNormalizationCreate, map[string]any{
		"client_id": "a", "client_secret": "b", "scopes": []any{"mail.send", "https://graph.microsoft.com/Mail.Read", "Files.Read"},
	})
	if err != nil || !reflect.DeepEqual(data["scopes"], []any{"Mail.Read", "Mail.Send"}) {
		t.Fatalf("outlook scopes = %v, %v", data["scopes"], err)
	}
	data, err = normalizeMicrosoft(t, "teams", CurrentConfigurationNormalizationUpdate, map[string]any{"scopes": "Files.Read"})
	if err != nil || !reflect.DeepEqual(data["scopes"], []any{"User.ReadBasic.All", "Chat.Read", "Team.ReadBasic.All", "Channel.ReadBasic.All"}) {
		t.Fatalf("teams default scopes = %v, %v", data["scopes"], err)
	}
	data, err = normalizeMicrosoft(t, "ms_graph", CurrentConfigurationNormalizationCreate, map[string]any{
		"client_id": "a", "client_secret": "b", "scopes": "mail.read  https://graph.microsoft.com/MAIL.READ openid",
	})
	if err != nil || !reflect.DeepEqual(data["scopes"], []any{"mail.read"}) {
		t.Fatalf("ms_graph scopes = %v, %v", data["scopes"], err)
	}
	_, err = normalizeMicrosoft(t, "ms_graph", CurrentConfigurationNormalizationCreate, map[string]any{
		"client_id": "a", "client_secret": "b", "scopes": []any{".default", "Mail.Read"},
	})
	assertMicrosoftFieldError(t, err, "data.scopes")
}

func assertMicrosoftFieldError(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), field) {
		t.Fatalf("error = %v, want a refusal of %s", err, field)
	}
}

func TestMicrosoftCreateRefusesAnAuthorityThatIsNotEntra(t *testing.T) {
	for _, endpoint := range []string{
		"https://login.example.test/t", "http://login.microsoftonline.com/t", "https://login.microsoftonline.com.evil.test/t",
		"https://login.microsoftonline.us/t", "https://login.microsoftonline.com/t/extra",
	} {
		_, err := normalizeMicrosoft(t, "teams", CurrentConfigurationNormalizationCreate, map[string]any{
			"client_id": "a", "client_secret": "b", "oauth_discovery_endpoint": endpoint,
		})
		assertMicrosoftFieldError(t, err, "oauth_discovery_endpoint")
	}
}

func TestMicrosoftCreateNeedsADefaultAppOrACustomAppClient(t *testing.T) {
	if _, err := normalizeMicrosoft(t, "outlook", CurrentConfigurationNormalizationCreate, map[string]any{
		"oauth_app_id": "7f8d0c1e-0000-4000-8000-000000000000",
	}); err != nil {
		t.Errorf("a Default app credential was refused: %v", err)
	}
	_, err := normalizeMicrosoft(t, "outlook", CurrentConfigurationNormalizationCreate, map[string]any{"client_secret": "b"})
	assertMicrosoftFieldError(t, err, "data.client_id")
	_, err = normalizeMicrosoft(t, "outlook", CurrentConfigurationNormalizationCreate, map[string]any{"client_id": "a"})
	assertMicrosoftFieldError(t, err, "data.client_secret")
	_, err = normalizeMicrosoft(t, "teams", CurrentConfigurationNormalizationCreate, map[string]any{"client_id": "  ", "client_secret": "b"})
	assertMicrosoftFieldError(t, err, "data.client_id")
	// An update carries the stored secret as a reference and may omit fields.
	if _, err := normalizeMicrosoft(t, "outlook", CurrentConfigurationNormalizationUpdate, map[string]any{"mailbox": "shared@contoso.com"}); err != nil {
		t.Errorf("a partial update was refused: %v", err)
	}
}

func TestDataverseCreateValidatesTheOrgAndTheTenant(t *testing.T) {
	good := map[string]any{
		"url": "https://ORG.crm4.dynamics.com/", "tenant_id": "contoso.onmicrosoft.com", "client_id": "a", "client_secret": "b",
		"refresh_token": "planted",
	}
	data, err := normalizeMicrosoft(t, "dataverse", CurrentConfigurationNormalizationCreate, good)
	if err != nil || data["url"] != "https://org.crm4.dynamics.com" {
		t.Fatalf("data = %v, %v", data, err)
	}
	if _, present := data["refresh_token"]; present {
		t.Error("a refresh token reached the credential's data")
	}
	for _, url := range []string{"", "https://evil.test", "http://o.crm.dynamics.com", "https://o.crm.dynamics.cn", "https://o.crm.dynamics.com/x"} {
		bad := map[string]any{"url": url, "tenant_id": "t", "client_id": "a", "client_secret": "b"}
		_, err := normalizeMicrosoft(t, "dataverse", CurrentConfigurationNormalizationCreate, bad)
		assertMicrosoftFieldError(t, err, "data.url")
	}
	for _, tenant := range []string{"", "a/b", "a b"} {
		bad := map[string]any{"url": "https://o.crm.dynamics.com", "tenant_id": tenant, "client_id": "a", "client_secret": "b"}
		_, err := normalizeMicrosoft(t, "dataverse", CurrentConfigurationNormalizationCreate, bad)
		assertMicrosoftFieldError(t, err, "data.tenant_id")
	}
	// An update validates what it carries.
	if _, err := normalizeMicrosoft(t, "dataverse", CurrentConfigurationNormalizationUpdate, map[string]any{"client_id": "b"}); err != nil {
		t.Errorf("a partial update was refused: %v", err)
	}
	_, err = normalizeMicrosoft(t, "dataverse", CurrentConfigurationNormalizationUpdate, map[string]any{"url": "https://evil.test"})
	assertMicrosoftFieldError(t, err, "data.url")
}

func TestMicrosoftNormalizerLeavesOtherTypesToTheNextOwner(t *testing.T) {
	called := false
	normalizer := NewCurrentMicrosoftDataNormalizer(currentNormalizerFunc(func(
		context.Context, CurrentConfigurationNormalizationRequest,
	) (CurrentConfigurationNormalizationResult, error) {
		called = true
		return CurrentConfigurationNormalizationResult{Complete: true}, nil
	}))
	// SharePoint is a pinned SDK type: not ours.
	if _, err := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: CurrentConfigurationNormalizationCreate, Type: "sharepoint", Data: map[string]any{},
	}); err != nil || !called {
		t.Fatalf("sharepoint was claimed: called=%v err=%v", called, err)
	}
}

// The four types are served with the shapes the descriptors expect.
func TestMicrosoftNativeTypesAreServedWithTheirDefaultsAndCheckboxes(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, typeName := range []string{"outlook", "teams", "ms_graph", "dataverse"} {
		entry, ok := catalog.EntryByType(typeName)
		if !ok || entry.Section != "credentials" || !entry.HasTestConnection {
			t.Fatalf("%s: %+v ok=%v", typeName, entry, ok)
		}
		schema, _ := catalog.DataSchemaByType(typeName)
		properties, _ := schema["properties"].(map[string]any)
		if _, present := properties["refresh_token"]; present {
			t.Errorf("%s serves a refresh_token field", typeName)
		}
		secret, _ := properties["client_secret"].(map[string]any)
		variants, _ := secret["anyOf"].([]any)
		if len(variants) == 0 || variants[0].(map[string]any)["format"] != "password" {
			t.Errorf("%s: client_secret is not a password field: %v", typeName, secret)
		}
	}
	schema, _ := catalog.DataSchemaByType("outlook")
	scopes, _ := schema["properties"].(map[string]any)["scopes"].(map[string]any)
	if scopes["ui_component"] != "checkbox_list" || len(scopes["checkbox_options"].([]any)) != 6 {
		t.Errorf("outlook scopes = %v", scopes)
	}
	schema, _ = catalog.DataSchemaByType("ms_graph")
	if _, checkboxes := schema["properties"].(map[string]any)["scopes"].(map[string]any)["checkbox_options"]; checkboxes {
		t.Error("ms_graph scopes are free text")
	}
}

func TestMicrosoftCreateStoresTheNormalizedDefaultScopesWhenNoneWereSent(t *testing.T) {
	cases := map[string][]any{
		"outlook":  {"Mail.Read"},
		"teams":    {"User.ReadBasic.All", "Chat.Read", "Team.ReadBasic.All", "Channel.ReadBasic.All"},
		"ms_graph": {"User.Read"},
	}
	for typeName, want := range cases {
		data, err := normalizeMicrosoft(t, typeName, CurrentConfigurationNormalizationCreate, map[string]any{
			"client_id": "a", "client_secret": "b",
		})
		if err != nil || !reflect.DeepEqual(data["scopes"], want) {
			t.Errorf("%s: scopes = %v, %v, want %v", typeName, data["scopes"], err, want)
		}
	}
	// An update that does not touch the scopes does not add them.
	data, _ := normalizeMicrosoft(t, "outlook", CurrentConfigurationNormalizationUpdate, map[string]any{"mailbox": "x@y.z"})
	if _, present := data["scopes"]; present {
		t.Errorf("an update invented scopes: %v", data)
	}
}

// Until the sign-in works on every client the four types are kept out of the
// create pickers (metadata.hidden, which the web selector and desktop honour;
// mobile has no credential-type picker) but stay in the catalogue.
func TestMicrosoftNativeTypesAreHiddenFromThePickersAndOwnTheirNormalizer(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, typeName := range []string{"outlook", "teams", "ms_graph", "dataverse"} {
		schema, ok := catalog.DataSchemaByType(typeName)
		metadata, _ := schema["metadata"].(map[string]any)
		if !ok || metadata["hidden"] != true {
			t.Errorf("%s is not hidden: %v", typeName, metadata)
		}
		if currentNativeConfigurationType(typeName) || !currentMicrosoftConfigurationType(typeName) {
			t.Errorf("%s must be owned by the Microsoft normalizer alone", typeName)
		}
	}
}
