package configurations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

type admissionResolverStub struct {
	calls []CurrentProviderConfigurationResolution
	err   error
}

func (stub *admissionResolverStub) ResolveCurrentProviderConfiguration(
	_ context.Context,
	resolution CurrentProviderConfigurationResolution,
) error {
	stub.calls = append(stub.calls, resolution)
	return stub.err
}

func admissionSnapshot(configType, section string, data map[string]any) CurrentConfigurationLifecycleSnapshot {
	if data == nil {
		data = map[string]any{}
	}
	return CurrentConfigurationLifecycleSnapshot{
		ID:          11,
		UUID:        "3f3f1f2e-0000-4000-8000-000000000001",
		ProjectID:   7,
		EliteaTitle: "Credential",
		Type:        configType,
		Section:     section,
		Data:        data,
	}
}

func TestCurrentProviderAdmissionRequiresCompleteDependencies(t *testing.T) {
	if _, err := NewCurrentProviderAdmission(nil, CurrentProviderProjectPolicy{PublicProjectID: 1}); err == nil {
		t.Fatal("nil resolver was accepted")
	}
	if _, err := NewCurrentProviderAdmission(&admissionResolverStub{}, CurrentProviderProjectPolicy{}); err == nil {
		t.Fatal("missing public project was accepted")
	}
}

// A credential row that resolves is the whole point of #457: the gateway reads
// only status_ok = true, and the write route is the only component in a
// shipped stack that can set it.
func TestCurrentProviderAdmissionAdmitsResolvedCredential(t *testing.T) {
	resolver := &admissionResolverStub{}
	admission, err := NewCurrentProviderAdmission(
		resolver,
		CurrentProviderProjectPolicy{AllowProjectOwnLLMs: true, PublicProjectID: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	snapshot := admissionSnapshot("open_ai", "ai_credentials", map[string]any{"api_key": "{{secret.openai}}"})
	decision, err := admission.AdmitCurrentProviderConfiguration(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Managed || !decision.StatusOK {
		t.Fatalf("decision = %#v, want managed and usable", decision)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(resolver.calls))
	}
	call := resolver.calls[0]
	if call.ProjectID != 7 || call.Section != "ai_credentials" || call.ConfigurationUUID != snapshot.UUID {
		t.Fatalf("resolution = %#v", call)
	}
}

// The negative direction. A row whose references or hidden secrets do not
// resolve must be stored and must stay refused. Storing it as usable would
// move the failure to the user's first completion request.
func TestCurrentProviderAdmissionRefusesUnresolvedCredential(t *testing.T) {
	resolver := &admissionResolverStub{err: errors.New("secret reference does not resolve")}
	admission, err := NewCurrentProviderAdmission(
		resolver,
		CurrentProviderProjectPolicy{AllowProjectOwnLLMs: true, PublicProjectID: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	decision, err := admission.AdmitCurrentProviderConfiguration(
		context.Background(),
		admissionSnapshot("open_ai", "ai_credentials", map[string]any{"api_key": "{{secret.absent}}"}),
	)
	if err != nil {
		t.Fatalf("a failed resolution must be an answer, not an error: %v", err)
	}
	if !decision.Managed || decision.StatusOK {
		t.Fatalf("decision = %#v, want managed and refused", decision)
	}
}

// ELITEA_ALLOW_PROJECT_OWN_LLMS is enforced through this decision and nowhere
// else. A project the policy refuses must never be resolved and must never be
// marked usable.
func TestCurrentProviderAdmissionAppliesProjectPolicy(t *testing.T) {
	resolver := &admissionResolverStub{}
	admission, err := NewCurrentProviderAdmission(
		resolver,
		CurrentProviderProjectPolicy{AllowProjectOwnLLMs: false, PublicProjectID: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	refused, err := admission.AdmitCurrentProviderConfiguration(
		context.Background(),
		admissionSnapshot("open_ai", "ai_credentials", map[string]any{"api_key": "literal"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !refused.Managed || refused.StatusOK {
		t.Fatalf("private project decision = %#v, want managed and refused", refused)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("a refused project was resolved: %#v", resolver.calls)
	}

	public := admissionSnapshot("open_ai", "ai_credentials", map[string]any{"api_key": "literal"})
	public.ProjectID = 1
	admitted, err := admission.AdmitCurrentProviderConfiguration(context.Background(), public)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.Managed || !admitted.StatusOK {
		t.Fatalf("public project decision = %#v, want managed and usable", admitted)
	}
}

func TestCurrentProviderAdmissionModelRows(t *testing.T) {
	for _, test := range []struct {
		name        string
		snapshot    CurrentConfigurationLifecycleSnapshot
		wantManaged bool
		wantStatus  bool
	}{
		{
			name: "linked model resolves",
			snapshot: admissionSnapshot("llm_model", "llm", map[string]any{
				"name":           "gpt-4o",
				"ai_credentials": map[string]any{"elitea_title": "OpenAI"},
			}),
			wantManaged: true,
			wantStatus:  true,
		},
		{
			name: "linked model without a wire name is unusable",
			snapshot: admissionSnapshot("llm_model", "llm", map[string]any{
				"ai_credentials": map[string]any{"elitea_title": "OpenAI"},
			}),
			wantManaged: true,
			wantStatus:  false,
		},
		{
			// An imported model declares no reference and holds no secret, so
			// this decision owns nothing about it and the writer keeps its own
			// value. The lifecycle reconciler makes the same choice.
			name:        "imported model is not managed",
			snapshot:    admissionSnapshot("llm_model", "llm", map[string]any{"name": "gpt-4o"}),
			wantManaged: false,
			wantStatus:  false,
		},
		{
			name:        "generic SDK configuration is not managed",
			snapshot:    admissionSnapshot("github", "credentials", map[string]any{"token": "literal"}),
			wantManaged: false,
			wantStatus:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			admission, err := NewCurrentProviderAdmission(
				&admissionResolverStub{},
				CurrentProviderProjectPolicy{AllowProjectOwnLLMs: true, PublicProjectID: 1},
			)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := admission.AdmitCurrentProviderConfiguration(context.Background(), test.snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Managed != test.wantManaged || decision.StatusOK != test.wantStatus {
				t.Fatalf("decision = %#v, want managed=%v status=%v", decision, test.wantManaged, test.wantStatus)
			}
		})
	}
}

func TestCurrentProviderAdmissionReportsContextFailure(t *testing.T) {
	admission, err := NewCurrentProviderAdmission(
		&admissionResolverStub{},
		CurrentProviderProjectPolicy{AllowProjectOwnLLMs: true, PublicProjectID: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := admission.AdmitCurrentProviderConfiguration(
		ctx,
		admissionSnapshot("open_ai", "ai_credentials", map[string]any{"api_key": "literal"}),
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v", err)
	}
}

// The credential type table decides which rows this platform can mark usable.
// The LLM data plane is the Bifrost gateway, and the gateway holds its own
// table. The two must agree, or a credential type the gateway can serve stays
// at status_ok = false for ever and the gateway never sees it.
//
// The gateway is a separate Go module, so this test reads its source instead of
// importing it. A missing file is a failure, not a skip: a check that cannot
// find its subject has not passed.
func TestCurrentProviderCredentialTypeCoversGatewayProviderTable(t *testing.T) {
	source := filepath.Join(
		"..", "..", "..", "..",
		"elitea-llm-gateway", "internal", "account", "credentials.go",
	)
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the gateway credential source %s: %v", source, err)
	}

	table := regexp.MustCompile(`(?s)var providerConfigTypes = map\[schemas\.ModelProvider\]\[\]string\{(.*?)\n\}`)
	block := table.FindSubmatch(content)
	if block == nil {
		t.Fatalf("providerConfigTypes is no longer declared in %s", source)
	}
	quoted := regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllSubmatch(block[1], -1)
	if len(quoted) == 0 {
		t.Fatalf("providerConfigTypes in %s lists no configuration type", source)
	}
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("load the pinned catalogue: %v", err)
	}

	for _, match := range quoted {
		configType := string(match[1])
		if !currentProviderCredentialType(configType) {
			t.Fatalf(
				"the gateway serves credential type %q, but this platform never marks such a row usable",
				configType,
			)
		}
		assertCatalogueDescribesCredentialType(t, catalog, configType)
	}
}

// assertCatalogueDescribesCredentialType is the second half of the same
// agreement, and it is the half that was missing (#G3).
//
// Marking a row usable is not enough. The write route reads the CATALOGUE, and
// two of its reads fail silently when the catalogue has no entry for the type:
//
//   - `sectionFor` (api/v2/configurations/handler.go) resolves the `section`
//     column from the entry. No entry stores `section = ”`, and the gateway
//     reads `WHERE section = 'ai_credentials'`. The credential never arrives.
//   - `sealConfigurationSecrets` (api/v2/configurations/secret_sealing.go)
//     reads the entry's data schema to find the password fields. No schema
//     means no password field, so the api_key was written in clear text into a
//     schema every tenant reads.
//
// So a dispatchable type with no entry is a credential that leaks and does not
// work. The section and the password markers are each asserted, not just the
// presence of an entry: an entry in the wrong section, or one whose api_key
// carries no `format: password`, reproduces one half of the defect each.
//
// A type that declares NO secret-shaped field is correct and stays correct.
// `ollama` is one: it holds an endpoint and nothing else.
func assertCatalogueDescribesCredentialType(
	t *testing.T,
	catalog *CurrentAvailableCatalog,
	configType string,
) {
	t.Helper()

	entry, ok := catalog.EntryByType(configType)
	if !ok {
		t.Fatalf(
			"the gateway dispatches to credential type %q and the pinned catalogue does not "+
				"describe it. Such a row is stored with section = '' (invisible to the gateway) "+
				"and with no data schema, so its api_key is not sealed. Add the type to "+
				"current_available_snapshot.json.",
			configType,
		)
	}
	if entry.Section != "ai_credentials" {
		t.Fatalf(
			"credential type %q is catalogued in section %q, not \"ai_credentials\"; the "+
				"gateway reads WHERE section = 'ai_credentials', so the row would be invisible",
			configType, entry.Section,
		)
	}
	dataSchema, ok := catalog.DataSchemaByType(configType)
	if !ok {
		t.Fatalf("credential type %q has no data schema, so its secret cannot be sealed", configType)
	}
	properties, ok := dataSchema["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		t.Fatalf("credential type %q declares no data properties", configType)
	}
	for name, raw := range properties {
		if !credentialFieldNameHoldsASecret(name) {
			continue
		}
		schema, ok := raw.(map[string]any)
		if !ok || !catalogueFieldIsPassword(schema) {
			t.Fatalf(
				"credential type %q declares field %q and does not mark it `format: password`, "+
					"so its value stays in plaintext in p_{project}.configuration",
				configType, name,
			)
		}
	}
}

// credentialFieldNameHoldsASecret names the credential fields that carry
// provider secrets today: the gateway's credentialData struct redeems each of
// them through the Fernet vault and never logs one.
func credentialFieldNameHoldsASecret(field string) bool {
	switch field {
	case "api_key", "api_token", "aws_secret_access_key", "vertex_credentials":
		return true
	default:
		return false
	}
}

// catalogueFieldIsPassword repeats the rule
// SealCurrentConfigurationSecrets applies, rather than calling it: a check that
// calls the function it measures agrees with any behaviour, including a broken
// one.
func catalogueFieldIsPassword(schema map[string]any) bool {
	if format, _ := schema["format"].(string); format == "password" {
		return true
	}
	options, ok := schema["anyOf"].([]any)
	if !ok {
		return false
	}
	for _, rawOption := range options {
		option, ok := rawOption.(map[string]any)
		if ok && catalogueFieldIsPassword(option) {
			return true
		}
	}
	return false
}

// TestEveryGatewayCredentialTypeHasOneCreateOwner keeps the three credential
// type lists in this package consistent.
//
// currentProviderCredentialType is the union the gateway dispatches to.
// currentLiteLLMCredentialType and currentGatewayCredentialType partition it
// into create-time normalizer owners. NewCurrentConfigurationDataNormalizer
// fails closed on a type with zero or two owners, so a type added to the
// catalogue and to neither list stops the whole application from starting —
// this test names the cause instead.
func TestEveryGatewayCredentialTypeHasOneCreateOwner(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("load the pinned catalogue: %v", err)
	}
	for _, entry := range catalog.PinnedEntries("ai_credentials") {
		if !currentProviderCredentialType(entry.Type) {
			t.Fatalf("catalogued credential type %q is not dispatchable; no runtime can use it",
				entry.Type)
		}
		owners := 0
		if currentLiteLLMCredentialType(entry.Type) {
			owners++
		}
		if currentGatewayCredentialType(entry.Type) {
			owners++
		}
		if owners != 1 {
			t.Fatalf("credential type %q has %d create-time owners, want exactly 1",
				entry.Type, owners)
		}
	}
}
